// Package install keeps the client in one fixed place, so a copy run from
// Downloads (or anywhere else) installs itself there and the login entry
// always starts the newest version.
package install

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Path is where the client lives: %LOCALAPPDATA%\Programs\ClaudeSync on
// Windows, ~/.local/bin on Linux.
func Path() (string, error) {
	if runtime.GOOS == "windows" {
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(dir, "Programs", "ClaudeSync", "claudesync.exe"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "claudesync"), nil
}

// Same reports whether two paths name the same file.
func Same(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Dev reports whether exe is a throwaway build from go run or go test,
// which must never install itself.
func Dev(exe string) bool {
	return strings.Contains(filepath.ToSlash(exe), "/go-build")
}

// Copy puts the program at src into dst, replacing what's there. It works
// while the old copy is running: on Windows the running file is renamed
// out of the way (it can't be overwritten), elsewhere the new file is
// renamed over it. It does nothing if dst already holds the same program.
func Copy(src, dst string) (changed bool, err error) {
	if equal(src, dst) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if runtime.GOOS == "windows" {
		old := dst + ".old"
		os.Remove(old)
		if err := os.Rename(dst, old); err != nil && !os.IsNotExist(err) {
			os.Remove(tmp)
			return false, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// MenuEntry adds ClaudeSync to the Linux app menu (Windows needs nothing:
// running the download again opens the installed copy).
func MenuEntry(exe string, iconPNG []byte) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		data = filepath.Join(home, ".local", "share")
	}
	icon := filepath.Join(data, "icons", "hicolor", "128x128", "apps", "claudesync.png")
	if err := os.MkdirAll(filepath.Dir(icon), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(icon, iconPNG, 0o644); err != nil {
		return err
	}
	entry := filepath.Join(data, "applications", "claudesync.desktop")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		return err
	}
	return os.WriteFile(entry, []byte(fmt.Sprintf("[Desktop Entry]\nType=Application\nName=ClaudeSync\nComment=Sync Claude sessions between your PCs\nExec=%q\nIcon=claudesync\nCategories=Utility;\nTerminal=false\n", exe)), 0o644)
}

// Cleanup removes the copy a Windows update moved aside, once it's no
// longer running.
func Cleanup(dst string) { os.Remove(dst + ".old") }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func equal(a, b string) bool {
	ha, err := hash(a)
	if err != nil {
		return false
	}
	hb, err := hash(b)
	return err == nil && bytes.Equal(ha, hb)
}

func hash(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
