package main

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Simplyph0/claudesync/internal/projsync"
	"github.com/Simplyph0/claudesync/internal/syncer"
)

var (
	modeValues = []string{projsync.ModeOff, projsync.ModeBackup, projsync.ModeSync}
	modeLong   = []string{
		"Off: leave this folder alone",
		"Back up only: keep a copy on the server, with history. Files on this PC are never changed.",
		"Sync: keep this folder the same on every PC that is set to Sync",
	}
	clashVals = []string{"", projsync.ClashKeepBoth, projsync.ClashNewest, projsync.ClashAsk}
	clashLong = []string{
		"Use my default (Settings → Project folders)",
		"Keep both: the other copy is saved next to yours, nothing is lost",
		"Newest wins: the most recently edited copy replaces the other (the replaced one goes to the trash)",
		"Ask me: each clash shows up here for you to decide",
	}
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

func subtle(s string) *widget.Label {
	l := wrapLabel(s)
	l.Importance = widget.LowImportance
	return l
}

func importantLabel(s string, imp widget.Importance) *widget.Label {
	l := wrapLabel(s)
	l.Importance = imp
	return l
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// relTime says when something happened in words: "3 min ago".
func relTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < 90*time.Minute:
		return fmt.Sprintf("%d min ago", max(1, int(d.Minutes()+0.5)))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()+0.5))
	case d < 48*time.Hour:
		return "yesterday " + t.Local().Format("15:04")
	}
	return t.Local().Format("Jan 2")
}

// showProblem explains an error in plain words, with what to do about it.
func (a *App) showProblem(title string, err error, folder string) {
	pr := projsync.Explain(err, folder)
	content := container.NewVBox(wrapLabel(pr.What), wrapLabel("What to do: "+pr.Fix),
		widget.NewAccordion(widget.NewAccordionItem("Technical details", subtle(err.Error()))))
	d := dialog.NewCustom(title, "OK", content, a.win)
	d.Resize(fyne.NewSize(520, 260))
	d.Show()
}

// projectsTab holds your project folders, the projects on the server, and
// the folders used for chats from other PCs.
func (a *App) projectsTab() (fyne.CanvasObject, func()) {
	folders, refreshFolders := a.projectFoldersView()
	server := a.onServerView()
	chats, refreshChats := a.chatFoldersTab()
	foldersTab := container.NewTabItem("My folders", folders)
	tabs := container.NewAppTabs(
		foldersTab,
		container.NewTabItem("On the server", server),
		container.NewTabItem("Chat folders", chats),
	)
	refresh := func() {
		n := refreshFolders()
		refreshChats()
		title := "My folders"
		if n > 0 {
			title = fmt.Sprintf("My folders (%d need you)", n)
		}
		if foldersTab.Text != title {
			foldersTab.Text = title
			tabs.Refresh()
		}
	}
	return tabs, refresh
}

// card states, most urgent first
const (
	stNeedsYou = iota
	stProblem
	stBusy
	stOK
	stIdle
)

type cardState struct {
	rank  int
	badge string
	imp   widget.Importance
}

func stateOf(s projsync.Status) cardState {
	switch {
	case s.Mode == projsync.ModeOff:
		return cardState{stIdle, "Off", widget.LowImportance}
	case s.Paused:
		return cardState{stIdle, "Paused", widget.LowImportance}
	case s.Decision != nil || len(s.Clashes) > 0:
		return cardState{stNeedsYou, "Needs you", widget.WarningImportance}
	case s.OnHold != "":
		return cardState{stProblem, "On hold", widget.WarningImportance}
	case s.Active:
		return cardState{stBusy, "Syncing…", widget.HighImportance}
	case s.Error != "":
		return cardState{stProblem, "Problem", widget.DangerImportance}
	case s.LastSync.IsZero():
		return cardState{stBusy, "Not synced yet", widget.MediumImportance}
	case s.Mode == projsync.ModeBackup && s.Status == "backed up":
		return cardState{stOK, "Backed up", widget.SuccessImportance}
	case s.Status == "up to date":
		return cardState{stOK, "Up to date", widget.SuccessImportance}
	}
	return cardState{stBusy, "Waiting", widget.WarningImportance}
}

