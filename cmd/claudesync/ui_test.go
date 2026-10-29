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
	"github.com/Simplyph0/claudesync/internal/projsync"
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

	// Project folders: the laptop syncs "sketch"; this PC links its own,
	// different copy, which waits for a decision.
	old := time.Now().Add(-time.Hour)
	mkProj := func(folder, file, content string) {
		os.MkdirAll(folder, 0o755)
		os.WriteFile(filepath.Join(folder, file), []byte(content), 0o644)
		os.Chtimes(filepath.Join(folder, file), old, old)
	}
	pmB, _ := projsync.Open(t.TempDir())
	pmB.API, pmB.Keys, pmB.DeviceID, pmB.DeviceName = cl2, projsync.NewKeys(key), other.DeviceID, "Laptop"
	fb := filepath.Join(t.TempDir(), "sketch")
	mkProj(fb, "sketch.ino", "laptop version")
	pmB.SetProject(projsync.Project{Key: "name:sketch", Name: "sketch", Folder: fb, Mode: projsync.ModeSync})
	if err := pmB.Sync(t.Context(), "name:sketch"); err != nil {
		t.Fatal(err)
	}
	pmA, _ := projsync.Open(t.TempDir())
	pmA.API, pmA.Keys, pmA.DeviceID, pmA.DeviceName = cl, projsync.NewKeys(key), resp.DeviceID, "Desktop"
	fa := filepath.Join(t.TempDir(), "sketch")
	mkProj(fa, "sketch.ino", "desktop version")
	mkProj(fa, "notes.txt", "x")
	pmA.SetProject(projsync.Project{Key: "name:sketch", Name: "sketch", Folder: fa, Mode: projsync.ModeSync})
	pmA.SetProject(projsync.Project{Key: "name:website", Name: "website", Folder: t.TempDir(), Mode: projsync.ModeBackup, Ignore: []string{"*.log", "build/"}})
	// a folder that has gone missing (an unplugged drive, say)
	pmA.SetProject(projsync.Project{Key: "name:notes", Name: "notes", Folder: filepath.Join(t.TempDir(), "gone"), Mode: projsync.ModeBackup})
	a.eng.ProjectSync = pmA

	_ = a.eng.Sync(t.Context()) // Claude "open": the remaining session waits; sketch asks how to link
	a.log.Printf("ClaudeSync started")
	a.log.Printf("restore: applied 3 item(s) from other PCs")

	var save func(name string, c fyne.CanvasObject)
	snap := func(name string) { save(name, nil) }
	save = func(name string, c fyne.CanvasObject) {
		if c != nil {
			fyne.DoAndWait(func() { a.win.SetContent(c) })
		}
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
	proj, _ := a.projectsTab()
	inner := proj.(*container.AppTabs)
	for i, name := range []string{"folders", "server", "chats"} {
		fyne.DoAndWait(func() { inner.SelectIndex(i) })
		time.Sleep(300 * time.Millisecond) // "On the server" loads in the background
		save("projects-"+name, inner)
	}
	var waiting, broken bool
	for _, s := range pmA.Projects() {
		waiting = waiting || s.Decision != nil
		broken = broken || (s.Error != "" && s.Fix != "")
	}
	if !waiting || !broken {
		t.Fatalf("expected one folder waiting for a decision and one explained problem: %+v", pmA.Projects())
	}
	// the dialogs on top of the Projects tab
	tabs2 := a.mainUI().(*container.AppTabs)
	fyne.DoAndWait(func() { tabs2.SelectIndex(1); a.win.SetContent(tabs2) })
	fyne.DoAndWait(func() { a.projectDetails("name:notes") })
	snap("dialog-details-problem")
	fyne.DoAndWait(func() { a.win.Canvas().Overlays().Remove(a.win.Canvas().Overlays().Top()) })
	fyne.DoAndWait(func() { a.projectDetails("name:sketch") })
	snap("dialog-details-sketch")
	fyne.DoAndWait(func() { a.win.Canvas().Overlays().Remove(a.win.Canvas().Overlays().Top()) })
	add := filepath.Join(t.TempDir(), "arduino-clock")
	mkProj(add, "clock.ino", "void setup(){}")
	mkProj(add, ".env", "WIFI_PASSWORD=x")
	os.MkdirAll(filepath.Join(add, "node_modules", "x"), 0o755)
	mkProj(add, "node_modules/x/index.js", "x")
	fi, err := projsync.Inspect(add, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	fyne.DoAndWait(func() { a.confirmAddFolder(add, fi) })
	snap("dialog-add-folder")
	fyne.DoAndWait(func() { a.win.Canvas().Overlays().Remove(a.win.Canvas().Overlays().Top()) })
	fyne.DoAndWait(func() { a.win.Resize(fyne.NewSize(760, 1500)) })
	save("settings-full", a.settingsTab())
}
