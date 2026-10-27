package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/Simplyph0/claudesync/internal/projsync"
	"github.com/Simplyph0/claudesync/internal/syncer"
)

var (
	modeNames  = []string{"Off", "Back up only", "Sync"}
	modeValues = []string{projsync.ModeOff, projsync.ModeBackup, projsync.ModeSync}
	clashNames = []string{"Default", "Keep both", "Newest wins", "Ask me"}
	clashVals  = []string{"", projsync.ClashKeepBoth, projsync.ClashNewest, projsync.ClashAsk}
)

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return 0
}

func (a *App) projects() *projsync.Manager {
	if e := a.engine(); e != nil {
		return e.ProjectSync
	}
	return nil
}

// projectsTab holds project folders, decisions waiting on you, projects on
// the server, and the folders used for chats from other PCs.
func (a *App) projectsTab() (fyne.CanvasObject, func()) {
	folders, refreshFolders := a.projectFoldersView()
	needs, refreshNeeds := a.needsYouView()
	server := a.onServerView()
	chats, refreshChats := a.chatFoldersTab()
	needsTab := container.NewTabItem("Needs you", needs)
	tabs := container.NewAppTabs(
		container.NewTabItem("Folders", folders),
		needsTab,
		container.NewTabItem("On the server", server),
		container.NewTabItem("Chat folders", chats),
	)
	refresh := func() {
		refreshFolders()
		n := refreshNeeds()
		refreshChats()
		title := "Needs you"
		if n > 0 {
			title = fmt.Sprintf("Needs you (%d)", n)
		}
		if needsTab.Text != title {
			needsTab.Text = title
			tabs.Refresh()
		}
	}
	return tabs, refresh
}

type folderRow struct {
	st         projsync.Status
	configured bool
}

func (a *App) projectFoldersView() (fyne.CanvasObject, func()) {
	var rows []folderRow
	list := widget.NewList(func() int { return len(rows) },
		func() fyne.CanvasObject {
			mode := widget.NewSelect(modeNames, nil)
			clash := widget.NewSelect(clashNames, nil)
			name := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			detail := widget.NewLabel("")
			name.Truncation, detail.Truncation = fyne.TextTruncateEllipsis, fyne.TextTruncateEllipsis
			return container.NewBorder(nil, nil, nil, container.NewHBox(mode, clash), container.NewVBox(name, detail))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r := rows[i]
			b := o.(*fyne.Container)
			labels := b.Objects[0].(*fyne.Container)
			sel := b.Objects[1].(*fyne.Container)
			mode, clash := sel.Objects[0].(*widget.Select), sel.Objects[1].(*widget.Select)
			status := r.st.Status
			if r.st.Error != "" {
				status = "error: " + r.st.Error
			}
			if !r.configured || r.st.Mode == projsync.ModeOff {
				status = "not synced"
			}
			when := ""
			if !r.st.LastSync.IsZero() {
				when = " · " + ago(r.st.LastSync)
			}
			labels.Objects[0].(*widget.Label).SetText(r.st.Name)
			labels.Objects[1].(*widget.Label).SetText(fmt.Sprintf("%s%s · %s", status, when, r.st.Folder))
			mode.OnChanged, clash.OnChanged = nil, nil
			mode.SetSelectedIndex(indexOf(modeValues, r.st.Mode))
			clash.SetSelectedIndex(indexOf(clashVals, r.st.Clash))
			if r.st.Mode != projsync.ModeSync {
				clash.Disable()
			} else {
				clash.Enable()
			}
			p := r.st.Project
			mode.OnChanged = func(s string) {
				p.Mode = modeValues[indexOf(modeNames, s)]
				a.saveProject(p)
			}
			clash.OnChanged = func(s string) {
				p.Clash = clashVals[indexOf(clashNames, s)]
				a.saveProject(p)
			}
		})
	refresh := func() {
		pm := a.projects()
		e := a.engine()
		if pm == nil || e == nil {
			return
		}
		rows = rows[:0]
		seen := map[string]bool{}
		for _, s := range pm.Projects() {
			rows = append(rows, folderRow{st: s, configured: true})
			seen[s.Key] = true
		}
		for key, folder := range e.KnownProjects() {
			if !seen[key] {
				rows = append(rows, folderRow{st: projsync.Status{Project: projsync.Project{Key: key, Name: filepath.Base(folder), Folder: folder, Mode: projsync.ModeOff}}})
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return strings.ToLower(rows[i].st.Name) < strings.ToLower(rows[j].st.Name) })
		list.Refresh()
	}
	add := widget.NewButton("Add folder…", func() {
		d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
			if err != nil || u == nil {
				return
			}
			folder := u.Path()
			key := syncer.DescribeProject(folder).Key
			a.saveProject(projsync.Project{Key: key, Name: filepath.Base(folder), Folder: folder, Mode: projsync.ModeBackup})
		}, a.win)
		d.Resize(fyne.NewSize(700, 500))
		d.Show()
	})
	syncNow := widget.NewButton("Sync now", func() { go a.doSync(true) })
	explain := widget.NewButton("What do these mean?", func() {
		dialog.ShowInformation("Project folders",
			"Off: not synced.\n\n"+
				"Back up only: this PC's copy is saved, with history, on your server. Nothing on this PC is changed.\n\n"+
				"Sync: the folder is kept the same on every PC set to Sync.\n\n"+
				"The second menu is what happens when the same file changed on two PCs (Default = your choice in Settings).\n"+
				"Text edited in different places is always merged automatically.\n\n"+
				"Anything a sync replaces or removes goes to the project trash first (Recover tab).\n"+
				"Folders git already syncs (pushed to GitHub) are best left on Back up only.", a.win)
	})
	refresh()
	return container.NewPadded(container.NewBorder(container.NewHBox(add, syncNow, explain), nil, nil, nil, list)), refresh
}

