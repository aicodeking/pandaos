package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeResp struct {
	out string
	err error
}

// fakeExec answers by exact argv; unknown argv fails like a missing binary.
type fakeExec struct {
	mu    sync.Mutex
	resps map[string]fakeResp
	calls [][]string
}

func (f *fakeExec) exec(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	r, ok := f.resps[strings.Join(argv, " ")]
	if !ok {
		return []byte("not found"), errors.New("exit status 127")
	}
	return []byte(r.out), r.err
}

func (f *fakeExec) called(prefix string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func newTest(t *testing.T, cfg Config, wheel bool, resps map[string]fakeResp) (*Server, *fakeExec) {
	t.Helper()
	if cfg.ConnectivityURL == "" {
		cfg.ConnectivityURL = "http://127.0.0.1:1/"
	}
	f := &fakeExec{resps: resps}
	return &Server{cfg: cfg, exec: f.exec, root: t.TempDir(), wheel: func() bool { return wheel },
		http: &http.Client{Timeout: 2 * time.Second}}, f
}

func writeFile(t *testing.T, root, p, data string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

const dfOut = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vda1 1000 400 600 40% /\n/dev/vda2 50 10 40 20% /mnt/my disk\n"

func TestMCPSession(t *testing.T) {
	s, _ := newTest(t, Config{}, false, nil)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sys_updates","arguments":{"action":"run","confirm":true}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	if err := s.ServeMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, m)
	}
	if len(resps) != 5 {
		t.Fatalf("want 5 replies (notification gets none), got %d: %s", len(resps), out.String())
	}
	toolList := resps[1]["result"].(map[string]any)["tools"].([]any)
	want := []string{"sys_status", "sys_services", "sys_logs", "sys_top", "sys_net", "sys_updates", "sys_ports", "sys_ckpt_space"}
	if len(toolList) != len(want) {
		t.Fatalf("tools: %v", toolList)
	}
	for i, tl := range toolList {
		if n := tl.(map[string]any)["name"]; n != want[i] {
			t.Errorf("tool %d = %v, want %s", i, n, want[i])
		}
	}
	r3 := resps[2]["result"].(map[string]any)
	if r3["isError"] != true || !strings.Contains(out.String(), "mutations are disabled") {
		t.Errorf("run without allowMutations not refused: %v", r3)
	}
	if resps[3]["result"].(map[string]any)["isError"] != true {
		t.Error("unknown tool should be isError")
	}
	if resps[4]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Error("want parse error")
	}
}

func TestStatus(t *testing.T) {
	s, _ := newTest(t, Config{}, false, map[string]fakeResp{
		"df -P -k -x tmpfs -x devtmpfs -x overlay -x squashfs -x efivarfs": {out: dfOut},
	})
	writeFile(t, s.root, "/etc/pandaos/VERSION", "26.05-abc\n")
	writeFile(t, s.root, "/proc/uptime", "3600.5 100.0\n")
	writeFile(t, s.root, "/proc/loadavg", "0.10 0.20 0.30 1/100 42\n")
	writeFile(t, s.root, "/proc/meminfo", "MemTotal:  1000 kB\nMemFree: 100 kB\nMemAvailable:  300 kB\n")
	writeFile(t, s.root, "/sys/class/power_supply/BAT0/capacity", "87\n")
	writeFile(t, s.root, "/sys/class/power_supply/BAT0/status", "Discharging\n")
	v, err := s.status()
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, v)
	if m["pandaosVersion"] != "26.05-abc" || m["uptimeSeconds"].(float64) != 3600 {
		t.Errorf("status: %v", m)
	}
	if mem := m["memory"].(map[string]any); mem["usedKB"].(float64) != 700 || mem["availableKB"].(float64) != 300 {
		t.Errorf("memory: %v", mem)
	}
	disks := m["disks"].([]any)
	if len(disks) != 2 || disks[1].(map[string]any)["mount"] != "/mnt/my disk" || disks[0].(map[string]any)["availKB"].(float64) != 600 {
		t.Errorf("disks: %v", disks)
	}
	if b := m["battery"].([]any)[0].(map[string]any); b["capacityPct"] != "87" || b["status"] != "Discharging" {
		t.Errorf("battery: %v", b)
	}
}

func TestServices(t *testing.T) {
	show := "systemctl show --no-pager -p Id,LoadState,ActiveState,SubState,UnitFileState,Result " + strings.Join(pandaUnits, " ")
	s, _ := newTest(t, Config{}, false, map[string]fakeResp{
		show: {out: "Id=panda-mem.service\nLoadState=loaded\nActiveState=active\n\nId=sshd.service\nLoadState=not-found\nActiveState=inactive\n"},
		"systemctl --failed --no-legend --plain --no-pager": {out: "foo.service loaded failed failed Foo\n"},
	})
	v, err := s.services()
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, v)
	units := m["units"].([]any)
	if len(units) != 2 || units[0].(map[string]any)["ActiveState"] != "active" {
		t.Errorf("units: %v", units)
	}
	if f := m["failed"].([]any); len(f) != 1 || !strings.HasPrefix(f[0].(string), "foo.service") {
		t.Errorf("failed: %v", f)
	}
}