func modeLine(s projsync.Status) string {
	switch s.Mode {
	case projsync.ModeBackup:
		return "Backing up to your server (one way)"
	case projsync.ModeSync:
		return "Syncing with your other PCs (both ways)"
	}
	return "Not synced"
}

func (a *App) projectFoldersView() (fyne.CanvasObject, func() int) {
	box := container.NewVBox()
	summary := bold("")
	var refresh func() int
	var lastSig string
	var lastNeeds int

	add := widget.NewButtonWithIcon("Add folder…", theme.ContentAddIcon(), func() { a.pickFolderToAdd() })
	add.Importance = widget.HighImportance
	syncAll := widget.NewButtonWithIcon("Sync all now", theme.ViewRefreshIcon(), func() { go a.doSync(true) })
	help := widget.NewButtonWithIcon("How this works", theme.HelpIcon(), a.showProjectHelp)
	header := container.NewVBox(container.NewBorder(nil, nil, nil, container.NewHBox(add, syncAll, help), summary), widget.NewSeparator())

	refresh = func() int {
		pm := a.projects()
		e := a.engine()
		if pm == nil || e == nil {
			box.RemoveAll()
			lastSig = ""
			summary.SetText("Not connected yet")
			box.Refresh()
			return 0
		}
		all := pm.Projects()
		sort.SliceStable(all, func(i, j int) bool {
			ri, rj := stateOf(all[i]).rank, stateOf(all[j]).rank
			if ri != rj {
				return ri < rj
			}
			return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
		})
		// Rebuilding the cards replaces their buttons, and a click that lands
		// on a button that is replaced between pressing and releasing is lost.
		// So only rebuild when something shown has changed; progress text is
		// updated in place.
		var sig strings.Builder
		for _, s := range all {
			sig.WriteString(projectSig(s))
		}
		for key, folder := range e.KnownProjects() {
			sig.WriteString(key + "=" + folder + ";")
		}
		if sig.String() == lastSig {
			for _, s := range all {
				if l := a.progressLbl[s.Key]; l != nil && s.Active {
					l.SetText(s.Progress)
				}
			}
			return lastNeeds
		}
		lastSig = sig.String()
		a.progressLbl = map[string]*widget.Label{}
		box.RemoveAll()
		var needs, problems, busy int
		configured := map[string]bool{}
		for _, s := range all {
			configured[s.Key] = true
			switch stateOf(s).rank {
			case stNeedsYou:
				needs++
			case stProblem:
				problems++
			case stBusy:
				busy++
			}
			box.Add(a.projectCard(s))
		}
		var chatOnly []string
		for key, folder := range e.KnownProjects() {
			set := configured[key]
			for _, s := range all { // the same folder under another key (a copy without .git) is still set up
				set = set || samePathUI(s.Folder, folder)
			}
			if !set {
				chatOnly = append(chatOnly, key+"\x00"+folder)
			}
		}
		sort.Strings(chatOnly)
		if len(all) == 0 {
			box.Add(widget.NewCard("Nothing is backed up or synced yet", "",
				container.NewVBox(wrapLabel("Project folders are the files you work on: code, notes, Arduino sketches.\n\n"+
					"Click Add folder…, pick one, and choose whether to back it up to your server or keep it the same on all your PCs. "+
					"Nothing is changed until you choose."))))
		}
		if len(chatOnly) > 0 {
			box.Add(bold("Folders your chats use that aren't set up"))
			for _, kv := range chatOnly {
				folder := kv[strings.Index(kv, "\x00")+1:]
				row := container.NewBorder(nil, nil, nil, widget.NewButton("Set up…", func() { a.addFolder(folder) }),
					container.NewVBox(bold(filepath.Base(folder)), subtle(folder)))
				box.Add(row)
			}
		}
		switch {
		case len(all) == 0:
			summary.SetText("No folders yet")
		default:
			parts := []string{plural(len(all), "folder", "folders")}
			if needs > 0 {
				parts = append(parts, fmt.Sprintf("%d need you", needs))
			}
			if problems > 0 {
				parts = append(parts, plural(problems, "has a problem", "have a problem"))
			}
			if busy > 0 {
				parts = append(parts, fmt.Sprintf("%d working", busy))
			}
			if needs+problems+busy == 0 {
				parts = append(parts, "all up to date")
			}
			summary.SetText(strings.Join(parts, " · "))
		}
		box.Refresh()
		lastNeeds = needs + problems
		return lastNeeds
	}
	refresh()

	// While something is syncing, keep the progress text fresh.
	go func() {
		t := time.NewTicker(1500 * time.Millisecond)
		defer t.Stop()
		wasBusy := false
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-t.C:
			}
			pm := a.projects()
			if pm == nil {
				continue
			}
			busy := false
			for _, s := range pm.Projects() {
				busy = busy || s.Active
			}
			if busy || wasBusy {
				fyne.Do(func() { refresh() })
			}
			wasBusy = busy
		}
	}()
	return container.NewBorder(header, nil, nil, nil, container.NewVScroll(container.NewPadded(box))), refresh
}

