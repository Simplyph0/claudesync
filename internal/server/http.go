package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Simplyph0/claudesync/internal/proto"
)

const maxBody = proto.MaxDirectUpload + 1<<20

// Server wires the store to HTTP.
type Server struct {
	store   *Store
	limiter *limiter
	Logf    func(format string, args ...any)
}

func New(store *Store) *Server {
	return &Server{store: store, limiter: newLimiter(), Logf: log.Printf}
}

type ctxKey struct{}

func deviceFrom(r *http.Request) proto.Device {
	d, _ := r.Context().Value(ctxKey{}).(proto.Device)
	return d
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/login", s.login)
	mux.Handle("POST /api/password", s.auth(s.changePassword))
	mux.Handle("GET /api/settings", s.auth(s.getSettings))
	mux.Handle("PUT /api/settings", s.auth(s.putSettings))
	mux.Handle("GET /api/vault", s.auth(s.getVault))
	mux.Handle("PUT /api/vault", s.auth(s.putVault))
	mux.Handle("GET /api/devices", s.auth(s.listDevices))
	mux.Handle("DELETE /api/devices/{id}", s.auth(s.revokeDevice))
	mux.Handle("GET /api/items", s.auth(s.listItems))
	mux.Handle("GET /api/items/{kind}/{id}", s.auth(s.getItem))
	mux.Handle("GET /api/items/{kind}/{id}/versions", s.auth(s.itemVersions))
	mux.Handle("PUT /api/items/{kind}/{id}", s.auth(s.putItem))
	mux.Handle("DELETE /api/items/{kind}/{id}", s.auth(s.removeItem))
	mux.Handle("PUT /api/manifests/{kind}/{id}", s.auth(s.putManifest))
	mux.Handle("POST /api/chunks/missing", s.auth(s.missingChunks))
	mux.Handle("PUT /api/chunks/{id}", s.auth(s.putChunkBlob))
	mux.Handle("GET /api/chunks/{id}", s.auth(s.getChunkBlob))
	mux.Handle("POST /api/items/{kind}/{id}/restore", s.auth(s.restoreItem))
	mux.Handle("POST /api/items/{kind}/{id}/revive", s.auth(s.reviveItem))
	mux.Handle("POST /api/uploads", s.auth(s.startUpload))
	mux.Handle("PUT /api/uploads/{upload}/{n}", s.auth(s.putChunk))
	mux.Handle("POST /api/uploads/{upload}/commit/{kind}/{id}", s.auth(s.commitUpload))
	return mux
}

func (s *Server) auth(fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get(proto.HeaderAuth), "Bearer ")
		dev, valid := s.store.Authenticate(tok)
		if !ok || !valid {
			writeErr(w, http.StatusUnauthorized, "not logged in (device revoked or token invalid)")
			return
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, dev)))
	})
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Config()
	writeJSON200(w, proto.ServerInfo{Version: proto.Version, Retention: cfg.Retention, HasVault: cfg.Vault != nil})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait := s.limiter.blocked(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var req proto.LoginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	resp, err := s.store.Login(req.Password, req.DeviceName)
	if errors.Is(err, ErrBadPassword) {
		s.limiter.fail(ip)
		s.Logf("failed login from %s", ip)
	} else if err == nil {
		s.limiter.reset(ip)
		s.Logf("device %q logged in from %s", req.DeviceName, ip)
	}
	if !s.check(w, err) {
		return
	}
	writeJSON200(w, resp)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limiter.blocked(ip) > 0 {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
		return
	}
	var req proto.PasswordChange
	if !decodeBody(w, r, &req) {
		return
	}
	err := s.store.ChangePassword(req.Old, req.New)
	if errors.Is(err, ErrBadPassword) {
		s.limiter.fail(ip)
	}
	if !s.check(w, err) {
		return
	}
	s.Logf("server password changed by device %q", deviceFrom(r).Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON200(w, proto.Settings{Retention: s.store.Config().Retention})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req proto.Settings
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Retention < 1 || req.Retention > 10000 {
		writeErr(w, http.StatusBadRequest, "retention must be between 1 and 10000")
		return
	}
	if !s.check(w, s.store.UpdateConfig(func(c *Config) { c.Retention = req.Retention })) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getVault(w http.ResponseWriter, r *http.Request) {
	v := s.store.Vault()
	if v == nil {
		writeErr(w, http.StatusNotFound, "no sync passphrase set yet")
		return
	}
	writeJSON200(w, v)
}

func (s *Server) putVault(w http.ResponseWriter, r *http.Request) {
	var v proto.Vault
	if !decodeBody(w, r, &v) {
		return
	}
	if !s.check(w, s.store.SetVault(v)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON200(w, s.store.Devices())
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	if !s.check(w, s.store.RevokeDevice(r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	writeJSON200(w, s.store.Changes(since))
}

func (s *Server) getItem(w http.ResponseWriter, r *http.Request) {
	v, _ := strconv.Atoi(r.URL.Query().Get("v"))
	h, blob, err := s.store.Get(r.PathValue("kind"), r.PathValue("id"), v)
	if !s.check(w, err) {
		return
	}
	hb, _ := json.Marshal(h)
	w.Header().Set("X-Item-Header", string(hb))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(blob)
}

func (s *Server) itemVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.store.Versions(r.PathValue("kind"), r.PathValue("id"))
	if !s.check(w, err) {
		return
	}
	writeJSON200(w, vs)
}

func (s *Server) putItem(w http.ResponseWriter, r *http.Request) {
	base, err := strconv.Atoi(r.Header.Get(proto.HeaderBaseVersion))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing "+proto.HeaderBaseVersion)
		return
	}
	blob, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large; use chunked upload")
		return
	}
	h, err := s.store.Put(deviceFrom(r), r.PathValue("kind"), r.PathValue("id"), base, blob, metaHeader(r))
	if !s.check(w, err) {
		return
	}
	writeJSON200(w, h)
}

func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.StartUpload()
	if !s.check(w, err) {
		return
	}
	writeJSON200(w, proto.UploadStart{UploadID: id})
}

func (s *Server) putChunk(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chunk number")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "chunk too large")
		return
	}
	if !s.check(w, s.store.PutChunk(r.PathValue("upload"), n, data)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) commitUpload(w http.ResponseWriter, r *http.Request) {
	base, err := strconv.Atoi(r.Header.Get(proto.HeaderBaseVersion))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing "+proto.HeaderBaseVersion)
		return
	}
	h, err := s.store.FinishUpload(deviceFrom(r), r.PathValue("upload"), r.PathValue("kind"), r.PathValue("id"), base, metaHeader(r))
	if !s.check(w, err) {
		return
	}
	writeJSON200(w, h)
}

