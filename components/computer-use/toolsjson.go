package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ro  = `"annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}`
	act = `"annotations":{"readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":false}`
	nav = `"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false}`

	pXY   = `"x":{"type":"integer","minimum":0,"description":"Screen x in screenshot pixels (full-size screenshot, 0 = left)."},"y":{"type":"integer","minimum":0,"description":"Screen y in screenshot pixels (0 = top)."}`
	pEl   = `"element_id":{"type":"string","description":"An id from the latest cu_tree, cu_find or cu_screenshot{marks} result; the pointer goes to the centre of its box."}`
	pShot = `"screenshot":{"type":"boolean","default":false,"description":"Also return a half-size screenshot taken 150 ms after the action, to save a round trip."}`
	pPt   = `{"type":"object","properties":{"x":{"type":"integer","minimum":0},"y":{"type":"integer","minimum":0}},"required":["x","y"]`
)

const toolsJSON = `[
{"name":"cu_screenshot","description":"PNG screenshot of the agent's own display session (no other user's screen). marks=true overlays numbered boxes on the interactive accessibility elements of the focused window (Set-of-Marks) and also returns their list [{mark,id,role,name,bbox:[x,y,w,h]}]; act on them with cu_click{element_id} or cu_act. Coordinates for the other tools are in full-size screenshot pixels.",` + ro + `,"inputSchema":{"type":"object","properties":{"marks":{"type":"boolean","default":false,"description":"Overlay Set-of-Marks boxes and return the element list."},"region":{"type":"object","description":"Only this rectangle, in screen pixels.","properties":{"x":{"type":"integer"},"y":{"type":"integer"},"w":{"type":"integer"},"h":{"type":"integer"}},"required":["x","y","w","h"]},"scale":{"type":"number","exclusiveMinimum":0,"maximum":1,"description":"Downscale factor, e.g. 0.5 for a cheaper image (default 1). Coordinates stay in full-size pixels."},"app":{"type":"string","description":"With marks: limit marks to this application."}}}},
{"name":"cu_click","description":"Move the pointer and click: at x,y or at the centre of element_id. Coordinates outside the screen are refused. Prefer cu_act (no pixels) when the element has an AT-SPI action.",` + act + `,"inputSchema":{"type":"object","properties":{` + pXY + `,` + pEl + `,"button":{"type":"string","enum":["left","right","middle"],"default":"left","description":"Mouse button."},"double":{"type":"boolean","default":false,"description":"Double-click."},` + pShot + `}}},
{"name":"cu_move","description":"Move the pointer (hover) to x,y or to the centre of element_id, without clicking.",` + nav + `,"inputSchema":{"type":"object","properties":{` + pXY + `,` + pEl + `,` + pShot + `}}},
{"name":"cu_drag","description":"Press a button at from, move in small steps to to, release (drag and drop, selections, sliders).",` + act + `,"inputSchema":{"type":"object","required":["from","to"],"properties":{"from":` + pPt + `,"description":"Start point in screen pixels."},"to":` + pPt + `,"description":"End point in screen pixels."},"button":{"type":"string","enum":["left","right","middle"],"default":"left","description":"Mouse button."},` + pShot + `}}},
{"name":"cu_scroll","description":"Turn the mouse wheel: dy notches down (negative = up), dx notches right (negative = left), optionally after moving the pointer to at.",` + nav + `,"inputSchema":{"type":"object","properties":{"dx":{"type":"integer","minimum":-50,"maximum":50,"default":0,"description":"Horizontal notches, +right."},"dy":{"type":"integer","minimum":-50,"maximum":50,"default":0,"description":"Vertical notches, +down."},"at":` + pPt + `,"description":"Scroll over this point (default: where the pointer is)."},` + pShot + `}}},
{"name":"cu_type","description":"Type text into the focused widget, as keystrokes (any Unicode; newline = Enter). At most 4000 characters per call. For long text or to replace a field's contents use cu_act set_text.",` + act + `,"inputSchema":{"type":"object","required":["text"],"properties":{"text":{"type":"string","maxLength":4000,"description":"Text to type."},` + pShot + `}}},
{"name":"cu_key","description":"Press key combos in order, space-separated: e.g. \"ctrl+s\", \"ctrl+a BackSpace\", \"alt+F4\", \"Return\". Modifiers: ctrl, shift, alt, altgr, super. Keys: XKB keysym names (Return, Escape, Tab, BackSpace, Delete, Home, End, Prior, Next, Up, F5, a, slash) or aliases (enter, esc, pgup, del, ...). At most 16 combos.",` + act + `,"inputSchema":{"type":"object","required":["combo"],"properties":{"combo":{"type":"string","maxLength":256,"description":"Combos such as ctrl+shift+t, separated by spaces."},` + pShot + `}}},
{"name":"cu_tree","description":"Compact AT-SPI accessibility tree of the running apps: [{id,d(depth),role,name,bbox:[x,y,w,h],states,actions,text}]. ids are child-index paths, valid for this snapshot (cu_act refuses an id whose element changed). Hidden subtrees are pruned.",` + ro + `,"inputSchema":{"type":"object","properties":{"max_depth":{"type":"integer","minimum":1,"maximum":50,"default":30,"description":"Depth limit below each application."},"app":{"type":"string","description":"Only applications whose name or window title contains this (case-insensitive), e.g. mousepad."},"role":{"type":"string","description":"Only elements with this role (e.g. \"push button\", \"text\", \"entry\", \"menu item\"); apps and windows are always kept."},"limit":{"type":"integer","minimum":1,"maximum":2000,"default":400,"description":"Maximum elements returned."}}}},
{"name":"cu_find","description":"Find accessibility elements whose name or text contains text (case-insensitive) and/or with this role. Returns elements with ids usable by cu_click and cu_act.",` + ro + `,"inputSchema":{"type":"object","properties":{"text":{"type":"string","description":"Substring of the name or text."},"role":{"type":"string","description":"Exact role, e.g. \"push button\"."},"app":{"type":"string","description":"Only this application."},"limit":{"type":"integer","minimum":1,"maximum":2000,"default":50,"description":"Maximum matches."}}}},
{"name":"cu_act","description":"Act on an element through AT-SPI, without pixels: click/press (runs its action), focus, set_text (replaces an editable field's whole contents, up to 100000 characters). Works even when the element is covered or off screen.",` + act + `,"inputSchema":{"type":"object","required":["element_id","action"],"properties":{` + pEl + `,"action":{"type":"string","enum":["click","focus","set_text","press"],"description":"What to do."},"text":{"type":"string","description":"For set_text: the new contents."},` + pShot + `}}},
{"name":"cu_wait","description":"Wait until an element with text and/or role exists (or, with gone=true, no longer exists), and/or a window whose title or app id contains window. Polls every 250 ms. Use after actions that open dialogs or load content.",` + ro + `,"inputSchema":{"type":"object","properties":{"text":{"type":"string","description":"Element name/text substring."},"role":{"type":"string","description":"Element role."},"app":{"type":"string","description":"Only look in this application."},"window":{"type":"string","description":"Window title or app id substring."},"gone":{"type":"boolean","default":false,"description":"Wait for it to disappear instead."},"timeout":{"type":"number","exclusiveMinimum":0,"maximum":120,"default":10,"description":"Seconds."}}}},
{"name":"cu_windows","description":"Open windows (app_id and title) in the agent's session.",` + ro + `,"inputSchema":{"type":"object","properties":{}}},
{"name":"cu_focus","description":"Raise and focus the window whose title or app id matches window (exact match first, then substring).",` + nav + `,"inputSchema":{"type":"object","required":["window"],"properties":{"window":{"type":"string","description":"Title or app id."},` + pShot + `}}},
{"name":"cu_launch","description":"Start an allowlisted app (its .desktop entry) in the agent's session and wait up to 20 s for its window. Unknown names are refused and the error lists the allowlist (default: editor, files, browser when configured).",` + nav + `,"inputSchema":{"type":"object","required":["app"],"properties":{"app":{"type":"string","description":"App id from the allowlist, e.g. editor."},` + pShot + `}}},
{"name":"cu_open","description":"Open a path (directory in the file manager, file in the text editor) or an http(s) URL (in the browser) in the agent's session.",` + nav + `,"inputSchema":{"type":"object","properties":{"path":{"type":"string","description":"Absolute path or ~/..."},"url":{"type":"string","description":"http(s) URL."},` + pShot + `}}},
{"name":"cu_clipboard_get","description":"Text on the session clipboard (empty if none).",` + ro + `,"inputSchema":{"type":"object","properties":{}}},
{"name":"cu_clipboard_set","description":"Put text on the session clipboard (up to 1 MiB), e.g. before ctrl+v.",` + nav + `,"inputSchema":{"type":"object","required":["text"],"properties":{"text":{"type":"string","description":"Text to copy."}}}}
]`

