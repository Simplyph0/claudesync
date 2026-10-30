package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/Simplyph0/claudesync/internal/appcfg"
	"github.com/Simplyph0/claudesync/internal/autostart"
	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/projsync"
	"github.com/Simplyph0/claudesync/internal/proto"
	"github.com/Simplyph0/claudesync/internal/syncer"
)

func (a *App) mainUI() fyne.CanvasObject {
	statusTab, refreshStatus := a.statusTab()
	projectsTab, refreshProjects := a.projectsTab()
	logTab, refreshLog := a.logTab()
	a.refreshUI = func() {
		refreshStatus()
		refreshProjects()
	}
	a.log.onChange = func() { fyne.Do(refreshLog) }
	a.refreshUI()
	refreshLog()
	return container.NewAppTabs(
		container.NewTabItem("Status", statusTab),
		container.NewTabItem("Projects", projectsTab),
		container.NewTabItem("History", a.historyTab()),
		container.NewTabItem("Settings", a.settingsTab()),
		container.NewTabItem("Recover", a.recoverTab()),
		container.NewTabItem("Server", a.serverTab()),
		container.NewTabItem("Log", logTab),
	)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "not yet"
	}
	return t.Format("Mon 15:04")
}

func bold(s string) *widget.Label {
	return widget.NewLabelWithStyle(s, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
}

func wrapLabel(s string) *widget.Label {
	l := widget.NewLabel(s)
	l.Wrapping = fyne.TextWrapWord
	return l
}

// --- Status ---

func (a *App) statusTab() (fyne.CanvasObject, func()) {
	summary := wrapLabel("")
	errLbl := wrapLabel("")
	errLbl.Importance = widget.DangerImportance
	banner := wrapLabel("")
	banner.Importance = widget.WarningImportance
	restartBtn := widget.NewButton("Close Claude and load them", func() { go a.closeAndApply() })
	restartBtn.Importance = widget.HighImportance
	bannerBox := container.NewVBox(banner, restartBtn)
	safety := widget.NewLabel("")
	projLine := wrapLabel("")

	backupBtn := widget.NewButton("Back up now", func() { go a.doBackup() })
	restoreBtn := widget.NewButton("Sync now", func() { go a.doSync(true) })

	var pending []syncer.Pending
	list := widget.NewList(func() int { return len(pending) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Skip", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			p := pending[i]
			row := o.(*fyne.Container)
			row.Objects[1].(*widget.Button).OnTapped = func() { a.confirmSkip(p) }
			title := p.Title
			if title == "" {
				title = p.Header.Kind + " " + p.Header.ID
			}
			why := map[string]string{
				syncer.WaitApp:       "loads when Claude is closed",
				syncer.WaitProject:   "project folder not found here (see Projects)",
				syncer.WaitFormat:    "paused: Claude's session format changed",
				syncer.WaitAmbiguous: "paused: choose your Claude data folder in Settings",
				syncer.WaitError:     "will retry: " + p.Detail,
				"":                   "queued",
			}[p.Reason]
			from := "from " + p.Header.DeviceName
			if p.Missing {
				from = "disappeared from this PC; putting it back"
			}
			row.Objects[0].(*widget.Label).SetText(fmt.Sprintf("%s  ·  %s  ·  %s", title, from, why))
		})
	skippedBtn := widget.NewButton("Skipped on this PC…", a.showSkipped)

	refresh := func() {
		e := a.engine()
		if e == nil {
			return
		}
		s := e.Status()
		c := a.settings()
		route := s.Route
		if route == "" {
			route = "not connected yet"
		}
		state := "Idle"
		if s.Busy {
			state = "Syncing…"
		}
		summary.SetText(fmt.Sprintf("%s  ·  %s  ·  connection: %s\nLast backup: %s (%d uploaded)   ·   every %d min\nLast sync: %s (%d applied)   ·   every %d min",
			c.DeviceName, state, route, ago(s.LastBackup), s.Uploaded, c.BackupMinutes, ago(s.LastRestore), s.Restored, c.RestoreMinutes))
		if s.LastSnapshot != "" {
			safety.SetText("Last sync saved the files it replaced. Undo it in the Recover tab if needed.")
		} else {
			safety.SetText("Backs up this PC before every sync. Never deletes; replaced files are snapshotted.")
		}
		a.projectSummaryLine(projLine)
		errLbl.SetText(s.LastError)
		pending = s.Pending
		list.Refresh()
		waitApp := 0
		for _, p := range s.Pending {
			if p.Reason == syncer.WaitApp {
				waitApp++
			}
		}
		if waitApp > 0 {
			banner.SetText(fmt.Sprintf("%d session(s) ready. They load as soon as Claude is closed.", waitApp))
			bannerBox.Show()
		} else {
			bannerBox.Hide()
		}
		if s.FormatPaused {
			errLbl.SetText(strings.TrimSpace(s.LastError + "\nClaude's session format changed after an update. Backups continue, but restoring desktop sessions is paused until ClaudeSync is updated."))
		}
	}
	top := container.NewVBox(summary, projLine, safety, container.NewHBox(backupBtn, restoreBtn), errLbl, bannerBox,
		container.NewBorder(nil, nil, bold("Waiting to restore"), skippedBtn))
	return container.NewPadded(container.NewBorder(top, nil, nil, nil, list)), refresh
}

// --- Projects ---

func (a *App) chatFoldersTab() (fyne.CanvasObject, func()) {
	type row struct {
		key, name, path string
		unmapped        bool
	}
	var rows []row
	list := widget.NewList(func() int { return len(rows) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, container.NewHBox(widget.NewButton("Choose folder…", nil), widget.NewButton("Forget", nil)), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r := rows[i]
			b := o.(*fyne.Container)
			lbl := b.Objects[0].(*widget.Label)
			btns := b.Objects[1].(*fyne.Container)
			if r.unmapped {
				lbl.SetText(fmt.Sprintf("⚠ %s  —  not found on this PC  (%s)", r.name, r.key))
			} else {
				lbl.SetText(fmt.Sprintf("%s  →  %s", r.name, r.path))
			}
			btns.Objects[0].(*widget.Button).OnTapped = func() { a.pickProjectFolder(r.key, r.name) }
			forget := btns.Objects[1].(*widget.Button)
			forget.OnTapped = func() {
				if e := a.engine(); e != nil {
					_ = e.MapProject(r.key, "")
				}
			}
			if r.unmapped {
				forget.Hide()
			} else {
				forget.Show()
			}
		})
	refresh := func() {
		e := a.engine()
		if e == nil {
			return
		}
		rows = rows[:0]
		seen := map[string]bool{}
		for _, p := range e.Status().Pending {
			if p.Reason == syncer.WaitProject && p.Project != nil && !seen[p.Project.Key] {
				seen[p.Project.Key] = true
				rows = append(rows, row{key: p.Project.Key, name: p.Project.Name, unmapped: true})
			}
		}
		var mapped []row
		for k, v := range e.Projects() {
			name := k[strings.Index(k, ":")+1:]
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			mapped = append(mapped, row{key: k, name: name, path: v})
		}
		sort.Slice(mapped, func(i, j int) bool { return mapped[i].name < mapped[j].name })
		rows = append(rows, mapped...)
		list.Refresh()
	}
	help := wrapLabel("Where chats from your other PCs open on this PC. Chats are matched to a folder here by git remote, then by folder name; if one isn't found, choose its folder and its chats load right after. This is only about chats. To back up or sync the files in a folder, use My folders.")
	return container.NewPadded(container.NewBorder(help, nil, nil, nil, list)), refresh
}

func (a *App) pickProjectFolder(key, name string) {
	d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
		if err != nil || u == nil {
			return
		}
		e := a.engine()
		if e == nil {
			return
		}
		if err := e.MapProject(key, u.Path()); err != nil {
			dialog.ShowError(err, a.win)
			return
		}
		a.log.Printf("project %s mapped to %s", name, u.Path())
		go a.doSync(true)
	}, a.win)
	d.Resize(fyne.NewSize(700, 500))
	d.Show()
}

