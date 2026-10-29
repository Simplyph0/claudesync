package projsync

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Simplyph0/claudesync/internal/client"
)

func (p *pc) syncErr() error {
	p.clock = p.clock.Add(time.Minute)
	return p.m.Sync(context.Background(), key)
}

// A folder that's gone (an unplugged drive, a rename) is never recreated empty,
// which would look like "delete everything everywhere"; the user is told why.
func TestMissingFolderIsNotRecreated(t *testing.T) {
	for _, mode := range []string{ModeSync, ModeBackup} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			a := w.pc("A", mode)
			a.write("a.txt", "1", time.Hour)
			a.sync()
			os.RemoveAll(a.folder)
			if err := a.syncErr(); err == nil {
				t.Fatal("a missing folder synced without a word")
			}
			st := a.status()
			if st.Status != "error" || !strings.Contains(st.Error, "isn't there") || st.Fix == "" || st.Detail == "" {
				t.Fatalf("not explained: %+v", st)
			}
			if _, err := os.Stat(a.folder); err == nil {
				t.Fatal("the folder was quietly recreated")
			}
			if n := len(st.Events); n == 0 || st.Events[n-1].Level != "error" {
				t.Fatalf("no error in the activity log: %+v", st.Events)
			}
			if mode == ModeSync {
				b := w.pc("B", ModeSync)
				b.sync()
				eq(t, "server still has the file", b.list(), []string{"a.txt"})
			}
			a.write("a.txt", "1", time.Hour) // plugged back in
			a.sync()
			if st := a.status(); st.Error != "" || st.Fix != "" || st.Status == "error" {
				t.Fatalf("error not cleared: %+v", st)
			}
		})
	}
}

func TestResultsAndActivityLog(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	for _, f := range []string{"1.txt", "2.txt", "3.txt"} {
		a.write(f, "x", time.Hour)
	}
	a.sync()
	l := a.status().Last
	if l == nil || l.Uploaded != 3 || l.Files != 3 || l.Downloaded != 0 {
		t.Fatalf("A's first sync: %+v", l)
	}
	b := w.pc("B", ModeSync)
	b.sync()
	if l := b.status().Last; l == nil || l.Downloaded != 3 || l.Uploaded != 0 || l.Summary() != "3 received" {
		t.Fatalf("B's first sync: %+v", l)
	}

	a.write("2.txt", "changed", time.Hour)
	a.sync()
	eq(t, "A sent one", a.status().Last.Uploaded, 1)
	b.sync()
	eq(t, "B got one", b.status().Last.Downloaded, 1)

	n := len(b.status().Events)
	b.sync() // nothing to do
	if st := b.status(); len(st.Events) != n || st.Last.Changed() {
		t.Fatalf("a quiet sync wrote to the log: %+v", st.Events)
	}
	ev := a.status().Events
	if len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Msg, "Synced: 1 sent to the server") {
		t.Fatalf("A's log: %+v", ev)
	}
	if st := a.status(); st.Active || st.Progress != "" {
		t.Fatalf("still shown as busy: %+v", st)
	}

	// The same problem again is counted, not repeated.
	os.RemoveAll(a.folder)
	a.syncErr()
	a.syncErr()
	a.syncErr()
	ev = a.status().Events
	if last := ev[len(ev)-1]; last.Level != "error" || last.N != 2 {
		t.Fatalf("repeated error: %+v", ev)
	}
}

func TestPausedProjectIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.write("a.txt", "1", time.Hour)
	p, _ := a.m.Get(key)
	p.Paused = true
	if err := a.m.SetProject(p); err != nil {
		t.Fatal(err)
	}
	a.sync()
	a.m.SyncAll(context.Background())
	if rp, _ := a.m.RemoteProjects(context.Background()); len(rp) != 0 {
		t.Fatalf("a paused project uploaded: %+v", rp)
	}
	eq(t, "status", a.status().Status, "paused")
	p.Paused = false
	a.m.SetProject(p)
	a.sync()
	if rp, _ := a.m.RemoteProjects(context.Background()); len(rp) != 1 {
		t.Fatalf("resuming didn't sync: %+v", rp)
	}
}

