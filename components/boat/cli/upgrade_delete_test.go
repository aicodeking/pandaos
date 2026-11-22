package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvUpgradeTargetsAndResults(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		targets        []string
		json           bool
		code           int
		wantIDs        []string
	}{
		{"all", `{"success":true,"upgraded":2,"upgradedLive":1,"upgradedStopped":1,"failed":0}`, nil, false, 0, nil},
		{"targets", `{"success":true,"upgraded":2,"upgradedLive":1,"upgradedStopped":1,"failed":0}`, []string{"vm1", "vm2"}, true, 0, []string{"vm1", "vm2"}},
		{"names", `{"success":true,"upgraded":1,"upgradedLive":1,"upgradedStopped":0,"failed":0}`, []string{"dev"}, false, 0, []string{"vm1"}},
		{"partial text", `{"success":false,"upgraded":1,"upgradedLive":1,"upgradedStopped":0,"failed":1}`, nil, false, 1, nil},
		{"partial json", `{"success":false,"upgraded":1,"upgradedLive":1,"upgradedStopped":0,"failed":1}`, nil, true, 1, nil},
		{"failed count", `{"success":true,"upgraded":0,"upgradedLive":0,"upgradedStopped":0,"failed":2}`, nil, true, 1, nil},
		{"false success", `{"success":false,"upgraded":0,"upgradedLive":0,"upgradedStopped":0,"failed":0}`, nil, false, 1, nil},
		{"invalid target", `{}`, []string{""}, false, 2, nil},
		{"missing target", `{}`, []string{"missing"}, false, 1, nil},
		{"invalid second target", `{}`, []string{"vm1", "missing"}, false, 1, nil},
		{"unknown flag", `{}`, []string{"--bogus"}, false, 2, nil},
		{"trailing command", `{}`, []string{"--", "vm2"}, false, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted bool
			var gotIDs []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && (r.URL.Path == "/v1/vms/vm1" || r.URL.Path == "/v1/vms/vm2"):
					json.NewEncoder(w).Encode(obj{"id": strings.TrimPrefix(r.URL.Path, "/v1/vms/")})
				case r.Method == "GET" && r.URL.Path == "/v1/vms":
					io.WriteString(w, `{"vms":[{"id":"vm1","name":"dev"}]}`)
				case r.Method == "GET":
					w.WriteHeader(404)
					io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
				case r.Method == "POST" && r.URL.Path == "/api/v1/environments/base/upgrade":
					posted = true
					body, _ := io.ReadAll(r.Body)
					if len(body) > 0 {
						var req struct {
							AgentIDs []string `json:"agentIds"`
						}
						if err := json.Unmarshal(body, &req); err != nil {
							t.Error(err)
						}
						gotIDs = req.AgentIDs
					}
					io.WriteString(w, tc.response)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			a, out, errw := upgradeDeleteApp(t, srv.URL)
			args := append([]string{"env", "upgrade", "base"}, tc.targets...)
			if tc.json {
				args = append(args, "--json")
			}
			if code := a.run(args); code != tc.code {
				t.Errorf("exit %d, want %d; stdout %s stderr %s", code, tc.code, out, errw)
			}
			wantPost := tc.response != `{}`
			if posted != wantPost || !reflect.DeepEqual(gotIDs, tc.wantIDs) {
				t.Errorf("posted=%v IDs=%v, want posted=%v IDs=%v", posted, gotIDs, wantPost, tc.wantIDs)
			}
			if !wantPost {
				return
			}
			if tc.json {
				lines := strings.Split(strings.TrimSpace(out.String()), "\n")
				if lines[0] != tc.response {
					var got, want obj
					json.Unmarshal([]byte(lines[0]), &got)
					json.Unmarshal([]byte(tc.response), &want)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("rollout response lost: %s", out)
					}
				}
				if tc.code != 0 && !strings.Contains(lines[len(lines)-1], `"event":"error"`) {
					t.Errorf("missing final error event: %s", out)
				}
			} else {
				var response obj
				json.Unmarshal([]byte(tc.response), &response)
				want := "env base: " + numStr(response, "upgraded") + " upgraded (" + numStr(response, "upgradedLive") + " live, " + numStr(response, "upgradedStopped") + " stopped), " + numStr(response, "failed") + " failed\n"
				if out.String() != want {
					t.Errorf("rollout counts %q, want %q", out.String(), want)
				}
			}
		})
	}
}