func (a *App) saveProject(p projsync.Project) {
	pm := a.projects()
	if pm == nil {
		return
	}
	if err := pm.SetProject(p); err != nil {
		dialog.ShowError(err, a.win)
		return
	}
	a.log.Printf("project %s: %s", p.Name, modeNames[indexOf(modeValues, p.Mode)])
	if a.refreshUI != nil {
		a.refreshUI()
	}
	if p.Mode != projsync.ModeOff {
		go a.doSync(true)
	}
}

// need is one thing waiting for the user: a decision or a clash.
type need struct {
	key, name string
	decision  *projsync.Decision
	clash     *projsync.Clash
}

func (a *App) needsYouView() (fyne.CanvasObject, func() int) {
	var needs []need
	box := container.NewVBox()
	empty := wrapLabel("Nothing needs you. When a project setting says \"Ask me\", questions show up here.")
	refresh := func() int {
		pm := a.projects()
		if pm == nil {
			return 0
		}
		needs = needs[:0]
		for _, s := range pm.Projects() {
			if s.Decision != nil {
				needs = append(needs, need{key: s.Key, name: s.Name, decision: s.Decision})
			}
			for i := range s.Clashes {
				needs = append(needs, need{key: s.Key, name: s.Name, clash: &s.Clashes[i]})
			}
		}
		box.RemoveAll()
		if len(needs) == 0 {
			box.Add(empty)
		}
		for _, n := range needs {
			box.Add(a.needCard(n))
			box.Add(widget.NewSeparator())
		}
		box.Refresh()
		return len(needs)
	}
	refresh()
	return container.NewVScroll(container.NewPadded(box)), refresh
}

