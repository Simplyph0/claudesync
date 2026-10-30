// Package client talks to a ClaudeSync server: LAN address first (self-signed
// certificate pinned by fingerprint), then the Cloudflare Tunnel address.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Simplyph0/claudesync/internal/proto"
	"github.com/Simplyph0/claudesync/internal/server"
)

var (
	ErrConflict     = errors.New("conflict")
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
	ErrPinMismatch  = errors.New("server certificate does not match the fingerprint you confirmed")
	ErrRemoved      = errors.New("removed from the server backup")
	ErrServerTooOld = errors.New("the server is too old to delete for good; it only removed the item")
)

// APIError is an error message returned by the server.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("server: %s (HTTP %d)", e.Msg, e.Status) }

type Client struct {
	LANURL    string // e.g. https://192.168.1.10:8443
	TunnelURL string // e.g. https://sync.example.com
	Pin       string // fingerprint of the LAN certificate
	Token     string

	lanHTTP, tunHTTP *http.Client
	mu               sync.Mutex
	preferTunnel     time.Time // skip LAN until this time after it failed
	lastRoute        string
}

func New(lan, tunnel, pin, token string) *Client {
	c := &Client{LANURL: strings.TrimRight(lan, "/"), TunnelURL: strings.TrimRight(tunnel, "/"), Pin: pin, Token: token}
	c.lanHTTP = &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // replaced by the fingerprint check below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || server.Fingerprint(raw[0]) != c.Pin {
					return ErrPinMismatch
				}
				return nil
			},
		},
	}}
	c.tunHTTP = &http.Client{Timeout: 10 * time.Minute}
	return c
}

// Route reports which address the last successful request used.
func (c *Client) Route() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRoute
}

// FetchFingerprint connects to a LAN address without trusting it and returns
// the certificate fingerprint for the user to confirm.
func FetchFingerprint(lanURL string) (string, error) {
	u, err := url.Parse(lanURL)
	if err != nil {
		return "", err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "8443")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("server sent no certificate")
	}
	return server.Fingerprint(certs[0].Raw), nil
}

type route struct {
	name string
	base string
	hc   *http.Client
}

func (c *Client) routes() []route {
	c.mu.Lock()
	skipLAN := time.Now().Before(c.preferTunnel)
	c.mu.Unlock()
	var rs []route
	if c.LANURL != "" && c.Pin != "" && !skipLAN {
		rs = append(rs, route{"LAN", c.LANURL, c.lanHTTP})
	}
	if c.TunnelURL != "" {
		rs = append(rs, route{"tunnel", c.TunnelURL, c.tunHTTP})
	}
	if len(rs) == 0 && c.LANURL != "" && c.Pin != "" {
		rs = append(rs, route{"LAN", c.LANURL, c.lanHTTP})
	}
	return rs
}

// do sends a request, falling back from LAN to tunnel on network errors.
func (c *Client) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	rs := c.routes()
	if len(rs) == 0 {
		return nil, errors.New("no server address configured")
	}
	var lastErr error
	for _, r := range rs {
		req, err := http.NewRequestWithContext(ctx, method, r.base+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if c.Token != "" {
			req.Header.Set(proto.HeaderAuth, "Bearer "+c.Token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := r.hc.Do(req)
		if err != nil {
			if errors.Is(err, ErrPinMismatch) {
				return nil, ErrPinMismatch
			}
			lastErr = fmt.Errorf("%s (%s): %w", r.name, r.base, err)
			if r.name == "LAN" {
				c.mu.Lock()
				c.preferTunnel = time.Now().Add(2 * time.Minute)
				c.mu.Unlock()
			}
			continue
		}
		c.mu.Lock()
		c.lastRoute = r.name
		c.mu.Unlock()
		return resp, nil
	}
	return nil, lastErr
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	resp, err := c.do(ctx, method, path, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	var e proto.ErrorResponse
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(b))
		if e.Error == "" {
			e.Error = resp.Status
		}
	}
	apiErr := &APIError{Status: resp.StatusCode, Msg: e.Error}
	switch resp.StatusCode {
	case http.StatusConflict:
		return fmt.Errorf("%w: %v", ErrConflict, apiErr)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %v", ErrNotFound, apiErr)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %v", ErrUnauthorized, apiErr)
	case http.StatusGone:
		return fmt.Errorf("%w: %v", ErrRemoved, apiErr)
	}
	return apiErr
}

func (c *Client) Info(ctx context.Context) (proto.ServerInfo, error) {
	var out proto.ServerInfo
	return out, c.call(ctx, "GET", "/api/info", nil, &out)
}

func (c *Client) Login(ctx context.Context, password, deviceName string) (proto.LoginResponse, error) {
	var out proto.LoginResponse
	err := c.call(ctx, "POST", "/api/login", proto.LoginRequest{Password: password, DeviceName: deviceName}, &out)
	if err == nil {
		c.Token = out.Token
	}
	return out, err
}

func (c *Client) ChangePassword(ctx context.Context, old, pw string) error {
	return c.call(ctx, "POST", "/api/password", proto.PasswordChange{Old: old, New: pw}, nil)
}

func (c *Client) Settings(ctx context.Context) (proto.Settings, error) {
	var out proto.Settings
	return out, c.call(ctx, "GET", "/api/settings", nil, &out)
}

func (c *Client) SetSettings(ctx context.Context, s proto.Settings) error {
	return c.call(ctx, "PUT", "/api/settings", s, nil)
}

func (c *Client) Vault(ctx context.Context) (proto.Vault, error) {
	var out proto.Vault
	return out, c.call(ctx, "GET", "/api/vault", nil, &out)
}

