package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Units reported by sys_services and readable by any caller via sys_logs.
var pandaUnits = []string{
	"pandacode-host.service",
	"panda-mem.service",
	"panda-browser.service",
	"panda-ckpt-auto-panda.timer", // services.panda-ckpt.users defaults to [ "panda" ]
	"panda-ckpt-auto-panda.service",
	"panda-agents-sync.service",
	"systemd-oomd.service", // kills in agents.slice
	"nixos-upgrade.service",
	"nixos-upgrade.timer",
	"sshd.service",
	"NetworkManager.service",
}

var (
	unitRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:\\-]{0,127}$`)
	sinceRe = regexp.MustCompile(`^[0-9A-Za-z :.+-]{1,40}$`)
)

type toolArgs struct {
	Unit    string `json:"unit,omitempty"`
	Since   string `json:"since,omitempty"`
	Lines   int    `json:"lines,omitempty"`
	Grep    string `json:"grep,omitempty"`
	N       int    `json:"n,omitempty"`
	Action  string `json:"action,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

type Server struct {
	cfg   Config
	exec  Execer
	root  string      // prefix for /proc, /etc, /sys reads ("" in production)
	wheel func() bool // is the caller privileged
	http  *http.Client
}

func newServer(cfg Config) *Server {
	return &Server{cfg: cfg, exec: realExec, wheel: callerIsWheel,
		http: &http.Client{Timeout: 5 * time.Second}}
}

func (s *Server) runTool(name string, a toolArgs) (any, error) {
	switch name {
	case "sys_status":
		return s.status()
	case "sys_services":
		return s.services()
	case "sys_logs":
		return s.logs(a)
	case "sys_top":
		return s.top(a.N)
	case "sys_net":
		return s.net(), nil
	case "sys_updates":
		return s.updates(a)
	case "sys_ports":
		return s.ports()
	case "sys_ckpt_space":
		return s.ckptSpace(), nil
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

func (s *Server) readFile(p string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root, p))
	return strings.TrimSpace(string(b)), err
}

// ---- sys_status

type disk struct {
	Mount   string `json:"mount"`
	FS      string `json:"fs"`
	SizeKB  int64  `json:"sizeKB"`
	UsedKB  int64  `json:"usedKB"`
	AvailKB int64  `json:"availKB"`
}

// parseDF reads `df -P -k` output.
func parseDF(out string) []disk {
	var ds []disk
	for _, l := range strings.Split(out, "\n")[1:] {
		f := strings.Fields(l)
		if len(f) < 6 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseInt(f[2], 10, 64)
		avail, _ := strconv.ParseInt(f[3], 10, 64)
		ds = append(ds, disk{Mount: strings.Join(f[5:], " "), FS: f[0], SizeKB: size, UsedKB: used, AvailKB: avail})
	}
	return ds
}

func (s *Server) status() (any, error) {
	st := map[string]any{"cpus": runtime.NumCPU()}
	st["hostname"], _ = os.Hostname()
	if v, err := s.readFile("/etc/pandaos/VERSION"); err == nil {
		st["pandaosVersion"] = v
	}
	if up, err := s.readFile("/proc/uptime"); err == nil {
		if f := strings.Fields(up); len(f) > 0 {
			sec, _ := strconv.ParseFloat(f[0], 64)
			st["uptimeSeconds"] = int64(sec)
		}
	}
	if la, err := s.readFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(la); len(f) >= 3 {
			st["load"] = f[:3]
		}
	}
	if mi, err := s.readFile("/proc/meminfo"); err == nil {
		kb := map[string]int64{}
		for _, l := range strings.Split(mi, "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok {
				n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
				kb[k] = n
			}
		}
		st["memory"] = map[string]int64{"totalKB": kb["MemTotal"], "availableKB": kb["MemAvailable"],
			"usedKB": kb["MemTotal"] - kb["MemAvailable"]}
	}
	out, err := s.run(execTimeout, "df", "-P", "-k", "-x", "tmpfs", "-x", "devtmpfs", "-x", "overlay", "-x", "squashfs", "-x", "efivarfs")
	if ds := parseDF(out); len(ds) > 0 {
		st["disks"] = ds // df exits 1 on one unreadable mount; keep the rest
	} else if err != nil {
		st["disksError"] = fmt.Sprintf("df: %v: %s", err, out)
	}
	var bats []map[string]string
	paths, _ := filepath.Glob(filepath.Join(s.root, "/sys/class/power_supply/BAT*"))
	for _, p := range paths {
		rel := strings.TrimPrefix(p, s.root)
		cap, _ := s.readFile(rel + "/capacity")
		stat, _ := s.readFile(rel + "/status")
		bats = append(bats, map[string]string{"name": filepath.Base(p), "capacityPct": cap, "status": stat})
	}
	if bats != nil {
		st["battery"] = bats
	}
	return st, nil
}

