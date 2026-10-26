package client

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

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
