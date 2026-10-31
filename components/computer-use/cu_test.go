package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"
)

func TestKeyArgs(t *testing.T) {
	for in, want := range map[string]string{
		"ctrl+s":           "-M ctrl -k s -m ctrl",
		"ctrl+shift+t":     "-M ctrl -M shift -k t -m shift -m ctrl",
		"Return":           "-k Return",
		"enter":            "-k Return",
		"ctrl+a BackSpace": "-M ctrl -k a -m ctrl -k BackSpace",
		"alt+F4":           "-M alt -k F4 -m alt",
		"super":            "-M logo -m logo",
		"ctrl++":           "-M ctrl -k plus -m ctrl",
		"ctrl+/":           "-M ctrl -k slash -m ctrl",
		"f5":               "-k F5",
		"ctrl+End":         "-M ctrl -k End -m ctrl",
	} {
		got, err := keyArgs(in)
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("keyArgs(%q) = %q, %v; want %q", in, strings.Join(got, " "), err, want)
		}
	}
	for _, bad := range []string{"", "hyper+s", "ctrl+$$", strings.Repeat("a ", 17), "ctrl+-k"} {
		if _, err := keyArgs(bad); err == nil {
			t.Errorf("keyArgs(%q): want error", bad)
		}
	}
}

func TestCheckText(t *testing.T) {
	if checkText(strings.Repeat("ü", maxTypeChars), maxTypeChars, "t") != nil {
		t.Error("limit counts characters, not bytes")
	}
	if checkText(strings.Repeat("a", maxTypeChars+1), maxTypeChars, "t") == nil {
		t.Error("want cap")
	}
	if checkText("\xff", 10, "t") == nil {
		t.Error("want UTF-8 error")
	}
}

func TestSessionEnv(t *testing.T) {
	env := sessionEnv(Config{}, 1001)
	if env["XDG_RUNTIME_DIR"] != "/run/user/1001" || env["WAYLAND_DISPLAY"] != "wayland-0" ||
		env["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/run/user/1001/bus" {
		t.Fatal(env)
	}
	env = sessionEnv(Config{Env: map[string]string{"XDG_RUNTIME_DIR": "/run/panda-cu"}}, 5)
	if env["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/run/panda-cu/bus" {
		t.Fatal(env)
	}
}

func TestMarksAndPNGSize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 60))
	var b bytes.Buffer
	png.Encode(&b, src)
	out, err := drawMarks(b.Bytes(), []mark{{1, image.Rect(10, 10, 50, 40)}, {12, image.Rect(90, 50, 140, 90)}})
	if err != nil {
		t.Fatal(err)
	}
	if w, h, ok := pngSize(out); !ok || w != 100 || h != 60 {
		t.Fatal(w, h, ok)
	}
	img, _ := png.Decode(bytes.NewReader(out))
	if c := color.RGBAModel.Convert(img.At(30, 39)).(color.RGBA); c != markColors[0] { // bottom outline
		t.Fatalf("outline colour %v", c)
	}
	if c := color.RGBAModel.Convert(img.At(30, 25)).(color.RGBA); c.A != 0 { // interior untouched
		t.Fatalf("interior %v", c)
	}
}

