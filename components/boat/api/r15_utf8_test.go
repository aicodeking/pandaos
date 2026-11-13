package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func r15Frames(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var frame map[string]any
		if err := dec.Decode(&frame); err == io.EOF {
			return frames
		} else if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
}

func r15Output(frames []map[string]any, stream string) string {
	var out strings.Builder
	for _, frame := range frames {
		if frame["type"] == stream {
			out.WriteString(frame["data"].(string))
		}
	}
	return out.String()
}

func TestR15UTF8ExecSplit(t *testing.T) {
	for _, char := range []string{"é", "€", "😀"} {
		for split := 1; split < len(char); split++ {
			t.Run(fmt.Sprintf("%s/%d", char, split), func(t *testing.T) {
				var body bytes.Buffer
				enc := json.NewEncoder(&body)
				for _, data := range []string{char[:split], char[split:]} {
					for _, stream := range []string{"stdout", "stderr"} {
						enc.Encode(execFrame{Stream: stream, DataB64: base64.StdEncoding.EncodeToString([]byte(data))})
					}
				}
				enc.Encode(map[string]any{"exit": 0})
				w := httptest.NewRecorder()
				(&server{}).compatStreamExec(w, &body, "cmd_utf8")
				frames := r15Frames(t, w.Body.String())
				for _, stream := range []string{"stdout", "stderr"} {
					if got := r15Output(frames, stream); got != char {
						t.Errorf("%s = %q, want %q", stream, got, char)
					}
				}
				if !w.Flushed || frames[0]["type"] != "started" || frames[len(frames)-1]["type"] != "exit" {
					t.Errorf("stream envelope/flush: %v", frames)
				}
			})
		}
	}
}

type r15ReadError struct{}

func (r15ReadError) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestR15UTF8ExecTerminal(t *testing.T) {
	for _, terminal := range []string{"exit", "error", "eof", "read-error"} {
		t.Run(terminal, func(t *testing.T) {
			var body bytes.Buffer
			enc := json.NewEncoder(&body)
			for _, stream := range []string{"stdout", "stderr"} {
				enc.Encode(execFrame{Stream: stream, DataB64: base64.StdEncoding.EncodeToString([]byte("A\xff\xfeB\xe2\x82"))})
			}
			switch terminal {
			case "exit":
				enc.Encode(map[string]any{"exit": 0})
			case "error":
				enc.Encode(execFrame{Error: "failed"})
			}
			var input io.Reader = &body
			if terminal == "read-error" {
				input = io.MultiReader(input, r15ReadError{})
			}
			w := httptest.NewRecorder()
			(&server{}).compatStreamExec(w, input, "cmd_utf8")
			frames := r15Frames(t, w.Body.String())
			for _, stream := range []string{"stdout", "stderr"} {
				if got := r15Output(frames, stream); got != "A�B�" {
					t.Errorf("%s = %q", stream, got)
				}
			}
			want := "error"
			if terminal == "exit" {
				want = "exit"
			}
			if frames[len(frames)-1]["type"] != want {
				t.Errorf("terminal frame: %v", frames)
			}
		})
	}
}

