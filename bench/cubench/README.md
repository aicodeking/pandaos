# bench/cubench: cross-OS computer-use benchmark

Measures how often an AI agent completes desktop tasks on each OS. The same 25
tasks, checkers, model and tool interface run on every OS. Each OS runs under
QEMU/KVM with identical resources: UEFI, 4 vCPU, 4 GB, virtio-vga fixed at
1280x800, a usb-tablet and a QMP socket.

- **Agent side (identical everywhere).** `qmp.py` drives the VM from outside:
  `screendump` (PNG) for screenshots, `input-send-event` for absolute mouse
  and wheel, and `send-key` for keys and typing (printable ASCII to US-layout
  qcodes, with shift).
  The generic tools are `screenshot`, `click`, `double_click`, `drag`,
  `scroll`, `type`, `key`, `wait` and `done`. Every action returns a fresh
  screenshot.
- **Harness side (never seen by the agent).** Setup and verification run over
  ssh inside the graphical session. The session env comes from the session
  process (or from `systemctl --user show-environment` on GNOME). Setup helpers:
  `launch ROLE [args]` (ROLE is editor, files, terminal or browser, mapped per
  OS in `os/<name>.json`), `serve_form` (`tasks/assets/formserver.py` on
  127.0.0.1:8000, logs each POST to `$B/form.log`), and `$B` = `~/bench`.
  Before each attempt, the OS's `reset` lines close the apps and wipe the
  Firefox/editor state and `~/bench`.

## Files

