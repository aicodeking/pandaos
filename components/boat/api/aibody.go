package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Buffered request bodies. The AI proxy reads a body whole when it has to patch
// it (aiMayInjectUsage). Its memory is bounded four ways: one body is capped
// (aiMaxBody), every VM may hold only aiBudgetPerVM bytes of bodies at once, every
// team aiBudgetPerTeam (so one team's VMs cannot take the whole global budget
// from the others) and all teams together aiBudgetGlobal. Bytes are charged as
// they arrive (as buffer capacity, which at most doubles), never by the
// Content-Length a client declares; a Content-Length over aiMaxBody is refused
// up front. The whole body must arrive within aiBodyDeadline and at least
// aiMinProgress bytes must arrive in every aiProgressWindow, so a trickle cannot
// hold its bytes for a minute. The bytes are given back once the body has gone
// upstream, not when the provider's headers return. Variables so tests can
// shrink them.
const aiMaxBody = 32 << 20

// The budgets count bytes received, but a request really holds about 3x its body
// while it is patched and sent (the read buffer, the patched or re-encoded copy,
// the one the transport writes). The defaults are set for that: the global budget
// of 128 MiB is at most ~384 MiB of real memory (it was 256 MiB, ~768 MiB real),
// a team's 32 MiB at most ~96 MiB (a quarter of the global one: at least four teams
// can hold their full budget at once), and a VM's 32 MiB fits one maximum body.
var (
	aiBudgetGlobal   int64 = 128 << 20
	aiBudgetPerTeam  int64 = 32 << 20
	aiBudgetPerVM    int64 = 32 << 20
	aiBodyDeadline         = 60 * time.Second
	aiMinProgress          = 64 << 10
	aiProgressWindow       = 10 * time.Second
)

// aiBodyError is a refusal of the request body: status and OpenAI-style type.
type aiBodyError struct {
	status     int
	typ        string
	msg        string
	retryAfter int // seconds; 0 = no header
}

func (e *aiBodyError) Error() string { return e.msg }

func writeAIBodyError(w http.ResponseWriter, e *aiBodyError) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	if e.status == 408 || e.status == 413 {
		w.Header().Set("Connection", "close") // the rest of the body is not read
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]string{"type": e.typ, "message": e.msg}})
}

func aiBudgetError(err error) *aiBodyError {
	var eb *errByteBudget
	if errors.As(err, &eb) {
		switch {
		case eb.perKey:
			return &aiBodyError{status: 429, typ: "boat_proxy_error", retryAfter: 5, msg: "too many request bytes buffered for this VM; retry shortly"}
		case eb.perTeam:
			return &aiBodyError{status: 429, typ: "boat_proxy_error", retryAfter: 5, msg: "too many request bytes buffered for this team; retry shortly"}
		}
	}
	return &aiBodyError{status: 503, typ: "boat_proxy_error", retryAfter: 5, msg: "the AI proxy is buffering too many request bytes; retry shortly"}
}