const instructions = `panda-cu drives the agent's own graphical session. Loop: look (cu_screenshot, or cu_screenshot{marks:true} for numbered clickable elements), act, look again; pass screenshot:true on actions to get the result in the same call. Prefer the accessibility tree (cu_find / cu_tree, then cu_act) over pixels: it does not depend on layout. Windows open fullscreen one at a time; switch with cu_focus. Use cu_wait after anything that opens a window or dialog.`

var mcpTools = json.RawMessage(toolsJSON)

func (s *Server) ServeMCP(r io.Reader, w io.Writer) error {
	srv := &mcpServer{
		Name: "panda-cu", Version: version, Instructions: instructions, Tools: mcpTools,
		Call: func(name string, raw json.RawMessage) ([]content, error) {
			var a toolArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %w", err)
			}
			return s.runTool(name, a)
		},
	}
	return srv.Serve(r, w)
}

// callCLI runs one tool for scripts: one JSON value on stdout. A tool's JSON
// text is printed as is; an image is folded in as "png_base64".
func callCLI(s *Server, args []string, w io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: panda-cu call TOOL [JSON-ARGS]")
	}
	var a toolArgs
	if len(args) == 2 {
		if err := json.Unmarshal([]byte(args[1]), &a); err != nil {
			return fmt.Errorf("bad arguments: %w", err)
		}
	}
	out, err := s.runTool(args[0], a)
	if err != nil {
		return err
	}
	if len(out) == 1 && out[0]["type"] == "text" && json.Valid([]byte(out[0]["text"])) {
		_, err := fmt.Fprintln(w, out[0]["text"])
		return err
	}
	res := map[string]any{}
	for _, c := range out {
		switch {
		case c["type"] == "image":
			res["png_base64"] = c["data"]
		case json.Unmarshal([]byte(c["text"]), &res) != nil:
			res["text"] = c["text"]
		}
	}
	return json.NewEncoder(w).Encode(res)
}