// projectCard is one folder: what it is, how it's doing, and what to do next.
func (a *App) projectCard(s projsync.Status) fyne.CanvasObject {
	cs := stateOf(s)
	badge := widget.NewLabelWithStyle(cs.badge, fyne.TextAlignTrailing, fyne.TextStyle{Bold: true})
	badge.Importance = cs.imp
	title := container.NewBorder(nil, nil, nil, badge, bold(s.Name))
	folder := subtle(modeLine(s) + " · " + s.Folder)
	folder.Wrapping = fyne.TextWrapOff
	folder.Truncation = fyne.TextTruncateEllipsis

	parts := []fyne.CanvasObject{title, folder}
	switch {
	case s.Mode == projsync.ModeOff:
		parts = append(parts, subtle("Not synced. Open Details to turn it on."))
	case s.Paused:
		parts = append(parts, subtle("Paused. Nothing is sent or received until you resume."))
	case s.Active:
		l := wrapLabel(s.Progress)
		if a.progressLbl != nil {
			a.progressLbl[s.Key] = l
		}
		parts = append(parts, l)
	}
	if s.Mode != projsync.ModeOff && !s.Paused {
		if s.Error != "" && !s.Active {
			parts = append(parts, importantLabel("✖ "+s.Error, widget.DangerImportance), wrapLabel("What to do: "+s.Fix))
		}
		if s.Decision != nil {
			parts = append(parts, a.questionBox(s.Key, s.Decision, nil))
		}
		if s.OnHold != "" && s.Decision == nil {
			again := widget.NewButton("Ask me again", func() {
				if pm := a.projects(); pm != nil {
					_ = pm.AskAgain(s.Key)
				}
				a.syncProject(s.Key)
			})
			parts = append(parts, importantLabel("⏸ On hold. You chose Not now, so nothing is synced here until something changes: "+s.OnHold, widget.WarningImportance), again)
		}
		for i := range s.Clashes {
			parts = append(parts, a.questionBox(s.Key, nil, &s.Clashes[i]))
		}
		if cs.rank == stOK || (cs.rank == stBusy && !s.Active && s.Error == "" && !s.LastSync.IsZero()) {
			line := "✔ Checked " + relTime(s.LastSync)
			if s.Status != "up to date" && s.Status != "backed up" && s.Status != "" {
				line = capFirst(s.Status)
			}
			if s.Last != nil {
				line += fmt.Sprintf(" · %s, %s", plural(s.Last.Files, "file", "files"), humanBytes(s.Last.Bytes))
				if s.Last.Changed() {
					line += " · last change: " + s.Last.Summary()
				}
			}
			parts = append(parts, wrapLabel(line))
		}
		if len(s.Skipped) > 0 {
			parts = append(parts, importantLabel(fmt.Sprintf("⚠ %s left out. Open Details to see which and why.", plural(len(s.Skipped), "file is", "files are")), widget.WarningImportance))
		}
	}

	syncBtn := widget.NewButtonWithIcon("Sync now", theme.ViewRefreshIcon(), func() { a.syncProject(s.Key) })
	if s.Mode == projsync.ModeOff || s.Paused || s.Active {
		syncBtn.Disable()
	}
	detail := widget.NewButtonWithIcon("Details", theme.SettingsIcon(), func() { a.projectDetails(s.Key) })
	parts = append(parts, container.NewHBox(syncBtn, detail))
	return widget.NewCard("", "", container.NewVBox(parts...))
}

