package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// element is one AT-SPI node as panda-cu-atspi reports it. IDs are stable
// for the helper's lifetime (one per accessible object), so results from
// cu_tree, cu_find, cu_wait and marks can be mixed freely.
type element struct {
	ID      string   `json:"id"`
	Depth   int      `json:"d,omitempty"`
	Role    string   `json:"role"`
	Name    string   `json:"name"`
	BBox    []int    `json:"bbox,omitempty"`
	States  []string `json:"states,omitempty"`
	Actions []string `json:"actions,omitempty"`
	Text    string   `json:"text,omitempty"`
	Chars   int      `json:"chars,omitempty"` // characters of text (text holds at most 500)
	Win     string   `json:"win,omitempty"`   // id of its toplevel window
	// Popup: inside a popup surface (a GTK 3 menu), whose screen position
	// is unknown on Wayland; such boxes are never used for pixel clicks.
	Popup bool `json:"popup,omitempty"`
	// boxSince: when this box was first reported (see clickPoint).
	boxSince time.Time
}

// a11yReply is the helper's answer to tree and find.
type a11yReply struct {
	Elements  []element          `json:"elements"`
	Windows   map[string]element `json:"windows"`
	Truncated bool               `json:"truncated"`
	Via       []string           `json:"via,omitempty"`
}

const (
	maxKnown = 50_000
	// A box must hold still this long before a click by element_id trusts it.
	// A cold Chromium first reports a page's input about (26,20) px from where
	// it is drawn, for up to about 1.2 s (CI runs 41296253198, 41305927008).
	// ponytail: a fixed window, not a proof; cu_type's landed report catches a
	// slower correction.
	boxSettle = 1500 * time.Millisecond
	// A box still changing after this long is refused.
	boxSettleLimit = 6 * time.Second
	// How long cu_type waits to see its text in the focused field.
	typeLandLimit = 5 * time.Second
)

// a11y sends one request to the helper (tests swap in a fake).
func (s *Server) a11y(timeout time.Duration, req map[string]any, v any) error {
	if err := s.waitReady(); err != nil {
		return err
	}
	if s.a11yCall != nil {
		return s.a11yCall(req, v)
	}
	s.helper.follow(s.cfg.sessionID())
	return s.helper.call(timeout, req, v)
}

// remember merges elements into the id cache: newer answers overwrite
// older ones, nothing is dropped until the cache is full.
func (s *Server) remember(els []element, wins map[string]element) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.known) > maxKnown {
		s.known = map[string]element{}
	}
	for _, e := range els {
		s.keep(e)
	}
	for _, w := range wins {
		s.keep(w)
	}
}

// keep stores e under s.mu, and when its box differs from the last one
// restarts the time the box has held still.
func (s *Server) keep(e element) {
	e.boxSince = time.Now()
	if old, ok := s.known[e.ID]; ok && slices.Equal(old.BBox, e.BBox) {
		e.boxSince = old.boxSince
	}
	s.known[e.ID] = e
}

func (s *Server) scaleBoxes(els []element) {
	if s.cfg.Scale <= 0 || s.cfg.Scale == 1 {
		return
	}
	for i := range els {
		for k := range els[i].BBox {
			els[i].BBox[k] = int(float64(els[i].BBox[k]) * s.cfg.Scale)
		}
	}
}

func (s *Server) query(cmd string, a toolArgs, extra map[string]any) (a11yReply, error) {
	req := map[string]any{"cmd": cmd, "app": a.App}
	for k, v := range extra {
		req[k] = v
	}
	var res a11yReply
	if err := s.a11y(20*time.Second, req, &res); err != nil {
		return res, fmt.Errorf("accessibility tree: %w", err)
	}
	s.scaleBoxes(res.Elements)
	for id, w := range res.Windows {
		s.scaleBoxes([]element{w})
		res.Windows[id] = w
	}
	s.remember(res.Elements, res.Windows)
	return res, nil
}

// roleAliases: newer at-spi2-core calls GtkButton "button"; either matches.
var roleAliases = map[string]string{"button": "push button"}

func normRole(r string) string {
	r = strings.ToLower(r)
	if a, ok := roleAliases[r]; ok {
		return a
	}
	return r
}

func matches(e element, text, role string) bool {
	if role != "" && !strings.EqualFold(e.Role, normRole(role)) {
		return false
	}
	t := strings.ToLower(text)
	return t == "" || strings.Contains(strings.ToLower(e.Name), t) || strings.Contains(strings.ToLower(e.Text), t)
}

func limitOf(a toolArgs, def int) int {
	if a.Limit <= 0 || a.Limit > 2000 {
		return def
	}
	return a.Limit
}

func depthOf(a toolArgs) int {
	if a.MaxDepth <= 0 || a.MaxDepth > 50 {
		return 30
	}
	return a.MaxDepth
}

