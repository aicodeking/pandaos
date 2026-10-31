package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// atspi runs the Python helper and decodes its JSON (errors come as {"error"}).
func (s *Server) atspi(timeout time.Duration, v any, args ...string) error {
	out, err := s.run(timeout, "", append([]string{"panda-cu-atspi"}, args...)...)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(out, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// snapshot walks the tree and replaces the element_id cache.
func (s *Server) snapshot(a toolArgs, marks bool) ([]element, error) {
	if err := s.waitReady(); err != nil {
		return nil, err
	}
	depth := a.MaxDepth
	if depth <= 0 || depth > 50 {
		depth = 30
	}
	argv := []string{"tree", "--max-depth", strconv.Itoa(depth)}
	if a.App != "" {
		argv = append(argv, "--app", a.App)
	}
	if marks {
		argv = append(argv, "--interactive", "--active") // what is clickable in the focused window
	}
	var res struct {
		Elements  []element `json:"elements"`
		Truncated bool      `json:"truncated"`
	}
	if err := s.atspi(30*time.Second, &res, argv...); err != nil {
		return nil, fmt.Errorf("accessibility tree: %w", err)
	}
	snap := make(map[string]element, len(res.Elements))
	for i, e := range res.Elements {
		if s.cfg.Scale > 0 && s.cfg.Scale != 1 {
			for k := range e.BBox {
				e.BBox[k] = int(float64(e.BBox[k]) * s.cfg.Scale)
			}
			res.Elements[i] = e
		}
		snap[e.ID] = e
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return res.Elements, nil
}

// roleAliases: newer at-spi2-core calls GtkButton "button", older ones
// "push button"; panda-cu-atspi reports "push button", and either name matches.
var roleAliases = map[string]string{"button": "push button"}

func matches(e element, text, role string) bool {
	if r, ok := roleAliases[strings.ToLower(role)]; ok {
		role = r
	}
	if role != "" && !strings.EqualFold(e.Role, role) {
		return false
	}
	t := strings.ToLower(text)
	return t == "" || strings.Contains(strings.ToLower(e.Name), t) || strings.Contains(strings.ToLower(e.Text), t)
}

func limitOf(a toolArgs, def int) int {
	if a.Limit <= 0 || a.Limit > 2000 {
		return def
	}
	return a.Limit
}

func (s *Server) tree(a toolArgs) (any, error) {
	els, err := s.snapshot(a, false)
	if err != nil {
		return nil, err
	}
	out := []element{}
	for _, e := range els {
		if a.Role == "" || strings.EqualFold(e.Role, a.Role) || e.Depth <= 1 {
			out = append(out, e)
		}
	}
	n := limitOf(a, 400)
	res := map[string]any{"elements": out[:min(n, len(out))], "count": len(out)}
	if len(out) > n {
		res["truncated"] = fmt.Sprintf("showing %d of %d; narrow with app, role or max_depth", n, len(out))
	}
	return res, nil
}

func (s *Server) find(a toolArgs) (any, error) {
	text := ""
	if a.Text != nil {
		text = *a.Text
	}
	if text == "" && a.Role == "" {
		return nil, errors.New("give text and/or role")
	}
	els, err := s.snapshot(a, false)
	if err != nil {
		return nil, err
	}
	out := []element{}
	for _, e := range els {
		if e.Depth > 0 && matches(e, text, a.Role) {
			out = append(out, e)
		}
	}
	return map[string]any{"elements": out[:min(limitOf(a, 50), len(out))], "count": len(out)}, nil
}

var actions = []string{"click", "focus", "set_text", "press"}

func (s *Server) act(a toolArgs) (any, error) {
	if !slices.Contains(actions, a.Action) {
		return nil, fmt.Errorf("action %q (click|focus|set_text|press)", a.Action)
	}
	s.mu.Lock()
	e, ok := s.snap[a.ElementID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown element_id %q: take a cu_tree or cu_find snapshot first", a.ElementID)
	}
	argv := []string{"act", e.ID, a.Action, "--role", e.Role, "--name", e.Name}
	if a.Action == "set_text" {
		if a.Text == nil {
			return nil, errors.New("set_text needs text")
		}
		if err := checkText(*a.Text, maxSetText, "text"); err != nil {
			return nil, err
		}
		argv = append(argv, "--text", *a.Text)
	}
	var res map[string]any
	err := s.atspi(15*time.Second, &res, argv...)
	return res, err
}

func (s *Server) wait(a toolArgs) (any, error) {
	text := ""
	if a.Text != nil {
		text = *a.Text
	}
	if text == "" && a.Role == "" && a.Window == "" {
		return nil, errors.New("give text, role and/or window")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10
	}
	if timeout > 120 {
		return nil, errors.New("timeout is at most 120 s")
	}
	t0 := time.Now()
	deadline := t0.Add(time.Duration(timeout * float64(time.Second)))
	for {
		found, what, err := s.present(a, text)
		if err == nil && found != a.Gone {
			return map[string]any{"ok": true, "match": what, "waited_ms": time.Since(t0).Milliseconds()}, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, fmt.Errorf("timed out after %.0f s: %w", timeout, err)
			}
			return nil, fmt.Errorf("timed out after %.0f s", timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (s *Server) present(a toolArgs, text string) (bool, any, error) {
	if a.Window != "" {
		ws, err := s.windows()
		if err != nil {
			return false, nil, err
		}
		w, ok := pickWindow(ws, a.Window)
		if !ok || (text == "" && a.Role == "") {
			return ok, w, nil
		}
	}
	els, err := s.snapshot(a, false)
	if err != nil {
		return false, nil, err
	}
	for _, e := range els {
		if e.Depth > 0 && matches(e, text, a.Role) {
			return true, e, nil
		}
	}
	return false, nil, nil
}

// ---- windows (wlr-foreign-toplevel via wlrctl)

type window struct {
	AppID string `json:"app_id"`
	Title string `json:"title"`
}

func parseToplevels(out string) []window {
	ws := []window{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			app, title, _ := strings.Cut(l, ": ")
			ws = append(ws, window{app, title})
		}
	}
	return ws
}

func (s *Server) windows() ([]window, error) {
	if err := s.waitReady(); err != nil {
		return nil, err
	}
	out, err := s.run(execTimeout, "", "wlrctl", "toplevel", "list")
	return parseToplevels(string(out)), err
}

func pickWindow(ws []window, q string) (window, bool) {
	q = strings.ToLower(q)
	for _, w := range ws {
		if strings.EqualFold(w.Title, q) || strings.EqualFold(w.AppID, q) {
			return w, true
		}
	}
	for _, w := range ws {
		if strings.Contains(strings.ToLower(w.Title), q) || strings.Contains(strings.ToLower(w.AppID), q) {
			return w, true
		}
	}
	return window{}, false
}

func (s *Server) focus(q string) (any, error) {
	ws, err := s.windows()
	if err != nil {
		return nil, err
	}
	w, ok := pickWindow(ws, q)
	if !ok {
		return nil, fmt.Errorf("no window matches %q; windows: %v", q, ws)
	}
	_, err = s.run(execTimeout, "", "wlrctl", "toplevel", "focus", "app_id:"+w.AppID, "title:"+w.Title)
	return map[string]any{"focused": w}, err
}

// ---- launching

// launchReq is the one-line JSON request `panda-cu session` accepts.
type launchReq struct {
	App string `json:"app"`
	Arg string `json:"arg,omitempty"`
}

func appArgv(cfg Config, app, arg string) ([]string, error) {
	a, ok := cfg.Apps[app]
	if !ok || len(a.Argv) == 0 {
		ids := make([]string, 0, len(cfg.Apps))
		for id := range cfg.Apps {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return nil, fmt.Errorf("app %q is not allowlisted (have: %s)", app, strings.Join(ids, ", "))
	}
	argv := slices.Clone(a.Argv)
	if arg != "" {
		argv = append(argv, arg)
	}
	return argv, nil
}

// launch starts an app inside the session: through `panda-cu session`'s
// socket in headless mode, so apps live in panda-cu.service rather than in
// the agent's sandbox, else (attach) as a detached child.
func (s *Server) launch(app, arg string) error {
	argv, err := appArgv(s.cfg, app, arg)
	if err != nil {
		return err
	}
	if s.cfg.Mode == "headless" {
		if err := s.waitReady(); err != nil {
			return err
		}
		c, err := net.DialTimeout("unix", s.cfg.runtimeFile("launch.sock"), 5*time.Second)
		if err != nil {
			return fmt.Errorf("session launcher: %w", err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if err := json.NewEncoder(c).Encode(launchReq{app, arg}); err != nil {
			return err
		}
		var res struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(c).Decode(&res); err != nil {
			return fmt.Errorf("session launcher: %w", err)
		}
		if res.Error != "" {
			return errors.New(res.Error)
		}
		return nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = s.cfg.environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // reap
	return nil
}

func (s *Server) launchTool(app, arg string) (any, error) {
	before, _ := s.windows()
	if err := s.launch(app, arg); err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		now, err := s.windows()
		if err != nil {
			continue
		}
		for _, w := range now {
			if !slices.Contains(before, w) {
				return map[string]any{"launched": app, "window": w}, nil
			}
		}
	}
	return map[string]any{"launched": app, "window": nil, "note": "no new window within 20 s; check cu_windows"}, nil
}

func (s *Server) open(a toolArgs) (any, error) {
	switch {
	case a.URL != "" && a.Path != "":
		return nil, errors.New("give path or url, not both")
	case a.URL != "":
		u, err := url.Parse(a.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("url must be http(s)://host/...")
		}
		return s.launchTool(s.cfg.Open["url"], u.String())
	case a.Path != "":
		p := a.Path
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(os.Getenv("HOME"), p[2:])
		}
		if !filepath.IsAbs(p) {
			return nil, errors.New("path must be absolute (or ~/...)")
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		kind := "file"
		if fi.IsDir() {
			kind = "dir"
		}
		return s.launchTool(s.cfg.Open[kind], filepath.Clean(p))
	}
	return nil, errors.New("give path or url")
}