// projectSig is everything a folder card shows, except live progress, so the
// cards are only rebuilt when something on them changes.
func projectSig(s projsync.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%t|%t|%s|%s|%s|%s|%s|%d|", s.Key, s.Name, s.Folder, s.Mode, s.Paused, s.Active, s.Status, s.Error, s.Fix, s.OnHold,
		relTime(s.LastSync), len(s.Skipped))
	if s.Decision != nil {
		b.WriteString(s.Decision.Kind + s.Decision.Message)
	}
	for _, c := range s.Clashes {
		b.WriteString("|" + c.Path + c.RemoteDevice)
	}
	if s.Last != nil {
		fmt.Fprintf(&b, "|%d|%d|%s", s.Last.Files, s.Last.Bytes, s.Last.Summary())
	}
	return b.String() + ";"
}

// syncProject syncs one folder now.
func (a *App) syncProject(key string) {
	pm := a.projects()
	if pm == nil {
		return
	}
	go func() {
		_ = pm.Sync(a.ctx, key) // the result shows on the folder's card
		fyne.Do(func() {
			if a.refreshUI != nil {
				a.refreshUI()
			}
		})
	}()
	if a.refreshUI != nil {
		a.refreshUI()
	}
}

func (a *App) showProjectHelp() {
	d := dialog.NewCustom("How project folders work", "Got it", container.NewVScroll(wrapLabel(
		"Each folder is set to one of three things:\n\n"+
			"• Off: ClaudeSync ignores it.\n"+
			"• Back up only: a copy of the folder, with history, is kept on your server. Files on this PC are never changed, and other PCs aren't touched unless you download a copy.\n"+
			"• Sync: the folder is kept the same on every PC where you set it to Sync. New and changed files travel both ways.\n\n"+
			"If the same file changed on two PCs, text files are merged line by line automatically. Anything else follows the folder's choice: keep both, newest wins, or ask you.\n\n"+
			"Nothing is ever lost quietly: a file that gets replaced or removed goes to the project trash first (Recover tab), and the server keeps earlier versions (History tab).\n\n"+
			"Folders git already keeps in step (pushed to GitHub) are best left on Back up only.\n\n"+
			"A folder that goes missing, for example an unplugged drive, is never treated as \"everything deleted\". ClaudeSync stops and tells you.\n\n"+
			"\"Don't sync these\" in Details skips files on this PC only, like a .gitignore. Common clutter (node_modules, build, .git) is always skipped.")), a.win)
	d.Resize(fyne.NewSize(560, 460))
	d.Show()
}