// --- History ---

func (a *App) confirmSkip(p syncer.Pending) {
	title := p.Title
	if title == "" {
		title = p.Header.ID
	}
	dialog.ShowConfirm("Skip on this PC?", fmt.Sprintf("Stop restoring %q on this PC?\n\nIt stays in the server backup and on your other PCs. Un-skip it any time with \"Skipped on this PC…\".", title), func(ok bool) {
		if !ok {
			return
		}
		if err := a.engine().Skip(p.Header.Key(), p.Title); err != nil {
			dialog.ShowError(err, a.win)
		}
	}, a.win)
}

func (a *App) showSkipped() {
	e := a.engine()
	if e == nil {
		return
	}
	type row struct{ key, title string }
	var rows []row
	load := func() {
		rows = rows[:0]
		for k, t := range e.Skipped() {
			if t == "" {
				t = k
			}
			rows = append(rows, row{k, t})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].title < rows[j].title })
	}
	load()
	var list *widget.List
	list = widget.NewList(func() int { return len(rows) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Un-skip", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r := rows[i]
			b := o.(*fyne.Container)
			b.Objects[0].(*widget.Label).SetText(r.title)
			b.Objects[1].(*widget.Button).OnTapped = func() {
				if err := e.Unskip(r.key); err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				load()
				list.Refresh()
				go a.doSync(true)
			}
		})
	var content fyne.CanvasObject = list
	if len(rows) == 0 {
		content = widget.NewLabel("Nothing is skipped on this PC.")
	}
	d := dialog.NewCustom("Skipped on this PC", "Close", content, a.win)
	d.Resize(fyne.NewSize(560, 360))
	d.Show()
}

