# Computer use

PandaOS lets agents use a graphical desktop the way a person does: look at
the screen, move the mouse, type, and read the accessibility (AT-SPI) tree.
Two parts:

- **`panda-cu`** (`components/computer-use`): an MCP server and CLI that the
  agent CLIs on the box get through the MCP registry
  (`/etc/panda/mcp.d/panda-cu.json`).
- **A desktop tuned for it** (`components/desktop`, the desktop profile):
  predictable screenshots and input for people and agents alike.

`bench/cubench` measures how well an agent does desktop tasks on PandaOS
against other Linux desktops.

## Modes

| `services.panda-cu.mode` | What it drives | Cost |
|---|---|---|
| `headless` (default, also in the desktop profile) | a private labwc session for `services.panda-cu.user` (default `agent`), `panda-cu.service`, 1280x800, runtime dir `/run/panda-cu` (0700) | labwc + pixman in RAM while the service runs |
| `attach` (opt-in) | the logged-in Wayland session of the user who runs `panda-cu` (`/run/user/$UID`); sockets owned by another user are refused | no service: the package and the AT-SPI env |

The module is in `nixosModules.default` and **off by default**. The desktop
profile turns it on in **headless** mode: agents get a desktop of their own
and never see or touch the person's screen. Sharing the person's session is
one option away, with the controls below:

```nix
services.panda-cu = { enable = true; mode = "attach"; };
```

## Safety defaults

Each one is an option; the defaults are the safe choice.

| Default | Option | Why |
|---|---|---|
| headless, not attach | `services.panda-cu.mode` | an agent drives its own screen, not the person's |
| attach: `cu_clipboard_get` refused | `services.panda-cu.allowClipboardRead` | the person's clipboard may hold passwords; headless always allows it (the agent's own clipboard) |
| attach: a red AGENT badge in waybar while an agent acts; Super+Escape (or a click on the badge) ends running `panda-cu` processes and refuses new calls for 10 minutes | `panda-cu revoke MINUTES`, `panda-cu revoke 0` | the person sees and can stop it. A stop button, not a sandbox: an agent running as the same user could undo it |
| `panda-cu-busy` marks count for at most 2 h | `pandaos.desktop.cuBusyMaxMinutes` | a forgotten `on` or a stuck `run` cannot keep the screen unlocked forever |
| apps started in the headless session run in `panda-cu.service`, inside `agents.slice`, with `MemoryMax` 2G, `TasksMax` 1024, `ProtectSystem=strict`, `ProtectHome`, `NoNewPrivileges` | `services.panda-cu.memoryMax`, `.tasksMax` | the agent's own limits; no access to `/home` or other people's sessions |
| the private session bus has no eavesdrop rule | | apps on it cannot read each other's D-Bus traffic |

The app allowlist (`cu_launch`, `cu_open`) is **not a security boundary**: it
keeps agents to known, tested apps, but those apps can open files and start
other programs (a browser, a file manager). The boundaries are the user
(`agent`), its sandbox and its limits.

## Design

