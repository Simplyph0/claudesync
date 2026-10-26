package syncer

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// gitRemote returns the origin URL of the repository at dir, if any.
func gitRemote(dir string) string {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	cfgPath := filepath.Join(gitPath, "config")
	if !info.IsDir() { // worktree or submodule: ".git" is a file pointing elsewhere
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		gd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(gd) {
			gd = filepath.Join(dir, gd)
		}
		cfgPath = filepath.Join(gd, "config")
		if c, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
			cfgPath = filepath.Join(gd, strings.TrimSpace(string(c)), "config")
		}
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	inOrigin, first := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
			v = strings.TrimSpace(v)
			if inOrigin {
				return v
			}
			if first == "" {
				first = v
			}
		}
	}
	return first
}

// normRemote makes https://github.com/a/b.git, git@github.com:a/b and
// ssh://git@github.com/a/b compare equal.
func normRemote(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.Replace(u, ":", "/", 1)
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return u
}

// DescribeProject identifies a project folder in a PC-independent way.
func DescribeProject(dir string) ProjectRef {
	name := filepath.Base(filepath.Clean(dir))
	ref := ProjectRef{Name: name, GitRemote: gitRemote(dir)}
	if ref.GitRemote != "" {
		ref.Key = "git:" + normRemote(ref.GitRemote)
	} else {
		ref.Key = "name:" + strings.ToLower(name)
	}
	return ref
}

// ProjectIndex finds local folders for projects from other PCs.
type ProjectIndex struct {
	roots []string
	known []string // folders sessions on this PC already use

	once  sync.Once
	byKey map[string][]string
	byNam map[string][]string
}

func NewProjectIndex(searchRoots, known []string) *ProjectIndex {
	return &ProjectIndex{roots: searchRoots, known: known}
}

var skipDirs = map[string]bool{"node_modules": true, "AppData": true, "Library": true, "venv": true, ".venv": true, "__pycache__": true, "target": true, "build": true, "dist": true}

func (pi *ProjectIndex) build() {
	pi.byKey, pi.byNam = map[string][]string{}, map[string][]string{}
	seen := map[string]bool{}
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if seen[strings.ToLower(dir)] {
			return
		}
		seen[strings.ToLower(dir)] = true
		ref := DescribeProject(dir)
		pi.byKey[ref.Key] = append(pi.byKey[ref.Key], dir)
		n := strings.ToLower(ref.Name)
		pi.byNam[n] = append(pi.byNam[n], dir)
	}
	for _, k := range pi.known {
		if info, err := os.Stat(k); err == nil && info.IsDir() {
			add(k)
		}
	}
	for _, root := range pi.roots {
		walkDepth(root, 3, add)
	}
}

func walkDepth(dir string, depth int, fn func(string)) {
	if depth == 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skipDirs[e.Name()] {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fn(p)
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			continue // don't descend into repositories
		}
		walkDepth(p, depth-1, fn)
	}
}

// Find returns the single local folder matching ref: same git remote first,
// then same folder name. Ambiguous or missing matches return "".
func (pi *ProjectIndex) Find(ref ProjectRef) string {
	pi.once.Do(pi.build)
	if strings.HasPrefix(ref.Key, "git:") {
		if c := pi.byKey[ref.Key]; len(c) == 1 {
			return c[0]
		} else if len(c) > 1 {
			return ""
		}
	}
	var c []string
	for _, d := range pi.byNam[strings.ToLower(ref.Name)] {
		// a same-named folder that is a different repository is not a match
		if r := gitRemote(d); r != "" && ref.GitRemote != "" && normRemote(r) != normRemote(ref.GitRemote) {
			continue
		}
		c = append(c, d)
	}
	if len(c) == 1 {
		return c[0]
	}
	return ""
}
