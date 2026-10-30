// Package proto holds the wire types shared by the ClaudeSync server and client.
package proto

import (
	"regexp"
	"time"
)

// Version is the protocol/app version reported by both programs.
const Version = "0.6.0"

// Item kinds.
const (
	KindSession = "session"
	KindConfig  = "config"
	KindMemory  = "memory"
)

// HTTP headers used by item uploads.
const (
	HeaderBaseVersion = "X-Base-Version"
	HeaderAuth        = "Authorization"
)

// MaxDirectUpload is the largest blob sent in one request; bigger blobs go
// through chunked uploads (Cloudflare limits request bodies to 100 MB).
const MaxDirectUpload = 32 << 20

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidID reports whether s is safe to use as an item kind or id (it becomes
// part of a file path on the server).
func ValidID(s string) bool {
	return idPattern.MatchString(s) && s != "." && s != ".."
}

type LoginRequest struct {
	Password   string `json:"password"`
	DeviceName string `json:"deviceName"`
}

type LoginResponse struct {
	DeviceID string `json:"deviceId"`
	Token    string `json:"token"`
}

type PasswordChange struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ItemHeader describes one stored version of an item. The blob itself is
// end-to-end encrypted; nothing here reveals its contents.
type ItemHeader struct {
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	Version    int       `json:"version"`
	Seq        int64     `json:"seq"`
	Device     string    `json:"device"`
	DeviceName string    `json:"deviceName"`
	Size       int64     `json:"size"`
	Time       time.Time `json:"time"`
	// Meta is a small end-to-end encrypted description (the title) so lists
	// can be shown without downloading every item.
	Meta []byte `json:"meta,omitempty"`
	// Removed items were struck from the server backup. They stop syncing
	// and are deleted for good after RemovedHoldDays.
	Removed   bool      `json:"removed,omitempty"`
	RemovedAt time.Time `json:"removedAt,omitzero"`
	// Purged items were deleted for good: only a small marker is left, so no
	// PC uploads them again. They can't be brought back.
	Purged bool `json:"purged,omitempty"`
}

// RemovedHoldDays is how long a removed item can still be brought back.
const RemovedHoldDays = 30

// HeaderMeta carries ItemHeader.Meta (base64) on uploads.
const HeaderMeta = "X-Item-Meta"

// Key returns "kind/id".
func (h ItemHeader) Key() string { return h.Kind + "/" + h.ID }

type ItemList struct {
	Items []ItemHeader `json:"items"`
	Seq   int64        `json:"seq"`
}

// Vault lets every PC verify the shared sync passphrase without the server
// learning it: Check is a known string sealed with the derived key.
type Vault struct {
	Salt  []byte `json:"salt"`
	Check []byte `json:"check"`
}

type Device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
}

type ServerInfo struct {
	Version   string `json:"version"`
	Retention int    `json:"retention"`
	HasVault  bool   `json:"hasVault"`
}

type Settings struct {
	Retention int `json:"retention"`
}

type UploadStart struct {
	UploadID string `json:"uploadId"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// Project sync.
const KindProject = "project"

// ManifestUpload is a project version plus the chunks it uses.
type ManifestUpload struct {
	Base int      `json:"base"`
	Meta []byte   `json:"meta,omitempty"`
	Blob []byte   `json:"blob"`
	Refs []string `json:"refs"`
}

type ChunkList struct {
	IDs []string `json:"ids"`
}