func TestParseToplevelsAndPick(t *testing.T) {
	ws := parseToplevels("org.xfce.mousepad: Untitled 1 - Mousepad\ncu-buttons: Buttons\n\n")
	if len(ws) != 2 || ws[1] != (window{"cu-buttons", "Buttons"}) {
		t.Fatal(ws)
	}
	if w, ok := pickWindow(ws, "mousepad"); !ok || w.AppID != "org.xfce.mousepad" {
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
	for _, o := range ops {
		f.log = append(f.log, fmt.Sprintf("key %c %s", o.kind, o.name))
	}
	return nil
}
func (f *fakePointer) sync() error  { return nil }
func (f *fakePointer) Close() error { return nil }

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func testServer(fp *fakePointer, calls *[][]string) *Server {
	s := newServer(Config{Mode: "attach", Env: map[string]string{}})
	s.display = func() (pointerDev, error) { return fp, nil }
	s.exec = func(_ context.Context, _ []string, stdin string, argv []string) ([]byte, error) {
		*calls = append(*calls, append(slices.Clone(argv), "stdin="+stdin))
		if argv[0] == "panda-cu-atspi" && argv[1] == "tree" {
			return []byte(`{"elements":[{"id":"0","d":0,"role":"application","name":"cu-buttons"},
			  {"id":"0.0.1","d":2,"role":"push button","name":"B1","bbox":[100,200,40,20],"actions":["click"]}]}`), nil
		}
		if argv[0] == "panda-cu-atspi" && argv[1] == "act" {
			return []byte(`{"error":"element 0.0.1 changed"}`), errors.New("exit 1")
		}
		return nil, nil
	}
	return s
}

func call(t *testing.T, s *Server, name, args string) (string, error) {
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
	s := testServer(fp, calls)
	if _, err := call(t, s, "cu_click", `{"x":1280,"y":5}`); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_click", `{"element_id":"0.0.1"}`); err == nil {
		t.Fatal("element ids need a snapshot")
	}
	if _, err := call(t, s, "cu_find", `{"text":"b1"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_click", `{"element_id":"0.0.1","double":true}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_drag", `{"from":{"x":0,"y":0},"to":{"x":120,"y":0},"button":"right"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_scroll", `{"dy":3,"at":{"x":5,"y":5}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_scroll", `{"dy":51}`); err == nil {
		t.Fatal("want notch cap")
	}
	got := strings.Join(fp.log, "|")
	want := "move 120,210|down|up|down|up|move 0,0|down|move 10,0"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "move 120,0|up|move 5,5|wheel 0 3") {
		t.Fatalf("pointer log %s", got)
	}
	if _, err := call(t, s, "cu_click", `{"button":"left4","x":1,"y":1}`); err == nil {
		t.Fatal("want bad button")
	}
}

func TestKeyboardAndA11yTools(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s := testServer(fp, calls)
	if _, err := call(t, s, "cu_type", `{"text":"`+strings.Repeat("x", maxTypeChars+1)+`"}`); err == nil {
		t.Fatal("want text cap")
	}
	if _, err := call(t, s, "cu_type", `{"text":"-hi\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_key", `{"combo":"ctrl+s"}`); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fp.log, "|"); got != "key M ctrl|key k s|key m ctrl" {
		t.Fatalf("key log %s", got)
	}
	if _, err := call(t, s, "cu_launch", `{"app":"rm"}`); err == nil {
		t.Fatal("want allowlist refusal")
	}
	out, err := call(t, s, "cu_tree", `{"role":"push button"}`)
	if err != nil || !strings.Contains(out, `"B1"`) {
		t.Fatal(out, err)
	}
	if _, err := call(t, s, "cu_act", `{"element_id":"0.0.1","action":"explode"}`); err == nil {
		t.Fatal("want bad action")
	}
	if _, err := call(t, s, "cu_act", `{"element_id":"0.0.1","action":"click"}`); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_open", `{"url":"file:///etc/passwd"}`); err == nil {
		t.Fatal("want url refusal")
	}
	joined := []string{}
	for _, c := range *calls {
		joined = append(joined, strings.Join(c, " "))
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{"wtype -d 2 - stdin=-hi\n", "act 0.0.1 click --role push button --name B1"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing call %q in\n%s", want, all)
		}
	}
}

// TestToolsDocumented: every tool has a description, all MCP annotation hints
// and a description on every parameter.
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
	for _, tl := range tools {
		if tl.Description == "" || tl.InputSchema.Type != "object" || len(tl.Annotations) != 4 {
			t.Errorf("%s: description/schema/annotations missing", tl.Name)
		}
		for p, d := range tl.InputSchema.Properties {
			if d.Description == "" {
				t.Errorf("%s.%s: no description", tl.Name, p)
			}
		}
	}
}

func TestServeMCP(t *testing.T) {
	fp, calls := &fakePointer{}, &[][]string{}
	s := testServer(fp, calls)
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
	s := testServer(fp, calls)
	var out bytes.Buffer
	if err := callCLI(s, []string{"cu_find", `{"role":"push button"}`}, &out); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res["count"] != 1.0 {
		t.Fatal(out.String(), err)
	}
	if err := callCLI(s, []string{"cu_click", `{"x":-5,"y":1}`}, &out); err == nil {
		t.Fatal("want error")
	}
}