// check writes an error response for err and reports whether err was nil.
func (s *Server) check(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrMissingChunks):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrRemoved), errors.Is(err, ErrPurged):
		writeErr(w, http.StatusGone, err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrBadPassword):
		writeErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrNotInitDone):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		s.Logf("internal error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal server error")
	}
	return false
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON200(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(proto.ErrorResponse{Error: msg})
}

// clientIP trusts Cloudflare's header only when the request came through the
// loopback listener that cloudflared connects to.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			return cf
		}
	}
	return host
}

// limiter blocks an address for a growing time after repeated failed logins.
type limiter struct {
	mu   sync.Mutex
	hits map[string]*hit
}

type hit struct {
	fails int
	until time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string]*hit{}} }

func (l *limiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h := l.hits[ip]; h != nil {
		return time.Until(h.until)
	}
	return 0
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hits[ip]
	if h == nil {
		h = &hit{}
		l.hits[ip] = h
	}
	h.fails++
	if h.fails >= 5 {
		d := time.Minute << min(h.fails-5, 6) // 1 min, doubling, capped at ~1 h
		h.until = time.Now().Add(d)
	}
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, ip)
}

func metaHeader(r *http.Request) []byte {
	b, _ := base64.StdEncoding.DecodeString(r.Header.Get(proto.HeaderMeta))
	return b
}

// removeItem strikes an item from the backup; every PC stops syncing it.
func (s *Server) removeItem(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("forever") == "1" {
		h, err := s.store.Purge(r.PathValue("kind"), r.PathValue("id"))
		if !s.check(w, err) {
			return
		}
		s.Logf("device %q deleted %s from the server for good", deviceFrom(r).Name, h.Key())
		writeJSON200(w, h)
		return
	}
	h, err := s.store.SetRemoved(r.PathValue("kind"), r.PathValue("id"), true)
	if !s.check(w, err) {
		return
	}
	s.Logf("device %q removed %s from the backup", deviceFrom(r).Name, h.Key())
	writeJSON200(w, h)
}

// restoreItem brings a removed item back during its hold period.
func (s *Server) restoreItem(w http.ResponseWriter, r *http.Request) {
	h, err := s.store.SetRemoved(r.PathValue("kind"), r.PathValue("id"), false)
	if !s.check(w, err) {
		return
	}
	s.Logf("device %q brought back %s", deviceFrom(r).Name, h.Key())
	writeJSON200(w, h)
}

// reviveItem lets a project that was deleted for good start over.
func (s *Server) reviveItem(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Revive(r.PathValue("kind"), r.PathValue("id")); !s.check(w, err) {
		return
	}
	s.Logf("device %q started a new copy of %s/%s after it was deleted for good", deviceFrom(r).Name, r.PathValue("kind"), r.PathValue("id"))
	writeJSON200(w, struct{}{})
}

// putManifest stores a project version together with the chunks it uses.
func (s *Server) putManifest(w http.ResponseWriter, r *http.Request) {
	var req proto.ManifestUpload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 96<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid manifest upload")
		return
	}
	if req.Refs == nil {
		req.Refs = []string{}
	}
	h, err := s.store.PutRefs(deviceFrom(r), r.PathValue("kind"), r.PathValue("id"), req.Base, req.Blob, req.Meta, req.Refs)
	if !s.check(w, err) {
		return
	}
	writeJSON200(w, h)
}

func (s *Server) missingChunks(w http.ResponseWriter, r *http.Request) {
	var req proto.ChunkList
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil || len(req.IDs) > 50000 {
		writeErr(w, http.StatusBadRequest, "invalid chunk list")
		return
	}
	missing := s.store.MissingChunks(req.IDs)
	if missing == nil {
		missing = []string{}
	}
	writeJSON200(w, proto.ChunkList{IDs: missing})
}

func (s *Server) putChunkBlob(w http.ResponseWriter, r *http.Request) {
	blob, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxChunkBlob+1))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "chunk too large")
		return
	}
	if !s.check(w, s.store.StoreChunk(r.PathValue("id"), blob)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getChunkBlob(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.LoadChunk(r.PathValue("id"))
	if !s.check(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}