// questionBox is something waiting on you: a decision or a clash.
func (a *App) questionBox(key string, d *projsync.Decision, c *projsync.Clash) fyne.CanvasObject {
	pm := a.projects()
	answer := func(choice string) func() {
		return func() {
			var err error
			if c != nil {
				err = pm.ResolveClash(key, c.Path, choice)
			} else {
				err = pm.Decide(key, choice)
			}
			if err != nil {
				a.showProblem("Couldn't save your answer", err, "")
				return
			}
			if choice != "no" { // Not now means stop: don't run the same check again
				go func() {
					_ = pm.Sync(a.ctx, key)
					fyne.Do(func() {
						if a.refreshUI != nil {
							a.refreshUI()
						}
					})
				}()
			}
			if a.refreshUI != nil {
				a.refreshUI()
			}
		}
	}
	btn := func(label, choice string, important bool) *widget.Button {
		b := widget.NewButton(label, answer(choice))
		if important {
			b.Importance = widget.HighImportance
		}
		return b
	}
	if c != nil {
		msg := fmt.Sprintf("%s was changed on this PC and on %s.\nThis PC: %s, edited %s   ·   %s: %s, edited %s",
			c.Path, c.RemoteDevice, humanBytes(c.LocalSize), c.LocalTime.Local().Format("Jan 2 15:04"),
			c.RemoteDevice, humanBytes(c.RemoteSize), c.RemoteTime.Local().Format("Jan 2 15:04"))
		return container.NewVBox(importantLabel("? "+msg, widget.WarningImportance), container.NewHBox(
			btn("Keep mine", "mine", false), btn("Take theirs", "theirs", false), btn("Keep both", "both", true)),
			subtle("Keep both saves the other copy next to yours. Whatever is replaced goes to the trash first."))
	}
	var buttons []fyne.CanvasObject
	help := ""
	switch d.Kind {
	case "first-link":
		buttons = []fyne.CanvasObject{btn("Merge", projsync.LinkMerge, true), btn("Use the server's copy", projsync.LinkServer, false),
			btn("Use this PC's copy", projsync.LinkLocal, false), btn("Not now", "no", false)}
		help = "Merge keeps everything from both sides. Use the server's copy makes this folder match the server; use this PC's copy makes the server match this folder. Anything replaced goes to the trash first."
	case "local-deletes":
		buttons = []fyne.CanvasObject{btn("Put them back here", "restore", true), btn("Delete on my other PCs too", "yes", false)}
		help = "If a drive isn't plugged in or a folder was emptied by mistake, put them back."
	case "large":
		buttons = []fyne.CanvasObject{btn("Go ahead", "yes", true), btn("Turn this folder off", "no", false)}
	case "git":
		buttons = []fyne.CanvasObject{btn("Sync anyway", "yes", false), btn("Not now", "no", true)}
		help = "Two PCs on different git branches or commits would swap files that belong to different versions of the project."
	default: // deletes, big-change
		buttons = []fyne.CanvasObject{btn("Go ahead", "yes", true), btn("Not now", "no", false)}
		help = "The files go to the project trash, not away for good."
	}
	parts := []fyne.CanvasObject{importantLabel("? "+d.Message, widget.WarningImportance)}
	if len(d.Paths) > 0 {
		show := d.Paths
		more := ""
		if len(show) > 8 {
			more = fmt.Sprintf("\n…and %d more", len(show)-8)
			show = show[:8]
		}
		parts = append(parts, subtle(strings.Join(show, "\n")+more))
	}
	parts = append(parts, container.NewHBox(buttons...))
	if help != "" {
		parts = append(parts, subtle(help))
	}
	return container.NewVBox(parts...)
}

// pickFolderToAdd asks for a folder, then shows what's in it before adding.
func (a *App) pickFolderToAdd() {
	d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
		if err != nil {
			a.showProblem("Couldn't open that folder", err, "")
			return
		}
		if u != nil {
			a.addFolder(u.Path())
		}
	}, a.win)
	d.Resize(fyne.NewSize(700, 500))
	d.Show()
}

// addFolder looks at a folder and lets you choose what to do with it.
func (a *App) addFolder(folder string) {
	pm := a.projects()
	if pm == nil {
		return
	}
	wait := dialog.NewCustomWithoutButtons("Looking at "+filepath.Base(folder), container.NewVBox(wrapLabel("Counting files…"), widget.NewProgressBarInfinite()), a.win)
	wait.Show()
	go func() {
		fi, err := projsync.Inspect(folder, a.settings().MaxFileMB, nil)
		fyne.Do(func() {
			wait.Hide()
			if err != nil {
				a.showProblem("Can't use that folder", err, folder)
				return
			}
			a.confirmAddFolder(folder, fi)
		})
	}()
}

