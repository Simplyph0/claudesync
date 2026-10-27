package syncer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/Simplyph0/claudesync/internal/claude"
	"github.com/Simplyph0/claudesync/internal/proto"
)

// Options choose what gets synced.
type Options struct {
	Sessions        bool // desktop app sessions (record + transcript)
	CLIOnly         bool // Claude Code transcripts with no desktop record
	Config          bool // settings, CLAUDE.md, commands, skills, agents
	Memory          bool // per-project memory folders
	Scratch         bool // working folders of folderless sessions
	ScratchMaxBytes int64
	DeviceName      string
}

// Local is an item found on this PC; build reads it fully.
type Local struct {
	Kind, ID string
	Title    string
	Stamp    string // cheap fingerprint of sizes and modification times
	build    func() (*Item, error)
}

func (l *Local) Key() string { return l.Kind + "/" + l.ID }

func (l *Local) Build() (*Item, error) { return l.build() }

// ScanResult is everything found in one pass.
type ScanResult struct {
	Items         map[string]*Local
	UnknownFormat int             // valid desktop records ClaudeSync doesn't understand (a format change)
	Corrupt       []string        // desktop records that aren't valid JSON
	Deleted       map[string]bool // desktop session ids the app marked deleted
	ProjectDirs   []string        // project folders used by local sessions
}

type Scanner struct {
	Loc claude.Locations
	Opt Options
	// AppRunning reports whether the desktop app is open (tests replace it).
	AppRunning func() bool
}

func (s *Scanner) appRunning() bool {
	if s.AppRunning != nil {
		return s.AppRunning()
	}
	return claude.DesktopRunning()
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

type desktopRecord struct {
	path      string
	rel       string // relative to the sessions dir, slash-separated
	raw       []byte
	id, cliID string
	cwd       string
	originCwd string
	anchor    string
	title     string
	known     bool
	corrupt   bool // not valid JSON
}

func readDesktopRecord(path, rel string) (*desktopRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &desktopRecord{path: path, rel: rel, raw: raw}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		r.corrupt = true
		return r, nil
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	r.id, r.cliID, r.cwd = str("sessionId"), str("cliSessionId"), str("cwd")
	r.originCwd, r.anchor, r.title = str("originCwd"), str("gitAnchorsFolderRealpath"), str("title")
	r.known = strings.HasPrefix(r.id, "local_") && proto.ValidID(r.id) && uuidRe.MatchString(r.cliID) && r.cwd != ""
	return r, nil
}

// transcriptIndex maps a CLI session id to the projects/<enc> folder holding it.
func (s *Scanner) transcriptIndex() map[string]string {
	idx := map[string]string{}
	dirs, _ := os.ReadDir(s.Loc.ProjectsDir())
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(s.Loc.ProjectsDir(), d.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if id, ok := strings.CutSuffix(f.Name(), ".jsonl"); ok && uuidRe.MatchString(id) {
				idx[id] = dir
			}
		}
	}
	return idx
}

