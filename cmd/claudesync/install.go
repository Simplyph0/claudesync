package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

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

	// A running copy holds the file and the settings: ask it to quit first.
	// (Versions before 0.4.3 ignore this and keep running until next login.)
	if c, err := net.DialTimeout("tcp", instanceAddr, time.Second); err == nil {
		fmt.Fprintln(c, "quit")
		c.Close()
		for i := 0; i < 40 && instanceRunning(); i++ {
			time.Sleep(250 * time.Millisecond)
		}
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

func instanceRunning() bool {
	ln, err := net.Listen("tcp", instanceAddr)
	if err != nil {
		return true
	}
	ln.Close()
	return false
}
