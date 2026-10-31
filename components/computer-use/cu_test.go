package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
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
	if !strings.Contains(usKeymap, `"pc+us+inet(evdev)+level3(ralt_switch)"`) || modKeys["altgr"] != (struct{ code, mask uint32 }{100, 128}) {
		t.Error("altgr must be Right Alt as ISO_Level3_Shift on Mod5")
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
