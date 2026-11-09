package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"
	"time"
)

// Idempotency-Key on POST /api/v1/sandboxes and …/fork, as boat.dev documents
// it: per team and key for 24 h; the same body replays the first answer, a
// different body is 409 idempotency_key_reused, a request still running is
// 409 idempotency_in_progress, and a failed request frees its key at once.
//
// ponytail: in memory, so a restart forgets the keys (a retry then creates a
// second VM), and one process only. Persist them if that matters.

const (
	idemTTL        = 24 * time.Hour
	maxIdemKey     = 255
	maxIdemEntries = 10000
)

type idemEntry struct {
	hash   string
	done   bool
	status int
	ctype  string
	body   []byte
	at     time.Time
}

type idemCache struct {
	mu  sync.Mutex
	m   map[string]*idemEntry
	now func() time.Time
}

// begin claims key for hash. It returns the finished entry to replay, or the
// error code to answer, or neither when the caller should run the request.
func (c *idemCache) begin(key, hash string) (*idemEntry, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.m == nil {
		c.m = map[string]*idemEntry{}
	}
	for k, e := range c.m { // cheap at maxIdemEntries; keeps the map bounded
		if now.Sub(e.at) > idemTTL {
			delete(c.m, k)
		}
	}
	if e := c.m[key]; e != nil {
		switch {
		case e.hash != hash:
			return nil, "idempotency_key_reused"
		case !e.done:
			return nil, "idempotency_in_progress"
		}
		return e, ""
	}
	if len(c.m) >= maxIdemEntries {
		return nil, "rate_limited"
	}
	c.m[key] = &idemEntry{hash: hash, at: now}
	return nil, ""
}

// finish keeps a successful answer for replay and frees the key otherwise.
func (c *idemCache) finish(key string, status int, ctype string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if status/100 != 2 {
		delete(c.m, key)
		return
	}
	if e := c.m[key]; e != nil {
		e.done, e.status, e.ctype, e.body = true, status, ctype, body
	}
}

func (c *idemCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// teeWriter passes the answer through and keeps a copy.
type teeWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (t *teeWriter) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *teeWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = 200
	}
	t.buf.Write(b)
	return t.ResponseWriter.Write(b)
}

// idempotent runs fn once per (team, Idempotency-Key) and body.
func (s *server) idempotent(w http.ResponseWriter, r *http.Request, p *principal, fn func(http.ResponseWriter)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		fn(w)
		return
	}
	if len(key) > maxIdemKey {
		writeErr(w, r, 400, "invalid_request", "Idempotency-Key is longer than 255 characters")
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		writeErr(w, r, 400, "invalid_json", err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), b...))
	k := p.Team.ID + "\x00" + key
	e, code := s.idem.begin(k, hex.EncodeToString(sum[:]))
	switch {
	case code != "":
		w.Header().Set("Retry-After", "1")
		writeErr(w, r, 409, code, "Idempotency-Key: "+code)
		return
	case e != nil:
		w.Header().Set("Content-Type", e.ctype)
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(e.status)
		w.Write(e.body)
		return
	}
	t := &teeWriter{ResponseWriter: w}
	defer func() { s.idem.finish(k, t.status, w.Header().Get("Content-Type"), t.buf.Bytes()) }()
	fn(t)
}