func TestLogsValidationAndAllowList(t *testing.T) {
	base := "journalctl --no-pager -q -o short-iso -n "
	s, f := newTest(t, Config{}, false, map[string]fakeResp{
		base + "50 --unit=panda-mem.service --since=1h ago --grep=err; rm -rf /": {out: "line1\nline2\n"},
	})
	v, err := s.logs(toolArgs{Unit: "panda-mem", Since: "1h ago", Lines: 50, Grep: "err; rm -rf /"})
	if err != nil {
		t.Fatal(err)
	}
	if l := asMap(t, v)["lines"].([]any); len(l) != 2 {
		t.Errorf("lines: %v", l)
	}

	bad := []toolArgs{
		{Unit: "sshd", Lines: 501},
		{Unit: "sshd", Lines: -1},
		{Unit: "--help"},
		{Unit: "a b"},
		{Unit: "cron.service"}, // not in the allow-list, caller not wheel
		{Since: "$(reboot)"},
		{Grep: "a\nb"},
		{Grep: strings.Repeat("x", 201)},
	}
	before := len(f.calls)
	for _, a := range bad {
		if _, err := s.logs(a); err == nil {
			t.Errorf("logs(%+v) should fail", a)
		}
	}
	if len(f.calls) != before {
		t.Errorf("invalid input reached exec: %v", f.calls[before:])
	}

	// Non-wheel, no unit: restricted to the allow-list.
	_, _ = s.logs(toolArgs{})
	last := f.calls[len(f.calls)-1]
	if n := strings.Count(strings.Join(last, " "), "--unit="); n != len(pandaUnits) {
		t.Errorf("non-wheel unrestricted query: %v", last)
	}

	// Wheel: any unit, and no unit means the whole journal.
	w, fw := newTest(t, Config{}, true, map[string]fakeResp{
		base + "100 --unit=cron.service": {out: "x\n"},
		base + "100":                     {out: "y\n"},
	})
	if _, err := w.logs(toolArgs{Unit: "cron.service"}); err != nil {
		t.Errorf("wheel cron: %v", err)
	}
	if _, err := w.logs(toolArgs{}); err != nil {
		t.Errorf("wheel all: %v", err)
	}
	if len(fw.calls) != 2 {
		t.Errorf("calls: %v", fw.calls)
	}
}

func TestLogsNoMatchIsNotError(t *testing.T) {
	s, _ := newTest(t, Config{}, true, map[string]fakeResp{
		"journalctl --no-pager -q -o short-iso -n 100 --grep=zzz": {err: errors.New("exit status 1")},
	})
	v, err := s.logs(toolArgs{Grep: "zzz"})
	if err != nil || len(asMap(t, v)["lines"].([]any)) != 0 {
		t.Errorf("v=%v err=%v", v, err)
	}
}

func TestUpdatesMutationGate(t *testing.T) {
	resps := map[string]fakeResp{
		"panda-update status":      {out: "updates: automatic\nsystem: /nix/store/x\n"},
		"sudo -n panda-update run": {out: "Update finished.\n"},
		"panda-update run":         {out: "Update finished.\n"},
	}
	cases := []struct {
		name   string
		allow  bool
		wheel  bool
		args   toolArgs
		refuse string
	}{
		{"mutations off", false, true, toolArgs{Action: "run", Confirm: true}, "mutations are disabled"},
		{"no confirm", true, true, toolArgs{Action: "run"}, "confirm:true"},
		{"not wheel", true, false, toolArgs{Action: "run", Confirm: true}, "not root or wheel"},
		{"bad action", true, true, toolArgs{Action: "rollback", Confirm: true}, "unknown action"},
	}
	for _, c := range cases {
		s, f := newTest(t, Config{AllowMutations: c.allow}, c.wheel, resps)
		_, err := s.updates(c.args)
		if err == nil || !strings.Contains(err.Error(), c.refuse) {
			t.Errorf("%s: err=%v", c.name, err)
		}
		if len(f.called("")) != 0 {
			t.Errorf("%s: exec ran: %v", c.name, f.calls)
		}
	}

	s, f := newTest(t, Config{AllowMutations: true}, true, resps)
	v, err := s.updates(toolArgs{Action: "run", Confirm: true})
	if err != nil || asMap(t, v)["ok"] != true {
		t.Fatalf("allowed run: v=%v err=%v", v, err)
	}
	if len(f.called("sudo -n panda-update run"))+len(f.called("panda-update run")) != 1 {
		t.Errorf("run calls: %v", f.calls)
	}

	s, _ = newTest(t, Config{}, false, resps)
	v, err = s.updates(toolArgs{})
	if err != nil || len(asMap(t, v)["status"].([]any)) != 2 {
		t.Errorf("status: v=%v err=%v", v, err)
	}
}