// Scan finds every syncable item on this PC.
func (s *Scanner) Scan() (*ScanResult, error) {
	res := &ScanResult{Items: map[string]*Local{}, Deleted: map[string]bool{}}
	tidx := s.transcriptIndex()
	usedCli := map[string]bool{}
	projectOfDir := map[string]string{} // projects/<enc> folder -> project root

	if s.Opt.Sessions && s.Loc.SessionsDir() != "" {
		recs, deleted := s.desktopRecords()
		res.Deleted = deleted
		for _, r := range recs {
			if !r.known {
				if r.corrupt {
					// never upload an unreadable file over the good copy on the server
					res.Corrupt = append(res.Corrupt, r.path)
					continue
				}
				res.UnknownFormat++
				l := s.unknownRecord(r)
				res.Items[l.Key()] = l
				continue
			}
			usedCli[r.cliID] = true
			tdir := tidx[r.cliID]
			if tdir == "" {
				tdir = filepath.Join(s.Loc.ProjectsDir(), claude.EncodeProjectDir(r.cwd))
			}
			proj := r.originCwd
			if proj == "" {
				proj = r.cwd
			}
			if claude.ScratchRel(r.cwd) == "" {
				res.ProjectDirs = append(res.ProjectDirs, proj)
				projectOfDir[tdir] = proj
			}
			l := s.sessionLocal(r, r.cliID, tdir, r.cwd)
			res.Items[l.Key()] = l
		}
	}
	if s.Opt.CLIOnly {
		for cli, tdir := range tidx {
			if usedCli[cli] {
				continue
			}
			cwd, title := transcriptInfo(filepath.Join(tdir, cli+".jsonl"))
			if cwd == "" {
				continue
			}
			if claude.ScratchRel(cwd) == "" {
				projectOfDir[tdir] = cwd
			}
			r := &desktopRecord{cliID: cli, cwd: cwd, title: title}
			l := s.sessionLocal(r, cli, tdir, cwd)
			res.Items[l.Key()] = l
		}
	}
	if s.Opt.Memory {
		// a project's memory folder belongs to whichever project its transcripts point at
		for _, tdir := range tidx {
			if _, ok := projectOfDir[tdir]; !ok {
				if cwd, _ := transcriptInfoInDir(tdir); cwd != "" && claude.ScratchRel(cwd) == "" {
					projectOfDir[tdir] = cwd
				}
			}
		}
		for tdir, proj := range projectOfDir {
			if l := s.memoryLocal(tdir, proj); l != nil {
				res.Items[l.Key()] = l
			}
		}
	}
	if s.Opt.Config {
		for _, l := range s.configLocals() {
			res.Items[l.Key()] = l
		}
	}
	return res, nil
}

func (s *Scanner) desktopRecords() ([]*desktopRecord, map[string]bool) {
	var out []*desktopRecord
	deleted := map[string]bool{}
	root := s.Loc.SessionsDir()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if id, ok := strings.CutPrefix(name, "deleted_"); ok {
			deleted["local_"+id] = true
			return nil
		}
		if !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if r, err := readDesktopRecord(p, filepath.ToSlash(rel)); err == nil {
			out = append(out, r)
		}
		return nil
	})
	return out, deleted
}

// sessionRoots lists the machine-specific folders inside a session's files.
func (s *Scanner) sessionRoots(cwd, originCwd, anchor string) []Root {
	var roots []Root
	if claude.ScratchRel(cwd) != "" {
		for _, p := range []string{cwd, originCwd, anchor} {
			if p != "" && claude.ScratchRel(p) != "" {
				roots = append(roots, Root{"SCRATCH", p})
			}
		}
	} else {
		proj := originCwd
		if proj == "" {
			proj = cwd
		}
		roots = append(roots, Root{"PROJECT", proj})
		if anchor != "" && anchor != proj && claude.ScratchRel(anchor) == "" {
			roots = append(roots, Root{"PROJECT", anchor})
		}
		if cwd != proj && !isUnder(cwd, proj) {
			roots = append(roots, Root{"CWD", cwd})
		}
	}
	return append(roots, s.commonRoots()...)
}

func (s *Scanner) commonRoots() []Root {
	var roots []Root
	if s.Loc.DesktopDir != "" {
		roots = append(roots, Root{"DESKTOP", s.Loc.DesktopDir})
	}
	for _, a := range s.Loc.DesktopAlt {
		roots = append(roots, Root{"DESKTOP", a})
	}
	return append(roots, Root{"CLAUDE", s.Loc.ClaudeHome}, Root{"HOME", s.Loc.Home})
}