func (a *App) historyTab() fyne.CanvasObject {
	var all, items []syncer.ServerItem
	var versions []proto.ItemHeader
	var current *syncer.ServerItem
	verList := widget.NewList(func() int { return len(versions) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Restore this version", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			v := versions[i]
			b := o.(*fyne.Container)
			b.Objects[0].(*widget.Label).SetText(fmt.Sprintf("v%d  ·  %s  ·  from %s  ·  %d KB", v.Version, v.Time.Local().Format("2006-01-02 15:04"), v.DeviceName, v.Size>>10))
			b.Objects[1].(*widget.Button).OnTapped = func() { a.confirmRollback(v) }
		})
	itemList := widget.NewList(func() int { return len(items) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			it := items[i]
			label := "[" + it.Header.Kind + "] " + it.Title
			if it.Header.Purged {
				label += "  ·  deleted for good"
			} else if it.Header.Removed {
				days := proto.RemovedHoldDays - int(time.Since(it.Header.RemovedAt).Hours()/24)
				label += fmt.Sprintf("  ·  removed (deleted for good in %d days)", max(days, 0))
			} else if it.Skipped {
				label += "  ·  skipped on this PC"
			}
			o.(*widget.Label).SetText(label)
		})
	showRemoved := widget.NewCheck("Show removed", nil)
	removeBtn := widget.NewButton("Remove from server backup", nil)
	removeBtn.Importance = widget.DangerImportance
	backBtn := widget.NewButton("Bring back", nil)
	deleteBtn := widget.NewButton("Delete from server for good…", nil)
	deleteBtn.Importance = widget.DangerImportance
	purgeAll := widget.NewButton("Delete all removed for good…", nil)
	removeBtn.Disable()
	deleteBtn.Disable()
	backBtn.Hide()

	filter := func() {
		items = items[:0]
		for _, it := range all {
			if !it.Header.Removed || showRemoved.Checked {
				items = append(items, it)
			}
		}
		itemList.UnselectAll()
		current, versions = nil, nil
		removeBtn.Disable()
		removeBtn.Show()
		deleteBtn.Disable()
		deleteBtn.Show()
		backBtn.Hide()
		verList.Refresh()
		itemList.Refresh()
	}
	var reload func()
	reload = func() {
		e := a.engine()
		if e == nil {
			return
		}
		go func() {
			list, err := e.ServerItems(context.Background())
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				all = list
				filter()
			})
		}()
	}
	showRemoved.OnChanged = func(bool) { filter() }

	itemList.OnSelected = func(i widget.ListItemID) {
		it := items[i]
		current = &it
		switch {
		case it.Header.Purged: // nothing left to restore, remove or delete
			removeBtn.Hide()
			backBtn.Hide()
			deleteBtn.Hide()
			versions = nil
			verList.Refresh()
			return
		case it.Header.Removed:
			removeBtn.Hide()
			backBtn.Show()
		default:
			backBtn.Hide()
			removeBtn.Show()
			removeBtn.Enable()
		}
		deleteBtn.Show()
		deleteBtn.Enable()
		go func() {
			vs, err := a.engine().API.Versions(context.Background(), it.Header.Kind, it.Header.ID)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				sort.Slice(vs, func(i, j int) bool { return vs[i].Version > vs[j].Version })
				versions = vs
				verList.Refresh()
			})
		}()
	}
	removeBtn.OnTapped = func() {
		if current == nil {
			return
		}
		it := *current
		dialog.ShowConfirm("Remove from the server backup?", fmt.Sprintf("Remove %q from the server backup?\n\n"+
			"• Your PCs stop syncing it, and PCs that don't have it won't get it.\n"+
			"• It is NOT deleted from any PC's Claude.\n"+
			"• You can bring it back for %d days (\"Show removed\"); after that the server deletes it.", it.Title, proto.RemovedHoldDays), func(ok bool) {
			if !ok {
				return
			}
			go func() {
				err := a.engine().RemoveFromServer(context.Background(), it.Header.Kind, it.Header.ID)
				fyne.Do(func() {
					if err != nil {
						dialog.ShowError(err, a.win)
					}
					reload()
				})
			}()
		}, a.win)
	}
	deleteBtn.OnTapped = func() {
		if current == nil {
			return
		}
		it := *current
		a.confirmDelete(it.Title, it.Header.Kind, it.Header.ID, reload)
	}
	purgeAll.OnTapped = func() {
		var gone []syncer.ServerItem
		for _, it := range all {
			if it.Header.Removed && !it.Header.Purged {
				gone = append(gone, it)
			}
		}
		if len(gone) == 0 {
			dialog.ShowInformation("Nothing to delete", "No removed items are waiting. Tick \"Show removed\" to see them.", a.win)
			return
		}
		d := dialog.NewConfirm("Delete all removed items for good?", fmt.Sprintf("Delete the %d removed item(s) from the server for good?\n\n"+
			"• Every saved version is erased. This can't be undone.\n"+
			"• Nothing is deleted from any PC.\n"+
			"• No PC will back them up again.", len(gone)), func(ok bool) {
			if !ok {
				return
			}
			go func() {
				var errs []error
				for _, it := range gone {
					if err := a.engine().DeleteFromServer(context.Background(), it.Header.Kind, it.Header.ID); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", it.Title, err))
					}
				}
				fyne.Do(func() {
					if err := errors.Join(errs...); err != nil {
						a.showProblem("Some items couldn't be deleted", err, "")
					}
					reload()
				})
			}()
		}, a.win)
		d.SetConfirmText("Delete for good")
		d.SetConfirmImportance(widget.DangerImportance)
		d.Show()
	}
	backBtn.OnTapped = func() {
		if current == nil {
			return
		}
		it := *current
		go func() {
			err := a.engine().BringBack(context.Background(), it.Header.Kind, it.Header.ID)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				reload()
				go a.doSync(true)
			})
		}()
	}

	refresh := widget.NewButton("Refresh", reload)
	reload()
	left := container.NewBorder(nil, container.NewHBox(removeBtn, backBtn, deleteBtn), nil, nil, itemList)
	split := container.NewHSplit(left, verList)
	split.Offset = 0.5
	help := wrapLabel("Everything in the server backup. Pick an item to see its saved versions. Restoring an old version puts it on this PC; it becomes the newest everywhere at the next backup.")
	return container.NewPadded(container.NewBorder(container.NewVBox(help, container.NewHBox(refresh, showRemoved, purgeAll)), nil, nil, nil, split))
}

