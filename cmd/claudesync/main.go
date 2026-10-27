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
	"github.com/Simplyph0/claudesync/internal/projsync"
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
	reopen    []claude.App // Claude as it was when ClaudeSync asked it to quit
	applying  bool
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
		fyne.NewMenuItem("Sync now", func() { go a.doSync(true) }),
		fyne.NewMenuItem("Close Claude and load updates", func() { go a.closeAndApply() }),
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
		API:         client.New(c.LANURL, c.TunnelURL, c.Pin, a.sec.Token),
		Key:         a.sec.Key,
		DeviceID:    c.DeviceID,
		Scanner:     &syncer.Scanner{Loc: loc, Opt: scannerOptions(c)},
		StatePath:   filepath.Join(appcfg.Dir(), "state.json"),
		SnapshotDir: snapshotDir(),
		Search:      c.SearchRoots,
		Logf:        a.log.Printf,
		Notify:      a.notify,
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
	pm, err := projsync.Open(filepath.Join(appcfg.Dir(), "projects"))
	if err != nil {
		return err
	}
	pm.API, pm.Keys, pm.DeviceID, pm.DeviceName = e.API, projsync.NewKeys(a.sec.Key), c.DeviceID, c.DeviceName
	pm.Settings, pm.Logf = projectSettings(c), a.log.Printf
	e.ProjectSync = pm
	a.mu.Lock()
	a.eng = e
	a.mu.Unlock()
	a.log.Printf("ClaudeSync started. Claude data: %s; desktop app data: %s", loc.ClaudeHome, orNone(loc.DesktopDir))
	if loc.Ambiguous {
		a.log.Printf("several Claude data folders found (%v); desktop sessions won't be restored until you pick one in Settings", loc.Candidates)
	}
	a.schedOnce.Do(func() {
		go a.scheduler()
		go a.watchAppClose()
	})
	return nil
}

func snapshotDir() string { return filepath.Join(appcfg.Dir(), "snapshots") }

// watchAppClose applies waiting updates as soon as Claude is closed, however
// it was closed.
func (a *App) watchAppClose() {
	for range time.Tick(3 * time.Second) {
		e := a.engine()
		if e == nil || e.WaitingForApp() == 0 || claude.DesktopRunning() {
			continue
		}
		time.Sleep(2 * time.Second) // let Claude finish writing its files
		if claude.DesktopRunning() {
			continue
		}
		a.log.Printf("Claude is closed; loading the waiting updates")
		a.applyAndReopen()
	}
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
		if e.ProjectSync != nil {
			e.ProjectSync.API = e.API
			e.ProjectSync.Settings = projectSettings(c)
			e.ProjectSync.DeviceName = c.DeviceName
		}
	})
	select {
	case a.resched <- struct{}{}:
	default:
	}
}