| Choice | Why |
|---|---|
| labwc (wlroots) in both modes; headless uses `WLR_BACKENDS=headless` + pixman | The same compositor as the desktop; no GPU needed. |
| Every headless window, dialogs included, opens **maximized** with client-side decorations only (labwc window rule, `<decoration>client</decoration>`) | AT-SPI boxes are window-relative on Wayland. A maximized CSD window sits at (0,0) with no shadow margin, so its boxes are screen pixels. Maximized, not fullscreen: GTK hides a fullscreen window's header bar (a file chooser's Save button). |
| Clicks by `element_id` re-read the element first (role, name, box, window) and are refused when a click could miss: it changed or is gone, it is hidden, its window is behind the active one, or its window is not maximized (unknown position, e.g. a fixed-size message dialog). Marks leave such windows out and count them. | Never misclick; `cu_act` still works there. |
| Pointer: `zwlr_virtual_pointer_v1` spoken directly from Go | `motion_absolute` gives exact pixels with no acceleration; separate press/release make drags work. |
| Keyboard: text through `wtype`; key combos through panda-cu's own virtual keyboard with the US keymap plus `level3(ralt_switch)` | wtype uploads a keymap per string, so any Unicode types. GTK matches accelerators by hardware keycode, so `cu_key` sends real evdev codes; `altgr` is Right Alt as ISO_Level3_Shift (Mod5). |
| Windows: `zwlr_foreign_toplevel_manager_v1` spoken directly | States (activated, maximized), dialogs, focusing by handle, and `cu_launch` finding the new window by handle, not by title. |
| AT-SPI: one long-lived `panda-cu-atspi` (Python + gi `Atspi`) per MCP session, JSON lines over a pipe; `cu_find`/`cu_wait` use the Collection API (one GetMatches per window) | Python start-up and typelib loading are paid once, not per call. The libatspi cache is off, so answers are never stale. Element ids name one accessible object for the helper's lifetime, whichever tool returned them. |
| `cu_launch` goes through the session's launcher, which reports the app's exit status and last output | An app that dies before its window is an error with the reason; `cu_windows` lists recent exits. |
| `screenshot:true` on an action waits until two frames in a row are identical (at most 1.5 s) | The image shows the result, not a half-painted frame. |
| `panda-cu session` failing stops labwc; `Restart=always` brings up a fresh session | Never a compositor without a launcher or ready file. |

## Tools

`panda-cu tools` prints the full schemas.

| Tool | Does |
|---|---|
| `cu_screenshot{marks?,region?,scale?,app?}` | PNG; `marks` overlays numbered boxes on interactive elements of the active window and returns them |
| `cu_click{x,y \| element_id, button?, double?}`, `cu_move`, `cu_drag{from,to}` (each `{x,y}` or `{element_id}`), `cu_scroll{dx,dy,at?}` | pointer |
| `cu_type{text}` (≤ 4000 chars), `cu_key{combo}` (≤ 16 combos) | keyboard |
| `cu_tree{max_depth?,app?,role?,limit?}`, `cu_find{text?,role?,app?}` | read the AT-SPI tree |
| `cu_act{element_id, action: click\|focus\|set_text\|press}` | act on an element without pixels |
| `cu_wait{text\|role\|window, gone?, timeout}` | wait for an element or window |
| `cu_windows`, `cu_focus{window}`, `cu_launch{app}`, `cu_open{path\|url}` | windows and apps |
| `cu_clipboard_get`, `cu_clipboard_set{text}` (≤ 1 MiB) | clipboard |

Scripts use `panda-cu call TOOL '{json}'`, which prints one JSON value
(images as `png_base64`) and exits non-zero on error.

## Determinism

What keeps screenshots and accessibility boxes the same from run to run.

Headless session (`module.nix` lists each one): no animations, no cursor
blink; one font family (DejaVu), 96 dpi, slight hinting, greyscale AA, scale
1; every window maximized at the origin; a private session D-Bus that only
knows the AT-SPI bus (no notification daemon, keyring or portal can start);
fresh XDG dirs on every start, the GSettings keyfile backend, Chromium
`--no-first-run --password-store=basic`.

Desktop (`components/desktop`, checked by `test-config.sh`): no drop
shadows, opaque windows, new windows centred and focused, a 2 px focus ring,
32x28 titlebar buttons, flat pointer acceleration, key repeat 25/s after
600 ms, US layout; GTK without animations or cursor blink, always-visible
scrollbars, dark Adwaita; the AT-SPI bus on; `panda-dnd` (Super+N);
`panda-cu-busy run CMD`; `pandaos.desktop.autoLock = false` turns the idle
lock off.

## Tests

