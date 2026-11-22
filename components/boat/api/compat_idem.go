package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
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
// Durability tradeoff: a finished answer is swapped into memory before the
// HTTP response flushes (so a retry replays it at once), but it reaches
// state.json lazily: Store.ApplyLazy coalesces every record finished within
// idemPersistDelay into one write, and neither the response nor the idem mutex
// waits for that fsync. If the process crashes inside that window the record
// is lost and a retry with the same key re-runs the operation (a duplicate
// create/fork). Graceful shutdown (Store.Close) persists it. The window is at
// most idemPersistDelay; any other durable Update in between persists it too.
//
// ponytail: answers live in state.json, which is rewritten on every update;
// maxIdemEntries bounds it. Move them to their own file if they grow.

const (
	idemTTL        = 24 * time.Hour
	maxIdemKey     = 255
	maxIdemPerTeam = 1024
	maxIdemEntries = 32768
)

// idemPersistDelay bounds the crash window described above. A var so tests can
// shorten it.
var idemPersistDelay = 250 * time.Millisecond

func idemTeamPrefix(key string) string {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return key[:i+1]
	}
	return ""
}

// idemRouteGensSnapshot builds the route-gen snapshot; a var so tests can count calls.
var idemRouteGensSnapshot = bumpedRouteGensSnapshot

// idemIndex is an in-memory, copy-on-write reverse index over State.Idempotency.
// It is shared across read-only snapshots (immutable once published) and mutated
// only inside Store.mu write callbacks. total tracks len(State.Idempotency) so
// direct map mutations in tests or fresh loads from disk trigger an automatic
// one-time rebuild.
type idemIndex struct {
	total  int
	byTeam map[string]int      // team prefix (e.g. "team\x00") -> entry count
	byVM   map[string][]string // vmID -> idempotency keys referencing vmID
}

func buildIdemIndex(m map[string]*IdemRecord) *idemIndex {
	idx := &idemIndex{
		total:  len(m),
		byTeam: make(map[string]int),
		byVM:   make(map[string][]string),
	}
	for k, rec := range m {
		if prefix := idemTeamPrefix(k); prefix != "" {
			idx.byTeam[prefix]++
		}
		if rec != nil {
			for _, vmID := range rec.VMIDs {
				idx.byVM[vmID] = append(idx.byVM[vmID], k)
			}
		}
	}
	return idx
}

func (st *State) cowIdemIndex() *idemIndex {
	if st.idemIdx == nil || st.idemIdx.total != len(st.Idempotency) {
		st.idemIdx = buildIdemIndex(st.Idempotency)
		return st.idemIdx
	}
	cp := &idemIndex{
		total:  st.idemIdx.total,
		byTeam: maps.Clone(st.idemIdx.byTeam),
		byVM:   maps.Clone(st.idemIdx.byVM),
	}
	st.idemIdx = cp
	return cp
}

func idemDeleteKey(st *State, idx *idemIndex, k string) {
	rec, ok := st.Idempotency[k]
	if !ok {
		return
	}
	delete(st.Idempotency, k)
	idx.total = len(st.Idempotency)
	if prefix := idemTeamPrefix(k); prefix != "" {
		if n := idx.byTeam[prefix]; n <= 1 {
			delete(idx.byTeam, prefix)
		} else {
			idx.byTeam[prefix] = n - 1
		}
	}
	if rec != nil {
		for _, vmID := range rec.VMIDs {
			keys := idx.byVM[vmID]
			if len(keys) == 0 {
				continue
			}
			kept := slices.DeleteFunc(slices.Clone(keys), func(x string) bool { return x == k })
			if len(kept) == 0 {
				delete(idx.byVM, vmID)
			} else {
				idx.byVM[vmID] = kept
			}
		}
	}
}

func idemPutKey(st *State, idx *idemIndex, k string, rec *IdemRecord) {
	if _, existed := st.Idempotency[k]; existed {
		idemDeleteKey(st, idx, k)
	}
	st.Idempotency[k] = rec
	idx.total = len(st.Idempotency)
	if prefix := idemTeamPrefix(k); prefix != "" {
		idx.byTeam[prefix]++
	}
	if rec != nil {
		for _, vmID := range rec.VMIDs {
			cur := idx.byVM[vmID]
			next := make([]string, len(cur), len(cur)+1)
			copy(next, cur)
			next = append(next, k)
			idx.byVM[vmID] = next
		}
	}
}

// forgetIdemByVM deletes every idempotency entry referencing vmID in O(K) using
// the reverse index (K = records referencing vmID) instead of scanning all records.
func forgetIdemByVM(st *State, vmID string) {
	if len(st.Idempotency) == 0 {
		return
	}
	idx := st.cowIdemIndex()
	keys := slices.Clone(idx.byVM[vmID])
	if len(keys) == 0 {
		return
	}
	for _, k := range keys {
		idemDeleteKey(st, idx, k)
	}
}