func (a *App) confirmAddFolder(folder string, fi projsync.FolderInfo) {
	pm := a.projects()
	ref := syncer.DescribeProject(folder)
	c := a.settings()

	facts := []string{fmt.Sprintf("%s, %s", plural(fi.Files, "file", "files"), humanBytes(fi.Bytes))}
	if fi.Truncated {
		facts[0] = "More than " + facts[0]
	}
	parts := []fyne.CanvasObject{bold(filepath.Base(folder)), subtle(folder), wrapLabel(facts[0])}
	switch {
	case fi.HasRemote:
		parts = append(parts, wrapLabel("This folder is a git repository with a remote. Git already keeps it in step across PCs, so Back up only is usually enough."))
	case fi.Git != nil:
		parts = append(parts, wrapLabel("This folder is a git repository without a remote. Sync would carry your unpushed work to your other PCs."))
	}
	if len(fi.Ignored) > 0 {
		parts = append(parts, subtle("Skipped on purpose: "+strings.Join(fi.Ignored, ", ")))
	}
	for _, w := range fi.Warnings {
		parts = append(parts, importantLabel("⚠ "+w, widget.WarningImportance))
	}
	if limit := int64(c.MaxProjectMB) << 20; c.MaxProjectMB > 0 && fi.Bytes > limit {
		parts = append(parts, importantLabel(fmt.Sprintf("⚠ This is over your %d MB project limit (Settings). You'll be asked before anything is sent.", c.MaxProjectMB), widget.WarningImportance))
	}

	mode := widget.NewRadioGroup(modeLong[1:], nil)
	choice := projsync.ModeBackup
	if c.ProjectDefault == projsync.ModeSync && !fi.HasRemote {
		choice = projsync.ModeSync
	}
	mode.SetSelected(modeLong[indexOf(modeValues, choice)])
	parts = append(parts, widget.NewSeparator(), bold("What should ClaudeSync do with it?"), mode,
		subtle("You can change this any time in Details. Nothing on this PC is changed until a sync, and anything replaced goes to the trash first."))

	existing, has := pm.Get(ref.Key)
	if has && !samePathUI(existing.Folder, folder) {
		parts = append(parts, importantLabel(fmt.Sprintf("⚠ ClaudeSync already has this project at %s. Adding this folder replaces that one and starts a fresh comparison with the server.", existing.Folder), widget.WarningImportance))
	}

	scroll := container.NewVScroll(container.NewVBox(parts...))
	scroll.SetMinSize(fyne.NewSize(520, 360))
	d := dialog.NewCustomConfirm("Add this folder?", "Add", "Cancel", scroll, func(ok bool) {
		if !ok {
			return
		}
		p := projsync.Project{Key: ref.Key, Name: filepath.Base(folder), Folder: folder, Mode: modeValues[1:][indexOf(modeLong[1:], mode.Selected)]}
		if has {
			p.Clash, p.Paused, p.Ignore, p.AllowLarge = existing.Clash, existing.Paused, existing.Ignore, existing.AllowLarge
		}
		a.saveProject(p)
	}, a.win)
	d.Resize(fyne.NewSize(580, 480))
	d.Show()
}

