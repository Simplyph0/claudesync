// Package server implements the ClaudeSync server: password login, device
// tokens, and a versioned store of encrypted item blobs.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Simplyph0/claudesync/internal/proto"
	"golang.org/x/crypto/argon2"
)

const DefaultRetention = 20

var (
	ErrConflict     = errors.New("item changed on the server since your base version")
	ErrNotFound     = errors.New("not found")
	ErrBadPassword  = errors.New("wrong password")
	ErrNotInitDone  = errors.New("server has no password yet; run 'claudesync-server init'")
	ErrInvalidInput = errors.New("invalid input")
)

// Config is persisted in <data>/server.json.
type Config struct {
	PasswordHash string       `json:"passwordHash"`
	Retention    int          `json:"retention"`
	LANAddr      string       `json:"lanAddr"`   // HTTPS, self-signed, for the LAN
	LocalAddr    string       `json:"localAddr"` // plain HTTP on loopback, for cloudflared
	Vault        *proto.Vault `json:"vault,omitempty"`
}

type device struct {
	proto.Device
	TokenHash string `json:"tokenHash"`
}

type itemMeta struct {
	Kind     string             `json:"kind"`
	ID       string             `json:"id"`
	Versions []proto.ItemHeader `json:"versions"` // oldest first
}

type index struct {
	Seq   int64                `json:"seq"`
	Items map[string]*itemMeta `json:"items"`
}

// Store is safe for concurrent use.
type Store struct {
	dir     string
	mu      sync.Mutex
	cfg     Config
	devices map[string]*device
	idx     index
}

func DefaultConfig() Config {
	return Config{Retention: DefaultRetention, LANAddr: ":8443", LocalAddr: "127.0.0.1:8480"}
}

// Open loads (or creates) the store in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "items"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, cfg: DefaultConfig(), devices: map[string]*device{}, idx: index{Items: map[string]*itemMeta{}}}
	if err := readJSON(filepath.Join(dir, "server.json"), &s.cfg); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if s.cfg.Retention <= 0 {
		s.cfg.Retention = DefaultRetention
	}
	var devs []*device
	if err := readJSON(filepath.Join(dir, "devices.json"), &devs); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, d := range devs {
		s.devices[d.ID] = d
	}
	if err := readJSON(filepath.Join(dir, "index.json"), &s.idx); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if s.idx.Items == nil {
		s.idx.Items = map[string]*itemMeta{}
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// UpdateConfig applies fn to the config and saves it.
func (s *Store) UpdateConfig(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	if s.cfg.Retention <= 0 {
		s.cfg.Retention = DefaultRetention
	}
	s.pruneAllLocked()
	if err := s.saveIndexLocked(); err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.dir, "server.json"), s.cfg)
}

// --- password ---

func HashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	h := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 4, 32)
	return "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(h)
}

func checkPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[1])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 4, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Store) HasPassword() bool { return s.Config().PasswordHash != "" }

// SetPassword replaces the password without checking the old one (used by
// the local 'init' and 'reset-password' commands).
func (s *Store) SetPassword(pw string) error {
	if len(pw) < 8 {
		return fmt.Errorf("%w: password must be at least 8 characters", ErrInvalidInput)
	}
	h := HashPassword(pw)
	return s.UpdateConfig(func(c *Config) { c.PasswordHash = h })
}

// ChangePassword is the client-initiated reset; existing device tokens stay valid.
func (s *Store) ChangePassword(old, pw string) error {
	if !checkPassword(s.Config().PasswordHash, old) {
		return ErrBadPassword
	}
	return s.SetPassword(pw)
}

// --- devices ---

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Login checks the password and registers a new device.
func (s *Store) Login(pw, name string) (proto.LoginResponse, error) {
	cfg := s.Config()
	if cfg.PasswordHash == "" {
		return proto.LoginResponse{}, ErrNotInitDone
	}
	if !checkPassword(cfg.PasswordHash, pw) {
		return proto.LoginResponse{}, ErrBadPassword
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return proto.LoginResponse{}, fmt.Errorf("%w: device name must be 1-64 characters", ErrInvalidInput)
	}
	token := randomHex(32)
	now := time.Now().UTC()
	d := &device{Device: proto.Device{ID: randomHex(8), Name: name, Created: now, LastSeen: now}, TokenHash: hashToken(token)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.ID] = d
	if err := s.saveDevicesLocked(); err != nil {
		return proto.LoginResponse{}, err
	}
	return proto.LoginResponse{DeviceID: d.ID, Token: d.ID + "." + token}, nil
}

// Authenticate maps a bearer token to its device.
func (s *Store) Authenticate(tok string) (proto.Device, bool) {
	id, secret, ok := strings.Cut(tok, ".")
	if !ok {
		return proto.Device{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[id]
	if d == nil || subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(hashToken(secret))) != 1 {
		return proto.Device{}, false
	}
	if time.Since(d.LastSeen) > time.Minute {
		d.LastSeen = time.Now().UTC()
		_ = s.saveDevicesLocked()
	}
	return d.Device, true
}

func (s *Store) Devices() []proto.Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d.Device)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *Store) RevokeDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devices[id] == nil {
		return ErrNotFound
	}
	delete(s.devices, id)
	return s.saveDevicesLocked()
}

// RevokeAll logs out every PC (used by 'reset-password --logout-all').
func (s *Store) RevokeAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices = map[string]*device{}
	return s.saveDevicesLocked()
}

// --- vault ---

func (s *Store) Vault() *proto.Vault { return s.Config().Vault }

