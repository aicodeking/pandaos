package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Keep ownership/status handling real while returning the guest's absolute log positions.
func r17LogSource(t *testing.T, e *env, read func(int) (int, string, bool)) {
	t.Helper()
	upstream := e.srv.boatd
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var args map[string]any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &args); err != nil {
				t.Error(err)
				return
			}
		}
		if args["op"] == "cmd_logs" {
			start, data, running := read(int(args["offset"].(float64)))
			writeJSON(w, 200, map[string]any{"offset": start, "next_offset": start + len(data),
				"data_b64": base64.StdEncoding.EncodeToString([]byte(data)), "running": running,
				"truncated": start > int(args["offset"].(float64))})
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

func TestR17AttachAbsoluteOffsets(t *testing.T) {
	defer func(d time.Duration) { attachMaxOpen = d }(attachMaxOpen)
	attachMaxOpen = 0
	for _, running := range []bool{false, true} {
		for _, tc := range []struct {
			name                 string
			base, initial, chunk int
			log, want            string
			requests             []int
		}{
			{"truncated", 8, 0, 4, "TAIL", "TAIL", []int{0, 12}},
			{"stale-resume", 8, 3, 4, "TAIL", "TAIL", []int{3, 12}},
			{"empty-retained", 8, 0, 4, "", "", []int{0}},
			{"past-end", 8, 99, 4, "TAIL", "", []int{99}},
			{"utf8-chunks", 0, 0, 2, "a€z", "a€z", []int{0, 2, 4, 5}},
			{"utf8-resume", 0, 1, 2, "a€z", "€z", []int{1, 3, 5}},
		} {
			terminal := "exit"
			if running {
				terminal = "reconnect"
			}
			t.Run(tc.name+"/"+terminal, func(t *testing.T) {
				e, path := r15Command(t)
				var requests []int
				r17LogSource(t, e, func(offset int) (int, string, bool) {
					requests = append(requests, offset)
					if len(requests) > 6 {
						t.Errorf("unbounded log replay: %v", requests)
						return offset, "", false
					}
					start := max(tc.base, min(offset, tc.base+len(tc.log)))
					end := min(start+tc.chunk, tc.base+len(tc.log))
					return start, tc.log[start-tc.base : end-tc.base], running
				})
				frames := e.attachFrames(path + "?stdoutOffset=" + jsonNum(tc.initial) + "&stderrOffset=7")
				if got := r15Output(frames, "stdout"); got != tc.want {
					t.Errorf("stdout = %q, want %q; frames: %v", got, tc.want, frames)
				}
				end := tc.base + len(tc.log)
				last := frames[len(frames)-1]
				if last["type"] != terminal || last["stdoutOffset"] != float64(end) || last["stderrOffset"] != float64(7) {
					t.Errorf("terminal offsets: %v, want %s at %d", last, terminal, end)
				}
				if frames[0]["stdoutOffset"] != float64(tc.initial) || !reflect.DeepEqual(requests, tc.requests) {
					t.Errorf("attached/fetch positions: %v / %v, want %v", frames[0], requests, tc.requests)
				}
				for _, frame := range frames {
					if frame["type"] == "stdout" && frame["data"] == tc.want && frame["nextOffset"] != float64(end) {
						t.Errorf("stdout offset must be absolute: %v", frame)
					}
				}
				requests = nil
				resumed := e.attachFrames(path + "?stdoutOffset=" + jsonNum(end))
				if len(resumed) != 2 || resumed[1]["type"] != terminal || !reflect.DeepEqual(requests, []int{end}) {
					t.Errorf("resume at end replayed output: %v, requests %v", resumed, requests)
				}
			})
		}
	}
}

func TestR17AttachRotationWithPendingUTF8(t *testing.T) {
	defer func(d time.Duration) { attachMaxOpen = d }(attachMaxOpen)
	attachMaxOpen = 0
	for _, running := range []bool{false, true} {
		terminal := "exit"
		if running {
			terminal = "reconnect"
		}
		t.Run(terminal, func(t *testing.T) {
			e, path := r15Command(t)
			var requests []int
			complete := false
			r17LogSource(t, e, func(offset int) (int, string, bool) {
				requests = append(requests, offset)
				if len(requests) > 6 {
					t.Errorf("unbounded log replay: %v", requests)
					return offset, "", false
				}
				if offset == 0 {
					return 0, "a\xe2", true
				}
				log := "\x82\xacTAIL\xf0"
				if complete {
					log = "\x82\xacTAIL😀z"
				}
				start := max(8, min(offset, 8+len(log)))
				return start, log[start-8:], running && !complete
			})
			frames := e.attachFrames(path + "?stderrOffset=9")
			want, end := "a��TAIL", 14
			wantOffsets := []int{1, 2, 14}
			if !running {
				want, end = want+"�", 15
				wantOffsets = append(wantOffsets, end)
			}
			if got := r15Output(frames, "stdout"); got != want {
				t.Errorf("bytes across a gap formed a false character: stdout %q, want %q; %v", got, want, frames)
			}
			var offsets []int
			for _, frame := range frames {
				if frame["type"] == "stdout" {
					offsets = append(offsets, int(frame["nextOffset"].(float64)))
				}
			}
			last := frames[len(frames)-1]
			if !reflect.DeepEqual(offsets, wantOffsets) || !reflect.DeepEqual(requests, []int{0, 2, 15}) || last["type"] != terminal || last["stdoutOffset"] != float64(end) || last["stderrOffset"] != float64(9) {
				t.Errorf("rotation offsets/requests: %v / %v; terminal %v", offsets, requests, last)
			}
			if running {
				complete, requests = true, nil
				resumed := e.attachFrames(path + "?stdoutOffset=" + jsonNum(end))
				if got := r15Output(resumed, "stdout"); got != "😀z" || !reflect.DeepEqual(requests, []int{14, 19}) || resumed[len(resumed)-1]["stdoutOffset"] != float64(19) {
					t.Errorf("new pending bytes were acknowledged early: %v, requests %v", resumed, requests)
				}
			}
		})
	}
}

func TestR17NativeFollowCursorResets(t *testing.T) {
	defer func(d time.Duration) { followGap = d }(followGap)
	followGap = 5 * time.Millisecond

	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	for _, tc := range []struct {
		name         string
		steps        func(call, offset int) (map[string]any, int)
		wantRequests []int
		wantFrames   []map[string]any
	}{
		{
			name: "9999-to-0-poll-output",
			steps: func(call, offset int) (map[string]any, int) {
				switch call {
				case 0:
					return map[string]any{"ok": true, "offset": 0, "next_offset": 0, "data_b64": "", "running": true}, 200
				case 1:
					return map[string]any{"ok": true, "offset": 0, "next_offset": 6, "data_b64": b64("fresh\n"), "running": false}, 200
				default:
					return map[string]any{"ok": true, "offset": 6, "next_offset": 6, "data_b64": "", "running": false}, 200
				}
			},
			wantRequests: []int{9999, 0, 6},
			wantFrames: []map[string]any{
				{"stream": "stdout", "data_b64": b64("fresh\n"), "next_offset": float64(6)},
				{"exit": float64(0)},
			},
		},
		{
			name: "9999-to-0-reconnect",
			steps: func(call, offset int) (map[string]any, int) {
				if call == 0 {
					return map[string]any{"ok": true, "offset": 0, "next_offset": 0, "data_b64": "", "running": true}, 200
				}
				return nil, http.StatusBadGateway
			},
			wantRequests: []int{9999, 0},
			wantFrames: []map[string]any{
				{"reconnect": true, "next_offset": float64(0)},
			},
		},
		{
			name: "9999-to-1004-poll-output",
			steps: func(call, offset int) (map[string]any, int) {
				switch call {
				case 0:
					return map[string]any{"ok": true, "offset": 1004, "next_offset": 1004, "data_b64": "", "running": true}, 200
				case 1:
					return map[string]any{"ok": true, "offset": 1004, "next_offset": 1009, "data_b64": b64("tail\n"), "running": false}, 200
				default:
					return map[string]any{"ok": true, "offset": 1009, "next_offset": 1009, "data_b64": "", "running": false}, 200
				}
			},
			wantRequests: []int{9999, 1004, 1009},
			wantFrames: []map[string]any{
				{"stream": "stdout", "data_b64": b64("tail\n"), "next_offset": float64(1009)},
				{"exit": float64(0)},
			},
		},
		{
			name: "9999-to-1004-reconnect",
			steps: func(call, offset int) (map[string]any, int) {
				if call == 0 {
					return map[string]any{"ok": true, "offset": 1004, "next_offset": 1004, "data_b64": "", "running": true}, 200
				}
				return nil, http.StatusBadGateway
			},
			wantRequests: []int{9999, 1004},
			wantFrames: []map[string]any{
				{"reconnect": true, "next_offset": float64(1004)},
			},
		},
		{
			name: "absent-next-offset-preserved",
			steps: func(call, offset int) (map[string]any, int) {
				switch call {
				case 0:
					return map[string]any{"ok": true, "data_b64": "", "running": true}, 200
				case 1:
					return map[string]any{"ok": true, "data_b64": b64("kept\n"), "running": true}, 200
				default:
					return nil, http.StatusBadGateway
				}
			},
			wantRequests: []int{9999, 9999, 9999},
			wantFrames: []map[string]any{
				{"stream": "stdout", "data_b64": b64("kept\n"), "next_offset": float64(9999)},
				{"reconnect": true, "next_offset": float64(9999)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			vmID := e.createVM()
			st, started := e.do(e.owner, "POST", "/v1/vms/"+vmID+"/commands", map[string]any{"cmd": []string{"echo", "hi"}})
			if st != 201 {
				t.Fatalf("start command: %d %v", st, started)
			}
			cid := started["id"].(string)

			var mu sync.Mutex
			var requests []int
			upstream := e.srv.boatd
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var args map[string]any
				if len(body) > 0 {
					if err := json.Unmarshal(body, &args); err != nil {
						t.Error(err)
						return
					}
				}
				if args["op"] == "cmd_logs" {
					off := int(args["offset"].(float64))
					mu.Lock()
					call := len(requests)
					requests = append(requests, off)
					mu.Unlock()
					if call >= 6 {
						t.Errorf("unbounded follow polling: %v", requests)
						writeJSON(w, 200, map[string]any{"ok": true, "data_b64": "", "running": false})
						return
					}
					reply, code := tc.steps(call, off)
					if code != 200 {
						w.WriteHeader(code)
						return
					}
					writeJSON(w, 200, reply)
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

			code, raw := e.raw(e.owner, "GET", "/v1/vms/"+vmID+"/commands/"+cid+"/logs?follow=1&offset=9999", nil)
			if code != 200 {
				t.Fatalf("GET follow logs: %d %s", code, raw)
			}
			gotFrames := r15Frames(t, string(raw))
			mu.Lock()
			gotRequests := append([]int(nil), requests...)
			mu.Unlock()
			if !reflect.DeepEqual(gotRequests, tc.wantRequests) {
				t.Errorf("requests = %v, want %v", gotRequests, tc.wantRequests)
			}
			if !reflect.DeepEqual(gotFrames, tc.wantFrames) {
				t.Errorf("frames = %v, want %v", gotFrames, tc.wantFrames)
			}
		})
	}
}
