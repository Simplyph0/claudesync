package projsync

import (
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// newestOf decides a clash by which version was edited last. Two edits less
// than tieWindow apart can't be ordered by file times (clocks differ a little
// between PCs), so they're ordered by content instead: both PCs compare the
// same two hashes, pick the same winner and stop clashing. Either way the
// loser is kept: in the project trash on the PC that replaces it, and in the
// server's History.
func newestOf(l, r *Entry) string {
	const tieWindow = 10 * time.Second
	switch d := l.Mtime.Sub(r.Mtime); {
	case d > tieWindow:
		return "mine"
	case d < -tieWindow:
		return "theirs"
	}
	if l.Hash > r.Hash {
		return "mine"
	}
	return "theirs"
}

// conflictCopy matches the " (from PC 2026-11-05 1430)" that Keep both adds
// to a file name.
var conflictCopy = regexp.MustCompile(` \(from [A-Za-z0-9_-]+ \d{4}-\d{2}-\d{2} \d{4}\)`)

// originalOf returns the name a "(from PC date)" copy was made from.
func originalOf(rel string) (string, bool) {
	dir, file := path.Split(rel)
	loc := conflictCopy.FindStringIndex(file)
	if loc == nil {
		return "", false
	}
	rest := file[loc[1]:]
	if rest != "" && !strings.HasPrefix(rest, ".") { // "name (from …) more" isn't one of ours
		return "", false
	}
	stem := file[:loc[0]]
	if stem == "" && rest == "" {
		return "", false
	}
	return dir + stem + rest, true
}

// findCopies lists copies left by Keep both whose original is still there.
func findCopies(files map[string]*Entry) []string {
	var out []string
	for rel, e := range files {
		if e.Dir {
			continue
		}
		if orig, ok := originalOf(rel); ok {
			if o := files[orig]; o != nil && !o.Dir {
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TidyCopies moves the leftover "(from PC date)" copies in a project's folder
// to the project trash. The originals stay. The next sync tells the other PCs,
// which trash their copies too. It returns how many it moved.
func (m *Manager) TidyCopies(key string) (int, error) {
	m.running.Lock() // not while a sync is running
	defer m.running.Unlock()
	m.mu.Lock()
	p := m.projects[key]
	var list []string
	if st := m.stateLocked(key); p != nil {
		list = append(list, st.Copies...)
	}
	m.mu.Unlock()
	if p == nil {
		return 0, errors.New("unknown project")
	}
	batch := m.trash().begin(p.Name, "leftover copies tidied up")
	w := &writer{m: m, root: p.Folder, batch: batch, failed: map[string]bool{}}
	n := 0
	var errs []error
	for _, rel := range list {
		orig, ok := originalOf(rel)
		if !ok {
			continue
		}
		if _, err := os.Stat(w.abs(orig)); err != nil { // the original is gone: this copy is all there is
			continue
		}
		if _, err := os.Lstat(w.abs(rel)); err != nil {
			continue
		}
		if err := w.remove(rel); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	m.mu.Lock()
	st := m.stateLocked(key)
	st.Copies = nil
	m.mu.Unlock()
	if n > 0 {
		m.note(key, "info", "Moved %d leftover copies to the trash (the originals stay). Your other PCs do the same at their next sync.", n)
	}
	if len(errs) > 0 {
		return n, fmt.Errorf("%d copies couldn't be moved: %w", len(errs), errors.Join(errs...))
	}
	return n, nil
}

// addCopies adds names to a sorted list without repeating any.
func addCopies(list, more []string) []string {
	seen := map[string]bool{}
	for _, c := range list {
		seen[c] = true
	}
	for _, c := range more {
		if !seen[c] {
			list = append(list, c)
			seen[c] = true
		}
	}
	sort.Strings(list)
	return list
}

// dropCopies forgets names that were removed from the folder.
func dropCopies(list, removed []string, failed map[string]bool) []string {
	gone := map[string]bool{}
	for _, r := range removed {
		if !failed[r] {
			gone[r] = true
		}
	}
	var out []string
	for _, c := range list {
		if !gone[c] {
			out = append(out, c)
		}
	}
	return out
}