func isUnder(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *Scanner) sessionLocal(r *desktopRecord, cli, tdir, cwd string) *Local {
	id := r.id
	if id == "" {
		id = "cli_" + cli
	}
	scratchRel := claude.ScratchRel(cwd)
	var stampPaths []string
	if r.path != "" {
		stampPaths = append(stampPaths, r.path)
	}
	stampPaths = append(stampPaths, filepath.Join(tdir, cli+".jsonl"), filepath.Join(tdir, cli))
	if scratchRel != "" {
		stampPaths = append(stampPaths, filepath.Join(tdir, "memory"))
		if s.Opt.Scratch {
			stampPaths = append(stampPaths, cwd)
		}
	}
	title := r.title
	if title == "" {
		title = "Session " + cli[:8]
	}
	return &Local{Kind: proto.KindSession, ID: id, Title: title, Stamp: stamp(stampPaths...), build: func() (*Item, error) {
		roots := s.sessionRoots(cwd, r.originCwd, r.anchor)
		m := Manifest{Format: FormatVersion, Kind: proto.KindSession, ID: id, Title: title, Device: s.Opt.DeviceName,
			Platform: runtime.GOOS + "/" + runtime.GOARCH, Cwd: Portable(cwd, roots), CliID: cli, DesktopID: r.id}
		if scratchRel != "" {
			m.ScratchRel = filepath.ToSlash(scratchRel)
		} else {
			proj := r.originCwd
			if proj == "" {
				proj = cwd
			}
			ref := DescribeProject(proj)
			m.Project = &ref
		}
		it := &Item{M: m}
		if r.path != "" {
			it.Files = append(it.Files, File{RoleDesktop, r.rel, []byte(Portable(string(r.raw), roots))})
		}
		addText := func(role, rel string, data []byte) {
			if isText(role, data) {
				data = []byte(Portable(string(data), roots))
			}
			it.Files = append(it.Files, File{role, rel, data})
		}
		tb, err := os.ReadFile(filepath.Join(tdir, cli+".jsonl"))
		if err == nil {
			addText(RoleTranscript, cli+".jsonl", tb)
		} else if r.path == "" {
			return nil, err
		}
		if err := walkFiles(filepath.Join(tdir, cli), cli, 0, func(rel string, b []byte) { addText(RoleTranscript, rel, b) }); err != nil {
			return nil, err
		}
		if scratchRel != "" {
			if err := walkFiles(filepath.Join(tdir, "memory"), "memory", 0, func(rel string, b []byte) { addText(RoleTranscript, rel, b) }); err != nil {
				return nil, err
			}
			if s.Opt.Scratch {
				if err := walkFiles(cwd, "", s.Opt.ScratchMaxBytes, func(rel string, b []byte) {
					it.Files = append(it.Files, File{RoleScratch, rel, b})
				}); err != nil {
					return nil, fmt.Errorf("working folder: %w", err)
				}
			}
		}
		return it, nil
	}}
}

// unknownRecord backs up a desktop record in a format ClaudeSync doesn't
// know, untouched, under its own id so it never replaces the readable
// version of that session on the server.
func (s *Scanner) unknownRecord(r *desktopRecord) *Local {
	base := strings.TrimSuffix(filepath.Base(r.path), ".json")
	sum := sha256.Sum256([]byte(r.rel))
	id := "raw_" + hex.EncodeToString(sum[:12])
	return &Local{Kind: proto.KindSession, ID: id, Title: base, Stamp: stamp(r.path), build: func() (*Item, error) {
		return &Item{M: Manifest{Format: FormatVersion, Kind: proto.KindSession, ID: id, Title: base, Device: s.Opt.DeviceName, UnknownFormat: true},
			Files: []File{{RoleDesktop, r.rel, r.raw}}}, nil
	}}
}

func (s *Scanner) memoryLocal(tdir, proj string) *Local {
	mem := filepath.Join(tdir, "memory")
	hasFiles := false
	_ = filepath.WalkDir(mem, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			hasFiles = true
			return filepath.SkipAll
		}
		return nil
	})
	if !hasFiles {
		return nil
	}
	ref := DescribeProject(proj)
	sum := sha256.Sum256([]byte(ref.Key))
	id := "mem_" + hex.EncodeToString(sum[:12])
	return &Local{Kind: proto.KindMemory, ID: id, Title: "Memory: " + ref.Name, Stamp: stamp(mem), build: func() (*Item, error) {
		roots := append([]Root{{"PROJECT", proj}}, s.commonRoots()...)
		it := &Item{M: Manifest{Format: FormatVersion, Kind: proto.KindMemory, ID: id, Title: "Memory: " + ref.Name, Device: s.Opt.DeviceName, Project: &ref}}
		err := walkFiles(mem, "", 0, func(rel string, b []byte) {
			if isText(RoleMemory, b) {
				b = []byte(Portable(string(b), roots))
			}
			it.Files = append(it.Files, File{RoleMemory, rel, b})
		})
		return it, err
	}}
}

