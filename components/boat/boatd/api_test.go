package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, srv *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if s, ok := body.(string); ok {
		r = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if p, ok := out.(*[]byte); ok {
			*p = b
		} else if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func TestAPI(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.PortMin, c.PortMax = 20000, 20001 })
	srv := httptest.NewServer(m.routes())
	defer srv.Close()

	var v VM
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "web", "team": "t1", "labels": map[string]string{"a": "b"}}, &v); c != 201 || v.State != Running {
		t.Fatalf("create: %d %+v", c, v)
	}
	var e map[string]any
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "Nope!"}, &e); c != 400 || e["error"] == "" {
		t.Fatalf("bad name: %d %v", c, e)
	}
	if c := call(t, srv, "GET", "/v1/vms/vm-nope", nil, &e); c != 404 {
		t.Fatalf("unknown: %d", c)
	}
	var list struct{ VMs []VM }
	call(t, srv, "GET", "/v1/vms?team=t1", nil, &list)
	call(t, srv, "GET", "/v1/vms?team=t2", nil, &struct{}{})
	if len(list.VMs) != 1 || list.VMs[0].ID != v.ID {
		t.Fatalf("list: %+v", list)
	}

	// exec streams the guest's lines as NDJSON, ending with exit.
	var out []byte
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/exec", map[string]any{"cmd": []string{"echo", "hi"}}, &out); c != 200 {
		t.Fatalf("exec: %d %s", c, out)
	}
	var lines []reply
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var r reply
		json.Unmarshal(sc.Bytes(), &r)
		lines = append(lines, r)
	}
	if len(lines) != 3 || lines[0].Data != b64("echo hi") || lines[1].Stream != "stderr" || lines[2].Exit == nil || *lines[2].Exit != 0 {
		t.Fatalf("exec stream: %s", out)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/exec", map[string]any{}, &e); c != 400 {
		t.Fatalf("empty argv: %d", c)
	}

	// files: 2.5 MiB goes over as 3 put_file chunks and comes back intact.
	data := strings.Repeat("0123456789abcdef", 5<<16/2)
	if c := call(t, srv, "PUT", "/v1/vms/"+v.ID+"/files?path=/tmp/big&mode=600", data, &e); c != 200 {
		t.Fatalf("put: %d %v", c, e)
	}
	var got []byte
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/files?path=/tmp/big", nil, &got); c != 200 || string(got) != data {
		t.Fatalf("get: %d len %d want %d", c, len(got), len(data))
	}
	if c := call(t, srv, "PUT", "/v1/vms/"+v.ID+"/files?path=/tmp/empty", "", &e); c != 200 {
		t.Fatalf("put empty: %d", c)
	}
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/files?path=/nope", nil, &e); c != 404 {
		t.Fatalf("missing file: %d", c)
	}
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/files?path=rel", nil, &e); c != 400 {
		t.Fatalf("relative path: %d", c)
	}

	// This general API fixture models a separately verified SSH policy. Dedicated
	// SSH tests assert an unverified new VM never publishes routes.
	if _, err := m.update(v.ID, "", func(x *VM) { x.SSHPolicyReady = true }); err != nil {
		t.Fatal(err)
	}
	// ports: hostname route always, host port only when public; routes.json follows.
	var p Port
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/ports", map[string]any{"port": 8080, "public": true}, &p); c != 201 || p.HostPort != 20000 || p.Hostname != "8080-"+v.ID {
		t.Fatalf("port: %d %+v", c, p)
	}
	var p2 Port
	call(t, srv, "POST", "/v1/vms/"+v.ID+"/ports", map[string]any{"guest_port": 3000}, &p2)
	if p2.HostPort != 0 || p2.GuestPort != 3000 {
		t.Fatalf("private port got a host port: %+v", p2)
	}
	var rf routeFile
	readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf)
	if r := rf.VMs[v.ID]; r.IP != v.IP || len(r.Ports) != 2 || r.DesktopPort != 6080 || len(r.Public) != 1 || r.Public[0] != 8080 || r.Ports[0] != 3000 {
		t.Fatalf("routes.json: %+v", rf)
	}
	if c := call(t, srv, "DELETE", "/v1/vms/"+v.ID+"/ports?port=8080", nil, nil); c != 200 {
		t.Fatalf("delete port: %d", c)
	}
	call(t, srv, "GET", "/v1/vms/"+v.ID, nil, &v)
	if len(v.Ports) != 1 || v.Ports[0].GuestPort != 3000 {
		t.Fatalf("ports after delete: %+v", v.Ports)
	}

	// End the synthetic route fixture before exercising snapshot lifecycle.
	// A real ready VM carries verified runtime config, covered by SSH tests.
	if _, err := m.update(v.ID, "", func(x *VM) { x.SSHPolicyReady = false }); err != nil {
		t.Fatal(err)
	}
	// snapshots + fork + stop/start through the API.
	var s Snapshot
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/snapshots", map[string]string{"name": "s1"}, &s); c != 201 || s.VMID != v.ID {
		t.Fatalf("snapshot: %d %+v", c, s)
	}
	var snaps struct{ Snapshots []Snapshot }
	call(t, srv, "GET", "/v1/vms/"+v.ID+"/snapshots", nil, &snaps)
	if len(snaps.Snapshots) != 1 {
		t.Fatalf("snapshots: %+v", snaps)
	}
	var f VM
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/fork", map[string]string{"name": "web2", "team": "t1"}, &f); c != 201 || f.Name != "web2" {
		t.Fatalf("fork: %d %+v", c, f)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/stop", nil, &v); c != 200 || v.State != Stopped {
		t.Fatalf("stop: %d %+v", c, v)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/exec", map[string]any{"argv": []string{"x"}}, &e); c != 409 {
		t.Fatalf("exec on stopped: %d", c)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/start", map[string]any{"env": map[string]string{"A": "1"}}, &v); c != 200 || v.State != Running {
		t.Fatalf("start: %d %+v", c, v)
	}
	if c := call(t, srv, "DELETE", "/v1/vms/"+v.ID+"/snapshots/"+s.ID, nil, nil); c != 200 {
		t.Fatalf("delete snapshot: %d", c)
	}

	var u usageReport
	if c := call(t, srv, "GET", "/v1/usage?since=0&team=t1", nil, &u); c != 200 || len(u.Teams) != 1 || u.Teams[0].VMSeconds <= 0 {
		t.Fatalf("usage: %d %+v", c, u)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+f.ID+"/delete", nil, nil); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	if c := call(t, srv, "DELETE", "/v1/vms/"+v.ID, nil, nil); c != 200 {
		t.Fatal(c)
	}
	if _, err := os.Stat(m.vmDir(v.ID)); !os.IsNotExist(err) {
		t.Fatal("vm dir not removed")
	}
}

