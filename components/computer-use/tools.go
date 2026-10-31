package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type point struct {
	X         *int   `json:"x"`
	Y         *int   `json:"y"`
	ElementID string `json:"element_id"`
}

type rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type toolArgs struct {
	Marks      bool    `json:"marks"`
	Region     *rect   `json:"region"`
	Scale      float64 `json:"scale"`
	X          *int    `json:"x"`
	Y          *int    `json:"y"`
	ElementID  string  `json:"element_id"`
	Button     string  `json:"button"`
	Double     bool    `json:"double"`
	From       *point  `json:"from"`
	To         *point  `json:"to"`
	DX         int     `json:"dx"`
	DY         int     `json:"dy"`
	At         *point  `json:"at"`
	Text       *string `json:"text"`
	Combo      string  `json:"combo"`
	MaxDepth   int     `json:"max_depth"`
	App        string  `json:"app"`
	Role       string  `json:"role"`
	Action     string  `json:"action"`
	Window     string  `json:"window"`
	Gone       bool    `json:"gone"`
	Timeout    float64 `json:"timeout"`
	Path       string  `json:"path"`
	URL        string  `json:"url"`
	Screenshot bool    `json:"screenshot"`
	Limit      int     `json:"limit"`
}

type Server struct {
	cfg  Config
	exec Execer
	// open a display connection with a virtual pointer; tests swap in a fake
	display func() (pointerDev, error)
	helper  *atspiHelper
	// tests swap these in for the helper and the window list
	a11yCall    func(req map[string]any, v any) error
	listWindows func() ([]window, error)
	screenSize  func() (int, int, string)
	mu          sync.Mutex
	known       map[string]element // every element seen, by id (merged across calls)
}

// pointerDev is what the pointer and key tools need from a display connection.
type pointerDev interface {
	size() (int, int, string)
	moveTo(x, y int) error
	button(btn uint32, pressed bool) error
	wheel(axis uint32, n int) error
	keys(ops []keyOp) error
	sync() error
	Close() error
}

func (d *desktop) size() (int, int, string) { return d.out.Width, d.out.Height, d.out.Name }
func (d *desktop) sync() error              { return d.roundtrip(nil) }

func newServer(cfg Config) *Server {
	s := &Server{cfg: cfg, exec: realExec, known: map[string]element{}, helper: &atspiHelper{env: cfg.environ()}}
	s.display = func() (pointerDev, error) {
		if err := s.waitReady(); err != nil {
			return nil, err
		}
		d, err := openDesktop(cfg.Env)
		if err == nil {
			err = d.pointer()
			if err != nil {
				d.Close()
			}
		}
		return d, err
	}
	return s
}

// waitReady: in headless mode, `panda-cu session` writes $XDG_RUNTIME_DIR/ready
// once the output has its configured size.
func (s *Server) waitReady() error {
	if s.cfg.Mode != "headless" {
		return nil
	}
	f := s.cfg.runtimeFile("ready")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		_, err := os.Stat(f)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("display session: %w (panda-cu acts only on its own user's session)", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("display session not ready after 30 s (%s missing; see journalctl -u panda-cu)", f)
		}
	}
}

