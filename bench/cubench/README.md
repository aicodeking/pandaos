# bench/cubench: cross-OS computer-use benchmark

Measures how often an AI agent completes desktop tasks on each OS. The same 35
tasks, checkers, model and tool interface run on every OS. Each OS runs under
QEMU/KVM with identical resources: UEFI, 4 vCPU, 4 GB, virtio-vga fixed at
1280x800, a usb-tablet and a QMP socket.

- **Agent side (identical everywhere).** `qmp.py` drives the VM from outside:
  `screendump` (PNG) for screenshots, `input-send-event` for absolute mouse
  and wheel, and for keys and typing (printable ASCII to US-layout qcodes,
  with shift; not `send-key`, whose timer queue stalls after `loadvm`).
  - Generic tools: `screenshot`, `click`, `double_click`, `drag`, `scroll`,
    `type`, `key`, `wait` and `done`.
  - The agent sees a screenshot at the start. After each turn in which it
    acted, the last tool result of that turn carries a fresh screenshot,
    whichever tools it used. Results of earlier calls in the same turn are
    text only, and read-only calls (`screenshot` excepted) add no image.
- **Isolation.** After boot, the harness snapshots the whole VM once (QMP
  `savevm`, with the OVMF vars on qcow2 so every writable drive can be
  snapshotted). Before every attempt, of the model and of validation alike, it
  restores that snapshot (`loadvm`): RAM, disk and the logged-in session are
  bit-identical. The guest clock is then set to the host's time.
- **Harness side (never seen by the agent).** Setup and checking run over ssh
  inside the graphical session (env from the session process, or from
  `systemctl --user show-environment` on GNOME). Setup helpers:
  - `launch ROLE [args]`: ROLE is editor, files, terminal or browser, mapped
    per OS in `os/<name>.json`;
  - `serve_form`: `tasks/assets/formserver.py` on 127.0.0.1:8000, which logs
    each POST to `$B/form.log` with the User-Agent class;
  - `audit_start` / `audit ROLE PATH...`: the fanotify writer audit (below);
  - `$B` is `~/bench`.

## Files

| File | Purpose |
|---|---|
| `cubench.py` | CLI: `prepare`, `run` (also `--smoke`, `--dry-run`), `validate`, `report` |
| `agent.py` | agent loop, providers (Anthropic, OpenAI-compatible, scripted), tool modes, cost, timing |
| `qmp.py` | QMP client (incl. `savevm`/`loadvm`), key mapping, fake QMP server |
| `score.py` | tool classes, task bootstrap, paired differences, Wilson, fail reasons, markdown |
| `tasks/*.json` | `{id, title, instruction, gui_only, setup: [sh], verify: sh, oracle: [steps], enforced?, timeout_s, max_steps, tools_exclude?}` |
| `tasks/assets/` | form server, pages, `audit.py` (fanotify), `cu_atspi.py` (AT-SPI tools), `tokencheck.py`, `probecheck.py`; uploaded to `~/.cubench` |
| `smoke/*.json` | plumbing tasks (`run --smoke`; also part of `validate`) |
| `os/<name>.json` | image pin, user, session, app commands, hints, writer process names, dconf keys, oracle keys |
| `os/common.sh`, `os/<name>.prepare.sh` | desktop install (incl. AT-SPI and dconf), run as root over ssh by `prepare` |
| `pandaos-desktop-image.nix` | PandaOS desktop qcow2 (its header lists the bench-only changes) |
| `test_cubench.py` | self-tests (no VM, no API) |

## OS matrix

| Profile | Base | Desktop / session | Apps (editor, files, terminal, browser) | Status |
|---|---|---|---|---|
| `pandaos-desktop` | flake desktop profile, built by nix | labwc, Wayland, greetd autologin | Mousepad, PCManFM, foot, Firefox | default |
| `ubuntu-24.04-gnome` | Ubuntu 24.04 cloud image 20261017 | `ubuntu-desktop-minimal`, GNOME on **Wayland**, gdm autologin | Text Editor, Files, GNOME Terminal, Firefox (Mozilla .deb) | default |
| `debian-13-xfce` | Debian 13 *generic* cloud image 20261026 | minimal Xfce on **X11**, lightdm autologin | Mousepad, Thunar, Xfce Terminal, Firefox ESR | default |
| `fedora-44-workstation` | Fedora 44 Cloud Base 1.7 + `@workstation-product-environment` | GNOME on Wayland, gdm autologin | Text Editor, Files, Ptyxis, Firefox | opt-in (a ~2 GB install) |