func TestEventsSSE(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	m.ev.publish(Event{Type: "vm.state", VMID: "a", Team: "t1", State: Running})
	m.ev.publish(Event{Type: "vm.state", VMID: "b", Team: "t2", State: Running})
	m.ev.publish(Event{Type: "vm.deleted", VMID: "a", Team: "t1"})
	req, _ := http.NewRequest("GET", srv.URL+"/v1/events?team=t1", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	var got []string
	for len(got) < 3 {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l != "\n" {
			got = append(got, strings.TrimSpace(l))
		}
	}
	if got[0] != "id: 3" || got[1] != "event: vm.deleted" || !strings.Contains(got[2], `"vm_id":"a"`) {
		t.Fatalf("sse: %v", got)
	}
}

func TestSummarize(t *testing.T) {
	t0 := mustTime("2026-01-01T00:00:00Z")
	recs := []usageRec{
		{"vm", "a", "t", "small", t0, t0.Add(3600e9)},
		{"vm", "a", "t", "small", t0.Add(7200e9), t0.Add(9000e9)},
		{"snapshot", "s", "t", "small", t0, t0.Add(86400e9 * 2)},
	}
	rep := summarize(recs, t0.Add(1800e9), t0.Add(86400e9))
	if rep.VMs[0].Seconds != 1800+1800 || rep.Teams[0].BySize["small"] != 3600 {
		t.Fatalf("%+v", rep)
	}
	if d := rep.Snapshots[0].Days; d < 0.979 || d > 0.98 {
		t.Fatalf("snapshot days %v", d)
	}
}

// A body that ends before its Content-Length is refused, and the guest file is not
// renamed into place: neither a client that went away nor a short reader commits it.
func TestPutFileShortBody(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var v VM
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "web", "team": "t1"}, &v); c != 201 {
		t.Fatalf("create: %d", c)
	}
	// 1.5 MiB promised, 1.25 MiB sent (one full chunk goes to the guest), then the client half-closes.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sent := fileChunk + fileChunk/4
	fmt.Fprintf(conn, "PUT /v1/vms/%s/files?path=/tmp/cut HTTP/1.1\r\nHost: boatd\r\nContent-Length: %d\r\n\r\n", v.ID, fileChunk+fileChunk/2)
	conn.Write(bytes.Repeat([]byte("x"), sent))
	conn.(*net.TCPConn).CloseWrite()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "short body") {
		t.Fatalf("short upload: %d %s", resp.StatusCode, b)
	}
	var got []byte
	for _, p := range []string{"/tmp/cut", "/tmp/cut.boat-part"} {
		if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/files?path="+p, nil, &got); c != 404 {
			t.Fatalf("%s after a short body: %d (%d bytes)", p, c, len(got))
		}
	}

	// A reader that ends early with a clean EOF, against its declared length.
	req := httptest.NewRequest("PUT", "/v1/vms/"+v.ID+"/files?path=/tmp/cut2", strings.NewReader("abc"))
	req.ContentLength = 10
	rec := httptest.NewRecorder()
	m.routes().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("short reader: %d %s", rec.Code, rec.Body)
	}
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/files?path=/tmp/cut2", nil, &got); c != 404 {
		t.Fatalf("short reader committed the file: %d", c)
	}
	// Control: the exact length still lands.
	if c := call(t, srv, "PUT", "/v1/vms/"+v.ID+"/files?path=/tmp/ok", "abc", nil); c != 200 {
		t.Fatalf("full body: %d", c)
	}
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/files?path=/tmp/ok", nil, &got); c != 200 || string(got) != "abc" {
		t.Fatalf("full body: %d %q", c, got)
	}
}

