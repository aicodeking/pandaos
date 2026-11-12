package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// Idempotency-Key on POST /api/v1/sandboxes and …/fork, as boat.dev documents
// it: per team and key for 24 h; the same body replays the first answer, a
// different body is 409 idempotency_key_reused, a request still running is
// 409 idempotency_in_progress, and a failed request frees its key at once.
//
// A finished answer is kept in the store (State.Idempotency), so a restart
// still replays it. A request in flight is held in memory only: if the API
// dies mid-request the key is free again after the restart.
//
// ponytail: answers live in state.json, which is rewritten on every update;
// maxIdemEntries bounds it. Move them to their own file if they grow.

const (
	idemTTL        = 24 * time.Hour
	maxIdemKey     = 255
	maxIdemEntries = 10000
)

// IdemRecord is one finished answer to replay.
type IdemRecord struct {
	Hash   string    `json:"hash"`
	Status int       `json:"status"`
	CType  string    `json:"ctype"`
	Body   []byte    `json:"body"`
	At     time.Time `json:"at"`
}

type idemCache struct {
	mu      sync.Mutex
	pending map[string]string // key -> body hash, while the first request runs
	store   *Store
	now     func() time.Time
}

// begin claims key for hash. It returns the finished answer to replay, or the
// error code to answer, or neither when the caller should run the request.
func (c *idemCache) begin(key, hash string) (*IdemRecord, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.store.Snapshot()
	if e := st.Idempotency[key]; e != nil && c.clock().Sub(e.At) <= idemTTL {
		if e.Hash != hash {
			return nil, "idempotency_key_reused"
		}
		return e, ""
	}
	if h, ok := c.pending[key]; ok {
		if h != hash {
			return nil, "idempotency_key_reused"
		}
		return nil, "idempotency_in_progress"
	}
	if len(c.pending)+len(st.Idempotency) >= maxIdemEntries {
		return nil, "rate_limited"
	}
	if c.pending == nil {
		c.pending = map[string]string{}
	}
	c.pending[key] = hash
	return nil, ""
}

// finish keeps a successful answer for replay and frees the key otherwise.
func (c *idemCache) finish(key string, status int, ctype string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hash := c.pending[key]
	delete(c.pending, key)
	if status/100 != 2 {
		return
	}
	now := c.clock()
	err := c.store.Update(func(st *State) error {
		for k, e := range st.Idempotency { // keeps the map bounded
			if now.Sub(e.At) > idemTTL {
				delete(st.Idempotency, k)
			}
		}
		st.Idempotency[key] = &IdemRecord{Hash: hash, Status: status, CType: ctype, Body: body, At: now}
		return nil
	})
	if err != nil {
		log.Printf("idempotency: %v", err) // the answer went out; a retry runs again
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
		w.Header().Set("Content-Type", e.CType)
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(e.Status)
		w.Write(e.Body)
		return
	}
	t := &teeWriter{ResponseWriter: w}
	defer func() { s.idem.finish(k, t.status, w.Header().Get("Content-Type"), t.buf.Bytes()) }()
	fn(t)
}
