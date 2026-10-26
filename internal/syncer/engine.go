package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/proto"
)

// Why a restore is waiting.
const (
	WaitApp     = "app-running" // desktop app is open
	WaitProject = "unmapped"    // project folder not found on this PC
	WaitFormat  = "format"      // desktop session format not recognised
	WaitError   = "error"       // last attempt failed; retried next time
)

type ItemState struct {
	Version int    `json:"v"`
	Hash    string `json:"h"`
	Stamp   string `json:"s"`
	Title   string `json:"t,omitempty"`
}

type Pending struct {
	Header  proto.ItemHeader `json:"header"`
	Reason  string           `json:"reason"`
	Title   string           `json:"title,omitempty"`
	Project *ProjectRef      `json:"project,omitempty"`
	Detail  string           `json:"detail,omitempty"`
}

// State is this PC's sync bookkeeping, saved as JSON.
type State struct {
	Seq      int64                 `json:"seq"`
	Items    map[string]*ItemState `json:"items"`
	Projects map[string]string     `json:"projects"` // project key -> local folder
	Pending  map[string]*Pending   `json:"pending"`
}

func LoadState(path string) (*State, error) {
	st := &State{}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			return nil, err
		}
	}
	if st.Items == nil {
		st.Items = map[string]*ItemState{}
	}
	if st.Projects == nil {
		st.Projects = map[string]string{}
	}
	if st.Pending == nil {
		st.Pending = map[string]*Pending{}
	}
	return st, nil
}

// Status is what the GUI shows.
type Status struct {
	LastBackup, LastRestore time.Time
	LastError               string
	Route                   string
	Busy                    bool
	Uploaded, Restored      int // in the last run
	Conflicts               int
	Pending                 []Pending
	FormatPaused            bool
}

type Engine struct {
	API       *client.Client
	Key       []byte
	DeviceID  string
	Scanner   *Scanner
	StatePath string
	Search    []string // folders searched for projects
	Logf      func(format string, args ...any)
	Notify    func(title, body string)
	OnChange  func()

	mu     sync.Mutex // guards st and status
	run    sync.Mutex // one backup/restore at a time
	st     *State
	status Status
}

func (e *Engine) Init() error {
	st, err := LoadState(e.StatePath)
	if err != nil {
		return err
	}
	e.st = st
	return nil
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.Route = e.API.Route()
	s.Pending = nil
	for _, p := range e.st.Pending {
		s.Pending = append(s.Pending, *p)
	}
	sort.Slice(s.Pending, func(i, j int) bool { return s.Pending[i].Header.Seq < s.Pending[j].Header.Seq })
	return s
}

// Projects returns the saved project-key -> folder mappings.
func (e *Engine) Projects() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for k, v := range e.st.Projects {
		out[k] = v
	}
	return out
}

// MapProject sets the local folder for a project from another PC.
func (e *Engine) MapProject(key, dir string) error {
	e.mu.Lock()
	if dir == "" {
		delete(e.st.Projects, key)
	} else {
		e.st.Projects[key] = dir
	}
	err := e.saveLocked()
	e.mu.Unlock()
	e.changed()
	return err
}

func (e *Engine) saveLocked() error {
	b, err := json.MarshalIndent(e.st, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.StatePath), 0o700); err != nil {
		return err
	}
	tmp := e.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.StatePath)
}

func (e *Engine) changed() {
	if e.OnChange != nil {
		e.OnChange()
	}
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *Engine) setBusy(b bool) {
	e.mu.Lock()
	e.status.Busy = b
	e.mu.Unlock()
	e.changed()
}

func (e *Engine) setErr(err error) {
	e.mu.Lock()
	if err != nil {
		e.status.LastError = time.Now().Format("15:04") + " " + err.Error()
	} else {
		e.status.LastError = ""
	}
	e.mu.Unlock()
}

// Backup uploads every item that changed on this PC since it last synced.
func (e *Engine) Backup(ctx context.Context) error {
	e.run.Lock()
	defer e.run.Unlock()
	e.setBusy(true)
	defer e.setBusy(false)
	err := e.backup(ctx)
	e.setErr(err)
	return err
}

