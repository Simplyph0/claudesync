package projsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// FolderInfo describes a folder before it's added, so the choice is informed.
type FolderInfo struct {
	Files     int
	Bytes     int64
	Skipped   []Skip   // left out, and why
	Ignored   []string // things skipped on purpose (node_modules/, build/ …)
	Sensitive []string // files that look like secrets
	Git       *GitInfo
	HasRemote bool     // a git remote is set, so git can carry it too
	Warnings  []string // things worth knowing, in plain words
	Truncated bool     // stopped counting early: it's a big folder
}

const (
	inspectMaxFiles = 200000
	inspectMaxTime  = 8 * time.Second
)

var secretNames = []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa", "id_ed25519", "id_ecdsa", "*.kdbx"}

// Inspect looks at a folder without reading file contents.
func Inspect(folder string, maxFileMB int, extraIgnore []string) (FolderInfo, error) {
	var fi FolderInfo
	info, err := os.Stat(folder)
	if err != nil {
		return fi, err
	}
	if !info.IsDir() {
		return fi, fmt.Errorf("%s is not a folder", folder)
	}
	fi.Git = readGit(folder)
	fi.HasRemote = HasGitRemote(folder)
	git := gitDir(folder) != ""
	m := NewMatcher()
	m.LoadDir(folder, "", git)
	m.AddLines(extraIgnore)
	secrets := &Matcher{}
	secrets.AddLines(secretNames)

	start := time.Now()
	maxFile := int64(maxFileMB) << 20
	ignored := map[string]bool{}
	winBad := 0
	err = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == folder {
				return err
			}
			rel, _ := filepath.Rel(folder, p)
			fi.Skipped = append(fi.Skipped, Skip{filepath.ToSlash(rel), "can't be read: " + shortErr(err)})
			return nil
		}
		if p == folder {
			return nil
		}
		if fi.Files >= inspectMaxFiles || time.Since(start) > inspectMaxTime {
			fi.Truncated = true
			return fs.SkipAll
		}
		relOS, _ := filepath.Rel(folder, p)
		rel := filepath.ToSlash(relOS)
		if m.Ignored(rel, d.IsDir()) {
			if d.IsDir() {
				if len(ignored) < 8 {
					ignored[path.Base(rel)+"/"] = true
				}
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			m.LoadDir(folder, rel, git)
			return nil
		}
		st, err := d.Info()
		if err != nil || !st.Mode().IsRegular() {
			return nil
		}
		if maxFile > 0 && st.Size() > maxFile {
			fi.Skipped = append(fi.Skipped, Skip{rel, fmt.Sprintf("%d MB, over the per-file limit", st.Size()>>20)})
			return nil
		}
		fi.Files++
		fi.Bytes += st.Size()
		if runtime.GOOS != "windows" && windowsBadName(rel) {
			winBad++
		}
		if len(fi.Sensitive) < 5 && secrets.Ignored(rel, false) {
			fi.Sensitive = append(fi.Sensitive, rel)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return fi, err
	}
	for n := range ignored {
		fi.Ignored = append(fi.Ignored, n)
	}
	sort.Strings(fi.Ignored)
	if fi.Truncated {
		fi.Warnings = append(fi.Warnings, "This folder is very large. The numbers above are only what was counted before stopping.")
	}
	if len(fi.Sensitive) > 0 {
		fi.Warnings = append(fi.Warnings, "It has files that look like passwords or keys ("+strings.Join(fi.Sensitive, ", ")+"). They're encrypted before leaving this PC, but Sync copies them to your other PCs too. Add them to \"Don't sync these\" if you don't want that.")
	}
	if winBad > 0 {
		fi.Warnings = append(fi.Warnings, fmt.Sprintf("%d file name(s) can't exist on Windows (characters like : or ? in the name). Windows PCs will skip them.", winBad))
	}
	if len(fi.Skipped) > 0 {
		fi.Warnings = append(fi.Warnings, fmt.Sprintf("%d file(s) will be left out (too big or unreadable).", len(fi.Skipped)))
	}
	return fi, nil
}

func shortErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// samePath reports whether two paths name the same place.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether child is inside parent (and not the same place).
func within(child, parent string) bool {
	if samePath(child, parent) {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateLocked checks a new or changed project folder against the others.
// The caller holds m.mu.
func (m *Manager) validateLocked(p Project) error {
	folder := filepath.Clean(p.Folder)
	if !filepath.IsAbs(folder) {
		return fmt.Errorf("%q isn't a full folder path. Choose the folder again.", p.Folder)
	}
	if filepath.Dir(folder) == folder {
		return errors.New("A whole drive can't be a project. Choose a folder inside it.")
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(folder, home) {
		return errors.New("Your whole home folder can't be a project. Choose a folder inside it.")
	}
	if m.Dir != "" && (samePath(folder, m.Dir) || within(folder, m.Dir) || within(m.Dir, folder)) {
		return errors.New("That folder contains ClaudeSync's own data. Choose a different one.")
	}
	for k, o := range m.projects {
		if k == p.Key {
			continue
		}
		switch {
		case samePath(folder, o.Folder):
			return fmt.Errorf("That folder is already used by the project %q.", o.Name)
		case within(folder, o.Folder):
			return fmt.Errorf("That folder is inside the project %q (%s). Syncing both would copy the same files twice.", o.Name, o.Folder)
		case within(o.Folder, folder):
			return fmt.Errorf("That folder contains the project %q (%s). Syncing both would copy the same files twice.", o.Name, o.Folder)
		}
	}
	return nil
}

// visible drops the files this PC was told not to sync (the project's own
// "don't sync these" list) from a server or base listing, so they're neither
// downloaded nor mistaken for deletions.
func (p Project) visible(files map[string]*Entry) map[string]*Entry {
	if len(p.Ignore) == 0 || len(files) == 0 {
		return files
	}
	m := NewMatcher()
	m.AddLines(p.Ignore)
	out := make(map[string]*Entry, len(files))
	for k, e := range files {
		if !ignoredPath(m, k, e.Dir) {
			out[k] = e
		}
	}
	return out
}

// ignoredPath applies the rules to a path and every folder above it.
func ignoredPath(m *Matcher, rel string, dir bool) bool {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if m.Ignored(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return m.Ignored(rel, dir)
}
