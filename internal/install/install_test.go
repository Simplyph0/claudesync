package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Downloads", "claudesync-new")
	dst := filepath.Join(dir, "Programs", "ClaudeSync", "claudesync.exe")
	os.MkdirAll(filepath.Dir(src), 0o755)
	os.WriteFile(src, []byte("v2"), 0o755)

	if changed, err := Copy(src, dst); err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "v2" {
		t.Fatalf("installed %q", b)
	}
	if changed, err := Copy(src, dst); err != nil || changed {
		t.Fatalf("same version again should do nothing: changed=%v err=%v", changed, err)
	}

	// An update replaces the old copy; the download itself is left alone.
	os.WriteFile(src, []byte("v3"), 0o755)
	if changed, err := Copy(src, dst); err != nil || !changed {
		t.Fatalf("update: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "v3" {
		t.Fatalf("after update %q", b)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("download removed: %v", err)
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind")
	}
	Cleanup(dst)
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Fatalf("old copy not cleaned up")
	}
}

func TestSameAndDev(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a")
	if !Same(p, filepath.Join(dir, ".", "a")) {
		t.Fatal("same path not recognised")
	}
	if Same(p, filepath.Join(dir, "b")) {
		t.Fatal("different paths reported the same")
	}
	if !Dev(filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "claudesync")) || Dev(p) {
		t.Fatal("Dev")
	}
}
