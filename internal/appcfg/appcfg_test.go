package appcfg

import (
	"os"
	"path/filepath"
	"testing"
)

func withSettings(t *testing.T, json string) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("HOME", dir)
	if json == "" {
		return
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "settings.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Newest wins is the default; settings saved with the old default move over
// once, and a later choice of Keep both sticks.
func TestClashPolicyDefaultAndMigration(t *testing.T) {
	withSettings(t, "")
	if s, _ := Load(); s.ClashPolicy != "newest" || s.SettingsVersion != 1 {
		t.Fatalf("fresh install: %+v", s)
	}

	withSettings(t, `{"clashPolicy":"keep-both","backupMinutes":15}`)
	s, err := Load()
	if err != nil || s.ClashPolicy != "newest" || s.BackupMinutes != 15 {
		t.Fatalf("old default not migrated: %+v %v", s, err)
	}

	withSettings(t, `{"clashPolicy":"ask"}`)
	if s, _ := Load(); s.ClashPolicy != "ask" {
		t.Fatalf("an explicit other choice was changed: %+v", s)
	}

	withSettings(t, `{"clashPolicy":"keep-both","settingsVersion":1}`)
	if s, _ := Load(); s.ClashPolicy != "keep-both" {
		t.Fatalf("a deliberate Keep both was overridden: %+v", s)
	}

	// Saving keeps the version, so a Keep both chosen now survives the next start.
	withSettings(t, `{"clashPolicy":"keep-both"}`)
	s, _ = Load()
	s.ClashPolicy = "keep-both"
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	if s2, _ := Load(); s2.ClashPolicy != "keep-both" {
		t.Fatalf("Keep both chosen after the update was lost: %+v", s2)
	}
}
