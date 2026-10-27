package main

import (
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"

	"github.com/Simplyph0/claudesync/internal/appcfg"
	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/server"
	"github.com/Simplyph0/claudesync/internal/syncer"
	"github.com/Simplyph0/claudesync/internal/vault"
)

// TestScreens renders every screen; set CLAUDESYNC_SCREENS=dir to save PNGs.
func TestScreens(t *testing.T) {
	out := os.Getenv("CLAUDESYNC_SCREENS")
	st, _ := server.Open(t.TempDir())
	_ = st.SetPassword("password123")
	ts := httptest.NewServer(server.New(st).Handler())
	defer ts.Close()
	cl := client.New("", ts.URL, "", "")
	resp, err := cl.Login(t.Context(), "password123", "Desktop")
	if err != nil {
		t.Fatal(err)
	}

	a := &App{log: newLogBuf(t.TempDir()), resched: make(chan struct{}, 1), ctx: t.Context()}
	t.Cleanup(func() { a.log.file.Close() })
	a.fa = test.NewTempApp(t)
	a.win = test.NewTempWindow(t, nil)
	a.win.Resize(fyne.NewSize(760, 560))
	a.cfg = appcfg.Defaults()
	a.cfg.DeviceName, a.cfg.TunnelURL, a.cfg.DeviceID = "Desktop", ts.URL, resp.DeviceID
	_, key, _ := vault.New("passphrase")
	desk := t.TempDir()
	os.MkdirAll(filepath.Join(desk, "claude-code-sessions"), 0o755)
	a.eng = &syncer.Engine{API: cl, Key: key, DeviceID: resp.DeviceID, StatePath: filepath.Join(t.TempDir(), "s.json"), SnapshotDir: t.TempDir(),
		Scanner: &syncer.Scanner{Loc: claude.Locations{Home: t.TempDir(), ClaudeHome: t.TempDir(), DesktopDir: desk},
			AppRunning: func() bool { return true }}}
	if err := a.eng.Init(); err != nil {
		t.Fatal(err)
	}
	// Sample data from "another PC": two sessions, one of them removed.
	other, _ := client.New("", ts.URL, "", "").Login(t.Context(), "password123", "Laptop")
	cl2 := client.New("", ts.URL, "", other.Token)
	for _, s := range []struct{ id, title string }{{"local_a", "Fix the parser"}, {"local_b", "Old experiment"}} {
		it := &syncer.Item{M: syncer.Manifest{Format: syncer.FormatVersion, Kind: "session", ID: s.id, Title: s.title}}
		blob, _ := syncer.Seal(it, key)
		if _, err := cl2.Put(t.Context(), "session", s.id, 0, blob, syncer.SealMeta(key, "session", s.id, s.title)); err != nil {
			t.Fatal(err)
		}
	}
	cl2.Remove(t.Context(), "session", "local_b")
	_ = a.eng.Sync(t.Context()) // Claude "open": the remaining session waits
	a.log.Printf("ClaudeSync started")
	a.log.Printf("restore: applied 3 item(s) from other PCs")

	save := func(name string, c fyne.CanvasObject) {
		fyne.DoAndWait(func() { a.win.SetContent(c) })
		time.Sleep(300 * time.Millisecond)
		if out == "" {
			return
		}
		_ = os.MkdirAll(out, 0o755)
		f, err := os.Create(filepath.Join(out, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_ = png.Encode(f, a.win.Canvas().Capture())
	}
	save("setup", a.setupUI())
	save("passphrase", a.passphraseUI(a.cfg, cl, "", true))
	tabs := a.mainUI().(*container.AppTabs)
	for i, name := range []string{"status", "projects", "history", "settings", "recover", "server", "log"} {
		fyne.DoAndWait(func() { tabs.SelectIndex(i) })
		save("tab-"+name, tabs)
	}
}