func TestTop(t *testing.T) {
	ps := "1 root 5.0 1.0 1000 systemd\n2 panda 50.0 20.0 90000 Web Content\n3 panda 1.0 0.5 10 sh\n"
	s, _ := newTest(t, Config{}, false, map[string]fakeResp{
		"ps -eo pid=,user=,pcpu=,pmem=,rss=,comm= --sort=-pcpu": {out: ps},
		"ps -eo pid=,user=,pcpu=,pmem=,rss=,comm= --sort=-rss":  {out: ps},
	})
	v, err := s.top(2)
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, v)
	cpu := m["byCPU"].([]any)
	if len(cpu) != 2 || cpu[1].(map[string]any)["comm"] != "Web Content" || cpu[1].(map[string]any)["rssKB"].(float64) != 90000 {
		t.Errorf("byCPU: %v", cpu)
	}
	for _, n := range []int{-1, 51} {
		if _, err := s.top(n); err == nil {
			t.Errorf("top(%d) should fail", n)
		}
	}
}

func TestNet(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s, _ := newTest(t, Config{ConnectivityURL: srv.URL}, false, map[string]fakeResp{
		"ip -j addr show":          {out: `[{"ifname":"lo"}]`},
		"ip -j route show default": {out: `[{"dst":"default","gateway":"10.0.2.2"}]`},
	})
	writeFile(t, s.root, "/etc/resolv.conf", "# x\nnameserver 10.0.2.3\nsearch lan example.com\n")
	m := asMap(t, s.net())
	if m["interfaces"].([]any)[0].(map[string]any)["ifname"] != "lo" {
		t.Errorf("interfaces: %v", m["interfaces"])
	}
	dns := m["dns"].(map[string]any)
	if dns["nameservers"].([]any)[0] != "10.0.2.3" || len(dns["search"].([]any)) != 2 {
		t.Errorf("dns: %v", dns)
	}
	c := m["connectivity"].(map[string]any)
	if c["ok"] != true || c["status"].(float64) != 204 || method != http.MethodHead {
		t.Errorf("connectivity: %v method=%s", c, method)
	}

	// Unreachable URL is reported, not a tool failure.
	s.cfg.ConnectivityURL = "http://127.0.0.1:1/"
	if c := asMap(t, s.net())["connectivity"].(map[string]any); c["ok"] != false || c["error"] == nil {
		t.Errorf("unreachable: %v", c)
	}
}

func TestPorts(t *testing.T) {
	s, _ := newTest(t, Config{}, false, map[string]fakeResp{
		"ss -H -t -u -l -n": {out: "udp UNCONN 0 0 0.0.0.0:68 0.0.0.0:*\ntcp LISTEN 0 128 [::]:22 [::]:*\n"},
	})
	v, err := s.ports()
	if err != nil {
		t.Fatal(err)
	}
	l := asMap(t, v)["listening"].([]any)
	if len(l) != 2 || l[1].(map[string]any)["local"] != "[::]:22" {
		t.Errorf("ports: %v", l)
	}
}

func TestCkptSpaceFallback(t *testing.T) {
	s, _ := newTest(t, Config{}, false, map[string]fakeResp{
		"btrfs filesystem usage -b /": {out: "Overall:\n    Device size: 100\n"},
		"df -P -k /.snapshots":        {out: dfOut},
	})
	b, _ := json.Marshal(s.ckptSpace())
	var res []map[string]any
	_ = json.Unmarshal(b, &res)
	if len(res) != 2 || res[0]["source"] != "btrfs" || res[1]["source"] != "df" || res[1]["btrfsError"] == nil {
		t.Errorf("ckpt: %s", b)
	}
}

func TestRealExecCapsAndTimesOut(t *testing.T) {
	s := newServer(Config{})
	out, err := s.run(5*time.Second, "head", "-c", "200000", "/dev/zero")
	if err != nil || len(out) > maxOutput+64 || !strings.HasSuffix(out, "[output truncated]\n") {
		t.Errorf("cap: len=%d err=%v", len(out), err)
	}
	start := time.Now()
	_, err = s.run(100*time.Millisecond, "sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Errorf("timeout: err=%v after %v", err, time.Since(start))
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := loadConfig(filepath.Join(dir, "missing.json"))
	if err != nil || c.AllowMutations || c.ConnectivityURL != defaultURL {
		t.Errorf("defaults: %+v %v", c, err)
	}
	p := filepath.Join(dir, "c.json")
	writeFile(t, "/", p, `{"allowMutations":true,"connectivityUrl":"https://example.com/"}`)
	if c, err := loadConfig(p); err != nil || !c.AllowMutations || c.ConnectivityURL != "https://example.com/" {
		t.Errorf("load: %+v %v", c, err)
	}
	writeFile(t, "/", p, `{"connectivityUrl":"file:///etc/shadow"}`)
	if _, err := loadConfig(p); err == nil {
		t.Error("file:// URL accepted")
	}
}