func (s *Server) tree(a toolArgs) (any, error) {
	res, err := s.query("tree", a, map[string]any{"max_depth": depthOf(a), "limit": 2000})
	if err != nil {
		return nil, err
	}
	out := []element{}
	for _, e := range res.Elements {
		if a.Role == "" || strings.EqualFold(e.Role, normRole(a.Role)) || e.Depth <= 1 {
			out = append(out, e)
		}
	}
	n := limitOf(a, 400)
	r := map[string]any{"elements": out[:min(n, len(out))], "count": len(out)}
	if len(out) > n {
		r["truncated"] = fmt.Sprintf("showing %d of %d; narrow with app, role or max_depth", n, len(out))
	}
	return r, nil
}

func textOf(a toolArgs) string {
	if a.Text == nil {
		return ""
	}
	return *a.Text
}

// findEls asks the helper (AT-SPI Collection) for elements by role/text.
func (s *Server) findEls(a toolArgs, limit int) ([]element, error) {
	res, err := s.query("find", a, map[string]any{"role": normRole(a.Role), "text": textOf(a), "limit": limit})
	return res.Elements, err
}

func (s *Server) find(a toolArgs) (any, error) {
	if textOf(a) == "" && a.Role == "" {
		return nil, errors.New("give text and/or role")
	}
	els, err := s.findEls(a, limitOf(a, 50))
	if err != nil {
		return nil, err
	}
	return map[string]any{"elements": els, "count": len(els)}, nil
}

var actions = []string{"click", "focus", "set_text", "press"}

func (s *Server) cached(id string) (element, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.known[id]
	return e, ok
}

func (s *Server) act(a toolArgs) (any, error) {
	if !slices.Contains(actions, a.Action) {
		return nil, fmt.Errorf("action %q (click|focus|set_text|press)", a.Action)
	}
	e, ok := s.cached(a.ElementID)
	if !ok {
		return nil, fmt.Errorf("unknown element_id %q: take a cu_tree, cu_find or cu_screenshot{marks} snapshot first", a.ElementID)
	}
	req := map[string]any{"cmd": "act", "id": e.ID, "action": a.Action, "role": e.Role, "name": e.Name}
	if a.Action == "set_text" {
		if a.Text == nil {
			return nil, errors.New("set_text needs text")
		}
		if err := checkText(*a.Text, maxSetText, "text"); err != nil {
			return nil, err
		}
		req["text"] = *a.Text
	}
	var res map[string]any
	err := s.a11y(15*time.Second, req, &res)
	delete(res, "seq")
	return res, err
}

// screenSized: a window whose box is the whole screen at the origin. On
// Wayland, AT-SPI boxes are relative to the window, so these are the only
// windows whose boxes are known to be screen pixels (headless mode
// maximizes every window, without server-side decorations, for this).
func screenSized(w element, sw, sh int) bool {
	return len(w.BBox) == 4 && w.BBox[0] == 0 && w.BBox[1] == 0 && w.BBox[2] == sw && w.BBox[3] == sh
}

// errNoPixels says why an element's box is not a screen position.
type errNoPixels struct{ why string }

func (e errNoPixels) Error() string { return e.why }