func (e *Engine) backup(ctx context.Context) error {
	scan, err := e.Scanner.Scan()
	if err != nil {
		return err
	}
	uploaded, conflicts := 0, 0
	var errs []error
	keys := sortedKeys(scan.Items)
	for _, k := range keys {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l := scan.Items[k]
		e.mu.Lock()
		st := e.st.Items[k]
		_, waiting := e.st.Pending[k]
		e.mu.Unlock()
		if waiting || (st != nil && st.Stamp == l.Stamp) {
			continue
		}
		it, err := l.Build()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.Title, err))
			continue
		}
		hash := it.Hash()
		if st != nil && st.Hash == hash {
			e.mu.Lock()
			st.Stamp = l.Stamp
			e.mu.Unlock()
			continue
		}
		blob, err := Seal(it, e.Key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		base := 0
		if st != nil {
			base = st.Version
		}
		h, err := e.API.Put(ctx, l.Kind, l.ID, base, blob)
		if errors.Is(err, client.ErrConflict) {
			conflicts++ // changed elsewhere too; the next restore keeps both copies
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("upload %s: %w", l.Title, err))
			if errors.Is(err, client.ErrUnauthorized) {
				break
			}
			continue
		}
		e.mu.Lock()
		e.st.Items[k] = &ItemState{Version: h.Version, Hash: hash, Stamp: l.Stamp, Title: l.Title}
		_ = e.saveLocked()
		e.mu.Unlock()
		uploaded++
	}
	e.mu.Lock()
	e.status.LastBackup = time.Now()
	e.status.Uploaded = uploaded
	e.status.FormatPaused = scan.UnknownFormat > 0
	saveErr := e.saveLocked()
	e.mu.Unlock()
	if uploaded > 0 {
		e.logf("backup: uploaded %d item(s)", uploaded)
	}
	if conflicts > 0 {
		e.logf("backup: %d item(s) also changed on another PC; resolving on next restore", conflicts)
	}
	if scan.UnknownFormat > 0 {
		e.logf("backup: %d desktop session record(s) in an unrecognised format were backed up as-is", scan.UnknownFormat)
	}
	return errors.Join(append(errs, saveErr)...)
}

// Restore fetches changes made on other PCs and applies what it can. With
// appClosed set, the caller has already closed the desktop app.
func (e *Engine) Restore(ctx context.Context, appClosed bool) error {
	e.run.Lock()
	defer e.run.Unlock()
	e.setBusy(true)
	defer e.setBusy(false)
	err := e.restore(ctx, appClosed)
	e.setErr(err)
	return err
}

func (e *Engine) restore(ctx context.Context, appClosed bool) error {
	e.mu.Lock()
	since := e.st.Seq
	e.mu.Unlock()
	list, err := e.API.Changes(ctx, since)
	if err != nil {
		return err
	}
	e.mu.Lock()
	for _, h := range list.Items {
		if h.Device == e.DeviceID {
			continue
		}
		if st := e.st.Items[h.Key()]; st != nil && st.Version >= h.Version {
			continue
		}
		e.st.Pending[h.Key()] = &Pending{Header: h}
	}
	e.st.Seq = list.Seq
	_ = e.saveLocked()
	pending := make([]*Pending, 0, len(e.st.Pending))
	for _, p := range e.st.Pending {
		pending = append(pending, p)
	}
	e.mu.Unlock()
	if len(pending) == 0 {
		e.mu.Lock()
		e.status.LastRestore = time.Now()
		e.status.Restored = 0
		e.mu.Unlock()
		return nil
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Header.Seq < pending[j].Header.Seq })

	scan, err := e.Scanner.Scan()
	if err != nil {
		return err
	}
	appRunning := !appClosed && claude.DesktopRunning()
	index := NewProjectIndex(e.Search, scan.ProjectDirs)
	restored, conflicts := 0, 0
	var errs []error
	var fresh []string
	for _, p := range pending {
		if ctx.Err() != nil {
			break
		}
		done, conflict, err := e.applyOne(ctx, p, scan, index, appRunning)
		if err != nil {
			errs = append(errs, err)
		}
		if conflict {
			conflicts++
		} else if done {
			fresh = append(fresh, p.Header.Key())
		}
		if done {
			restored++
		}
	}
	e.rehash(fresh)
	e.mu.Lock()
	e.status.LastRestore = time.Now()
	e.status.Restored = restored
	e.status.Conflicts += conflicts
	waitApp := 0
	for _, p := range e.st.Pending {
		if p.Reason == WaitApp {
			waitApp++
		}
	}
	saveErr := e.saveLocked()
	e.mu.Unlock()
	if restored > 0 {
		e.logf("restore: applied %d item(s) from other PCs", restored)
	}
	if conflicts > 0 && e.Notify != nil {
		e.Notify("ClaudeSync: kept both copies", fmt.Sprintf("%d session(s) changed on two PCs; the other PC's version was added as a separate session.", conflicts))
	}
	if waitApp > 0 && e.Notify != nil {
		e.Notify("ClaudeSync: updates ready", fmt.Sprintf("%d session(s) from your other PCs are ready. Restart Claude to load them.", waitApp))
	}
	return errors.Join(append(errs, saveErr)...)
}