// SetVault stores the passphrase check record; it can only be set once
// (changing the passphrase would make every stored blob unreadable).
func (s *Store) SetVault(v proto.Vault) error {
	if len(v.Salt) < 16 || len(v.Check) == 0 {
		return ErrInvalidInput
	}
	s.mu.Lock()
	exists := s.cfg.Vault != nil
	s.mu.Unlock()
	if exists {
		return ErrConflict
	}
	return s.UpdateConfig(func(c *Config) { c.Vault = &v })
}

// --- items ---

func (s *Store) blobPath(kind, id string, v int) string {
	return filepath.Join(s.dir, "items", kind, id, fmt.Sprintf("%d.blob", v))
}

// Put stores a new version. base must equal the current latest version
// (0 for a new item) or ErrConflict is returned.
func (s *Store) Put(dev proto.Device, kind, id string, base int, blob []byte) (proto.ItemHeader, error) {
	if !proto.ValidID(kind) || !proto.ValidID(id) {
		return proto.ItemHeader{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := kind + "/" + id
	m := s.idx.Items[key]
	latest := 0
	if m != nil && len(m.Versions) > 0 {
		latest = m.Versions[len(m.Versions)-1].Version
	}
	if base != latest {
		return proto.ItemHeader{}, ErrConflict
	}
	h := proto.ItemHeader{Kind: kind, ID: id, Version: latest + 1, Seq: s.idx.Seq + 1, Device: dev.ID, DeviceName: dev.Name, Size: int64(len(blob)), Time: time.Now().UTC()}
	p := s.blobPath(kind, id, h.Version)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return proto.ItemHeader{}, err
	}
	if err := writeFileAtomic(p, blob); err != nil {
		return proto.ItemHeader{}, err
	}
	if m == nil {
		m = &itemMeta{Kind: kind, ID: id}
		s.idx.Items[key] = m
	}
	m.Versions = append(m.Versions, h)
	s.idx.Seq = h.Seq
	s.pruneLocked(m)
	return h, s.saveIndexLocked()
}

func (s *Store) pruneLocked(m *itemMeta) {
	for len(m.Versions) > s.cfg.Retention {
		old := m.Versions[0]
		_ = os.Remove(s.blobPath(m.Kind, m.ID, old.Version))
		m.Versions = m.Versions[1:]
	}
}

func (s *Store) pruneAllLocked() {
	for _, m := range s.idx.Items {
		s.pruneLocked(m)
	}
}

// Changes lists the latest header of every item changed after seq.
func (s *Store) Changes(since int64) proto.ItemList {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := proto.ItemList{Seq: s.idx.Seq, Items: []proto.ItemHeader{}}
	for _, m := range s.idx.Items {
		if n := len(m.Versions); n > 0 && m.Versions[n-1].Seq > since {
			out.Items = append(out.Items, m.Versions[n-1])
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Seq < out.Items[j].Seq })
	return out
}

func (s *Store) Versions(kind, id string) ([]proto.ItemHeader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.idx.Items[kind+"/"+id]
	if m == nil {
		return nil, ErrNotFound
	}
	return append([]proto.ItemHeader(nil), m.Versions...), nil
}

// Get returns a version's blob (v <= 0 means latest).
func (s *Store) Get(kind, id string, v int) (proto.ItemHeader, []byte, error) {
	s.mu.Lock()
	m := s.idx.Items[kind+"/"+id]
	var h proto.ItemHeader
	found := false
	if m != nil {
		for _, x := range m.Versions {
			if x.Version == v || (v <= 0 && x.Version > h.Version) {
				h, found = x, true
			}
		}
	}
	s.mu.Unlock()
	if !found {
		return h, nil, ErrNotFound
	}
	b, err := os.ReadFile(s.blobPath(kind, id, h.Version))
	return h, b, err
}

// --- uploads (chunked) ---

func (s *Store) uploadDir(id string) string { return filepath.Join(s.dir, "uploads", id) }

func (s *Store) StartUpload() (string, error) {
	id := randomHex(12)
	return id, os.MkdirAll(s.uploadDir(id), 0o700)
}

func (s *Store) PutChunk(uploadID string, n int, data []byte) error {
	if !proto.ValidID(uploadID) || n < 0 || n > 100000 {
		return ErrInvalidInput
	}
	dir := s.uploadDir(uploadID)
	if _, err := os.Stat(dir); err != nil {
		return ErrNotFound
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d", n)), data, 0o600)
}

// FinishUpload concatenates the chunks and stores them as an item version.
func (s *Store) FinishUpload(dev proto.Device, uploadID, kind, id string, base int) (proto.ItemHeader, error) {
	if !proto.ValidID(uploadID) {
		return proto.ItemHeader{}, ErrInvalidInput
	}
	dir := s.uploadDir(uploadID)
	defer os.RemoveAll(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return proto.ItemHeader{}, ErrNotFound
	}
	var blob []byte
	for _, e := range entries { // ReadDir sorts by name; names are zero-padded
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return proto.ItemHeader{}, err
		}
		blob = append(blob, b...)
	}
	return s.Put(dev, kind, id, base, blob)
}

// CleanUploads removes abandoned chunked uploads older than a day.
func (s *Store) CleanUploads() {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "uploads"))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(s.dir, "uploads", e.Name()))
		}
	}
}

// --- persistence ---

func (s *Store) saveDevicesLocked() error {
	devs := make([]*device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, d)
	}
	return writeJSON(filepath.Join(s.dir, "devices.json"), devs)
}

func (s *Store) saveIndexLocked() error {
	return writeJSON(filepath.Join(s.dir, "index.json"), s.idx)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
