package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/Simplyph0/claudesync/internal/appcfg"
	"github.com/Simplyph0/claudesync/internal/autostart"
	"github.com/Simplyph0/claudesync/internal/install"
)

// handOffToInstalled installs this copy (say, a fresh download) to the
// fixed install folder, replacing any older version there, points start at
// login at it and starts it. It returns false when this copy should just
// keep running: it already is the installed one, or installing failed.
func handOffToInstalled() bool {
	exe, err := os.Executable()
	if err != nil || install.Dev(exe) {
		return false
	}
	dst, err := install.Path()
	if err != nil {
		return false
	}
	if install.Same(exe, dst) {
		install.Cleanup(dst)
		return false
	}
	log := newLogBuf(appcfg.Dir())

	// A running copy holds the port and the settings: it has to go first,
	// or this update would quietly leave the old version running.
	if err := stopRunning(instanceAddr); err != nil {
		log.Printf("couldn't close the running ClaudeSync, so nothing was installed: %v", err)
		showFatal("ClaudeSync is already running and couldn't be closed, so this update wasn't installed.\n\n" +
			"Quit ClaudeSync from its tray icon, then open this file again.\n\n" + err.Error())
		return true
	}

	changed, err := install.Copy(exe, dst)
	if err != nil {
		log.Printf("couldn't install to %s, running from %s: %v", dst, exe, err)
		return false
	}
	if changed {
		log.Printf("installed to %s (from %s)", dst, exe)
	}
	if _, err := os.Stat(filepath.Join(appcfg.Dir(), "settings.json")); err == nil {
		if cfg, err := appcfg.Load(); err == nil && cfg.StartAtLogin {
			if err := autostart.SetExe(true, dst); err != nil {
				log.Printf("start at login: %v", err)
			}
		}
	}
	if err := install.MenuEntry(dst, iconIdle.Content()); err != nil {
		log.Printf("app menu entry: %v", err)
	}

	cmd := exec.Command(dst, os.Args[1:]...)
	cmd.Dir = filepath.Dir(dst)
	if err := cmd.Start(); err != nil {
		log.Printf("couldn't start %s, running from %s: %v", dst, exe, err)
		return false
	}
	_ = cmd.Process.Release()
	return true
}

func instanceRunning() bool { return portHeld(instanceAddr) }

func portHeld(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

func waitFree(addr string, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if !portHeld(addr) {
			return true
		}
	}
	return !portHeld(addr)
}

// quitWait is how long a running copy gets to quit when asked nicely.
var quitWait = 8 * time.Second

// stopRunning closes a running ClaudeSync and waits until it has let go of
// its port. It asks politely first. Versions before 0.5.2 don't understand
// that, so if the port is still held, the process holding it is stopped, but
// only if it is ClaudeSync.
func stopRunning(addr string) error {
	if !portHeld(addr) {
		return nil
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		fmt.Fprintln(c, "quit")
		c.Close()
	}
	if waitFree(addr, quitWait) {
		return nil
	}
	pid, what, err := portOwner(addr)
	if err != nil {
		return fmt.Errorf("something is using %s and it can't be identified: %w", addr, err)
	}
	if !strings.Contains(strings.ToLower(what), "claudesync") {
		return fmt.Errorf("%s is in use by %s, which isn't ClaudeSync", addr, what)
	}
	if int(pid) == os.Getpid() {
		return fmt.Errorf("%s is held by this very process", addr)
	}
	p, err := process.NewProcess(pid)
	if err != nil {
		return err
	}
	_ = p.Terminate()
	if !waitFree(addr, 5*time.Second) {
		_ = p.Kill()
	}
	if !waitFree(addr, 5*time.Second) {
		return fmt.Errorf("the old ClaudeSync (process %d) didn't stop", pid)
	}
	return nil
}

// portOwner finds the process listening on addr and says what it is.
func portOwner(addr string) (int32, string, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, "", err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, "", err
	}
	conns, err := gnet.Connections("tcp")
	if err != nil {
		return 0, "", err
	}
	for _, c := range conns {
		if c.Status == "LISTEN" && int(c.Laddr.Port) == port && c.Pid != 0 {
			p, err := process.NewProcess(c.Pid)
			if err != nil {
				return 0, "", err
			}
			name, _ := p.Name()
			exe, _ := p.Exe()
			return c.Pid, name + " " + exe, nil
		}
	}
	return 0, "", fmt.Errorf("no process found listening on port %d", port)
}

// showFatal tells the user something they have to act on, in a small window
// (there is no console to print to).
func showFatal(msg string) {
	a := app.NewWithID("io.github.simplyph0.claudesync")
	w := a.NewWindow("ClaudeSync")
	l := widget.NewLabel(msg)
	l.Wrapping = fyne.TextWrapWord
	w.SetContent(container.NewVBox(l, widget.NewButton("OK", a.Quit)))
	w.Resize(fyneSize(460, 220))
	w.ShowAndRun()
}

func fyneSize(w, h float32) fyne.Size { return fyne.NewSize(w, h) }
