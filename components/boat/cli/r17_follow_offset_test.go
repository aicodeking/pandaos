package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestR17FollowLogsOffsets(t *testing.T) {
	for _, command := range []struct{ verb, collection, id string }{
		{"cmd", "commands", "c_1"},
		{"prompts", "prompts", "prm_1"},
	} {
		for _, initial := range []int{0, 123} {
			for _, asJSON := range []bool{false, true} {
				for _, terminal := range []struct {
					name  string
					event obj
					code  int
					err   string
				}{
					{"success", obj{"exit": 0}, 0, ""},
					{"failure", obj{"exit": 23}, 23, ""},
					{"signal", obj{"signal": "TERM"}, 128, "boat: the command was killed by TERM\n"},
					{"lost", nil, 1, "boat: the command ended with no exit status (lost)\n"},
				} {
					name := command.verb + "/" + strconv.Itoa(initial) + "/json=" + strconv.FormatBool(asJSON) + "/" + terminal.name
					t.Run(name, func(t *testing.T) {
						chunks := []string{"first €\n", "second\n", "last\n"}
						// The API's absolute cursor can jump past trimmed output.
						end1 := initial + 4096 + len(chunks[0])
						resume1 := end1 + 2048
						end2 := resume1 + len(chunks[1])
						chunk := func(data string, next int) obj {
							return obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte(data)), "next_offset": next}
						}
						frames := [][]obj{
							{chunk(chunks[0], end1), {"reconnect": true, "next_offset": resume1}},
							{chunk(chunks[1], end2), {"reconnect": true}},
							{chunk(chunks[2], end2+len(chunks[2]))},
						}
						if terminal.event != nil {
							frames[2] = append(frames[2], terminal.event)
						}
						wantQueries := []string{
							"follow=1&offset=" + strconv.Itoa(initial),
							"follow=1&offset=" + strconv.Itoa(resume1),
							"follow=1&offset=" + strconv.Itoa(end2),
						}
						var mu sync.Mutex
						var queries []string
						mux := http.NewServeMux()
						mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
							json.NewEncoder(w).Encode(vm1)
						})
						mux.HandleFunc("GET "+vmPath("vm1", command.collection, command.id, "logs"), func(w http.ResponseWriter, r *http.Request) {
							mu.Lock()
							n := len(queries)
							queries = append(queries, r.URL.RawQuery)
							mu.Unlock()
							if n >= len(frames) || r.URL.RawQuery != wantQueries[n] {
								t.Errorf("log request %d: %s; want queries %v", n, r.URL.RawQuery, wantQueries)
								http.Error(w, "wrong log offset", http.StatusBadRequest)
								return
							}
							w.Header().Set("Content-Type", "application/x-ndjson")
							for _, event := range frames[n] {
								json.NewEncoder(w).Encode(event)
							}
							// Reconnect ends this response; unread frames must not be replayed.
							if n < 2 {
								json.NewEncoder(w).Encode(chunk("unread", end2+1000))
							}
						})
						srv := httptest.NewServer(mux)
						defer srv.Close()
						h := newHarness(t, &jobAPI{srv: srv})
						args := []string{command.verb, "logs", "vm1", command.id, "--offset", strconv.Itoa(initial), "--follow"}
						if asJSON {
							args = append(args, "--json")
						}
						if code := h.a.run(args); code != terminal.code {
							t.Errorf("exit %d, want %d; stderr %s", code, terminal.code, h.err)
						}
						mu.Lock()
						gotQueries := slices.Clone(queries)
						mu.Unlock()
						if !slices.Equal(gotQueries, wantQueries) {
							t.Errorf("queries %v, want %v", gotQueries, wantQueries)
						}
						wantOutput := strings.Join(chunks, "")
						if asJSON {
							var out bytes.Buffer
							for _, response := range frames {
								for _, event := range response {
									json.NewEncoder(&out).Encode(event)
								}
							}
							wantOutput = out.String()
						}
						if h.out.String() != wantOutput || h.err.String() != terminal.err {
							t.Errorf("output %q, stderr %q; want output %q, stderr %q", h.out, h.err, wantOutput, terminal.err)
						}
					})
				}
			}
		}
	}
}

