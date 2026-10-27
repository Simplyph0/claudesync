package projsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// settleTime: files changed more recently than this may still be being
// written, so they wait for the next sync.
const settleTime = 10 * time.Second

// cacheEntry lets a scan skip re-reading files that haven't changed.
type cacheEntry struct {
	Size   int64    `json:"s"`
	Mtime  int64    `json:"m"`
	Hash   string   `json:"h"`
	Chunks []string `json:"c"`
}

// Skip is a file left out of a scan, with the reason.
type Skip struct {
	Path, Why string
}

type scanResult struct {
	files     map[string]*Entry
	unsettled map[string]bool // changed in the last few seconds; treated as unchanged this time
	skipped   []Skip
	total     int64
}

type scanner struct {
	root    string
	keys    Keys
	maxFile int64
	cache   map[string]cacheEntry
	now     time.Time
	git     bool
}

func (s *scanner) scan() (*scanResult, error) {
	info, err := os.Stat(s.root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", s.root)
	}
	res := &scanResult{files: map[string]*Entry{}, unsettled: map[string]bool{}}
	m := NewMatcher()
	m.LoadDir(s.root, "", s.git)
	newCache := map[string]cacheEntry{}
	lowerSeen := map[string]string{}

	err = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == s.root {
				return err
			}
			res.skipped = append(res.skipped, Skip{p, err.Error()})
			return nil
		}
		if p == s.root {
			return nil
		}
		relOS, _ := filepath.Rel(s.root, p)
		rel := filepath.ToSlash(relOS)
		isDir := d.IsDir()
		if m.Ignored(rel, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if lk := strings.ToLower(rel); lowerSeen[lk] != "" && lowerSeen[lk] != rel {
			res.skipped = append(res.skipped, Skip{rel, "differs from " + lowerSeen[lk] + " only in upper/lower case"})
			if isDir {
				return filepath.SkipDir
			}
			return nil
		} else {
			lowerSeen[lk] = rel
		}
		if isDir {
			m.LoadDir(s.root, rel, s.git)
			if empty, _ := isEmptyDir(p); empty {
				res.files[rel] = &Entry{Dir: true}
			}
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			res.skipped = append(res.skipped, Skip{rel, err.Error()})
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			res.files[rel] = &Entry{Link: filepath.ToSlash(target), Mtime: fi.ModTime().UTC(), Hash: fileHash([]byte("link:" + target))}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if s.maxFile > 0 && fi.Size() > s.maxFile {
			res.skipped = append(res.skipped, Skip{rel, fmt.Sprintf("%d MB, over the per-file limit", fi.Size()>>20)})
			return nil
		}
		mt := fi.ModTime()
		e := &Entry{Size: fi.Size(), Mtime: mt.UTC(), Exec: runtime.GOOS != "windows" && fi.Mode()&0o111 != 0}
		if c, ok := s.cache[rel]; ok && c.Size == fi.Size() && c.Mtime == mt.UnixNano() {
			e.Hash, e.Chunks = c.Hash, c.Chunks
		} else {
			if s.now.Sub(mt) < settleTime && s.now.Sub(mt) > -time.Hour {
				res.unsettled[rel] = true
				return nil
			}
			data, err := readStable(p, fi)
			if err != nil {
				res.unsettled[rel] = true // changed while reading; next time
				return nil
			}
			e.Hash = fileHash(data)
			for _, c := range split(data) {
				e.Chunks = append(e.Chunks, s.keys.ChunkID(c))
			}
		}
		newCache[rel] = cacheEntry{Size: e.Size, Mtime: mt.UnixNano(), Hash: e.Hash, Chunks: e.Chunks}
		res.files[rel] = e
		res.total += e.Size
		return nil
	})
	s.cache = newCache
	return res, err
}

// readStable reads a file and makes sure it didn't change meanwhile.
func readStable(p string, before os.FileInfo) ([]byte, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || int64(len(data)) != after.Size() {
		return nil, errors.New("file changed while being read")
	}
	return data, nil
}

func isEmptyDir(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return len(names) == 0 && err != nil, nil
}
