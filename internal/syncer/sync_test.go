package syncer

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/server"
	"github.com/Simplyph0/claudesync/internal/vault"
)

func TestPortableRoundTrip(t *testing.T) {
	src := []Root{{"PROJECT", `C:\Users\al\code\app`}, {"HOME", `C:\Users\al`}}
	dst := []Root{{"PROJECT", `/home/bob/src/app`}, {"HOME", `/home/bob`}}
	in := `{"cwd":"C:\\Users\\al\\code\\app","f":"C:/Users/al/code/app/x.go","h":"C:\\Users\\al\\.claude","enc":"C--Users-al-code-app","other":"C:\\Users\\alice"}`
	p := Portable(in, src)
	if strings.Contains(p, `Users\\al\\`) || !strings.Contains(p, `C:\\Users\\alice`) {
		t.Fatalf("portable form wrong: %s", p)
	}
	out := Localize(p, dst, false)
	want := `{"cwd":"/home/bob/src/app","f":"/home/bob/src/app/x.go","h":"/home/bob\\.claude","enc":"-home-bob-src-app","other":"C:\\Users\\alice"}`
	if out != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
}

const acct = "acct-1111/org-2222"

// pc is a fake computer: its own home, Claude folders and a project.
type pc struct {
	t             *testing.T
	home, project string
	eng           *Engine
	appOpen       bool
}

type world struct {
	t    *testing.T
	root string
	url  string
	key  []byte
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	st, err := server.Open(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	srv := server.New(st)
	srv.Logf = func(string, ...any) {}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	_, key, err := vault.New("sync passphrase")
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, root: root, url: ts.URL, key: key}
}

