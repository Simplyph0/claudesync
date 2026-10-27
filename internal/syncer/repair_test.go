package syncer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Simplyph0/claudesync/internal/proto"
)

// A session from a Linux PC restored on Windows must be valid JSON with
// backslashes escaped (ClaudeSync 0.1 wrote them unescaped).
func TestLinuxPathsIntoWindowsJSON(t *testing.T) {
	linux := []Root{{"PROJECT", "/home/bob/app"}, {"HOME", "/home/bob"}}
	win := []Root{{"PROJECT", `C:\Users\Al\Documents\app`}, {"HOME", `C:\Users\Al`}}
	rec := `{"cwd":"/home/bob/app","gitAnchors":[{"gitRoot":"/home/bob/app","commonDir":"/home/bob/app/.git"}],"note":"/home/bob/notes"}`
	out := Localize(Portable(rec, linux), win, true)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if m["cwd"] != `C:\Users\Al\Documents\app` {
		t.Fatalf("cwd = %v", m["cwd"])
	}
	// Plain-text files keep plain paths.
	if got := Localize(Portable("see /home/bob/app/x", linux), win, false); got != `see C:\Users\Al\Documents\app/x` {
		t.Fatalf("text file: %s", got)
	}
}

// Files already broken by 0.1 are repaired, the originals are snapshotted,
// and the session syncs normally afterwards.
func TestRepairBrokenFiles(t *testing.T) {
	w := newWorld(t)
	p := w.pc("Windows", "code")
	if !strings.Contains(p.project, `\`) {
		t.Skip("needs a PC whose paths contain backslashes (Windows)")
	}
	broken := `{"sessionId":"local_aaaa","cliSessionId":"` + cliA + `","cwd":"` + p.project + `","originCwd":"` + p.project + `","title":"From Linux"}`
	recPath := filepath.Join(p.sessionsDir(), "local_aaaa.json")
	os.WriteFile(recPath, []byte(broken), 0o644)
	os.MkdirAll(p.transcriptDir(), 0o755)
	trPath := filepath.Join(p.transcriptDir(), cliA+".jsonl")
	os.WriteFile(trPath, []byte(`{"type":"user","cwd":"`+p.project+`","message":"hi"}`+"\n"), 0o644)
	if json.Valid([]byte(broken)) {
		t.Fatal("test setup: record should be invalid")
	}
	p.eng.MapProject("name:myapp", p.project)

	p.sync()
	rec, _ := os.ReadFile(recPath)
	var m map[string]any
	if err := json.Unmarshal(rec, &m); err != nil || m["cwd"] != p.project {
		t.Fatalf("record not repaired: %v %s", err, rec)
	}
	tr, _ := os.ReadFile(trPath)
	if !validJSONFile(trPath, tr) {
		t.Fatalf("transcript not repaired: %s", tr)
	}
	snaps := ListSnapshots(p.eng.SnapshotDir)
	if len(snaps) == 0 {
		t.Fatal("originals not snapshotted before repair")
	}
	if st := p.eng.Status(); st.Uploaded != 1 || len(st.Corrupt) != 0 {
		t.Fatalf("after repair: uploaded %d, corrupt %v", st.Uploaded, st.Corrupt)
	}
}

// An unreadable record is never uploaded over the good copy, and a PC that
// receives an unreadable latest version uses the newest readable one.
func TestUnreadableVersionsDontWin(t *testing.T) {
	w := newWorld(t)
	desk := w.pc("Desktop", "code")
	desk.addSession("local_aaaa", cliA, "Good chat")
	desk.sync()

	// Corrupt beyond repair on the desktop: must not be uploaded.
	os.WriteFile(filepath.Join(desk.sessionsDir(), "local_aaaa.json"), []byte(`{"sessionId": broken`), 0o644)
	desk.sync()
	vs, _ := desk.eng.API.Versions(context.Background(), proto.KindSession, "local_aaaa")
	if len(vs) != 1 {
		t.Fatalf("unreadable record was uploaded: %d versions", len(vs))
	}

	// Simulate 0.1 having uploaded an unreadable copy as the newest version.
	bad := &Item{M: Manifest{Format: FormatVersion, Kind: proto.KindSession, ID: "local_aaaa", Title: "local_aaaa", UnknownFormat: true},
		Files: []File{{RoleDesktop, acct + "/local_aaaa.json", []byte(`{"broken`)}}}
	blob, _ := Seal(bad, w.key)
	if _, err := desk.eng.API.Put(context.Background(), proto.KindSession, "local_aaaa", 1, blob); err != nil {
		t.Fatal(err)
	}
	lap := w.pc("Laptop", "code")
	lap.sync()
	if s := lap.sessions(); len(s) != 1 || s[0] != "Good chat" {
		t.Fatalf("laptop got %v, want the readable version", s)
	}
}