| File | Purpose |
|---|---|
| `cubench.py` | CLI: `prepare` (build a desktop disk), `run` (also `--smoke`, `--dry-run`), `report` |
| `agent.py` | agent loop, providers (Anthropic, OpenAI-compatible, scripted), tool specs, cost |
| `qmp.py` | QMP client, key mapping, fake QMP server |
| `score.py` | Wilson intervals, fail reasons, summary and markdown |
| `tasks/*.json` | `{id, title, instruction, setup: [sh], verify: sh, timeout_s, max_steps, tools_exclude?}` |
| `tasks/assets/` | form server, static pages and `probecheck.py`, uploaded to `~/.cubench` in the VM |
| `smoke/*.json` | plumbing tasks for `run --smoke` (an optional `script` replaces the scripted agent's default actions) |
| `os/<name>.json` | image pin, user, session env source, app commands, agent hints, reset |
| `os/common.sh`, `os/<name>.prepare.sh` | desktop install, run as root over ssh by `prepare` |
| `pandaos-desktop-image.nix` | PandaOS desktop qcow2 (how it differs from the shipped desktop: see its header) |
| `test_cubench.py` | self-tests (no VM, no API) |

## OS matrix

| Profile | Base | Desktop / session | Apps (editor, files, terminal, browser) | Status |
|---|---|---|---|---|
| `pandaos-desktop` | flake desktop profile, built by nix | labwc, Wayland, greetd autologin | Mousepad, PCManFM, foot, Firefox | default |
| `ubuntu-24.04-gnome` | Ubuntu 24.04 cloud image 20261017 | `ubuntu-desktop-minimal`, GNOME on **Wayland**, gdm autologin | Text Editor, Files, GNOME Terminal, Firefox (Mozilla .deb) | default |
| `debian-13-xfce` | Debian 13 *generic* cloud image 20261026 (full kernel, so virtio-gpu DRM is present) | minimal Xfce on **X11**, lightdm autologin | Mousepad, Thunar, Xfce Terminal, Firefox ESR | default |
| `fedora-44-workstation` | Fedora 44 Cloud Base 1.7 + `@workstation-product-environment` | GNOME on Wayland, gdm autologin | Text Editor, Files, Ptyxis, Firefox | opt-in: the install is ~2 GB and takes about 45-60 min the first time |

PandaOS uses the editor and file manager its desktop ships (Mousepad,
PCManFM). The bench image adds Firefox, the browser every other column uses,
so the comparison is about shells and compositors, not app choices.

Every OS gets the same small set of changes: no idle lock or blanking, no
first-run tours, and the same Firefox policies (no welcome page, no
crash-restore prompt). Each result records the session type and display size
that were actually used. The real-VM path is checked by smoke mode (see Status).

## Tool modes and the panda-cu contract

`--tools generic` is the default and the only mode for non-PandaOS columns.
`--tools panda` adds PandaOS's in-OS tools: `cu_tree`, `cu_find`, `cu_act`,
`cu_screenshot`, `cu_wait`, `cu_launch`, `cu_clipboard_get` and
`cu_clipboard_set`. Each call runs over ssh inside the session as:

```sh
panda-cu call <tool> '<json-args>'
```

It must print one JSON value on stdout and exit 0, or exit non-zero with the
error on stderr (the error goes back to the model). If the result has a
`"png_base64"` field, or a `"path"` to a `.png` in the guest, cubench forwards
that image to the model. This matches `components/computer-use` (`panda-cu
call`). The desktop profile ships `services.panda-cu` in `mode = "attach"`,
so the image's autologin session has it; the image only sets Firefox as its
browser.

## Scoring

- **Pass rate** per OS and tool mode, with a 95% Wilson interval.
- **Median steps and seconds** per attempt.
- **Fail reasons:**
  - `crash`: setup failed, the API failed or refused, or the VM died.
  - `misclick`: at least 3 clicks, and at least half of them left the screen
    pixel-identical (a heuristic).
  - `timeout`: the attempt ran out of `max_steps` or `timeout_s`.
  - `wrong_result`: the agent finished but the checker failed.
- A checker exit code of 0 is the only definition of a pass. What the agent
  claims does not count.

Defaults: 3 repeats per task, task order shuffled with `--seed`. Outputs:

- `results-<os>-<tools>[-rK].json` per run;
- `report` merges them into `results/<date>.json` and `results/<date>.md`;
- transcripts go to `transcripts/<os>-<tools>/<task>-rK/`
  (`transcript.jsonl` plus every screenshot).

Providers:

- **Anthropic** (raw HTTP, default `claude-sonnet-5-5`): custom tools with
  image blocks in tool results, not the computer-use tool, so every provider
  sees the same tools. History is append-only and auto-cached.
- **OpenAI-compatible** (`OPENAI_BASE_URL`, `OPENAI_API_KEY`,
  `OPENAI_MODEL`; e.g. Z.AI GLM-V): keeps the last 3 screenshots and sends
  `seed`.
- Server-side model fallbacks are deliberately off: one model per run.
- For models not in `agent.PRICES`, pass `--price in,out` (USD per million
  tokens) to get cost figures.

## Smoke mode (no API key)

`run --smoke` proves the VM plumbing before any money is spent on a model.
The scripted agent runs `smoke/input-probe`: setup starts the form server and
a kiosk browser on `probe.html`, which logs every mouse down, wheel event and
keystroke; the agent clicks (640,400) and (900,600), scrolls and types
`cubench probe 42`; `probecheck.py` passes only if both clicks arrived at the
same offset from their targets (within 2 px, so 1:1), the wheel moved and the
text arrived. The `--tasks` given also run, so their setup and checkers are
exercised over ssh (the scripted agent fails them, which is fine). The exit
code is non-zero if the QMP display is not 1280x800, a setup crashed, a smoke
task failed, or, with `--tools panda`, `panda-cu call cu_windows` /
`cu_screenshot` failed in the session. Results carry `smokeFailures`.

## Running

```sh
python3 bench/cubench/test_cubench.py                     # self-tests
python3 bench/cubench/cubench.py run --dry-run --os pandaos-desktop --repeats 1 --out /tmp/cu
ssh-keygen -t ed25519 -N '' -f key
python3 bench/cubench/cubench.py prepare --os debian-13-xfce --key key
python3 bench/cubench/cubench.py run --smoke --os debian-13-xfce --key key --repeats 1 \
  --tasks editor-new-file,browser-form --out smoke
ANTHROPIC_API_KEY=... python3 bench/cubench/cubench.py run --os debian-13-xfce --key key --out out
python3 bench/cubench/cubench.py report out/results-*.json --out bench/cubench/results/$(date -u +%F)
```

The host needs `qemu-system-x86_64`, `qemu-img` (7.1 or later, for PNG
screendump), OVMF, `cloud-localds` and `ssh`. It also reuses
`bench/compare/run.py` for image download, checksums and the base VM.

## CI: `.github/workflows/cubench.yml` (manual)

**Inputs:**

- `provider`: `anthropic` or `openai`
- `model`
- `repeats`
- `os`: comma-separated profiles
- `tasks`: a glob over task ids
- `panda_tools`: also run the PandaOS panda column

**Secrets:**

- `ANTHROPIC_API_KEY`, or `OPENAI_API_KEY` plus the repo variables
  `OPENAI_BASE_URL` and `OPENAI_MODEL`;
- optional: `CACHIX_AUTH_TOKEN` and the variable `CACHIX_CACHE`, as in
  compare.yml.

Without the provider's key the workflow runs in smoke mode: `selftest`, then
`prepare` and one `run` job per OS (plus the PandaOS panda column) with
`run --smoke --tasks <smoke_tasks>`. Artifacts: `cubench-smoke-<os>-<tools>-r1`
(results, transcripts and `<os>-<tools>-desktop.png`, the first screenshot).

**Jobs:**

- `prepare` builds each cloud desktop disk once and caches it, keyed by the OS
  profile, its prepare script and `common.sh`. The image hash is in the
  profile.
- `run` runs one job per (OS, tool mode, repeat), each in a fresh VM.
- `report` merges the results.

Cache budget: about 2 to 3 GB per zstd-compressed disk, against the
repository's 10 GB Actions cache limit.

**Estimates for a full default run** (3 OSes plus the panda column, 25 tasks, 3
repeats, so 12 run jobs and 300 attempts):

- **CI minutes:** about 12 × 45-75 min for the runs. The first run also takes
  about 60 min to prepare disks; cached runs take about 5 min to restore them.
  The PandaOS image is rebuilt for each job, about 10 min each with a warm
  cache. Total: roughly 700-1000 runner-minutes, and about 1-1.5 h of wall
  time at `max-parallel: 8`.
- **API cost with claude-sonnet-5-5:** about $0.10-0.60 per attempt
  (12-40 steps, a screenshot of about 1.4k tokens per step, cached prefix), so
  about **$40-100 per full run**. This estimate has not been measured yet: the
  report shows the measured cost per column.

## Status

- Smoke mode is green on every default OS (cubench runs 41184240650 and
  41201988784, no API key). The prepared disks took 105 s (Debian 13 Xfce) and 192 s (Ubuntu
  24.04 GNOME) of install script, and about 4-6 min per prepare job, so
  preparing fits easily in CI. Each OS booted to its desktop at 1280x800
  (Debian on X11, Ubuntu and PandaOS on Wayland). Both QMP clicks reached the
  kiosk page at offset (0, 0), the wheel and the typed text arrived, setup and
  checkers ran over ssh, and `panda-cu call` worked in the PandaOS session.
  Smoke jobs took 3-5 min on the cloud images and 12-16 min on PandaOS, most
  of it building the image.
- No model has been run yet: that needs `ANTHROPIC_API_KEY` (or an
  OpenAI-compatible key).
- Still unverified: whether `/etc/firefox/policies` is honoured by Debian's
  firefox-esr (it is also written to `/etc/firefox-esr/policies`).