func TestVMsSSHSummaryEndpoints(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()

	var v1, v2 VM
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "one", "team": "t1", "labels": map[string]string{"env": "prod"}}, &v1); c != 201 {
		t.Fatalf("create v1: %d", c)
	}
	if _, err := m.AddPort(v1.ID, 8080, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.update(v1.ID, "", func(x *VM) {
		x.SSHPolicyReady = true
		x.SSHPolicyDigest = "digest-v1"
	}); err != nil {
		t.Fatal(err)
	}
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "two", "team": "t2"}, &v2); c != 201 {
		t.Fatalf("create v2: %d", c)
	}
	pending, err := m.alloc(VM{Name: "pending", Team: "t1", Size: "default", VCPU: 1, MemMiB: 512, DiskGiB: 1, PendingCopy: true}, Forking)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/v1/vms?view=ssh", "/v1/vms/ssh-summary"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d body %s", path, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), `"ports"`) || strings.Contains(string(raw), `"labels"`) {
			t.Fatalf("%s leaked heavy VM fields: %s", path, raw)
		}
		var out struct {
			VMs []map[string]any `json:"vms"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.VMs) != 2 {
			t.Fatalf("%s: expected 2 visible VMs (excluding pending_copy), got %d: %s", path, len(out.VMs), raw)
		}
		got1 := out.VMs[0]
		if got1["id"] != v1.ID || got1["team"] != "t1" || got1["state"] != string(Running) ||
			got1["ip"] != v1.IP || got1["agent_ready"] != true ||
			got1["ssh_boot_epoch"] != v1.SSHBootEpoch ||
			got1["ssh_policy_ready"] != true || got1["ssh_policy_digest"] != "digest-v1" {
			t.Fatalf("%s: unexpected v1 summary: %+v", path, got1)
		}
	}

	// Team filter + include_pending=true:
	var filtered struct {
		VMs []vmSSHSummary `json:"vms"`
	}
	if c := call(t, srv, "GET", "/v1/vms?view=ssh&team=t1&include_pending=true", nil, &filtered); c != 200 {
		t.Fatalf("filtered view=ssh: %d", c)
	}
	if len(filtered.VMs) != 2 || filtered.VMs[0].ID != v1.ID || filtered.VMs[1].ID != pending.ID || !filtered.VMs[1].PendingCopy {
		t.Fatalf("filtered include_pending view=ssh mismatch: %+v", filtered.VMs)
	}

	// Single-VM ?view=ssh avoids heavy VM fields while returning full SSH projection:
	resp, err := http.Get(srv.URL + "/v1/vms/" + v1.ID + "?view=ssh")
	if err != nil {
		t.Fatal(err)
	}
	singleRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("single view=ssh status=%d body=%s", resp.StatusCode, singleRaw)
	}
	if strings.Contains(string(singleRaw), `"ports"`) || strings.Contains(string(singleRaw), `"labels"`) || strings.Contains(string(singleRaw), `"env"`) {
		t.Fatalf("single view=ssh leaked heavy VM fields: %s", singleRaw)
	}
	var single vmSSHSummary
	if err := json.Unmarshal(singleRaw, &single); err != nil {
		t.Fatal(err)
	}
	if single.ID != v1.ID || single.Team != "t1" || single.State != Running || single.IP != v1.IP ||
		!single.AgentReady || single.PendingCopy || single.SSHBootEpoch != v1.SSHBootEpoch ||
		!single.SSHPolicyReady || single.SSHPolicyDigest != "digest-v1" {
		t.Fatalf("unexpected single view=ssh: %+v", single)
	}
	if c := call(t, srv, "GET", "/v1/vms/nonexistent?view=ssh", nil, &single); c != 404 {
		t.Fatalf("missing VM view=ssh: %d", c)
	}
	if c := call(t, srv, "GET", "/v1/vms/"+pending.ID+"?view=ssh", nil, &single); c != 404 {
		t.Fatalf("pending VM view=ssh without include_pending: %d", c)
	}
	if c := call(t, srv, "GET", "/v1/vms/"+pending.ID+"?view=ssh&include_pending=true", nil, &single); c != 200 || !single.PendingCopy || single.ID != pending.ID {
		t.Fatalf("pending VM view=ssh with include_pending: %d %+v", c, single)
	}
}

func TestDeleteSnapshotByIDNotFound(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()

	// Missing snapshot ID must return 404 cleanly (no nil deref or 500).
	var errResp map[string]any
	if c := call(t, srv, "DELETE", "/v1/snapshots/snap-does-not-exist", nil, &errResp); c != 404 {
		t.Fatalf("DELETE missing snapshot: status=%d body=%v, want 404", c, errResp)
	}

	// Existing snapshot ID deletes cleanly via DELETE /v1/snapshots/{sid}.
	var v VM
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "sdel", "team": "t1"}, &v); c != 201 {
		t.Fatalf("create: %d", c)
	}
	var s Snapshot
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/snapshots", map[string]string{"name": "snap1"}, &s); c != 201 {
		t.Fatalf("snapshot: %d", c)
	}
	var okResp map[string]bool
	if c := call(t, srv, "DELETE", "/v1/snapshots/"+s.ID, nil, &okResp); c != 200 || !okResp["ok"] {
		t.Fatalf("DELETE existing snapshot: status=%d body=%v", c, okResp)
	}
	// Deleting the same snapshot a second time returns 404.
	if c := call(t, srv, "DELETE", "/v1/snapshots/"+s.ID, nil, &errResp); c != 404 {
		t.Fatalf("DELETE already-deleted snapshot: status=%d body=%v, want 404", c, errResp)
	}
}

func TestGuestOpErrorHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      *agentErr
		wantHTTP int
		wantCode string
	}{
		{"command_not_running", &agentErr{msg: "command is not running: exited"}, 409, ""},
		{"directory_not_empty", &agentErr{msg: "/work/dir is a non-empty directory; set recursive"}, 409, ""},
		{"directory_not_empty_os", &agentErr{msg: "Directory not empty"}, 409, ""},
		{"already_exists", &agentErr{msg: "/tmp/dst already exists"}, 409, ""},
		{"exists_overwrite", &agentErr{msg: "/tmp/dst exists; set overwrite"}, 409, ""},
		{"refusing_to_delete", &agentErr{msg: "refusing to delete /"}, 400, ""},
		{"not_a_directory", &agentErr{msg: "open /etc/hosts: Not a directory"}, 400, ""},
		{"is_a_directory", &agentErr{msg: "open /tmp: Is a directory"}, 400, ""},
		{"not_found", &agentErr{msg: "No such command: c99"}, 404, ""},
		{"structured_conflict_code", &agentErr{msg: "custom state mismatch", code: "command_not_running"}, 409, "command_not_running"},
		{"structured_invalid_code", &agentErr{msg: "bad path arg", code: "invalid_path"}, 400, "invalid_path"},
		{"structured_not_found_code", &agentErr{msg: "gone", code: "not_found"}, 404, "not_found"},
		{"unexpected_guest_failure", &agentErr{msg: "start: supervisor did not report in 10 s"}, 502, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, tc.err)
			if rec.Code != tc.wantHTTP {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantHTTP, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if !strings.Contains(body["error"], tc.err.msg) {
				t.Fatalf("error message not preserved: got %q want substring %q", body["error"], tc.err.msg)
			}
			if body["code"] != tc.wantCode {
				t.Fatalf("code=%q want=%q", body["code"], tc.wantCode)
			}
		})
	}

	// End-to-end via POST /v1/vms/{id}/agent returning a structured {"error", "code"} from the guest socket.
	m, _ := newTestManager(t, nil)
	v, err := m.Create(t.Context(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	if err := os.Rename(sock, sock+".old"); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
				fmt.Fprint(c, "OK 1024\n")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					_ = json.Unmarshal([]byte(line), &req)
					switch req["op"] {
					case "cmd_signal":
						fmt.Fprint(c, `{"error":"command is not running: exited","code":"command_not_running"}`+"\n")
					case "file_delete":
						fmt.Fprint(c, `{"error":"refusing to delete /","code":"refused"}`+"\n")
					default:
						fmt.Fprint(c, `{"error":"internal failure"}`+"\n")
					}
				}
			}(c)
		}
	}()

	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var out map[string]string
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": "cmd_signal", "cmd_id": "c1"}, &out); c != 409 || out["code"] != "command_not_running" {
		t.Fatalf("cmd_signal: status=%d body=%v, want 409 command_not_running", c, out)
	}
	out = nil
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": "file_delete", "path": "/"}, &out); c != 400 || out["code"] != "refused" {
		t.Fatalf("file_delete: status=%d body=%v, want 400 refused", c, out)
	}
}