func TestDeleteInteractiveConfirmation(t *testing.T) {
	for _, verb := range []string{"delete", "rm"} {
		for _, tc := range []struct {
			name, input     string
			json, confirmed bool
		}{
			{"yes", "yes\n", false, true},
			{"y", "y\n", false, true},
			{"case and whitespace", "  YES\r\n", false, true},
			{"declined", "no\n", false, false},
			{"default no", "\n", false, false},
			{"missing", "", false, false},
			{"incomplete line", "yes", false, false},
			{"oversized", strings.Repeat("y", 256) + "\n", false, false},
			{"json refuses prompt", "yes\n", true, false},
		} {
			t.Run(verb+"/"+tc.name, func(t *testing.T) {
				deletes := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "DELETE" {
						deletes++
					}
					json.NewEncoder(w).Encode(obj{"id": "vm1"})
				}))
				defer srv.Close()
				a, out, errw := upgradeDeleteApp(t, srv.URL)
				a.in = strings.NewReader(tc.input)
				a.isTerminal = func() bool { return true }
				args := []string{verb, "dev"}
				if tc.json {
					args = append(args, "--json")
				}
				code := a.run(args)
				if tc.confirmed {
					if code != 0 || deletes != 1 {
						t.Errorf("confirmed: exit=%d deletes=%d stderr=%s", code, deletes, errw)
					}
				} else if code == 0 || deletes != 0 {
					t.Errorf("unconfirmed: exit=%d deletes=%d stdout=%s stderr=%s", code, deletes, out, errw)
				}
				if tc.json {
					if strings.Contains(errw.String(), "[y/N]") || !strings.Contains(out.String(), `"event":"error"`) {
						t.Errorf("JSON confirmation: stdout=%s stderr=%s", out, errw)
					}
				} else if !strings.Contains(errw.String(), "Permanently delete vm1? [y/N]") {
					t.Errorf("missing resolved-ID confirmation: %s", errw)
				}
			})
		}
	}
}

func upgradeDeleteApp(t *testing.T, base string) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	env := map[string]string{"BOAT_URL": base, "BOAT_API_KEY": "k", "BOAT_CONFIG": filepath.Join(t.TempDir(), "config.json")}
	return &app{out: out, errw: errw, in: strings.NewReader("yes\n"), getenv: func(k string) string { return env[k] }}, out, errw
}

func TestDeleteRequiresExplicitConfirmation(t *testing.T) {
	for _, verb := range []string{"delete", "rm"} {
		for _, flags := range [][]string{nil, {"--json"}, {"--yes=false"}, {"--yes"}, {"-y", "--json"}} {
			t.Run(verb+strings.Join(flags, " "), func(t *testing.T) {
				deletes := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "DELETE" {
						deletes++
					}
					json.NewEncoder(w).Encode(obj{"id": "vm1"})
				}))
				defer srv.Close()
				a, out, errw := upgradeDeleteApp(t, srv.URL)
				code := a.run(append([]string{verb, "vm1"}, flags...))
				confirmed := len(flags) > 0 && (flags[0] == "--yes" || flags[0] == "-y")
				if confirmed {
					if code != 0 || deletes != 1 {
						t.Errorf("confirmed: exit=%d deletes=%d stderr=%s", code, deletes, errw)
					}
				} else if code == 0 || deletes != 0 || !strings.Contains(errw.String(), "--yes") {
					t.Errorf("unconfirmed: exit=%d deletes=%d stdout=%s stderr=%s", code, deletes, out, errw)
				}
			})
		}
	}
}