// confirmDelete asks, in plain words, before erasing an item from the server
// for good, then does it and calls done.
func (a *App) confirmDelete(title, kind, id string, done func()) {
	msg := fmt.Sprintf("Delete %q from the server for good?\n\n"+
		"• Every saved version is erased from the server. This can't be undone.\n"+
		"• It is NOT deleted from any PC. Your own copies stay.\n"+
		"• No PC will back it up again; a tiny marker stays so it can't come back.\n"+
		"• The server frees the space within a day.", title)
	if pm := a.projects(); kind == proto.KindProject && pm != nil {
		if folder := pm.FolderForItem(id); folder != "" {
			msg += fmt.Sprintf("\n\nThis PC's folder %s keeps its files, but stops being synced or backed up.", folder)
		}
	}
	d := dialog.NewConfirm("Delete from the server for good?", msg, func(ok bool) {
		if !ok {
			return
		}
		go func() {
			err := a.engine().DeleteFromServer(context.Background(), kind, id)
			fyne.Do(func() {
				if err != nil {
					a.showProblem("Couldn't delete it", err, "")
				}
				done()
				if a.refreshUI != nil {
					a.refreshUI()
				}
			})
		}()
	}, a.win)
	d.SetConfirmText("Delete for good")
	d.SetConfirmImportance(widget.DangerImportance)
	d.Show()
}

func (a *App) confirmRollback(v proto.ItemHeader) {
	if v.Kind == proto.KindProject {
		a.restoreProjectVersion(v.ID, v.Version, v.ID)
		return
	}
	dialog.ShowConfirm("Restore an older version?", fmt.Sprintf("Replace this PC's copy with version %d from %s (%s)?", v.Version, v.DeviceName, v.Time.Local().Format("2006-01-02 15:04")), func(ok bool) {
		if !ok {
			return
		}
		go func() {
			err := a.engine().RestoreVersion(context.Background(), v.Kind, v.ID, v.Version)
			fyne.Do(func() {
				if errors.Is(err, syncer.ErrCloseApp) {
					dialog.ShowInformation("Close Claude first", "Close the Claude app, then try again.", a.win)
				} else if err != nil {
					dialog.ShowError(err, a.win)
				} else {
					dialog.ShowInformation("Restored", "Done. It will sync to your other PCs at the next backup.", a.win)
				}
			})
		}()
	}, a.win)
}

// --- Settings ---