Every OS gets the same small set of changes: no idle lock or blanking, no
first-run tours, the same Firefox policies, AT-SPI and `GNOME_ACCESSIBILITY=1`
for launched apps, and passwordless sudo for the harness. Each result records
the OS, kernel, session type, display size and the desktop, editor, file
manager, terminal and browser versions the guest reports.

## Tool modes and like-for-like comparison

| Mode | Tools | Runs on | Class |
|---|---|---|---|
| `generic` | the QMP tools above | every OS | **generic**: the headline |
| `atspi` | generic + `cu_tree`, `cu_find`, `cu_act` with panda-cu's exact schemas, run by `tasks/assets/cu_atspi.py` on panda-cu's own AT-SPI walker (`panda-cu-atspi` on PandaOS, a copy of `components/computer-use/atspi.py` elsewhere) | every OS | a11y |
| `panda` | generic + every panda-cu tool, schemas loaded from `panda-cu tools` at run start (retried after each reset if that fails; the result records `pandaToolsSource`). Calls go through one `panda-cu mcp` session per attempt over ssh, because panda-cu's element ids live as long as its process | PandaOS (`services.panda-cu.mode = "attach"` in the bench image) | a11y |

The report has one table per class, and its paired differences never cross
classes. The like-for-like accessibility comparison is atspi against atspi. A
panda row only ever appears next to atspi rows, labelled as having more tools.
On keyboard-only tasks the mouse tools, `cu_click`, `cu_drag`, `cu_move`,
`cu_scroll` and `cu_act`'s click/press actions are removed, and cu_act
click/press calls are also refused at run time.

panda-cu contract: `panda-cu mcp` (MCP over stdio) for the panda column;
`panda-cu call <tool> '<json-args>'` (one JSON value, exit 0, or non-zero
with the error on stderr) for probes. Images come back to the model as image
blocks. The atspi column's `cu_atspi.py` keeps one panda-cu-atspi helper per
session too (a small relay on a unix socket), for the same reason.

## Tasks and how the method is enforced

35 tasks: 11 text editor and dialog, 6 file manager, 4 multi-step or
multi-app, 6 terminal or settings, and 8 browser (5 of them in kiosk mode, so
page pixels are screen pixels). A task with `gui_only: true` gets no terminal
pre-launched and no terminal hint. Its checker also verifies how the result
was made, where the title promises a method (each task's `enforced` field
says how):

- **Writer audit (`audit.py`).** A root fanotify watcher (filesystem mark,
  `FAN_REPORT_DFID_NAME`) logs the writing process of every create, rename
  and close-after-write under `$HOME` to `/var/tmp/cubench-audit.log`, where
  the agent cannot write. The checker requires the last write to each result
  path to come from the role's process (`procs` in the OS profile, e.g.
  `^mousepad$`), so a terminal command (`bash`, `mv`, `cp`, `python3`, ...)
  cannot produce a passing result. This covers editor, dialog and file
  manager tasks.
- **Drag and drop.**
  - `browser-drag-to-basket` passes only on the page's trusted HTML5 `drop`
    event (exactly one, of the banana).
  - `files-drag-between-windows` requires the file manager process to have
    made the move. A cut-and-paste inside the file manager also passes: the
    checker cannot tell it from a drag, and its `enforced` field says so.
- **Clipboard.** `clipboard-browser-to-editor` uses a token that exists only
  in the form server's memory (the page is served no-store, and the log
  holds only its sha256). The page must report a `copy` event from Firefox,
  and the editor must write the file. `multi-app-editor-to-browser` requires
  a `paste` event in the field.
- **Browser forms.** The server tags every POST with `ua=firefox` or
  `ua=other`, so a `curl` from a terminal fails.
- **Outcome only.** `editor-preferences-line-numbers` (the editor's dconf
  key) and `settings-browser-homepage` (Firefox's `prefs.js`) check only the
  result. Their `enforced` field says so.

## Validation (`cubench.py validate`)

Every checker is proven on the real VMs, from the same snapshot, for every
task (plus the smoke input probe):

- **(a) Null agent.** Setup only, then the checker, which must FAIL.
- **(b) Oracle.** The task's `oracle` steps, then the checker, which must
  PASS. Oracle steps:
  - `sh`: over ssh, only where the checker does not enforce a method;
  - `key`/`type`: QMP keys, e.g. the editors' ctrl+s and the file managers'
    ctrl+a / F2 / ctrl+shift+n / ctrl+l;
  - `click`/`rclick`/`drag`: QMP at fixed kiosk coordinates;
  - `a11y`: `cu_atspi.py do` (find an element, then act on it).

  Placeholders `{HOME}`, `{HOSTNAME}` and the OS profile's `keys` (e.g.
  `{switch_window}`) are filled in per OS. A task can override its steps per
  OS with `{"default": [...], "<os>": [...]}`. Because the oracles of the
  method-enforced tasks drive the real apps, they pass the writer audit.