// clickPoint returns the centre of element id's box, once the box has held
// still for boxSettle: an element that just appeared can report a box it is
// not drawn at, and a click there misses. An element seen earlier with the
// same box does not wait.
func (s *Server) clickPoint(id string, sw, sh int) (int, int, error) {
	deadline := time.Now().Add(boxSettleLimit)
	for {
		x, y, err := s.readPoint(id, sw, sh)
		if err != nil {
			return 0, 0, err
		}
		e, _ := s.cached(id)
		if time.Since(e.boxSince) >= s.boxSettle {
			return x, y, nil
		}
		if time.Now().After(deadline) {
			return 0, 0, fmt.Errorf("element %s (%s %q) is still moving (now at %v); look again, or use cu_act", id, e.Role, e.Name, e.BBox)
		}
		if err := s.guard(); err != nil {
			return 0, 0, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readPoint re-reads element id from the app (role, name, box, window)
// and returns the centre of its box. It fails when a click there could
// miss: the element changed or is gone, is hidden, or its window is behind
// the active one. When the box is not a screen position (a popup menu, or a
// window that is not maximized) the error is an errNoPixels.
func (s *Server) readPoint(id string, sw, sh int) (int, int, error) {
	was, ok := s.cached(id)
	if !ok {
		return 0, 0, fmt.Errorf("unknown element_id %q: take a cu_tree, cu_find or cu_screenshot{marks} snapshot first", id)
	}
	var res struct {
		Element element            `json:"element"`
		Showing bool               `json:"showing"`
		Windows map[string]element `json:"windows"`
	}
	if err := s.a11y(10*time.Second, map[string]any{"cmd": "get", "id": id}, &res); err != nil {
		return 0, 0, fmt.Errorf("element %s: %w", id, err)
	}
	e := res.Element
	s.scaleBoxes([]element{e})
	win := res.Windows[e.Win]
	s.scaleBoxes([]element{win})
	s.remember([]element{e}, res.Windows)
	switch {
	case e.Role != was.Role || e.Name != was.Name:
		return 0, 0, fmt.Errorf("element %s changed (was %s %q, now %s %q); find it again", id, was.Role, was.Name, e.Role, e.Name)
	case !res.Showing:
		return 0, 0, fmt.Errorf("element %s (%s %q) is not on screen now", id, e.Role, e.Name)
	case !slices.Contains(win.States, "active"):
		return 0, 0, fmt.Errorf("refused: element %s is in window %q, which is behind the active window; cu_focus it first", id, win.Name)
	case e.Popup:
		return 0, 0, errNoPixels{fmt.Sprintf("element %s is in a popup menu, whose screen position the toolkit does not report", id)}
	case !screenSized(win, sw, sh):
		return 0, 0, errNoPixels{fmt.Sprintf("element %s is in window %q (%v), which is not maximized, so its screen position is unknown", id, win.Name, win.BBox)}
	case len(e.BBox) != 4 || e.BBox[2] <= 0 || e.BBox[3] <= 0:
		return 0, 0, errNoPixels{fmt.Sprintf("element %s has no on-screen box", id)}
	}
	cx, cy := e.BBox[0]+e.BBox[2]/2, e.BBox[1]+e.BBox[3]/2
	if cx < 0 || cy < 0 || cx >= sw || cy >= sh {
		return 0, 0, fmt.Errorf("element %s is off screen at (%d,%d); scroll it into view", id, cx, cy)
	}
	return cx, cy, nil
}

// focusedField asks the active window which element has the keyboard focus.
// False when that is unknown (no accessibility tree, no focused element).
func (s *Server) focusedField() (element, bool) {
	var res a11yReply
	req := map[string]any{"cmd": "find", "active": true, "focused": true, "role": "", "text": "", "limit": 1}
	if err := s.a11y(10*time.Second, req, &res); err != nil || len(res.Elements) == 0 {
		return element{}, false
	}
	return res.Elements[0], true
}

// typeText types text with wtype and says whether it was seen to land:
// keystrokes that go nowhere are not an error of wtype's. "landed" is absent
// when that cannot be known: no focus reported, the field is gone (Enter
// submitted it), a newline in the text (it may send the field's contents
// away), or a field that already holds exactly this text.
func (s *Server) typeText(text string) (any, error) {
	before, known := s.focusedField()
	if _, err := s.run(60*time.Second, text, "wtype", "-d", "2", "-"); err != nil {
		return nil, err
	}
	res := map[string]any{"typed": len([]rune(text))}
	if !known || strings.TrimSpace(text) == "" || strings.Contains(text, "\n") || before.Text == text {
		return res, nil
	}
	if !slices.Contains(before.States, "editable") {
		res["landed"] = false
		res["warning"] = fmt.Sprintf("the focus is on %s %q, which is not editable, so the text may have gone nowhere; look, and focus the field", before.Role, before.Name)
		return res, nil
	}
	for deadline := time.Now().Add(s.typeLand); ; time.Sleep(100 * time.Millisecond) {
		var now struct {
			Element element `json:"element"`
		}
		if err := s.a11y(10*time.Second, map[string]any{"cmd": "get", "id": before.ID}, &now); err != nil {
			return res, nil
		}
		if now.Element.Text != before.Text || now.Element.Chars != before.Chars {
			res["landed"] = true
			return res, nil
		}
		if time.Now().After(deadline) {
			res["landed"] = false
			res["warning"] = fmt.Sprintf("%s %q had the focus but its contents did not change, so the text went elsewhere; look, click the field and type again", before.Role, before.Name)
			return res, nil
		}
	}
}

func (s *Server) wait(a toolArgs) (any, error) {
	text := textOf(a)
	if text == "" && a.Role == "" && a.Window == "" {
		return nil, errors.New("give text, role and/or window")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10
	}
	if timeout > 120 {
		return nil, errors.New("timeout is at most 120 s")
	}
	t0 := time.Now()
	deadline := t0.Add(time.Duration(timeout * float64(time.Second)))
	polls := 0
	for {
		polls++
		found, what, err := s.present(a, text)
		if err == nil && found != a.Gone {
			return map[string]any{"ok": true, "match": what, "waited_ms": time.Since(t0).Milliseconds(), "polls": polls}, nil
		}
		if err := s.guard(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, fmt.Errorf("timed out after %g s: %w", timeout, err)
			}
			return nil, fmt.Errorf("timed out after %g s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Server) present(a toolArgs, text string) (bool, any, error) {
	if a.Window != "" {
		ws, err := s.windows()
		if err != nil {
			return false, nil, err
		}
		w, ok := pickWindow(ws, a.Window)
		if !ok || (text == "" && a.Role == "") {
			return ok, w, nil
		}
	}
	els, err := s.findEls(a, 1)
	if err != nil {
		return false, nil, err
	}
	if len(els) > 0 {
		return true, els[0], nil
	}
	return false, nil, nil
}

func pickWindow(ws []window, q string) (window, bool) {
	q = strings.ToLower(q)
	for _, w := range ws {
		if strings.EqualFold(w.Title, q) || strings.EqualFold(w.AppID, q) {
			return w, true
		}
	}
	for _, w := range ws {
		if strings.Contains(strings.ToLower(w.Title), q) || strings.Contains(strings.ToLower(w.AppID), q) {
			return w, true
		}
	}
	return window{}, false
}
