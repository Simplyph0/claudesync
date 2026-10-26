package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/Simplyph0/claudesync/internal/client"
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
	restartBtn := widget.NewButton("Restart Claude and load them", func() { go a.restartApply() })
	restartBtn.Importance = widget.HighImportance
	bannerBox := container.NewVBox(banner, restartBtn)

	backupBtn := widget.NewButton("Back up now", func() { go a.doBackup() })
	restoreBtn := widget.NewButton("Restore now", func() { go a.doRestore(true) })

	var pending []syncer.Pending
	list := widget.NewList(func() int { return len(pending) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			p := pending[i]
			title := p.Title
			if title == "" {
				title = p.Header.Kind + " " + p.Header.ID
			}
			why := map[string]string{
				syncer.WaitApp:     "waiting for Claude to restart",
				syncer.WaitProject: "project folder not found here (see Projects)",
				syncer.WaitFormat:  "paused: Claude's session format changed",
				syncer.WaitError:   "will retry: " + p.Detail,
				"":                 "queued",
			}[p.Reason]
			o.(*widget.Label).SetText(fmt.Sprintf("%s  ·  from %s  ·  %s", title, p.Header.DeviceName, why))
		})

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
		summary.SetText(fmt.Sprintf("%s  ·  %s  ·  connection: %s\nLast backup: %s (%d uploaded)   ·   every %d min\nLast restore: %s (%d applied)   ·   every %d min",
			c.DeviceName, state, route, ago(s.LastBackup), s.Uploaded, c.BackupMinutes, ago(s.LastRestore), s.Restored, c.RestoreMinutes))
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
			banner.SetText(fmt.Sprintf("%d session(s) from your other PCs are ready. Claude must close briefly to load them.", waitApp))
			bannerBox.Show()
		} else {
			bannerBox.Hide()
		}
		if s.FormatPaused {
			errLbl.SetText(strings.TrimSpace(s.LastError + "\nClaude's session format changed after an update. Backups continue, but restoring desktop sessions is paused until ClaudeSync is updated."))
		}
	}
	top := container.NewVBox(summary, container.NewHBox(backupBtn, restoreBtn), errLbl, bannerBox, bold("Waiting to restore"))
	return container.NewPadded(container.NewBorder(top, nil, nil, nil, list)), refresh
}

// --- Projects ---

func (a *App) projectsTab() (fyne.CanvasObject, func()) {
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
	help := wrapLabel("Sessions from other PCs are matched to a project folder here by git remote, then by folder name. If a project isn't found, choose its folder; its sessions restore right after.")
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
		go a.doRestore(true)
	}, a.win)
	d.Resize(fyne.NewSize(700, 500))
	d.Show()
}

// --- History ---

func (a *App) historyTab() fyne.CanvasObject {
	type entry struct{ key, title string }
	var items []entry
	var versions []proto.ItemHeader
	selected := ""
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
		func(i widget.ListItemID, o fyne.CanvasObject) { o.(*widget.Label).SetText(items[i].title) })
	itemList.OnSelected = func(i widget.ListItemID) {
		selected = items[i].key
		kind, id, _ := strings.Cut(selected, "/")
		go func() {
			vs, err := a.engine().API.Versions(context.Background(), kind, id)
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
	load := widget.NewButton("Refresh list", func() {
		e := a.engine()
		if e == nil {
			return
		}
		items = items[:0]
		for k, st := range e.Known() {
			t := st.Title
			if t == "" {
				t = k
			}
			kind, _, _ := strings.Cut(k, "/")
			items = append(items, entry{k, "[" + kind + "] " + t})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].title < items[j].title })
		itemList.UnselectAll()
		versions = nil
		verList.Refresh()
		itemList.Refresh()
	})
	load.OnTapped()
	split := container.NewHSplit(itemList, verList)
	split.Offset = 0.45
	help := wrapLabel("Pick a session or setting to see the versions the server keeps. Restoring an old version puts it on this PC; it becomes the newest version on every PC at the next backup.")
	return container.NewPadded(container.NewBorder(container.NewVBox(help, load), nil, nil, nil, split))
}

func (a *App) confirmRollback(v proto.ItemHeader) {
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
	desktopDir := widget.NewEntry()
	desktopDir.SetText(c.DesktopDir)
	desktopDir.SetPlaceHolder("auto-detect")

	save := widget.NewButton("Save", func() {
		n := a.settings()
		bm, err1 := strconv.Atoi(strings.TrimSpace(backup.Text))
		rm, err2 := strconv.Atoi(strings.TrimSpace(restore.Text))
		smb, err3 := strconv.Atoi(strings.TrimSpace(scratchMB.Text))
		if err1 != nil || err2 != nil || err3 != nil || bm < 1 || rm < 1 || smb < 1 {
			dialog.ShowError(errors.New("intervals and size limit must be whole numbers of at least 1"), a.win)
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
		widget.NewFormItem("Desktop app data folder", desktopDir),
		widget.NewFormItem("", login),
	)
	return container.NewVScroll(container.NewPadded(container.NewVBox(form, save)))
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
