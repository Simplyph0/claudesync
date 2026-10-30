package server

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Simplyph0/claudesync/internal/proto"
)

// Deleting for good erases every version and its chunks, but leaves a marker
// so nothing is ever uploaded again, and can't be brought back.
func TestPurgeDeletesForGood(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dev := proto.Device{ID: "d1", Name: "pc"}
	mine := "k" + strings.Repeat("a", 64)
	shared := "k" + strings.Repeat("b", 64)
	other := "k" + strings.Repeat("c", 64)
	for _, c := range []string{mine, shared, other} {
		if err := st.StoreChunk(c, []byte("data "+c[:3])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PutRefs(dev, "project", "prj_one", 0, []byte("m1"), []byte("meta"), []string{mine, shared}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRefs(dev, "project", "prj_one", 1, []byte("m2"), []byte("meta"), []string{mine, shared}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRefs(dev, "project", "prj_two", 0, []byte("m"), nil, []string{shared, other}); err != nil {
		t.Fatal(err)
	}
	before := st.Changes(0).Seq

	h, err := st.Purge("project", "prj_one")
	if err != nil || !h.Removed || !h.Purged || h.Size != 0 || h.Meta != nil {
		t.Fatalf("purge: %+v %v", h, err)
	}
	ch := st.Changes(before)
	if len(ch.Items) != 1 || !ch.Items[0].Purged || !ch.Items[0].Removed {
		t.Fatalf("PCs aren't told: %+v", ch.Items)
	}
	vs, _ := st.Versions("project", "prj_one")
	if len(vs) != 1 {
		t.Fatalf("old versions kept: %+v", vs)
	}
	if _, err := os.Stat(st.blobPath("project", "prj_one", 1)); err == nil {
		t.Fatal("an old version's blob is still on disk")
	}
	if _, err := os.Stat(st.refsPath("project", "prj_one", 1)); err == nil {
		t.Fatal("an old version's chunk list is still on disk")
	}
	got, blob, err := st.Get("project", "prj_one", 0)
	if err != nil || len(blob) != 0 || !got.Purged || !got.Removed {
		t.Fatalf("reading the marker: %+v %q %v", got, blob, err)
	}

	// nothing can put it back
	if _, err := st.PutRefs(dev, "project", "prj_one", 2, []byte("m3"), nil, []string{mine}); !errors.Is(err, ErrRemoved) {
		t.Fatalf("upload after delete: %v", err)
	}
	if _, err := st.Put(dev, "session", "s", 0, []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoved("project", "prj_one", false); !errors.Is(err, ErrPurged) {
		t.Fatalf("bring back after delete: %v", err)
	}
	if n := st.PurgeRemoved(time.Now().Add(1000 * 24 * time.Hour)); n != 0 {
		t.Fatal("the marker expired")
	}
	if _, _, err := st.Get("project", "prj_one", 0); err != nil {
		t.Fatal("the marker disappeared")
	}

	// the space comes back, but only for chunks nothing else uses
	if n, err := st.GCChunks(0); err != nil || n != 1 {
		t.Fatalf("freed %d chunks, %v", n, err)
	}
	if missing := st.MissingChunks([]string{shared, other}); len(missing) != 0 {
		t.Fatalf("another project lost chunks: %v", missing)
	}
	if missing := st.MissingChunks([]string{mine}); len(missing) != 1 {
		t.Fatal("the deleted project's chunk is still stored")
	}

	// deleting twice, or something that isn't there
	if _, err := st.Purge("project", "prj_one"); err != nil {
		t.Fatalf("deleting again: %v", err)
	}
	if _, err := st.Purge("project", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a missing item: %v", err)
	}
}

// An item that was only removed (not yet deleted) can be deleted for good too.
func TestPurgeAfterRemove(t *testing.T) {
	st, _ := Open(t.TempDir())
	dev := proto.Device{ID: "d1", Name: "pc"}
	st.Put(dev, "session", "local_x", 0, []byte("v1"), []byte("meta"))
	rem, _ := st.SetRemoved("session", "local_x", true)
	h, err := st.Purge("session", "local_x")
	if err != nil || !h.Purged || !h.RemovedAt.Equal(rem.RemovedAt) {
		t.Fatalf("%+v %v", h, err)
	}
}

// A project deleted for good can start over under the same name; a chat can't.
func TestReviveLetsAProjectStartOver(t *testing.T) {
	st, _ := Open(t.TempDir())
	dev := proto.Device{ID: "d1", Name: "pc"}
	c := "k" + strings.Repeat("d", 64)
	st.StoreChunk(c, []byte("x"))
	st.PutRefs(dev, "project", "prj_x", 0, []byte("m"), []byte("meta"), []string{c})
	st.Put(dev, "session", "local_x", 0, []byte("chat"), nil)

	if err := st.Revive("project", "prj_x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reviving something that wasn't deleted for good: %v", err)
	}
	st.SetRemoved("project", "prj_x", true)
	if err := st.Revive("project", "prj_x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reviving something only removed: %v", err)
	}
	if _, err := st.Purge("project", "prj_x"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRefs(dev, "project", "prj_x", 1, []byte("m"), nil, nil); !errors.Is(err, ErrRemoved) {
		t.Fatalf("upload before reviving: %v", err)
	}
	if err := st.Revive("project", "prj_x"); err != nil {
		t.Fatal(err)
	}
	h, err := st.PutRefs(dev, "project", "prj_x", 0, []byte("m2"), nil, nil)
	if err != nil || h.Version != 1 || h.Removed || h.Purged {
		t.Fatalf("a fresh start: %+v %v", h, err)
	}
	if err := st.Revive("project", "prj_nothing"); err != nil {
		t.Fatalf("reviving what isn't there: %v", err)
	}

	// a deleted chat stays deleted: reviving it would let a PC that still has it upload it again
	st.Purge("session", "local_x")
	if err := st.Revive("session", "local_x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("revived a chat: %v", err)
	}
	if _, err := st.Put(dev, "session", "local_x", 1, []byte("again"), nil); !errors.Is(err, ErrRemoved) {
		t.Fatalf("a deleted chat can be uploaded again: %v", err)
	}
}