// Intercept only log reads; keep the existing fake's VM ownership and command status.
func r15LogSource(t *testing.T, e *env, read func(int) (string, bool, bool)) {
	t.Helper()
	upstream := e.srv.boatd
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var args map[string]any
		json.Unmarshal(body, &args)
		if args["op"] == "cmd_logs" {
			offset := int(args["offset"].(float64))
			data, running, fail := read(offset)
			if fail {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			writeJSON(w, 200, map[string]any{"data_b64": base64.StdEncoding.EncodeToString([]byte(data)), "running": running})
			return
		}
		resp, err := upstream.do(r.Context(), r.Method, r.URL.RequestURI(), bytes.NewReader(body), r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(ts.Close)
	e.srv.boatd = newBoatd(ts.URL)
}

func r15Command(t *testing.T) (*env, string) {
	t.Helper()
	e := setup(t)
	base := "/api/v1/sandboxes/" + e.newSandbox(nil)
	started := e.ok("POST", base+"/commands", map[string]any{"command": "echo utf8", "detached": true}, 200)
	return e, base + "/commands/" + jsonNum(started["processId"]) + "/stream"
}

func TestR15UTF8AttachSplit(t *testing.T) {
	for _, char := range []string{"é", "€", "😀"} {
		for split := 1; split < len(char); split++ {
			t.Run(fmt.Sprintf("%s/%d", char, split), func(t *testing.T) {
				e, path := r15Command(t)
				log := "a" + char + "z"
				r15LogSource(t, e, func(offset int) (string, bool, bool) {
					end := len(log)
					if offset == 0 {
						end = 1 + split
					}
					return log[offset:end], false, false
				})
				frames := e.attachFrames(path + "?stderrOffset=7")
				if got := r15Output(frames, "stdout"); got != log {
					t.Errorf("stdout = %q, want %q", got, log)
				}
				if frames[1]["data"] != "a" || frames[1]["nextOffset"] != float64(1) {
					t.Errorf("must flush complete prefix with its byte offset: %v", frames)
				}
				last := frames[len(frames)-1]
				if last["type"] != "exit" || last["stdoutOffset"] != float64(len(log)) || last["stderrOffset"] != float64(7) {
					t.Errorf("exit offsets: %v", last)
				}
				for _, frame := range frames {
					if frame["type"] == "stdout" {
						resumed := e.attachFrames(path + "?stdoutOffset=" + jsonNum(frame["nextOffset"]))
						off := int(frame["nextOffset"].(float64))
						if got := r15Output(resumed, "stdout"); got != log[off:] {
							t.Errorf("resume at %d = %q, want %q", off, got, log[off:])
						}
					}
				}
			})
		}
	}
}

func TestR15UTF8AttachEOF(t *testing.T) {
	e, path := r15Command(t)
	log := "A\xff\xfeB\xe2\x82"
	r15LogSource(t, e, func(offset int) (string, bool, bool) { return log[offset:], false, false })
	frames := e.attachFrames(path)
	if got := r15Output(frames, "stdout"); got != "A�B�" {
		t.Errorf("stdout = %q", got)
	}
	last := frames[len(frames)-1]
	if last["type"] != "exit" || last["stdoutOffset"] != float64(len(log)) {
		t.Errorf("EOF offsets: %v", frames)
	}
}

func TestR15UTF8AttachResumePending(t *testing.T) {
	defer func(h, m, p time.Duration) { attachHeartbeat, attachMaxOpen, attachPoll = h, m, p }(attachHeartbeat, attachMaxOpen, attachPoll)
	attachHeartbeat, attachMaxOpen, attachPoll = 0, 0, 0
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("read-error=%t", failure), func(t *testing.T) {
			e, path := r15Command(t)
			complete := false
			r15LogSource(t, e, func(offset int) (string, bool, bool) {
				if complete {
					return "a€z"[offset:], false, false
				}
				if offset == 0 {
					return "a\xe2", true, false
				}
				return "", true, failure
			})
			frames := e.attachFrames(path + "?stderrOffset=9")
			last := frames[len(frames)-1]
			want := "reconnect"
			if failure {
				want = "error"
			}
			if got := r15Output(frames, "stdout"); got != "a" || last["type"] != want || last["stdoutOffset"] != float64(1) || last["stderrOffset"] != float64(9) {
				t.Fatalf("pending bytes must be replayable: %v", frames)
			}
			complete = true
			resumed := e.attachFrames(path + "?stdoutOffset=" + jsonNum(last["stdoutOffset"]))
			if got := r15Output(resumed, "stdout"); got != "€z" {
				t.Errorf("resumed stdout = %q", got)
			}
		})
	}
}
