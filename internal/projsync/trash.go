package projsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The trash keeps every project file ClaudeSync replaces or removes:
// <dir>/<batch>/files/<rel path>, plus <batch>/batch.json describing it.

type TrashFile struct {
	Rel  string `json:"rel"`
	Orig string `json:"orig"` // absolute path it came from
	Size int64  `json:"size"`
}

type TrashBatch struct {
	ID      string      `json:"id"`
	Project string      `json:"project"`
	Reason  string      `json:"reason"`
	Time    time.Time   `json:"time"`
	Files   []TrashFile `json:"files"`
	dir     string
}

type Trash struct {
	Dir      string
	Days     int
	MaxBytes int64
}

type trashBatch struct {
	t     *Trash
	b     TrashBatch
	ready bool
}

// begin starts a batch; it only appears on disk if something is added.
func (t *Trash) begin(project, reason string) *trashBatch {
	id := time.Now().Format("2006-01-02_15-04-05.000") + "_" + safeID(project)
	return &trashBatch{t: t, b: TrashBatch{ID: id, Project: project, Reason: reason, Time: time.Now()}}
}

func safeID(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

// add copies orig into the batch. The caller only changes or removes orig
// after this returns nil.
func (tb *trashBatch) add(orig, rel string) error {
	src, err := os.Open(orig)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	dst := filepath.Join(tb.t.Dir, tb.b.ID, "files", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if info, err := os.Stat(orig); err == nil && info.Size() != n {
		return fmt.Errorf("copy of %s to trash is incomplete", orig)
	}
	tb.b.Files = append(tb.b.Files, TrashFile{Rel: rel, Orig: orig, Size: n})
	tb.ready = true
	return tb.save()
}

func (tb *trashBatch) save() error {
	b, _ := json.MarshalIndent(tb.b, "", " ")
	return os.WriteFile(filepath.Join(tb.t.Dir, tb.b.ID, "batch.json"), b, 0o600)
}

func (tb *trashBatch) count() int { return len(tb.b.Files) }

// List returns trash batches, newest first.
func (t *Trash) List() []TrashBatch {
	entries, _ := os.ReadDir(t.Dir)
	var out []TrashBatch
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(t.Dir, e.Name(), "batch.json"))
		if err != nil {
			continue
		}
		var tb TrashBatch
		if json.Unmarshal(b, &tb) == nil {
			tb.dir = filepath.Join(t.Dir, e.Name())
			out = append(out, tb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// Prune removes batches older than Days, then the oldest until under MaxBytes.
func (t *Trash) Prune() {
	list := t.List()
	var total int64
	for _, b := range list {
		size := int64(0)
		for _, f := range b.Files {
			size += f.Size
		}
		old := t.Days > 0 && time.Since(b.Time) > time.Duration(t.Days)*24*time.Hour
		if old || (t.MaxBytes > 0 && total+size > t.MaxBytes) {
			_ = os.RemoveAll(b.dir)
			continue
		}
		total += size
	}
}

// Restore puts files from a batch back (all of them if rels is empty).
// Files that would be overwritten go into a new batch first.
func (t *Trash) Restore(id string, rels []string) (int, error) {
	var batch *TrashBatch
	for _, b := range t.List() {
		if b.ID == id {
			b := b
			batch = &b
		}
	}
	if batch == nil {
		return 0, errors.New("that trash entry no longer exists")
	}
	want := map[string]bool{}
	for _, r := range rels {
		want[r] = true
	}
	undo := t.begin(batch.Project, "before restoring from trash")
	n := 0
	var errs []error
	for _, f := range batch.Files {
		if len(want) > 0 && !want[f.Rel] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(batch.dir, "files", filepath.FromSlash(f.Rel)))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if cur, err := os.ReadFile(f.Orig); err == nil {
			if bytes.Equal(cur, data) {
				continue
			}
			if err := undo.add(f.Orig, f.Rel); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := writeFileAtomic(f.Orig, data, time.Time{}, false); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

func writeFileAtomic(dst string, data []byte, mtime time.Time, exec bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	tmp := dst + ".claudesync-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if exec {
		_ = os.Chmod(tmp, mode)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if !mtime.IsZero() {
		_ = os.Chtimes(dst, mtime, mtime)
	}
	return nil
}
