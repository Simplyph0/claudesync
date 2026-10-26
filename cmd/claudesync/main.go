// Command claudesync is the desktop client: it backs up this PC's Claude
// sessions to a ClaudeSync server and restores the ones from your other PCs.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"

	"github.com/Simplyph0/claudesync/internal/appcfg"
	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/syncer"
)

// Local port used only to keep a single copy running and to bring its
// window forward when you open the app again.
const instanceAddr = "127.0.0.1:47613"

type App struct {
	fa  fyne.App
	win fyne.Window
	log *logBuf

	mu  sync.Mutex
	cfg appcfg.Settings
	sec appcfg.Secrets
	eng *syncer.Engine

	ctx       context.Context
	resched   chan struct{}
	lastAsked int
	trayMenu  *fyne.Menu
	refreshUI func()
	schedOnce sync.Once
}

func main() {
	hidden := flag.Bool("hidden", false, "start in the tray without opening the window")
	scan := flag.Bool("scan", false, "list what would be synced on this PC and exit")
	flag.Parse()
	if *scan {
		runScan()
		return
	}
	ln, err := net.Listen("tcp", instanceAddr)
	if err != nil { // already running: ask it to show its window
		if c, err := net.DialTimeout("tcp", instanceAddr, time.Second); err == nil {
			fmt.Fprintln(c, "show")
			c.Close()
		}
		return
	}

	a := &App{log: newLogBuf(appcfg.Dir()), resched: make(chan struct{}, 1), ctx: context.Background()}
	a.cfg, err = appcfg.Load()
	if err != nil {
		a.log.Printf("settings unreadable, using defaults: %v", err)
	}
	a.sec = appcfg.LoadSecrets()

	a.fa = app.NewWithID("io.github.simplyph0.claudesync")
	a.fa.SetIcon(iconIdle)
	a.win = a.fa.NewWindow("ClaudeSync")
	a.win.SetIcon(iconIdle)
	a.win.Resize(fyne.NewSize(760, 560))
	a.win.SetCloseIntercept(func() { a.win.Hide() })

	go a.listenInstance(ln)
	a.setupTray()

	configured := a.sec.Token != "" && len(a.sec.Key) == 32 && (a.cfg.LANURL != "" || a.cfg.TunnelURL != "")
	if configured {
		if err := a.startEngine(); err != nil {
			a.log.Printf("start: %v", err)
			configured = false
		}
	}
	if configured {
		a.win.SetContent(a.mainUI())
		if !*hidden {
			a.win.Show()
		}
	} else {
		a.win.SetContent(a.setupUI())
		a.win.Show()
	}
	a.fa.Run()
}

func (a *App) listenInstance(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if line == "show\n" {
			fyne.Do(a.showWindow)
		}
	}
}

func (a *App) showWindow() {
	a.win.Show()
	a.win.RequestFocus()
}

func (a *App) setupTray() {
	desk, ok := a.fa.(desktop.App)
	if !ok {
		return
	}
	a.trayMenu = fyne.NewMenu("ClaudeSync",
		fyne.NewMenuItem("Open ClaudeSync", a.showWindow),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Back up now", func() { go a.doBackup() }),
		fyne.NewMenuItem("Restore now", func() { go a.doRestore(true) }),
		fyne.NewMenuItem("Restart Claude && apply updates", func() { go a.restartApply() }),
	)
	desk.SetSystemTrayMenu(a.trayMenu)
	desk.SetSystemTrayIcon(iconIdle)
}

func (a *App) setTrayIcon(r fyne.Resource) {
	if desk, ok := a.fa.(desktop.App); ok {
		desk.SetSystemTrayIcon(r)
	}
}

func (a *App) notify(title, body string) {
	a.fa.SendNotification(fyne.NewNotification(title, body))
}

func (a *App) settings() appcfg.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) engine() *syncer.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.eng
}

func scannerOptions(c appcfg.Settings) syncer.Options {
	return syncer.Options{Sessions: c.Sessions, CLIOnly: c.CLIOnly, Config: c.Config, Memory: c.Memory, Scratch: c.Scratch,
		ScratchMaxBytes: int64(c.ScratchMaxMB) << 20, DeviceName: c.DeviceName}
}

func (a *App) startEngine() error {
	c := a.settings()
	loc := claude.Detect(c.ClaudeHome, c.DesktopDir)
	e := &syncer.Engine{
		API:       client.New(c.LANURL, c.TunnelURL, c.Pin, a.sec.Token),
		Key:       a.sec.Key,
		DeviceID:  c.DeviceID,
		Scanner:   &syncer.Scanner{Loc: loc, Opt: scannerOptions(c)},
		StatePath: filepath.Join(appcfg.Dir(), "state.json"),
		Search:    c.SearchRoots,
		Logf:      a.log.Printf,
		Notify:    a.notify,
		OnChange: func() {
			fyne.Do(func() {
				if a.refreshUI != nil {
					a.refreshUI()
				}
			})
		},
	}
	if err := e.Init(); err != nil {
		return err
	}
	a.mu.Lock()
	a.eng = e
	a.mu.Unlock()
	a.log.Printf("ClaudeSync started. Claude data: %s; desktop app data: %s", loc.ClaudeHome, orNone(loc.DesktopDir))
	a.schedOnce.Do(func() { go a.scheduler() })
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(desktop app not found)"
	}
	return s
}

