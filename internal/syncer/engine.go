package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/projsync"
	"github.com/Simplyph0/claudesync/internal/proto"
)

// Why a restore is waiting.
const (
	WaitApp       = "app-running" // desktop app is open
	WaitProject   = "unmapped"    // project folder not found on this PC
	WaitFormat    = "format"      // desktop session format not recognised
	WaitAmbiguous = "ambiguous"   // several Claude data folders; the user must pick one
	WaitError     = "error"       // last attempt failed; retried next time
)

type ItemState struct {
	Version int    `json:"v"`
	Hash    string `json:"h"` // portable hash of the copy on this PC at last sync; "" if it isn't here
	Stamp   string `json:"s"`
	Title   string `json:"t,omitempty"`
}

type Pending struct {
	Header  proto.ItemHeader `json:"header"`
	Reason  string           `json:"reason"`
	Title   string           `json:"title,omitempty"`
	Project *ProjectRef      `json:"project,omitempty"`
	Detail  string           `json:"detail,omitempty"`
	Missing bool             `json:"missing,omitempty"` // was on this PC, disappeared without being deleted in Claude
}

// State is this PC's sync bookkeeping, saved as JSON.
type State struct {
	Items    map[string]*ItemState `json:"items"`
	Projects map[string]string     `json:"projects"` // project key -> local folder
	Pending  map[string]*Pending   `json:"pending"`
	Skipped  map[string]string     `json:"skipped,omitempty"` // key -> title; never restored on this PC
	Removed  map[string]bool       `json:"removed,omitempty"` // struck from the server backup; not synced
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
	if st.Skipped == nil {
		st.Skipped = map[string]string{}
	}
	if st.Removed == nil {
		st.Removed = map[string]bool{}
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
	Healed                  int // sessions put back after disappearing, in the last run
	Conflicts               int
	Pending                 []Pending
	FormatPaused            bool
	Corrupt                 []string // desktop records Claude can't read
	LastSnapshot            string
}

type Engine struct {
	API         *client.Client
	Key         []byte
	DeviceID    string
	Scanner     *Scanner
	StatePath   string
	SnapshotDir string
	Search      []string          // folders searched for projects
	ProjectSync *projsync.Manager // project folders; nil if not used
	Logf        func(format string, args ...any)
	Notify      func(title, body string)
	OnChange    func()

	mu           sync.Mutex // guards st, status and repairedUpTo
	run          sync.Mutex // one backup/restore at a time
	st           *State
	status       Status
	repairedUpTo time.Time
	projectDirs  []string // project folders local sessions use (from the last scan)
	chatDirs     []string // every folder chats use (from the last scan)
	chatsOK      bool     // chatDirs is from a scan in the current sync
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

func (e *Engine) notify(title, body string) {
	if e.Notify != nil {
		e.Notify(title, body)
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
// It never removes anything from the server.
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
	for _, k := range sortedKeys(scan.Items) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l := scan.Items[k]
		e.mu.Lock()
		st := e.st.Items[k]
		removed := e.st.Removed[k]
		e.mu.Unlock()
		if removed || (st != nil && st.Hash != "" && st.Stamp == l.Stamp) {
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
		h, err := e.API.Put(ctx, l.Kind, l.ID, base, blob, SealMeta(e.Key, l.Kind, l.ID, l.Title))
		if errors.Is(err, client.ErrRemoved) {
			e.mu.Lock()
			e.st.Removed[k] = true
			e.mu.Unlock()
			continue
		}
		if errors.Is(err, client.ErrConflict) {
			conflicts++ // changed elsewhere too; the restore that follows keeps both copies
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
	e.status.Corrupt = scan.Corrupt
	saveErr := e.saveLocked()
	e.mu.Unlock()
	if uploaded > 0 {
		e.logf("backup: uploaded %d item(s)", uploaded)
	}
	if conflicts > 0 {
		e.logf("backup: %d item(s) also changed on another PC; keeping both copies", conflicts)
	}
	if scan.UnknownFormat > 0 {
		e.logf("backup: %d desktop session record(s) in an unrecognised format were backed up as-is", scan.UnknownFormat)
	}
	if len(scan.Corrupt) > 0 {
		e.logf("backup: %d desktop session record(s) aren't readable and weren't uploaded: %v", len(scan.Corrupt), scan.Corrupt)
	}
	return errors.Join(append(errs, saveErr)...)
}

// Sync is one full cycle: back up this PC, then restore from the server.
// Nothing on this PC is replaced without being saved in a local snapshot
// first, and nothing is ever deleted.
func (e *Engine) Sync(ctx context.Context) error {
	e.run.Lock()
	defer e.run.Unlock()
	e.setBusy(true)
	defer e.setBusy(false)
	err := e.sync(ctx)
	e.setErr(err)
	return err
}

// Restore is Sync; restoring always backs up first.
func (e *Engine) Restore(ctx context.Context) error { return e.Sync(ctx) }

// repair fixes files broken by ClaudeSync 0.1 (see Repair). It only runs
// while Claude is closed, since Claude may be writing those files.
func (e *Engine) repair() {
	if e.Scanner.appRunning() {
		return
	}
	started := time.Now()
	roots := []string{e.Scanner.Loc.Home, e.Scanner.Loc.ClaudeHome, e.Scanner.Loc.DesktopDir}
	e.mu.Lock()
	for _, p := range e.st.Projects {
		roots = append(roots, p)
	}
	since := e.repairedUpTo
	e.mu.Unlock()
	snap := NewSnapshot(e.SnapshotDir, "before repair")
	fixed, err := e.Scanner.Repair(roots, since, snap)
	if cerr := snap.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if err != nil {
		e.logf("repair: %v", err)
		return
	}
	e.mu.Lock()
	e.repairedUpTo = started.Add(-time.Minute)
	e.mu.Unlock()
	if len(fixed) > 0 {
		e.logf("repaired %d file(s) that ClaudeSync 0.1 had written with broken paths (originals saved in %s): %v", len(fixed), snap.Path(), fixed)
	}
}

func (e *Engine) sync(ctx context.Context) error {
	e.mu.Lock()
	e.chatsOK = false // only a scan in this run may say which folders have chats
	e.mu.Unlock()
	e.repair()
	berr := e.backup(ctx)
	if berr != nil {
		e.logf("backup before restore: %v", berr)
		if errors.Is(berr, client.ErrUnauthorized) {
			return berr
		}
	}
	list, err := e.API.Changes(ctx, 0)
	if err != nil {
		return errors.Join(berr, err)
	}
	// Project folders sync before sessions, so a restored chat finds its code.
	if e.ProjectSync != nil {
		if err := e.ProjectSync.SyncAll(ctx); err != nil {
			e.logf("project folders: %v", err)
		}
	}

	scan, err := e.Scanner.Scan()
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.projectDirs = scan.ProjectDirs
	e.chatDirs, e.chatsOK = scan.ChatDirs, scan.SessionsOK
	e.mu.Unlock()

	// Work out what this PC is missing or behind on.
	e.mu.Lock()
	pending := map[string]*Pending{}
	for _, h := range list.Items {
		if h.Kind == proto.KindProject {
			continue // project folders are synced by ProjectSync
		}
		k := h.Key()
		if h.Removed {
			e.st.Removed[k] = true
			continue
		}
		delete(e.st.Removed, k)
		if _, skip := e.st.Skipped[k]; skip {
			continue
		}
		st := e.st.Items[k]
		local := scan.Items[k]
		if local == nil && h.Kind == proto.KindSession && scan.Deleted[h.ID] {
			// deleted in Claude on this PC: don't bring it back
			if st == nil || st.Version < h.Version || st.Hash != "" {
				e.st.Items[k] = &ItemState{Version: h.Version}
			}
			continue
		}
		switch {
		case st == nil || st.Version < h.Version:
			pending[k] = &Pending{Header: h}
		case local == nil && st.Hash != "":
			pending[k] = &Pending{Header: h, Missing: true}
		}
		if old := e.st.Pending[k]; old != nil && pending[k] != nil {
			pending[k].Title, pending[k].Project = old.Title, old.Project
		}
	}
	e.st.Pending = pending
	_ = e.saveLocked()
	todo := make([]*Pending, 0, len(pending))
	for _, p := range pending {
		todo = append(todo, p)
	}
	e.mu.Unlock()
	sort.Slice(todo, func(i, j int) bool { return todo[i].Header.Seq < todo[j].Header.Seq })

	appRunning := e.Scanner.appRunning()
	index := NewProjectIndex(e.Search, scan.ProjectDirs)
	snap := e.newSnapshot("before restore", scan)
	restored, healed, conflicts := 0, 0, 0
	var errs []error
	var fresh []string
	for _, p := range todo {
		if ctx.Err() != nil {
			break
		}
		done, conflict, err := e.applyOne(ctx, p, scan, index, appRunning, snap)
		if err != nil {
			errs = append(errs, err)
		}
		switch {
		case conflict:
			conflicts++
		case done:
			fresh = append(fresh, p.Header.Key())
		}
		if done {
			restored++
			if p.Missing {
				healed++
			}
		}
	}
	if err := snap.Close(); err != nil {
		errs = append(errs, fmt.Errorf("snapshot: %w", err))
	}
	e.rehash(fresh)

	e.mu.Lock()
	e.status.LastRestore = time.Now()
	e.status.Restored, e.status.Healed = restored, healed
	e.status.Conflicts += conflicts
	if snap.Count() > 0 {
		e.status.LastSnapshot = snap.Path()
	}
	waitApp := 0
	for _, p := range e.st.Pending {
		if p.Reason == WaitApp {
			waitApp++
		}
	}
	saveErr := e.saveLocked()
	e.mu.Unlock()

	if snap.Count() > 0 {
		e.logf("saved %d file(s) to a snapshot before restoring: %s", snap.Count(), snap.Path())
	}
	if restored > 0 {
		e.logf("restore: applied %d item(s)", restored)
	}
	if healed > 0 {
		e.logf("restore: put back %d session(s) that had disappeared from this PC", healed)
		e.notify("ClaudeSync: sessions put back", fmt.Sprintf("%d session(s) had disappeared from this PC and were restored from your backups.", healed))
	}
	if conflicts > 0 {
		e.notify("ClaudeSync: kept both copies", fmt.Sprintf("%d session(s) changed on two PCs; the other PC's version was added as a separate session.", conflicts))
	}
	if waitApp > 0 {
		e.notify("ClaudeSync: updates ready", fmt.Sprintf("%d session(s) are ready. They load as soon as Claude is closed.", waitApp))
	}
	return errors.Join(append(errs, saveErr)...)
}

// newSnapshot starts a snapshot that also records the whole desktop session
// list the first time anything is written.
func (e *Engine) newSnapshot(reason string, scan *ScanResult) *guardedSnapshot {
	return &guardedSnapshot{Snapshot: NewSnapshot(e.SnapshotDir, reason), sessions: e.Scanner.Loc.SessionsDir()}
}

type guardedSnapshot struct {
	*Snapshot
	sessions string
	primed   bool
}

// prime saves the full session list before the first change.
func (g *guardedSnapshot) prime() error {
	if g.primed {
		return nil
	}
	g.primed = true
	if g.sessions == "" {
		return nil
	}
	return g.AddTree(g.sessions)
}

// applyOne restores one item. done means it no longer waits.
func (e *Engine) applyOne(ctx context.Context, p *Pending, scan *ScanResult, index *ProjectIndex, appRunning bool, snap *guardedSnapshot) (done, conflict bool, err error) {
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
	if it.M.UnknownFormat {
		// An older ClaudeSync uploaded an unreadable copy over this session;
		// use the newest readable version from the history instead.
		if good := e.newestReadable(ctx, h.Kind, h.ID); good != nil {
			it = good
		}
	}
	e.mu.Lock()
	p.Title, p.Project = it.M.Title, it.M.Project
	e.mu.Unlock()

	if h.Kind == proto.KindSession {
		if it.M.UnknownFormat || it.M.Format > FormatVersion || scan.UnknownFormat > 0 {
			return wait(WaitFormat, "Claude's session format changed; desktop restores are paused until ClaudeSync is updated")
		}
		if e.Scanner.Loc.Ambiguous {
			return wait(WaitAmbiguous, "")
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
	if err := snap.prime(); err != nil {
		wait(WaitError, err.Error())
		return false, false, fmt.Errorf("couldn't take a snapshot, so nothing was changed: %w", err)
	}

	// Did this item also change here since the last sync?
	e.mu.Lock()
	st := e.st.Items[key]
	e.mu.Unlock()
	if local := scan.Items[key]; local != nil {
		li, err := local.Build()
		if err != nil {
			return wait(WaitError, err.Error())
		}
		lh := li.Hash()
		if lh == it.Hash() {
			finish(&ItemState{Version: hdr.Version, Hash: lh, Stamp: local.Stamp, Title: local.Title})
			return true, false, nil
		}
		if st == nil || lh != st.Hash {
			if h.Kind == proto.KindSession {
				cp, err := AsCopy(it, hdr.DeviceName)
				if err != nil {
					return wait(WaitError, err.Error())
				}
				if err := e.Scanner.Write(cp, t, snap.Snapshot); err != nil {
					return wait(WaitError, err.Error())
				}
				// This PC's copy stays as it is and uploads at the next backup.
				old := ""
				if st != nil {
					old = st.Hash
				}
				finish(&ItemState{Version: hdr.Version, Hash: old, Title: local.Title})
				e.logf("conflict on %q: kept this PC's copy and added %q", it.M.Title, cp.M.Title)
				return true, true, nil
			}
			// settings/memory: the newer version wins; this PC's copy is in the snapshot and server history
			e.logf("%s changed here and on %s; using %s's version (this PC's copy is in the snapshot)", it.M.Title, hdr.DeviceName, hdr.DeviceName)
		}
	}

	if err := e.Scanner.Write(it, t, snap.Snapshot); err != nil {
		return wait(WaitError, err.Error())
	}
	if p.Missing {
		e.logf("put back %q, which had disappeared from this PC", it.M.Title)
	}
	finish(&ItemState{Version: hdr.Version, Title: it.M.Title}) // hash filled in by rehash
	return true, false, nil
}

func (e *Engine) newestReadable(ctx context.Context, kind, id string) *Item {
	vs, err := e.API.Versions(ctx, kind, id)
	if err != nil {
		return nil
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version > vs[j].Version })
	for _, v := range vs {
		_, blob, err := e.API.Get(ctx, kind, id, v.Version)
		if err != nil {
			continue
		}
		if it, err := Unseal(blob, e.Key, kind, id); err == nil && !it.M.UnknownFormat {
			return it
		}
	}
	return nil
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

// ErrCloseApp means the desktop app must be closed for this action.
var ErrCloseApp = errors.New("close the Claude app first")

// RestoreVersion writes an older server version of an item over this PC's
// copy (saving the current copy in a snapshot first). The next backup
// uploads it as the newest version, so every PC rolls back.
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
	if kind == proto.KindSession && e.Scanner.Loc.SessionsDir() != "" && e.Scanner.appRunning() {
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
	snap := NewSnapshot(e.SnapshotDir, "before rollback")
	werr := e.Scanner.Write(it, t, snap)
	if err := errors.Join(werr, snap.Close()); err != nil {
		return err
	}
	e.logf("rolled %s back to version %d", it.M.Title, version)
	return nil
}

// UndoSnapshot puts back the files saved in a snapshot. Claude must be closed.
func (e *Engine) UndoSnapshot(path string) (int, error) {
	e.run.Lock()
	defer e.run.Unlock()
	if e.Scanner.appRunning() {
		return 0, ErrCloseApp
	}
	n, err := RestoreSnapshot(path, e.SnapshotDir)
	if n > 0 {
		e.logf("put back %d file(s) from snapshot %s", n, filepath.Base(path))
	}
	return n, err
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

// ServerItem is one entry in the server's backup, for the History tab.
type ServerItem struct {
	Header  proto.ItemHeader
	Title   string
	Skipped bool // skipped on this PC
}

// ServerItems lists everything in the server backup, including removed
// items that can still be brought back.
func (e *Engine) ServerItems(ctx context.Context) ([]ServerItem, error) {
	list, err := e.API.Changes(ctx, 0)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ServerItem, 0, len(list.Items))
	for _, h := range list.Items {
		k := h.Key()
		title := OpenMeta(e.Key, h.Meta, h.Kind, h.ID)
		if title == "" {
			if st := e.st.Items[k]; st != nil && st.Title != "" {
				title = st.Title
			} else if p := e.st.Pending[k]; p != nil && p.Title != "" {
				title = p.Title
			} else if t := e.st.Skipped[k]; t != "" {
				title = t
			} else {
				title = h.ID
			}
		}
		_, skipped := e.st.Skipped[k]
		out = append(out, ServerItem{Header: h, Title: title, Skipped: skipped})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

// RemoveFromServer strikes an item from the server backup. Every PC stops
// syncing it; no PC's own copy is touched. It can be brought back for
// proto.RemovedHoldDays.
func (e *Engine) RemoveFromServer(ctx context.Context, kind, id string) error {
	if _, err := e.API.Remove(ctx, kind, id); err != nil {
		return err
	}
	e.mu.Lock()
	e.st.Removed[kind+"/"+id] = true
	delete(e.st.Pending, kind+"/"+id)
	err := e.saveLocked()
	e.mu.Unlock()
	e.logf("removed %s/%s from the server backup", kind, id)
	e.changed()
	return err
}

// DeleteFromServer erases an item from the server for good: every stored
// version is deleted, and no PC will upload it again. No PC's own copy is
// touched. Unlike RemoveFromServer this can't be undone.
func (e *Engine) DeleteFromServer(ctx context.Context, kind, id string) error {
	if _, err := e.API.Purge(ctx, kind, id); err != nil {
		return err
	}
	e.mu.Lock()
	e.st.Removed[kind+"/"+id] = true
	delete(e.st.Pending, kind+"/"+id)
	err := e.saveLocked()
	e.mu.Unlock()
	e.logf("deleted %s/%s from the server for good", kind, id)
	e.changed()
	return err
}

// BringBack undoes RemoveFromServer.
func (e *Engine) BringBack(ctx context.Context, kind, id string) error {
	if _, err := e.API.BringBack(ctx, kind, id); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.st.Removed, kind+"/"+id)
	err := e.saveLocked()
	e.mu.Unlock()
	e.logf("brought %s/%s back into the server backup", kind, id)
	e.changed()
	return err
}

// Skip stops an item from ever being restored on this PC and takes it off
// the waiting list. It changes nothing on the server or other PCs.
func (e *Engine) Skip(key, title string) error {
	e.mu.Lock()
	if title == "" {
		if p := e.st.Pending[key]; p != nil {
			title = p.Title
		}
	}
	e.st.Skipped[key] = title
	delete(e.st.Pending, key)
	err := e.saveLocked()
	e.mu.Unlock()
	e.logf("skipping %q on this PC", title)
	e.changed()
	return err
}

// ChatFolders lists every folder a chat on this PC uses, from the sync that
// just ran. ok is false when that sync didn't get as far as reading the
// sessions (or Claude's sessions folder isn't there): then "no chats" means
// nothing and no folder may be judged by it.
func (e *Engine) ChatFolders() (dirs []string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.chatDirs...), e.chatsOK
}

// Unskip lets a skipped item restore again at the next sync.
func (e *Engine) Unskip(key string) error {
	e.mu.Lock()
	delete(e.st.Skipped, key)
	err := e.saveLocked()
	e.mu.Unlock()
	e.changed()
	return err
}

// Skipped lists items skipped on this PC (key -> title).
func (e *Engine) Skipped() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for k, v := range e.st.Skipped {
		out[k] = v
	}
	return out
}

// KnownProjects lists project folders on this PC (project key -> folder):
// ones local sessions use and ones mapped for sessions from other PCs.
func (e *Engine) KnownProjects() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for _, d := range e.projectDirs {
		out[DescribeProject(d).Key] = d
	}
	for k, d := range e.st.Projects {
		out[k] = d
	}
	return out
}
