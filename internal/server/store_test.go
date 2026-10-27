package server

import (
	"errors"
	"testing"
	"time"

	"github.com/Simplyph0/claudesync/internal/proto"
)

func TestRetentionAndPassword(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPassword("first-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Login("nope", "pc"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("bad password accepted: %v", err)
	}
	resp, err := st.Login("first-password", "pc")
	if err != nil {
		t.Fatal(err)
	}
	dev, ok := st.Authenticate(resp.Token)
	if !ok {
		t.Fatal("token rejected")
	}

	if err := st.UpdateConfig(func(c *Config) { c.Retention = 3 }); err != nil {
		t.Fatal(err)
	}
	for v := 0; v < 5; v++ {
		if _, err := st.Put(dev, "session", "local_x", v, []byte{byte(v)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Put(dev, "session", "local_x", 2, []byte("stale"), nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale base accepted: %v", err)
	}
	vs, _ := st.Versions("session", "local_x")
	if len(vs) != 3 || vs[0].Version != 3 {
		t.Fatalf("kept versions %+v, want 3..5", vs)
	}
	if _, _, err := st.Get("session", "local_x", 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("pruned version still readable")
	}

	// Password change from a client keeps existing devices signed in.
	if err := st.ChangePassword("wrong", "second-password"); !errors.Is(err, ErrBadPassword) {
		t.Fatal("changed password without the old one")
	}
	if err := st.ChangePassword("first-password", "second-password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Authenticate(resp.Token); !ok {
		t.Fatal("device signed out by password change")
	}
	if _, err := st.Login("second-password", "pc2"); err != nil {
		t.Fatal(err)
	}

	// Data survives a restart.
	st2, err := Open(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if _, b, err := st2.Get("session", "local_x", 0); err != nil || b[0] != 4 {
		t.Fatalf("latest after reopen: %v %v", b, err)
	}
	if _, err := st2.Put(dev, "../evil", "x", 0, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("path traversal in kind accepted")
	}
}

func TestRemoveAndBringBack(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dev := proto.Device{ID: "d1", Name: "pc"}
	if _, err := st.Put(dev, "session", "local_x", 0, []byte("v1"), []byte("meta")); err != nil {
		t.Fatal(err)
	}
	before := st.Changes(0).Seq
	h, err := st.SetRemoved("session", "local_x", true)
	if err != nil || !h.Removed {
		t.Fatalf("remove: %+v %v", h, err)
	}
	ch := st.Changes(before)
	if len(ch.Items) != 1 || !ch.Items[0].Removed || string(ch.Items[0].Meta) != "meta" {
		t.Fatalf("PCs aren't told about the removal: %+v", ch.Items)
	}
	if _, err := st.Put(dev, "session", "local_x", 1, []byte("v2"), nil); !errors.Is(err, ErrRemoved) {
		t.Fatalf("upload to a removed item: %v", err)
	}
	if _, _, err := st.Get("session", "local_x", 0); err != nil {
		t.Fatal("removed item unreadable during the hold period")
	}
	if n := st.PurgeRemoved(time.Now()); n != 0 {
		t.Fatal("purged before the hold period ended")
	}
	if _, err := st.SetRemoved("session", "local_x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(dev, "session", "local_x", 1, []byte("v2"), nil); err != nil {
		t.Fatalf("upload after bringing back: %v", err)
	}
	st.SetRemoved("session", "local_x", true)
	if n := st.PurgeRemoved(time.Now().Add((proto.RemovedHoldDays + 1) * 24 * time.Hour)); n != 1 {
		t.Fatal("not purged after the hold period")
	}
	if _, _, err := st.Get("session", "local_x", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("purged item still readable")
	}
}