func (a *App) needCard(n need) fyne.CanvasObject {
	pm := a.projects()
	answer := func(choice string) func() {
		return func() {
			var err error
			if n.clash != nil {
				err = pm.ResolveClash(n.key, n.clash.Path, choice)
			} else {
				err = pm.Decide(n.key, choice)
			}
			if err != nil {
				dialog.ShowError(err, a.win)
				return
			}
			if a.refreshUI != nil {
				a.refreshUI()
			}
			go a.doSync(true)
		}
	}
	btn := func(label, choice string, important bool) *widget.Button {
		b := widget.NewButton(label, answer(choice))
		if important {
			b.Importance = widget.HighImportance
		}
		return b
	}
	if c := n.clash; c != nil {
		msg := fmt.Sprintf("%s: %s was changed on this PC and on %s.\nThis PC: %d KB, %s   ·   %s: %d KB, %s",
			n.name, c.Path, c.RemoteDevice, c.LocalSize>>10, c.LocalTime.Local().Format("Jan 2 15:04"),
			c.RemoteDevice, c.RemoteSize>>10, c.RemoteTime.Local().Format("Jan 2 15:04"))
		return container.NewVBox(wrapLabel(msg), container.NewHBox(
			btn("Keep mine", "mine", false), btn("Take theirs", "theirs", false), btn("Keep both", "both", true)))
	}
	d := n.decision
	var buttons []fyne.CanvasObject
	switch d.Kind {
	case "first-link":
		buttons = []fyne.CanvasObject{btn("Merge", projsync.LinkMerge, true), btn("Use the server's copy", projsync.LinkServer, false),
			btn("Use this PC's copy", projsync.LinkLocal, false), btn("Not now", "no", false)}
	case "local-deletes":
		buttons = []fyne.CanvasObject{btn("Put them back here", "restore", true), btn("Delete on my other PCs too", "yes", false)}
	case "large":
		buttons = []fyne.CanvasObject{btn("Go ahead", "yes", true), btn("Turn this project off", "no", false)}
	case "git":
		buttons = []fyne.CanvasObject{btn("Sync anyway", "yes", false), btn("Not now", "no", true)}
	default: // deletes, big-change
		buttons = []fyne.CanvasObject{btn("Go ahead", "yes", true), btn("Not now", "no", false)}
	}
	parts := []fyne.CanvasObject{bold(n.name), wrapLabel(d.Message)}
	if len(d.Paths) > 0 {
		show := d.Paths
		more := ""
		if len(show) > 8 {
			more = fmt.Sprintf("\n…and %d more", len(show)-8)
			show = show[:8]
		}
		parts = append(parts, wrapLabel(strings.Join(show, "\n")+more))
	}
	parts = append(parts, container.NewHBox(buttons...))
	return container.NewVBox(parts...)
}

func (a *App) onServerView() fyne.CanvasObject {
	var items []projsync.RemoteProject
	info := wrapLabel("")
	list := widget.NewList(func() int { return len(items) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Download to…", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r := items[i]
			b := o.(*fyne.Container)
			kind := "synced"
			if r.Backup {
				kind = "backup"
			}
			b.Objects[0].(*widget.Label).SetText(fmt.Sprintf("%s  ·  %s  ·  v%d from %s, %s", r.Title, kind, r.Version, r.Device, r.Time.Local().Format("Jan 2 15:04")))
			b.Objects[1].(*widget.Button).OnTapped = func() { a.downloadProject(r) }
		})
	load := func() {
		pm := a.projects()
		if pm == nil {
			return
		}
		go func() {
			list2, err := pm.RemoteProjects(context.Background())
			fyne.Do(func() {
				if err != nil {
					info.SetText(err.Error())
					return
				}
				items = items[:0]
				for _, r := range list2 {
					if !r.Removed {
						items = append(items, r)
					}
				}
				if len(items) == 0 {
					info.SetText("No project folders on the server yet. Set a project to Back up only or Sync in Folders.")
				} else {
					info.SetText("Projects on your server. Download one into a new, empty folder; synced projects then stay in sync there.")
				}
				list.Refresh()
			})
		}()
	}
	load()
	return container.NewPadded(container.NewBorder(container.NewVBox(info, widget.NewButton("Refresh", load)), nil, nil, nil, list))
}

