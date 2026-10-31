// A minimal Wayland client: just enough wire protocol for
// zwlr_virtual_pointer_v1 (absolute motion, buttons, wheel), wl_output (size)
// and an idle zwp_virtual_keyboard_v1. In-process instead of wlrctl because
// wlrctl only has relative motion and atomic clicks: no absolute
// coordinates, no press/hold for drags.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ne = binary.NativeEndian // Wayland uses host byte order

const (
	btnLeft   = 0x110 // linux/input-event-codes.h
	btnRight  = 0x111
	btnMiddle = 0x112
)

type wlConn struct {
	c      *net.UnixConn
	nextID uint32
}

// waylandSocket resolves the display socket and refuses one owned by another
// user: panda-cu acts only on the caller's own session.
func waylandSocket(env map[string]string) (string, error) {
	p := env["WAYLAND_DISPLAY"]
	if !filepath.IsAbs(p) {
		p = filepath.Join(env["XDG_RUNTIME_DIR"], p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("no display session at %s (is panda-cu.service running?): %w", p, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("refused: %s belongs to uid %d, not to the caller", p, st.Uid)
	}
	return p, nil
}

func dialWayland(env map[string]string) (*wlConn, error) {
	p, err := waylandSocket(env)
	if err != nil {
		return nil, err
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		return nil, err
	}
	return &wlConn{c: c, nextID: 2}, nil // 1 is wl_display
}

func (w *wlConn) Close() error { return w.c.Close() }

func (w *wlConn) newID() uint32 { w.nextID++; return w.nextID - 1 }

// marshal encodes one request. Args: uint32, int32 or string.
func marshal(obj uint32, op uint16, args ...any) []byte {
	b := make([]byte, 8, 64)
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			b = ne.AppendUint32(b, v)
		case int32:
			b = ne.AppendUint32(b, uint32(v))
		case string:
			n := len(v) + 1 // with NUL
			b = ne.AppendUint32(b, uint32(n))
			b = append(b, v...)
			b = append(b, make([]byte, (n+3)&^3-len(v))...)
		default:
			panic(fmt.Sprintf("wayland arg %T", a))
		}
	}
	ne.PutUint32(b, obj)
	ne.PutUint32(b[4:], uint32(len(b))<<16|uint32(op))
	return b
}

func (w *wlConn) send(obj uint32, op uint16, args ...any) error {
	_, err := w.c.Write(marshal(obj, op, args...))
	return err
}

// sendFD sends a request whose fd argument travels as SCM_RIGHTS.
func (w *wlConn) sendFD(fd int, obj uint32, op uint16, args ...any) error {
	_, _, err := w.c.WriteMsgUnix(marshal(obj, op, args...), syscall.UnixRights(fd), nil)
	return err
}

func readEvent(r io.Reader) (obj uint32, op uint16, body []byte, err error) {
	var hdr [8]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return
	}
	obj, x := ne.Uint32(hdr[:]), ne.Uint32(hdr[4:])
	size, op := x>>16, uint16(x)
	if size < 8 {
		return 0, 0, nil, errors.New("wayland: bad message size")
	}
	body = make([]byte, size-8)
	_, err = io.ReadFull(r, body)
	return
}

func argU32(b []byte, off int) uint32 {
	if off+4 > len(b) {
		return 0
	}
	return ne.Uint32(b[off:])
}

func argString(b []byte, off int) (string, int) {
	n := int(argU32(b, off))
	off += 4
	if n == 0 || off+n > len(b) {
		return "", off
	}
	return string(b[off : off+n-1]), off + (n+3)&^3
}

// roundtrip sends wl_display.sync and feeds events to h until it is done.
func (w *wlConn) roundtrip(h func(obj uint32, op uint16, body []byte)) error {
	cb := w.newID()
	if err := w.send(1, 0, cb); err != nil {
		return err
	}
	_ = w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		obj, op, body, err := readEvent(w.c)
		if err != nil {
			return fmt.Errorf("wayland: %w", err)
		}
		switch {
		case obj == 1 && op == 0: // wl_display.error(object, code, message)
			msg, _ := argString(body, 8)
			return fmt.Errorf("wayland protocol error %d: %s", argU32(body, 4), msg)
		case obj == cb && op == 0:
			return nil
		case h != nil:
			h(obj, op, body)
		}
	}
}

type global struct{ name, version uint32 }

type output struct {
	Name          string
	Width, Height int
}

// desktop is a display connection with a virtual pointer on the first output.
type desktop struct {
	*wlConn
	globals map[string]global
	reg     uint32
	out     output
	outID   uint32
	ptr     uint32
}

