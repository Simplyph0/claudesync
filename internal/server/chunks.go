package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Chunk store for project files. Chunks are encrypted on the PC and named by
// a keyed hash of their plaintext, so equal chunks are stored once. A chunk
// is kept while any stored project version lists it in its refs.

var chunkIDRe = regexp.MustCompile(`^k[0-9a-f]{64}$`)

// MaxChunkBlob is the largest encrypted chunk accepted (4 MB plus overhead).
const MaxChunkBlob = 5 << 20

var ErrMissingChunks = errors.New("missing chunks")

func (s *Store) chunkPath(id string) string {
	return filepath.Join(s.dir, "chunks", id[1:3], id)
}

func (s *Store) refsPath(kind, id string, v int) string {
	return filepath.Join(s.dir, "items", kind, id, fmt.Sprintf("%d.refs", v))
}

// MissingChunks returns the ids the server doesn't have. Chunks it does
// have are marked as recently wanted, so cleanup leaves them alone while the
// PC that asked finishes its upload.
func (s *Store) MissingChunks(ids []string) []string {
	var out []string
	now := time.Now()
	for _, id := range ids {
		if !chunkIDRe.MatchString(id) {
			continue
		}
		p := s.chunkPath(id)
		if _, err := os.Stat(p); err != nil {
			out = append(out, id)
		} else {
			_ = os.Chtimes(p, now, now)
		}
	}
	return out
}

// StoreChunk saves an encrypted chunk (a no-op if it's already there).
func (s *Store) StoreChunk(id string, blob []byte) error {
	if !chunkIDRe.MatchString(id) || len(blob) > MaxChunkBlob {
		return ErrInvalidInput
	}
	p := s.chunkPath(id)
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		_ = os.Chtimes(p, now, now) // freshly wanted again: protect from cleanup
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, blob)
}

func (s *Store) LoadChunk(id string) ([]byte, error) {
	if !chunkIDRe.MatchString(id) {
		return nil, ErrInvalidInput
	}
	b, err := os.ReadFile(s.chunkPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// GCChunks deletes chunks no stored version refers to. Chunks younger than
// minAge are kept: they may belong to an upload still in progress.
func (s *Store) GCChunks(minAge time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(s.dir, "items"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".refs") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err // can't tell what's used: delete nothing
		}
		var refs []string
		if err := json.Unmarshal(b, &refs); err != nil {
			return err
		}
		for _, r := range refs {
			used[r] = true
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := 0
	_ = filepath.WalkDir(filepath.Join(s.dir, "chunks"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || used[d.Name()] {
			return nil
		}
		if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > minAge {
			if os.Remove(p) == nil {
				n++
			}
		}
		return nil
	})
	return n, nil
}