// ---- sys_services

func (s *Server) services() (any, error) {
	argv := append([]string{"systemctl", "show", "--no-pager", "-p", "Id,LoadState,ActiveState,SubState,UnitFileState,Result"}, pandaUnits...)
	out, err := s.run(execTimeout, argv...)
	if err != nil {
		return nil, fmt.Errorf("systemctl show: %v: %s", err, out)
	}
	var units []map[string]string
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		u := map[string]string{}
		for _, l := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				u[k] = v
			}
		}
		if len(u) > 0 {
			units = append(units, u)
		}
	}
	failed, ferr := s.run(execTimeout, "systemctl", "--failed", "--no-legend", "--plain", "--no-pager")
	res := map[string]any{"units": units, "failed": nonEmptyLines(failed)}
	if ferr != nil {
		res["failedError"] = ferr.Error()
	}
	return res, nil
}

func nonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---- sys_logs

func normUnit(u string) string {
	if !strings.Contains(u, ".") {
		return u + ".service"
	}
	return u
}

func (s *Server) logs(a toolArgs) (any, error) {
	lines := a.Lines
	if lines == 0 {
		lines = 100
	}
	if lines < 1 || lines > 500 {
		return nil, errors.New("lines must be 1..500")
	}
	argv := []string{"journalctl", "--no-pager", "-q", "-o", "short-iso", "-n", strconv.Itoa(lines)}
	wheel := s.wheel()
	switch {
	case a.Unit != "":
		if !unitRe.MatchString(a.Unit) {
			return nil, errors.New("invalid unit name")
		}
		u := normUnit(a.Unit)
		if !wheel && !slices.Contains(pandaUnits, u) {
			return nil, fmt.Errorf("unit %s is not readable by non-wheel callers; allowed: %s", u, strings.Join(pandaUnits, ", "))
		}
		argv = append(argv, "--unit="+u)
	case !wheel:
		for _, u := range pandaUnits {
			argv = append(argv, "--unit="+u)
		}
	}
	if a.Since != "" {
		if !sinceRe.MatchString(a.Since) {
			return nil, errors.New("invalid since (e.g. '1h ago', '2026-10-29 10:00', 'today')")
		}
		argv = append(argv, "--since="+a.Since)
	}
	if a.Grep != "" {
		if len(a.Grep) > 200 || strings.IndexFunc(a.Grep, unicode.IsControl) >= 0 {
			return nil, errors.New("grep must be <=200 chars with no control characters")
		}
		argv = append(argv, "--grep="+a.Grep)
	}
	out, err := s.run(execTimeout, argv...)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	// journalctl exits 1 when --grep matches nothing; that is not an error.
	if err != nil && strings.TrimSpace(out) != "" && !strings.Contains(out, "-- No entries --") {
		return nil, fmt.Errorf("journalctl: %v: %s", err, out)
	}
	return map[string]any{"lines": nonEmptyLines(out)}, nil
}

// ---- sys_top

type proc struct {
	PID  int     `json:"pid"`
	User string  `json:"user"`
	CPU  float64 `json:"cpuPct"`
	Mem  float64 `json:"memPct"`
	RSS  int64   `json:"rssKB"`
	Comm string  `json:"comm"`
}

func (s *Server) psSorted(key string, n int) ([]proc, error) {
	out, err := s.run(execTimeout, "ps", "-eo", "pid=,user=,pcpu=,pmem=,rss=,comm=", "--sort="+key)
	if err != nil {
		return nil, fmt.Errorf("ps: %v: %s", err, out)
	}
	ps := []proc{}
	for _, l := range nonEmptyLines(out) {
		f := strings.Fields(l)
		if len(f) < 6 {
			continue
		}
		var p proc
		p.PID, _ = strconv.Atoi(f[0])
		p.User = f[1]
		p.CPU, _ = strconv.ParseFloat(f[2], 64)
		p.Mem, _ = strconv.ParseFloat(f[3], 64)
		p.RSS, _ = strconv.ParseInt(f[4], 10, 64)
		p.Comm = strings.Join(f[5:], " ")
		if ps = append(ps, p); len(ps) == n {
			break
		}
	}
	return ps, nil
}

