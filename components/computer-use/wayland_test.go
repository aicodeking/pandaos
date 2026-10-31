package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
		case obj == 1 && op == 0: // sync
			send(marshal(argU32(body, 0), 0, uint32(0)))
		case obj == reg && op == 0: // bind(name, iface, version, id)
			iface, off := argString(body, 4)
			id := argU32(body, off+4)
			if iface == "wl_output" {
				out = id
				send(marshal(out, 1, uint32(1), int32(1280), int32(800), int32(60000)))
				send(marshal(out, 4, "HEADLESS-1"))
			}
		case op == 2 && obj != reg && obj > out && f.ptrID == 0: // manager.create_virtual_pointer_with_output
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
