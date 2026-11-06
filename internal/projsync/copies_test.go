package projsync

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestOriginalOf(t *testing.T) {
	for in, want := range map[string]string{
		"main (from James-PC 2026-11-05 1430).cpp":    "main.cpp",
		"src/lib/util (from swift 2026-01-02 0903).h": "src/lib/util.h",
		"README (from A 2026-11-05 1430)":             "README",
		" (from A 2026-11-05 1430).env":               ".env",
		"notes.v2 (from A 2026-11-05 1430).tar.gz":    "notes.v2.tar.gz",
		"x (from A 2026-11-05 1430) more.txt":         "",
		"main (from A yesterday).cpp":                 "",
		"plain.cpp":                                   "",
		"(from A 2026-11-05 1430)":                    "",
		"dir (from A 2026-11-05 1430)/inner.txt":      "",
	} {
		got, ok := originalOf(in)
		if (want == "") == ok || got != want {
			t.Errorf("originalOf(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// With Newest wins, a file edited on both PCs ends up the same on both, with
// no "(from PC date)" copy anywhere, and the older edit is kept in the trash.
func TestNewestWinsLeavesNoCopies(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.m.Settings = DefaultSettings()
	a.write("notes.bin", "v1", 3*time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.m.Settings = DefaultSettings()
	b.sync()

	a.write("notes.bin", "A edit", 20*time.Minute)
	b.write("notes.bin", "B edit, later", 5*time.Minute)
	a.sync()
	b.sync()
	a.sync()
	b.sync()
	for _, p := range []*pc{a, b} {
		eq(t, p.name+" has one file", p.list(), []string{"notes.bin"})
		eq(t, p.name+" has the later edit", p.read("notes.bin"), "B edit, later")
		if st := p.status(); st.Decision != nil || len(st.Clashes) > 0 || len(st.Copies) > 0 {
			t.Fatalf("%s is still unsettled: %+v", p.name, st)
		}
	}
	if !a.trashHas("notes.bin", "A edit") {
		t.Fatal("A's older edit wasn't kept in the trash")
	}
	var seen bool
	for _, p := range []*pc{a, b} { // whichever PC saw the clash says so
		for _, e := range p.status().Events {
			seen = seen || strings.Contains(e.Msg, "newer one")
		}
	}
	if !seen {
		t.Fatalf("the log doesn't say which was kept: A %+v, B %+v", a.status().Events, b.status().Events)
	}
}

// Two edits within seconds can't be ordered by file time; both PCs must still
// pick the same one instead of leaving copies.
func TestNewestWinsNearTieConverges(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.m.Settings = DefaultSettings()
	a.write("gen.bin", "v1", 3*time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.m.Settings = DefaultSettings()
	b.sync()
	a.write("gen.bin", "from A", 5*time.Minute)
	b.write("gen.bin", "from B", 5*time.Minute)
	for i := 0; i < 3; i++ {
		a.sync()
		b.sync()
	}
	if a.read("gen.bin") != b.read("gen.bin") || a.read("gen.bin") == "<missing>" {
		t.Fatalf("PCs disagree: A=%q B=%q", a.read("gen.bin"), b.read("gen.bin"))
	}
	eq(t, "A has one file", a.list(), []string{"gen.bin"})
	eq(t, "B has one file", b.list(), []string{"gen.bin"})
	if !(a.trashHas("gen.bin", "from A") || b.trashHas("gen.bin", "from B")) {
		t.Fatal("the losing edit wasn't kept")
	}
}

func TestTidyCopies(t *testing.T) {
	w := newWorld(t)
	a := w.pc("A", ModeSync)
	a.m.Settings.ClashPolicy = ClashKeepBoth
	a.write("main.cpp", "one", 3*time.Hour)
	a.sync()
	b := w.pc("B", ModeSync)
	b.m.Settings.ClashPolicy = ClashKeepBoth
	b.sync()
	a.write("main.cpp", "A", 20*time.Minute)
	b.write("main.cpp", "B", 5*time.Minute)
	a.sync()
	b.sync()
	a.sync()
	copies := a.status().Copies
	if len(copies) == 0 {
		t.Fatalf("Keep both left no copy to tidy: %v", a.list())
	}
	a.write("orphan (from Z 2026-01-01 0000).txt", "no original here", time.Hour)
	a.write("keep (from Z 2026-01-01 0000) more.txt", "not one of ours", time.Hour)
	a.sync()
	got := a.status().Copies
	sort.Strings(got)
	for _, c := range got {
		if strings.HasPrefix(c, "orphan") || strings.HasPrefix(c, "keep") {
			t.Fatalf("offered a file that isn't a leftover copy: %v", got)
		}
	}

	n, err := a.m.TidyCopies(key)
	if err != nil || n != len(copies) {
		t.Fatalf("tidied %d of %d: %v", n, len(copies), err)
	}
	for _, c := range copies {
		if _, err := os.Stat(filepath.Join(a.folder, filepath.FromSlash(c))); err == nil {
			t.Fatalf("%s is still there", c)
		}
		if !a.trashHas(c, a.m.trashContent(t, c)) {
			t.Fatalf("%s didn't go to the trash", c)
		}
	}
	eq(t, "originals and others stay", a.read("orphan (from Z 2026-01-01 0000).txt"), "no original here")
	if a.read("main.cpp") == "<missing>" {
		t.Fatal("the original was removed")
	}
	// The other PC drops its copies at its next sync, and nothing comes back.
	a.sync()
	b.sync()
	a.sync()
	for _, p := range []*pc{a, b} {
		if c := p.status().Copies; len(c) != 0 {
			t.Fatalf("%s still has copies: %v (%v)", p.name, c, p.list())
		}
	}
	eq(t, "same files on both", strings.Join(a.list(), "|"), strings.Join(b.list(), "|"))
}

// trashContent returns what a trashed copy held (for the assertion above).
func (m *Manager) trashContent(t *testing.T, rel string) string {
	for _, b := range m.Trash().List() {
		if data, err := os.ReadFile(filepath.Join(m.Trash().Dir, b.ID, "files", filepath.FromSlash(rel))); err == nil {
			return string(data)
		}
	}
	return "<not in trash>"
}
