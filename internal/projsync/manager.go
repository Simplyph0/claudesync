package projsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/proto"
	"github.com/Simplyph0/claudesync/internal/vault"
)

// What a project is set to.
const (
	ModeOff    = "off"
	ModeBackup = "backup" // this PC's copy is backed up; nothing here is changed
	ModeSync   = "sync"   // kept the same on every PC that syncs it
)

// The "always do X / ask me" choices.
const (
	ClashKeepBoth = "keep-both" // the other PC's version arrives next to yours
	ClashNewest   = "newest"    // the most recently edited version wins; the other goes to trash
	ClashAsk      = "ask"

	LinkAsk    = "ask" // linking a folder that already has files: show a preview
	LinkMerge  = "merge"
	LinkServer = "server" // make this folder match the server
	LinkLocal  = "local"  // make the server match this folder

	BigAsk      = "ask" // a sync would move many files to trash
	BigContinue = "continue"

	GitAsk  = "ask"  // PCs are on different git branches/commits
	GitSync = "sync" // sync anyway
	GitSkip = "skip" // don't sync until they match

	DeletesTrash = "trash" // files deleted on another PC go to trash here
	DeletesAsk   = "ask"
)

// Settings are the PC-wide choices (from the client's Settings tab).
type Settings struct {
	ClashPolicy   string
	FirstLink     string
	BigChange     string
	GitGuard      string
	RemoteDeletes string
	MaxFileMB     int
	MaxProjectMB  int
	TrashDays     int
	TrashGB       int
	LANOnly       bool
	UploadCapMB   int // per sync; 0 = no cap
}

func DefaultSettings() Settings {
	return Settings{ClashPolicy: ClashKeepBoth, FirstLink: LinkAsk, BigChange: BigAsk, GitGuard: GitAsk,
		RemoteDeletes: DeletesTrash, MaxFileMB: 200, MaxProjectMB: 2048, TrashDays: 30, TrashGB: 5}
}

// Project is one folder's configuration on this PC.
type Project struct {
	Key          string    `json:"key"`  // same key as sessions use (git remote or folder name)
	Name         string    `json:"name"` // shown in lists
	Folder       string    `json:"folder"`
	Mode         string    `json:"mode"`
	Clash        string    `json:"clash,omitempty"`       // "" = use the PC-wide setting
	AllowLarge   bool      `json:"allowLarge,omitempty"`  // OK'd going over the size limit
	Paused       bool      `json:"paused,omitempty"`      // stays set up, but isn't synced until resumed
	Ignore       []string  `json:"ignore,omitempty"`      // "don't sync these" lines (gitignore style), on this PC only
	Manual       bool      `json:"manual,omitempty"`      // added by hand with Add folder: never removed because its chats are gone
	ChatSeen     bool      `json:"chatSeen,omitempty"`    // a chat on this PC has used this folder
	NoChatsSince time.Time `json:"noChatsSince,omitzero"` // since when no chat uses it (see ReconcileChats)
}

// Decision is something waiting for the user because a setting says "ask".
type Decision struct {
	Kind    string    `json:"kind"` // first-link, big-change, git, deletes, large
	Message string    `json:"message"`
	Preview *Preview  `json:"preview,omitempty"`
	Paths   []string  `json:"paths,omitempty"`
	Created time.Time `json:"created"`
}

// Preview summarises what linking a folder would do.
type Preview struct {
	Identical, OnlyHere, OnlyServer, Differ int
	Device                                  string
}

// Clash is a file changed differently on two PCs, waiting for the user.
type Clash struct {
	Path         string    `json:"path"`
	LocalHash    string    `json:"localHash"`
	RemoteHash   string    `json:"remoteHash"`
	LocalTime    time.Time `json:"localTime"`
	RemoteTime   time.Time `json:"remoteTime"`
	LocalSize    int64     `json:"localSize"`
	RemoteSize   int64     `json:"remoteSize"`
	RemoteDevice string    `json:"remoteDevice"`
}

type pstate struct {
	BaseVersion int                   `json:"baseVersion"`
	BaseItem    string                `json:"baseItem,omitempty"` // server item the base belongs to (synced or backup)
	Cache       map[string]cacheEntry `json:"cache"`
	Decision    *Decision             `json:"decision,omitempty"`
	Clashes     map[string]*Clash     `json:"clashes,omitempty"`
	Resolved    map[string]string     `json:"resolved,omitempty"` // path -> mine/theirs/both (for the clash recorded)
	Once        map[string]string     `json:"once,omitempty"`     // one-time answers to a Decision
	LastSync    time.Time             `json:"lastSync"`
	Status      string                `json:"status"`
	Error       string                `json:"error,omitempty"`
	Skipped     []Skip                `json:"skipped,omitempty"`
	Snoozed     map[string]string     `json:"snoozed,omitempty"`  // decision kind -> the question you said "Not now" to
	Approved    map[string]string     `json:"approved,omitempty"` // decision kind -> the question you said yes to, for these exact facts
	Hold        string                `json:"hold,omitempty"`     // the snoozed question that kept the last sync from running
	Fix         string                `json:"fix,omitempty"`      // what to do about Error
	Detail      string                `json:"detail,omitempty"`   // the raw error, for bug reports
	Last        *Result               `json:"last,omitempty"`
	Events      []Event               `json:"events,omitempty"`
}

// Status is what the GUI shows for a project.
type Status struct {
	Project
	LastSync time.Time
	Status   string
	Error    string
	Decision *Decision
	Clashes  []Clash
	Skipped  []Skip
	OnHold   string  // a question you said "Not now" to, which is why nothing is syncing
	Fix      string  // what to do about Error
	Detail   string  // the raw error
	Progress string  // what a running sync is doing
	Active   bool    // a sync is running now
	Last     *Result // what the last finished sync did
	Events   []Event // newest last
}

type Manager struct {
	API        *client.Client
	Keys       Keys
	DeviceID   string
	DeviceName string
	Dir        string // ClaudeSync's data folder for projects
	Settings   Settings
	Logf       func(format string, args ...any)
	Now        func() time.Time

	mu       sync.Mutex
	projects map[string]*Project
	states   map[string]*pstate
	running  sync.Mutex
	active   map[string]bool    // projects being synced now
	cur      map[string]*Result // what the running syncs have done so far
	progress map[string]string
}