func (s *Server) runTool(name string, a toolArgs) ([]content, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	switch name {
	case "cu_screenshot":
		return s.screenshot(a)
	case "cu_click", "cu_move", "cu_drag", "cu_scroll":
		v, err := s.pointerTool(name, a)
		return s.after(a, v, err)
	case "cu_type":
		if a.Text == nil {
			return nil, errors.New("text is required")
		}
		if err := checkText(*a.Text, maxTypeChars, "text"); err != nil {
			return nil, err
		}
		_, err := s.run(60*time.Second, *a.Text, "wtype", "-d", "2", "-")
		return s.after(a, map[string]any{"typed": len([]rune(*a.Text))}, err)
	case "cu_key":
		args, err := keyArgs(a.Combo)
		if err != nil {
			return nil, err
		}
		ops, onUS, err := keyOps(args)
		if err != nil {
			return nil, err
		}
		if !onUS { // a keysym outside the US table: wtype builds a keymap for it
			_, err = s.run(execTimeout, "", append([]string{"wtype"}, args...)...)
			return s.after(a, map[string]any{"pressed": a.Combo}, err)
		}
		d, err := s.display()
		if err != nil {
			return nil, err
		}
		err = d.keys(ops)
		d.Close()
		return s.after(a, map[string]any{"pressed": a.Combo}, err)
	case "cu_tree":
		return jsonOut(s.tree(a))
	case "cu_find":
		return jsonOut(s.find(a))
	case "cu_act":
		v, err := s.act(a)
		return s.after(a, v, err)
	case "cu_wait":
		return jsonOut(s.wait(a))
	case "cu_windows":
		ws, err := s.windows()
		if err != nil {
			return nil, err
		}
		res := map[string]any{"windows": ws}
		if ex := s.recentExits(); len(ex) > 0 {
			res["recent_exits"] = ex
		}
		return jsonOut(res, nil)
	case "cu_focus":
		v, err := s.focus(a.Window)
		return s.after(a, v, err)
	case "cu_launch":
		v, err := s.launchTool(a.App, "")
		return s.after(a, v, err)
	case "cu_open":
		v, err := s.open(a)
		return s.after(a, v, err)
	case "cu_clipboard_get":
		if s.cfg.Mode == "attach" && !s.cfg.AllowClipboardRead {
			return nil, errors.New("refused: reading the clipboard of the person's session is off (services.panda-cu.allowClipboardRead)")
		}
		out, err := s.run(execTimeout, "", "wl-paste", "--no-newline")
		if err != nil && strings.Contains(err.Error(), "No selection") {
			return jsonOut(map[string]any{"text": ""}, nil)
		}
		return jsonOut(map[string]any{"text": string(out)}, err)
	case "cu_clipboard_set":
		if a.Text == nil {
			return nil, errors.New("text is required")
		}
		if err := checkText(*a.Text, maxClipboard, "text"); err != nil {
			return nil, err
		}
		// An explicit type: otherwise wl-copy sniffs it with xdg-mime.
		_, err := s.run(execTimeout, *a.Text, "wl-copy", "--type", "text/plain;charset=utf-8")
		return jsonOut(map[string]any{"set": len([]rune(*a.Text))}, err)
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

func jsonOut(v any, err error) ([]content, error) {
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	return []content{textContent(string(b))}, err
}

// after renders an action's result, plus (screenshot:true) a half-size
// screenshot once the screen has settled.
func (s *Server) after(a toolArgs, v any, err error) ([]content, error) {
	out, err := jsonOut(v, err)
	if err != nil || !a.Screenshot {
		return out, err
	}
	png, settled, ms, err := s.settled(0.5, settleLimit)
	if err != nil {
		return append(out, textContent("screenshot failed: "+err.Error())), nil
	}
	meta, _ := json.Marshal(map[string]any{"settled": settled, "settle_ms": ms})
	return append(out, imageContent(png), textContent(string(meta))), nil
}

const settleLimit = 1500 * time.Millisecond

// settled captures until two frames in a row are identical (the app has
// stopped repainting) or limit passes; it returns the last frame.
func (s *Server) settled(scale float64, limit time.Duration) ([]byte, bool, int64, error) {
	t0 := time.Now()
	time.Sleep(30 * time.Millisecond) // let the client see the input first
	prev, err := s.capture(nil, scale)
	if err != nil {
		return nil, false, 0, err
	}
	for time.Since(t0) < limit {
		time.Sleep(40 * time.Millisecond)
		cur, err := s.capture(nil, scale)
		if err != nil {
			return nil, false, 0, err
		}
		if bytes.Equal(cur, prev) {
			return cur, true, time.Since(t0).Milliseconds(), nil
		}
		prev = cur
	}
	return prev, false, time.Since(t0).Milliseconds(), nil
}

// screen is the output size: from the config in headless mode (no display
// round trip), else from wl_output.
func (s *Server) screen() (int, int, string, error) {
	if s.cfg.Mode == "headless" && s.cfg.Width > 0 && s.cfg.Height > 0 {
		return s.cfg.Width, s.cfg.Height, "", s.waitReady()
	}
	if s.screenSize != nil {
		w, h, n := s.screenSize()
		return w, h, n, nil
	}
	d, err := openDesktop(s.cfg.Env)
	if err != nil {
		return 0, 0, "", err
	}
	defer d.Close()
	w, h, n := d.size()
	return w, h, n, nil
}

// ---- screenshots

func (s *Server) capture(region *rect, scale float64) ([]byte, error) {
	w, h, name, err := s.screen()
	if err != nil {
		return nil, err
	}
	argv := []string{"grim", "-t", "png", "-l", "1"}
	if scale != 0 && scale != 1 {
		argv = append(argv, "-s", strconv.FormatFloat(scale, 'f', 3, 64))
	}
	switch {
	case region != nil:
		if region.W <= 0 || region.H <= 0 || region.X < 0 || region.Y < 0 || region.X+region.W > w || region.Y+region.H > h {
			return nil, fmt.Errorf("region must lie inside the %dx%d screen", w, h)
		}
		argv = append(argv, "-g", fmt.Sprintf("%d,%d %dx%d", region.X, region.Y, region.W, region.H))
	case name != "":
		argv = append(argv, "-o", name)
	}
	return s.run(execTimeout, "", append(argv, "-")...)
}

func (s *Server) screenshot(a toolArgs) ([]content, error) {
	if a.Scale < 0 || a.Scale > 1 {
		return nil, errors.New("scale must be in (0, 1]")
	}
	t0 := time.Now()
	png, err := s.capture(a.Region, a.Scale)
	if err != nil {
		return nil, err
	}
	w, h, _ := pngSize(png)
	meta := map[string]any{"width": w, "height": h, "ms": time.Since(t0).Milliseconds()}
	if a.Marks {
		if png, err = s.marks(a, png, w, h, meta); err != nil {
			return nil, err
		}
	}
	b, _ := json.Marshal(meta)
	return []content{imageContent(png), textContent(string(b))}, nil
}

// marks draws Set-of-Marks boxes on the interactive elements of the active
// window. Elements in a window whose screen position is unknown (not
// maximized) are left out and counted, never drawn in the wrong place.
func (s *Server) marks(a toolArgs, png []byte, w, h int, meta map[string]any) ([]byte, error) {
	sw, sh, _, err := s.screen()
	if err != nil {
		return nil, err
	}
	res, err := s.query("tree", toolArgs{App: a.App}, map[string]any{"interactive": true, "active": true, "max_depth": 30, "limit": 500})
	if err != nil {
		return nil, err
	}
	scale := a.Scale
	if scale == 0 {
		scale = 1
	}
	origin := image.Point{}
	if a.Region != nil {
		origin = image.Pt(a.Region.X, a.Region.Y)
	}
	var marks []mark
	list := []map[string]any{}
	unplaced := map[string]int{}
	for _, e := range res.Elements {
		if len(e.BBox) != 4 {
			continue
		}
		if win := res.Windows[e.Win]; !screenSized(win, sw, sh) || e.Popup {
			unplaced[win.Name]++
			continue
		}
		r := image.Rect(e.BBox[0], e.BBox[1], e.BBox[0]+e.BBox[2], e.BBox[1]+e.BBox[3]).Sub(origin)
		r = image.Rect(int(float64(r.Min.X)*scale), int(float64(r.Min.Y)*scale), int(float64(r.Max.X)*scale), int(float64(r.Max.Y)*scale))
		if r.Intersect(image.Rect(0, 0, w, h)).Empty() {
			continue
		}
		marks = append(marks, mark{len(marks) + 1, r})
		list = append(list, map[string]any{"mark": len(marks), "id": e.ID, "role": e.Role, "name": e.Name, "bbox": e.BBox})
	}
	meta["elements"] = list
	meta["hint"] = "bbox is [x,y,w,h] in screen pixels; act on an element with cu_click{element_id} or cu_act{element_id}"
	if len(unplaced) > 0 {
		meta["unplaced"] = unplaced
		meta["unplaced_hint"] = "elements in popup menus or in windows that are not maximized: their boxes are not screen positions; cu_click{element_id} clicks them through AT-SPI, or use cu_act"
	}
	return drawMarks(png, marks)
}

// ---- pointer

var buttons = map[string]uint32{"": btnLeft, "left": btnLeft, "right": btnRight, "middle": btnMiddle}

// target resolves a point: x,y (checked against the screen) or the centre
// of element_id, re-read from the app just before the pointer moves.
func (s *Server) target(d pointerDev, x, y *int, id string) (int, int, error) {
	w, h, _ := d.size()
	switch {
	case id != "":
		return s.clickPoint(id, w, h)
	case x != nil && y != nil:
		if *x < 0 || *y < 0 || *x >= w || *y >= h {
			return 0, 0, fmt.Errorf("(%d,%d) is outside the %dx%d screen", *x, *y, w, h)
		}
		return *x, *y, nil
	}
	return 0, 0, errors.New("give x and y, or element_id")
}

func (s *Server) pointerTool(name string, a toolArgs) (any, error) {
	btn, ok := buttons[a.Button]
	if !ok {
		return nil, fmt.Errorf("button %q (left|right|middle)", a.Button)
	}
	d, err := s.display()
	if err != nil {
		return nil, err
	}
	defer d.Close()
	pt := func(p *point, what string) (int, int, error) {
		if p == nil {
			return 0, 0, fmt.Errorf("%s is required", what)
		}
		x, y, err := s.target(d, p.X, p.Y, p.ElementID)
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", what, err)
		}
		return x, y, nil
	}
	settle := func() { _ = d.sync(); time.Sleep(30 * time.Millisecond) } // let the client see enter/motion
	switch name {
	case "cu_move", "cu_click":
		x, y, err := s.target(d, a.X, a.Y, a.ElementID)
		var np errNoPixels
		if errors.As(err, &np) && name == "cu_click" && btn == btnLeft && !a.Double {
			// No screen position, but a left click can still land: as the
			// element's own AT-SPI action.
			res, aerr := s.act(toolArgs{ElementID: a.ElementID, Action: "click"})
			if aerr != nil {
				return nil, fmt.Errorf("refused: %s, and its AT-SPI action failed: %w", np.why, aerr)
			}
			return map[string]any{"via": res.(map[string]any)["via"], "why": np.why + "; clicked through AT-SPI instead"}, nil
		}
		if errors.As(err, &np) {
			return nil, fmt.Errorf("refused: %s; a click by pixels could miss (use cu_act, or pixels from a screenshot)", np.why)
		}
		if err != nil {
			return nil, err
		}
		if err := d.moveTo(x, y); err != nil {
			return nil, err
		}
		settle()
		if name == "cu_click" {
			for i := range clicks(a.Double) {
				if i > 0 {
					time.Sleep(60 * time.Millisecond)
				}
				if err := errors.Join(d.button(btn, true), d.button(btn, false)); err != nil {
					return nil, err
				}
			}
		}
		return map[string]any{"x": x, "y": y}, d.sync()
	case "cu_drag":
		x0, y0, err := pt(a.From, "from")
		if err != nil {
			return nil, err
		}
		x1, y1, err := pt(a.To, "to")
		if err != nil {
			return nil, err
		}
		if err := d.moveTo(x0, y0); err != nil {
			return nil, err
		}
		settle()
		if err := d.button(btn, true); err != nil {
			return nil, err
		}
		settle()
		const steps = 16
		for i := 1; i <= steps; i++ {
			if err := d.moveTo(x0+(x1-x0)*i/steps, y0+(y1-y0)*i/steps); err != nil {
				return nil, err
			}
			_ = d.sync()
			time.Sleep(15 * time.Millisecond)
		}
		settle()
		return map[string]any{"from": []int{x0, y0}, "to": []int{x1, y1}}, errors.Join(d.button(btn, false), d.sync())
	case "cu_scroll":
		if a.DX < -50 || a.DX > 50 || a.DY < -50 || a.DY > 50 {
			return nil, errors.New("dx and dy are wheel notches, -50..50")
		}
		if a.At != nil {
			x, y, err := pt(a.At, "at")
			if err != nil {
				return nil, err
			}
			if err := d.moveTo(x, y); err != nil {
				return nil, err
			}
			settle()
		}
		return map[string]any{"dx": a.DX, "dy": a.DY}, errors.Join(d.wheel(0, a.DY), d.wheel(1, a.DX), d.sync())
	}
	return nil, fmt.Errorf("unknown pointer tool %q", name)
}

func clicks(double bool) int {
	if double {
		return 2
	}
	return 1
}