func (c *Client) SetVault(ctx context.Context, v proto.Vault) error {
	return c.call(ctx, "PUT", "/api/vault", v, nil)
}

func (c *Client) Devices(ctx context.Context) ([]proto.Device, error) {
	var out []proto.Device
	return out, c.call(ctx, "GET", "/api/devices", nil, &out)
}

func (c *Client) RevokeDevice(ctx context.Context, id string) error {
	return c.call(ctx, "DELETE", "/api/devices/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Changes(ctx context.Context, since int64) (proto.ItemList, error) {
	var out proto.ItemList
	return out, c.call(ctx, "GET", "/api/items?since="+strconv.FormatInt(since, 10), nil, &out)
}

func (c *Client) Versions(ctx context.Context, kind, id string) ([]proto.ItemHeader, error) {
	var out []proto.ItemHeader
	return out, c.call(ctx, "GET", "/api/items/"+kind+"/"+id+"/versions", nil, &out)
}

// Get downloads a version's blob (v <= 0 for the latest).
func (c *Client) Get(ctx context.Context, kind, id string, v int) (proto.ItemHeader, []byte, error) {
	var h proto.ItemHeader
	path := "/api/items/" + kind + "/" + id
	if v > 0 {
		path += "?v=" + strconv.Itoa(v)
	}
	resp, err := c.do(ctx, "GET", path, nil, nil)
	if err != nil {
		return h, nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return h, nil, err
	}
	_ = json.Unmarshal([]byte(resp.Header.Get("X-Item-Header")), &h)
	b, err := io.ReadAll(resp.Body)
	return h, b, err
}

// Put uploads a blob as the next version after base, chunking large blobs.
func (c *Client) Put(ctx context.Context, kind, id string, base int, blob, meta []byte) (proto.ItemHeader, error) {
	var h proto.ItemHeader
	hdr := map[string]string{proto.HeaderBaseVersion: strconv.Itoa(base)}
	if len(meta) > 0 {
		hdr[proto.HeaderMeta] = base64.StdEncoding.EncodeToString(meta)
	}
	path := "/api/items/" + kind + "/" + id
	if len(blob) > proto.MaxDirectUpload {
		var up proto.UploadStart
		if err := c.call(ctx, "POST", "/api/uploads", nil, &up); err != nil {
			return h, err
		}
		for i, n := 0, 0; i < len(blob); i, n = i+proto.MaxDirectUpload, n+1 {
			chunk := blob[i:min(i+proto.MaxDirectUpload, len(blob))]
			resp, err := c.do(ctx, "PUT", fmt.Sprintf("/api/uploads/%s/%d", up.UploadID, n), chunk, nil)
			if err != nil {
				return h, err
			}
			err = checkStatus(resp)
			resp.Body.Close()
			if err != nil {
				return h, err
			}
		}
		path = "/api/uploads/" + up.UploadID + "/commit/" + kind + "/" + id
		blob = nil
		resp, err := c.do(ctx, "POST", path, nil, hdr)
		return decodeHeader(resp, err)
	}
	resp, err := c.do(ctx, "PUT", path, blob, hdr)
	return decodeHeader(resp, err)
}

func decodeHeader(resp *http.Response, err error) (proto.ItemHeader, error) {
	var h proto.ItemHeader
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return h, err
	}
	return h, json.NewDecoder(resp.Body).Decode(&h)
}

// Remove strikes an item from the server backup (kept for
// proto.RemovedHoldDays so it can be brought back).
func (c *Client) Remove(ctx context.Context, kind, id string) (proto.ItemHeader, error) {
	var h proto.ItemHeader
	return h, c.call(ctx, "DELETE", "/api/items/"+kind+"/"+id, nil, &h)
}

// Purge deletes an item from the server for good (every version). Nothing on
// any PC is touched, and the item can't be brought back or uploaded again.
func (c *Client) Purge(ctx context.Context, kind, id string) (proto.ItemHeader, error) {
	var h proto.ItemHeader
	if err := c.call(ctx, "DELETE", "/api/items/"+kind+"/"+id+"?forever=1", nil, &h); err != nil {
		return h, err
	}
	if !h.Purged { // a server from before deleting for good ignores "forever" and only removes
		return h, ErrServerTooOld
	}
	return h, nil
}

// BringBack undoes Remove.
func (c *Client) BringBack(ctx context.Context, kind, id string) (proto.ItemHeader, error) {
	var h proto.ItemHeader
	return h, c.call(ctx, "POST", "/api/items/"+kind+"/"+id+"/restore", nil, &h)
}

// MissingChunks asks which chunks the server doesn't have yet.
func (c *Client) MissingChunks(ctx context.Context, ids []string) ([]string, error) {
	var missing []string
	for i := 0; i < len(ids); i += 20000 {
		var out proto.ChunkList
		if err := c.call(ctx, "POST", "/api/chunks/missing", proto.ChunkList{IDs: ids[i:min(i+20000, len(ids))]}, &out); err != nil {
			return nil, err
		}
		missing = append(missing, out.IDs...)
	}
	return missing, nil
}

func (c *Client) PutChunk(ctx context.Context, id string, blob []byte) error {
	resp, err := c.do(ctx, "PUT", "/api/chunks/"+id, blob, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp)
}

func (c *Client) GetChunk(ctx context.Context, id string) ([]byte, error) {
	resp, err := c.do(ctx, "GET", "/api/chunks/"+id, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// PutManifest uploads a project version with the chunks it uses.
func (c *Client) PutManifest(ctx context.Context, kind, id string, up proto.ManifestUpload) (proto.ItemHeader, error) {
	var h proto.ItemHeader
	return h, c.call(ctx, "PUT", "/api/manifests/"+kind+"/"+id, up, &h)
}