// applyOne restores one pending item. done means it no longer waits.
func (e *Engine) applyOne(ctx context.Context, p *Pending, scan *ScanResult, index *ProjectIndex, appRunning bool) (done, conflict bool, err error) {
	h := p.Header
	key := h.Key()
	wait := func(reason, detail string) (bool, bool, error) {
		e.mu.Lock()
		p.Reason, p.Detail = reason, detail
		e.mu.Unlock()
		return false, false, nil
	}
	finish := func(st *ItemState) {
		e.mu.Lock()
		delete(e.st.Pending, key)
		if st != nil {
			e.st.Items[key] = st
		}
		_ = e.saveLocked()
		e.mu.Unlock()
	}

	hdr, blob, err := e.API.Get(ctx, h.Kind, h.ID, 0)
	if errors.Is(err, client.ErrNotFound) {
		finish(nil)
		return false, false, nil
	}
	if err != nil {
		wait(WaitError, err.Error())
		return false, false, fmt.Errorf("download %s: %w", key, err)
	}
	if hdr.Version == 0 {
		hdr = h
	}
	it, err := Unseal(blob, e.Key, h.Kind, h.ID)
	if err != nil {
		wait(WaitError, err.Error())
		return false, false, fmt.Errorf("open %s: %w", key, err)
	}
	e.mu.Lock()
	p.Title, p.Project = it.M.Title, it.M.Project
	e.mu.Unlock()

	if h.Kind == proto.KindSession && e.Scanner.Loc.SessionsDir() != "" {
		if it.M.UnknownFormat || it.M.Format > FormatVersion || scan.UnknownFormat > 0 {
			return wait(WaitFormat, "Claude's session format changed; desktop restores are paused until ClaudeSync is updated")
		}
		if scan.Deleted[it.M.DesktopID] {
			finish(&ItemState{Version: hdr.Version}) // deleted on this PC: don't bring it back
			return false, false, nil
		}
		if appRunning {
			return wait(WaitApp, "")
		}
	}

	project := ""
	if it.M.Project != nil {
		e.mu.Lock()
		project = e.st.Projects[it.M.Project.Key]
		e.mu.Unlock()
		if project != "" {
			if info, err := os.Stat(project); err != nil || !info.IsDir() {
				project = ""
			}
		}
		if project == "" {
			if project = index.Find(*it.M.Project); project != "" {
				e.mu.Lock()
				e.st.Projects[it.M.Project.Key] = project
				e.mu.Unlock()
				e.logf("mapped project %s to %s", it.M.Project.Name, project)
			}
		}
	}
	t, err := e.Scanner.Resolve(it.M, project)
	if errors.Is(err, ErrUnmapped) {
		return wait(WaitProject, "")
	}
	if err != nil {
		return wait(WaitError, err.Error())
	}

	// Did this item also change here since the last sync?
	e.mu.Lock()
	st := e.st.Items[key]
	e.mu.Unlock()
	if local := scan.Items[key]; local != nil {
		li, err := local.Build()
		if err == nil {
			lh := li.Hash()
			if lh == it.Hash() {
				finish(&ItemState{Version: hdr.Version, Hash: lh, Stamp: local.Stamp, Title: local.Title})
				return true, false, nil
			}
			if st == nil || lh != st.Hash {
				if h.Kind != proto.KindSession {
					// settings/memory: the newer version wins, the older stays in server history
					e.logf("%s changed on this PC and on %s; using %s's version (older one kept in history)", it.M.Title, hdr.DeviceName, hdr.DeviceName)
				} else {
					cp, err := AsCopy(it, hdr.DeviceName)
					if err != nil {
						return wait(WaitError, err.Error())
					}
					if err := e.Scanner.Write(cp, t); err != nil {
						return wait(WaitError, err.Error())
					}
					// keep the local copy as-is; it uploads over the server's next backup
					old := ""
					if st != nil {
						old = st.Hash
					}
					finish(&ItemState{Version: hdr.Version, Hash: old, Title: local.Title})
					e.logf("conflict on %q: kept this PC's copy and added %q", it.M.Title, cp.M.Title)
					return true, true, nil
				}
			}
		}
	}

	if err := e.Scanner.Write(it, t); err != nil {
		return wait(WaitError, err.Error())
	}
	finish(&ItemState{Version: hdr.Version, Title: it.M.Title}) // hash filled in by rehash
	return true, false, nil
}