// "Don't sync these" is per PC: it neither sends nor receives those files, and
// never makes the other PCs think they were deleted.
func TestDontSyncTheseList(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.write("keep.txt", "1", time.Hour)
	a.write("secret.env", "s1", time.Hour)
	a.write("logs/a.log", "l1", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.sync()
	eq(t, "B got everything", len(b.list()), 3)

	p, _ := b.m.Get(key)
	p.Ignore = []string{"*.env", "logs/"}
	if err := b.m.SetProject(p); err != nil {
		t.Fatal(err)
	}
	b.write("other.env", "mine", time.Hour)
	b.write("logs/b.log", "mine", time.Hour)
	b.write("keep.txt", "2", time.Hour)
	b.sync()
	a.sync()
	eq(t, "A keeps its files", a.read("secret.env"), "s1")
	eq(t, "B's new ignored files didn't go to A", a.read("other.env"), "<missing>")
	eq(t, "B's ordinary edit did", a.read("keep.txt"), "2")

	a.write("secret.env", "s2", time.Hour)
	os.Remove(filepath.Join(a.folder, "logs", "a.log"))
	a.sync()
	b.sync()
	eq(t, "B's ignored file isn't replaced", b.read("secret.env"), "s1")
	eq(t, "B's ignored file isn't deleted", b.read("logs/a.log"), "l1")
	if st := b.status(); st.Error != "" || st.Decision != nil {
		t.Fatalf("B ended up confused: %+v", st)
	}
}

func TestExplainSpeaksPlainly(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "My Sketch")
	cases := []struct {
		err  error
		want string
	}{
		{client.ErrUnauthorized, "doesn't recognise this PC"},
		{client.ErrPinMismatch, "certificate changed"},
		{&fs.PathError{Op: "stat", Path: folder, Err: fs.ErrNotExist}, "isn't there"},
		{&fs.PathError{Op: "open", Path: filepath.Join(folder, "x"), Err: fs.ErrPermission}, "isn't allowed"},
		{errors.New("write x: no space left on device"), "disk is full"},
		{errors.New("The process cannot access the file because it is being used by another process."), "open in another program"},
		{errors.New("chacha20poly1305: message authentication failed"), "passphrase"},
		{&client.APIError{Status: 413, Msg: "too big"}, "too big"},
		{&client.APIError{Status: 502, Msg: "bad gateway"}, "server hit an error"},
		{context.DeadlineExceeded, "Can't reach"},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, "Can't reach"},
		{errors.New("something nobody planned for"), "Sync failed: something nobody planned for"},
	}
	for _, c := range cases {
		pr := Explain(c.err, folder)
		if !strings.Contains(pr.What, c.want) || pr.Fix == "" {
			t.Errorf("%v\n  said %+v\n  wanted %q", c.err, pr, c.want)
		}
	}
	if pr := Explain(nil, folder); pr != (Problem{}) {
		t.Errorf("nil error explained as %+v", pr)
	}
}

func TestFoldersCantOverlap(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	data := filepath.Join(root, "data")
	m, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	add := func(k, f string) error {
		return m.SetProject(Project{Key: k, Folder: f, Mode: ModeBackup})
	}
	proj := filepath.Join(home, "proj")
	if err := add("a", proj); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]string{
		"same folder":     proj,
		"inside":          filepath.Join(proj, "sub"),
		"contains":        home,
		"home":            home,
		"drive root":      string(filepath.Separator),
		"relative":        "proj2",
		"ClaudeSync data": filepath.Join(data, "projects"),
	} {
		if err := add("b", f); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := add("b", filepath.Join(home, "proj2")); err != nil {
		t.Errorf("a separate folder was refused: %v", err)
	}
	// Changing an existing project's settings never re-checks its folder.
	m.projects["a"].Folder = filepath.Join(proj, "legacy")
	m.projects["c"] = &Project{Key: "c", Folder: filepath.Join(proj, "legacy", "deeper"), Mode: ModeBackup}
	if err := m.SetProject(Project{Key: "c", Folder: filepath.Join(proj, "legacy", "deeper"), Mode: ModeSync}); err != nil {
		t.Errorf("changing the mode of an already-nested project: %v", err)
	}
}

func TestInspectFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	put := func(rel string, size int) {
		f := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("main.ino", 100)
	put("lib/util.h", 50)
	put("node_modules/dep/index.js", 999)
	put(".env", 10)
	put("id_rsa", 10)
	put("big.bin", 3<<20)
	put("scratch/notes.txt", 70)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)

	fi, err := Inspect(dir, 2, []string{"scratch/"})
	if err != nil {
		t.Fatal(err)
	}
	if fi.Files != 4 || fi.Bytes != 170 {
		t.Errorf("counted %d files, %d bytes: %+v", fi.Files, fi.Bytes, fi)
	}
	if len(fi.Skipped) != 1 || fi.Skipped[0].Path != "big.bin" || !strings.Contains(fi.Skipped[0].Why, "per-file limit") {
		t.Errorf("skipped: %+v", fi.Skipped)
	}
	if strings.Join(fi.Ignored, ",") != ".git/,node_modules/,scratch/" {
		t.Errorf("ignored: %v", fi.Ignored)
	}
	if len(fi.Sensitive) != 2 || fi.Git == nil || fi.Git.Branch != "main" || fi.HasRemote {
		t.Errorf("sensitive/git: %+v", fi)
	}
	if !strings.Contains(strings.Join(fi.Warnings, "\n"), "passwords or keys") {
		t.Errorf("no warning about secrets: %v", fi.Warnings)
	}
	if _, err := Inspect(filepath.Join(dir, "nope"), 0, nil); err == nil {
		t.Error("inspecting a missing folder worked")
	}
}

func setGitHead(p *pc, branch, commit string) {
	os.MkdirAll(filepath.Join(p.folder, ".git", "refs", "heads"), 0o755)
	os.WriteFile(filepath.Join(p.folder, ".git", "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644)
	os.WriteFile(filepath.Join(p.folder, ".git", "refs", "heads", branch), []byte(commit+"\n"), 0o644)
}

// "Not now" means not now: the same question isn't asked again at every sync,
// only when what it's about changes or the user asks.
func TestNotNowSticks(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	setGitHead(a, "main", "aaa111")
	a.write("src.c", "main", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	setGitHead(b, "main", "bbb222")
	b.sync()
	if d := b.status().Decision; d == nil || d.Kind != "git" {
		t.Fatalf("expected the git question: %+v", d)
	}
	if err := b.m.Decide(key, "no"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // scheduled syncs, or the app re-checking
		b.sync()
	}
	st := b.status()
	if st.Decision != nil || st.OnHold == "" || !strings.Contains(st.Status, "on hold") {
		t.Fatalf("asked again, or not shown as on hold: %+v", st)
	}
	eq(t, "nothing synced", b.read("src.c"), "<missing>")

	// A new commit on either PC is a different question.
	setGitHead(b, "main", "ccc333")
	b.sync()
	if d := b.status().Decision; d == nil {
		t.Fatal("a different situation wasn't asked about")
	}
	// Asking again on request.
	b.m.Decide(key, "no")
	b.sync()
	if b.status().Decision != nil {
		t.Fatal("snooze lost")
	}
	b.m.AskAgain(key)
	b.sync()
	if b.status().Decision == nil || b.status().OnHold != "" {
		t.Fatalf("Ask me again didn't ask: %+v", b.status())
	}
}

// "Sync anyway" is remembered for those two checkouts, not asked every time.
func TestSyncAnywayIsRemembered(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	setGitHead(a, "main", "aaa111")
	a.write("src.c", "main", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	setGitHead(b, "main", "bbb222")
	b.sync()
	b.m.Decide(key, "yes")
	b.sync()
	eq(t, "synced", b.read("src.c"), "main")
	a.write("src.c", "main 2", time.Hour)
	a.sync()
	b.sync()
	if b.status().Decision != nil {
		t.Fatalf("asked again about the same two checkouts: %+v", b.status().Decision)
	}
	eq(t, "kept syncing", b.read("src.c"), "main 2")
	setGitHead(a, "main", "ddd444") // A moves on
	a.write("src.c", "main 3", time.Hour)
	a.sync()
	b.sync()
	if b.status().Decision == nil {
		t.Fatal("a new commit on the other PC wasn't asked about")
	}
}
