package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// cu_key speaks zwp_virtual_keyboard_v1 itself instead of running wtype.
// Under labwc, `wtype -M ctrl -k s` reached GTK apps as a plain "s" (the
// modifiers request alone never made it; checks.panda-cu, CI run
// 41190383817). Here the keymap has real modifier keys, every modifier is a
// key press first (which makes this keyboard the seat's keyboard and updates
// the compositor's xkb state) and then also an explicit modifiers request.

// keyOp is one cu_key step: 'M' press a modifier, 'm' release it, 'k' tap a key.
type keyOp struct {
	kind byte
	name string // modifier (ctrl, shift, alt, altgr, logo) or keysym
}

type modKey struct {
	sym, real string
	mask      uint32
}

var modKeys = map[string]modKey{
	"shift": {"Shift_L", "Shift", 1},
	"ctrl":  {"Control_L", "Control", 4},
	"alt":   {"Alt_L", "Mod1", 8},
	"logo":  {"Super_L", "Mod4", 64},
	"altgr": {"ISO_Level3_Shift", "Mod5", 128},
}

var modOrder = []string{"shift", "ctrl", "alt", "logo", "altgr"}

// keyOps turns keyArgs' wtype-style list (-M mod, -k sym, -m mod) into steps.
func keyOps(args []string) ([]keyOp, error) {
	var ops []keyOp
	for i := 0; i+1 < len(args); i += 2 {
		f := args[i]
		if f != "-M" && f != "-m" && f != "-k" {
			return nil, fmt.Errorf("key step %q", f)
		}
		if f != "-k" {
			if _, ok := modKeys[args[i+1]]; !ok {
				return nil, fmt.Errorf("modifier %q", args[i+1])
			}
		}
		ops = append(ops, keyOp{f[1], args[i+1]})
	}
	if len(ops)*2 != len(args) {
		return nil, fmt.Errorf("odd key step list %q", args)
	}
	return ops, nil
}

// keymapFor builds an XKB keymap with the five modifier keys (evdev 1..5)
// and each distinct keysym in ops (evdev 6..). It returns the evdev code
// of every modifier and keysym.
func keymapFor(ops []keyOp) (string, map[string]uint32) {
	codes := map[string]uint32{}
	var kc, sy strings.Builder
	for i, m := range modOrder {
		codes[m] = uint32(1 + i)
		fmt.Fprintf(&kc, "<M%d> = %d; ", i, 9+i)
		fmt.Fprintf(&sy, "key <M%d> {[ %s ]}; modifier_map %s { <M%d> };\n", i, modKeys[m].sym, modKeys[m].real, i)
	}
	n := 0
	for _, op := range ops {
		if op.kind != 'k' || codes["k:"+op.name] != 0 {
			continue
		}
		codes["k:"+op.name] = uint32(1 + len(modOrder) + n)
		fmt.Fprintf(&kc, "<K%d> = %d; ", n, 9+len(modOrder)+n)
		fmt.Fprintf(&sy, "key <K%d> {[ %s ]};\n", n, op.name)
		n++
	}
	km := "xkb_keymap {\nxkb_keycodes \"(unnamed)\" { minimum = 8; maximum = 255; " + kc.String() + "};\n" +
		"xkb_types \"(unnamed)\" { include \"complete\" };\n" +
		"xkb_compatibility \"(unnamed)\" { include \"complete\" };\n" +
		"xkb_symbols \"(unnamed)\" {\n" + sy.String() + "};\n};\n"
	return km, codes
}

// keys runs ops on a fresh virtual keyboard of this connection.
func (d *desktop) keys(ops []keyOp) error {
	seat, _, err := d.bind("wl_seat", 1)
	if err != nil {
		return err
	}
	mgr, _, err := d.bind("zwp_virtual_keyboard_manager_v1", 1)
	if err != nil {
		return err
	}
	km, codes := keymapFor(ops)
	km += "\x00"
	// /tmp, not $XDG_RUNTIME_DIR: agent sandboxes often have a read-only /run.
	f, err := os.CreateTemp("", "panda-cu-keymap")
	if err != nil {
		return err
	}
	defer f.Close()
	os.Remove(f.Name())
	if _, err := f.WriteString(km); err != nil {
		return err
	}
	kb := d.newID()
	if err := d.send(mgr, 0, seat, kb); err != nil {
		return err
	}
	if err := d.sendFD(int(f.Fd()), kb, 0, uint32(1), uint32(len(km))); err != nil { // keymap(xkb_v1, fd, size)
		return err
	}
	if err := d.roundtrip(nil); err != nil {
		return err
	}
	key := func(code uint32, down bool) error {
		state := uint32(0)
		if down {
			state = 1
		}
		return d.send(kb, 1, now(), code, state) // key(time, key, state)
	}
	var mods uint32
	step := func() error { // let the client see each step before the next
		err := d.roundtrip(nil)
		time.Sleep(15 * time.Millisecond)
		return err
	}
	for _, op := range ops {
		switch op.kind {
		case 'M', 'm':
			down := op.kind == 'M'
			if err := key(codes[op.name], down); err != nil {
				return err
			}
			if down {
				mods |= modKeys[op.name].mask
			} else {
				mods &^= modKeys[op.name].mask
			}
			if err := d.send(kb, 2, mods, uint32(0), uint32(0), uint32(0)); err != nil { // modifiers
				return err
			}
		case 'k':
			c := codes["k:"+op.name]
			if err := key(c, true); err != nil {
				return err
			}
			if err := key(c, false); err != nil {
				return err
			}
		}
		if err := step(); err != nil {
			return err
		}
	}
	time.Sleep(30 * time.Millisecond)     // before the seat switches back to another keyboard
	if err := d.send(kb, 3); err != nil { // destroy
		return err
	}
	return d.roundtrip(nil)
}