func (a *App) settingsTab() fyne.CanvasObject {
	c := a.settings()
	lan := widget.NewEntry()
	lan.SetText(strings.TrimPrefix(c.LANURL, "https://"))
	tun := widget.NewEntry()
	tun.SetText(c.TunnelURL)
	tun.SetPlaceHolder("https://sync.example.com")
	backup := widget.NewEntry()
	backup.SetText(strconv.Itoa(c.BackupMinutes))
	restore := widget.NewEntry()
	restore.SetText(strconv.Itoa(c.RestoreMinutes))
	sessions := widget.NewCheck("Desktop app sessions", nil)
	sessions.SetChecked(c.Sessions)
	scratch := widget.NewCheck("Working folders of folderless sessions", nil)
	scratch.SetChecked(c.Scratch)
	scratchMB := widget.NewEntry()
	scratchMB.SetText(strconv.Itoa(c.ScratchMaxMB))
	cfgChk := widget.NewCheck("Settings, CLAUDE.md, commands, skills, agents", nil)
	cfgChk.SetChecked(c.Config)
	mem := widget.NewCheck("Project memory", nil)
	mem.SetChecked(c.Memory)
	cli := widget.NewCheck("Claude Code terminal sessions (no desktop record)", nil)
	cli.SetChecked(c.CLIOnly)
	login := widget.NewCheck("Start ClaudeSync when I sign in", nil)
	login.SetChecked(c.StartAtLogin)
	roots := widget.NewMultiLineEntry()
	roots.SetText(strings.Join(c.SearchRoots, "\n"))
	roots.SetMinRowsVisible(3)
	claudeHome := widget.NewEntry()
	claudeHome.SetText(c.ClaudeHome)
	claudeHome.SetPlaceHolder("auto (~/.claude)")
	// Project folders: what to always do, or ask.
	pick := func(names, values []string, cur string) *widget.Select {
		s := widget.NewSelect(names, nil)
		s.SetSelectedIndex(indexOf(values, cur))
		return s
	}
	projDefault := pick([]string{"Off (I'll pick)", "Back up only", "Sync"}, modeValues, c.ProjectDefault)
	gitDefault := pick([]string{"Back up only", "Sync"}, []string{projsync.ModeBackup, projsync.ModeSync}, c.ProjectGitDefault)
	clashPol := pick([]string{"Keep both", "Newest wins", "Ask me"}, clashVals[1:], c.ClashPolicy)
	firstLink := pick([]string{"Ask me (show a preview)", "Always merge", "Always use the server's copy", "Always use this PC's copy"},
		[]string{projsync.LinkAsk, projsync.LinkMerge, projsync.LinkServer, projsync.LinkLocal}, c.FirstLink)
	bigChange := pick([]string{"Ask me", "Always go ahead"}, []string{projsync.BigAsk, projsync.BigContinue}, c.BigChange)
	gitGuard := pick([]string{"Ask me", "Always sync anyway", "Never (wait until they match)"},
		[]string{projsync.GitAsk, projsync.GitSync, projsync.GitSkip}, c.GitGuard)
	remoteDel := pick([]string{"Move them to the trash here too", "Ask me"}, []string{projsync.DeletesTrash, projsync.DeletesAsk}, c.RemoteDeletes)
	num := func(v int) *widget.Entry {
		e := widget.NewEntry()
		e.SetText(strconv.Itoa(v))
		return e
	}
	maxFile, maxProj, trashDays, trashGB, upCap := num(c.MaxFileMB), num(c.MaxProjectMB), num(c.TrashDays), num(c.TrashGB), num(c.UploadCapMB)
	lanOnly := widget.NewCheck("Only sync project folders on my home network", nil)
	lanOnly.SetChecked(c.ProjectsLANOnly)
	forgetChatless := widget.NewCheck("Remove a folder from My Folders when its last chat is deleted (files and server copy are never touched)", nil)
	forgetChatless.SetChecked(c.ForgetChatless)
	readProjectSettings := func(n *appcfg.Settings) error {
		vals := map[*widget.Entry]*int{maxFile: &n.MaxFileMB, maxProj: &n.MaxProjectMB, trashDays: &n.TrashDays, trashGB: &n.TrashGB, upCap: &n.UploadCapMB}
		for e, dst := range vals {
			v, err := strconv.Atoi(strings.TrimSpace(e.Text))
			if err != nil || v < 0 {
				return errors.New("project limits must be whole numbers (0 or more)")
			}
			*dst = v
		}
		n.ProjectDefault = modeValues[projDefault.SelectedIndex()]
		n.ProjectGitDefault = []string{projsync.ModeBackup, projsync.ModeSync}[gitDefault.SelectedIndex()]
		n.ClashPolicy = clashVals[1:][clashPol.SelectedIndex()]
		n.FirstLink = []string{projsync.LinkAsk, projsync.LinkMerge, projsync.LinkServer, projsync.LinkLocal}[firstLink.SelectedIndex()]
		n.BigChange = []string{projsync.BigAsk, projsync.BigContinue}[bigChange.SelectedIndex()]
		n.GitGuard = []string{projsync.GitAsk, projsync.GitSync, projsync.GitSkip}[gitGuard.SelectedIndex()]
		n.RemoteDeletes = []string{projsync.DeletesTrash, projsync.DeletesAsk}[remoteDel.SelectedIndex()]
		n.ProjectsLANOnly = lanOnly.Checked
		n.ForgetChatless = forgetChatless.Checked
		return nil
	}

	loc := claude.Detect(c.ClaudeHome, c.DesktopDir)
	desktopDir := widget.NewSelectEntry(loc.Candidates)
	desktopDir.SetText(c.DesktopDir)
	desktopDir.SetPlaceHolder("auto-detect: " + orNone(loc.DesktopDir))
	desktopNote := wrapLabel("")
	if len(loc.Candidates) > 1 && c.DesktopDir == "" && runtime.GOOS != "windows" {
		desktopNote.SetText("⚠ This PC has more than one folder with Claude sessions. Choose the one your Claude app uses (open the list) and press Save; until then, sessions from other PCs aren't loaded here.")
		desktopNote.Importance = widget.WarningImportance
	} else {
		desktopNote.Hide()
	}

	save := widget.NewButton("Save", func() {
		n := a.settings()
		bm, err1 := strconv.Atoi(strings.TrimSpace(backup.Text))
		rm, err2 := strconv.Atoi(strings.TrimSpace(restore.Text))
		smb, err3 := strconv.Atoi(strings.TrimSpace(scratchMB.Text))
		if err1 != nil || err2 != nil || err3 != nil || bm < 1 || rm < 1 || smb < 1 {
			dialog.ShowError(errors.New("intervals and size limit must be whole numbers of at least 1"), a.win)
			return
		}
		if err := readProjectSettings(&n); err != nil {
			dialog.ShowError(err, a.win)
			return
		}
		newLAN := lanURL(lan.Text)
		n.BackupMinutes, n.RestoreMinutes, n.ScratchMaxMB = bm, rm, smb
		n.TunnelURL = appcfg.NormalizeURL(tun.Text)
		n.Sessions, n.Scratch, n.Config, n.Memory, n.CLIOnly = sessions.Checked, scratch.Checked, cfgChk.Checked, mem.Checked, cli.Checked
		n.ClaudeHome, n.DesktopDir = strings.TrimSpace(claudeHome.Text), strings.TrimSpace(desktopDir.Text)
		n.SearchRoots = nil
		for _, l := range strings.Split(roots.Text, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				n.SearchRoots = append(n.SearchRoots, l)
			}
		}
		if n.StartAtLogin != login.Checked {
			if err := autostart.Set(login.Checked); err != nil {
				dialog.ShowError(err, a.win)
			}
			n.StartAtLogin = login.Checked
		}
		commit := func() {
			if err := appcfg.Save(n); err != nil {
				dialog.ShowError(err, a.win)
				return
			}
			a.applySettings(n)
			a.log.Printf("settings saved")
			if a.refreshUI != nil {
				a.refreshUI()
			}
		}
		if newLAN == n.LANURL || newLAN == "" {
			if newLAN == "" {
				n.LANURL, n.Pin = "", ""
			}
			commit()
			return
		}
		// New LAN address: confirm its certificate first.
		go func() {
			fp, err := client.FetchFingerprint(newLAN)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(fmt.Errorf("couldn't reach %s: %w", newLAN, err), a.win)
					return
				}
				dialog.ShowConfirm("Check the server's fingerprint", "The server at "+newLAN+" identifies itself as:\n\n"+fp+"\n\nDoes this match 'claudesync-server fingerprint' on the server?", func(ok bool) {
					if ok {
						n.LANURL, n.Pin = newLAN, fp
						commit()
					}
				}, a.win)
			})
		}()
	})
	save.Importance = widget.HighImportance

	form := widget.NewForm(
		widget.NewFormItem("Server on your network", lan),
		widget.NewFormItem("Cloudflare Tunnel address", tun),
		widget.NewFormItem("Back up every (minutes)", backup),
		widget.NewFormItem("Check for updates every (minutes)", restore),
		widget.NewFormItem("Sync", container.NewVBox(sessions, scratch, cfgChk, mem, cli)),
		widget.NewFormItem("Working folder size limit (MB)", scratchMB),
		widget.NewFormItem("Look for projects in", roots),
		widget.NewFormItem("Claude Code folder", claudeHome),
		widget.NewFormItem("Desktop app data folder", container.NewVBox(desktopDir, desktopNote)),
		widget.NewFormItem("", login),
	)
	projForm := widget.NewForm(
		widget.NewFormItem("New projects", projDefault),
		widget.NewFormItem("…but pushed git projects at most", gitDefault),
		widget.NewFormItem("Same file changed on two PCs", clashPol),
		widget.NewFormItem("Linking a folder that already has files", firstLink),
		widget.NewFormItem("A sync would remove many files", bigChange),
		widget.NewFormItem("PCs on different git branches", gitGuard),
		widget.NewFormItem("Files deleted on another PC", remoteDel),
		widget.NewFormItem("Largest file (MB)", maxFile),
		widget.NewFormItem("Ask above project size (MB)", maxProj),
		widget.NewFormItem("Keep trash (days / GB)", container.NewGridWithColumns(2, trashDays, trashGB)),
		widget.NewFormItem("Upload limit per sync (MB, 0 = none)", upCap),
		widget.NewFormItem("", lanOnly),
		widget.NewFormItem("", forgetChatless),
	)
	projNote := widget.NewLabel("Text files changed in different places on two PCs are always merged automatically.\nAn emptied project folder always asks before anything is deleted on your other PCs.")
	return container.NewVScroll(container.NewPadded(container.NewVBox(form, bold("Project folders"), projNote, projForm, save)))
}

