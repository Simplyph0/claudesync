// Package claude knows where Claude Code and the Claude desktop app keep
// their data, and how to detect, close and relaunch the desktop app.
package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Locations are the folders ClaudeSync reads and writes on this PC.
type Locations struct {
	Home       string   // user home
	ClaudeHome string   // ~/.claude (or $CLAUDE_CONFIG_DIR)
	DesktopDir string   // desktop app data folder containing claude-code-sessions; "" if none
	DesktopAlt []string // other paths that refer to the desktop data folder (Windows Store virtualization)
}

// SessionsDir is where the desktop app keeps its Code-tab session records.
func (l Locations) SessionsDir() string {
	if l.DesktopDir == "" {
		return ""
	}
	return filepath.Join(l.DesktopDir, "claude-code-sessions")
}

// ScratchRoot is where folderless sessions get their working folders.
func (l Locations) ScratchRoot() string {
	if l.DesktopDir == "" {
		return ""
	}
	return filepath.Join(l.DesktopDir, "scratch-workspaces")
}

// ProjectsDir holds Claude Code transcripts, one folder per project path.
func (l Locations) ProjectsDir() string { return filepath.Join(l.ClaudeHome, "projects") }

// Detect finds the folders, honouring non-empty overrides.
func Detect(claudeHomeOverride, desktopOverride string) Locations {
	home, _ := os.UserHomeDir()
	l := Locations{Home: home, ClaudeHome: claudeHomeOverride, DesktopDir: desktopOverride}
	if l.ClaudeHome == "" {
		if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
			l.ClaudeHome = env
		} else {
			l.ClaudeHome = filepath.Join(home, ".claude")
		}
	}
	cands := desktopCandidates(home)
	if l.DesktopDir == "" {
		l.DesktopDir = pickDesktopDir(cands)
	}
	for _, c := range cands {
		if c != l.DesktopDir && dirExists(c) {
			l.DesktopAlt = append(l.DesktopAlt, c)
		}
	}
	return l
}

func desktopCandidates(home string) []string {
	var out []string
	switch runtime.GOOS {
	case "windows":
		local, roaming := os.Getenv("LOCALAPPDATA"), os.Getenv("APPDATA")
		m, _ := filepath.Glob(filepath.Join(local, "Claude-Data", "claude_*"))
		out = append(out, m...)
		out = append(out, filepath.Join(roaming, "Claude"))
		m, _ = filepath.Glob(filepath.Join(local, "Packages", "Claude_*", "LocalCache", "Roaming", "Claude"))
		out = append(out, m...)
		out = append(out, filepath.Join(local, "AnthropicClaude"))
	case "darwin":
		out = append(out, filepath.Join(home, "Library", "Application Support", "Claude"))
	default:
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		out = append(out, filepath.Join(cfg, "Claude"), filepath.Join(cfg, "claude-desktop"), filepath.Join(cfg, "claude"))
	}
	return out
}

// pickDesktopDir prefers the candidate whose session folder changed most recently.
func pickDesktopDir(cands []string) string {
	type c struct {
		dir string
		mod time.Time
	}
	var found []c
	for _, d := range cands {
		if info, err := os.Stat(filepath.Join(d, "claude-code-sessions")); err == nil && info.IsDir() {
			found = append(found, c{d, latestMod(filepath.Join(d, "claude-code-sessions"))})
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	return found[0].dir
}

func latestMod(dir string) time.Time {
	var t time.Time
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil && info.ModTime().After(t) {
				t = info.ModTime()
			}
		}
		return nil
	})
	return t
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// EncodeProjectDir mirrors how Claude Code names a project's transcript
// folder: every character that isn't a letter or digit becomes '-'.
func EncodeProjectDir(path string) string {
	var b strings.Builder
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// ScratchRel returns the part of path after a "scratch-workspaces" folder,
// or "" if path isn't a folderless session's working folder.
func ScratchRel(path string) string {
	p := filepath.ToSlash(path)
	_, rest, ok := strings.Cut(p, "/scratch-workspaces/")
	if !ok || rest == "" {
		return ""
	}
	return filepath.FromSlash(rest)
}
