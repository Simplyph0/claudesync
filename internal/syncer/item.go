package syncer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Simplyph0/claudesync/internal/vault"
	"github.com/klauspost/compress/zstd"
)

// FormatVersion is bumped when the archive layout changes.
const FormatVersion = 1

// File roles say where a file goes on restore.
const (
	RoleDesktop    = "desktop"    // <desktop>/claude-code-sessions/<rel>
	RoleTranscript = "transcript" // <claude>/projects/<enc(cwd)>/<rel>
	RoleScratch    = "scratch"    // <desktop>/scratch-workspaces/<scratchRel>/<rel>
	RoleClaude     = "claude"     // <claude>/<rel>
	RoleMemory     = "memory"     // <claude>/projects/<enc(project)>/memory/<rel>
)

type ProjectRef struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	GitRemote string `json:"gitRemote,omitempty"`
}

// Manifest describes an item; it travels encrypted inside the blob.
type Manifest struct {
	Format        int         `json:"format"`
	Kind          string      `json:"kind"`
	ID            string      `json:"id"`
	Title         string      `json:"title,omitempty"`
	Device        string      `json:"device,omitempty"`
	Platform      string      `json:"platform,omitempty"`
	Project       *ProjectRef `json:"project,omitempty"`
	ScratchRel    string      `json:"scratchRel,omitempty"` // slash-separated
	Cwd           string      `json:"cwd,omitempty"`        // portable form
	CliID         string      `json:"cliId,omitempty"`
	DesktopID     string      `json:"desktopId,omitempty"`
	UnknownFormat bool        `json:"unknownFormat,omitempty"`
}

type File struct {
	Role string
	Rel  string // slash-separated
	Data []byte
}

type Item struct {
	M     Manifest
	Files []File
}

func (it *Item) Key() string { return it.M.Kind + "/" + it.M.ID }

// Hash covers file names and portable contents (not the manifest), so the
// same session rebuilt on another PC hashes the same.
func (it *Item) Hash() string {
	fs := append([]File(nil), it.Files...)
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Role != fs[j].Role {
			return fs[i].Role < fs[j].Role
		}
		return fs[i].Rel < fs[j].Rel
	})
	h := sha256.New()
	for _, f := range fs {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", f.Role, f.Rel, len(f.Data))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isText says whether a file's contents should have paths rewritten.
func isText(role string, data []byte) bool {
	if role == RoleScratch {
		return false
	}
	return utf8.Valid(data) && !bytes.Contains(data, []byte{0})
}

// Seal packs and encrypts an item.
func Seal(it *Item, key []byte) ([]byte, error) {
	var raw bytes.Buffer
	zw, err := zstd.NewWriter(&raw, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	mb, err := json.Marshal(it.M)
	if err != nil {
		return nil, err
	}
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("manifest.json", mb); err != nil {
		return nil, err
	}
	for _, f := range it.Files {
		if err := add(f.Role+"/"+f.Rel, f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return vault.Seal(key, raw.Bytes(), []byte(it.Key()))
}

// Unseal decrypts and unpacks a blob for kind/id.
func Unseal(blob, key []byte, kind, id string) (*Item, error) {
	raw, err := vault.Open(key, blob, []byte(kind+"/"+id))
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	it := &Item{}
	gotManifest := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if h.Name == "manifest.json" {
			if err := json.Unmarshal(data, &it.M); err != nil {
				return nil, err
			}
			gotManifest = true
			continue
		}
		role, rel, ok := strings.Cut(h.Name, "/")
		if !ok || !safeRel(rel) {
			return nil, fmt.Errorf("unsafe path in archive: %q", h.Name)
		}
		it.Files = append(it.Files, File{Role: role, Rel: rel, Data: data})
	}
	if !gotManifest {
		return nil, errors.New("archive has no manifest")
	}
	if it.M.Kind != kind || it.M.ID != id {
		return nil, errors.New("archive belongs to a different item")
	}
	return it, nil
}

// safeRel rejects absolute paths and anything that climbs out with "..".
func safeRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) || (len(rel) > 1 && rel[1] == ':') {
		return false
	}
	clean := path.Clean(rel)
	return clean == rel && clean != ".." && !strings.HasPrefix(clean, "../")
}