Each validate job also checks that `cu_atspi.py` finds the editor's window
(the atspi mode works on that OS) and, on PandaOS, that `panda-cu call`
works. It saves a screenshot after every oracle step. It exits non-zero
unless the null fail-rate and the oracle pass-rate are both 100%.

## Scoring

- **Task pass rate.** Repeats are first averaged per task. The mean over
  tasks gets a 95% bootstrap interval over tasks. The attempt-level pass rate
  with a Wilson interval is also shown.
- **Paired per-task differences** between OSes of the same class: mean
  difference, a 95% bootstrap interval over tasks, and the count of tasks
  where A is better, tied or worse.
- **Timing.**
  - API time (waiting for the model) and environment time (everything else)
    are measured separately; medians of both are reported.
  - `timeout_s` budgets environment time only, so a slow API cannot fail a
    task. `max_steps` caps steps.
- **Fail reasons.**
  - `crash`: snapshot restore or setup failed, the API failed or refused, or
    the VM died.
  - `misclick`: at least 3 clicks, and at least half of them left the screen
    pixel-identical (a heuristic).
  - `timeout`: the attempt ran out of steps or environment time.
  - `wrong_result`: the checker failed.
- **What counts as a pass.** Only the checker's exit code decides it. What
  the agent claims does not count.

Defaults: 3 repeats, with task order shuffled by `--seed`. The outputs are
`results-<os>-<tools>[-rK].json`, `validate-<os>.json`, and transcripts
(`transcript.jsonl` plus every screenshot). `report` merges them into
`results/<date>.json` and `.md`.

**Providers:**

- **Anthropic** (raw HTTP, default `claude-sonnet-5-5`): custom tools with
  image blocks, so every provider sees the same tools. History is append-only
  and auto-cached.
- **OpenAI-compatible** (`OPENAI_BASE_URL`, `OPENAI_API_KEY`,
  `OPENAI_MODEL`): keeps the last 3 screenshots and sends `seed`.
- Model fallbacks are off.
- For other models, pass `--price in,out` to get cost figures.

## Running

```sh
python3 bench/cubench/test_cubench.py
ssh-keygen -t ed25519 -N '' -f key
python3 bench/cubench/cubench.py prepare --os debian-13-xfce --key key
python3 bench/cubench/cubench.py validate --os debian-13-xfce --key key --out val
ANTHROPIC_API_KEY=... python3 bench/cubench/cubench.py run --os debian-13-xfce --tools generic --key key --out out
python3 bench/cubench/cubench.py report out/results-*.json val/validate-*.json --out bench/cubench/results/$(date -u +%F)
```

The host needs `qemu-system-x86_64` and `qemu-img` (7.1 or later), OVMF,
`cloud-localds` and `ssh`. cubench reuses `bench/compare/run.py`.

## CI: `.github/workflows/cubench.yml` (manual)

**Inputs:**

- `provider`
- `model`
- `repeats`
- `os`
- `tasks`
- `mode`:
  - `auto` runs the model when the key secret exists, and validates
    otherwise;
  - `validate`;
  - `run`.
- `atspi_tools`: also run the atspi column on every OS.
- `panda_tools`: also run the PandaOS panda column.

**Secrets:** `ANTHROPIC_API_KEY`, or `OPENAI_API_KEY` with the variables
`OPENAI_BASE_URL` and `OPENAI_MODEL`. `CACHIX_AUTH_TOKEN` is optional.

**Jobs:**

- `selftest`: unit tests and dry runs.
- `prepare`: builds each cloud disk once, cached.
- `run`:
  - in run mode, one job per (OS, tool mode, repeat);
  - in validate mode, one job per OS.
- `report`

**Cost of a full default run** (3 OSes × generic and atspi, plus the PandaOS
panda column; 35 tasks; 3 repeats; that is 21 jobs and 735 attempts):

- **CI time:** about 21 × 60-90 min of runner time.
- **API cost:** at an estimated $0.10-0.60 per attempt with claude-sonnet-5-5,
  roughly $75-440. Running `atspi_tools=false` cuts that to 420 attempts.
- These are estimates. The report shows the measured cost per column.

## Status

See the validation table in the latest `cubench` workflow run (validate
mode). No model run yet: that needs an API key secret.
