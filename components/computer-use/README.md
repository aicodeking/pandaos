# panda-cu: computer use for agents

`services.panda-cu.enable = true;` gives the agent user a desktop it can see
and drive over MCP (`/etc/panda/mcp.d/panda-cu.json` → `panda-cu mcp`).
Scripts can run one tool with `panda-cu call TOOL '{json}'`, which prints one
JSON value (images come back as `png_base64`).

## Design choices

| Choice | Why |
|---|---|
| **headless** labwc (`WLR_BACKENDS=headless`, pixman, one output sized with `wlr-randr --custom-mode`) as `panda-cu.service`, User=agent, runtime dir `/run/panda-cu` (0700) | Wayland and the same compositor as the desktop profile. Nobody else sees the session, and only the agent uid can open its socket. |
| Pointer: zwlr_virtual_pointer_v1 spoken directly by Go (`wayland.go`) | `motion_absolute` on the output gives exact pixels with no acceleration. Separate press and release make drags possible. wlrctl only does relative moves and atomic clicks. ydotool/uinput would need a libinput seat, which the headless backend does not have. |
| Keyboard: text through `wtype` (zwp_virtual_keyboard_v1); combos (`cu_key`) through panda-cu's own virtual keyboard (`keys.go`) with the standard US keymap and real evdev codes | wtype builds a keymap per string, so any Unicode works. But GDK matches accelerators by hardware keycode after the short-lived keyboard is gone and the seat is back on the US keyboard, so wtype's custom keycodes never fired ctrl+s. Keysyms outside the US table still go through wtype. |
| `panda-cu session` holds an idle virtual pointer and keyboard | The seat never loses its pointer or keyboard capability between calls, so clients never rebind and drop the first event. |
| Apps start through the session's launcher socket | Apps then live in panda-cu.service, with a writable `/run/panda-cu` and the session env, not in the agent CLI's sandbox. Allowlisted ids only. The one extra argument must be an absolute path or an http(s) URL. |
| AT-SPI through `panda-cu-atspi` (Python + gi `Atspi`, argv in, JSON out) | This is the only well-supported client library. Element ids are child-index paths. `cu_act` refuses an id whose role or name changed. |
| Windows open fullscreen (labwc window rule) | On Wayland, AT-SPI boxes are relative to the window. With every window at (0,0), they are screen pixels, which is what makes `cu_click{element_id}` and Set-of-Marks correct. |
| Fallback (Xvfb + X WM + xdotool) not used | labwc implements virtual pointer, virtual keyboard, screencopy, foreign-toplevel and data-control. |

Determinism and the dialogs that are suppressed are listed one by one at the
top of `module.nix`.

**attach mode** targets `/run/user/$UID` of the caller (e.g.
`pandaos.desktop.autologinUser = "agent"`). It refuses sockets that belong
to other users. Windows there are not fullscreen, so AT-SPI boxes are
window-relative. Prefer `cu_find` + `cu_act` and click by pixels from
screenshots.

## Tools

`cu_screenshot{marks?,region?,scale?,app?}`, `cu_click{x,y|element_id,button?,double?}`,
`cu_move`, `cu_drag{from,to}`, `cu_scroll{dx,dy,at?}`, `cu_type{text}` (≤4000 chars),
`cu_key{combo}` (≤16 combos), `cu_tree{max_depth?,app?,role?,limit?}`, `cu_find{text?,role?}`,
`cu_act{element_id,action:click|focus|set_text|press}`, `cu_wait{text|role|window,gone?,timeout}`,
`cu_windows`, `cu_focus{window}`, `cu_launch{app}`, `cu_open{path|url}`,
`cu_clipboard_get`, `cu_clipboard_set{text}` (≤1 MiB). Every action tool takes
`screenshot:true` and then also returns a half-size screenshot. The full schemas are
printed by `panda-cu tools`.

## Apps

editor = mousepad (GTK 3; GtkSourceView exposes an editable `text` node;
much lighter than gnome-text-editor's GTK 4 + libadwaita). files = pcmanfm
(GTK 3; lighter than thunar + xfconf/exo). browser = `services.panda-browser`'s
Chromium if that is enabled, or `services.panda-cu.browser = pkgs.firefox`.
Add more with `extraApps`.
