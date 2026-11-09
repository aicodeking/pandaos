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
	"sync/atomic"
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
	JumpKeysFile     string            // authorized_keys for the ssh jump user; empty disables
	MaxKeysPerTeam   int               // 0 = unlimited
	MaxHooksPerTeam  int               // 0 = unlimited
	AIProxyURL       string            // what VMs use to reach the AI proxy; empty = no proxy
	AIUpstreams      map[string]string // provider base URL overrides (tests)
	PublicURL        string            // public base URL of this API, for signed snapshot links
	MaxImportBytes   int64
	VMDomain         string // port routes are <port>-<vmid>.<VMDomain> (scheme optional, default https)
	VMAPIURL         string // this API as VMs reach it (BOAT_URL in the guest); empty = PublicURL
	PortURLTTL       time.Duration
}

type server struct {
	store      *Store
	keys       *Keyring
	boatd      *boatdClient
	limiter    *limiter  // per key
	teamLim    *limiter  // per team (all keys of a team share it)
	aiLim      *limiter  // AI proxy requests per VM
	streams    *inflight // long responses per team
	uploads    *inflight // uploads and imports per team
	aiInflight *inflight // AI proxy requests per VM
	aiMeter    *aiMeter
	aiClient   *http.Client
	otel       *otelExporter // nil = no instance export
	ctx        context.Context
	keyUse     sync.Map // key id -> *keyUsage
	audit      *auditLog
	hooks      *dispatcher
	cfg        Config
	mux        *http.ServeMux
	jumpMu     sync.Mutex
	// routeTable lists every registered "METHOD /path" (OpenAPI drift test).
	routeTable []string
	observe    func(pattern string, r *http.Request, status int) // OpenAPI drift test
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
	// Operator is the instance operator (set by admin init); only operators
	// create or delete teams. Still limited by the key's admin scope.
	Operator bool
	// VMID: a VM's own token (selftoken.go); it reaches only that VM's routes.
	VMID string
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
	if strings.HasPrefix(tok, selfTokenPrefix) {
		return s.authenticateSelf(strings.TrimSpace(tok))
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
		// Rotation overlap: the previous secret works until PrevUntil.
		if k.PrevUntil == nil || !time.Now().Before(*k.PrevUntil) ||
			subtle.ConstantTimeCompare([]byte(s.keys.HashSecret(k.PrevSalt, secret)), []byte(k.PrevHash)) != 1 {
			return nil
		}
	}
	u, t := st.Users[k.UserID], st.Teams[k.TeamID]
	if u == nil || t == nil {
		return nil
	}
	role := t.role(u.ID)
	if role == "" {
		return nil
	}
	return &principal{Key: k, User: u, Team: t, Role: role, Scopes: effectiveScopes(k.Scopes, role), Operator: u.Operator}
}

// ---- rate limiting: token bucket per key ----

type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
	swept   time.Time
}

const limiterSweepEvery = time.Minute

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// allow takes one token; when empty it reports how long until one is available.
func (l *limiter) allow(key string) (bool, time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) >= limiterSweepEvery {
		l.sweep(now)
	}
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

// sweep drops refilled buckets: a new one would start the same, and keys can be
// client IPs on the public snapshot link. Caller holds l.mu.
func (l *limiter) sweep(now time.Time) {
	l.swept = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
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
	Query     string    `json:"query,omitempty"` // e.g. the file path of a file read
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
	id      string
	p       *principal
	audit   bool   // audit even though it is a GET (sensitive read)
	pattern string // the matched route, for traces and the OpenAPI drift test
	vmOK    bool   // ownVM passed, so a later boatd 404 is about something else
}

// baseCtx outlives requests (env builds).
func (s *server) baseCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// keyUsage is in-memory per process: requests since start and the last use.
type keyUsage struct {
	requests atomic.Int64
	last     atomic.Int64
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
	traceID, parent := traceIDs(r.Header.Get("Traceparent"))
	s.mux.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ri)))
	if s.otel != nil {
		attrs := map[string]any{"http.request.method": r.Method, "http.route": ri.pattern, "http.response.status_code": sw.status, "boat.request_id": rid}
		if ri.p != nil {
			attrs["boat.team_id"] = ri.p.Team.ID
		}
		s.otel.span(traceID, parent, orDefault(ri.pattern, r.Method+" unmatched"), start, time.Now(), sw.status, attrs)
	}
	if s.observe != nil {
		s.observe(ri.pattern, r, sw.status)
	}
	if r.Method == http.MethodGet && !ri.audit && sw.status != http.StatusUnauthorized &&
		sw.status != http.StatusForbidden && sw.status != http.StatusTooManyRequests {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	e := auditEntry{Time: start.UTC(), RequestID: rid, Method: r.Method, Path: auditPath(r), Query: r.URL.RawQuery, Status: sw.status,
		RemoteIP: ip, Forwarded: r.Header.Get("X-Forwarded-For"), Millis: time.Since(start).Milliseconds()}
	if ri.p != nil {
		e.KeyID, e.UserID, e.TeamID = ri.p.Key.ID, ri.p.User.ID, ri.p.Team.ID
	}
	s.audit.write(e)
}

