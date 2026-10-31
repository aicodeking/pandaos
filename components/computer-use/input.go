package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxTypeChars = 4000 // cu_type
	maxCombos    = 16   // cu_key: space-separated combos per call
	maxSetText   = 100_000
	maxClipboard = 1 << 20
)

var modifiers = map[string]string{
	"ctrl": "ctrl", "control": "ctrl", "shift": "shift", "alt": "alt", "altgr": "altgr",
	"super": "logo", "meta": "logo", "win": "logo", "logo": "logo", "cmd": "logo",
}

// keyAliases maps friendly names (lower case) to XKB keysym names.
var keyAliases = map[string]string{
	"enter": "Return", "return": "Return", "esc": "Escape", "escape": "Escape", "tab": "Tab",
	"backspace": "BackSpace", "delete": "Delete", "del": "Delete", "insert": "Insert",
	"home": "Home", "end": "End", "pageup": "Prior", "pgup": "Prior", "pagedown": "Next", "pgdn": "Next",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right", "space": "space", "menu": "Menu",
	"plus": "plus", "minus": "minus", "-": "minus", "=": "equal", "/": "slash", "\\": "backslash",
	".": "period", ",": "comma", ";": "semicolon", "'": "apostrophe", "`": "grave",
	"[": "bracketleft", "]": "bracketright", "printscreen": "Print",
}

var keysymRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// keyArgs turns "ctrl+s" or "ctrl+a BackSpace" into wtype arguments:
// -M mod ... -k key -m mod ... per combo, in order.
func keyArgs(spec string) ([]string, error) {
	combos := strings.Fields(spec)
	if len(combos) == 0 {
		return nil, errors.New("combo is empty")
	}
	if len(combos) > maxCombos || len(spec) > 256 {
		return nil, fmt.Errorf("at most %d combos and 256 bytes per cu_key", maxCombos)
	}
	var args []string
	for _, c := range combos {
		parts := strings.Split(c, "+")
		if strings.HasSuffix(c, "++") { // "ctrl++" = ctrl+plus
			parts = append(parts[:len(parts)-2], "plus")
		}
		key, mods := parts[len(parts)-1], parts[:len(parts)-1]
		var press, release []string
		for _, m := range mods {
			wm, ok := modifiers[strings.ToLower(m)]
			if !ok {
				return nil, fmt.Errorf("unknown modifier %q in %q (ctrl, shift, alt, altgr, super)", m, c)
			}
			press, release = append(press, "-M", wm), append([]string{"-m", wm}, release...)
		}
		if wm, ok := modifiers[strings.ToLower(key)]; ok && len(mods) == 0 { // a lone modifier tap
			args = append(args, "-M", wm, "-m", wm)
			continue
		}
		sym, err := keysym(key)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", c, err)
		}
		args = append(args, press...)
		args = append(args, "-k", sym)
		args = append(args, release...)
	}
	return args, nil
}

func keysym(k string) (string, error) {
	if s, ok := keyAliases[strings.ToLower(k)]; ok {
		return s, nil
	}
	if len(k) >= 2 && (k[0] == 'f' || k[0] == 'F') && keysymRe.MatchString(k) && strings.Trim(k[1:], "0123456789") == "" {
		return "F" + k[1:], nil
	}
	if keysymRe.MatchString(k) {
		return k, nil // a letter, digit or XKB keysym name such as Return or KP_Enter
	}
	return "", fmt.Errorf("unknown key %q: use an XKB keysym name (Return, Escape, F5, a) or an alias", k)
}

func checkText(s string, limit int, what string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	if n := utf8.RuneCountInString(s); n > limit {
		return fmt.Errorf("%s has %d characters; the limit is %d per call", what, n, limit)
	}
	return nil
}