func (d *desktop) bind(iface string, maxVer uint32) (uint32, uint32, error) {
	g, ok := d.globals[iface]
	if !ok {
		return 0, 0, fmt.Errorf("compositor lacks %s", iface)
	}
	id, v := d.newID(), min(g.version, maxVer)
	return id, v, d.send(d.reg, 0, g.name, iface, v, id)
}

func openDesktop(env map[string]string) (*desktop, error) {
	w, err := dialWayland(env)
	if err != nil {
		return nil, err
	}
	d := &desktop{wlConn: w, globals: map[string]global{}}
	if err := d.setup(); err != nil {
		w.Close()
		return nil, err
	}
	return d, nil
}

func (d *desktop) setup() error {
	d.reg = d.newID()
	if err := d.send(1, 1, d.reg); err != nil { // wl_display.get_registry
		return err
	}
	err := d.roundtrip(func(obj uint32, op uint16, b []byte) {
		if obj == d.reg && op == 0 { // global(name, interface, version)
			iface, off := argString(b, 4)
			if _, dup := d.globals[iface]; !dup {
				d.globals[iface] = global{argU32(b, 0), argU32(b, off)}
			}
		}
	})
	if err != nil {
		return err
	}
	if d.outID, _, err = d.bind("wl_output", 4); err != nil {
		return err
	}
	err = d.roundtrip(func(obj uint32, op uint16, b []byte) {
		switch {
		case obj == d.outID && op == 1 && argU32(b, 0)&1 != 0: // mode(flags, w, h, refresh), current
			d.out.Width, d.out.Height = int(int32(argU32(b, 4))), int(int32(argU32(b, 8)))
		case obj == d.outID && op == 4: // name
			d.out.Name, _ = argString(b, 0)
		}
	})
	if err != nil {
		return err
	}
	if d.out.Width <= 0 || d.out.Height <= 0 {
		return errors.New("wayland: output has no current mode")
	}
	return nil
}

// pointer creates the virtual pointer, mapped to the first output when the
// manager is v2 (so absolute motion is in that output's pixels).
func (d *desktop) pointer() error {
	mgr, v, err := d.bind("zwlr_virtual_pointer_manager_v1", 2)
	if err != nil {
		return err
	}
	d.ptr = d.newID()
	if v >= 2 {
		err = d.send(mgr, 2, uint32(0), d.outID, d.ptr) // with_output(seat=null, output, id)
	} else {
		err = d.send(mgr, 0, uint32(0), d.ptr)
	}
	if err != nil {
		return err
	}
	return d.roundtrip(nil)
}

func now() uint32 { return uint32(time.Now().UnixMilli()) }

func (d *desktop) frame() error { return d.send(d.ptr, 4) }

func (d *desktop) moveTo(x, y int) error {
	if err := d.send(d.ptr, 1, now(), uint32(x), uint32(y), uint32(d.out.Width), uint32(d.out.Height)); err != nil {
		return err
	}
	return d.frame()
}

func (d *desktop) button(btn uint32, pressed bool) error {
	state := uint32(0)
	if pressed {
		state = 1
	}
	if err := d.send(d.ptr, 2, now(), btn, state); err != nil {
		return err
	}
	return d.frame()
}

// wheel scrolls n notches on axis 0 (vertical, +down) or 1 (horizontal, +right).
func (d *desktop) wheel(axis uint32, n int) error {
	if n == 0 {
		return nil
	}
	if err := d.send(d.ptr, 5, uint32(0)); err != nil { // axis_source(wheel)
		return err
	}
	// axis_discrete(time, axis, value fixed 24.8, discrete): 15 px per notch
	if err := d.send(d.ptr, 7, now(), axis, int32(15*n*256), int32(n)); err != nil {
		return err
	}
	return d.frame()
}

// idleKeyboard creates a virtual keyboard that never types. Held by
// `panda-cu session` so the seat always advertises a keyboard: otherwise each
// short-lived wtype keyboard would make clients rebind wl_keyboard and the
// first keys could be lost.
func (d *desktop) idleKeyboard(dir string) error {
	seat, _, err := d.bind("wl_seat", 1)
	if err != nil {
		return err
	}
	mgr, _, err := d.bind("zwp_virtual_keyboard_manager_v1", 1)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "keymap")
	if err != nil {
		return err
	}
	defer f.Close()
	os.Remove(f.Name())
	// Same minimal keymap wtype uploads; the compositor resolves the includes.
	km := "xkb_keymap {\nxkb_keycodes \"(unnamed)\" { minimum = 8; maximum = 255; <K1> = 9; };\n" +
		"xkb_types \"(unnamed)\" { include \"complete\" };\nxkb_compatibility \"(unnamed)\" { include \"complete\" };\n" +
		"xkb_symbols \"(unnamed)\" { key <K1> {[space]}; };\n};\n\x00"
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
	return d.roundtrip(nil)
}
