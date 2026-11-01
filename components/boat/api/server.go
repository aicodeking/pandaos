package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	SSHUser          string
	SSHJump          string // optional user@host:port bastion returned to clients
	DesktopURL       string // base URL of the desktop gateway; empty disables /desktop
	DesktopKey       []byte
	DesktopTTL       time.Duration
	Images           []string
	AllowPrivateHook bool // permit webhook targets on loopback/private nets (dev/tests)
	MaxFileBytes     int64
	JumpKeysFile     string // authorized_keys for the ssh jump user; empty disables
}

type server struct {
	store   *Store
	keys    *Keyring
	boatd   *boatdClient
	limiter *limiter
	audit   *auditLog
	hooks   *dispatcher
	cfg     Config
	mux     *http.ServeMux
	jumpMu  sync.Mutex
}

// ---- scopes and roles ----

var allScopes = []string{"vms:read", "vms:write", "exec", "admin"}

func roleScopes(role string) []string {
	switch role {
	case "owner", "admin":
		return allScopes
	case "member":
		return []string{"vms:read", "vms:write", "exec"}
	}
	return nil
}

func validRole(r string) bool { return r == "owner" || r == "admin" || r == "member" }

type principal struct {
	Key    *APIKey
	User   *User
	Team   *Team
	Role   string
	Scopes []string // key scopes ∩ role scopes
}

func (p *principal) has(scope string) bool { return slices.Contains(p.Scopes, scope) }

func effectiveScopes(keyScopes []string, role string) []string {
	rs := roleScopes(role)
	if len(keyScopes) == 0 {
		return rs
	}
	var out []string
	for _, s := range keyScopes {
		if slices.Contains(rs, s) {
			out = append(out, s)
		}
	}
	return out
}

// authenticate resolves a bearer token to a principal, or nil.
func (s *server) authenticate(header string) *principal {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return nil
	}
	id, secret, ok := ParseToken(strings.TrimSpace(tok))
	if !ok {
		return nil
	}
	st := s.store.Snapshot()
	k := st.Keys[id]
	if k == nil {
		return nil
	}
	got := s.keys.HashSecret(k.Salt, secret)
	if subtle.ConstantTimeCompare([]byte(got), []byte(k.Hash)) != 1 {
		return nil
	}
	u, t := st.Users[k.UserID], st.Teams[k.TeamID]
	if u == nil || t == nil {
		return nil
	}
	role := t.role(u.ID)
	if role == "" {
		return nil
	}
	return &principal{Key: k, User: u, Team: t, Role: role, Scopes: effectiveScopes(k.Scopes, role)}
}

// ---- rate limiting: token bucket per key ----

type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// allow takes one token; when empty it reports how long until one is available.
func (l *limiter) allow(key string) (bool, time.Duration) {
	if l.rate <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// ---- audit log ----

type auditLog struct {
	mu sync.Mutex
	w  io.Writer
}

type auditEntry struct {
	Time      time.Time `json:"ts"`
	RequestID string    `json:"request_id"`
	KeyID     string    `json:"key_id,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	TeamID    string    `json:"team_id,omitempty"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	RemoteIP  string    `json:"remote_ip"`
	Forwarded string    `json:"forwarded_for,omitempty"`
	Millis    int64     `json:"ms"`
}

func (a *auditLog) write(e auditEntry) {
	if a == nil || a.w == nil {
		return
	}
	b, _ := json.Marshal(e)
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.w.Write(append(b, '\n')); err != nil {
		log.Printf("audit write: %v", err)
	}
}

// ---- request plumbing ----

type ctxKey struct{}

type reqInfo struct {
	id string
	p  *principal
}

func info(r *http.Request) *reqInfo { return r.Context().Value(ctxKey{}).(*reqInfo) }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ServeHTTP adds a request id, then audits every mutating or failed-auth request.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rid := r.Header.Get("X-Request-Id")
	if rid == "" || len(rid) > 64 {
		rid = "req_" + randHex(8)
	}
	ri := &reqInfo{id: rid}
	w.Header().Set("X-Request-Id", rid)
	sw := &statusWriter{ResponseWriter: w}
	s.mux.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ri)))
	if r.Method == http.MethodGet && sw.status != http.StatusUnauthorized && sw.status != http.StatusForbidden {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	e := auditEntry{Time: start.UTC(), RequestID: rid, Method: r.Method, Path: r.URL.Path, Status: sw.status,
		RemoteIP: ip, Forwarded: r.Header.Get("X-Forwarded-For"), Millis: time.Since(start).Milliseconds()}
	if ri.p != nil {
		e.KeyID, e.UserID, e.TeamID = ri.p.Key.ID, ri.p.User.ID, ri.p.Team.ID
	}
	s.audit.write(e)
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, p *principal)

// route registers an authenticated, rate-limited handler requiring scope ("" = any key).
func (s *server) route(pattern, scope string, h handlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		p := s.authenticate(r.Header.Get("Authorization"))
		if p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="boat"`)
			writeErr(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		info(r).p = p
		if ok, wait := s.limiter.allow(p.Key.ID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
			return
		}
		if scope != "" && !p.has(scope) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "key lacks scope "+scope)
			return
		}
		h(w, r, p)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": msg, "request_id": info(r).id,
	}})
}

// writeUpstream maps a boatd failure to a client error without leaking internals.
func writeUpstream(w http.ResponseWriter, r *http.Request, err error) {
	var be *boatdError
	if errors.As(err, &be) {
		switch {
		case be.Status == http.StatusNotFound:
			writeErr(w, r, 404, "not_found", "vm not found")
		case be.Status == http.StatusTooManyRequests:
			writeErr(w, r, 429, "quota_exceeded", be.message())
		case be.Status == http.StatusConflict:
			writeErr(w, r, 409, "conflict", be.message())
		case be.Status/100 == 4:
			writeErr(w, r, be.Status, "bad_request", be.message())
		default:
			log.Printf("%s: %v", info(r).id, err)
			writeErr(w, r, http.StatusBadGateway, "upstream_error", "vm manager error")
		}
		return
	}
	log.Printf("%s: boatd: %v", info(r).id, err)
	writeErr(w, r, http.StatusBadGateway, "upstream_unavailable", "vm manager unavailable")
}

const maxJSONBody = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}