func samePathUI(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func (a *App) saveProject(p projsync.Project) {
	pm := a.projects()
	if pm == nil {
		return
	}
	if err := pm.SetProject(p); err != nil {
		dialog.ShowCustom("Can't use that folder", "OK", container.NewVBox(wrapLabel(err.Error())), a.win)
		return
	}
	a.log.Printf("project %s: saved (%s%s)", p.Name, p.Mode, map[bool]string{true: ", paused"}[p.Paused])
	if a.refreshUI != nil {
		a.refreshUI()
	}
	if p.Mode != projsync.ModeOff && !p.Paused {
		a.syncProject(p.Key)
	}
}

func (a *App) openFolder(folder string) {
	u, err := url.Parse(storage.NewFileURI(folder).String())
	if err == nil {
		err = a.fa.OpenURL(u)
	}
	if err != nil {
		a.showProblem("Couldn't open the folder", err, folder)
	}
}

// projectDetails is everything about one folder: settings, activity, and
// anything left out.
func (a *App) projectDetails(key string) {
	pm := a.projects()
	if pm == nil {
		return
	}
	var st projsync.Status
	found := false
	for _, s := range pm.Projects() {
		if s.Key == key {
			st, found = s, true
		}
	}
	if !found {
		return
	}
	p := st.Project

	var dlg *dialog.ConfirmDialog
	mode := widget.NewRadioGroup(modeLong, nil)
	mode.SetSelected(modeLong[indexOf(modeValues, p.Mode)])
	clash := widget.NewRadioGroup(clashLong, nil)
	clash.SetSelected(clashLong[indexOf(clashVals, p.Clash)])
	syncOnly := func() {
		if modeValues[indexOf(modeLong, mode.Selected)] == projsync.ModeSync {
			clash.Enable()
		} else {
			clash.Disable()
		}
	}
	mode.OnChanged = func(string) { syncOnly() }
	syncOnly()
	paused := widget.NewCheck("Pause: stay set up, but send and receive nothing until I resume", nil)
	paused.SetChecked(p.Paused)
	ignore := widget.NewMultiLineEntry()
	ignore.SetText(strings.Join(p.Ignore, "\n"))
	ignore.SetPlaceHolder("*.log\nbuild/\nsecret.env")
	ignore.SetMinRowsVisible(4)

	folderBtns := container.NewHBox(
		widget.NewButtonWithIcon("Open folder", theme.FolderOpenIcon(), func() { a.openFolder(p.Folder) }),
		widget.NewButton("Change folder…", func() {
			pick := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
				if err != nil || u == nil {
					return
				}
				dialog.ShowConfirm("Use this folder instead?", fmt.Sprintf("%s\n\nClaudeSync will start fresh with this folder: the next sync compares it with the copy on the server. Your files are not deleted, and the old folder is left as it is.", u.Path()), func(ok bool) {
					if !ok {
						return
					}
					np := p
					np.Folder = u.Path()
					dlg.Hide()
					a.saveProject(np)
				}, a.win)
			}, a.win)
			pick.Resize(fyne.NewSize(700, 500))
			pick.Show()
		}))

	var problem, rest []fyne.CanvasObject
	if st.Error != "" && !st.Active {
		problem = append(problem, importantLabel("✖ "+st.Error, widget.DangerImportance), wrapLabel("What to do: "+st.Fix))
		if st.Detail != "" {
			problem = append(problem, widget.NewAccordion(widget.NewAccordionItem("Technical details", subtle(st.Detail))))
		}
		problem = append(problem, widget.NewSeparator())
	}
	if len(st.Skipped) > 0 {
		rest = append(rest, bold(fmt.Sprintf("Left out (%d)", len(st.Skipped))))
		for i, sk := range st.Skipped {
			if i == 12 {
				rest = append(rest, subtle(fmt.Sprintf("…and %d more", len(st.Skipped)-12)))
				break
			}
			rest = append(rest, subtle(sk.Path+": "+sk.Why))
		}
		rest = append(rest, widget.NewSeparator())
	}
	rest = append(rest, bold("Recent activity"))
	if len(st.Events) == 0 {
		rest = append(rest, subtle("Nothing yet."))
	}
	for i := len(st.Events) - 1; i >= 0 && i >= len(st.Events)-25; i-- {
		ev := st.Events[i]
		icon, imp := "•", widget.MediumImportance
		switch ev.Level {
		case "error":
			icon, imp = "✖", widget.DangerImportance
		case "warn":
			icon, imp = "⚠", widget.WarningImportance
		}
		when := ev.Time.Local().Format("Jan 2 15:04")
		if time.Since(ev.Time) < 24*time.Hour {
			when = ev.Time.Local().Format("15:04")
		}
		msg := ev.Msg
		if ev.N > 0 {
			msg += fmt.Sprintf(" (and %d more times)", ev.N)
		}
		rest = append(rest, importantLabel(when+"  "+icon+" "+msg, imp))
	}

	forget := widget.NewButtonWithIcon("Stop managing this folder", theme.DeleteIcon(), func() {
		dialog.ShowConfirm("Stop managing "+p.Name+"?", "ClaudeSync forgets this folder on this PC.\n\nYour files stay where they are, and the copy on the server stays too (remove it under History if you want it gone). Other PCs aren't affected.", func(ok bool) {
			if !ok {
				return
			}
			if err := pm.Forget(key); err != nil {
				a.showProblem("Couldn't stop managing it", err, p.Folder)
				return
			}
			a.log.Printf("project %s: no longer managed on this PC", p.Name)
			dlg.Hide()
			if a.refreshUI != nil {
				a.refreshUI()
			}
		}, a.win)
	})

	content := []fyne.CanvasObject{bold(p.Name), subtle(p.Folder), folderBtns, widget.NewSeparator()}
	content = append(content, problem...)
	content = append(content, bold("What should ClaudeSync do with this folder?"), mode, paused, widget.NewSeparator(),
		bold("If the same file changed on two PCs"), clash,
		subtle("Text files edited in different places are merged automatically before this applies."), widget.NewSeparator(),
		bold("Don't sync these (this PC only)"), ignore,
		subtle("One per line, like a .gitignore: *.log, build/, secret.env. Already skipped: "+strings.Join(projsync.DefaultIgnores(), "  ")),
		widget.NewSeparator())
	content = append(content, rest...)
	content = append(content, widget.NewSeparator(), forget)

	scroll := container.NewVScroll(container.NewPadded(container.NewVBox(content...)))
	scroll.SetMinSize(fyne.NewSize(600, 480))
	dlg = dialog.NewCustomConfirm("Folder details", "Save", "Close", scroll, func(ok bool) {
		if !ok {
			return
		}
		np := p
		np.Mode = modeValues[indexOf(modeLong, mode.Selected)]
		np.Clash = clashVals[indexOf(clashLong, clash.Selected)]
		np.Paused = paused.Checked
		np.Ignore = nil
		for _, l := range strings.Split(ignore.Text, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				np.Ignore = append(np.Ignore, l)
			}
		}
		if np.Mode == p.Mode && np.Clash == p.Clash && np.Paused == p.Paused && strings.Join(np.Ignore, "\n") == strings.Join(p.Ignore, "\n") {
			return
		}
		a.saveProject(np)
	}, a.win)
	dlg.Resize(fyne.NewSize(680, 600))
	dlg.Show()
}