// auditPath keeps a signed link's secret code out of the log.
func auditPath(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/snapshot-links/") {
		return "/v1/snapshot-links/REDACTED/download"
	}
	return r.URL.Path
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, p *principal)

// route registers an authenticated, rate-limited handler requiring scope ("" = any key).
// auditedReads are GETs that disclose data or grant access, so they are
// audited like writes.
var auditedReads = map[string]bool{
	"GET /v1/vms/{id}/files":                    true,
	"GET /v1/vms/{id}/desktop":                  true,
	"GET /v1/vms/{id}/ports/{port}/url":         true,
	"GET /v1/secrets":                           true,
	"GET /v1/keys":                              true,
	"GET /v1/webhooks/{id}/deliveries":          true,
	"GET /v1/vms/{id}/files/list":               true,
	"GET /v1/vms/{id}/files/search":             true,
	"GET /v1/vms/{id}/files/info":               true,
	"GET /v1/vms/{id}/commands/{cid}/logs":      true,
	"GET /v1/vms/{id}/prompts/{pid}/logs":       true,
	"GET /v1/vms/{id}/snapshots/{sid}/download": true,
	"GET /v1/vms/{id}/snapshots/{sid}/shares":   true,
	"GET /v1/ai/credentials":                    true,
	"GET /v1/telemetry":                         true,
}

// streamRoutes hold a connection for as long as the guest or client keeps it open.
var streamRoutes = map[string]bool{
	"POST /v1/vms/{id}/exec":                    true,
	"POST /v1/vms/{id}/prompt":                  true,
	"GET /v1/events":                            true,
	"GET /v1/vms/{id}/commands/{cid}/logs":      true,
	"GET /v1/vms/{id}/prompts/{pid}/logs":       true,
	"GET /v1/vms/{id}/files":                    true,
	"GET /v1/vms/{id}/snapshots/{sid}/download": true,
}

var uploadRoutes = map[string]bool{
	"PUT /v1/vms/{id}/files":          true,
	"POST /v1/snapshots/import":       true,
	"POST /v1/snapshots/import-share": true,
}

// holdSlot takes pattern's per-team slot; false means it already answered 429.
func (s *server) holdSlot(w http.ResponseWriter, r *http.Request, pattern, team string) (func(), bool) {
	lim := s.streams
	switch {
	case uploadRoutes[pattern]:
		lim = s.uploads
	case !streamRoutes[pattern]:
		return func() {}, true
	}
	return s.acquireOr429(w, r, lim, team)
}

func (s *server) acquireOr429(w http.ResponseWriter, r *http.Request, lim *inflight, key string) (func(), bool) {
	release, ok := lim.acquire(key)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeErr(w, r, http.StatusTooManyRequests, "too_many_in_flight", "too many open streams or uploads for this team; wait for one to finish")
	}
	return release, ok
}

// handle registers an unauthenticated route.
func (s *server) handle(pattern string, h http.HandlerFunc) {
	s.routeTable = append(s.routeTable, pattern)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		info(r).pattern = pattern
		h(w, r)
	})
}

func (s *server) route(pattern, scope string, h handlerFunc) {
	s.routeTable = append(s.routeTable, pattern)
	audit := auditedReads[pattern]
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		info(r).audit = audit
		info(r).pattern = pattern
		p := s.authenticate(r.Header.Get("Authorization"))
		if p != nil && p.VMID != "" && !selfAllowed(pattern, r, p.VMID) {
			// Like another team's VM: nothing to see here.
			writeErr(w, r, http.StatusNotFound, "not_found", "not found")
			return
		}
		if p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="boat"`)
			writeErr(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		info(r).p = p
		s.countKeyUse(p.Key.ID)
		if ok, wait := s.limiter.allow(p.Key.ID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded for this key")
			return
		}
		if ok, wait := s.teamLim.allow(p.Team.ID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded for this team")
			return
		}
		if scope != "" && !p.has(scope) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "key lacks scope "+scope)
			return
		}
		release, ok := s.holdSlot(w, r, pattern, p.Team.ID)
		if !ok {
			return
		}
		defer release()
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
		case be.Status == http.StatusNotFound && info(r).vmOK:
			writeErr(w, r, 404, "not_found", "not found")
		case be.Status == http.StatusNotFound:
			writeErr(w, r, 404, "not_found", "vm not found")
		case be.Status == http.StatusTooManyRequests:
			writeErr(w, r, 429, "quota_exceeded", be.message())
		case be.Status == http.StatusConflict:
			writeErr(w, r, 409, "conflict", be.message())
		case be.Status == http.StatusServiceUnavailable && (be.code() == "no_ready_machine" || be.code() == "host_capacity"):
			// The host is full (or, with fail_fast, still starting): the caller may retry later.
			w.Header().Set("Retry-After", "10")
			writeErr(w, r, 503, be.code(), be.message())
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

// decodeHook lets the OpenAPI drift test see each route's request struct.
var decodeHook func(r *http.Request, v any)

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if decodeHook != nil {
		decodeHook(r, v)
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}
