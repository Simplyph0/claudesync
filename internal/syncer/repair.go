package syncer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ClaudeSync 0.1 wrote paths from Linux PCs into Windows JSON files without
// escaping the backslashes, which leaves files Claude can't read. Repair
// finds such files and fixes exactly those paths. Each file is saved in the
// snapshot before it is changed, and a file is only rewritten if the result
// is valid JSON.

// Repair checks desktop records, and transcripts changed since `since`, and
// fixes the ones broken by unescaped local paths. roots are this PC's
// folders that may appear in them (project folders, home, Claude folders).
func (s *Scanner) Repair(roots []string, since time.Time, snap *Snapshot) (fixed []string, err error) {
	if len(roots) == 0 {
		return nil, nil
	}
	// Longest first, so a project inside the home folder is fixed as a whole.
	var cands []string
	seen := map[string]bool{}
	for _, r := range roots {
		if strings.Contains(r, `\`) && !seen[r] {
			seen[r] = true
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return nil, nil // paths without backslashes can't cause this
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i]) > len(cands[j]) })

	var files []string
	if dir := s.Loc.SessionsDir(); dir != "" {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
				files = append(files, p)
			}
			return nil
		})
	}
	_ = filepath.WalkDir(s.Loc.ProjectsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".jsonl") || strings.HasSuffix(p, ".json")) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(since) {
			files = append(files, p)
		}
		return nil
	})

	var errs []error
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil || validJSONFile(p, data) {
			continue
		}
		out := string(data)
		for _, r := range cands {
			out = replaceBounded(out, r, jsonEscape(r))
		}
		if out == string(data) || !validJSONFile(p, []byte(out)) {
			continue // not something this repair understands; leave it alone
		}
		if err := snap.AddFile(p); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := writeAtomic(p, []byte(out)); err != nil {
			errs = append(errs, err)
			continue
		}
		fixed = append(fixed, p)
	}
	return fixed, errors.Join(errs...)
}

func validJSONFile(path string, data []byte) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return json.Valid(data)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 && !json.Valid(line) {
			return false
		}
	}
	return sc.Err() == nil
}