// deleteTeamIdem removes every idempotency entry belonging to teamID.
func deleteTeamIdem(st *State, teamID string) {
	if len(st.Idempotency) == 0 {
		return
	}
	prefix := teamID + "\x00"
	idx := st.cowIdemIndex()
	if idx.byTeam[prefix] == 0 {
		return
	}
	for k := range st.Idempotency {
		if strings.HasPrefix(k, prefix) {
			idemDeleteKey(st, idx, k)
		}
	}
}

func evictOldestIdem(st *State, idx *idemIndex, prefix string) bool {
	oldestKey := ""
	var oldestAt time.Time
	for k, e := range st.Idempotency {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		if oldestKey == "" || e.At.Before(oldestAt) || (e.At.Equal(oldestAt) && k < oldestKey) {
			oldestKey, oldestAt = k, e.At
		}
	}
	if oldestKey == "" {
		return false
	}
	idemDeleteKey(st, idx, oldestKey)
	return true
}

// IdemRecord is one finished answer to replay.
type IdemRecord struct {
	Hash      string         `json:"hash"`
	Status    int            `json:"status"`
	CType     string         `json:"ctype"`
	Body      []byte         `json:"body"`
	At        time.Time      `json:"at"`
	KeyID     string         `json:"key_id,omitempty"`
	KeyGen    int64          `json:"key_gen,omitempty"`
	TeamID    string         `json:"team_id,omitempty"`
	VMIDs     []string       `json:"vm_ids,omitempty"`
	RouteGens map[string]int `json:"route_gens,omitempty"`
	Native    bool           `json:"native,omitempty"`
}

// idemEntryValid reports whether e is still safe to replay. Pass a precomputed
// rg from bumpedRouteGensSnapshot when checking multiple entries; nil computes
// per-VM route generations directly without allocating a host-wide map.
func idemEntryValid(st *State, e *IdemRecord, now time.Time, rg map[string]int) bool {
	if e == nil {
		return false
	}
	if e.KeyID != "" && !keyAliveGenIn(st, e.KeyID, e.KeyGen, e.TeamID, "", now) {
		return false
	}
	if len(e.VMIDs) > 0 {
		for _, vmID := range e.VMIDs {
			if tok := st.VMTokens[vmID]; tok != nil && tok.Retired {
				return false
			}
			if ai := st.AIVMs[vmID]; ai != nil && ai.Retired {
				return false
			}
			if !e.Native && st.CompatByVM[vmID] == "" {
				return false
			}
			var gen int
			if rg != nil {
				gen = rg[vmID]
			} else {
				gen = st.RouteGens[vmID]
				if vmHasDeadPortMinters(st, vmID, now) {
					gen++
				}
			}
			if gen != e.RouteGens[vmID] {
				return false
			}
		}
	}
	return true
}

type idemCache struct {
	mu      sync.Mutex
	pending map[string]string // key -> body hash, while the first request runs
	store   *Store
	now     func() time.Time
}

func idemTeamCountReadOnly(st *State, prefix string) int {
	if st.idemIdx != nil && st.idemIdx.total == len(st.Idempotency) {
		return st.idemIdx.byTeam[prefix]
	}
	n := 0
	for sk := range st.Idempotency {
		if strings.HasPrefix(sk, prefix) {
			n++
		}
	}
	return n
}

