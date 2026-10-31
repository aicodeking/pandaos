package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// cu_key speaks zwp_virtual_keyboard_v1 itself for keys on a US keyboard.
// `wtype -M ctrl -k s` typed fine but never fired GTK accelerators
// (checks.panda-cu, CI runs 41190383817 and 41192408136): GDK applies a
// keymap event at once but matches accelerators by hardware keycode later,
// in its main loop, after the short-lived keyboard is gone and the seat has
// switched back to the compositor's US keyboard. Here the keymap is the
// standard US one and the codes are real evdev codes, so that switch changes
// nothing. Modifiers are pressed as keys (which makes this keyboard the
// seat's) and also sent as a modifiers request. Keysyms outside the table
// fall back to wtype.

// keyOp is one cu_key step: 'M' press a modifier, 'm' release it, 'k' tap a key.
type keyOp struct {
	kind byte
	name string // modifier (ctrl, shift, alt, altgr, logo) or keysym
}

const usKeymap = "xkb_keymap {\n" +
	"xkb_keycodes { include \"evdev+aliases(qwerty)\" };\n" +
	"xkb_types { include \"complete\" };\n" +
	"xkb_compatibility { include \"complete\" };\n" +
	"xkb_symbols { include \"pc+us+inet(evdev)+level3(ralt_switch)\" };\n};\n"

// modKeys: evdev code and modifier mask (Shift 1, Control 4, Mod1 8, Mod4 64, Mod5 128).
// altgr is Right Alt as ISO_Level3_Shift (level3(ralt_switch) in the keymap),
// which xkb puts on Mod5; plain US maps Right Alt to Alt_R (Mod1), so
// "altgr" used to arrive as a second Alt.
var modKeys = map[string]struct{ code, mask uint32 }{
	"shift": {42, 1}, "ctrl": {29, 4}, "alt": {56, 8}, "logo": {125, 64}, "altgr": {100, 128},
}

// usKey is a key on the US layout: evdev code, plus whether it needs shift.
type usKey struct {
	code  uint32
	shift bool
}

var usKeys = func() map[string]usKey {
	m := map[string]usKey{
		"Escape": {1, false}, "minus": {12, false}, "equal": {13, false}, "BackSpace": {14, false},
		"Tab": {15, false}, "bracketleft": {26, false}, "bracketright": {27, false}, "Return": {28, false},
		"semicolon": {39, false}, "apostrophe": {40, false}, "grave": {41, false}, "backslash": {43, false},
		"comma": {51, false}, "period": {52, false}, "slash": {53, false}, "space": {57, false},
		"Print": {99, false}, "Home": {102, false}, "Up": {103, false}, "Prior": {104, false},
		"Left": {105, false}, "Right": {106, false}, "End": {107, false}, "Down": {108, false},
		"Next": {109, false}, "Insert": {110, false}, "Delete": {111, false}, "Menu": {127, false},
		"KP_Enter": {96, false}, "F11": {87, false}, "F12": {88, false},
		"underscore": {12, true}, "plus": {13, true}, "braceleft": {26, true}, "braceright": {27, true},
		"colon": {39, true}, "quotedbl": {40, true}, "asciitilde": {41, true}, "bar": {43, true},
		"less": {51, true}, "greater": {52, true}, "question": {53, true},
	}
	for i, r := range "1234567890" {
		m[string(r)] = usKey{uint32(2 + i), false}
	}
	for i, n := range []string{"exclam", "at", "numbersign", "dollar", "percent", "asciicircum", "ampersand", "asterisk", "parenleft", "parenright"} {
		m[n] = usKey{uint32(2 + i), true}
	}
	for row, letters := range map[uint32]string{16: "qwertyuiop", 30: "asdfghjkl", 44: "zxcvbnm"} {
		for i, r := range letters {
			// "ctrl+S" means ctrl+s, as in cubench's QMP keys; shift is explicit.
			m[string(r)] = usKey{row + uint32(i), false}
			m[strings.ToUpper(string(r))] = usKey{row + uint32(i), false}
		}
	}
	for i := 1; i <= 10; i++ {
		m[fmt.Sprintf("F%d", i)] = usKey{uint32(58 + i), false}
	}
	return m
}()

// keyOps turns keyArgs' wtype-style list (-M mod, -k sym, -m mod) into
// steps; ok is false when a keysym is not on the US table (use wtype).
func keyOps(args []string) (ops []keyOp, ok bool, err error) {
	if len(args)%2 != 0 {
		return nil, false, fmt.Errorf("odd key step list %q", args)
	}
	ok = true
	for i := 0; i < len(args); i += 2 {
		f, name := args[i], args[i+1]
		switch f {
		case "-M", "-m":
			if _, known := modKeys[name]; !known {
				return nil, false, fmt.Errorf("modifier %q", name)
			}
		case "-k":
			_, onUS := usKeys[name]
			ok = ok && onUS
		default:
			return nil, false, fmt.Errorf("key step %q", f)
		}
		ops = append(ops, keyOp{f[1], name})
	}
	return ops, ok, nil
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
	km := usKeymap + "\x00"
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
	setMods := func(m uint32) error { return d.send(kb, 2, m, uint32(0), uint32(0), uint32(0)) } // modifiers
	for _, op := range ops {
		switch op.kind {
		case 'M', 'm':
			down := op.kind == 'M'
			if err := key(modKeys[op.name].code, down); err != nil {
				return err
			}
			if down {
				mods |= modKeys[op.name].mask
			} else {
				mods &^= modKeys[op.name].mask
			}
			if err := setMods(mods); err != nil {
				return err
			}
		case 'k':
			k := usKeys[op.name]
			if k.shift && mods&1 == 0 {
				if err := errors.Join(key(modKeys["shift"].code, true), setMods(mods|1)); err != nil {
					return err
				}
			}
			if err := errors.Join(key(k.code, true), key(k.code, false)); err != nil {
				return err
			}
			if k.shift && mods&1 == 0 {
				if err := errors.Join(key(modKeys["shift"].code, false), setMods(mods)); err != nil {
					return err
				}
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
