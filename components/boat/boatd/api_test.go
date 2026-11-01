package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
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
