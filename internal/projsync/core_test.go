package projsync

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestChunkingIsStableAroundEdits(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 12<<20)
	r.Read(data)
	k := NewKeys(bytes.Repeat([]byte{7}, 32))
	ids := func(d []byte) map[string]bool {
		m := map[string]bool{}
		for _, c := range split(d) {
			if len(c) > maxChunk {
				t.Fatalf("chunk of %d bytes", len(c))
			}
			m[k.ChunkID(c)] = true
		}
		return m
	}
	a := ids(data)
	edited := append(append(append([]byte{}, data[:100]...), []byte("INSERTED")...), data[100:]...)
	b := ids(edited)
	shared := 0
	for id := range b {
		if a[id] {
			shared++
		}
	}
	if len(a) < 4 || shared < len(a)-2 {
		t.Fatalf("an insert near the start changed too many chunks: %d of %d shared", shared, len(a))
	}
	var joined []byte
	for _, c := range split(data) {
		joined = append(joined, c...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("chunks don't reassemble the file")
	}
	blob, _ := k.SealChunk("kx", []byte("hello"))
	if _, err := k.OpenChunk("kx", blob); err == nil {
		t.Fatal("chunk accepted under the wrong id")
	}
}

func TestIgnoreRules(t *testing.T) {
	m := NewMatcher()
	m.add("*.log", "")
	m.add("/secret.txt", "")
	m.add("cache/", "")
	m.add("!keep.log", "")
	m.add("**/gen/*.c", "")
	m.add("local.cfg", "sub")
	cases := map[string]bool{
		"a.log": true, "x/b.log": true, "keep.log": false, "secret.txt": true, "x/secret.txt": false,
		"node_modules": true, ".git": true, "src/main.cpp": false, "a/gen/x.c": true, "gen/x.c": true,
		"sub/local.cfg": true, "local.cfg": false,
	}
	for p, want := range cases {
		dir := p == "node_modules" || p == ".git"
		if got := m.Ignored(p, dir); got != want {
			t.Errorf("Ignored(%q) = %v, want %v", p, got, want)
		}
	}
	if !m.Ignored("cache", true) || m.Ignored("cache", false) {
		t.Error("dir-only rule")
	}
}

func TestPlanTable(t *testing.T) {
	e := func(h string) *Entry { return &Entry{Hash: h} }
	base := map[string]*Entry{"same": e("1"), "mine": e("1"), "theirs": e("1"), "both": e("1"), "conv": e("1"),
		"delmine": e("1"), "deltheirs": e("1"), "delmine-edtheirs": e("1"), "edmine-deltheirs": e("1")}
	local := map[string]*Entry{"same": e("1"), "mine": e("2"), "theirs": e("1"), "both": e("2"), "conv": e("3"),
		"deltheirs": e("1"), "delmine-edtheirs": nil, "edmine-deltheirs": e("2"), "newmine": e("9")}
	remote := map[string]*Entry{"same": e("1"), "mine": e("1"), "theirs": e("2"), "both": e("3"), "conv": e("3"),
		"delmine": e("1"), "delmine-edtheirs": e("2"), "newtheirs": e("8")}
	delete(local, "delmine-edtheirs")
	want := map[string]actKind{"mine": actUpload, "theirs": actDownload, "both": actClash, "delmine": actRecordDelete,
		"deltheirs": actTrashLocal, "delmine-edtheirs": actDownload, "edmine-deltheirs": actUpload, "newmine": actUpload, "newtheirs": actDownload}
	got := map[string]actKind{}
	for _, a := range plan(local, remote, base) {
		got[a.path] = a.kind
	}
	if len(got) != len(want) {
		t.Errorf("got actions %v", got)
	}
	for p, k := range want {
		if g, ok := got[p]; !ok || g != k {
			t.Errorf("%s: got %v, want %v", p, g, k)
		}
	}
}

func TestMerge3(t *testing.T) {
	base := "a\nb\nc\nd\ne\nf\ng\n"
	mine := "a\nB\nc\nd\ne\nf\ng\n"      // changed line 2
	theirs := "a\nb\nc\nd\ne\nF\ng\nh\n" // changed line 6, added a line
	got, ok := merge3([]byte(base), []byte(mine), []byte(theirs))
	if !ok || string(got) != "a\nB\nc\nd\ne\nF\ng\nh\n" {
		t.Fatalf("merge: ok=%v\n%s", ok, got)
	}
	if _, ok := merge3([]byte(base), []byte("a\nX\nc\nd\ne\nf\ng\n"), []byte("a\nY\nc\nd\ne\nf\ng\n")); ok {
		t.Fatal("overlapping edits merged")
	}
	same, ok := merge3([]byte(base), []byte(mine), []byte(mine))
	if !ok || string(same) != mine {
		t.Fatal("identical edits")
	}
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	big := sb.String()
	m2 := strings.Replace(big, "line 0\n", "first\n", 1)
	t2 := big + "last\n"
	if got, ok := merge3([]byte(big), []byte(m2), []byte(t2)); !ok || !strings.HasPrefix(string(got), "first\n") || !strings.HasSuffix(string(got), "last\n") {
		t.Fatal("merge of a long file")
	}
}
