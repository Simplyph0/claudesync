package projsync

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Always skipped, whatever the project's own ignore files say.
var defaultIgnores = []string{
	".git/", "node_modules/", ".venv/", "venv/", "__pycache__/", "build/", "dist/", "target/",
	".pio/", ".gradle/", ".next/", "bin/Debug/", "bin/Release/", "obj/",
	".DS_Store", "Thumbs.db", "desktop.ini", "*.swp", "~$*", "*.claudesync-tmp",
}

type rule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	base    string // folder the rule's file was in ("" = project root)
}

// Matcher applies gitignore-style rules: built-in skips, .gitignore (git
// projects) and .claudesyncignore.
type Matcher struct {
	rules []rule
}

func NewMatcher() *Matcher {
	m := &Matcher{}
	for _, p := range defaultIgnores {
		m.add(p, "")
	}
	return m
}

// LoadDir adds the ignore files found in dir (relative path rel).
func (m *Matcher) LoadDir(root, rel string, git bool) {
	names := []string{".claudesyncignore"}
	if git {
		names = append([]string{".gitignore"}, names...)
	}
	for _, n := range names {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel), n))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			m.add(sc.Text(), rel)
		}
		f.Close()
	}
}

func (m *Matcher) add(line, base string) {
	line = strings.TrimRight(line, " \r")
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	r := rule{base: base}
	if strings.HasPrefix(line, "!") {
		r.negate, line = true, line[1:]
	}
	line = strings.TrimPrefix(line, `\`)
	if strings.HasSuffix(line, "/") {
		r.dirOnly, line = true, strings.TrimSuffix(line, "/")
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	re := globToRegexp(line)
	if anchored {
		re = "^" + re + "$"
	} else {
		re = "^(?:.*/)?" + re + "$"
	}
	compiled, err := regexp.Compile(re)
	if err != nil {
		return
	}
	r.re = compiled
	m.rules = append(m.rules, r)
}

func globToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "/**"):
			b.WriteString("(?:/.*)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			j := strings.IndexByte(g[i:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// Ignored reports whether rel (slash path from the project root) is skipped.
// The last matching rule wins, as in git.
func (m *Matcher) Ignored(rel string, dir bool) bool {
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !dir {
			continue
		}
		p := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			p = strings.TrimPrefix(rel, r.base+"/")
		}
		if r.re.MatchString(p) {
			ignored = !r.negate
		}
	}
	return ignored
}

// windowsBadName reports names Windows can't create.
func windowsBadName(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.ContainsAny(part, `<>:"|?*\`) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return true
		}
		base := strings.ToUpper(strings.TrimSuffix(part, path.Ext(part)))
		switch base {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "LPT1", "LPT2", "LPT3":
			return true
		}
	}
	return false
}
