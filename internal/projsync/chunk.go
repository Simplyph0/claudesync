// Package projsync syncs project folders: files are split into encrypted,
// deduplicated chunks; each project version is an encrypted manifest (the
// file list) stored in the server's versioned item store. Syncing compares
// this PC, the server and the last version both agreed on (the base).
package projsync

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"github.com/Simplyph0/claudesync/internal/vault"
	"github.com/klauspost/compress/zstd"
)

// Content-defined chunking: boundaries depend on the bytes, so an insert
// near the start of a file only changes the chunks around it.
const (
	smallFile = 64 << 10 // stored as a single chunk
	minChunk  = 256 << 10
	maxChunk  = 4 << 20
	cutMask   = (1 << 20) - 1 // ~1 MB average after minChunk
)

var gear = func() (g [256]uint64) {
	x := uint64(0x9E3779B97F4A7C15) // splitmix64: fixed so every PC cuts identically
	for i := range g {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		g[i] = z ^ (z >> 31)
	}
	return
}()

// split cuts data into chunks.
func split(data []byte) [][]byte {
	if len(data) <= smallFile {
		return [][]byte{data}
	}
	var out [][]byte
	for len(data) > 0 {
		n := cut(data)
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

func cut(data []byte) int {
	if len(data) <= minChunk {
		return len(data)
	}
	limit := min(len(data), maxChunk)
	var h uint64
	for i := minChunk; i < limit; i++ {
		h = (h << 1) + gear[data[i]]
		if h&cutMask == 0 {
			return i + 1
		}
	}
	return limit
}

// Keys derives the two keys project sync uses from the vault key.
type Keys struct {
	Enc []byte // encrypts chunks and manifests
	ID  []byte // names chunks (HMAC), so equal chunks dedupe but contents stay secret
}

func NewKeys(vaultKey []byte) Keys {
	m := hmac.New(sha256.New, vaultKey)
	m.Write([]byte("claudesync chunk-id v1"))
	return Keys{Enc: vaultKey, ID: m.Sum(nil)}
}

// ChunkID names a chunk by a keyed hash of its plaintext.
func (k Keys) ChunkID(data []byte) string {
	m := hmac.New(sha256.New, k.ID)
	m.Write(data)
	return "k" + hex.EncodeToString(m.Sum(nil))
}

var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zdec, _ = zstd.NewReader(nil)
)

// SealChunk compresses and encrypts a chunk, bound to its id.
func (k Keys) SealChunk(id string, data []byte) ([]byte, error) {
	return vault.Seal(k.Enc, zenc.EncodeAll(data, nil), []byte(id))
}

// OpenChunk reverses SealChunk and checks the content matches its id.
func (k Keys) OpenChunk(id string, blob []byte) ([]byte, error) {
	comp, err := vault.Open(k.Enc, blob, []byte(id))
	if err != nil {
		return nil, err
	}
	data, err := zdec.DecodeAll(comp, nil)
	if err != nil {
		return nil, err
	}
	if k.ChunkID(data) != id {
		return nil, errChunkMismatch
	}
	return data, nil
}

type chunkErr string

func (e chunkErr) Error() string { return string(e) }

const errChunkMismatch = chunkErr("chunk content doesn't match its id")

func fileHash(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func isText(data []byte) bool {
	return len(data) <= 5<<20 && !bytes.Contains(data, []byte{0})
}
