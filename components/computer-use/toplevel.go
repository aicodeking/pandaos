package main

// zwlr_foreign_toplevel_manager_v1, spoken directly: the window list with
// states (activated, maximized, fullscreen) and parents, focusing by handle,
// and new-window detection by handle rather than by title (wlrctl only
// prints "app_id: title", so two windows with one title looked the same).

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

type window struct {
	AppID  string   `json:"app_id"`
	Title  string   `json:"title"`
	States []string `json:"states,omitempty"`
	Dialog bool     `json:"dialog,omitempty"` // has a parent window
	handle uint32
	done   bool
}

var toplevelStates = map[uint32]string{0: "maximized", 1: "minimized", 2: "activated", 3: "fullscreen"}

// toplevels tracks the compositor's windows on one connection.
type toplevels struct {
	d    *desktop
	mgr  uint32
	seat uint32
	wins map[uint32]*window
	// order of appearance, so "new since" is a set difference of handles
	order []uint32
}

func openToplevels(d *desktop) (*toplevels, error) {
	mgr, _, err := d.bind("zwlr_foreign_toplevel_manager_v1", 3)
	if err != nil {
		return nil, err
	}
	t := &toplevels{d: d, mgr: mgr, wins: map[uint32]*window{}}
	// Two round trips: the manager announces handles, then each handle its
	// title, app_id, state and done.
	for range 2 {
		if err := t.pump(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *toplevels) pump() error {
	return t.d.roundtrip(func(obj uint32, op uint16, b []byte) {
		if obj == t.mgr && op == 0 { // toplevel(new_id handle)
			h := argU32(b, 0)
			t.wins[h] = &window{handle: h}
			t.order = append(t.order, h)
			return
		}
		w, ok := t.wins[obj]
		if !ok {
			return
		}
		switch op {
		case 0:
			w.Title, _ = argString(b, 0)
		case 1:
			w.AppID, _ = argString(b, 0)
		case 4: // state(array of uint32)
			n := int(argU32(b, 0))
			w.States = nil
			for off := 4; off+4 <= 4+n && off+4 <= len(b); off += 4 {
				if s, ok := toplevelStates[argU32(b, off)]; ok {
					w.States = append(w.States, s)
				}
			}
		case 5:
			w.done = true
		case 6: // closed
			delete(t.wins, obj)
			t.order = slices.DeleteFunc(t.order, func(h uint32) bool { return h == obj })
		case 7: // parent(object or null)
			w.Dialog = argU32(b, 0) != 0
		}
	})
}

// list returns the windows that have sent done, in order of appearance.
func (t *toplevels) list() []window {
	out := []window{}
	for _, h := range t.order {
		if w := t.wins[h]; w != nil && w.done {
			out = append(out, *w)
		}
	}
	return out
}

func (t *toplevels) activate(w window) error {
	if t.seat == 0 {
		seat, _, err := t.d.bind("wl_seat", 1)
		if err != nil {
			return err
		}
		t.seat = seat
	}
	if err := t.d.send(w.handle, 4, t.seat); err != nil { // activate(seat)
		return err
	}
	return t.pump()
}

// waitNew pumps until a window whose handle is not in before has sent done,
// stop() says to give up, or the deadline passes.
func (t *toplevels) waitNew(before []window, deadline time.Time, stop func() error) (*window, error) {
	old := map[uint32]bool{}
	for _, w := range before {
		old[w.handle] = true
	}
	for {
		if err := t.pump(); err != nil {
			return nil, err
		}
		for _, w := range t.list() {
			if !old[w.handle] {
				return &w, nil
			}
		}
		if err := stop(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Server) openWindows() (*toplevels, error) {
	if err := s.waitReady(); err != nil {
		return nil, err
	}
	d, err := openDesktop(s.cfg.Env)
	if err != nil {
		return nil, err
	}
	t, err := openToplevels(d)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("window list: %w", err)
	}
	return t, nil
}

func (s *Server) windows() ([]window, error) {
	if s.listWindows != nil { // tests
		return s.listWindows()
	}
	t, err := s.openWindows()
	if err != nil {
		return nil, err
	}
	defer t.d.Close()
	return t.list(), nil
}

func (s *Server) focus(q string) (any, error) {
	t, err := s.openWindows()
	if err != nil {
		return nil, err
	}
	defer t.d.Close()
	ws := t.list()
	w, ok := pickWindow(ws, q)
	if !ok {
		return nil, fmt.Errorf("no window matches %q; windows: %v", q, ws)
	}
	if err := t.activate(w); err != nil {
		return nil, err
	}
	for range 20 { // until the compositor reports it activated
		for _, x := range t.list() {
			if x.handle == w.handle && slices.Contains(x.States, "activated") {
				return map[string]any{"focused": x}, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
		if err := t.pump(); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("the compositor did not activate the window")
}
