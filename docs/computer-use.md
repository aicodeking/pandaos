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
| `attach` (desktop profile default) | the logged-in Wayland session of the user who runs `panda-cu` (`/run/user/$UID`); sockets owned by another user are refused | no service, no idle RAM: the package and the AT-SPI env |
| `headless` | a private labwc session for `services.panda-cu.user` (default `agent`), `panda-cu.service`, 1280x800, runtime dir `/run/panda-cu` (0700) | labwc + pixman in RAM while the service runs |

The module is in `nixosModules.default` and **off by default**. The desktop
profile turns it on in `attach` mode, because a desktop box already has a
session to share and attach mode adds no running service. That keeps the
idle-RAM budget unchanged. A box where agents need a screen of their own
(the headless profile, or a desktop where the `agent` user must not see your
session) sets:

```nix
services.panda-cu = { enable = true; mode = "headless"; };
```

In attach mode the registry entry is the same, but the tools only work for
an agent CLI that runs inside the logged-in session. The `agent` user of
`pandacode-host` has no session there, so its calls fail with a clear error.

## Design

| Choice | Why |
|---|---|
| labwc (wlroots) in both modes; headless uses `WLR_BACKENDS=headless` + pixman | The same compositor as the desktop; no GPU needed. |
| Pointer: `zwlr_virtual_pointer_v1` spoken directly from Go | `motion_absolute` gives exact pixels with no acceleration; separate press/release make drags work. |
| Keyboard: text through `wtype`; key combos through panda-cu's own virtual keyboard with the standard US keymap | wtype uploads a keymap per string, so any Unicode types correctly. GTK matches accelerators by hardware keycode once the seat is back on the US keyboard, so `cu_key` sends real US evdev codes (modifiers as key presses); other keysyms fall back to wtype. |
| Screenshots: `grim` (wlr-screencopy); windows: `wlrctl` (foreign-toplevel); clipboard: `wl-clipboard` | Standard wlroots protocols, small tools. |
| AT-SPI: `panda-cu-atspi` (Python + gi `Atspi`) | The only well-supported client library. Element ids are child-index paths; `cu_act` refuses an id whose role or name changed. |
| Headless windows open fullscreen | AT-SPI boxes are window-relative on Wayland; with every window at (0,0) they are screen pixels, so Set-of-Marks and `cu_click{element_id}` are exact. |
| Apps start from an allowlist (`editor` = Mousepad, `files` = PCManFM, `browser` when configured, plus `extraApps`) | `cu_launch` cannot run arbitrary commands. |

## Tools

`panda-cu tools` prints the full schemas. Every action tool also takes
`screenshot: true` and then returns a half-size screenshot taken 150 ms
after the action.

| Tool | Does |
|---|---|
| `cu_screenshot{marks?,region?,scale?,app?}` | PNG; `marks` overlays numbered boxes on interactive elements and returns them |
| `cu_click{x,y \| element_id, button?, double?}`, `cu_move`, `cu_drag{from,to}`, `cu_scroll{dx,dy,at?}` | pointer |
| `cu_type{text}` (≤ 4000 chars), `cu_key{combo}` (≤ 16 combos) | keyboard |
| `cu_tree{max_depth?,app?,role?,limit?}`, `cu_find{text?,role?,app?}` | read the AT-SPI tree |
| `cu_act{element_id, action: click\|focus\|set_text\|press}` | act on an element without pixels |
| `cu_wait{text\|role\|window, gone?, timeout}` | wait for an element or window |
| `cu_windows`, `cu_focus{window}`, `cu_launch{app}`, `cu_open{path\|url}` | windows and apps |
| `cu_clipboard_get`, `cu_clipboard_set{text}` (≤ 1 MiB) | clipboard |

Scripts use `panda-cu call TOOL '{json}'`, which prints one JSON value
(images as `png_base64`) and exits non-zero on error.

Tips for agents (also in `/etc/panda/AGENTS.md`): prefer `cu_find`/`cu_tree`
then `cu_act` over pixel clicks; when you must click, use
`cu_screenshot{marks:true}` and `cu_click{element_id}`; call `cu_wait` after
anything that opens a window.

## Determinism

What keeps screenshots and accessibility boxes the same from run to run.

Headless session (`module.nix` lists each one):

- no animations (labwc has none; GTK `gtk-enable-animations=false`), no
  cursor blink;
- one font family (DejaVu), 96 dpi, slight hinting, greyscale AA, scale 1;
- windows fullscreen, so the window origin is the screen origin;
- a private session D-Bus that only knows the AT-SPI bus: no notification
  daemon, keyring or portal can start;
- fresh XDG config/cache/data dirs on every start, the GSettings keyfile
  backend, Chromium `--no-first-run --password-store=basic`: no first-run or
  keyring dialogs.

Desktop (`components/desktop`, checked by `test-config.sh`):

- labwc: no drop shadows, opaque windows, new windows centred and focused,
  a 2 px accent ring on the focused window, 32x28 titlebar buttons, flat
  pointer acceleration, absolute devices (QEMU usb-tablet) mapped 1:1, key
  repeat 25/s after 600 ms, US layout;
- GTK: no animations or cursor blink, always-visible scrollbars, dark
  Adwaita, a fixed cursor theme and size; waybar has no hover transitions;
- fontconfig: DejaVu, slight hinting, greyscale AA;
- AT-SPI bus on (`services.gnome.at-spi2-core`), Qt apps join it;
- `panda-dnd on|off|toggle` (Super+N, the menu, a waybar badge) pauses
  notifications; `panda-cu-busy run CMD` keeps the idle lock away while a
  task runs; `pandaos.desktop.autoLock = false` turns the idle lock off.

## Tests

- `checks.panda-cu` (`components/computer-use/vmtest.nix`): one MCP stdio
  session as `agent` in a pandacode-host-like sandbox. It checks screenshot
  size and latency, writes and saves a file in Mousepad through AT-SPI and
  the keyboard, Set-of-Marks, click accuracy on a grid of 80 GTK buttons (by
  `element_id` and by coordinates), the clipboard, refused inputs, and that
  another user cannot reach the session. METRICs: `screenshot_ms_median`,
  `click_ms_median`, `move_with_screenshot_ms`, `click_accuracy`.
- `checks.boot-desktop`: `test-config.sh` on `/etc/xdg`, the AT-SPI bus
  address, Mousepad and PCManFM launched through `panda-cu` (attach),
  Mousepad's text node, and do-not-disturb.
- CI: `go vet`/`go test -race`, the desktop shell tests, shellcheck.

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