func (w *world) pc(name, projectParent string) *pc {
	t := w.t
	cl := client.New("", w.url, "", "")
	resp, err := cl.Login(context.Background(), "correct horse", name)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(w.root, name)
	project := filepath.Join(home, projectParent, "myapp")
	desktop := filepath.Join(home, "desktop-data")
	for _, d := range []string{project, filepath.Join(desktop, "claude-code-sessions", filepath.FromSlash(acct)), filepath.Join(home, ".claude", "projects")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := &pc{t: t, home: home, project: project}
	loc := claude.Locations{Home: home, ClaudeHome: filepath.Join(home, ".claude"), DesktopDir: desktop}
	sc := &Scanner{Loc: loc, Opt: Options{Sessions: true, Config: true, Memory: true, Scratch: true, ScratchMaxBytes: 10 << 20, DeviceName: name},
		AppRunning: func() bool { return p.appOpen }}
	p.eng = &Engine{API: cl, Key: w.key, DeviceID: resp.DeviceID, Scanner: sc, StatePath: filepath.Join(home, "state.json"),
		SnapshotDir: filepath.Join(home, "snapshots"), Search: []string{home}}
	if err := p.eng.Init(); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *pc) sessionsDir() string {
	return filepath.Join(p.home, "desktop-data", "claude-code-sessions", filepath.FromSlash(acct))
}

func (p *pc) transcriptDir() string {
	return filepath.Join(p.home, ".claude", "projects", claude.EncodeProjectDir(p.project))
}

// addSession creates a desktop session with a short transcript.
func (p *pc) addSession(id, cli, title string) {
	writeJSON(p.t, filepath.Join(p.sessionsDir(), id+".json"), map[string]any{
		"sessionId": id, "cliSessionId": cli, "cwd": p.project, "originCwd": p.project, "title": title, "lastActivityAt": 1791426402117})
	line, _ := json.Marshal(map[string]any{"type": "user", "cwd": p.project, "sessionId": cli, "timestamp": "2026-10-20T10:00:00Z",
		"message": map[string]any{"role": "user", "content": "edit " + filepath.Join(p.project, "main.go")}})
	if err := os.MkdirAll(p.transcriptDir(), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.transcriptDir(), cli+".jsonl"), append(line, '\n'), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pc) appendLine(cli, text string) {
	f, err := os.OpenFile(filepath.Join(p.transcriptDir(), cli+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		p.t.Fatal(err)
	}
	l, _ := json.Marshal(map[string]any{"type": "user", "sessionId": cli, "message": text})
	f.Write(append(l, '\n'))
	f.Close()
}

func (p *pc) sessions() []string {
	m, _ := filepath.Glob(filepath.Join(p.sessionsDir(), "local_*.json"))
	var out []string
	for _, f := range m {
		b, _ := os.ReadFile(f)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		out = append(out, v["title"].(string))
	}
	return out
}

func (p *pc) sync() {
	p.t.Helper()
	if err := p.eng.Sync(context.Background()); err != nil {
		p.t.Fatal(err)
	}
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func writeJSON(t *testing.T, path string, v any) {
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	cliA = "6a7a8b25-b02d-4b47-83ea-b893a968699e"
	cliB = "11111111-2222-4333-8444-555555555555"
	cliC = "99999999-8888-4777-8666-555555555555"
)

// What happened on the laptop: the desktop PC is already backed up; the
// laptop joins with chats of its own. Its own chats must survive and reach
// the server, and the desktop's must arrive.
func TestJoiningPCKeepsItsOwnChats(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "Documents")
	desk.addSession("local_aaaa", cliA, "Desktop chat")
	desk.sync()

	lap := w.pc("Laptop", filepath.Join("work", "repos"))
	lap.addSession("local_bbbb", cliB, "Laptop chat 1")
	lap.addSession("local_cccc", cliC, "Laptop chat 2")
	lap.appOpen = true
	lap.sync() // Claude open: back up only, nothing written
	if s := lap.sessions(); len(s) != 2 {
		t.Fatalf("laptop changed while Claude was open: %v", s)
	}
	if n := lap.eng.WaitingForApp(); n != 1 {
		t.Fatalf("waiting for app = %d, want 1", n)
	}
	lap.appOpen = false
	lap.sync()
	s := lap.sessions()
	if len(s) != 3 || !has(s, "Laptop chat 1") || !has(s, "Laptop chat 2") || !has(s, "Desktop chat") {
		t.Fatalf("laptop sessions after sync: %v", s)
	}
	desk.sync()
	s = desk.sessions()
	if len(s) != 3 || !has(s, "Laptop chat 1") || !has(s, "Laptop chat 2") {
		t.Fatalf("desktop didn't get the laptop's chats: %v", s)
	}
	// restored with the laptop's own folder paths
	b, _ := os.ReadFile(filepath.Join(lap.sessionsDir(), "local_aaaa.json"))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["cwd"] != lap.project {
		t.Fatalf("restored cwd = %v, want %s", m["cwd"], lap.project)
	}
	tr, err := os.ReadFile(filepath.Join(lap.transcriptDir(), cliA+".jsonl"))
	if err != nil || !strings.Contains(string(tr), strings.ReplaceAll(filepath.Join(lap.project, "main.go"), `\`, `\\`)) {
		t.Fatalf("transcript not restored with laptop paths: %v %s", err, tr)
	}
	// A second round changes nothing and uploads nothing.
	lap.sync()
	if st := lap.eng.Status(); st.Uploaded != 0 || st.Restored != 0 {
		t.Fatalf("idle round uploaded %d, restored %d", st.Uploaded, st.Restored)
	}
}

// Sessions that vanish from a PC without being deleted in Claude come back.
func TestDisappearedSessionsComeBack(t *testing.T) {
	w := newWorld(t)
	lap := w.pc("Laptop", "code")
	lap.addSession("local_bbbb", cliB, "Laptop chat 1")
	lap.addSession("local_cccc", cliC, "Laptop chat 2")
	lap.sync()

	// Both sidebar entries and one transcript disappear (e.g. the app reset).
	os.Remove(filepath.Join(lap.sessionsDir(), "local_bbbb.json"))
	os.Remove(filepath.Join(lap.sessionsDir(), "local_cccc.json"))
	os.Remove(filepath.Join(lap.transcriptDir(), cliC+".jsonl"))
	lap.sync()
	if s := lap.sessions(); len(s) != 2 {
		t.Fatalf("sessions after self-heal: %v", s)
	}
	if _, err := os.Stat(filepath.Join(lap.transcriptDir(), cliC+".jsonl")); err != nil {
		t.Fatal("transcript not restored")
	}
	if st := lap.eng.Status(); st.Healed != 2 {
		t.Fatalf("healed = %d, want 2", st.Healed)
	}
}

// Sessions deleted in Claude (which leaves a deleted_ marker) stay deleted,
// on that PC, and nothing is deleted anywhere else.
func TestDeletedInClaudeStaysDeleted(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Old chat")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()
	if s := lap.sessions(); len(s) != 1 {
		t.Fatalf("laptop: %v", s)
	}
	os.Remove(filepath.Join(lap.sessionsDir(), "local_aaaa.json"))
	os.WriteFile(filepath.Join(lap.sessionsDir(), "deleted_aaaa"), []byte("1"), 0o644)
	lap.sync()
	if s := lap.sessions(); len(s) != 0 {
		t.Fatalf("deleted chat came back: %v", s)
	}
	desk.sync()
	if s := desk.sessions(); len(s) != 1 {
		t.Fatalf("deleting on the laptop removed it from the desktop: %v", s)
	}
}

// Every file a restore replaces is saved first, and can be put back.
func TestSnapshotBeforeOverwrite(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Shared chat")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()
	before, _ := os.ReadFile(filepath.Join(lap.transcriptDir(), cliA+".jsonl"))

	desk.appendLine(cliA, "more from desktop")
	desk.sync()
	lap.sync()
	after, _ := os.ReadFile(filepath.Join(lap.transcriptDir(), cliA+".jsonl"))
	if !strings.Contains(string(after), "more from desktop") {
		t.Fatal("update not applied")
	}
	snaps := ListSnapshots(lap.eng.SnapshotDir)
	if len(snaps) == 0 {
		t.Fatal("no snapshot taken before overwriting")
	}
	files, _ := SnapshotFiles(snaps[0].Path)
	found := false
	for _, f := range files {
		if strings.HasSuffix(f, cliA+".jsonl") {
			found = true
		}
	}
	if !found {
		t.Fatalf("overwritten transcript missing from snapshot: %v", files)
	}
	if _, err := lap.eng.UndoSnapshot(snaps[0].Path); err != nil {
		t.Fatal(err)
	}
	undone, _ := os.ReadFile(filepath.Join(lap.transcriptDir(), cliA+".jsonl"))
	if string(undone) != string(before) {
		t.Fatal("undo didn't restore the previous transcript")
	}
}

// The same session continued on two PCs: both versions are kept everywhere.
func TestConflictKeepsBoth(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Fix the thing")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.sync()

	desk.appendLine(cliA, "more from desktop")
	lap.appendLine(cliA, "more from laptop")
	desk.sync()
	lap.sync() // backs up first (conflict), then restores the desktop's as a copy
	s := lap.sessions()
	if len(s) != 2 || !has(s, "Fix the thing (from Desktop)") {
		t.Fatalf("laptop after conflict: %v", s)
	}
	tr, _ := os.ReadFile(filepath.Join(lap.transcriptDir(), cliA+".jsonl"))
	if !strings.Contains(string(tr), "more from laptop") {
		t.Fatal("laptop's own version was overwritten")
	}
	lap.sync() // uploads the laptop's version and the copy
	desk.sync()
	if s := desk.sessions(); len(s) != 2 {
		t.Fatalf("desktop after conflict: %v", s)
	}
	trD, _ := os.ReadFile(filepath.Join(desk.transcriptDir(), cliA+".jsonl"))
	if !strings.Contains(string(trD), "more from laptop") {
		t.Fatal("desktop didn't get the laptop's continuation")
	}
}

// Conversations still on disk but missing from Claude's sidebar can be added back.
func TestAdoptOrphans(t *testing.T) {
	w := newWorld(t)
	lap := w.pc("Laptop", "code")
	lap.addSession("local_bbbb", cliB, "Still listed")
	lap.addSession("local_cccc", cliC, "Lost entry")
	os.Remove(filepath.Join(lap.sessionsDir(), "local_cccc.json")) // sidebar entry gone, conversation remains

	orphans, err := lap.eng.Scanner.Orphans()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].CliID != cliC || orphans[0].Cwd != lap.project {
		t.Fatalf("orphans: %+v", orphans)
	}
	if !strings.HasPrefix(orphans[0].Title, "edit ") || orphans[0].Started.Year() != 2026 {
		t.Fatalf("orphan title/time: %q %v", orphans[0].Title, orphans[0].Started)
	}
	lap.appOpen = true
	if _, err := lap.eng.Scanner.Adopt(orphans); err == nil {
		t.Fatal("adopted while Claude was open")
	}
	lap.appOpen = false
	n, err := lap.eng.Scanner.Adopt(orphans)
	if err != nil || n != 1 {
		t.Fatalf("adopt: %d %v", n, err)
	}
	if s := lap.sessions(); len(s) != 2 {
		t.Fatalf("sessions after adopt: %v", s)
	}
	scan, _ := lap.eng.Scanner.Scan()
	found := false
	for _, l := range scan.Items {
		if it, err := l.Build(); err == nil && it.M.CliID == cliC {
			found = true
		}
	}
	if !found {
		t.Fatal("adopted session isn't picked up for syncing")
	}
	if o, _ := lap.eng.Scanner.Orphans(); len(o) != 0 {
		t.Fatalf("still orphaned after adopt: %+v", o)
	}
}

// With two Claude data folders and none chosen, desktop sessions aren't restored.
func TestAmbiguousFolderWaits(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Chat")
	desk.sync()
	lap := w.pc("Laptop", "code")
	lap.eng.Scanner.Loc.Ambiguous = true
	lap.sync()
	if s := lap.sessions(); len(s) != 0 {
		t.Fatalf("restored into an ambiguous folder: %v", s)
	}
	st := lap.eng.Status()
	if len(st.Pending) != 1 || st.Pending[0].Reason != WaitAmbiguous {
		t.Fatalf("pending: %+v", st.Pending)
	}
	_ = time.Now
}
