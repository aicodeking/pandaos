// Package fakerelay is an in-memory relay speaking the placeholder protocol
// (see ../../protocol.go). For tests and NixOS VM tests only: no persistence,
// admin endpoints are unauthenticated.
package fakerelay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Job struct {
	ID          string   `json:"id"`
	Argv        []string `json:"argv"`
	TimeoutSec  int      `json:"timeoutSec,omitempty"`
	MemoryMaxMB int      `json:"memoryMaxMB,omitempty"`
	KeepWorkdir bool     `json:"keepWorkdir,omitempty"`
}

type Result struct {
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

type Relay struct {
	longPoll time.Duration
	jobs     chan Job

	mu       sync.Mutex
	codes    map[string]string // pairing code -> token ("" until first status poll)
	tokens   map[string]bool
	results  map[string]Result
	cancels  map[string]bool // cancelled jobs without a result yet
	pairings int
}

func New(longPoll time.Duration) *Relay {
	return &Relay{
		longPoll: longPoll,
		jobs:     make(chan Job, 1024),
		codes:    map[string]string{},
		tokens:   map[string]bool{},
		results:  map[string]Result{},
		cancels:  map[string]bool{},
	}
}

// Handler serves the box-facing API, plus /admin/* when admin is true.
func (r *Relay) Handler(admin bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pair/start", r.pairStart)
	mux.HandleFunc("GET /v1/pair/status", r.pairStatus)
	mux.HandleFunc("POST /v1/heartbeat", r.auth(r.heartbeat))
	mux.HandleFunc("GET /v1/jobs/next", r.auth(r.nextJob))
	mux.HandleFunc("POST /v1/jobs/{id}/result", r.auth(r.result))
	if admin {
		mux.HandleFunc("POST /v1/jobs/{id}/cancel", r.cancel)
		mux.HandleFunc("POST /admin/jobs", r.adminEnqueue)
		mux.HandleFunc("GET /admin/results/{id}", r.adminResult)
	}
	return mux
}

// Enqueue queues a job; false if the queue is full.
func (r *Relay) Enqueue(j Job) bool {
	select {
	case r.jobs <- j:
		return true
	default:
		return false
	}
}

func (r *Relay) Result(id string) (Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.results[id]
	return res, ok
}

// Cancel asks boxes (via heartbeat) to stop job id. A job still queued is
// dropped with a cancelled result when a box next polls. False if the job
// already has a result.
func (r *Relay) Cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, done := r.results[id]; done {
		return false
	}
	r.cancels[id] = true
	return true
}

// Revoke invalidates every issued token, forcing boxes to re-pair.
func (r *Relay) Revoke() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = map[string]bool{}
}

// Pairings counts tokens issued so far.
func (r *Relay) Pairings() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairings
}

func (r *Relay) valid(token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens[token]
}

func (r *Relay) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !ok || !r.valid(tok) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, req)
	}
}

func (r *Relay) pairStart(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Code == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	if _, ok := r.codes[in.Code]; !ok {
		r.codes[in.Code] = ""
	}
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, struct{}{})
}

// pairStatus approves on the first poll and hands out the token from the second on.
func (r *Relay) pairStatus(w http.ResponseWriter, req *http.Request) {
	code := req.URL.Query().Get("code")
	r.mu.Lock()
	token, ok := r.codes[code]
	if ok && token == "" {
		r.codes[code] = newID()
		r.tokens[r.codes[code]] = true
		r.pairings++
	}
	r.mu.Unlock()
	switch {
	case !ok:
		http.Error(w, "unknown code", http.StatusNotFound)
	case token == "":
		writeJSON(w, http.StatusAccepted, struct{}{})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
	}
}

func (r *Relay) heartbeat(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.cancels))
	for id := range r.cancels {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string][]string{"cancel": ids})
}

// takeCancelled records a cancelled result for j if it was cancelled while queued.
func (r *Relay) takeCancelled(j Job) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cancels[j.ID] {
		return false
	}
	delete(r.cancels, j.ID)
	r.results[j.ID] = Result{ExitCode: 130, Stderr: "cancelled before start", Cancelled: true}
	return true
}

func (r *Relay) nextJob(w http.ResponseWriter, req *http.Request) {
	t := time.NewTimer(r.longPoll)
	defer t.Stop()
	for {
		select {
		case j := <-r.jobs:
			if r.takeCancelled(j) {
				continue
			}
			r.sendJob(w, req, j)
		case <-t.C:
			w.WriteHeader(http.StatusNoContent)
		case <-req.Context().Done():
		}
		return
	}
}

func (r *Relay) sendJob(w http.ResponseWriter, req *http.Request, j Job) {
	// The token may have been revoked while we waited; don't hand the job to a dead session.
	if tok, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); !r.valid(tok) {
		r.Enqueue(j)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (r *Relay) result(w http.ResponseWriter, req *http.Request) {
	var res Result
	if err := json.NewDecoder(req.Body).Decode(&res); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.results[req.PathValue("id")] = res
	delete(r.cancels, req.PathValue("id"))
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) cancel(w http.ResponseWriter, req *http.Request) {
	if !r.Cancel(req.PathValue("id")) {
		http.Error(w, "job already finished", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (r *Relay) adminEnqueue(w http.ResponseWriter, req *http.Request) {
	var j Job
	if err := json.NewDecoder(req.Body).Decode(&j); err != nil || len(j.Argv) == 0 {
		http.Error(w, "want {id?, argv:[...], timeoutSec?, memoryMaxMB?, keepWorkdir?}", http.StatusBadRequest)
		return
	}
	if j.ID == "" {
		j.ID = newID()
	}
	if !r.Enqueue(j) {
		http.Error(w, "queue full", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": j.ID})
}

func (r *Relay) adminResult(w http.ResponseWriter, req *http.Request) {
	res, ok := r.Result(req.PathValue("id"))
	if !ok {
		http.Error(w, "no result yet", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
