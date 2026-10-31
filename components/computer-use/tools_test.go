package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPickWindow(t *testing.T) {
	ws := []window{{AppID: "org.xfce.mousepad", Title: "Untitled 1 - Mousepad"}, {AppID: "cu-buttons", Title: "Buttons"}}
	if w, ok := pickWindow(ws, "mousepad"); !ok || w.AppID != "org.xfce.mousepad" {
		t.Fatal(w, ok)
	}
	if w, ok := pickWindow(ws, "buttons"); !ok || w.AppID != "cu-buttons" {
		t.Fatal(w, ok)
	}
	if _, ok := pickWindow(ws, "firefox"); ok {
		t.Fatal("want no match")
	}
}

func TestLaunchAllowlist(t *testing.T) {
	cfg := Config{Apps: map[string]App{"editor": {"Editor", []string{"/nix/store/x/bin/mousepad"}}}}
	if argv, err := appArgv(cfg, "editor", "/tmp/a"); err != nil || !slices.Equal(argv, []string{"/nix/store/x/bin/mousepad", "/tmp/a"}) {
		t.Fatal(argv, err)
	}
	if _, err := appArgv(cfg, "bash", ""); err == nil || !strings.Contains(err.Error(), "editor") {
		t.Fatal(err)
	}
	for arg, ok := range map[string]bool{"": true, "/x": true, "https://e.com": true, "--no-sandbox": false, "file:///etc": false} {
		if launchArgOK(arg) != ok {
			t.Errorf("launchArgOK(%q) != %v", arg, ok)
		}
	}
}

func TestStartAppReportsExit(t *testing.T) {
	_, done, err := startApp("x", []string{"/bin/sh", "-c", "echo boom >&2; exit 3"}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if e.OK || e.Status != "exit status 3" || e.Stderr != "boom" {
			t.Fatalf("%+v", e)
		}
		if msg := exitErr(e).Error(); !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "boom") {
			t.Fatal(msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit")
	}
}

// fakePointer stands in for the display connection.
type fakePointer struct{ log []string }

func (f *fakePointer) size() (int, int, string) { return 1280, 800, "HEADLESS-1" }
func (f *fakePointer) moveTo(x, y int) error {
	f.log = append(f.log, "move "+itoa(x)+","+itoa(y))
	return nil
}
func (f *fakePointer) button(b uint32, p bool) error {
	f.log = append(f.log, map[bool]string{true: "down", false: "up"}[p])
	return nil
}
func (f *fakePointer) wheel(axis uint32, n int) error {
	if n != 0 {
		f.log = append(f.log, "wheel "+itoa(int(axis))+" "+itoa(n))
	}
	return nil
}
func (f *fakePointer) keys(ops []keyOp) error {
	for _, op := range ops {
		f.log = append(f.log, "key "+string(op.kind)+" "+op.name)
	}
	return nil
}
func (f *fakePointer) sync() error  { return nil }
func (f *fakePointer) Close() error { return nil }

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// fakeApp is the accessibility tree the fake helper answers from.
type fakeApp struct {
	els  map[string]element
	wins map[string]element
	gone map[string]bool
	reqs []map[string]any
}

func newFakeApp() *fakeApp {
	full := element{ID: "w1", Role: "frame", Name: "CU Buttons", BBox: []int{0, 0, 1280, 800}, States: []string{"active"}}
	small := element{ID: "w2", Role: "dialog", Name: "Small", BBox: []int{0, 0, 300, 200}, States: []string{"active"}}
	back := element{ID: "w3", Role: "frame", Name: "Behind", BBox: []int{0, 0, 1280, 800}}
	return &fakeApp{
		els: map[string]element{
			"e1": {ID: "e1", Role: "push button", Name: "B1", BBox: []int{100, 200, 40, 20}, Actions: []string{"click"}, Win: "w1"},
			"e2": {ID: "e2", Role: "push button", Name: "OK", BBox: []int{10, 10, 40, 20}, Win: "w2"},
			"e3": {ID: "e3", Role: "push button", Name: "Hidden", BBox: []int{10, 10, 40, 20}, Win: "w3"},
		},
		wins: map[string]element{"w1": full, "w2": small, "w3": back},
		gone: map[string]bool{},
	}
}

func (f *fakeApp) call(req map[string]any, v any) error {
	f.reqs = append(f.reqs, req)
	var res any
	switch req["cmd"] {
	case "tree", "find":
		els := []element{}
		for _, id := range []string{"e1", "e2", "e3"} {
			e := f.els[id]
			if req["cmd"] == "find" && !matches(e, req["text"].(string), req["role"].(string)) {
				continue
			}
			els = append(els, e)
		}
		res = a11yReply{Elements: els, Windows: f.wins}
	case "get":
		id := req["id"].(string)
		if f.gone[id] {
			return errors.New("element " + id + " is gone")
		}
		e := f.els[id]
		res = map[string]any{"element": e, "showing": true, "windows": map[string]element{e.Win: f.wins[e.Win]}}
	case "act":
		if f.gone[req["id"].(string)] {
			return errors.New("element is gone")
		}
		res = map[string]any{"ok": true, "via": "action:click"}
	default:
		return errors.New("bad cmd")
	}
	b, _ := json.Marshal(res)
	return json.Unmarshal(b, v)
}

func testServer(fp *fakePointer, calls *[][]string) (*Server, *fakeApp) {
	s := newServer(Config{Mode: "attach", Env: map[string]string{}})
	app := newFakeApp()
	s.display = func() (pointerDev, error) { return fp, nil }
	s.a11yCall = app.call
	s.screenSize = func() (int, int, string) { return 1280, 800, "HEADLESS-1" }
	s.listWindows = func() ([]window, error) {
		return []window{{AppID: "org.xfce.mousepad", Title: "Untitled 1 - Mousepad", States: []string{"activated"}}}, nil
	}
	s.exec = func(_ context.Context, _ []string, stdin string, argv []string) ([]byte, error) {
		*calls = append(*calls, append(slices.Clone(argv), "stdin="+stdin))
		return nil, nil
	}
	return s, app
}

func call(t *testing.T, s *Server, name, args string) (string, error) {
	t.Helper()
	var a toolArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		t.Fatal(err)
	}
	out, err := s.runTool(name, a)
	if err != nil {
		return "", err
	}
	return out[0]["text"], nil
}