// --- Recover ---

func (a *App) recoverTab() fyne.CanvasObject {
	// Conversations on disk that Claude's sidebar doesn't show.
	var orphans []syncer.Orphan
	picked := map[int]bool{}
	orphanInfo := wrapLabel("")
	orphanList := widget.NewList(func() int { return len(orphans) },
		func() fyne.CanvasObject { return widget.NewCheck("", nil) },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			c := o.(*widget.Check)
			x := orphans[i]
			c.Text = fmt.Sprintf("%s  ·  %s  ·  %s  ·  %d KB", x.Modified.Local().Format("2006-01-02 15:04"), x.Title, x.Cwd, x.Size>>10)
			if x.PartOf != "" {
				c.Text += "  ·  earlier part of a chat already in Claude"
			}
			c.OnChanged = nil
			c.SetChecked(picked[i])
			c.OnChanged = func(b bool) { picked[i] = b }
			c.Refresh()
		})
	findOrphans := func() {
		e := a.engine()
		if e == nil {
			return
		}
		go func() {
			list, err := e.Scanner.Orphans()
			fyne.Do(func() {
				orphans, picked = list, map[int]bool{}
				lost := 0
				for i, o := range orphans {
					picked[i] = o.PartOf == "" // earlier parts of listed chats would only be duplicates
					if o.PartOf == "" {
						lost++
					}
				}
				switch {
				case err != nil:
					orphanInfo.SetText(err.Error())
				case len(orphans) == 0:
					orphanInfo.SetText("Every conversation on this PC is in Claude's sidebar.")
				default:
					orphanInfo.SetText(fmt.Sprintf("%d conversation(s) are on this PC but not in Claude's sidebar; %d look lost and are ticked. The rest are earlier parts of chats you still have. Close Claude, then press Add.", len(orphans), lost))
				}
				orphanList.Refresh()
			})
		}()
	}
	adopt := widget.NewButton("Add ticked chats to Claude", func() {
		var sel []syncer.Orphan
		for i, o := range orphans {
			if picked[i] {
				sel = append(sel, o)
			}
		}
		if len(sel) == 0 {
			return
		}
		go func() {
			n, err := a.engine().Scanner.Adopt(sel)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
				}
				if n > 0 {
					a.log.Printf("added %d recovered chat(s) to Claude", n)
					dialog.ShowInformation("Chats added", fmt.Sprintf("%d chat(s) are back in Claude's sidebar. Open Claude to see them; they'll be backed up at the next sync.", n), a.win)
				}
				findOrphans()
			})
		}()
	})
	adopt.Importance = widget.HighImportance

	// Snapshots taken before ClaudeSync changed anything.
	var snaps []syncer.SnapshotInfo
	snapList := widget.NewList(func() int { return len(snaps) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Put these files back", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			s := snaps[i]
			b := o.(*fyne.Container)
			b.Objects[0].(*widget.Label).SetText(fmt.Sprintf("%s  ·  %s  ·  %d KB", s.Time.Format("2006-01-02 15:04:05"), s.Reason, s.Size>>10))
			b.Objects[1].(*widget.Button).OnTapped = func() { a.confirmUndo(s) }
		})
	loadSnaps := func() {
		snaps = syncer.ListSnapshots(snapshotDir())
		snapList.Refresh()
	}
	refresh := widget.NewButton("Refresh", func() {
		findOrphans()
		loadSnaps()
	})
	findOrphans()
	loadSnaps()

	top := container.NewVBox(
		container.NewBorder(nil, nil, bold("Chats missing from Claude's sidebar"), refresh),
		orphanInfo,
	)
	orphansBox := container.NewBorder(top, adopt, nil, nil, orphanList)
	snapsBox := container.NewBorder(container.NewVBox(bold("Snapshots"),
		wrapLabel("Before every change, ClaudeSync saves the files it is about to replace. Putting a snapshot back restores those files exactly; nothing is deleted.")), nil, nil, nil, snapList)
	trashList, refreshTrash := a.projectTrashView()
	trashBox := container.NewBorder(container.NewVBox(
		container.NewBorder(nil, nil, bold("Project trash"), widget.NewButton("Refresh", refreshTrash)),
		wrapLabel("Every project file a sync replaced or removed. Put back restores them where they were; nothing current is lost (it goes to the trash first).")),
		nil, nil, nil, trashList)
	return container.NewAppTabs(
		container.NewTabItem("Lost chats", container.NewPadded(orphansBox)),
		container.NewTabItem("Chat snapshots", container.NewPadded(snapsBox)),
		container.NewTabItem("Project trash", container.NewPadded(trashBox)),
	)
}