func TestR17FollowLaunchStartsAtZero(t *testing.T) {
	for _, launch := range []struct {
		name, collection, id string
		args                 []string
	}{
		{"command", "commands", "c_1", []string{"cmd", "start", "vm1", "--follow", "--", "true"}},
		{"prompt", "prompts", "prm_1", []string{"prompt", "vm1", "hi", "--new"}},
		{"new-setup", "commands", "setup1", []string{"new", "--wait-setup"}},
		{"run-setup", "commands", "setup1", []string{"run", "--keep", "--", "true"}},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(launch.name+"/json="+strconv.FormatBool(asJSON), func(t *testing.T) {
				var mu sync.Mutex
				var queries []string
				mux := http.NewServeMux()
				mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(vm1)
				})
				mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(obj{"id": "vm1", "state": "running", "setup_cmd_id": "setup1"})
				})
				mux.HandleFunc("POST /v1/vms/vm1/commands", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(obj{"id": "c_1"})
				})
				mux.HandleFunc("POST /v1/vms/vm1/prompts", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(obj{"id": "prm_1", "conversation_id": "conv1"})
				})
				mux.HandleFunc("POST /v1/vms/vm1/exec", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(obj{"exit": 0})
				})
				mux.HandleFunc("GET "+vmPath("vm1", launch.collection, launch.id, "logs"), func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					n := len(queries)
					queries = append(queries, r.URL.RawQuery)
					mu.Unlock()
					if n == 0 {
						json.NewEncoder(w).Encode(obj{"reconnect": true, "next_offset": 700})
					} else {
						json.NewEncoder(w).Encode(obj{"exit": 0})
					}
				})
				srv := httptest.NewServer(mux)
				defer srv.Close()
				h := newHarness(t, &jobAPI{srv: srv})
				args := slices.Clone(launch.args)
				if asJSON {
					// Keep the global flag before the command's literal arguments.
					args = append([]string{"--json"}, args...)
				}
				if code := h.a.run(args); code != 0 {
					t.Errorf("exit %d, stderr %s", code, h.err)
				}
				mu.Lock()
				gotQueries := slices.Clone(queries)
				mu.Unlock()
				if want := []string{"follow=1&offset=0", "follow=1&offset=700"}; !slices.Equal(gotQueries, want) {
					t.Errorf("queries %v, want %v", gotQueries, want)
				}
			})
		}
	}
}

func TestR17NegativeLogOffsetReachesAPIValidation(t *testing.T) {
	for _, verb := range []string{"cmd", "prompts"} {
		for _, follow := range []bool{false, true} {
			t.Run(verb+"/follow="+strconv.FormatBool(follow), func(t *testing.T) {
				collection := "commands"
				if verb == "prompts" {
					collection = "prompts"
				}
				mux := http.NewServeMux()
				mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(vm1)
				})
				mux.HandleFunc("GET "+vmPath("vm1", collection, "run1", "logs"), func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("offset") != "-1" {
						t.Errorf("offset %q, want -1", r.URL.Query().Get("offset"))
					}
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(obj{"error": obj{"code": "invalid_request", "message": "offset must be non-negative"}})
				})
				srv := httptest.NewServer(mux)
				defer srv.Close()
				h := newHarness(t, &jobAPI{srv: srv})
				args := []string{verb, "logs", "vm1", "run1", "--offset=-1"}
				if follow {
					args = append(args, "--follow")
				}
				if code := h.a.run(args); code != 1 || h.out.Len() != 0 || h.err.String() != "boat: offset must be non-negative (HTTP 400)\n" {
					t.Errorf("exit %d, output %q, stderr %q", code, h.out, h.err)
				}
			})
		}
	}
}

