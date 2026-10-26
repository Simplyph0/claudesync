package server

import (
	"errors"
	"testing"
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
		if _, err := st.Put(dev, "session", "local_x", v, []byte{byte(v)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Put(dev, "session", "local_x", 2, []byte("stale")); !errors.Is(err, ErrConflict) {
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
	if _, err := st2.Put(dev, "../evil", "x", 0, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("path traversal in kind accepted")
	}
}
