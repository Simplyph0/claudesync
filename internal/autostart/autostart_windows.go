// Package autostart turns "start at login" on and off.
package autostart

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// Set turns start at login on or off for this program.
func Set(enabled bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return SetExe(enabled, exe)
}

// SetExe turns start at login on or off for the program at exe.
func SetExe(enabled bool, exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enabled {
		if err := k.DeleteValue("ClaudeSync"); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	return k.SetStringValue("ClaudeSync", `"`+exe+`" --hidden`)
}