func TestPointerTools(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, _ := testServer(fp, calls)
	if _, err := call(t, s, "cu_click", `{"x":1280,"y":5}`); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_click", `{"element_id":"e1"}`); err == nil || !strings.Contains(err.Error(), "unknown element_id") {
		t.Fatal("element ids need a snapshot first", err)
	}
	if _, err := call(t, s, "cu_find", `{"text":"b1"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_click", `{"element_id":"e1","double":true}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_drag", `{"from":{"element_id":"e1"},"to":{"x":120,"y":0},"button":"right"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_drag", `{"from":{"x":1},"to":{"x":120,"y":0}}`); err == nil {
		t.Fatal("want from error")
	}
	if _, err := call(t, s, "cu_scroll", `{"dy":3,"at":{"x":5,"y":5}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_scroll", `{"dy":51}`); err == nil {
		t.Fatal("want notch cap")
	}
	got := strings.Join(fp.log, "|")
	want := "move 120,210|down|up|down|up|move 120,210|down|"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "move 120,0|up|move 5,5|wheel 0 3") {
		t.Fatalf("pointer log %s", got)
	}
	if _, err := call(t, s, "cu_click", `{"button":"left4","x":1,"y":1}`); err == nil {
		t.Fatal("want bad button")
	}
}

// TestClickByIDRefusals: every reason a click by id could miss is refused.
func TestClickByIDRefusals(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, app := testServer(fp, calls)
	if _, err := call(t, s, "cu_tree", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_click", `{"element_id":"e3"}`); err == nil || !strings.Contains(err.Error(), "behind the active window") {
		t.Errorf("e3: %v", err)
	}
	// A window of unknown position: a plain click goes through AT-SPI, a
	// move, a double or right click is refused.
	out, err := call(t, s, "cu_click", `{"element_id":"e2"}`)
	if err != nil || !strings.Contains(out, "through AT-SPI") {
		t.Errorf("e2 click: %q %v", out, err)
	}
	for _, args := range []string{`{"element_id":"e2","double":true}`, `{"element_id":"e2","button":"right"}`} {
		if _, err := call(t, s, "cu_click", args); err == nil || !strings.Contains(err.Error(), "not maximized") {
			t.Errorf("%s: %v", args, err)
		}
	}
	if _, err := call(t, s, "cu_move", `{"element_id":"e2"}`); err == nil || !strings.Contains(err.Error(), "not maximized") {
		t.Errorf("move e2: %v", err)
	}
	// A popup menu item in a maximized window: also never by pixels.
	pop := app.els["e1"]
	pop.Popup = true
	app.els["e1"] = pop
	if out, err := call(t, s, "cu_click", `{"element_id":"e1"}`); err != nil || !strings.Contains(out, "popup menu") {
		t.Errorf("popup click: %q %v", out, err)
	}
	pop.Popup = false
	app.els["e1"] = pop
	e := app.els["e1"]
	e.Name = "B2"
	app.els["e1"] = e
	if _, err := call(t, s, "cu_click", `{"element_id":"e1"}`); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("renamed: %v", err)
	}
	app.gone["e1"] = true
	if _, err := call(t, s, "cu_click", `{"element_id":"e1"}`); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("gone: %v", err)
	}
	if _, err := call(t, s, "cu_act", `{"element_id":"e1","action":"click"}`); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("act gone: %v", err)
	}
	if len(fp.log) != 0 {
		t.Fatalf("a refused click moved the pointer: %v", fp.log)
	}
}

// TestIDsSurviveOtherTools: ids from cu_tree still work after marks and
// cu_wait (the cache is merged, not replaced).
func TestIDsSurviveOtherTools(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, app := testServer(fp, calls)
	if _, err := call(t, s, "cu_tree", `{}`); err != nil {
		t.Fatal(err)
	}
	only := app.els
	app.els = map[string]element{"e1": only["e1"], "e2": only["e1"], "e3": only["e1"]} // later answers hold other things
	if _, err := call(t, s, "cu_wait", `{"text":"B1","timeout":1}`); err != nil {
		t.Fatal(err)
	}
	app.els = only
	if _, ok := s.cached("e3"); !ok {
		t.Fatal("e3 from cu_tree was forgotten")
	}
}

func TestKeyboardAndA11yTools(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, app := testServer(fp, calls)
	if _, err := call(t, s, "cu_type", `{"text":"`+strings.Repeat("x", maxTypeChars+1)+`"}`); err == nil {
		t.Fatal("want text cap")
	}
	if _, err := call(t, s, "cu_type", `{"text":"-hi\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_key", `{"combo":"ctrl+s"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_launch", `{"app":"rm"}`); err == nil {
		t.Fatal("want allowlist refusal")
	}
	out, err := call(t, s, "cu_tree", `{"role":"push button"}`)
	if err != nil || !strings.Contains(out, `"B1"`) {
		t.Fatal(out, err)
	}
	if _, err := call(t, s, "cu_act", `{"element_id":"e1","action":"explode"}`); err == nil {
		t.Fatal("want bad action")
	}
	if out, err := call(t, s, "cu_act", `{"element_id":"e1","action":"click"}`); err != nil || strings.Contains(out, "seq") {
		t.Fatal(out, err)
	}
	last := app.reqs[len(app.reqs)-1]
	if last["role"] != "push button" || last["name"] != "B1" {
		t.Fatalf("act must send the expected role and name: %v", last)
	}
	if _, err := call(t, s, "cu_open", `{"url":"file:///etc/passwd"}`); err == nil {
		t.Fatal("want url refusal")
	}
	all := ""
	for _, c := range *calls {
		all += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(all, "wtype -d 2 - stdin=-hi\n") {
		t.Errorf("missing wtype call in\n%s", all)
	}
	if !strings.Contains(strings.Join(fp.log, "|"), "key M ctrl|key k s|key m ctrl") {
		t.Errorf("cu_key: %v", fp.log)
	}
}

func TestScreenshotMarksAndSettle(t *testing.T) {
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 1280, 800)))
	fp, calls := &fakePointer{}, &[][]string{}
	s, _ := testServer(fp, calls)
	frames := 0
	s.exec = func(_ context.Context, _ []string, _ string, argv []string) ([]byte, error) {
		if argv[0] != "grim" {
			return nil, nil
		}
		*calls = append(*calls, argv)
		frames++
		if frames < 3 { // the app is still repainting: frames differ
			var b bytes.Buffer
			png.Encode(&b, image.NewRGBA(image.Rect(0, 0, frames, 1)))
			return b.Bytes(), nil
		}
		return pngBuf.Bytes(), nil
	}
	frames = 2
	out, err := s.runTool("cu_screenshot", toolArgs{Marks: true})
	if err != nil || out[0]["type"] != "image" {
		t.Fatal(out, err)
	}
	var meta map[string]any
	json.Unmarshal([]byte(out[1]["text"]), &meta)
	els := meta["elements"].([]any)
	if len(els) != 2 || meta["unplaced"].(map[string]any)["Small"] != 1.0 {
		t.Fatalf("marks must skip the dialog of unknown position: %v", meta)
	}
	frames = 0
	out, err = s.runTool("cu_move", toolArgs{X: ptr(5), Y: ptr(5), Screenshot: true})
	if err != nil || len(out) != 3 || out[1]["type"] != "image" || !strings.Contains(out[2]["text"], `"settled":true`) {
		t.Fatal(out, err)
	}
	if frames != 4 {
		t.Fatalf("settle took %d frames, want 4 (two differing, then two equal)", frames)
	}
	if _, err := s.runTool("cu_screenshot", toolArgs{Region: &rect{1200, 0, 100, 10}}); err == nil {
		t.Fatal("want region outside screen")
	}
}

func TestClipboardReadGate(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, _ := testServer(fp, calls)
	if _, err := call(t, s, "cu_clipboard_get", `{}`); err == nil || !strings.Contains(err.Error(), "allowClipboardRead") {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_clipboard_set", `{"text":"x"}`); err != nil {
		t.Fatal("setting stays allowed", err)
	}
	s.cfg.AllowClipboardRead = true
	if _, err := call(t, s, "cu_clipboard_get", `{}`); err != nil {
		t.Fatal(err)
	}
	s.cfg.AllowClipboardRead, s.cfg.Mode = false, "headless"
	s.cfg.Width, s.cfg.Height = 1280, 800
	s.cfg.Env["XDG_RUNTIME_DIR"] = t.TempDir()
	os.WriteFile(s.cfg.runtimeFile("ready"), nil, 0o600)
	if _, err := call(t, s, "cu_clipboard_get", `{}`); err != nil {
		t.Fatal("headless clipboard is the agent's own", err)
	}
}

func TestRevokeAndIndicator(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, _ := testServer(fp, calls)
	s.cfg.Env["XDG_RUNTIME_DIR"] = t.TempDir()
	now := time.Now()
	if st := indicatorState(s.cfg, now); st["class"] != "idle" {
		t.Fatal(st)
	}
	if _, err := call(t, s, "cu_windows", `{}`); err != nil {
		t.Fatal(err)
	}
	if st := indicatorState(s.cfg, time.Now()); st["class"] != "active" {
		t.Fatal("a call must light the badge", st)
	}
	if st := indicatorState(s.cfg, time.Now().Add(activeFor+time.Second)); st["class"] != "idle" {
		t.Fatal(st)
	}
	if _, err := revoke(s.cfg, defaultRevoked); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_screenshot", `{}`); err == nil || !strings.Contains(err.Error(), "stopped computer use") {
		t.Fatal(err)
	}
	if st := indicatorState(s.cfg, time.Now()); st["class"] != "revoked" {
		t.Fatal(st)
	}
	if _, err := revoke(s.cfg, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_windows", `{}`); err != nil {
		t.Fatal("allowed again", err)
	}
	s.cfg.Mode = "headless" // the private desktop has no person to stop it
	if err := s.guard(); err != nil {
		t.Fatal(err)
	}
}

func TestIsCUAction(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/run/current-system/sw/bin/panda-cu\x00mcp":     true,
		"/nix/store/x/bin/.panda-cu-wrapped\x00call\x00": true,
		"panda-cu\x00indicator":                          false,
		"panda-cu\x00revoke":                             false,
		"/bin/sh\x00mcp":                                 false,
	} {
		if got := isCUAction(bytes.Split([]byte(cmd), []byte{0})); got != want {
			t.Errorf("%q: %v", cmd, got)
		}
	}
	// killCU on a fake /proc of other users' and non-matching processes kills nothing.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "1"), 0o755)
	os.WriteFile(filepath.Join(dir, "1", "cmdline"), []byte("/bin/sh\x00mcp\x00"), 0o644)
	if n := killCU(dir, os.Getuid(), 0); n != 0 {
		t.Fatal(n)
	}
}

// TestToolsDocumented: every tool has a description, all MCP annotation
// hints and a description on every parameter; every cu_* name mentioned in
// any description, hint or the instructions is a real tool.
func TestToolsDocumented(t *testing.T) {
	var tools []struct {
		Name, Description string
		InputSchema       struct {
			Type       string
			Properties map[string]struct{ Description string }
		}
		Annotations map[string]any
	}
	if err := json.Unmarshal(mcpTools, &tools); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
		if tl.Description == "" || tl.InputSchema.Type != "object" || len(tl.Annotations) != 4 {
			t.Errorf("%s: description/schema/annotations missing", tl.Name)
		}
		for p, d := range tl.InputSchema.Properties {
			if d.Description == "" {
				t.Errorf("%s.%s: no description", tl.Name, p)
			}
		}
	}
	src := toolsJSON + instructions
	for _, f := range []string{"tools.go", "a11y.go", "launch.go", "guard.go"} {
		b, _ := os.ReadFile(f)
		src += string(b)
	}
	for _, m := range regexp.MustCompile(`\bcu_[a-z_]+`).FindAllString(src, -1) {
		if !names[m] {
			t.Errorf("%s is mentioned but is not a tool", m)
		}
	}
}

func TestServeMCP(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, _ := testServer(fp, calls)
	in := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cu_click","arguments":{"x":-1,"y":0}}}
{"jsonrpc":"2.0","id":2,"method":"nope"}
`
	var out bytes.Buffer
	if err := s.ServeMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"2025-03-26"`) || !strings.Contains(lines[1], `"isError":true`) || !strings.Contains(lines[2], "-32601") {
		t.Fatal(out.String())
	}
}

func TestCallCLI(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s, _ := testServer(fp, calls)
	var out bytes.Buffer
	if err := callCLI(s, []string{"cu_find", `{"role":"push button"}`}, &out); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res["count"] != 3.0 {
		t.Fatal(out.String(), err)
	}
	if err := callCLI(s, []string{"cu_click", `{"x":-5,"y":1}`}, &out); err == nil {
		t.Fatal("want error")
	}
}

func ptr(i int) *int { return &i }

func TestLoadConfig(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "wayland-7")
	c, err := loadConfig("/nonexistent/cu.json")
	if err != nil || c.Mode != "attach" || c.Env["WAYLAND_DISPLAY"] != "wayland-7" || c.AllowClipboardRead {
		t.Fatal(c, err)
	}
}

func TestHelperProtocol(t *testing.T) {
	// A stand-in helper: echoes the seq with a canned answer, after one
	// stale line that must be skipped.
	dir := t.TempDir()
	script := filepath.Join(dir, "panda-cu-atspi")
	os.WriteFile(script, []byte("#!/bin/sh\nwhile read -r line; do\n  seq=$(echo \"$line\" | sed 's/.*\"seq\":\\([0-9]*\\).*/\\1/')\n"+
		"  echo '{\"seq\":0,\"ok\":\"stale\"}'\n  case \"$line\" in *boom*) echo \"{\\\"seq\\\":$seq,\\\"error\\\":\\\"bad\\\"}\";; *hang*) sleep 5;; *) echo \"{\\\"seq\\\":$seq,\\\"ok\\\":\\\"yes\\\"}\";; esac\ndone\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	h := &atspiHelper{env: os.Environ()}
	defer h.stop()
	var v struct{ OK string }
	if err := h.call(2*time.Second, map[string]any{"cmd": "ping"}, &v); err != nil || v.OK != "yes" {
		t.Fatal(v, err)
	}
	if err := h.call(2*time.Second, map[string]any{"cmd": "boom"}, &v); err == nil || err.Error() != "bad" {
		t.Fatal(err)
	}
	if err := h.call(300*time.Millisecond, map[string]any{"cmd": "hang"}, &v); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatal(err)
	}
	v.OK = ""
	if err := h.call(2*time.Second, map[string]any{"cmd": "ping"}, &v); err != nil || v.OK != "yes" {
		t.Fatal("a fresh helper after the timeout", v, err)
	}
}