func TestR17FollowAuthoritativeCursorResets(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	chunk := func(data string, next int) obj {
		return obj{"stream": "stdout", "data_b64": b64(data), "next_offset": next}
	}

	for _, command := range []struct{ verb, collection, id string }{
		{"cmd", "commands", "c_1"},
		{"prompts", "prompts", "prm_1"},
	} {
		for _, tc := range []struct {
			name        string
			frames      [][]obj
			wantQueries []string
			wantOutput  string
		}{
			{
				name: "9999-to-0",
				frames: [][]obj{
					{{"reconnect": true, "next_offset": 0}},
					{chunk("zero\n", 5), {"exit": 0}},
				},
				wantQueries: []string{
					"follow=1&offset=9999",
					"follow=1&offset=0",
				},
				wantOutput: "zero\n",
			},
			{
				name: "9999-to-1004",
				frames: [][]obj{
					{{"reconnect": true, "next_offset": 1004}},
					{chunk("clamped\n", 1012), {"reconnect": true}},
					{chunk("done\n", 1017), {"exit": 0}},
				},
				wantQueries: []string{
					"follow=1&offset=9999",
					"follow=1&offset=1004",
					"follow=1&offset=1012",
				},
				wantOutput: "clamped\ndone\n",
			},
			{
				name: "absent-next-offset",
				frames: [][]obj{
					{{"reconnect": true}},
					{{"stream": "stdout", "data_b64": b64("kept\n")}, {"reconnect": true, "next_offset": nil}},
					{{"exit": 0}},
				},
				wantQueries: []string{
					"follow=1&offset=9999",
					"follow=1&offset=9999",
					"follow=1&offset=9999",
				},
				wantOutput: "kept\n",
			},
		} {
			t.Run(command.verb+"/"+tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var queries []string
				mux := http.NewServeMux()
				mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(vm1)
				})
				mux.HandleFunc("GET "+vmPath("vm1", command.collection, command.id, "logs"), func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					n := len(queries)
					queries = append(queries, r.URL.RawQuery)
					mu.Unlock()
					if n >= len(tc.frames) || r.URL.RawQuery != tc.wantQueries[n] {
						t.Errorf("log request %d: %s; want queries %v", n, r.URL.RawQuery, tc.wantQueries)
						http.Error(w, "wrong log offset", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/x-ndjson")
					for _, ev := range tc.frames[n] {
						json.NewEncoder(w).Encode(ev)
					}
				})
				srv := httptest.NewServer(mux)
				defer srv.Close()
				h := newHarness(t, &jobAPI{srv: srv})
				args := []string{command.verb, "logs", "vm1", command.id, "--offset", "9999", "--follow"}
				if code := h.a.run(args); code != 0 {
					t.Errorf("exit %d, stderr %s", code, h.err)
				}
				mu.Lock()
				gotQueries := slices.Clone(queries)
				mu.Unlock()
				if !slices.Equal(gotQueries, tc.wantQueries) {
					t.Errorf("queries %v, want %v", gotQueries, tc.wantQueries)
				}
				if h.out.String() != tc.wantOutput || h.err.Len() != 0 {
					t.Errorf("output %q, stderr %q; want %q", h.out.String(), h.err.String(), tc.wantOutput)
				}
			})
		}
	}
}

func TestR17FollowReconnectOutageRateLimit(t *testing.T) {
	const (
		rate   = 10.0
		burst  = 40.0
		outage = 1200 * time.Millisecond
	)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	var (
		mu             sync.Mutex
		tokens         = burst
		last           = time.Now()
		outageUntil    time.Time
		postOutageSent bool
		postOutageAt   time.Time
		afterResetGap  time.Duration
		logRequests    int
		got429         atomic.Bool
	)

	allow := func() bool {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		tokens = min(burst, tokens+now.Sub(last).Seconds()*rate)
		last = now
		if tokens < 1 {
			return false
		}
		tokens--
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(vm1)
	})
	mux.HandleFunc("GET /v1/vms/vm1/commands/c_1/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)

		mu.Lock()
		logRequests++
		reqNum := logRequests
		now := time.Now()
		if reqNum == 1 {
			outageUntil = now.Add(outage)
			mu.Unlock()
			enc.Encode(obj{"stream": "stdout", "data_b64": b64("before\n"), "next_offset": 7})
			enc.Encode(obj{"reconnect": true, "next_offset": 7})
			return
		}
		if now.Before(outageUntil) {
			mu.Unlock()
			enc.Encode(obj{"reconnect": true, "next_offset": 7})
			return
		}
		if !postOutageSent {
			postOutageSent = true
			postOutageAt = now
			mu.Unlock()
			enc.Encode(obj{"stream": "stdout", "data_b64": b64("middle\n"), "next_offset": 14})
			enc.Encode(obj{"reconnect": true, "next_offset": 14})
			return
		}
		afterResetGap = now.Sub(postOutageAt)
		mu.Unlock()
		enc.Encode(obj{"stream": "stdout", "data_b64": b64("after\n"), "next_offset": 20})
		enc.Encode(obj{"exit": 0})
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allow() {
			got429.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(obj{"error": obj{"code": "rate_limited", "message": "rate limit exceeded for this key"}})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	h := newHarness(t, &jobAPI{srv: srv})
	code := h.a.run([]string{"cmd", "logs", "vm1", "c_1", "--follow"})
	if got429.Load() {
		t.Fatalf("CLI exhausted 10 rps / burst 40 limiter during >1s reconnect outage (%d log requests): stderr=%q", logRequests, h.err.String())
	}
	if code != 0 || h.err.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.err.String())
	}
	if want := "before\nmiddle\nafter\n"; h.out.String() != want {
		t.Errorf("output = %q, want %q", h.out.String(), want)
	}
	mu.Lock()
	reqs, gap := logRequests, afterResetGap
	mu.Unlock()
	if reqs < 3 || reqs > 12 {
		t.Errorf("expected bounded backoff request count during 1.2s outage, got %d", reqs)
	}
	if gap <= 0 || gap >= 600*time.Millisecond {
		t.Errorf("expected reconnect backoff to reset after data progress, got gap %v", gap)
	}
}
