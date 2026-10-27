package syncer

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// A snapshot is a local archive of files as they were just before ClaudeSync
// changed anything. Every restore writes one first, so any change can be undone.

const snapshotsKept = 40

// SnapshotInfo describes a saved snapshot.
type SnapshotInfo struct {
	Path   string
	Time   time.Time
	Reason string
	Size   int64
}

// Snapshot collects files into one archive. It is created lazily, so a
// snapshot with nothing in it leaves nothing on disk.
type Snapshot struct {
	dir, reason string
	path        string
	f           *os.File
	zw          *zstd.Encoder
	tw          *tar.Writer
	seen        map[string]bool
	count       int
}

// NewSnapshot prepares a snapshot in dir.
func NewSnapshot(dir, reason string) *Snapshot {
	return &Snapshot{dir: dir, reason: reason, seen: map[string]bool{}}
}

func (s *Snapshot) open() error {
	if s.tw != nil {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	name := time.Now().Format("2006-01-02_15-04-05.000") + "_" + safeName(s.reason) + ".tar.zst"
	s.path = filepath.Join(s.dir, name)
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		f.Close()
		return err
	}
	s.f, s.zw, s.tw = f, zw, tar.NewWriter(zw)
	return nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, s)
}

// AddFile saves the current contents of path (absolute). Missing files are
// skipped. A file is only saved once per snapshot.
func (s *Snapshot) AddFile(path string) error {
	if s == nil {
		return nil
	}
	path = filepath.Clean(path)
	if s.seen[path] {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, _ := os.Stat(path)
	if err := s.open(); err != nil {
		return err
	}
	hdr := &tar.Header{Name: filepath.ToSlash(path), Mode: 0o600, Size: int64(len(data)), Format: tar.FormatPAX}
	if info != nil {
		hdr.ModTime = info.ModTime()
	}
	if err := s.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := s.tw.Write(data); err != nil {
		return err
	}
	s.seen[path] = true
	s.count++
	return nil
}

// AddTree saves every file under dir.
func (s *Snapshot) AddTree(dir string) error {
	if s == nil {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			return s.AddFile(p)
		}
		return nil
	})
}

// Count is the number of files saved so far.
func (s *Snapshot) Count() int {
	if s == nil {
		return 0
	}
	return s.count
}

// Close finishes the archive and prunes old snapshots.
func (s *Snapshot) Close() error {
	if s == nil || s.tw == nil {
		return nil
	}
	err := errors.Join(s.tw.Close(), s.zw.Close(), s.f.Sync(), s.f.Close())
	s.tw = nil
	pruneSnapshots(s.dir)
	return err
}

// Path is the archive file ("" if nothing was saved).
func (s *Snapshot) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// ListSnapshots returns snapshots newest first.
func ListSnapshots(dir string) []SnapshotInfo {
	entries, _ := os.ReadDir(dir)
	var out []SnapshotInfo
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".tar.zst") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(name, ".tar.zst")
		ts, reason, _ := strings.Cut(base, "_")
		if t2, r2, ok := strings.Cut(reason, "_"); ok {
			ts, reason = ts+"_"+t2, r2
		}
		t, err := time.ParseInLocation("2006-01-02_15-04-05.000", ts, time.Local)
		if err != nil {
			t = info.ModTime()
		}
		out = append(out, SnapshotInfo{Path: filepath.Join(dir, name), Time: t, Reason: strings.ReplaceAll(reason, "-", " "), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func pruneSnapshots(dir string) {
	list := ListSnapshots(dir)
	for i := snapshotsKept; i < len(list); i++ {
		_ = os.Remove(list[i].Path)
	}
}

// SnapshotFiles lists the files in a snapshot.
func SnapshotFiles(path string) ([]string, error) {
	var out []string
	err := readSnapshot(path, func(name string, _ []byte) error {
		out = append(out, filepath.FromSlash(name))
		return nil
	})
	return out, err
}

func readSnapshot(path string, fn func(name string, data []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := fn(h.Name, data); err != nil {
			return err
		}
	}
}

// RestoreSnapshot puts every file in the snapshot back. Files that would be
// overwritten are first saved in a new "before undo" snapshot. Nothing is
// deleted. It returns how many files were written.
func RestoreSnapshot(path, snapDir string) (int, error) {
	undo := NewSnapshot(snapDir, "before undo")
	n := 0
	err := readSnapshot(path, func(name string, data []byte) error {
		dst := filepath.FromSlash(name)
		if !filepath.IsAbs(dst) {
			return fmt.Errorf("snapshot entry %q is not an absolute path", name)
		}
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
			return nil
		}
		if err := undo.AddFile(dst); err != nil {
			return fmt.Errorf("saving %s before undo: %w", dst, err)
		}
		if err := writeAtomic(dst, data); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, errors.Join(err, undo.Close())
}
