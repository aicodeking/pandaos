# panda-cu: computer use for agents

`services.panda-cu.enable = true;` gives the agent user a desktop it can see
and drive over MCP (`/etc/panda/mcp.d/panda-cu.json` → `panda-cu mcp`).
Scripts run one tool with `panda-cu call TOOL '{json}'`: one JSON value on
stdout (images as `png_base64`), non-zero exit on error.

Design, safety defaults, tools, determinism, tests and limits:
[docs/computer-use.md](../../docs/computer-use.md).

Files:

| File | What |
|---|---|
| `main.go`, `mcp.go`, `toolsjson.go`, `tools.go` | CLI, MCP stdio, tool schemas, screenshots and pointer tools |
| `wayland.go`, `keys.go`, `toplevel.go` | Wayland wire protocol: virtual pointer, virtual keyboard, foreign-toplevel windows |
| `a11y.go`, `helper.go`, `atspi.py` | AT-SPI: the long-lived Python helper and the checks before a click by `element_id` |
| `launch.go`, `session.go` | launching apps (exit status, crashes) and the headless session helper |
| `guard.go` | attach mode: the activity badge and the Super+Escape stop |
| `module.nix`, `package.nix` | NixOS module, package |
| `vmtest.nix`, `scenario.py`, `buttons.py`, `lab.py`, `form.html` | `checks.panda-cu` |
