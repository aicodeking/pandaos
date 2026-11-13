package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// GET /api/v1/sandboxes/{id}/commands/{pid}/stream: boat.dev's attachCommand
// (docs.boat.dev/openapi/boat-v1.yaml, CommandAttachFrame), which the official
// SDKs' run_command / follow_command (runCommand / followCommand) read. One
// JSON object per line:
//
//	attached  {processId, stdoutOffset, stderrOffset}       first line
//	stdout    {data, nextOffset}                            one per chunk
//	heartbeat {}                                            after 15 s of silence
//	exit      {status, exitCode, signal, stdoutOffset, stderrOffset}   last
//	reconnect {stdoutOffset, stderrOffset}                  last, after 10 min
//	error     {error, message, stdoutOffset, stderrOffset}  last, on a failure
//
// A detached command here has one combined log (stdout and stderr in the order
// written), served as stdout; stderr never has data, and its offset is echoed.
// Offsets are byte positions in the log, so reopening with the last nextOffset
// (or a reconnect line's offsets) continues with no gap and no repeat.

var (
	attachHeartbeat = 15 * time.Second
	attachMaxOpen   = 10 * time.Minute
	attachPoll      = 250 * time.Millisecond
)

func (s *server) compatAttach(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	rec, ok := s.pathProcess(w, r, "pid")
	if !ok {
		return
	}
	q := r.URL.Query()
	stdoutOff, err1 := strconv.ParseInt(orDefault(q.Get("stdoutOffset"), "0"), 10, 64)
	stderrOff, err2 := strconv.ParseInt(orDefault(q.Get("stderrOffset"), "0"), 10, 64)
	if err1 != nil || err2 != nil || stdoutOff < 0 || stderrOff < 0 {
		writeErr(w, r, 400, "invalid_query", "stdoutOffset and stderrOffset are byte offsets >= 0")
		return
	}
	a := &attachStream{w: w, enc: json.NewEncoder(w), last: time.Now()}
	a.flusher, _ = w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	offsets := func(m map[string]any) map[string]any {
		m["stdoutOffset"], m["stderrOffset"] = stdoutOff, stderrOff
		return m
	}
	if !a.emit(offsets(map[string]any{"type": "attached", "processId": rec.ProcessID})) {
		return
	}
	vmID := r.PathValue("id")
	deadline := time.Now().Add(attachMaxOpen)
	var stdout compatUTF8Stream
	readOff := stdoutOff
	for {
		out, err := s.readLogs(r, vmID, rec.ID, readOff, compatOutCap)
		if err != nil {
			if r.Context().Err() == nil {
				a.emit(offsets(map[string]any{"type": "error", "error": "agent_unavailable",
					"message": "cannot read the command's output: " + err.Error()}))
			}
			return
		}
		b, _ := base64.StdEncoding.DecodeString(str(out, "data_b64"))
		if len(b) > 0 {
			readOff += int64(len(b))
			text := stdout.text(b, false)
			// Public offsets acknowledge emitted bytes; pending bytes must be replayed on reconnect.
			stdoutOff = readOff - int64(len(stdout.pending))
			if text != "" && !a.emit(map[string]any{"type": "stdout", "data": text, "nextOffset": stdoutOff}) {
				return
			}
			continue // more may be waiting
		}
		if out["running"] != true {
			if text := stdout.text(nil, true); text != "" {
				stdoutOff = readOff
				if !a.emit(map[string]any{"type": "stdout", "data": text, "nextOffset": stdoutOff}) {
					return
				}
			}
			a.emit(offsets(attachExit(out)))
			return
		}
		if time.Now().After(deadline) {
			a.emit(offsets(map[string]any{"type": "reconnect"}))
			return
		}
		if time.Since(a.last) >= attachHeartbeat && !a.emit(map[string]any{"type": "heartbeat"}) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(attachPoll):
		}
	}
}

// attachExit is the exit line for a finished command's cmd_logs answer.
func attachExit(out map[string]any) map[string]any {
	m := map[string]any{"type": "exit", "status": "exited", "exitCode": nil, "signal": nil}
	if f, ok := out["exit"].(float64); ok {
		m["exitCode"] = int(f)
	}
	if sig, ok := out["signal"].(string); ok && sig != "" {
		m["signal"] = sig
	}
	if out["state"] == "lost" || (m["exitCode"] == nil && m["signal"] == nil) {
		m["status"] = "lost"
	}
	return m
}

type attachStream struct {
	w       http.ResponseWriter
	enc     *json.Encoder
	flusher http.Flusher
	last    time.Time
}

func (a *attachStream) emit(v any) bool {
	if a.enc.Encode(v) != nil {
		return false
	}
	if a.flusher != nil {
		a.flusher.Flush()
	}
	a.last = time.Now()
	return true
}
