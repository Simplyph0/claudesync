//go:build !windows

// Package autostart turns "start at login" on and off.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

// Set writes or removes an XDG autostart entry.
func Set(enabled bool) error {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		cfg = filepath.Join(home, ".config")
	}
	path := filepath.Join(cfg, "autostart", "claudesync.desktop")
	if !enabled {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=ClaudeSync\nComment=Sync Claude sessions between your PCs\nExec=%q --hidden\nX-GNOME-Autostart-enabled=true\nTerminal=false\n", exe)
	return os.WriteFile(path, []byte(entry), 0o644)
}
