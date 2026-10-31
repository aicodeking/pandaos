package main

import (
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
	X int `json:"x"`
	Y int `json:"y"`
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

// element is one AT-SPI node as panda-cu-atspi reports it.
type element struct {
	ID      string   `json:"id"`
	Depth   int      `json:"d"`
	Role    string   `json:"role"`
	Name    string   `json:"name"`
	BBox    []int    `json:"bbox,omitempty"`
	States  []string `json:"states,omitempty"`
	Actions []string `json:"actions,omitempty"`
	Text    string   `json:"text,omitempty"`
}

type Server struct {
	cfg  Config
	exec Execer
	// open a display connection; tests swap in a fake
	display func() (pointerDev, error)
	mu      sync.Mutex
	snap    map[string]element // last cu_tree / cu_find / marks snapshot
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
	s := &Server{cfg: cfg, exec: realExec, snap: map[string]element{}}
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
		ops, err := keyOps(args)
		if err != nil {
			return nil, err
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
		return jsonOut(s.windows())
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
		_, err := s.run(execTimeout, *a.Text, "wl-copy")
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

// after renders an action's result, plus a small screenshot when asked.
func (s *Server) after(a toolArgs, v any, err error) ([]content, error) {
	out, err := jsonOut(v, err)
	if err != nil || !a.Screenshot {
		return out, err
	}
	time.Sleep(150 * time.Millisecond) // let the app repaint
	png, err := s.capture(nil, 0.5)
	if err != nil {
		return append(out, textContent("screenshot failed: "+err.Error())), nil
	}
	return append(out, imageContent(png)), nil
}

// ---- screenshots

func (s *Server) capture(region *rect, scale float64) ([]byte, error) {
	d, err := s.display()
	if err != nil {
		return nil, err
	}
	w, h, name := d.size()
	d.Close()
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
		els, err := s.snapshot(toolArgs{App: a.App}, true)
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
		var list []map[string]any
		for _, e := range els {
			if len(e.BBox) != 4 {
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
		if png, err = drawMarks(png, marks); err != nil {
			return nil, err
		}
		meta["elements"] = list
		meta["hint"] = "bbox is [x,y,w,h] in screen pixels; use cu_click{element_id} or cu_act{element_id}"
	}
	b, _ := json.Marshal(meta)
	return []content{imageContent(png), textContent(string(b))}, nil
}

// ---- pointer

var buttons = map[string]uint32{"": btnLeft, "left": btnLeft, "right": btnRight, "middle": btnMiddle}

func (s *Server) target(d pointerDev, x, y *int, id string) (int, int, error) {
	w, h, _ := d.size()
	switch {
	case id != "":
		s.mu.Lock()
		e, ok := s.snap[id]
		s.mu.Unlock()
		if !ok {
			return 0, 0, fmt.Errorf("unknown element_id %q: take a cu_tree, cu_find or cu_screenshot{marks} snapshot first", id)
		}
		if len(e.BBox) != 4 || e.BBox[2] <= 0 || e.BBox[3] <= 0 {
			return 0, 0, fmt.Errorf("element %s has no on-screen box; try cu_act", id)
		}
		cx, cy := e.BBox[0]+e.BBox[2]/2, e.BBox[1]+e.BBox[3]/2
		if cx < 0 || cy < 0 || cx >= w || cy >= h {
			return 0, 0, fmt.Errorf("element %s is off screen at (%d,%d); scroll it into view", id, cx, cy)
		}
		return cx, cy, nil
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
	pt := func(p *point) (*int, *int) {
		if p == nil {
			return nil, nil
		}
		return &p.X, &p.Y
	}
	settle := func() { _ = d.sync(); time.Sleep(30 * time.Millisecond) } // let the client see enter/motion
	switch name {
	case "cu_move", "cu_click":
		x, y, err := s.target(d, a.X, a.Y, a.ElementID)
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
		fx, fy := pt(a.From)
		x0, y0, err := s.target(d, fx, fy, "")
		if err != nil {
			return nil, fmt.Errorf("from: %w", err)
		}
		tx, ty := pt(a.To)
		x1, y1, err := s.target(d, tx, ty, "")
		if err != nil {
			return nil, fmt.Errorf("to: %w", err)
		}
		if err := d.moveTo(x0, y0); err != nil {
			return nil, err
		}
		settle()
		if err := d.button(btn, true); err != nil {
			return nil, err
		}
		settle()
		const steps = 12
		for i := 1; i <= steps; i++ {
			if err := d.moveTo(x0+(x1-x0)*i/steps, y0+(y1-y0)*i/steps); err != nil {
				return nil, err
			}
			time.Sleep(15 * time.Millisecond)
		}
		settle()
		return map[string]any{"from": []int{x0, y0}, "to": []int{x1, y1}}, errors.Join(d.button(btn, false), d.sync())
	case "cu_scroll":
		if a.DX < -50 || a.DX > 50 || a.DY < -50 || a.DY > 50 {
			return nil, errors.New("dx and dy are wheel notches, -50..50")
		}
		if a.At != nil {
			x, y, err := s.target(d, &a.At.X, &a.At.Y, "")
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
