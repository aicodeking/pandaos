package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestMarshalString(t *testing.T) {
	b := marshal(7, 3, uint32(5), "abc", int32(-1))
	if len(b) != 8+4+4+4+4 {
		t.Fatalf("len %d", len(b))
	}
	if ne.Uint32(b) != 7 || ne.Uint32(b[4:]) != uint32(len(b))<<16|3 {
		t.Fatalf("header %x", b[:8])
	}
	s, off := argString(b[8:], 4)
	if s != "abc" || off != 12 || int32(argU32(b[8:], off)) != -1 {
		t.Fatalf("got %q %d", s, off)
	}
	if b2 := marshal(1, 0, "abcd"); len(b2) != 8+4+8 { // 5 bytes with NUL -> padded to 8
		t.Fatalf("pad: %d", len(b2))
	}
}

// fakeCompositor answers registry, wl_output and sync, and records requests
// to the virtual pointer (object ids are the client's, as in Wayland).
type fakeCompositor struct {
	mu      sync.Mutex
	ptrReqs []uint16
	ptrID   uint32
	tlMgr   uint32
	extra   int // windows the compositor announces on the next sync
	focused uint32
}

// toplevel sends one foreign-toplevel handle with its properties.
func (f *fakeCompositor) toplevel(send func([]byte), h uint32, app, title string, states []uint32) {
	send(marshal(f.tlMgr, 0, h))
	send(marshal(h, 1, app))
	send(marshal(h, 0, title))
	send(marshal(h, 4, states))
	send(marshal(h, 5))
}

func (f *fakeCompositor) serve(t *testing.T, c net.Conn) {
	defer c.Close()
	var reg, out uint32
	send := func(b []byte) { c.Write(b) }
	for {
		obj, op, body, err := readEvent(c)
		if err != nil {
			return
		}
		switch {
		case obj == 1 && op == 1: // get_registry
			reg = argU32(body, 0)
			send(marshal(reg, 0, uint32(1), "wl_output", uint32(4)))
			send(marshal(reg, 0, uint32(2), "zwlr_virtual_pointer_manager_v1", uint32(2)))
			send(marshal(reg, 0, uint32(3), "zwlr_foreign_toplevel_manager_v1", uint32(3)))
			send(marshal(reg, 0, uint32(4), "wl_seat", uint32(7)))
		case obj == 1 && op == 0: // sync
			f.mu.Lock()
			for ; f.extra > 0; f.extra-- {
				f.toplevel(send, 0xff000010+uint32(f.extra), "cu-lab", "CU Lab", nil)
			}
			f.mu.Unlock()
			send(marshal(argU32(body, 0), 0, uint32(0)))
		case obj >= 0xff000000 && op == 4: // handle.activate(seat)
			f.focused = obj
			send(marshal(obj, 4, []uint32{0, 2})) // maximized, activated
			send(marshal(obj, 5))
		case obj == reg && op == 0: // bind(name, iface, version, id)
			iface, off := argString(body, 4)
			id := argU32(body, off+4)
			if iface == "zwlr_foreign_toplevel_manager_v1" {
				f.tlMgr = id
				f.toplevel(send, 0xff000001, "org.xfce.mousepad", "Untitled 1 - Mousepad", []uint32{0, 2})
				f.toplevel(send, 0xff000002, "org.xfce.mousepad", "Untitled 1 - Mousepad", []uint32{0}) // same title
			}
			if iface == "wl_output" {
				out = id
				send(marshal(out, 1, uint32(1), int32(1280), int32(800), int32(60000)))
				send(marshal(out, 4, "HEADLESS-1"))
			}
		case op == 2 && obj != reg && obj > out && obj < 0xff000000 && obj != f.tlMgr && f.ptrID == 0: // manager.create_virtual_pointer_with_output
			f.mu.Lock()
			f.ptrID = argU32(body, 8)
			f.mu.Unlock()
		default:
			f.mu.Lock()
			if obj == f.ptrID {
				f.ptrReqs = append(f.ptrReqs, op)
			}
			f.mu.Unlock()
		}
	}
}

func TestDesktopAgainstFakeCompositor(t *testing.T) {
	dir, err := os.MkdirTemp("", "cu") // short path: sun_path is 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "wayland-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := &fakeCompositor{}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			f.serve(t, c)
		}
	}()
	d, err := openDesktop(map[string]string{"XDG_RUNTIME_DIR": dir, "WAYLAND_DISPLAY": "wayland-0"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if w, h, n := d.size(); w != 1280 || h != 800 || n != "HEADLESS-1" {
		t.Fatalf("size %d %d %q", w, h, n)
	}
	if err := d.pointer(); err != nil {
		t.Fatal(err)
	}
	if err := d.moveTo(10, 20); err != nil {
		t.Fatal(err)
	}
	if err := d.button(btnLeft, true); err != nil {
		t.Fatal(err)
	}
	if err := d.wheel(0, 2); err != nil {
		t.Fatal(err)
	}
	if err := d.sync(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []uint16{1, 4, 2, 4, 5, 7, 4} // motion_absolute frame button frame axis_source axis_discrete frame
	if !bytes.Equal(u16(f.ptrReqs), u16(want)) {
		t.Fatalf("pointer requests %v, want %v", f.ptrReqs, want)
	}
}

func u16(v []uint16) []byte {
	var b []byte
	for _, x := range v {
		b = append(b, byte(x))
	}
	return b
}

func TestWaylandSocketMissing(t *testing.T) {
	if _, err := waylandSocket(map[string]string{"XDG_RUNTIME_DIR": "/nonexistent", "WAYLAND_DISPLAY": "wayland-0"}); err == nil {
		t.Fatal("want error")
	}
}

func fakeDisplay(t *testing.T) (*fakeCompositor, map[string]string) {
	dir, err := os.MkdirTemp("", "cu") // short path: sun_path is 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "wayland-0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeCompositor{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(t, c)
		}
	}()
	return f, map[string]string{"XDG_RUNTIME_DIR": dir, "WAYLAND_DISPLAY": "wayland-0"}
}

func TestToplevels(t *testing.T) {
	f, env := fakeDisplay(t)
	d, err := openDesktop(env)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tl, err := openToplevels(d)
	if err != nil {
		t.Fatal(err)
	}
	ws := tl.list()
	if len(ws) != 2 || ws[0].Title != ws[1].Title || ws[0].handle == ws[1].handle || !slices.Equal(ws[0].States, []string{"maximized", "activated"}) {
		t.Fatalf("%+v", ws)
	}
	// A new window with an old title is still new: it is found by handle.
	f.mu.Lock()
	f.extra = 1
	f.mu.Unlock()
	w, err := tl.waitNew(ws, time.Now().Add(2*time.Second), func() error { return nil })
	if err != nil || w == nil || w.Title != "CU Lab" {
		t.Fatal(w, err)
	}
	// Nothing new: waitNew gives up at the deadline; stop() errors end it at once.
	if w, err := tl.waitNew(tl.list(), time.Now().Add(300*time.Millisecond), func() error { return nil }); w != nil || err != nil {
		t.Fatal(w, err)
	}
	if _, err := tl.waitNew(tl.list(), time.Now().Add(time.Minute), func() error { return errors.New("died") }); err == nil {
		t.Fatal("stop error ignored")
	}
	if err := tl.activate(ws[1]); err != nil {
		t.Fatal(err)
	}
	if got := tl.list()[1].States; !slices.Contains(got, "activated") || f.focused != ws[1].handle {
		t.Fatal(got)
	}
}
