package projsync

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/Simplyph0/claudesync/internal/vault"
	"github.com/klauspost/compress/zstd"
)

// Entry is one file (or empty folder, or symlink) in a project.
type Entry struct {
	Size   int64     `json:"size"`
	Mtime  time.Time `json:"mtime"`
	Exec   bool      `json:"exec,omitempty"`
	Link   string    `json:"link,omitempty"` // symlink target
	Dir    bool      `json:"dir,omitempty"`  // empty folder
	Hash   string    `json:"hash,omitempty"` // sha256 of the content
	Chunks []string  `json:"chunks,omitempty"`
}

// same reports whether two entries have the same content. A missing entry
// only equals another missing entry.
func same(a, b *Entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Hash == b.Hash && a.Link == b.Link && a.Dir == b.Dir && a.Exec == b.Exec
}

type GitInfo struct {
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
}

// Manifest is one version of a project: every file and what it contains.
type Manifest struct {
	Format  int                  `json:"format"`
	Key     string               `json:"key"`
	Name    string               `json:"name"`
	Device  string               `json:"device"`
	Time    time.Time            `json:"time"`
	Git     *GitInfo             `json:"git,omitempty"`
	Files   map[string]*Entry    `json:"files"`             // slash path from the project root
	Deleted map[string]time.Time `json:"deleted,omitempty"` // removed paths, so stale copies don't come back
}

const manifestFormat = 1

// deletedKept is how long a deletion is remembered.
const deletedKept = 180 * 24 * time.Hour

func newManifest(key, name, device string) *Manifest {
	return &Manifest{Format: manifestFormat, Key: key, Name: name, Device: device,
		Files: map[string]*Entry{}, Deleted: map[string]time.Time{}}
}

func (m *Manifest) clone() *Manifest {
	if m == nil {
		return nil
	}
	c := *m
	c.Files = make(map[string]*Entry, len(m.Files))
	for k, v := range m.Files {
		e := *v
		c.Files[k] = &e
	}
	c.Deleted = make(map[string]time.Time, len(m.Deleted))
	for k, v := range m.Deleted {
		c.Deleted[k] = v
	}
	return &c
}

// Refs lists the chunks the manifest uses, so the server can tell which
// chunks are still needed.
func (m *Manifest) Refs() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range m.Files {
		for _, c := range e.Chunks {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (m *Manifest) totalSize() int64 {
	var n int64
	for _, e := range m.Files {
		n += e.Size
	}
	return n
}

func files(m *Manifest) map[string]*Entry {
	if m == nil {
		return nil
	}
	return m.Files
}

func sealManifest(k Keys, kind, id string, m *Manifest) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	return vault.Seal(k.Enc, enc.EncodeAll(b, nil), []byte(kind+"/"+id))
}

func openManifest(k Keys, kind, id string, blob []byte) (*Manifest, error) {
	comp, err := vault.Open(k.Enc, blob, []byte(kind+"/"+id))
	if err != nil {
		return nil, err
	}
	b, err := zdec.DecodeAll(comp, nil)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	if m.Files == nil {
		m.Files = map[string]*Entry{}
	}
	if m.Deleted == nil {
		m.Deleted = map[string]time.Time{}
	}
	return m, nil
}
