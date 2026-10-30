package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Simplyph0/claudesync/internal/proto"
	"github.com/Simplyph0/claudesync/internal/server"
)

func TestPinningAndFallback(t *testing.T) {
	st, err := server.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPassword("password123"); err != nil {
		t.Fatal(err)
	}
	h := server.New(st).Handler()
	lan := httptest.NewTLSServer(h)
	defer lan.Close()
	tunnel := httptest.NewServer(h)
	defer tunnel.Close()
	ctx := context.Background()

	fp, err := FetchFingerprint(lan.URL)
	if err != nil {
		t.Fatal(err)
	}
	if fp != server.Fingerprint(lan.Certificate().Raw) {
		t.Fatal("fetched fingerprint differs from the server certificate")
	}

	// Pinned LAN works and is preferred.
	c := New(lan.URL, tunnel.URL, fp, "")
	if _, err := c.Login(ctx, "password123", "pc"); err != nil {
		t.Fatal(err)
	}
	if c.Route() != "LAN" {
		t.Fatalf("route = %s, want LAN", c.Route())
	}

	// A different certificate on the LAN address is refused, not bypassed.
	bad := New(lan.URL, tunnel.URL, "0000 0000", "")
	if _, err := bad.Info(ctx); !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("wrong pin: got %v, want ErrPinMismatch", err)
	}

	// LAN unreachable (away from home): falls back to the tunnel.
	away := New("https://127.0.0.1:1", tunnel.URL, fp, c.Token)
	if _, err := away.Devices(ctx); err != nil {
		t.Fatal(err)
	}
	if away.Route() != "tunnel" {
		t.Fatalf("route = %s, want tunnel", away.Route())
	}

	// Wrong password is rejected; revoked devices are signed out.
	if _, err := New("", tunnel.URL, "", "").Login(ctx, "nope", "x"); err == nil {
		t.Fatal("wrong password accepted")
	}
	devs, _ := c.Devices(ctx)
	if err := c.RevokeDevice(ctx, devs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Devices(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked device still accepted: %v", err)
	}
}

// An old server ignores "forever" and only removes; saying "deleted" would be a lie.
func TestPurgeNeedsANewServer(t *testing.T) {
	st, err := server.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.SetPassword("password123")
	ts := httptest.NewServer(server.New(st).Handler())
	defer ts.Close()
	ctx := context.Background()
	c := New("", ts.URL, "", "")
	resp, err := c.Login(ctx, "password123", "pc")
	if err != nil {
		t.Fatal(err)
	}
	c.Token = resp.Token
	if _, err := c.Put(ctx, "session", "local_x", 0, []byte("v1"), nil); err != nil {
		t.Fatal(err)
	}
	h, err := c.Purge(ctx, "session", "local_x")
	if err != nil || !h.Purged {
		t.Fatalf("purge on a current server: %+v %v", h, err)
	}

	// the same call against a server that doesn't know "forever"
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"session","id":"local_x","version":1,"removed":true}`))
	}))
	defer old.Close()
	_, err = New("", old.URL, "", "tok").Purge(ctx, "session", "local_x")
	if !errors.Is(err, ErrServerTooOld) {
		t.Fatalf("an old server's answer was taken as success: %v", err)
	}
}

func TestReviveAndPurgedErrors(t *testing.T) {
	st, err := server.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.SetPassword("password123")
	ts := httptest.NewServer(server.New(st).Handler())
	defer ts.Close()
	ctx := context.Background()
	c := New("", ts.URL, "", "")
	resp, err := c.Login(ctx, "password123", "pc")
	if err != nil {
		t.Fatal(err)
	}
	c.Token = resp.Token
	if _, err := c.PutManifest(ctx, "project", "prj_x", proto.ManifestUpload{Blob: []byte("m"), Refs: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Purge(ctx, "project", "prj_x"); err != nil {
		t.Fatal(err)
	}
	_, err = c.PutManifest(ctx, "project", "prj_x", proto.ManifestUpload{Base: 1, Blob: []byte("m"), Refs: []string{}})
	if !errors.Is(err, ErrPurged) || !errors.Is(err, ErrRemoved) {
		t.Fatalf("an upload after deleting for good: %v", err)
	}
	if err := c.Revive(ctx, "project", "prj_x"); err != nil {
		t.Fatal(err)
	}
	if h, err := c.PutManifest(ctx, "project", "prj_x", proto.ManifestUpload{Blob: []byte("m"), Refs: []string{}}); err != nil || h.Version != 1 {
		t.Fatalf("a new copy: %+v %v", h, err)
	}
	// only removed (not deleted for good) is a different error
	c.Remove(ctx, "project", "prj_x")
	_, err = c.PutManifest(ctx, "project", "prj_x", proto.ManifestUpload{Base: 1, Blob: []byte("m"), Refs: []string{}})
	if errors.Is(err, ErrPurged) || !errors.Is(err, ErrRemoved) {
		t.Fatalf("a plain removal looked like a deletion: %v", err)
	}

	// a server from before reviving doesn't know the request
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if err := New("", old.URL, "", "tok").Revive(ctx, "project", "prj_x"); !errors.Is(err, ErrServerTooOld) {
		t.Fatalf("an old server's 404: %v", err)
	}
}
