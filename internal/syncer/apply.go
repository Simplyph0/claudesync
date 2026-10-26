package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/google/uuid"
)

// Target is where an item lands on this PC.
type Target struct {
	Project string // local project folder ("" for folderless sessions and config)
	Scratch string // local working folder of a folderless session
	Cwd     string // local working folder of the session
	roots   []Root
}

var ErrUnmapped = errors.New("project folder not found on this PC")

// Resolve works out local paths for an item. project is the folder the
// project maps to here ("" if unknown).
func (s *Scanner) Resolve(m Manifest, project string) (Target, error) {
	var t Target
	if m.Project != nil {
		if project == "" {
			return t, ErrUnmapped
		}
		t.Project = project
		t.roots = append(t.roots, Root{"PROJECT", project}, Root{"CWD", project})
	}
	if m.ScratchRel != "" {
		root := s.Loc.ScratchRoot()
		if root == "" {
			root = filepath.Join(s.Loc.Home, ".claudesync", "scratch-workspaces")
		}
		t.Scratch = filepath.Join(root, filepath.FromSlash(m.ScratchRel))
		t.roots = append(t.roots, Root{"SCRATCH", t.Scratch})
	}
	desktop := s.Loc.DesktopDir
	if desktop == "" {
		desktop = filepath.Join(s.Loc.Home, ".claudesync", "no-desktop-app")
	}
	t.roots = append(t.roots, Root{"DESKTOP", desktop}, Root{"CLAUDE", s.Loc.ClaudeHome}, Root{"HOME", s.Loc.Home})
	if m.Cwd != "" {
		t.Cwd = filepath.Clean(Localize(m.Cwd, t.roots))
	}
	return t, nil
}

// Write puts an item's files in place on this PC. Desktop records are only
// written when this PC has the desktop app.
func (s *Scanner) Write(it *Item, t Target) error {
	var errs []error
	for _, f := range it.Files {
		dst, err := s.destination(it, t, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if dst == "" {
			continue
		}
		data := f.Data
		if isText(f.Role, data) {
			data = []byte(Localize(string(data), t.roots))
		}
		if err := writeAtomic(dst, data); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Scanner) destination(it *Item, t Target, f File) (string, error) {
	if !safeRel(f.Rel) {
		return "", fmt.Errorf("unsafe path %q", f.Rel)
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(f.Rel, `<>:"|?*`) {
		return "", nil // can't exist on Windows; skip quietly
	}
	rel := filepath.FromSlash(f.Rel)
	switch f.Role {
	case RoleDesktop:
		if s.Loc.SessionsDir() == "" {
			return "", nil
		}
		return filepath.Join(s.Loc.SessionsDir(), s.accountRel(rel)), nil
	case RoleTranscript:
		if t.Cwd == "" {
			return "", errors.New("session has no working folder")
		}
		return filepath.Join(s.Loc.ProjectsDir(), claude.EncodeProjectDir(t.Cwd), rel), nil
	case RoleScratch:
		if t.Scratch == "" {
			return "", nil
		}
		return filepath.Join(t.Scratch, rel), nil
	case RoleClaude:
		return filepath.Join(s.Loc.ClaudeHome, rel), nil
	case RoleMemory:
		if t.Project == "" {
			return "", ErrUnmapped
		}
		return filepath.Join(s.Loc.ProjectsDir(), claude.EncodeProjectDir(t.Project), "memory", rel), nil
	}
	return "", fmt.Errorf("unknown file role %q", f.Role)
}

// accountRel keeps <account>/<org>/file, but if this PC's app uses a single,
// different account folder, the record goes there instead.
func (s *Scanner) accountRel(rel string) string {
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 {
		return rel
	}
	if _, err := os.Stat(filepath.Join(s.Loc.SessionsDir(), parts[0], parts[1])); err == nil {
		return rel
	}
	accts, _ := filepath.Glob(filepath.Join(s.Loc.SessionsDir(), "*", "*"))
	var dirs []string
	for _, a := range accts {
		if info, err := os.Stat(a); err == nil && info.IsDir() {
			dirs = append(dirs, a)
		}
	}
	if len(dirs) == 1 {
		r, _ := filepath.Rel(s.Loc.SessionsDir(), dirs[0])
		return filepath.Join(r, parts[2])
	}
	return rel
}

func writeAtomic(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := dst + ".claudesync-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// AsCopy turns a session from another PC into a separate session with new
// ids and "(from <PC>)" in its title, so a conflicting local copy is kept.
func AsCopy(it *Item, fromDevice string) (*Item, error) {
	if it.M.Kind != "session" || it.M.CliID == "" {
		return nil, errors.New("only sessions can be copied")
	}
	newCli := uuid.NewString()
	newDesk := "local_" + uuid.NewString()
	oldCli, oldDesk := it.M.CliID, it.M.DesktopID
	swap := func(s string) string {
		s = strings.ReplaceAll(s, oldCli, newCli)
		if oldDesk != "" {
			s = strings.ReplaceAll(s, oldDesk, newDesk)
		}
		return s
	}
	cp := &Item{M: it.M}
	cp.M.CliID = newCli
	if oldDesk != "" {
		cp.M.DesktopID = newDesk
		cp.M.ID = newDesk
	} else {
		cp.M.ID = "cli_" + newCli
	}
	cp.M.Title = it.M.Title + " (from " + fromDevice + ")"
	for _, f := range it.Files {
		nf := File{Role: f.Role, Rel: swap(f.Rel), Data: f.Data}
		if isText(f.Role, f.Data) {
			nf.Data = []byte(swap(string(f.Data)))
		}
		if f.Role == RoleDesktop {
			nf.Data = retitle(nf.Data, cp.M.Title)
		}
		cp.Files = append(cp.Files, nf)
	}
	return cp, nil
}

func retitle(raw []byte, title string) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return raw
	}
	m["title"] = title
	m["titleSource"] = "user"
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
