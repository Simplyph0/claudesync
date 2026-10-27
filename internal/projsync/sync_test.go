package projsync

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/proto"
	"github.com/Simplyph0/claudesync/internal/server"
	"github.com/Simplyph0/claudesync/internal/vault"
)

type world struct {
	t     *testing.T
	root  string
	url   string
	keys  Keys
	store *server.Store
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	st, err := server.Open(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetPassword("correct horse")
	srv := server.New(st)
	srv.Logf = func(string, ...any) {}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	_, key, _ := vault.New("passphrase")
	return &world{t: t, root: root, url: ts.URL, keys: NewKeys(key), store: st}
}

// pc is a simulated computer with one project folder.
type pc struct {
	t      *testing.T
	name   string
	folder string
	m      *Manager
	clock  time.Time
}

const key = "name:sketch"

func (w *world) pc(name string, mode string) *pc {
	cl := client.New("", w.url, "", "")
	resp, err := cl.Login(context.Background(), "correct horse", name)
	if err != nil {
		w.t.Fatal(err)
	}
	m, err := Open(filepath.Join(w.root, name, "data"))
	if err != nil {
		w.t.Fatal(err)
	}
	p := &pc{t: w.t, name: name, folder: filepath.Join(w.root, name, "sketch"), clock: time.Now()}
	m.API, m.Keys, m.DeviceID, m.DeviceName = cl, w.keys, resp.DeviceID, name
	m.Now = func() time.Time { return p.clock }
	m.Logf = func(f string, a ...any) { w.t.Logf(name+": "+f, a...) }
	p.m = m
	os.MkdirAll(p.folder, 0o755)
	if mode != "" {
		if err := m.SetProject(Project{Key: key, Name: "sketch", Folder: p.folder, Mode: mode}); err != nil {
			w.t.Fatal(err)
		}
	}
	return p
}

// write creates a file with a modification time in the past (so it's settled).
func (p *pc) write(rel, content string, age time.Duration) {
	f := filepath.Join(p.folder, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(f), 0o755)
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
	mt := p.clock.Add(-age)
	os.Chtimes(f, mt, mt)
}

func (p *pc) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(p.folder, filepath.FromSlash(rel)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (p *pc) list() []string {
	var out []string
	filepath.WalkDir(p.folder, func(f string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(p.folder, f)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (p *pc) sync() {
	p.t.Helper()
	p.clock = p.clock.Add(time.Minute)
	if err := p.m.Sync(context.Background(), key); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pc) status() Status {
	for _, s := range p.m.Projects() {
		if s.Key == key {
			return s
		}
	}
	return Status{}
}

func (p *pc) trashHas(rel, content string) bool {
	for _, b := range p.m.Trash().List() {
		data, err := os.ReadFile(filepath.Join(p.m.Trash().Dir, b.ID, "files", filepath.FromSlash(rel)))
		if err == nil && string(data) == content {
			return true
		}
	}
	return false
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !equal(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func equal(a, b any) bool {
	as, aok := a.([]string)
	bs, bok := b.([]string)
	if aok && bok {
		return strings.Join(as, "|") == strings.Join(bs, "|")
	}
	return a == b
}

func TestBackupAndDownloadACopy(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeBackup)
	a.write("main.ino", "void setup(){}", time.Hour)
	a.write("node_modules/x.js", "ignored", time.Hour)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<17) // 2 MB, several chunks
	os.WriteFile(filepath.Join(a.folder, "data.bin"), big, 0o644)
	old := a.clock.Add(-time.Hour)
	os.Chtimes(filepath.Join(a.folder, "data.bin"), old, old)
	a.sync()
	eq(t, "status", a.status().Status, "backed up")
	a.sync() // nothing changed: no new version
	rp, _ := a.m.RemoteProjects(context.Background())
	if len(rp) != 1 || !rp[0].Backup || rp[0].Version != 1 || !strings.Contains(rp[0].Title, "backup from A") {
		t.Fatalf("server projects: %+v", rp)
	}

	b := w.pc("B", "")
	dst := filepath.Join(w.root, "B", "copy")
	n, err := b.m.Download(context.Background(), rp[0].ID, 0, dst, false)
	if err != nil || n != 2 {
		t.Fatalf("download: %d %v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "data.bin"))
	if !bytes.Equal(got, big) {
		t.Fatal("big file differs after download")
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); err == nil {
		t.Fatal("ignored folder was backed up")
	}
}

func TestTwoWaySync(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.write("main.ino", "v1", time.Hour)
	a.write("lib/util.h", "util", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync) // empty folder: downloads everything
	b.sync()
	eq(t, "B files", b.list(), []string{"lib/util.h", "main.ino"})

	// Different files edited on each PC.
	a.write("main.ino", "v2 from A", time.Second*30)
	b.write("lib/util.h", "util from B", time.Second*30)
	a.sync()
	b.sync()
	a.sync()
	eq(t, "A main", a.read("main.ino"), "v2 from A")
	eq(t, "A util", a.read("lib/util.h"), "util from B")
	eq(t, "B main", b.read("main.ino"), "v2 from A")

	// Deleted on A: moved to trash on B (default), not lost.
	os.Remove(filepath.Join(a.folder, "lib", "util.h"))
	a.sync()
	b.sync()
	eq(t, "B after delete", b.list(), []string{"main.ino"})
	if !b.trashHas("lib/util.h", "util from B") {
		t.Fatal("deleted file not in B's trash")
	}
	// Edited on B while deleted on A: the edit wins.
	b.write("gone.txt", "x", time.Hour)
	b.sync()
	a.sync()
	os.Remove(filepath.Join(a.folder, "gone.txt"))
	b.write("gone.txt", "edited", time.Second*30)
	a.sync()
	b.sync()
	a.sync()
	eq(t, "edit beats delete", a.read("gone.txt"), "edited")

	// Trash can put files back.
	restored := false
	for _, bt := range b.m.Trash().List() {
		for _, f := range bt.Files {
			if f.Rel == "lib/util.h" && !restored {
				if _, err := b.m.Trash().Restore(bt.ID, []string{"lib/util.h"}); err != nil {
					t.Fatal(err)
				}
				restored = true
			}
		}
	}
	eq(t, "restored from trash", b.read("lib/util.h"), "util from B")
}

func clashSetup(t *testing.T, policy string) (*world, *pc, *pc) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.m.Settings.ClashPolicy = policy
	a.write("notes.txt", "1\n2\n3\n4\n5\n6\n7\n8\n", time.Hour)
	a.write("img.bin", "\x00base", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.m.Settings.ClashPolicy = policy
	b.sync()
	return w, a, b
}

func TestTextEditsMerge(t *testing.T) {
	_, a, b := clashSetup(t, ClashKeepBoth)
	a.write("notes.txt", "1\nTWO\n3\n4\n5\n6\n7\n8\n", 30*time.Second)
	b.write("notes.txt", "1\n2\n3\n4\n5\n6\nSEVEN\n8\n", 30*time.Second)
	a.sync()
	b.sync()
	a.sync()
	want := "1\nTWO\n3\n4\n5\n6\nSEVEN\n8\n"
	eq(t, "B merged", b.read("notes.txt"), want)
	eq(t, "A merged", a.read("notes.txt"), want)
	if strings.Contains(strings.Join(b.list(), " "), "from A") {
		t.Fatal("merged file also produced a conflict copy")
	}
	if !b.trashHas("notes.txt", "1\n2\n3\n4\n5\n6\nSEVEN\n8\n") {
		t.Fatal("B's pre-merge version not in trash")
	}
}

func TestClashKeepBoth(t *testing.T) {
	_, a, b := clashSetup(t, ClashKeepBoth)
	a.write("img.bin", "\x00from A", 30*time.Second)
	b.write("img.bin", "\x00from B", 30*time.Second)
	a.sync()
	b.sync()
	a.sync()
	// B spotted the clash: its own file stays put and A's arrives as a copy.
	// Every PC ends up with both versions under the same names.
	eq(t, "B keeps its own", b.read("img.bin"), "\x00from B")
	copyName := ""
	for _, f := range b.list() {
		if strings.HasPrefix(f, "img (from A ") {
			copyName = f
		}
	}
	if copyName == "" {
		t.Fatalf("no conflict copy on B: %v", b.list())
	}
	eq(t, "copy content", b.read(copyName), "\x00from A")
	eq(t, "same files on both PCs", a.list(), b.list())
	eq(t, "A has B's at the path", a.read("img.bin"), "\x00from B")
	eq(t, "A has its own as the copy", a.read(copyName), "\x00from A")
	// Overlapping text edits are a clash too.
	a.write("notes.txt", "1\nA\n3\n4\n5\n6\n7\n8\n", 20*time.Second)
	b.write("notes.txt", "1\nB\n3\n4\n5\n6\n7\n8\n", 20*time.Second)
	a.sync()
	b.sync()
	eq(t, "B text", b.read("notes.txt"), "1\nB\n3\n4\n5\n6\n7\n8\n")
}

func TestClashNewestWins(t *testing.T) {
	_, a, b := clashSetup(t, ClashNewest)
	a.write("img.bin", "\x00older", 50*time.Minute)
	b.write("img.bin", "\x00newer", 20*time.Second)
	a.sync()
	b.sync()
	a.sync()
	eq(t, "A gets newest", a.read("img.bin"), "\x00newer")
	if !a.trashHas("img.bin", "\x00older") {
		t.Fatal("losing version not in trash")
	}
	if len(a.list()) != 2 {
		t.Fatalf("unexpected files: %v", a.list())
	}
}

func TestClashAskMe(t *testing.T) {
	_, a, b := clashSetup(t, ClashAsk)
	a.write("img.bin", "\x00from A", 30*time.Second)
	b.write("img.bin", "\x00from B", 30*time.Second)
	a.write("other.txt", "unrelated", 30*time.Second)
	a.sync()
	b.sync()
	st := b.status()
	if len(st.Clashes) != 1 || st.Clashes[0].Path != "img.bin" {
		t.Fatalf("clashes: %+v", st.Clashes)
	}
	eq(t, "rest still syncs", b.read("other.txt"), "unrelated")
	eq(t, "B untouched while asking", b.read("img.bin"), "\x00from B")
	b.sync() // asking again doesn't change anything
	eq(t, "still asking", len(b.status().Clashes), 1)
	if err := b.m.ResolveClash(key, "img.bin", "theirs"); err != nil {
		t.Fatal(err)
	}
	b.sync()
	eq(t, "took theirs", b.read("img.bin"), "\x00from A")
	eq(t, "clash gone", len(b.status().Clashes), 0)
	if !b.trashHas("img.bin", "\x00from B") {
		t.Fatal("replaced version not in trash")
	}
}

func TestFirstLinkAskThenMerge(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.write("shared.txt", "same", time.Hour)
	a.write("a-only.txt", "a", time.Hour)
	a.write("both.bin", "\x00A", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.write("shared.txt", "same", time.Hour)
	b.write("b-only.txt", "b", time.Hour)
	b.write("both.bin", "\x00B", time.Hour)
	b.sync()
	d := b.status().Decision
	if d == nil || d.Kind != "first-link" || d.Preview.Identical != 1 || d.Preview.OnlyHere != 1 || d.Preview.OnlyServer != 1 || d.Preview.Differ != 1 {
		t.Fatalf("first-link decision: %+v %+v", d, d.Preview)
	}
	eq(t, "nothing changed while asking", b.list(), []string{"b-only.txt", "both.bin", "shared.txt"})
	if err := b.m.Decide(key, LinkMerge); err != nil {
		t.Fatal(err)
	}
	b.sync()
	a.sync()
	for _, p := range []*pc{a, b} {
		files := strings.Join(p.list(), " ")
		if !strings.Contains(files, "a-only.txt") || !strings.Contains(files, "b-only.txt") || !strings.Contains(files, "both (from") {
			t.Fatalf("%s after merge: %v", p.name, p.list())
		}
	}
}

func TestFirstLinkAlwaysUseServer(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.write("x.txt", "server", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.m.Settings.FirstLink = LinkServer
	b.write("x.txt", "local", time.Hour)
	b.write("extra.txt", "extra", time.Hour)
	b.sync()
	eq(t, "B matches server", b.list(), []string{"x.txt"})
	eq(t, "content", b.read("x.txt"), "server")
	if !b.trashHas("extra.txt", "extra") || !b.trashHas("x.txt", "local") {
		t.Fatal("B's own files not in trash")
	}
}

func TestDeletesAndBigChangesAsk(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	for i := 0; i < 60; i++ {
		a.write("f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", "x", time.Hour)
	}
	a.sync()
	b := w.pc("B", ModeSync)
	b.m.Settings.RemoteDeletes = DeletesAsk
	b.sync()
	for _, f := range a.list()[:55] {
		os.Remove(filepath.Join(a.folder, f))
	}
	a.sync()
	if d := a.status().Decision; d == nil || d.Kind != "local-deletes" {
		t.Fatalf("A removing 55 files didn't ask: %+v", d)
	}
	a.m.Decide(key, "yes")
	a.sync()
	b.sync()
	if d := b.status().Decision; d == nil || d.Kind != "deletes" || len(d.Paths) != 55 {
		t.Fatalf("expected a deletes decision: %+v", d)
	}
	eq(t, "nothing removed yet", len(b.list()), 60)
	b.m.Decide(key, "yes")
	b.sync() // OK'd removals aren't asked about again as a "big change"
	eq(t, "removed after OK", len(b.list()), 5)
	if b.status().Decision != nil {
		t.Fatalf("asked again: %+v", b.status().Decision)
	}

	// A PC whose folder suddenly empties is asked first, whatever the settings.
	c := w.pc("C", ModeSync)
	c.m.Settings.BigChange = BigContinue
	c.sync()
	eq(t, "C has the files", len(c.list()), 5)
	os.RemoveAll(c.folder)
	c.sync()
	if d := c.status().Decision; d == nil || d.Kind != "local-deletes" {
		t.Fatalf("emptied folder didn't ask: %+v", d)
	}
	a.sync()
	eq(t, "A unaffected while C asks", len(a.list()), 5)
	c.m.Decide(key, "restore")
	c.sync()
	eq(t, "put back on C", len(c.list()), 5)
	a.sync()
	eq(t, "still on A", len(a.list()), 5)

	// When the removal is on purpose, OK'ing it spreads it, and the other PCs'
	// big-change guard (set to ask) still applies to a large removal.
	d := w.pc("D", ModeSync)
	for i := 0; i < 60; i++ {
		d.write("big/"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", "x", time.Hour)
	}
	d.sync() // first link: D has files, server has 5 -> ask
	d.m.Decide(key, LinkMerge)
	d.sync()
	b.sync()
	eq(t, "B got D's files", len(b.list()), 65)
	os.RemoveAll(filepath.Join(d.folder, "big"))
	d.sync()
	if dd := d.status().Decision; dd == nil || dd.Kind != "local-deletes" {
		t.Fatalf("D removing 60 files didn't ask: %+v", dd)
	}
	d.m.Decide(key, "yes")
	d.sync()
	b.m.Settings.RemoteDeletes = DeletesTrash
	b.sync()
	if bd := b.status().Decision; bd == nil || bd.Kind != "big-change" {
		t.Fatalf("B didn't ask about a big change: %+v", bd)
	}
	eq(t, "B unchanged while asking", len(b.list()), 65)
	b.m.Decide(key, "yes")
	b.sync()
	eq(t, "B after OK", len(b.list()), 5)
}
func TestGitGuard(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	setGit := func(p *pc, branch string) {
		os.MkdirAll(filepath.Join(p.folder, ".git", "refs", "heads"), 0o755)
		os.WriteFile(filepath.Join(p.folder, ".git", "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644)
		os.WriteFile(filepath.Join(p.folder, ".git", "refs", "heads", branch), []byte("abc123\n"), 0o644)
	}
	setGit(a, "main")
	a.write("src.c", "main", time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	setGit(b, "feature")
	b.sync()
	if d := b.status().Decision; d == nil || d.Kind != "git" {
		t.Fatalf("expected the git guard to ask: %+v", d)
	}
	eq(t, "nothing synced", b.read("src.c"), "<missing>")
	if _, err := os.Stat(filepath.Join(w.root, "B", "sketch", ".git", "HEAD")); err != nil {
		t.Fatal(".git touched")
	}
	b.m.Decide(key, "yes")
	b.sync()
	eq(t, "synced after OK", b.read("src.c"), "main")
}

func TestStillBeingWrittenWaits(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.write("done.txt", "x", time.Hour)
	a.sync()
	a.write("busy.txt", "half", 0)
	a.clock = a.clock.Add(-time.Minute) // sync() adds a minute: file is 0s old at scan time
	a.sync()
	b := w.pc("B", ModeSync)
	b.sync()
	eq(t, "busy file not synced yet", b.read("busy.txt"), "<missing>")
	a.sync()
	b.sync()
	eq(t, "synced once settled", b.read("busy.txt"), "half")
}

func TestServerCleansUnusedChunks(t *testing.T) {
	w := newWorld(t)
	w.store.UpdateConfig(func(c *server.Config) { c.Retention = 1 })
	a := w.pc("A", ModeSync)
	a.write("f.bin", strings.Repeat("A", 100000), time.Hour)
	a.sync()
	a.write("f.bin", strings.Repeat("B", 100000), 30*time.Second)
	a.sync()
	n, err := w.store.GCChunks(0)
	if err != nil || n != 1 {
		t.Fatalf("GC removed %d (err %v), want the 1 chunk only the pruned version used", n, err)
	}
	b := w.pc("B", ModeSync)
	b.sync()
	eq(t, "latest still complete", b.read("f.bin"), strings.Repeat("B", 100000))
	// A manifest can't refer to chunks that aren't there.
	_, err = a.m.API.PutManifest(context.Background(), proto.KindProject, "prj_x", proto.ManifestUpload{Blob: []byte("x"), Refs: []string{"k" + strings.Repeat("0", 64)}})
	if err == nil {
		t.Fatal("manifest with missing chunks accepted")
	}
}

// Switching a project from Back up only to Sync must not treat the backup's
// file list as "what the PCs agreed on" (that made every file look deleted
// elsewhere, because the shared copy didn't exist yet).
func TestSwitchBackupToSyncKeepsFiles(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeBackup)
	a.m.Settings.BigChange = BigContinue // the dangerous setting: no guard to catch it
	for i := 0; i < 5; i++ {
		a.write(fmt.Sprintf("f%d.txt", i), "x", time.Hour)
	}
	a.sync()
	a.m.SetProject(Project{Key: key, Name: "sketch", Folder: a.folder, Mode: ModeSync})
	a.sync()
	eq(t, "files kept", len(a.list()), 5)
	if d := a.status().Decision; d != nil {
		t.Fatalf("unexpected question: %+v", d)
	}
	if len(a.m.Trash().List()) != 0 {
		t.Fatal("files went to trash")
	}
	b := w.pc("B", ModeSync)
	b.sync()
	eq(t, "B got them", len(b.list()), 5)
}

// With nothing on the server, local files are never treated as deleted.
func TestNoServerCopyNeverTrashes(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.m.Settings.BigChange = BigContinue
	a.write("x.txt", "x", time.Hour)
	a.sync()
	id := itemID(key)
	if _, err := w.store.SetRemoved(proto.KindProject, id, true); err != nil {
		t.Fatal(err)
	}
	w.store.PurgeRemoved(time.Now().Add(365 * 24 * time.Hour)) // server copy gone for good
	a.sync()
	eq(t, "still there", a.read("x.txt"), "x")
	if len(a.m.Trash().List()) != 0 {
		t.Fatal("files went to trash")
	}
}