// applySettings pushes changed settings into the running engine.
func (a *App) applySettings(c appcfg.Settings) {
	a.mu.Lock()
	a.cfg = c
	e := a.eng
	a.mu.Unlock()
	if e == nil {
		return
	}
	e.Reconfigure(func(e *syncer.Engine) {
		e.API = client.New(c.LANURL, c.TunnelURL, c.Pin, a.sec.Token)
		e.Scanner.Loc = claude.Detect(c.ClaudeHome, c.DesktopDir)
		e.Scanner.Opt = scannerOptions(c)
		e.Search = c.SearchRoots
	})
	select {
	case a.resched <- struct{}{}:
	default:
	}
}

// scheduler runs restores and backups on their own intervals. A restore
// runs soon after start so a PC you sit back down at catches up first.
func (a *App) scheduler() {
	restoreT := time.NewTimer(5 * time.Second)
	backupT := time.NewTimer(30 * time.Second)
	for {
		select {
		case <-restoreT.C:
			a.doRestore(false)
			restoreT.Reset(time.Duration(a.settings().RestoreMinutes) * time.Minute)
		case <-backupT.C:
			a.doBackup()
			backupT.Reset(time.Duration(a.settings().BackupMinutes) * time.Minute)
		case <-a.resched:
			restoreT.Reset(time.Duration(a.settings().RestoreMinutes) * time.Minute)
			backupT.Reset(time.Duration(a.settings().BackupMinutes) * time.Minute)
		}
	}
}

func (a *App) doBackup() {
	e := a.engine()
	if e == nil {
		return
	}
	fyne.Do(func() { a.setTrayIcon(iconBusy) })
	err := e.Backup(a.ctx)
	a.afterRun("backup", err)
}

// doRestore pulls changes; if sessions are waiting for Claude to close it
// asks once per new batch whether to restart Claude.
func (a *App) doRestore(manual bool) {
	e := a.engine()
	if e == nil {
		return
	}
	fyne.Do(func() { a.setTrayIcon(iconBusy) })
	err := e.Restore(a.ctx, false)
	a.afterRun("restore", err)
	n := e.WaitingForApp()
	a.mu.Lock()
	ask := n > 0 && (manual || n > a.lastAsked)
	a.lastAsked = n
	a.mu.Unlock()
	if ask {
		fyne.Do(func() { a.askRestart(n) })
	}
}

func (a *App) afterRun(what string, err error) {
	icon := iconIdle
	if err != nil {
		icon = iconWarn
		a.log.Printf("%s: %v", what, err)
		if errors.Is(err, client.ErrUnauthorized) {
			a.notify("ClaudeSync: signed out", "This PC was removed from the server. Open ClaudeSync to sign in again.")
		}
	}
	fyne.Do(func() { a.setTrayIcon(icon) })
}

func (a *App) askRestart(n int) {
	a.showWindow()
	msg := fmt.Sprintf("%d session(s) from your other PCs are ready.\n\nClaude has to close for a moment to load them; it reopens straight after.\nRestart Claude now?", n)
	dialog.ShowConfirm("Updates ready", msg, func(ok bool) {
		if ok {
			go a.restartApply()
		}
	}, a.win)
}

func (a *App) restartApply() {
	e := a.engine()
	if e == nil {
		return
	}
	fyne.Do(func() { a.setTrayIcon(iconBusy) })
	err := e.RestartAndApply(a.ctx)
	a.afterRun("restart and apply", err)
	if err == nil {
		a.mu.Lock()
		a.lastAsked = 0
		a.mu.Unlock()
		a.notify("ClaudeSync", "Claude restarted with your latest sessions.")
	} else {
		fyne.Do(func() { dialog.ShowError(err, a.win) })
	}
}

// runScan prints what this PC would sync, without contacting a server.
func runScan() {
	c, _ := appcfg.Load()
	loc := claude.Detect(c.ClaudeHome, c.DesktopDir)
	fmt.Println("Claude Code data:", loc.ClaudeHome)
	fmt.Println("Desktop app data:", orNone(loc.DesktopDir))
	for _, alt := range loc.DesktopAlt {
		fmt.Println("  also known as:", alt)
	}
	fmt.Println("Desktop app running:", claude.DesktopRunning())
	sc := &syncer.Scanner{Loc: loc, Opt: scannerOptions(c)}
	res, err := sc.Scan()
	if err != nil {
		fmt.Println("scan failed:", err)
		os.Exit(1)
	}
	keys := make([]string, 0, len(res.Items))
	for k := range res.Items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := res.Items[k]
		it, err := l.Build()
		if err != nil {
			fmt.Printf("%-8s %-44s ERROR %v\n", l.Kind, l.ID, err)
			continue
		}
		var size int
		for _, f := range it.Files {
			size += len(f.Data)
		}
		proj := ""
		if it.M.Project != nil {
			proj = " project=" + it.M.Project.Key
		} else if it.M.ScratchRel != "" {
			proj = " folderless"
		}
		fmt.Printf("%-8s %-44s %4d files %7d KB  %q%s cwd=%s\n", l.Kind, l.ID, len(it.Files), size>>10, l.Title, proj, it.M.Cwd)
	}
	fmt.Printf("%d items; %d unrecognised desktop records; %d deleted markers\n", len(res.Items), res.UnknownFormat, len(res.Deleted))
}