// Open loads the project list and state from dir.
func Open(dir string) (*Manager, error) {
	m := &Manager{Dir: dir, Settings: DefaultSettings(), projects: map[string]*Project{}, states: map[string]*pstate{}, Now: time.Now}
	var list []*Project
	b, err := os.ReadFile(filepath.Join(dir, "projects.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, err
		}
	}
	for _, p := range list {
		m.projects[p.Key] = p
	}
	return m, nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func keyHash(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

// itemID is the shared item every PC syncing the project uses; backups are
// per PC so two PCs backing up the same project don't overwrite each other.
func itemID(key string) string { return "prj_" + keyHash(key)[:24] }

func (m *Manager) backupID(key string) string { return "prb_" + keyHash(key)[:20] + "_" + m.DeviceID }

func (m *Manager) trash() *Trash {
	return &Trash{Dir: filepath.Join(m.Dir, "trash"), Days: m.Settings.TrashDays, MaxBytes: int64(m.Settings.TrashGB) << 30}
}

// Trash gives the GUI access to the project trash.
func (m *Manager) Trash() *Trash { return m.trash() }

// --- persistence ---

func (m *Manager) saveProjectsLocked() error {
	list := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	b, _ := json.MarshalIndent(list, "", " ")
	return writeFileAtomic(filepath.Join(m.Dir, "projects.json"), b, time.Time{}, false)
}

func (m *Manager) statePath(key string) string {
	return filepath.Join(m.Dir, "state", keyHash(key)[:24]+".json")
}

func (m *Manager) basePath(key string) string {
	return filepath.Join(m.Dir, "base", keyHash(key)[:24]+".json")
}

func (m *Manager) stateLocked(key string) *pstate {
	if st := m.states[key]; st != nil {
		return st
	}
	st := &pstate{}
	if b, err := os.ReadFile(m.statePath(key)); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Cache == nil {
		st.Cache = map[string]cacheEntry{}
	}
	if st.Clashes == nil {
		st.Clashes = map[string]*Clash{}
	}
	if st.Resolved == nil {
		st.Resolved = map[string]string{}
	}
	if st.Once == nil {
		st.Once = map[string]string{}
	}
	m.states[key] = st
	return st
}

func (m *Manager) saveStateLocked(key string) error {
	b, _ := json.Marshal(m.stateLocked(key))
	return writeFileAtomic(m.statePath(key), b, time.Time{}, false)
}

func (m *Manager) loadBase(key string) *Manifest {
	b, err := os.ReadFile(m.basePath(key))
	if err != nil {
		return nil
	}
	var man Manifest
	if json.Unmarshal(b, &man) != nil {
		return nil
	}
	if man.Files == nil {
		man.Files = map[string]*Entry{}
	}
	return &man
}

func (m *Manager) saveBase(key string, man *Manifest) error {
	b, _ := json.Marshal(man)
	return writeFileAtomic(m.basePath(key), b, time.Time{}, false)
}

// --- configuration (GUI) ---

// Projects lists configured projects with their status.
func (m *Manager) Projects() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Status
	for _, p := range m.projects {
		st := m.stateLocked(p.Key)
		s := Status{Project: *p, LastSync: st.LastSync, Status: st.Status, Error: st.Error, Decision: st.Decision, Skipped: st.Skipped,
			OnHold: st.Hold, Fix: st.Fix, Detail: st.Detail, Progress: m.progress[p.Key], Active: m.active[p.Key], Events: append([]Event(nil), st.Events...)}
		if st.Last != nil {
			l := *st.Last
			s.Last = &l
		}
		for _, c := range st.Clashes {
			s.Clashes = append(s.Clashes, *c)
		}
		sort.Slice(s.Clashes, func(i, j int) bool { return s.Clashes[i].Path < s.Clashes[j].Path })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Get returns one project's configuration.
func (m *Manager) Get(key string) (Project, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.projects[key]
	if p == nil {
		return Project{}, false
	}
	return *p, true
}

// SetProject adds or changes a project.
func (m *Manager) SetProject(p Project) error {
	if p.Key == "" || p.Folder == "" {
		return errors.New("a project needs a key and a folder")
	}
	if p.Name == "" {
		p.Name = filepath.Base(p.Folder)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.projects[p.Key]; old == nil || !samePath(old.Folder, p.Folder) {
		if err := m.validateLocked(p); err != nil {
			return err
		}
	}
	if old := m.projects[p.Key]; old != nil && old.Folder != p.Folder {
		// a different folder: start over with this folder (nothing on disk changes)
		_ = os.Remove(m.basePath(p.Key))
		delete(m.states, p.Key)
		_ = os.Remove(m.statePath(p.Key))
	} else if old != nil && old.Mode != p.Mode {
		// Back up only and Sync use different server copies: what was agreed
		// for one says nothing about the other.
		m.resetLocked(p.Key)
		_ = m.saveStateLocked(p.Key)
	}
	m.projects[p.Key] = &p
	return m.saveProjectsLocked()
}

// resetLocked forgets what this PC last agreed with the server for a
// project, and any questions about it. Files on disk aren't touched.
func (m *Manager) resetLocked(key string) {
	st := m.stateLocked(key)
	_ = os.Remove(m.basePath(key))
	st.BaseVersion, st.BaseItem = 0, ""
	st.Decision = nil
	st.Once = map[string]string{}
	st.Clashes = map[string]*Clash{}
	st.Resolved = map[string]string{}
	st.Snoozed, st.Approved, st.Hold = nil, nil, ""
}

// Forget stops managing a project on this PC. Its files aren't touched.
func (m *Manager) Forget(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projects, key)
	delete(m.states, key)
	_ = os.Remove(m.basePath(key))
	_ = os.Remove(m.statePath(key))
	return m.saveProjectsLocked()
}

// Decide answers a project's pending Decision. choice depends on the kind:
// first-link: merge/server/local; big-change, deletes, large, git: yes/no.
func (m *Manager) Decide(key, choice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	if st.Decision == nil {
		return errors.New("nothing is waiting for a decision")
	}
	kind := st.Decision.Kind
	if choice == "no" {
		// Remember the exact question: it isn't asked again until something
		// it depends on changes (a different commit, more files) or you ask.
		if st.Snoozed == nil {
			st.Snoozed = map[string]string{}
		}
		st.Snoozed[kind] = st.Decision.Message
		st.Hold = st.Decision.Message
		st.Decision = nil
		st.Status = "on hold: you chose Not now"
		m.eventLocked(key, "info", "You chose Not now. Nothing is synced until something changes or you ask again.")
		if p := m.projects[key]; p != nil && kind == "large" {
			p.Mode = ModeOff
			_ = m.saveProjectsLocked()
		}
		return m.saveStateLocked(key)
	}
	if kind == "git" && choice == "yes" {
		if st.Approved == nil {
			st.Approved = map[string]string{}
		}
		st.Approved[kind] = st.Decision.Message // for these two checkouts; a new commit asks again
	}
	switch kind {
	case "large":
		if p := m.projects[key]; p != nil {
			p.AllowLarge = true
			_ = m.saveProjectsLocked()
		}
	default:
		st.Once[kind] = choice
	}
	st.Decision = nil
	return m.saveStateLocked(key)
}

// ResolveClash records the user's answer for a clash: mine, theirs or both.
func (m *Manager) ResolveClash(key, p, choice string) error {
	if choice != "mine" && choice != "theirs" && choice != "both" {
		return errors.New("choose mine, theirs or both")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	c := st.Clashes[p]
	if c == nil {
		return errors.New("that clash is already resolved")
	}
	st.Resolved[p] = choice + "|" + c.LocalHash + "|" + c.RemoteHash
	delete(st.Clashes, p)
	return m.saveStateLocked(key)
}

// AskAgain forgets a "Not now" so the question is asked again at the next sync.
func (m *Manager) AskAgain(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	st.Snoozed, st.Hold = nil, ""
	if st.Status == "on hold: you chose Not now" {
		st.Status = ""
	}
	return m.saveStateLocked(key)
}

// --- syncing ---

// SyncAll syncs every project that isn't off.
func (m *Manager) SyncAll(ctx context.Context) error {
	m.mu.Lock()
	var keys []string
	for k, p := range m.projects {
		if (p.Mode == ModeBackup || p.Mode == ModeSync) && !p.Paused {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	sort.Strings(keys)
	if m.Settings.LANOnly && m.API.Route() == "tunnel" {
		for _, k := range keys {
			m.setStatus(k, "waiting: project sync is set to happen only on your home network", nil)
		}
		return nil
	}
	var errs []error
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		if err := m.Sync(ctx, k); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		}
	}
	m.trash().Prune()
	return errors.Join(errs...)
}

func (m *Manager) setStatus(key, status string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	st.Status = status
	st.Error, st.Fix, st.Detail = "", "", ""
	if err != nil {
		st.Error = err.Error()
	}
	_ = m.saveStateLocked(key)
}

// setProblem records a failed sync in plain words, with what to do about it.
func (m *Manager) setProblem(key string, err error, folder string) {
	pr := Explain(err, folder)
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	st.Status, st.Error, st.Fix, st.Detail = "error", pr.What, pr.Fix, err.Error()
	_ = m.saveStateLocked(key)
}

// Sync syncs one project.
func (m *Manager) Sync(ctx context.Context, key string) error {
	m.running.Lock()
	defer m.running.Unlock()
	m.mu.Lock()
	pp := m.projects[key]
	if pp == nil {
		m.mu.Unlock()
		return errors.New("unknown project")
	}
	p := *pp
	m.mu.Unlock()
	if p.Mode != ModeBackup && p.Mode != ModeSync {
		return nil
	}
	if p.Paused {
		m.setStatus(key, "paused", nil)
		return nil
	}
	m.begin(key)
	defer m.end(key)
	m.mu.Lock()
	m.stateLocked(key).Hold = ""
	m.mu.Unlock()
	var err error
	if p.Mode == ModeBackup {
		err = m.backup(ctx, p)
	} else {
		err = m.sync(ctx, p)
	}
	if err != nil {
		m.setProblem(key, err, p.Folder)
		m.event(key, "error", "%s", Explain(err, p.Folder).What)
		m.logf("project %s: %v", p.Name, err)
	}
	return err
}

func (m *Manager) scanFolder(p Project, st *pstate) (*scanResult, error) {
	sc := &scanner{root: p.Folder, keys: m.Keys, maxFile: int64(m.Settings.MaxFileMB) << 20, cache: st.Cache, now: m.Now(), git: gitDir(p.Folder) != "", extra: p.Ignore}
	res, err := sc.scan()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	st.Cache = sc.cache
	if len(res.skipped) != len(st.Skipped) && len(res.skipped) > 0 {
		m.eventLocked(p.Key, "warn", "%d file(s) are left out: %s (%s)", len(res.skipped), res.skipped[0].Path, res.skipped[0].Why)
	}
	st.Skipped = res.skipped
	m.mu.Unlock()
	nf, nb := countFiles(res.files)
	rs := m.res(p.Key)
	rs.Files, rs.Bytes = nf, nb
	m.setProgress(p.Key, "Comparing with the server…")
	return res, nil
}

// waitFor records a Decision and stops this project's sync until answered.
func (m *Manager) waitFor(key string, d *Decision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	if st.Snoozed[d.Kind] == d.Message {
		st.Hold = d.Message
		st.Status = "on hold: you chose Not now"
		st.Error, st.Fix, st.Detail = "", "", ""
		return m.saveStateLocked(key)
	}
	d.Created = m.Now()
	st.Decision = d
	st.Status = "waiting for your decision"
	st.Error, st.Fix, st.Detail = "", "", ""
	m.eventLocked(key, "warn", "Waiting for you: %s", d.Message)
	m.logf("project %s: %s", key, d.Message)
	return m.saveStateLocked(key)
}

// once reports (and uses up) a one-time answer for a decision kind.
func (m *Manager) once(key, kind string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	v := st.Once[kind]
	if v != "" {
		delete(st.Once, kind)
		_ = m.saveStateLocked(key)
	}
	return v
}

func (m *Manager) tooLarge(p Project, total int64) bool {
	return !p.AllowLarge && m.Settings.MaxProjectMB > 0 && total > int64(m.Settings.MaxProjectMB)<<20
}

// backup uploads this PC's copy as a new version when it changed.
func (m *Manager) backup(ctx context.Context, p Project) error {
	m.mu.Lock()
	st := m.stateLocked(p.Key)
	m.mu.Unlock()
	res, err := m.scanFolder(p, st)
	if err != nil {
		return err
	}
	if m.tooLarge(p, res.total) {
		return m.waitFor(p.Key, &Decision{Kind: "large", Message: fmt.Sprintf("%s is %d MB, over the %d MB project limit. Back it up anyway?", p.Name, res.total>>20, m.Settings.MaxProjectMB)})
	}
	id := m.backupID(p.Key)
	base := m.loadBase(p.Key)
	if st.BaseItem != id {
		base = nil
	}
	man := newManifest(p.Key, p.Name, m.DeviceName)
	man.Git = readGit(p.Folder)
	for path, e := range res.files {
		man.Files[path] = e
	}
	if base != nil {
		for path, e := range base.Files {
			if res.unsettled[path] {
				man.Files[path] = e
			}
		}
		if sameFiles(base.Files, man.Files) {
			m.finish(p.Key, id, st.BaseVersion, base, "backed up", nil)
			return nil
		}
	}
	var before map[string]*Entry
	if base != nil {
		before = base.Files
	}
	nUp := changedFiles(before, man.Files)
	m.res(p.Key).Uploaded = nUp
	done, err := m.uploadChunks(ctx, p, man.Files, keysOf(man.Files))
	if err != nil {
		return err
	}
	if !done {
		m.setStatus(p.Key, "uploading (continues next sync)", nil)
		return nil
	}
	h, err := m.putManifest(ctx, id, st.BaseVersion, man)
	if errors.Is(err, client.ErrConflict) {
		// only this PC writes its backup; resync the version number and retry
		if vs, verr := m.API.Versions(ctx, proto.KindProject, id); verr == nil && len(vs) > 0 {
			h, err = m.putManifest(ctx, id, vs[len(vs)-1].Version, man)
		}
	}
	if errors.Is(err, client.ErrPurged) {
		proceed, perr := m.handlePurged(ctx, p, id)
		if !proceed {
			return perr
		}
		h, err = m.putManifest(ctx, id, 0, man)
	}
	if err != nil {
		return err
	}
	m.finish(p.Key, id, h.Version, man, "backed up", nil)
	m.logf("project %s: backed up version %d", p.Name, h.Version)
	return nil
}

func keysOf(files map[string]*Entry) []string {
	out := make([]string, 0, len(files))
	for k := range files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameFiles(a, b map[string]*Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !same(v, b[k]) {
			return false
		}
	}
	return true
}

func (m *Manager) finish(key, item string, version int, base *Manifest, status string, clashes map[string]*Clash) {
	if base != nil {
		_ = m.saveBase(key, base)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(key)
	st.BaseVersion, st.BaseItem = version, item
	st.LastSync = m.Now()
	st.Status = status
	st.Error, st.Fix, st.Detail = "", "", ""
	if r := m.cur[key]; r != nil {
		cp := *r
		cp.Time, cp.Skipped = m.Now(), len(st.Skipped)
		st.Last = &cp
		if cp.Changed() {
			m.eventLocked(key, "info", "Synced: %s", cp.Summary())
		}
	}
	if clashes != nil {
		st.Clashes = clashes
	}
	_ = m.saveStateLocked(key)
}

func (m *Manager) putManifest(ctx context.Context, id string, base int, man *Manifest) (proto.ItemHeader, error) {
	man.Time = m.Now().UTC()
	blob, err := sealManifest(m.Keys, proto.KindProject, id, man)
	if err != nil {
		return proto.ItemHeader{}, err
	}
	title := man.Name
	if strings.HasPrefix(id, "prb_") {
		title += " (backup from " + m.DeviceName + ")"
	}
	meta := sealTitle(m.Keys, id, title)
	return m.API.PutManifest(ctx, proto.KindProject, id, proto.ManifestUpload{Base: base, Meta: meta, Blob: blob, Refs: man.Refs()})
}

func sealTitle(k Keys, id, title string) []byte {
	b, _ := json.Marshal(map[string]string{"t": title})
	out, _ := vault.Seal(k.Enc, b, []byte("meta:"+proto.KindProject+"/"+id))
	return out
}

// uploadChunks uploads the chunks of the given files that the server lacks.
// done is false if the upload cap was reached (the rest goes next sync).
func (m *Manager) uploadChunks(ctx context.Context, p Project, files map[string]*Entry, paths []string) (bool, error) {
	var ids []string
	for _, path := range paths {
		ids = append(ids, files[path].Chunks...)
	}
	if len(ids) == 0 {
		return true, nil
	}
	missing, err := m.API.MissingChunks(ctx, ids)
	if err != nil {
		return false, err
	}
	need := map[string]bool{}
	for _, id := range missing {
		need[id] = true
	}
	capBytes := int64(m.Settings.UploadCapMB) << 20
	var sent int64
	wantFiles, doneFiles := 0, 0
	for _, path := range paths {
		for _, c := range files[path].Chunks {
			if need[c] {
				wantFiles++
				break
			}
		}
	}
	for _, path := range paths {
		e := files[path]
		want := false
		for _, c := range e.Chunks {
			want = want || need[c]
		}
		if !want {
			continue
		}
		doneFiles++
		m.setProgress(p.Key, "Sending file %d of %d: %s", doneFiles, wantFiles, path)
		data, err := os.ReadFile(filepath.Join(p.Folder, filepath.FromSlash(path)))
		if err != nil || fileHash(data) != e.Hash {
			return false, fmt.Errorf("%s changed while uploading; trying again next sync", path)
		}
		for _, c := range split(data) {
			id := m.Keys.ChunkID(c)
			if !need[id] {
				continue
			}
			blob, err := m.Keys.SealChunk(id, c)
			if err != nil {
				return false, err
			}
			if err := m.API.PutChunk(ctx, id, blob); err != nil {
				return false, err
			}
			delete(need, id)
			sent += int64(len(blob))
			if capBytes > 0 && sent >= capBytes {
				return len(need) == 0, nil
			}
		}
	}
	return true, nil
}

// fetch downloads a file's content from its chunks and checks it.
func (m *Manager) fetch(ctx context.Context, e *Entry) ([]byte, error) {
	var data []byte
	for _, id := range e.Chunks {
		blob, err := m.API.GetChunk(ctx, id)
		if err != nil {
			return nil, err
		}
		c, err := m.Keys.OpenChunk(id, blob)
		if err != nil {
			return nil, err
		}
		data = append(data, c...)
	}
	if e.Hash != "" && fileHash(data) != e.Hash {
		return nil, errors.New("downloaded file doesn't match its checksum")
	}
	return data, nil
}

// remote returns the latest shared manifest (nil if there's none yet).
func (m *Manager) remote(ctx context.Context, id string) (*Manifest, proto.ItemHeader, error) {
	h, blob, err := m.API.Get(ctx, proto.KindProject, id, 0)
	if errors.Is(err, client.ErrNotFound) {
		return nil, proto.ItemHeader{}, nil
	}
	if err != nil {
		return nil, h, err
	}
	if h.Purged { // deleted from the server for good: only the marker is left
		return nil, h, nil
	}
	man, err := openManifest(m.Keys, proto.KindProject, id, blob)
	return man, h, err
}

// writer applies changes to the folder, putting everything it replaces or
// removes into one trash batch first.
type writer struct {
	m      *Manager
	root   string
	batch  *trashBatch
	notes  []Skip
	failed map[string]bool // paths that couldn't be written here
}

func (w *writer) abs(rel string) string { return filepath.Join(w.root, filepath.FromSlash(rel)) }

func (w *writer) toTrash(rel string) error {
	return w.batch.add(w.abs(rel), rel)
}

// put writes data to rel, trashing what was there if it differs.
func (w *writer) put(rel string, e *Entry, data []byte) error {
	if runtime.GOOS == "windows" && windowsBadName(rel) {
		w.notes = append(w.notes, Skip{rel, "name not allowed on Windows"})
		w.failed[rel] = true
		return nil
	}
	dst := w.abs(rel)
	if e.Dir {
		return os.MkdirAll(dst, 0o755)
	}
	if e.Link != "" {
		if runtime.GOOS == "windows" {
			w.notes = append(w.notes, Skip{rel, "symlink (not created on Windows)"})
			w.failed[rel] = true
			return nil
		}
		if err := w.toTrash(rel); err != nil {
			return err
		}
		_ = os.Remove(dst)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.Symlink(filepath.FromSlash(e.Link), dst)
	}
	if cur, err := os.ReadFile(dst); err == nil {
		if fileHash(cur) == fileHash(data) {
			return nil
		}
		if err := w.toTrash(rel); err != nil {
			return fmt.Errorf("not replacing %s: couldn't save it to trash first: %w", rel, err)
		}
	}
	return writeFileAtomic(dst, data, e.Mtime, e.Exec)
}

// remove moves rel to trash, then deletes it.
func (w *writer) remove(rel string) error {
	dst := w.abs(rel)
	info, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		_ = os.Remove(dst) // only if empty
		return nil
	}
	if err := w.toTrash(rel); err != nil {
		return fmt.Errorf("not removing %s: couldn't save it to trash first: %w", rel, err)
	}
	return os.Remove(dst)
}

// conflictName is where the other PC's version of a clashing file goes.
func conflictName(rel, device string, t time.Time) string {
	dir, file := path.Split(rel)
	ext := path.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	return dir + stem + " (from " + safeID(device) + " " + t.Local().Format("2006-01-02 1504") + ")" + ext
}

// sync is the two-way sync of one project.
func (m *Manager) sync(ctx context.Context, p Project) error {
	id := itemID(p.Key)
	m.mu.Lock()
	st := m.stateLocked(p.Key)
	baseVersion := st.BaseVersion
	m.mu.Unlock()
	if err := checkFolder(p.Folder, baseVersion); err != nil {
		return err
	}
	R, rh, err := m.remote(ctx, id)
	if err != nil {
		return err
	}
	if rh.Purged {
		proceed, err := m.handlePurged(ctx, p, id)
		if !proceed {
			return err
		}
		R, rh = nil, proto.ItemHeader{} // a new copy starts from this folder
	} else if rh.Removed {
		m.setStatus(p.Key, "removed from the server backup; not syncing", nil)
		return nil
	}
	// The base must be what this PC last agreed for this very item. A base from
	// the project's backup (or from before 0.4.1 recorded which item it was)
	// only counts if it matches the server copy exactly.
	B := m.loadBase(p.Key)
	m.mu.Lock()
	if st.BaseItem != id {
		if st.BaseItem == "" && B != nil && R != nil && st.BaseVersion == rh.Version && sameFiles(B.Files, R.Files) {
			st.BaseItem = id
		} else {
			m.resetLocked(p.Key)
			st.BaseItem = id // the (now empty) base belongs to this item: don't reset again
			B = nil
		}
		_ = m.saveStateLocked(p.Key)
	}
	waiting := st.Decision != nil
	m.mu.Unlock()
	if waiting {
		return nil // still waiting for the user
	}
	if R == nil {
		B = nil // nothing on the server: never conclude files were deleted elsewhere
	}
	res, err := m.scanFolder(p, st)
	if err != nil {
		return err
	}
	if m.tooLarge(p, res.total) {
		return m.waitFor(p.Key, &Decision{Kind: "large", Message: fmt.Sprintf("%s is %d MB, over the %d MB project limit. Sync it anyway?", p.Name, res.total>>20, m.Settings.MaxProjectMB)})
	}
	if B != nil && st.BaseVersion == 0 {
		B = nil
	}
	localGit := readGit(p.Folder)

	// Git guard: don't mix files from two different checkouts.
	if R != nil && R.Device != m.DeviceName && differentCheckout(R.Git, localGit) {
		choice := m.Settings.GitGuard
		gitMsg := fmt.Sprintf("%s has %s @ %s checked out; this PC has %s @ %s. Syncing now would mix files from the two. Pull/push or switch branch first, or sync anyway?",
			R.Device, R.Git.Branch, R.Git.Head, localGit.Branch, localGit.Head)
		m.mu.Lock()
		approved := st.Approved["git"] == gitMsg
		m.mu.Unlock()
		if o := m.once(p.Key, "git"); o == "yes" || approved {
			choice = GitSync
		}
		switch choice {
		case GitSkip:
			m.setStatus(p.Key, fmt.Sprintf("skipped: %s is on %s@%s, this PC on %s@%s", R.Device, R.Git.Branch, R.Git.Head, localGit.Branch, localGit.Head), nil)
			return nil
		case GitAsk:
			return m.waitFor(p.Key, &Decision{Kind: "git", Message: gitMsg})
		}
	}

	local := map[string]*Entry{}
	for k, v := range res.files {
		local[k] = v
	}
	// Files still being written count as unchanged this time.
	for k := range res.unsettled {
		if B != nil && B.Files[k] != nil {
			local[k] = B.Files[k]
		}
	}

	// First link: this folder has files but has never synced with the server copy.
	linkChoice := ""
	if B == nil && R != nil && len(local) > 0 {
		linkChoice = m.Settings.FirstLink
		if o := m.once(p.Key, "first-link"); o != "" {
			linkChoice = o
		}
		if linkChoice == LinkAsk {
			pv := &Preview{Device: R.Device}
			for path, e := range local {
				switch r := R.Files[path]; {
				case r == nil:
					pv.OnlyHere++
				case same(e, r):
					pv.Identical++
				default:
					pv.Differ++
				}
			}
			for path := range p.visible(R.Files) {
				if local[path] == nil {
					pv.OnlyServer++
				}
			}
			return m.waitFor(p.Key, &Decision{Kind: "first-link", Preview: pv,
				Message: fmt.Sprintf("Link %s with the copy on the server (last from %s)? %d identical, %d only on this PC, %d only on the server, %d different on both.",
					p.Name, R.Device, pv.Identical, pv.OnlyHere, pv.OnlyServer, pv.Differ)})
		}
	}

	var baseFiles map[string]*Entry
	if B != nil {
		baseFiles = B.Files
	}
	acts := plan(local, p.visible(files(R)), p.visible(baseFiles))
	switch linkChoice {
	case LinkServer: // make this folder match the server
		for i := range acts {
			switch {
			case acts[i].remote == nil && acts[i].local != nil:
				acts[i].kind = actTrashLocal
			case acts[i].kind == actUpload || acts[i].kind == actClash:
				acts[i].kind = actDownload
			}
		}
	case LinkLocal: // make the server match this folder
		for i := range acts {
			switch {
			case acts[i].local == nil && acts[i].remote != nil:
				acts[i].kind = actRecordDelete
			case acts[i].kind == actDownload || acts[i].kind == actClash:
				acts[i].kind = actUpload
			}
		}
	}

	// Hold back anything not ready: files still being written.
	held := map[string]bool{}
	var keep []action
	for _, a := range acts {
		if res.unsettled[a.path] {
			held[a.path] = true
			continue
		}
		keep = append(keep, a)
	}
	acts = keep

	// Files gone from this PC: a whole folder emptying is usually an accident
	// (a wiped folder, a drive not plugged in), so that always asks; many
	// removals ask if big changes are set to ask.
	var localDels []string
	for _, a := range acts {
		if a.kind == actRecordDelete {
			localDels = append(localDels, a.path)
		}
	}
	emptied := len(local) == 0 && B != nil && len(B.Files) > 0
	if len(localDels) > 0 && linkChoice != LinkLocal && (emptied || (len(localDels) > max(50, len(local)/5) && m.Settings.BigChange == BigAsk)) {
		switch m.once(p.Key, "local-deletes") {
		case "yes":
		case "restore":
			for i := range acts {
				if acts[i].kind == actRecordDelete && acts[i].remote != nil {
					acts[i].kind = actDownload
				}
			}
		default:
			msg := fmt.Sprintf("%d file(s) are gone from %s on this PC. Delete them on your other PCs too, or put them back here?", len(localDels), p.Name)
			if emptied {
				msg = fmt.Sprintf("The folder for %s (%s) is empty on this PC. Was that on purpose? Delete everything on your other PCs too, or put the files back here?", p.Name, p.Folder)
			}
			return m.waitFor(p.Key, &Decision{Kind: "local-deletes", Paths: localDels, Message: msg})
		}
	}

	// Deletions from another PC, if set to ask.
	var dels []string
	for _, a := range acts {
		if a.kind == actTrashLocal {
			dels = append(dels, a.path)
		}
	}
	deletesOK := false
	if len(dels) > 0 && linkChoice != LinkServer && m.Settings.RemoteDeletes == DeletesAsk {
		if deletesOK = m.once(p.Key, "deletes") == "yes"; !deletesOK {
			return m.waitFor(p.Key, &Decision{Kind: "deletes", Paths: dels,
				Message: fmt.Sprintf("%d file(s) in %s were deleted on another PC. Move them to the trash here too?", len(dels), p.Name)})
		}
	}
	// Big changes, if set to ask (not asked twice for removals already OK'd).
	threshold := max(50, len(local)/5)
	if len(dels) > threshold && !deletesOK && m.Settings.BigChange == BigAsk && m.once(p.Key, "big-change") != "yes" {
		return m.waitFor(p.Key, &Decision{Kind: "big-change", Paths: dels,
			Message: fmt.Sprintf("This sync would move %d of %d files in %s to the trash. Go ahead?", len(dels), len(local), p.Name)})
	}

	// Build the new version, starting from the server's.
	N := newManifest(p.Key, p.Name, m.DeviceName)
	if R != nil {
		N = R.clone()
		N.Device = m.DeviceName
	}
	N.Git = localGit
	w := &writer{m: m, root: p.Folder, batch: m.trash().begin(p.Name, "sync from server"), failed: map[string]bool{}}
	var uploads []string
	clashes := map[string]*Clash{}
	policy := p.Clash
	if policy == "" {
		policy = m.Settings.ClashPolicy
	}
	now := m.Now().UTC()
	var errs []error

	// 1. Work out uploads and clash outcomes (no files written yet).
	type pendingWrite struct {
		rel  string
		e    *Entry
		data []byte // nil: fetch e from the server
	}
	var writes []pendingWrite
	var removes []string
	for _, a := range acts {
		switch a.kind {
		case actUpload:
			N.Files[a.path] = a.local
			delete(N.Deleted, a.path)
			uploads = append(uploads, a.path)
		case actRecordDelete:
			delete(N.Files, a.path)
			N.Deleted[a.path] = now
		case actDownload:
			writes = append(writes, pendingWrite{rel: a.path, e: a.remote})
		case actTrashLocal:
			removes = append(removes, a.path)
		case actClash:
			l, r := a.local, a.remote
			if l.Dir || r.Dir { // a folder on one side, a file on the other: keep the file
				if l.Dir {
					writes = append(writes, pendingWrite{rel: a.path, e: r})
				} else {
					N.Files[a.path] = l
					uploads = append(uploads, a.path)
				}
				continue
			}
			// Text edited in different places: merge line by line.
			if a.base != nil && l.Link == "" && r.Link == "" {
				if merged, ok := m.tryMerge(ctx, p, a); ok {
					me := &Entry{Size: int64(len(merged)), Mtime: now, Exec: l.Exec, Hash: fileHash(merged)}
					for _, c := range split(merged) {
						me.Chunks = append(me.Chunks, m.Keys.ChunkID(c))
					}
					writes = append(writes, pendingWrite{rel: a.path, e: me, data: merged})
					N.Files[a.path] = me
					uploads = append(uploads, a.path)
					m.logf("project %s: merged changes from both PCs in %s", p.Name, a.path)
					m.res(p.Key).Merged++
					continue
				}
			}
			choice := policy
			if answer := st.Resolved[a.path]; answer != "" {
				parts := strings.Split(answer, "|")
				if len(parts) == 3 && parts[1] == l.Hash && parts[2] == r.Hash {
					choice = map[string]string{"mine": "mine", "theirs": "theirs", "both": ClashKeepBoth}[parts[0]]
				}
			}
			if choice == ClashNewest {
				switch d := l.Mtime.Sub(r.Mtime); {
				case d > 2*time.Minute:
					choice = "mine"
				case d < -2*time.Minute:
					choice = "theirs"
				default:
					choice = ClashKeepBoth // too close to call
				}
			}
			switch choice {
			case "mine":
				N.Files[a.path] = l
				uploads = append(uploads, a.path)
			case "theirs":
				writes = append(writes, pendingWrite{rel: a.path, e: r})
			case ClashAsk:
				clashes[a.path] = &Clash{Path: a.path, LocalHash: l.Hash, RemoteHash: r.Hash, LocalTime: l.Mtime, RemoteTime: r.Mtime,
					LocalSize: l.Size, RemoteSize: r.Size, RemoteDevice: R.Device}
				held[a.path] = true // server keeps its version; this PC keeps its own
			default: // keep both
				other := conflictName(a.path, R.Device, r.Mtime)
				writes = append(writes, pendingWrite{rel: other, e: r})
				N.Files[other] = r
				N.Files[a.path] = l
				uploads = append(uploads, a.path)
			}
		}
	}

	// 2. Upload first, so nothing exists only on this PC when files change.
	done, err := m.uploadChunks(ctx, p, local, uploads)
	if err != nil {
		return err
	}
	// Merged files aren't on disk under that content yet: upload from memory.
	for _, wr := range writes {
		if wr.data != nil {
			if err := m.uploadData(ctx, wr.data); err != nil {
				return err
			}
		}
	}
	if !done {
		m.setStatus(p.Key, "uploading (continues next sync)", nil)
		return nil
	}

	// 3. Change this PC's folder (everything replaced or removed goes to trash first).
	for i, wr := range writes {
		m.setProgress(p.Key, "Receiving file %d of %d: %s", i+1, len(writes), wr.rel)
		data := wr.data
		if data == nil && !wr.e.Dir && wr.e.Link == "" {
			if data, err = m.fetch(ctx, wr.e); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", wr.rel, err))
				w.failed[wr.rel] = true
				continue
			}
		}
		if err := w.put(wr.rel, wr.e, data); err != nil {
			errs = append(errs, err)
			w.failed[wr.rel] = true
		}
	}
	for _, rel := range removes {
		if err := w.remove(rel); err != nil {
			errs = append(errs, err)
			w.failed[rel] = true
		}
	}
	if w.batch.count() > 0 {
		m.logf("project %s: %d replaced/removed file(s) saved to trash", p.Name, w.batch.count())
	}

	rs := m.res(p.Key)
	rs.Uploaded = len(uploads) - rs.Merged
	for _, wr := range writes {
		if wr.data == nil && !w.failed[wr.rel] {
			rs.Downloaded++
		}
	}
	for _, rel := range removes {
		if !w.failed[rel] {
			rs.Removed++
		}
	}

	// 4. Publish the new version if this PC changed anything.
	for k, t := range N.Deleted {
		if N.Files[k] != nil || now.Sub(t) > deletedKept {
			delete(N.Deleted, k)
		}
	}
	version := rh.Version
	if R == nil || !sameFiles(N.Files, R.Files) || len(N.Deleted) != len(R.Deleted) {
		h, err := m.putManifest(ctx, id, rh.Version, N)
		if errors.Is(err, client.ErrConflict) {
			m.setStatus(p.Key, "another PC synced at the same moment; finishing next sync", nil)
			return nil
		}
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		version = h.Version
		m.logf("project %s: published version %d", p.Name, version)
	}

	// 5. The new base is what was agreed, except for anything held back or not
	// written here, which keeps its old base so it's looked at again.
	newBase := N.clone()
	for path := range held {
		if B != nil && B.Files[path] != nil {
			newBase.Files[path] = B.Files[path]
		} else {
			delete(newBase.Files, path)
		}
	}
	for path := range w.failed {
		delete(newBase.Files, path)
	}
	status := "up to date"
	if len(clashes) > 0 {
		status = fmt.Sprintf("%d clash(es) waiting for you", len(clashes))
	}
	m.mu.Lock()
	for path := range st.Resolved {
		if clashes[path] == nil {
			delete(st.Resolved, path) // used
		}
	}
	st.Skipped = append(st.Skipped, w.notes...)
	m.mu.Unlock()
	m.finish(p.Key, id, version, newBase, status, clashes)
	return errors.Join(errs...)
}

func (m *Manager) uploadData(ctx context.Context, data []byte) error {
	chunks := split(data)
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = m.Keys.ChunkID(c)
	}
	missing, err := m.API.MissingChunks(ctx, ids)
	if err != nil {
		return err
	}
	need := map[string]bool{}
	for _, id := range missing {
		need[id] = true
	}
	for i, c := range chunks {
		if !need[ids[i]] {
			continue
		}
		blob, err := m.Keys.SealChunk(ids[i], c)
		if err != nil {
			return err
		}
		if err := m.API.PutChunk(ctx, ids[i], blob); err != nil {
			return err
		}
	}
	return nil
}

// tryMerge attempts a line-by-line merge of a clashing text file.
func (m *Manager) tryMerge(ctx context.Context, p Project, a action) ([]byte, bool) {
	if a.local.Size > 5<<20 || a.remote.Size > 5<<20 || a.base.Size > 5<<20 {
		return nil, false
	}
	mine, err := os.ReadFile(filepath.Join(p.Folder, filepath.FromSlash(a.path)))
	if err != nil || fileHash(mine) != a.local.Hash || !isText(mine) {
		return nil, false
	}
	theirs, err := m.fetch(ctx, a.remote)
	if err != nil || !isText(theirs) {
		return nil, false
	}
	base, err := m.fetch(ctx, a.base)
	if err != nil || !isText(base) {
		return nil, false // base version no longer on the server
	}
	return merge3(base, mine, theirs)
}

// --- server copies (download a copy, restore a version) ---

// RemoteProject is a project on the server.
type RemoteProject struct {
	ID      string
	Title   string
	Device  string
	Time    time.Time
	Version int
	Backup  bool // a one-PC backup rather than a synced project
	Removed bool
}

// RemoteProjects lists the projects stored on the server.
func (m *Manager) RemoteProjects(ctx context.Context) ([]RemoteProject, error) {
	list, err := m.API.Changes(ctx, 0)
	if err != nil {
		return nil, err
	}
	var out []RemoteProject
	for _, h := range list.Items {
		if h.Kind != proto.KindProject {
			continue
		}
		title := h.ID
		if b, err := vault.Open(m.Keys.Enc, h.Meta, []byte("meta:"+h.Kind+"/"+h.ID)); err == nil {
			var t map[string]string
			if json.Unmarshal(b, &t) == nil && t["t"] != "" {
				title = t["t"]
			}
		}
		out = append(out, RemoteProject{ID: h.ID, Title: title, Device: h.DeviceName, Time: h.Time, Version: h.Version,
			Backup: strings.HasPrefix(h.ID, "prb_"), Removed: h.Removed})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

// Download writes a stored project version (0 = latest) into folder. Files
// it would replace go to trash first; files not in that version are left
// alone. With link set (synced projects only), the folder then syncs.
func (m *Manager) Download(ctx context.Context, id string, version int, folder string, link bool) (int, error) {
	h, blob, err := m.API.Get(ctx, proto.KindProject, id, version)
	if err != nil {
		return 0, err
	}
	man, err := openManifest(m.Keys, proto.KindProject, id, blob)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return 0, err
	}
	w := &writer{m: m, root: folder, batch: m.trash().begin(man.Name, "before download"), failed: map[string]bool{}}
	n := 0
	var errs []error
	for _, rel := range keysOf(man.Files) {
		e := man.Files[rel]
		var data []byte
		if !e.Dir && e.Link == "" {
			if data, err = m.fetch(ctx, e); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rel, err))
				continue
			}
		}
		if err := w.put(rel, e, data); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if link && strings.HasPrefix(id, "prj_") && len(errs) == 0 {
		p := Project{Key: man.Key, Name: man.Name, Folder: folder, Mode: ModeSync}
		if err := m.SetProject(p); err != nil {
			return n, err
		}
		base := man.clone()
		for rel := range w.failed {
			delete(base.Files, rel)
		}
		m.finish(man.Key, id, h.Version, base, "downloaded", nil)
	}
	m.logf("downloaded %d file(s) of %s into %s", n, man.Name, folder)
	return n, errors.Join(errs...)
}

// FolderForItem returns the local folder of the project stored under a
// server item id ("" if this PC doesn't sync or back it up).
func (m *Manager) FolderForItem(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.projects {
		if itemID(p.Key) == id || m.backupID(p.Key) == id {
			return p.Folder
		}
	}
	return ""
}

// checkFolder makes sure a project's folder is there before syncing. A folder
// that has synced before and is now gone is never quietly recreated: that is
// usually a drive that isn't plugged in, and an empty folder would look like
// "everything was deleted".
func checkFolder(folder string, syncedBefore int) error {
	info, err := os.Stat(folder)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", folder)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if _, perr := os.Stat(filepath.Dir(folder)); perr != nil || syncedBefore > 0 {
			return err
		}
		return os.MkdirAll(folder, 0o755)
	default:
		return err
	}
}

// handlePurged deals with a project whose copy on the server was deleted for
// good. Nothing is uploaded again until the user says, on this PC, to start a
// new copy from this folder; otherwise a PC that still syncs the project would
// quietly undo a delete someone meant. proceed is true once the new copy may
// be started.
func (m *Manager) handlePurged(ctx context.Context, p Project, id string) (proceed bool, err error) {
	if m.once(p.Key, "purged") != "yes" {
		return false, m.waitFor(p.Key, &Decision{Kind: "purged",
			Message: fmt.Sprintf("The copy of %s on the server was deleted for good. Start a new copy from this PC's folder? The files here are kept, and your other PCs will follow the new copy.", p.Name)})
	}
	if err := m.API.Revive(ctx, proto.KindProject, id); err != nil {
		return false, err
	}
	m.mu.Lock()
	m.resetLocked(p.Key)
	m.eventLocked(p.Key, "info", "Started a new copy on the server from this folder.")
	err = m.saveStateLocked(p.Key)
	m.mu.Unlock()
	return err == nil, err
}