- `checks.panda-cu` (`components/computer-use/vmtest.nix`): one MCP stdio
  session as `agent` in a pandacode-host-like sandbox. Every section runs and
  reports, the test fails if any failed. Thresholds are exact: Mousepad's
  save dialog must save by **keyboard** (the AT-SPI path is only a separately
  reported fallback); 20/20 clicks on 80 GTK buttons (by `element_id` and by
  coordinates); 12/12 on 24 px buttons; a menu item and a dialog button by
  `element_id`; a fixed-size message dialog is hit or refused, never missed;
  a list row that needs scrolling becomes visible and is clicked; a slider
  dragged from its `element_id` reports its new value; Unicode multi-line
  `cu_type` reads back exactly; a Chromium form is filled and submitted; an
  app killed mid-task gives clean errors and shows in `recent_exits`; an app
  that exits at launch is an error with its status and output; p50 of
  `cu_find`, `cu_act` and one `cu_wait` poll under 200 ms; the clipboard;
  refused inputs; another user cannot reach the session; killing
  `panda-cu session` restarts the whole session. METRICs include
  `screenshot_ms_median`, `click_ms_median`, `find_ms_p50`, `act_ms_p50`,
  `wait_poll_ms_p50`, `move_with_screenshot_ms`, `clipboard_set_ms`,
  `click_accuracy`, `small_accuracy`, `drag_value`, `save_via`.
- `checks.boot-desktop`: `test-config.sh` on `/etc/xdg`, the AT-SPI bus,
  panda-cu in attach mode in the person's session, do-not-disturb.
- CI: `go vet`/`go test -race`, the desktop shell tests, shellcheck.

## Known limits

- attach mode: windows are not maximized (and have server-side title bars),
  so clicks by `element_id` are refused there; use `cu_act`, or pixels from
  screenshots.
- Popups that are windows of their own (a combo box list, an entry
  completion) count as unknown positions too.
- `cu_key` sends US-layout keycodes; other keysyms go through wtype, whose
  accelerators GTK may not fire.

## cubench

`bench/cubench` runs the same 25 tasks, checkers, model and tool interface
on PandaOS, Ubuntu 24.04 GNOME and Debian 13 Xfce (Fedora 44 opt-in), each in
a QEMU/KVM VM with 4 vCPU, 4 GB and a 1280x800 virtio-vga display. The agent
sees QMP screenshots and sends QMP mouse and keyboard events (the generic
tools). On PandaOS a second column adds the `cu_*` tools (`--tools panda`).
Details: [bench/cubench/README.md](../bench/cubench/README.md).

Locally (Linux with KVM, QEMU ≥ 7.1, OVMF, `cloud-localds`):

```sh
python3 bench/cubench/test_cubench.py
ssh-keygen -t ed25519 -N '' -f key
python3 bench/cubench/cubench.py prepare --os debian-13-xfce --key key
# VM plumbing only, no API key: scripted agent, strict exit code
python3 bench/cubench/cubench.py run --smoke --os debian-13-xfce --key key --repeats 1 \
  --tasks editor-new-file,browser-form --out smoke
ANTHROPIC_API_KEY=... python3 bench/cubench/cubench.py run --os debian-13-xfce --key key --out out
python3 bench/cubench/cubench.py report out/results-*.json --out bench/cubench/results/$(date -u +%F)
```

In CI (`.github/workflows/cubench.yml`, manual):

```sh
gh secret set ANTHROPIC_API_KEY            # once
gh workflow run cubench.yml -f provider=anthropic -f repeats=3
```

Without the provider's key secret the same workflow runs in **smoke mode**:
the dry-run self-test, then each OS disk is prepared (cached) and booted, and
the scripted agent checks that QMP screenshots are 1280x800, that clicks,
the wheel and keys reach a kiosk browser page 1:1 (`smoke/input-probe`), that
task setup and checkers run over ssh, and that `panda-cu call` works in the
PandaOS session. Each job uploads `cubench-smoke-<os>-<tools>-r1` with
`<os>-<tools>-desktop.png`, the first screenshot after boot.