// begin claims key for hash. It returns the finished answer to replay, or the
// error code to answer, or neither when the caller should run the request.
func (c *idemCache) begin(key, hash string) (*IdemRecord, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.store.Snapshot()
	now := c.clock()
	if e := st.Idempotency[key]; e != nil && now.Sub(e.At) <= idemTTL {
		if idemEntryValid(st, e, now, nil) {
			if e.Hash != hash {
				return nil, "idempotency_key_reused"
			}
			return e, ""
		}
		if err := c.store.ApplyLazy(func(st *State) error {
			idx := st.cowIdemIndex()
			idemDeleteKey(st, idx, key)
			return nil
		}, idemPersistDelay); err != nil {
			log.Printf("idempotency stale eviction: %v", err)
			return nil, "store_error"
		}
		st = c.store.Snapshot()
	} else if e != nil {
		// Expired hit for this key: drop lazily before claiming.
		if err := c.store.ApplyLazy(func(st *State) error {
			idx := st.cowIdemIndex()
			idemDeleteKey(st, idx, key)
			return nil
		}, idemPersistDelay); err != nil {
			log.Printf("idempotency expired eviction: %v", err)
			return nil, "store_error"
		}
		st = c.store.Snapshot()
	}
	if h, ok := c.pending[key]; ok {
		if h != hash {
			return nil, "idempotency_key_reused"
		}
		return nil, "idempotency_in_progress"
	}
	prefix := idemTeamPrefix(key)
	pendingTeam := 0
	storedTeam := 0
	if prefix != "" {
		for pk := range c.pending {
			if strings.HasPrefix(pk, prefix) {
				pendingTeam++
			}
		}
		if pendingTeam >= maxIdemPerTeam {
			return nil, "rate_limited"
		}
		storedTeam = idemTeamCountReadOnly(st, prefix)
	}
	needGlobalPurge := len(c.pending)+len(st.Idempotency) >= maxIdemEntries
	needTeamPurge := prefix != "" && pendingTeam+storedTeam >= maxIdemPerTeam
	if needGlobalPurge || needTeamPurge {
		if err := c.store.ApplyLazy(func(st *State) error {
			idx := st.cowIdemIndex()
			for k, e := range st.Idempotency {
				if now.Sub(e.At) > idemTTL {
					idemDeleteKey(st, idx, k)
				}
			}
			if prefix != "" {
				for pendingTeam+idx.byTeam[prefix] >= maxIdemPerTeam {
					if !evictOldestIdem(st, idx, prefix) {
						break
					}
				}
			}
			if prefix != "" && len(c.pending)+len(st.Idempotency) >= maxIdemEntries && len(c.pending) < maxIdemEntries {
				for len(c.pending)+len(st.Idempotency) >= maxIdemEntries {
					if !evictOldestIdem(st, idx, "") {
						break
					}
				}
			}
			return nil
		}, idemPersistDelay); err != nil {
			log.Printf("idempotency eviction: %v", err)
			return nil, "store_error"
		}
		st = c.store.Snapshot()
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
	c.finishWithMeta(key, status, ctype, body, nil, nil)
}

func (c *idemCache) finishWithMeta(key string, status int, ctype string, body []byte, p *principal, vmIDs []string) {
	// The claim stays in c.pending until the record is in the store, so a retry
	// racing the swap sees "in progress", never a free key. c.mu is not held
	// across the store call, so distinct keys never queue behind one another.
	c.mu.Lock()
	hash := c.pending[key]
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()
	if status/100 != 2 {
		return
	}
	now := c.clock()
	prefix := idemTeamPrefix(key)
	var keyID, teamID string
	var keyGen int64
	if p != nil {
		keyID = durableKeyID(p)
		keyGen = p.KeyGen
		teamID = p.Team.ID
	}
	err := c.store.ApplyLazy(func(st *State) error {
		idx := st.cowIdemIndex()
		var rg map[string]int
		getRG := func() map[string]int {
			if rg == nil {
				rg = idemRouteGensSnapshot(st, now)
			}
			return rg
		}
		needTeam := prefix != "" && st.Idempotency[key] == nil && idx.byTeam[prefix] >= maxIdemPerTeam
		needGlobal := st.Idempotency[key] == nil && len(st.Idempotency) >= maxIdemEntries
		if needTeam || needGlobal {
			// Only sweep expired/invalid entries when hitting a capacity cap,
			// computing the route-gen snapshot once for the entire sweep.
			snapRG := getRG()
			for k, e := range st.Idempotency {
				if now.Sub(e.At) > idemTTL || !idemEntryValid(st, e, now, snapRG) {
					idemDeleteKey(st, idx, k)
				}
			}
			if prefix != "" && st.Idempotency[key] == nil {
				for idx.byTeam[prefix] >= maxIdemPerTeam {
					if !evictOldestIdem(st, idx, prefix) {
						break
					}
				}
			}
			for st.Idempotency[key] == nil && len(st.Idempotency) >= maxIdemEntries {
				if !evictOldestIdem(st, idx, "") {
					break
				}
			}
		}
		var vmsCopy []string
		var routeGens map[string]int
		if len(vmIDs) > 0 {
			vmsCopy = slices.Clone(vmIDs)
			snapRG := getRG()
			routeGens = make(map[string]int, len(vmIDs))
			for _, vmID := range vmIDs {
				routeGens[vmID] = snapRG[vmID]
			}
		}
		idemPutKey(st, idx, key, &IdemRecord{
			Hash:      hash,
			Status:    status,
			CType:     ctype,
			Body:      slices.Clone(body),
			At:        now,
			KeyID:     keyID,
			KeyGen:    keyGen,
			TeamID:    teamID,
			VMIDs:     vmsCopy,
			RouteGens: routeGens,
			Native:    strings.Count(key, "\x00") >= 2,
		})
		return nil
	}, idemPersistDelay)
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

// idempotent authorizes every request before running or replaying its answer.
func (s *server) idempotent(w http.ResponseWriter, r *http.Request, p *principal, authorize func() bool, fn func(http.ResponseWriter)) {
	s.idempotentScoped(w, r, p, "", authorize, fn)
}

func (s *server) idempotentScoped(w http.ResponseWriter, r *http.Request, p *principal, scope string, authorize func() bool, fn func(http.ResponseWriter)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		if authorize() {
			fn(w)
		}
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
	if !authorize() {
		return
	}
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), b...))
	k := p.Team.ID + "\x00" + key
	if scope != "" {
		k = p.Team.ID + "\x00" + scope + "\x00" + key
	}
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
	defer func() {
		s.idem.finishWithMeta(k, t.status, w.Header().Get("Content-Type"), t.buf.Bytes(), p, info(r).idemVMs)
		if t.status >= 400 || t.status == 0 {
			s.store.Kick()
		}
	}()
	fn(t)
}
