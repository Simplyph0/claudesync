package syncer

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	out := Localize(p, dst)
	want := `{"cwd":"/home/bob/src/app","f":"/home/bob/src/app/x.go","h":"/home/bob\\.claude","enc":"-home-bob-src-app","other":"C:\\Users\\alice"}`
	if out != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
}

// pc is a fake computer: its own home, Claude folders and a project.
type pc struct {
	home, project string
	eng           *Engine
}

const acct = "acct-1111/org-2222"

func newPC(t *testing.T, root, name, projectParent, url string, key []byte, cl *client.Client, deviceID string) *pc {
	home := filepath.Join(root, name)
	project := filepath.Join(home, projectParent, "myapp")
	desktop := filepath.Join(home, "desktop-data")
	for _, d := range []string{project, filepath.Join(desktop, "claude-code-sessions", filepath.FromSlash(acct)), filepath.Join(home, ".claude", "projects")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	loc := claude.Locations{Home: home, ClaudeHome: filepath.Join(home, ".claude"), DesktopDir: desktop}
	sc := &Scanner{Loc: loc, Opt: Options{Sessions: true, Config: true, Memory: true, Scratch: true, ScratchMaxBytes: 10 << 20, DeviceName: name}}
	e := &Engine{API: cl, Key: key, DeviceID: deviceID, Scanner: sc, StatePath: filepath.Join(home, "state.json"), Search: []string{home}}
	if err := e.Init(); err != nil {
		t.Fatal(err)
	}
	return &pc{home: home, project: project, eng: e}
}

func login(t *testing.T, url, name string) (*client.Client, string) {
	cl := client.New("", url, "", "")
	resp, err := cl.Login(context.Background(), "correct horse", name)
	if err != nil {
		t.Fatal(err)
	}
	return cl, resp.DeviceID
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

func TestTwoPCs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := server.Open(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st).Handler())
	defer ts.Close()

	_, key, err := vault.New("sync passphrase")
	if err != nil {
		t.Fatal(err)
	}
	clA, idA := login(t, ts.URL, "Desktop")
	clB, idB := login(t, ts.URL, "Laptop")
	a := newPC(t, root, "a", "Documents", ts.URL, key, clA, idA)
	b := newPC(t, root, "b", filepath.Join("work", "repos"), ts.URL, key, clB, idB)

	// A session on PC A.
	cli := "6a7a8b25-b02d-4b47-83ea-b893a968699e"
	sessFile := filepath.Join(a.home, "desktop-data", "claude-code-sessions", filepath.FromSlash(acct), "local_aaaa.json")
	writeJSON(t, sessFile, map[string]any{"sessionId": "local_aaaa", "cliSessionId": cli, "cwd": a.project, "originCwd": a.project, "title": "Fix the thing", "lastActivityAt": 1791426402117})
	tdirA := filepath.Join(a.home, ".claude", "projects", claude.EncodeProjectDir(a.project))
	line, _ := json.Marshal(map[string]any{"type": "user", "cwd": a.project, "sessionId": cli, "message": "edit " + filepath.Join(a.project, "main.go")})
	if err := os.MkdirAll(tdirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tdirA, cli+".jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(a.home, ".claude", "skills", "deploy", "SKILL.md"), "deploy from "+a.home)

	if err := a.eng.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	if s := a.eng.Status(); s.Uploaded != 2 {
		t.Fatalf("A uploaded %d items, want 2 (session + skill)", s.Uploaded)
	}

	// PC B restores; the project is found by folder name at a different path.
	if err := b.eng.Restore(ctx, true); err != nil {
		t.Fatal(err)
	}
	if p := b.eng.Status().Pending; len(p) != 0 {
		t.Fatalf("B still has pending restores: %+v", p)
	}
	rec, err := os.ReadFile(filepath.Join(b.home, "desktop-data", "claude-code-sessions", filepath.FromSlash(acct), "local_aaaa.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(rec, &m)
	if m["cwd"] != b.project {
		t.Fatalf("restored cwd = %v, want %s", m["cwd"], b.project)
	}
	tr, err := os.ReadFile(filepath.Join(b.home, ".claude", "projects", claude.EncodeProjectDir(b.project), cli+".jsonl"))
	if err != nil {
		t.Fatal("transcript not restored under B's project folder:", err)
	}
	if strings.Contains(string(tr), filepath.Base(a.home)+string(filepath.Separator)+"Documents") || !strings.Contains(string(tr), strings.ReplaceAll(filepath.Join(b.project, "main.go"), `\`, `\\`)) {
		t.Fatalf("transcript paths not rewritten: %s", tr)
	}
	skill, _ := os.ReadFile(filepath.Join(b.home, ".claude", "skills", "deploy", "SKILL.md"))
	if !strings.Contains(string(skill), strings.ReplaceAll(b.home, `\`, `\\`)) {
		t.Fatalf("skill not restored with B's home: %s", skill)
	}

	// Nothing changed, so B's backup uploads nothing.
	if err := b.eng.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	if s := b.eng.Status(); s.Uploaded != 0 {
		t.Fatalf("B re-uploaded %d unchanged items", s.Uploaded)
	}

	// Both PCs continue the session before syncing: keep both.
	appendLine := func(p *pc, text string) {
		f := filepath.Join(p.home, ".claude", "projects", claude.EncodeProjectDir(p.project), cli+".jsonl")
		fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		l, _ := json.Marshal(map[string]any{"type": "user", "sessionId": cli, "message": text})
		fh.Write(append(l, '\n'))
		fh.Close()
	}
	appendLine(a, "more from A")
	appendLine(b, "more from B")
	if err := a.eng.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.eng.Backup(ctx); err != nil { // conflicts: A got there first
		t.Fatal(err)
	}
	if err := b.eng.Restore(ctx, true); err != nil {
		t.Fatal(err)
	}
	recs, _ := filepath.Glob(filepath.Join(b.home, "desktop-data", "claude-code-sessions", filepath.FromSlash(acct), "local_*.json"))
	if len(recs) != 2 {
		t.Fatalf("B has %d sessions after conflict, want 2", len(recs))
	}
	foundCopy := false
	for _, r := range recs {
		b, _ := os.ReadFile(r)
		if strings.Contains(string(b), "Fix the thing (from Desktop)") {
			foundCopy = true
		}
	}
	if !foundCopy {
		t.Fatal("conflict copy titled '(from Desktop)' not found")
	}
	// B's own version and the copy upload; A then gets both.
	if err := b.eng.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.eng.Restore(ctx, true); err != nil {
		t.Fatal(err)
	}
	trA, _ := os.ReadFile(filepath.Join(tdirA, cli+".jsonl"))
	if !strings.Contains(string(trA), "more from B") {
		t.Fatal("A did not receive B's continuation")
	}
	recsA, _ := filepath.Glob(filepath.Join(a.home, "desktop-data", "claude-code-sessions", filepath.FromSlash(acct), "local_*.json"))
	if len(recsA) != 2 {
		t.Fatalf("A has %d sessions, want 2 (original + copy)", len(recsA))
	}
}
