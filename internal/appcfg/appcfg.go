// Package appcfg stores the desktop client's settings and secrets.
package appcfg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

const keyringService = "ClaudeSync"

type Settings struct {
	LANURL     string `json:"lanUrl"`
	TunnelURL  string `json:"tunnelUrl"`
	Pin        string `json:"pin"`
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`

	BackupMinutes  int `json:"backupMinutes"`
	RestoreMinutes int `json:"restoreMinutes"`

	Sessions     bool `json:"sessions"`
	CLIOnly      bool `json:"cliOnly"`
	Config       bool `json:"config"`
	Memory       bool `json:"memory"`
	Scratch      bool `json:"scratch"`
	ScratchMaxMB int  `json:"scratchMaxMb"`

	ClaudeHome  string   `json:"claudeHome,omitempty"`
	DesktopDir  string   `json:"desktopDir,omitempty"`
	SearchRoots []string `json:"searchRoots"`

	StartAtLogin bool `json:"startAtLogin"`
}

// Secrets live in the OS keychain when there is one, else in a 0600 file.
type Secrets struct {
	Token string `json:"token"`
	Key   []byte `json:"key"`
}

func Defaults() Settings {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	var roots []string
	for _, d := range []string{"", "Documents", "source", "code", "src", "projects", "dev", "git", "repos"} {
		p := filepath.Join(home, d)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			roots = append(roots, p)
		}
	}
	return Settings{
		DeviceName: host, BackupMinutes: 30, RestoreMinutes: 30,
		Sessions: true, Config: true, Memory: true, Scratch: true, ScratchMaxMB: 200,
		SearchRoots: roots, StartAtLogin: true,
	}
}

// Dir is the client's folder for settings, state and logs.
func Dir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d, _ = os.UserHomeDir()
	}
	return filepath.Join(d, "ClaudeSync")
}

func settingsPath() string { return filepath.Join(Dir(), "settings.json") }
func secretsPath() string  { return filepath.Join(Dir(), "secrets.json") }

func Load() (Settings, error) {
	s := Defaults()
	b, err := os.ReadFile(settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	if s.BackupMinutes < 1 {
		s.BackupMinutes = 30
	}
	if s.RestoreMinutes < 1 {
		s.RestoreMinutes = 30
	}
	return s, err
}

func Save(s Settings) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath())
}

func LoadSecrets() Secrets {
	var s Secrets
	if v, err := keyring.Get(keyringService, "token"); err == nil {
		s.Token = v
		if k, err := keyring.Get(keyringService, "key"); err == nil {
			s.Key, _ = base64.StdEncoding.DecodeString(k)
		}
		return s
	}
	if b, err := os.ReadFile(secretsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// SaveSecrets prefers the keychain and falls back to a private file (for
// Linux machines without a secret service, e.g. a bare Raspberry Pi).
func SaveSecrets(s Secrets) error {
	if err1 := keyring.Set(keyringService, "token", s.Token); err1 == nil {
		if err2 := keyring.Set(keyringService, "key", base64.StdEncoding.EncodeToString(s.Key)); err2 == nil {
			_ = os.Remove(secretsPath())
			return nil
		}
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return os.WriteFile(secretsPath(), b, 0o600)
}

func ClearSecrets() {
	_ = keyring.Delete(keyringService, "token")
	_ = keyring.Delete(keyringService, "key")
	_ = os.Remove(secretsPath())
}

// NormalizeURL adds https:// and strips trailing slashes.
func NormalizeURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return strings.TrimRight(u, "/")
}