// aiReadBody reads the whole request body within the size cap, the byte budget
// (res), the read deadline and the minimum-progress rule. The budget is charged
// as the buffer grows (never by the declared length), so a client that declares
// a big body and sends little holds little.
func aiReadBody(w http.ResponseWriter, r *http.Request, res *byteRes) ([]byte, *aiBodyError) {
	declared := r.ContentLength
	if declared > aiMaxBody {
		return nil, &aiBodyError{status: 413, typ: "boat_proxy_error", msg: "the request body is too large"}
	}
	rc := http.NewResponseController(w)
	defer rc.SetReadDeadline(time.Time{})
	start := time.Now()
	overall := start.Add(aiBodyDeadline)
	progressBy, sinceMark := start.Add(aiProgressWindow), 0
	src := http.MaxBytesReader(w, r.Body, aiMaxBody)
	var buf []byte
	for {
		if len(buf) == cap(buf) {
			if declared > 0 && int64(len(buf)) >= declared {
				return buf, nil
			}
			grown := make([]byte, len(buf), aiGrowCap(cap(buf), declared))
			if err := res.add(int64(cap(grown) - cap(buf))); err != nil {
				return nil, aiBudgetError(err)
			}
			buf = grown[:copy(grown, buf)]
		}
		rc.SetReadDeadline(minTime(overall, progressBy))
		n, err := src.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if sinceMark += n; sinceMark >= aiMinProgress {
			progressBy, sinceMark = time.Now().Add(aiProgressWindow), 0
		}
		if declared > 0 && int64(len(buf)) >= declared {
			return buf, nil
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			switch {
			case errors.As(err, &tooBig):
				return nil, &aiBodyError{status: 413, typ: "boat_proxy_error", msg: "the request body is too large"}
			case errors.Is(err, os.ErrDeadlineExceeded):
				return nil, &aiBodyError{status: 408, typ: "boat_proxy_error", msg: "the request body did not arrive in time"}
			}
			return nil, &aiBodyError{status: 400, typ: "invalid_request", msg: "cannot read the request body"}
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// aiProgressReader enforces the same overall deadline and minimum-progress
// stall guard as aiReadBody on non-buffered streaming request bodies, so a
// trickling guest cannot pin an upstream AI connection open indefinitely.
type aiProgressReader struct {
	src        io.ReadCloser
	rc         *http.ResponseController
	overall    time.Time
	progressBy time.Time
	window     time.Duration
	minBytes   int
	sinceMark  int

	mu      sync.Mutex
	cleared bool
	err     *aiBodyError
}

func newAIProgressReader(w http.ResponseWriter, src io.ReadCloser) *aiProgressReader {
	now := time.Now()
	return &aiProgressReader{
		src:        src,
		rc:         http.NewResponseController(w),
		overall:    now.Add(aiBodyDeadline),
		progressBy: now.Add(aiProgressWindow),
		window:     aiProgressWindow,
		minBytes:   aiMinProgress,
	}
}

func (u *aiProgressReader) setErr(e *aiBodyError) {
	u.mu.Lock()
	if u.err == nil {
		u.err = e
	}
	u.mu.Unlock()
}

func (u *aiProgressReader) bodyError() *aiBodyError {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

func (u *aiProgressReader) clearDeadline() {
	u.mu.Lock()
	if !u.cleared {
		u.cleared = true
		_ = u.rc.SetReadDeadline(time.Time{})
	}
	u.mu.Unlock()
}

func (u *aiProgressReader) done() { u.clearDeadline() }

func (u *aiProgressReader) Read(p []byte) (int, error) {
	now := time.Now()
	deadline := minTime(u.overall, u.progressBy)
	if !now.Before(deadline) {
		u.setErr(&aiBodyError{status: 408, typ: "boat_proxy_error", msg: "the request body did not arrive in time"})
		u.clearDeadline()
		return 0, os.ErrDeadlineExceeded
	}
	var (
		n   int
		err error
	)
	if derr := u.rc.SetReadDeadline(deadline); derr != nil {
		wait := time.Until(deadline)
		if wait <= 0 {
			u.setErr(&aiBodyError{status: 408, typ: "boat_proxy_error", msg: "the request body did not arrive in time"})
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.AfterFunc(wait, func() { _ = u.src.Close() })
		n, err = u.src.Read(p)
		if !timer.Stop() && err != nil {
			err = os.ErrDeadlineExceeded
		}
	} else {
		n, err = u.src.Read(p)
	}
	if n > 0 {
		u.sinceMark += n
		if u.sinceMark >= u.minBytes {
			u.progressBy, u.sinceMark = time.Now().Add(u.window), 0
		}
	}
	if err == io.EOF {
		u.clearDeadline()
		return n, io.EOF
	}
	if err != nil {
		u.clearDeadline()
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			u.setErr(&aiBodyError{status: 413, typ: "boat_proxy_error", msg: "the request body is too large"})
		case isUploadTimeout(err):
			u.setErr(&aiBodyError{status: 408, typ: "boat_proxy_error", msg: "the request body did not arrive in time"})
		}
		return n, err
	}
	return n, nil
}

func (u *aiProgressReader) Close() error {
	u.clearDeadline()
	return u.src.Close()
}

// aiGrowCap is the next buffer capacity: it doubles from 4 KiB, never past the
// declared length (when there is one) and never past what MaxBytesReader needs to
// notice a body over aiMaxBody (one byte more).
func aiGrowCap(have int, declared int64) int {
	next := max(4<<10, 2*have)
	limit := int64(aiMaxBody) + 1
	if declared > 0 {
		limit = declared
	}
	return int(min(int64(next), limit))
}

// aiHeldBody is a request body the proxy holds in memory for the upstream call.
// Once the transport has written it all (or the call is over) drop gives the
// bytes and their budget back; until then GetBody can replay it, which the
// transport needs when a pooled connection turns out to be dead.
type aiHeldBody struct {
	mu   sync.Mutex
	buf  []byte
	gone bool
	res  *byteRes
}

var errAIBodyReleased = errors.New("the request body was already sent")

func newAIHeldBody(buf []byte, res *byteRes) *aiHeldBody { return &aiHeldBody{buf: buf, res: res} }

func (h *aiHeldBody) reader() io.ReadCloser { return &aiHeldReader{h: h} }

// getBody is http.Request.GetBody.
func (h *aiHeldBody) getBody() (io.ReadCloser, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gone {
		return nil, errAIBodyReleased
	}
	return &aiHeldReader{h: h}, nil
}

// drop frees the buffer and returns its budget; it is safe to call twice.
func (h *aiHeldBody) drop() {
	h.mu.Lock()
	h.buf, h.gone = nil, true
	h.mu.Unlock()
	h.res.release()
}

type aiHeldReader struct {
	h   *aiHeldBody
	off int
}

func (r *aiHeldReader) Read(p []byte) (int, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if r.h.gone {
		return 0, errAIBodyReleased
	}
	if r.off >= len(r.h.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.h.buf[r.off:])
	r.off += n
	return n, nil
}

func (r *aiHeldReader) Close() error { return nil }

// aiPrepareBody checks a body the proxy will patch and returns what to forward:
// a streaming body re-encoded with include_usage forced, any other valid body as
// sent. A body that is not a JSON object, or whose stream is present and not a
// boolean, is refused. The caller must not use raw afterwards (it is dropped
// while the output is built, so a large body is not held three times).
func aiPrepareBody(raw []byte) ([]byte, *aiBodyError) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, &aiBodyError{status: 400, typ: "invalid_request", msg: "the request body must be a JSON object"}
	}
	stream := false
	if v, has := m["stream"]; has && !bytes.Equal(v, []byte("null")) {
		if json.Unmarshal(v, &stream) != nil {
			return nil, &aiBodyError{status: 400, typ: "invalid_request", msg: "stream must be a boolean"}
		}
	}
	if !stream {
		return raw, nil
	}
	opts, ok := aiStreamOptions(m)
	if !ok {
		return raw, nil // stream_options is not an object: the provider says so
	}
	raw = nil // dead from here: m holds its own copy of every value
	out, err := aiMarshalWithUsage(m, opts)
	if err != nil {
		return nil, &aiBodyError{status: 500, typ: "boat_proxy_error", msg: "cannot re-encode the request body"}
	}
	return out, nil
}

// aiStreamOptions returns the body's stream_options object (empty when absent or
// null); ok=false when it is present and not an object.
func aiStreamOptions(m map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	opts := map[string]json.RawMessage{}
	if raw, has := m["stream_options"]; has && string(raw) != "null" {
		if json.Unmarshal(raw, &opts) != nil || opts == nil {
			return nil, false
		}
	}
	return opts, true
}

func aiMarshalWithUsage(m, opts map[string]json.RawMessage) ([]byte, error) {
	opts["include_usage"] = json.RawMessage("true")
	so, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	m["stream_options"] = so
	return json.Marshal(m)
}

// aiMayInjectUsage: an OpenAI-style completions POST (chat, legacy, FIM and
// agents completions; anthropic's own messages API reports usage in its
// stream). The body's size, framing and Content-Type do not matter: every one
// is read and patched, or the guest could choose one that is not.
func aiMayInjectUsage(provider string, r *http.Request) bool {
	p := r.PathValue("path")
	// Mistral's FIM and agents endpoints are not known to accept stream_options;
	// only its chat endpoint is patched (others still count as unmetered).
	if provider == "mistral" && !strings.HasSuffix(p, "chat/completions") {
		return false
	}
	return provider != "anthropic" && r.Method == http.MethodPost &&
		(p == "completions" || strings.HasSuffix(p, "/completions")) &&
		r.ContentLength != 0
}