func (a *App) confirmUndo(s syncer.SnapshotInfo) {
	files, _ := syncer.SnapshotFiles(s.Path)
	dialog.ShowConfirm("Put files back?", fmt.Sprintf("Restore the %d file(s) saved at %s (%s)?\n\nClose Claude first. The current versions are saved in a new snapshot, so this can be undone too.",
		len(files), s.Time.Format("2006-01-02 15:04:05"), s.Reason), func(ok bool) {
		if !ok {
			return
		}
		go func() {
			n, err := a.engine().UndoSnapshot(s.Path)
			fyne.Do(func() {
				if errors.Is(err, syncer.ErrCloseApp) {
					dialog.ShowInformation("Close Claude first", "Quit Claude, then try again.", a.win)
				} else if err != nil {
					dialog.ShowError(err, a.win)
				} else {
					dialog.ShowInformation("Done", fmt.Sprintf("Put back %d file(s).", n), a.win)
				}
			})
		}()
	}, a.win)
}

// --- Server ---

func (a *App) serverTab() fyne.CanvasObject {
	ctx := context.Background()
	retention := widget.NewEntry()
	retention.SetPlaceHolder("20")
	go func() {
		if s, err := a.engine().API.Settings(ctx); err == nil {
			fyne.Do(func() { retention.SetText(strconv.Itoa(s.Retention)) })
		}
	}()
	saveRet := widget.NewButton("Save", func() {
		n, err := strconv.Atoi(strings.TrimSpace(retention.Text))
		if err != nil || n < 1 {
			dialog.ShowError(errors.New("enter a whole number of at least 1"), a.win)
			return
		}
		go func() {
			err := a.engine().API.SetSettings(ctx, proto.Settings{Retention: n})
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
				} else {
					dialog.ShowInformation("Saved", fmt.Sprintf("The server now keeps the last %d versions of each item.", n), a.win)
				}
			})
		}()
	})

	oldPw, newPw, newPw2 := widget.NewPasswordEntry(), widget.NewPasswordEntry(), widget.NewPasswordEntry()
	changePw := widget.NewButton("Change server password", func() {
		if len(newPw.Text) < 8 || newPw.Text != newPw2.Text {
			dialog.ShowError(errors.New("the new password must be at least 8 characters and both entries must match"), a.win)
			return
		}
		go func() {
			err := a.engine().API.ChangePassword(ctx, oldPw.Text, newPw.Text)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				oldPw.SetText("")
				newPw.SetText("")
				newPw2.SetText("")
				dialog.ShowInformation("Password changed", "PCs already connected stay connected. New PCs need the new password.", a.win)
			})
		}()
	})

	var devices []proto.Device
	devList := widget.NewList(func() int { return len(devices) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Remove", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			d := devices[i]
			b := o.(*fyne.Container)
			me := ""
			if d.ID == a.settings().DeviceID {
				me = "  (this PC)"
			}
			b.Objects[0].(*widget.Label).SetText(fmt.Sprintf("%s%s  ·  last seen %s", d.Name, me, d.LastSeen.Local().Format("2006-01-02 15:04")))
			b.Objects[1].(*widget.Button).OnTapped = func() {
				dialog.ShowConfirm("Remove "+d.Name+"?", "It stops syncing until it signs in again with the server password.", func(ok bool) {
					if !ok {
						return
					}
					go func() {
						err := a.engine().API.RevokeDevice(ctx, d.ID)
						fyne.Do(func() {
							if err != nil {
								dialog.ShowError(err, a.win)
							}
						})
					}()
				}, a.win)
			}
		})
	loadDevs := widget.NewButton("Refresh", func() {
		go func() {
			ds, err := a.engine().API.Devices(ctx)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				devices = ds
				devList.Refresh()
			})
		}()
	})
	loadDevs.OnTapped()

	signOut := widget.NewButton("Sign this PC out", func() {
		dialog.ShowConfirm("Sign out?", "This PC stops syncing and forgets the server password and sync passphrase. Your local sessions are not touched.", func(ok bool) {
			if !ok {
				return
			}
			appcfg.ClearSecrets()
			a.mu.Lock()
			a.eng = nil
			a.sec = appcfg.Secrets{}
			a.mu.Unlock()
			a.win.SetContent(a.setupUI())
		}, a.win)
	})
	signOut.Importance = widget.DangerImportance

	top := container.NewVBox(
		bold("Version history"),
		widget.NewForm(widget.NewFormItem("Versions kept per item", container.NewBorder(nil, nil, nil, saveRet, retention))),
		bold("Server password"),
		widget.NewForm(widget.NewFormItem("Current password", oldPw), widget.NewFormItem("New password", newPw), widget.NewFormItem("Repeat new password", newPw2)),
		changePw,
		container.NewBorder(nil, nil, bold("Connected PCs"), loadDevs),
	)
	return container.NewPadded(container.NewBorder(top, signOut, nil, nil, devList))
}

// --- Log ---

func (a *App) logTab() (fyne.CanvasObject, func()) {
	lbl := widget.NewLabel("")
	lbl.Wrapping = fyne.TextWrapWord
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	scroll := container.NewVScroll(lbl)
	return scroll, func() {
		lbl.SetText(a.log.Text())
		scroll.ScrollToBottom()
	}
}