func (a *App) downloadProject(r projsync.RemoteProject) {
	d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
		if err != nil || u == nil {
			return
		}
		name := strings.TrimSuffix(r.Title, " (backup from "+r.Device+")")
		folder := filepath.Join(u.Path(), name)
		dialog.ShowConfirm("Download "+name+"?", fmt.Sprintf("Download into:\n%s\n\nFiles already there with the same name go to the project trash first.", folder), func(ok bool) {
			if !ok {
				return
			}
			go func() {
				n, err := a.projects().Download(context.Background(), r.ID, 0, folder, !r.Backup)
				fyne.Do(func() {
					if err != nil {
						dialog.ShowError(err, a.win)
					}
					if n > 0 {
						msg := fmt.Sprintf("Downloaded %d file(s) to %s.", n, folder)
						if !r.Backup {
							msg += "\nThis folder now syncs with your other PCs."
						}
						dialog.ShowInformation("Done", msg, a.win)
					}
					if a.refreshUI != nil {
						a.refreshUI()
					}
				})
			}()
		}, a.win)
	}, a.win)
	d.Resize(fyne.NewSize(700, 500))
	d.Show()
}

// projectTrashView lists the project trash with "Put back".
func (a *App) projectTrashView() (fyne.CanvasObject, func()) {
	var batches []projsync.TrashBatch
	list := widget.NewList(func() int { return len(batches) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Put back", nil), widget.NewLabel(""))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			bt := batches[i]
			b := o.(*fyne.Container)
			first := ""
			if len(bt.Files) > 0 {
				first = " · " + bt.Files[0].Rel
				if len(bt.Files) > 1 {
					first += fmt.Sprintf(" +%d", len(bt.Files)-1)
				}
			}
			b.Objects[0].(*widget.Label).SetText(fmt.Sprintf("%s · %s · %s%s", bt.Time.Format("2006-01-02 15:04"), bt.Project, bt.Reason, first))
			b.Objects[1].(*widget.Button).OnTapped = func() {
				dialog.ShowConfirm("Put files back?", fmt.Sprintf("Put back the %d file(s) from %s?\nCurrent versions go to the trash first.", len(bt.Files), bt.Time.Format("2006-01-02 15:04")), func(ok bool) {
					if !ok {
						return
					}
					n, err := a.projects().Trash().Restore(bt.ID, nil)
					if err != nil {
						dialog.ShowError(err, a.win)
						return
					}
					dialog.ShowInformation("Done", fmt.Sprintf("Put back %d file(s).", n), a.win)
				}, a.win)
			}
		})
	refresh := func() {
		if pm := a.projects(); pm != nil {
			batches = pm.Trash().List()
			list.Refresh()
		}
	}
	refresh()
	return list, refresh
}

// restoreProjectVersion writes an old project version into its folder.
func (a *App) restoreProjectVersion(id string, version int, title string) {
	pm := a.projects()
	run := func(folder string) {
		go func() {
			n, err := pm.Download(context.Background(), id, version, folder, false)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, a.win)
					return
				}
				dialog.ShowInformation("Restored", fmt.Sprintf("Wrote %d file(s) from version %d into %s. Replaced files are in the project trash.", n, version, folder), a.win)
			})
		}()
	}
	if folder := pm.FolderForItem(id); folder != "" {
		dialog.ShowConfirm("Restore this version?", fmt.Sprintf("Write version %d of %s into %s?\nFiles it replaces go to the project trash; files not in that version are left alone.", version, title, folder), func(ok bool) {
			if ok {
				run(folder)
			}
		}, a.win)
		return
	}
	d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
		if err == nil && u != nil {
			run(filepath.Join(u.Path(), strings.Fields(title + " project")[0]+"-v"+fmt.Sprint(version)))
		}
	}, a.win)
	d.Resize(fyne.NewSize(700, 500))
	d.Show()
}