// scheduler runs syncs (backup, then restore) and plain backups on their own
// intervals. A sync runs soon after start so a PC you sit back down at
// catches up, always backing up its own sessions first.
func (a *App) scheduler() {
	restoreT := time.NewTimer(10 * time.Second)
	backupT := time.NewTimer(time.Duration(a.settings().BackupMinutes) * time.Minute)
	for {
		select {
		case <-restoreT.C:
			a.doSync(false)
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

// doSync backs up, then restores; if sessions are waiting for Claude to
// close it asks once per new batch.
func (a *App) doSync(manual bool) {
	e := a.engine()
	if e == nil {
		return
	}
	fyne.Do(func() { a.setTrayIcon(iconBusy) })
	a.addDefaultProjects()
	err := e.Sync(a.ctx)
	a.afterRun("sync", err)
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
	msg := fmt.Sprintf("%d session(s) from your other PCs are ready.\n\n"+
		"They load the next time Claude is closed: quit it whenever suits you.\n"+
		"This PC's own sessions are already backed up, and anything replaced is saved first.\n\n"+
		"Close Claude now and reopen it with the updates?", n)
	dialog.ShowConfirm("Updates ready", msg, func(ok bool) {
		if ok {
			go a.closeAndApply()
		}
	}, a.win)
}

const quitHelp = "If Claude only hides (it can keep running in the tray), quit it yourself:\n" +
	"right-click its tray icon → Quit, or use File → Quit in Claude.\n\n" +
	"Chats running in Claude stop while it's closed.\n" +
	"The updates load the moment Claude has closed, then Claude reopens."

// closeAndApply asks Claude to quit (never forcing it), and from then on the
// watcher loads the updates as soon as Claude is closed and reopens it.
func (a *App) closeAndApply() {
	e := a.engine()
	if e == nil {
		return
	}
	apps, err := claude.RunningApps()
	if err != nil {
		a.log.Printf("couldn't check whether Claude is running: %v", err)
	}
	if len(apps) == 0 {
		a.log.Printf("Claude is already closed; loading updates")
		a.applyAndReopen()
		return
	}
	a.mu.Lock()
	first := a.reopen == nil
	a.reopen = apps
	a.mu.Unlock()
	if first {
		a.log.Printf("asked Claude to quit so the updates can load")
	}
	claude.AskToQuit(apps)
	fyne.Do(func() {
		a.showWindow()
		dialog.ShowInformation("Waiting for Claude to close", "ClaudeSync has asked Claude to quit.\n\n"+quitHelp, a.win)
	})
	if claude.WaitClosed(a.ctx, 20*time.Second) {
		a.applyAndReopen()
		return
	}
	a.log.Printf("Claude is still open (probably in the tray); the updates load as soon as you quit it")
}

// applyAndReopen loads waiting updates now that Claude is closed and, if
// ClaudeSync asked Claude to quit, opens it again.
func (a *App) applyAndReopen() {
	a.mu.Lock()
	if a.applying {
		a.mu.Unlock()
		return
	}
	a.applying = true
	apps := a.reopen
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.applying = false
		a.mu.Unlock()
	}()

	a.doSync(false)
	a.mu.Lock()
	a.reopen = nil
	a.lastAsked = 0
	a.mu.Unlock()
	if len(apps) == 0 {
		return
	}
	if err := claude.Relaunch(apps); err != nil {
		a.log.Printf("couldn't reopen Claude: %v", err)
		a.notify("ClaudeSync", "Updates loaded. Open Claude to see them.")
		return
	}
	a.log.Printf("updates loaded; reopened Claude")
	a.notify("ClaudeSync", "Updates loaded and Claude reopened.")
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
	if len(loc.Candidates) > 1 {
		fmt.Println("Folders with desktop sessions:", loc.Candidates)
		if loc.Ambiguous {
			fmt.Println("  more than one! Pick the right one in Settings before restoring.")
		}
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
	if orphans, err := sc.Orphans(); err == nil && len(orphans) > 0 {
		fmt.Printf("\n%d conversation(s) on disk aren't in Claude's sidebar (recoverable in the Recover tab):\n", len(orphans))
		for _, o := range orphans {
			part := ""
			if o.PartOf != "" {
				part = "  (earlier part of a chat still in Claude)"
			}
			fmt.Printf("  %s  %q  %s%s\n", o.Modified.Format("2006-01-02 15:04"), o.Title, o.Cwd, part)
		}
	}
}

func projectSettings(c appcfg.Settings) projsync.Settings {
	return projsync.Settings{ClashPolicy: c.ClashPolicy, FirstLink: c.FirstLink, BigChange: c.BigChange, GitGuard: c.GitGuard,
		RemoteDeletes: c.RemoteDeletes, MaxFileMB: c.MaxFileMB, MaxProjectMB: c.MaxProjectMB, TrashDays: c.TrashDays,
		TrashGB: c.TrashGB, LANOnly: c.ProjectsLANOnly, UploadCapMB: c.UploadCapMB}
}

// addDefaultProjects applies the "new projects" setting to project folders
// ClaudeSync hasn't seen before. Pushed git projects get at most the git default.
func (a *App) addDefaultProjects() {
	e := a.engine()
	c := a.settings()
	if e == nil || e.ProjectSync == nil || c.ProjectDefault == "" || c.ProjectDefault == projsync.ModeOff {
		return
	}
	for key, folder := range e.KnownProjects() {
		if _, ok := e.ProjectSync.Get(key); ok {
			continue
		}
		mode := c.ProjectDefault
		if mode == projsync.ModeSync && projsync.HasGitRemote(folder) && c.ProjectGitDefault != projsync.ModeSync {
			mode = c.ProjectGitDefault
		}
		if err := e.ProjectSync.SetProject(projsync.Project{Key: key, Name: filepath.Base(folder), Folder: folder, Mode: mode}); err == nil {
			a.log.Printf("project %s set to %s (your default for new projects)", filepath.Base(folder), mode)
		}
	}
}
