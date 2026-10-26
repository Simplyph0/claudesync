package syncer

import (
	"sort"
	"strings"

	"github.com/Simplyph0/claudesync/internal/claude"
)

// A Root is a machine-specific folder that is swapped for a placeholder
// ({{NAME}}, {{NAME_J}}, {{NAME_F}}, {{NAME_E}}) before upload and expanded
// to this PC's equivalent on restore.
type Root struct {
	Name string
	Path string
}

// Each path appears in Claude's files in up to four spellings.
var variantSuffixes = []string{"", "_J", "_F", "_E"}

func variant(p, suffix string) string {
	switch suffix {
	case "_J": // inside JSON strings, backslashes are escaped
		return strings.ReplaceAll(p, `\`, `\\`)
	case "_F":
		return strings.ReplaceAll(p, `\`, `/`)
	case "_E": // ~/.claude/projects folder name
		return claude.EncodeProjectDir(p)
	}
	return p
}

func placeholder(name, suffix string) string { return "{{" + name + suffix + "}}" }

type pair struct{ from, to string }

// Portable replaces this PC's paths with placeholders.
func Portable(text string, roots []Root) string {
	var pairs []pair
	seen := map[string]bool{}
	for _, r := range roots {
		if len(r.Path) < 3 {
			continue
		}
		for _, s := range variantSuffixes {
			v := variant(r.Path, s)
			if !seen[v] {
				seen[v] = true
				pairs = append(pairs, pair{v, placeholder(r.Name, s)})
			}
		}
	}
	// Longest first so a project inside the home folder wins over the home folder.
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].from) > len(pairs[j].from) })
	for _, p := range pairs {
		text = replaceBounded(text, p.from, p.to)
	}
	return text
}

// replaceBounded replaces from with to only where the match isn't followed
// by a letter or digit, so /home/al doesn't match /home/alice.
func replaceBounded(text, from, to string) string {
	if !strings.Contains(text, from) {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, from)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(from)
		if end < len(text) && isAlnum(text[end]) {
			b.WriteString(text[:end])
		} else {
			b.WriteString(text[:i])
			b.WriteString(to)
		}
		text = text[end:]
	}
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// Localize expands placeholders using this PC's paths. Roots with the same
// name keep the first one.
func Localize(text string, roots []Root) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	var args []string
	done := map[string]bool{}
	for _, r := range roots {
		if done[r.Name] {
			continue
		}
		done[r.Name] = true
		for _, s := range variantSuffixes {
			args = append(args, placeholder(r.Name, s), variant(r.Path, s))
		}
	}
	return strings.NewReplacer(args...).Replace(text)
}
