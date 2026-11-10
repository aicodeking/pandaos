package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// attachFrames GETs …/commands/{pid}/stream and parses its NDJSON lines.
func (e *env) attachFrames(path string) []map[string]any {
	e.t.Helper()
	st, out, hdr := e.compat(e.owner, "GET", path, nil, nil)
	if st != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/x-ndjson") {
		e.t.Fatalf("GET %s: %d %v", path, st, out)
	}
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out["_raw"].(string)), "\n") {
		var f map[string]any
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			e.t.Fatalf("line %q: %v", line, err)
		}
		frames = append(frames, f)
	}
	return frames
}

// The official SDKs' run_command / follow_command attach to a detached command
// with boat.dev's offsets: attached, stdout chunks with nextOffset, then exit.
// Reopening at a nextOffset gives only what follows: no gap, no repeat.
func TestCompatAttachStream(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	started := e.ok("POST", base+"/commands", map[string]any{"command": "echo attached-ok", "detached": true}, 200)
	pid := jsonNum(started["processId"])
	log := "ran: bash-lcecho attached-ok\n" // what the fake agent writes (fmt.Sprint of its argv)

	fr := e.attachFrames(base + "/commands/" + pid + "/stream")
	if len(fr) != 3 || fr[0]["type"] != "attached" || fr[0]["processId"] != started["processId"] || fr[0]["stdoutOffset"] != float64(0) {
		t.Fatalf("frames: %v", fr)
	}
	if fr[1]["type"] != "stdout" || fr[1]["data"] != log || fr[1]["nextOffset"] != float64(len(log)) {
		t.Errorf("stdout: %v", fr[1])
	}
	if fr[2]["type"] != "exit" || fr[2]["status"] != "exited" || fr[2]["exitCode"] != float64(0) || fr[2]["stdoutOffset"] != float64(len(log)) {
		t.Errorf("exit: %v", fr[2])
	}

	fr = e.attachFrames(base + "/commands/" + pid + "/stream?stdoutOffset=5&stderrOffset=3")
	if fr[0]["stdoutOffset"] != float64(5) || fr[0]["stderrOffset"] != float64(3) || fr[1]["data"] != log[5:] {
		t.Errorf("resume at an offset: %v", fr)
	}
	fr = e.attachFrames(base + "/commands/" + pid + "/stream?stdoutOffset=" + jsonNum(len(log)))
	if len(fr) != 2 || fr[1]["type"] != "exit" {
		t.Errorf("at the end: only attached and exit: %v", fr)
	}

	if st, out, _ := e.compat(e.owner, "GET", base+"/commands/"+pid+"/stream?stdoutOffset=-1", nil, nil); st != 400 || out["ok"] != false {
		t.Errorf("negative offset: %d %v", st, out)
	}
	if st, _, _ := e.compat(e.owner, "GET", base+"/commands/77/stream", nil, nil); st != 404 {
		t.Errorf("unknown process: %d", st)
	}
}

// A running command: heartbeats while silent, then a reconnect line with the
// offsets to reopen at once the connection has been open for its maximum.
func TestCompatAttachHeartbeatAndReconnect(t *testing.T) {
	defer func(h, m, p time.Duration) { attachHeartbeat, attachMaxOpen, attachPoll = h, m, p }(attachHeartbeat, attachMaxOpen, attachPoll)
	attachHeartbeat, attachMaxOpen, attachPoll = 20*time.Millisecond, 120*time.Millisecond, 10*time.Millisecond
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	started := e.ok("POST", base+"/commands", map[string]any{"command": "sleep-forever", "detached": true}, 200)
	fr := e.attachFrames(base + "/commands/" + jsonNum(started["processId"]) + "/stream")
	last := fr[len(fr)-1]
	if fr[1]["type"] != "stdout" || fr[1]["data"] != "started\n" {
		t.Fatalf("output first: %v", fr)
	}
	beats := 0
	for _, f := range fr {
		if f["type"] == "heartbeat" {
			beats++
		}
	}
	if beats == 0 || last["type"] != "reconnect" || last["stdoutOffset"] != float64(len("started\n")) {
		t.Errorf("heartbeat then reconnect at the end of the output: %v", fr)
	}
}