func (s *Server) top(n int) (any, error) {
	if n == 0 {
		n = 10
	}
	if n < 1 || n > 50 {
		return nil, errors.New("n must be 1..50")
	}
	cpu, err := s.psSorted("-pcpu", n)
	if err != nil {
		return nil, err
	}
	mem, err := s.psSorted("-rss", n)
	if err != nil {
		return nil, err
	}
	return map[string]any{"byCPU": cpu, "byMemory": mem}, nil
}

// ---- sys_net

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// jsonOrText keeps `ip -j` output as JSON when it parses.
func jsonOrText(out string, err error) any {
	if err != nil {
		return map[string]string{"error": err.Error(), "output": out}
	}
	return rawJSON(out)
}

func (s *Server) net() any {
	res := map[string]any{
		"interfaces":   jsonOrText(s.run(execTimeout, "ip", "-j", "addr", "show")),
		"defaultRoute": jsonOrText(s.run(execTimeout, "ip", "-j", "route", "show", "default")),
	}
	dns := map[string][]string{"nameservers": {}, "search": {}}
	if rc, err := s.readFile("/etc/resolv.conf"); err == nil {
		for _, l := range strings.Split(rc, "\n") {
			f := strings.Fields(l)
			switch {
			case len(f) >= 2 && f[0] == "nameserver":
				dns["nameservers"] = append(dns["nameservers"], f[1])
			case len(f) >= 2 && (f[0] == "search" || f[0] == "domain"):
				dns["search"] = append(dns["search"], f[1:]...)
			}
		}
	}
	res["dns"] = dns
	conn := map[string]any{"url": s.cfg.ConnectivityURL, "ok": false}
	start := time.Now()
	if req, err := http.NewRequest(http.MethodHead, s.cfg.ConnectivityURL, nil); err != nil {
		conn["error"] = err.Error()
	} else if resp, err := s.http.Do(req); err != nil {
		conn["error"] = err.Error()
	} else {
		resp.Body.Close()
		conn["status"] = resp.StatusCode
		conn["ok"] = resp.StatusCode < 500
	}
	conn["ms"] = time.Since(start).Milliseconds()
	res["connectivity"] = conn
	return res
}

// ---- sys_updates

func (s *Server) updates(a toolArgs) (any, error) {
	switch a.Action {
	case "", "status":
		out, err := s.run(execTimeout, "panda-update", "status")
		if err != nil {
			return nil, fmt.Errorf("panda-update status: %v: %s", err, out)
		}
		return map[string]any{"status": nonEmptyLines(out)}, nil
	case "run":
		if !s.cfg.AllowMutations {
			return nil, errors.New("refused: mutations are disabled (services.panda-sys.allowMutations = false)")
		}
		if !a.Confirm {
			return nil, errors.New("refused: sys_updates run is mutating; pass confirm:true")
		}
		if !s.wheel() {
			return nil, errors.New("refused: caller is not root or wheel")
		}
		argv := []string{"sudo", "-n", "panda-update", "run"}
		if os.Geteuid() == 0 {
			argv = argv[2:]
		}
		out, err := s.run(runTimeout, argv...)
		res := map[string]any{"ok": err == nil, "output": nonEmptyLines(out)}
		if err != nil {
			res["error"] = err.Error()
		}
		return res, nil
	}
	return nil, fmt.Errorf("unknown action %q (status|run)", a.Action)
}

// ---- sys_ports

func (s *Server) ports() (any, error) {
	out, err := s.run(execTimeout, "ss", "-H", "-t", "-u", "-l", "-n")
	if err != nil {
		return nil, fmt.Errorf("ss: %v: %s", err, out)
	}
	ports := []map[string]string{}
	for _, l := range nonEmptyLines(out) {
		f := strings.Fields(l)
		if len(f) >= 5 {
			ports = append(ports, map[string]string{"proto": f[0], "state": f[1], "local": f[4]})
		}
	}
	return map[string]any{"listening": ports}, nil
}

// ---- sys_ckpt_space

func (s *Server) ckptSpace() any {
	var res []map[string]any
	for _, p := range []string{"/", "/.snapshots"} {
		e := map[string]any{"path": p}
		if out, err := s.run(execTimeout, "btrfs", "filesystem", "usage", "-b", p); err == nil {
			e["source"], e["usage"] = "btrfs", nonEmptyLines(out)
		} else if out, derr := s.run(execTimeout, "df", "-P", "-k", p); derr == nil {
			e["source"], e["df"] = "df", parseDF(out)
			e["btrfsError"] = err.Error()
		} else {
			e["error"] = fmt.Sprintf("btrfs: %v; df: %v: %s", err, derr, strings.TrimSpace(out))
		}
		res = append(res, e)
	}
	return res
}