func TestScreenshotWindowsWaitClipboard(t *testing.T) {
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 1280, 800)))
	fp, calls := &fakePointer{}, &[][]string{}
	s := testServer(fp, calls)
	inner := s.exec
	s.exec = func(ctx context.Context, env []string, stdin string, argv []string) ([]byte, error) {
		switch argv[0] {
		case "grim":
			*calls = append(*calls, argv)
			return pngBuf.Bytes(), nil
		case "wlrctl":
			*calls = append(*calls, argv)
			return []byte("org.xfce.mousepad: Untitled 1 - Mousepad\n"), nil
		case "wl-paste":
			return []byte("clip"), nil
		}
		return inner(ctx, env, stdin, argv)
	}
	out, err := s.runTool("cu_screenshot", toolArgs{Marks: true})
	if err != nil || out[0]["type"] != "image" || !strings.Contains(out[1]["text"], `"mark":1`) {
		t.Fatal(out, err)
	}
	if _, err := s.runTool("cu_screenshot", toolArgs{Region: &rect{1200, 0, 100, 10}}); err == nil {
		t.Fatal("want region outside screen")
	}
	out, err = s.runTool("cu_click", toolArgs{X: ptr(5), Y: ptr(5), Screenshot: true})
	if err != nil || len(out) != 2 || out[1]["type"] != "image" {
		t.Fatal(out, err)
	}
	if txt, err := call(t, s, "cu_wait", `{"window":"mousepad","timeout":1}`); err != nil || !strings.Contains(txt, `"ok":true`) {
		t.Fatal(txt, err)
	}
	if _, err := call(t, s, "cu_wait", `{"text":"nothing like this","timeout":0.3}`); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatal(err)
	}
	if _, err := call(t, s, "cu_wait", `{"text":"B1","gone":true,"timeout":0.3}`); err == nil {
		t.Fatal("B1 is present; gone must time out")
	}
	if _, err := call(t, s, "cu_focus", `{"window":"Mousepad"}`); err != nil {
		t.Fatal(err)
	}
	if txt, err := call(t, s, "cu_clipboard_get", `{}`); err != nil || txt != `{"text":"clip"}` {
		t.Fatal(txt, err)
	}
	if _, err := call(t, s, "cu_clipboard_set", `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, c := range *calls {
		all += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"grim -t png -l 1 -o HEADLESS-1 -", "grim -t png -l 1 -s 0.500 -o HEADLESS-1 -", "wlrctl toplevel focus app_id:org.xfce.mousepad title:Untitled 1 - Mousepad", "grim -t png -l 1 -g 1200,0 100x10 -"} {
		if want == "grim -t png -l 1 -g 1200,0 100x10 -" {
			if strings.Contains(all, want) {
				t.Error("out-of-screen region must not reach grim")
			}
			continue
		}
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
}

func ptr(i int) *int { return &i }

func TestLoadConfig(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "wayland-7")
	c, err := loadConfig("/nonexistent/cu.json")
	if err != nil || c.Mode != "attach" || c.Env["WAYLAND_DISPLAY"] != "wayland-7" {
		t.Fatal(c, err)
	}
}

func TestKeyOps(t *testing.T) {
	args, err := keyArgs("ctrl+shift+t ctrl+S BackSpace plus F12")
	if err != nil {
		t.Fatal(err)
	}
	ops, onUS, err := keyOps(args)
	if err != nil || !onUS || len(ops) != 11 {
		t.Fatal(ops, onUS, err)
	}
	for sym, want := range map[string]usKey{"s": {31, false}, "S": {31, false}, "t": {20, false}, "0": {11, false},
		"plus": {13, true}, "question": {53, true}, "F10": {68, false}, "Return": {28, false}, "z": {44, false}} {
		if usKeys[sym] != want {
			t.Errorf("usKeys[%q] = %v, want %v", sym, usKeys[sym], want)
		}
	}
	if _, onUS, _ := keyOps([]string{"-k", "XF86AudioMute"}); onUS {
		t.Error("XF86AudioMute is not on the US table")
	}
	if _, _, err := keyOps([]string{"-M", "hyper"}); err == nil {
		t.Error("want bad modifier")
	}
	if _, _, err := keyOps([]string{"-k"}); err == nil {
		t.Error("want odd list")
	}
	if !strings.Contains(usKeymap, `"pc+us+inet(evdev)"`) {
		t.Error(usKeymap)
	}
}

func TestRoleAlias(t *testing.T) {
	e := element{Role: "push button", Name: "Save"}
	if !matches(e, "save", "button") || !matches(e, "", "push button") || matches(e, "", "toggle button") {
		t.Error("role alias")
	}
}

func TestRealExecForkedChild(t *testing.T) {
	// The child keeps stdout open after its parent exits 0, like wl-copy.
	out, err := realExec(context.Background(), nil, "", []string{"/bin/sh", "-c", "echo hi; (sleep 5) &"})
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := realExec(context.Background(), nil, "", []string{"/bin/sh", "-c", "exit 3"}); err == nil {
		t.Fatal("want exit error")
	}
}