func (a *App) onServerView() fyne.CanvasObject {
	var items []projsync.RemoteProject
	info := wrapLabel("")
	list := widget.NewList(func() int { return len(items) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Download to…", nil), container.NewVBox(bold(""), subtle("")))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r := items[i]
			b := o.(*fyne.Container)
			kind := "kept in sync"
			if r.Backup {
				kind = "backup"
			}
			labels := b.Objects[0].(*fyne.Container)
			labels.Objects[0].(*widget.Label).SetText(strings.TrimSuffix(r.Title, " (backup from "+r.Device+")"))
			labels.Objects[1].(*widget.Label).SetText(fmt.Sprintf("%s · version %d from %s · %s", kind, r.Version, r.Device, relTime(r.Time)))
			b.Objects[1].(*widget.Button).OnTapped = func() { a.downloadProject(r) }
		})
	load := func() {
		pm := a.projects()
		if pm == nil {
			return
		}
		info.SetText("Loading…")
		go func() {
			list2, err := pm.RemoteProjects(context.Background())
			fyne.Do(func() {
				if err != nil {
					pr := projsync.Explain(err, "")
					info.SetText(pr.What + " " + pr.Fix)
					return
				}
				items = items[:0]
				for _, r := range list2 {
					if !r.Removed {
						items = append(items, r)
					}
				}
				if len(items) == 0 {
					info.SetText("Nothing is stored on your server yet. Add a folder in My folders and set it to Back up only or Sync.")
				} else {
					info.SetText("What your server holds. Download one into a new, empty folder to get a copy on this PC. A \"kept in sync\" project then stays in step from there; a backup is just a copy.")
				}
				list.Refresh()
			})
		}()
	}
	load()
	return container.NewPadded(container.NewBorder(container.NewVBox(info, widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), load)), nil, nil, nil, list))
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
						a.showProblem("Download didn't finish", err, folder)
					}
					if n > 0 {
						msg := fmt.Sprintf("Downloaded %s to %s.", plural(n, "file", "files"), folder)
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
						a.showProblem("Couldn't put the files back", err, "")
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
					a.showProblem("Couldn't restore that version", err, folder)
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

// projectSummaryLine fills the Status tab's one-line view of the project
// folders, so a problem or a question isn't hidden behind another tab.
func (a *App) projectSummaryLine(l *widget.Label) {
	pm := a.projects()
	var on, needs, problems, paused int
	if pm != nil {
		for _, s := range pm.Projects() {
			if s.Mode == projsync.ModeOff {
				continue
			}
			on++
			switch {
			case s.Paused:
				paused++
			case s.Decision != nil || len(s.Clashes) > 0:
				needs++
			case s.Error != "" && !s.Active:
				problems++
			}
		}
	}
	switch {
	case on == 0:
		l.Hide()
		return
	case needs+problems > 0:
		parts := []string{}
		if needs > 0 {
			parts = append(parts, fmt.Sprintf("%d need you", needs))
		}
		if problems > 0 {
			parts = append(parts, plural(problems, "has a problem", "have a problem"))
		}
		l.Importance = widget.WarningImportance
		l.SetText("Project folders: " + strings.Join(parts, ", ") + ". Open the Projects tab.")
	default:
		l.Importance = widget.MediumImportance
		text := "Project folders: " + plural(on, "folder", "folders") + ", all up to date."
		if paused > 0 {
			text = fmt.Sprintf("Project folders: %s (%d paused).", plural(on, "folder", "folders"), paused)
		}
		l.SetText(text)
	}
	l.Show()
}