// Config entries synced from ~/.claude. Each file, or each child of a
// folder, is its own item. Credentials and settings.local.json are excluded.
var (
	configFiles = []string{"settings.json", "CLAUDE.md", "keybindings.json"}
	configDirs  = []string{"commands", "skills", "agents", "output-styles"}
)

func (s *Scanner) configLocals() []*Local {
	var rels []string
	for _, f := range configFiles {
		if info, err := os.Stat(filepath.Join(s.Loc.ClaudeHome, f)); err == nil && !info.IsDir() {
			rels = append(rels, f)
		}
	}
	for _, d := range configDirs {
		entries, _ := os.ReadDir(filepath.Join(s.Loc.ClaudeHome, d))
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				rels = append(rels, d+"/"+e.Name())
			}
		}
	}
	var out []*Local
	for _, rel := range rels {
		rel := rel
		sum := sha256.Sum256([]byte(rel))
		id := "cfg_" + hex.EncodeToString(sum[:12])
		full := filepath.Join(s.Loc.ClaudeHome, filepath.FromSlash(rel))
		out = append(out, &Local{Kind: proto.KindConfig, ID: id, Title: rel, Stamp: stamp(full), build: func() (*Item, error) {
			roots := s.commonRoots()
			it := &Item{M: Manifest{Format: FormatVersion, Kind: proto.KindConfig, ID: id, Title: rel, Device: s.Opt.DeviceName}}
			add := func(r string, b []byte) {
				if isText(RoleClaude, b) {
					b = []byte(Portable(string(b), roots))
				}
				it.Files = append(it.Files, File{RoleClaude, r, b})
			}
			info, err := os.Stat(full)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				b, err := os.ReadFile(full)
				if err != nil {
					return nil, err
				}
				add(rel, b)
				return it, nil
			}
			return it, walkFiles(full, rel, 0, add)
		}})
	}
	return out
}

// walkFiles calls fn for every regular file under dir with its path
// relative to dir, prefixed by prefix. A non-zero limit caps the total size.
func walkFiles(dir, prefix string, limit int64, fn func(rel string, data []byte)) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	type f struct {
		path, rel string
	}
	var files []f
	var total int64
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if i, err := d.Info(); err == nil {
			total += i.Size()
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if prefix != "" {
			rel = prefix + "/" + rel
		}
		files = append(files, f{p, rel})
		return nil
	})
	if err != nil {
		return err
	}
	if limit > 0 && total > limit {
		return fmt.Errorf("%s is %d MB, over the %d MB limit; skipped", dir, total>>20, limit>>20)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	for _, x := range files {
		b, err := os.ReadFile(x.path)
		if err != nil {
			continue // file vanished or is locked; next backup gets it
		}
		fn(x.rel, b)
	}
	return nil
}

// stamp fingerprints files and folders by name, size and modification time.
func stamp(paths ...string) string {
	h := sha256.New()
	for _, root := range paths {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if info, err := d.Info(); err == nil && !d.IsDir() {
				fmt.Fprintf(h, "%s|%d|%d\n", p, info.Size(), info.ModTime().UnixNano())
			}
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// transcriptInfo reads the working folder and custom title from a transcript.
func transcriptInfo(path string) (cwd, title string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 0; sc.Scan() && n < 2000; n++ {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"cwd"`)) && !bytes.Contains(line, []byte(`"customTitle"`)) {
			continue
		}
		var v struct {
			Cwd         string `json:"cwd"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(line, &v) == nil {
			if cwd == "" && v.Cwd != "" {
				cwd = v.Cwd
			}
			if v.CustomTitle != "" {
				title = v.CustomTitle
			}
		}
	}
	return cwd, title
}

func transcriptInfoInDir(dir string) (string, string) {
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".jsonl") {
			if cwd, t := transcriptInfo(filepath.Join(dir, f.Name())); cwd != "" {
				return cwd, t
			}
		}
	}
	return "", ""
}