// rehash records the on-disk hash of freshly restored items so the next
// backup doesn't upload them straight back.
func (e *Engine) rehash(keys []string) {
	if len(keys) == 0 {
		return
	}
	scan, err := e.Scanner.Scan()
	if err != nil {
		return
	}
	for _, k := range keys {
		l := scan.Items[k]
		if l == nil {
			continue
		}
		li, err := l.Build()
		if err != nil {
			continue
		}
		e.mu.Lock()
		if st := e.st.Items[k]; st != nil {
			st.Hash, st.Stamp = li.Hash(), l.Stamp
		}
		e.mu.Unlock()
	}
	e.mu.Lock()
	_ = e.saveLocked()
	e.mu.Unlock()
}

// Reconfigure changes settings between runs (never during a backup/restore).
func (e *Engine) Reconfigure(fn func(e *Engine)) {
	e.run.Lock()
	defer e.run.Unlock()
	fn(e)
}

// WaitingForApp reports how many restores need the desktop app closed.
func (e *Engine) WaitingForApp() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, p := range e.st.Pending {
		if p.Reason == WaitApp {
			n++
		}
	}
	return n
}

// RestartAndApply closes the desktop app, applies waiting restores and opens
// the app again.
func (e *Engine) RestartAndApply(ctx context.Context) error {
	apps, err := claude.CloseDesktop(15 * time.Second)
	if err != nil {
		return err
	}
	rerr := e.Restore(ctx, true)
	if err := claude.Relaunch(apps); err != nil {
		return errors.Join(rerr, fmt.Errorf("reopen Claude: %w", err))
	}
	return rerr
}

// ErrCloseApp means the desktop app must be closed for this action.
var ErrCloseApp = errors.New("close the Claude app first")

// RestoreVersion writes an older server version of an item over this PC's
// copy. The next backup then uploads it as the newest version, so every PC
// rolls back.
func (e *Engine) RestoreVersion(ctx context.Context, kind, id string, version int) error {
	e.run.Lock()
	defer e.run.Unlock()
	_, blob, err := e.API.Get(ctx, kind, id, version)
	if err != nil {
		return err
	}
	it, err := Unseal(blob, e.Key, kind, id)
	if err != nil {
		return err
	}
	if kind == proto.KindSession && e.Scanner.Loc.SessionsDir() != "" && claude.DesktopRunning() {
		return ErrCloseApp
	}
	project := ""
	if it.M.Project != nil {
		e.mu.Lock()
		project = e.st.Projects[it.M.Project.Key]
		e.mu.Unlock()
	}
	t, err := e.Scanner.Resolve(it.M, project)
	if err != nil {
		return err
	}
	if err := e.Scanner.Write(it, t); err != nil {
		return err
	}
	e.logf("rolled %s back to version %d", it.M.Title, version)
	return nil
}

// Known lists items this PC has synced, for the history view.
func (e *Engine) Known() map[string]ItemState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]ItemState{}
	for k, v := range e.st.Items {
		out[k] = *v
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
