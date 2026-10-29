# Agents

A PandaOS box is an agent box through **PandaCode Connect**: `pandacode-host` runs PandaCode's
headless host, so the PandaCode app on your other devices sees the box as one more computer. `panda-mem`
keeps shared memory, and the optional `panda-browser` gives agents a headless browser. The older
`panda-agentd` job runner is **optional and off by default** (new in Unreleased).

## Agent runtime (new in Unreleased)

`pandaos.agents` (on by default, `components/agent-env`) is where agent CLIs live:

- **`agents.slice`.** `pandacode-host` and every agent CLI it starts run in this slice. Its limits are
  `MemoryHigh=75%` (throttling), `MemoryMax=85%` (the kernel OOM-kills inside the slice),
  `MemorySwapMax=25%`, `CPUWeight=50` (system units have 100, so agents yield under load) and
  `TasksMax=8192`. **systemd-oomd** kills inside the slice when memory pressure stays above 50% or
  swap is over 90% full. `pandacode-host` has `OOMPolicy=continue`, so one killed agent does not stop
  the host. Each limit is an option, for example `pandaos.agents.memoryMax = "70%";`. See
  [security.md](security.md#agent-limits-and-oom).
- **`/etc/panda/AGENTS.md`**: a short brief for agents (the box, its tools, its limits, its rules).
  `panda-agents sync` links it to `~/AGENTS.md`, and to `~/.codex/AGENTS.md` when Codex is installed.

### MCP registry

Every MCP server on the box has a file in `/etc/panda/mcp.d/<name>.json`:

```json
{"command": "panda-ckpt", "args": ["mcp"]}
```

Agent CLIs see each one as `panda-<name>`. On a default box these are `panda-mem`, `panda-ckpt`
and `panda-sys`, plus `panda-browser` if `services.panda-browser.mcp` is set. Modules register
servers with `pandaos.agents.mcpServers.<name> = { command; args; env; }`. `env` is world-readable,
so do not put secrets in it. `/etc/panda/mcp.json` is still generated, from the same registry
(`{"mcpServers": {"panda-<name>": ...}}`), for clients that read the old file.

### panda-agents

```sh
panda-agents mcp list             # the registry
panda-agents mcp sync [--user U]  # merge it into each installed agent CLI's config
panda-agents sync [--user U]      # mcp sync, plus link AGENTS.md
panda-agents doctor [--user U]    # check the registry, CLI configs, /mem socket, checkpoints, AGENTS.md
```

`sync` knows Claude Code (`~/.claude.json`), Codex (`~/.codex/config.toml`), opencode, Gemini CLI
and Kimi CLI. It writes only `panda-*` entries. Your own servers and settings stay as they are, and a
second run changes nothing. Run as root with `--user`, it drops to that user first.
`panda-agents-sync.service` runs `sync` at boot and again after a rebuild changes the registry or the
brief. If you install a CLI later, run `panda-agents sync` (`doctor` reminds you). In the desktop,
use `panda-menu` → **Agents doctor**.

### panda-sys: box status for agents

`services.panda-sys` (on by default) is an MCP server that agents use to inspect the box:

| Tool | Returns |
|---|---|
| `sys_status` | hostname, uptime, load, CPUs, memory, disk free, PandaOS version, battery |
| `sys_services` | state of the PandaOS units, and `systemctl --failed` |
| `sys_logs` | `journalctl` (`unit`, `since`, `lines` 1..500, `grep`) |
| `sys_top` | top processes by CPU and by memory |
| `sys_net` | interfaces, default route, DNS, and a 5 s HEAD request to `connectivityUrl` |
| `sys_ports` | listening sockets |
| `sys_updates` | `panda-update status`. `action=run` is the only tool that changes anything, and it is gated |
| `sys_ckpt_space` | btrfs usage of `/` and `/.snapshots` |

Callers outside `wheel` can read only the PandaOS units' logs. For the permission model, see
[security.md](security.md#mcp-tools-and-their-permissions).

### Checkpoints

`panda-ckpt` (on by default) keeps instant btrfs checkpoints of `~/work/<name>` workspaces. Agents
use its MCP tools to checkpoint before risky changes, and to diff, restore or fork. See
[checkpoints.md](checkpoints.md).

## PandaCode Connect (pandacode-host)

`services.pandacode-host` (on by default) runs `pandacode serve` from PandaCode's pinned release
AppImage, under `xvfb-run` (PandaCode needs a display to start; it draws nothing). See PandaCode's
[headless docs](https://github.com/aicodeking/pandacode/blob/main/docs/headless.md).

Sign in once, with a network:

```sh
sudo panda-connect sign-in                    # or: --device-name build-box (default: the hostname)
```

It prints a GitHub device-flow code and link. Approve it with the GitHub account you use in
PandaCode. When PandaCode reports the box is hosting, `panda-connect` saves the sign-in and starts the
`pandacode-host` service. The box then shows up as a computer in the PandaCode app, where you can
start, watch and approve tasks on it. On the desktop, the dock's *PandaCode* button and
`panda-menu` → *PandaCode Connect* do the same. First boot also offers it, when a network is up.

| Command | What |
|---|---|
| `panda-connect status [--json]` | `signedIn`, `service` (`active`, `activating`, `inactive`, `failed`), `deviceName`, `version` |
| `sudo panda-connect full-access on\|off` | Let your devices pick Full access (no approval prompts). **Off by default.** |
| `panda-connect logs [-f]` | The host's journal |
| `sudo panda-connect sign-out` | Stop the host and forget the sign-in |

NixOS options:

```nix
services.pandacode-host = {
  enable = true;          # on by default; idle until signed in
  user = "panda";         # the host and every agent CLI it starts run as this user
  deviceName = "pandaos"; # default name for sign-in (default: networking.hostName)
  fullAccess = false;     # default for Full access; `panda-connect full-access` overrides it
};
```

Agents run as `user` with its home: install and log in the agent CLIs (`claude`, `codex`, ...) and
`gh` as that user. PandaCode keeps its sign-in and secrets in that user's PandaCode data directory
(files mode 0600, not a keyring).

Until you sign in, the service is skipped (`ConditionPathExists` on
`/var/lib/pandacode-host/signed-in`) and the health check ignores it, so an unsigned box is healthy
and updates normally. Once signed in, `pandacode-host.service` is one of the units `panda-health`
requires. If PandaCode reports it is no longer signed in (sign-in revoked elsewhere), the unit drops
the marker instead of restarting forever; run `panda-connect sign-in` again.

## panda-agentd (optional)

Off by default; PandaCode Connect replaced it. Enable it with `services.panda-agentd.enable = true`
(or `panda-install --relay URL`) if you run your own relay.

`panda-agentd` dials **out** to a relay. Nothing on the box listens for it (see [security.md](security.md)).
The relay protocol is a placeholder (HTTP/JSON) until PandaConnect is defined.

NixOS options:

```nix
services.panda-agentd = {
  enable = true;                              # off by default
  relayUrl = "https://relay.example.com";     # default is a placeholder: https://connect.pandacode.invalid
  allowInsecure = false;                      # true allows http://, to loopback, private, .local or single-label hosts only
  slots = 2;                                  # max jobs at once
  cpuQuota = "200%";                          # CPUQuota= per job (100% = one CPU)
};
```

The relay URL must be `https://`. A plain `http://` URL fails the build unless `allowInsecure = true`,
and even then the daemon only accepts it for local hosts.

`panda-install --relay URL` writes the URL to `/var/lib/panda-agentd/relay` (new in Unreleased). When
that file holds a URL, it overrides `relayUrl`, so you can change the relay without a rebuild: edit
the file and run `sudo systemctl restart panda-agentd`.

State lives in `/var/lib/panda-agentd`: the token (mode 0600, root only) and the job work directories
in `jobs/<id>`. Runtime files live in `/run/panda-agentd`. The daemon's flags (`-relay`,
`-relay-file`, `-allow-insecure`, `-slots`, `-cpu-quota`, `-min-free-mb`, `-state-dir`, `-run-dir`,
`-job-rw-path`) are set by the NixOS module. `panda-agentd -h` lists them. `-min-free-mb` (default
1024) makes the daemon refuse new jobs while the state directory has less free space than that.

**Updates do not restart the daemon** (`restartIfChanged = false`, new in Unreleased): job units are
bound to it, so a restart would kill running jobs. A new `panda-agentd` takes over on the next reboot
or `sudo systemctl restart panda-agentd`. If it does restart while jobs run, it reports those jobs to
the relay as interrupted.

### Pairing

On first start (no token yet) the daemon asks the relay for a pairing code and shows it:

```sh
journalctl -u panda-agentd | grep 'pairing code:'
sudo cat /run/panda-agentd/pairing-code   # 0640 root:wheel; removed once paired
panda-agentd status                       # shows the code if you are root or in wheel
```

Enter the code on the relay side. The daemon then stores its token in `/var/lib/panda-agentd/token`.
If the relay later rejects the token, the daemon deletes it and pairs again. To re-pair by hand, see
[troubleshooting.md](troubleshooting.md#re-pairing).

### Status

> **New in Unreleased.**

```sh
panda-agentd status            # human-readable
panda-agentd status -json      # the raw file
cat /run/panda-agentd/status.json
```

`status.json` is world-readable and is rewritten atomically, so it holds no secrets. Its fields:
`paired`, `pairing` (true while a pairing code waits for approval; the code itself is only in
`/run/panda-agentd/pairing-code`), `relay`, `slots`, `running`, `completed`, `failed`, `version`,
`since`, and `lastError` / `lastErrorAt` (the last failed relay call, empty once a call works again).
`panda-agents-status` turns the same status into a waybar module; it is not on the default bar since agentd is off by default.

### Jobs

A job from the relay is an argv plus limits:

```json
{"id": "echo-hi", "argv": ["echo", "hi"], "timeoutSec": 30, "memoryMaxMB": 64, "keepWorkdir": false}
```

The daemon runs it in a transient systemd unit, `panda-job-<id>.service`, as the `panda-agent` user.
The job's working directory and `HOME` is a fresh `/var/lib/panda-agentd/jobs/<id>`. It is deleted
afterwards unless `keepWorkdir` is set. Limits: `RuntimeMaxSec` from `timeoutSec` (default 1 hour),
`MemoryMax` from `memoryMaxMB` (default 80% of RAM), `CPUQuota` from `cpuQuota`, and `TasksMax=512`.
The daemon sends back `exitCode`, `stdout` and `stderr` (each capped at 64 KiB). A job that times out
gets exit code 124. At most `slots` jobs run at once. The sandbox is described in
[security.md](security.md#what-jobs-can-and-cannot-touch).

The daemon sends a heartbeat to the relay every 30 s (every 5 s while jobs run) with hostname, CPUs,
total and available memory, and free slots.

### Cancelling a job

The relay cancels a job by listing its id in a heartbeat reply. The relay side of this is
`POST /v1/jobs/<id>/cancel`. The daemon stops the job's unit and reports it with `"cancelled": true`
and exit code 130. On shutdown, running jobs get 30 s to finish. Then they are cancelled and reported
the same way.

## panda-mem: shared memory at /mem

`/mem` is a git repo of markdown files, shared by every agent on the box. Each write is a git commit,
so every change has history. Memories have a kind: `episodic`, `semantic` or `procedural`.

> **Changed in Unreleased:** `panda-mem.service` is now the **single writer**. It runs as the
> `panda-mem` system user, owns `/mem` (mode 0750), and serves clients on the Unix socket
> `/run/panda-mem/sock`. Members of the `panda-mem` group (your login user, the job user
> `panda-agent`, and anyone in `services.panda-mem.clients`) can use the socket and read `/mem`, but
> cannot write the repo directly. See [security.md](security.md#mem-and-git) for why.

The CLI talks to the socket when it exists:

```sh
panda-mem write -kind semantic -title "build host" -body "Use the shed box for Nix builds"
panda-mem search "build host"            # multi-term: ranked by how many terms match, then by matches
panda-mem list
panda-mem read semantic/<file>.md
panda-mem history
```

Search splits the query into terms. A memory matches if it contains any term; memories that match
more distinct terms rank first, then those with more occurrences.

### MCP

`panda-mem mcp` is an MCP server on stdio that forwards to the socket. It is registered in the
[MCP registry](#mcp-registry) as `/etc/panda/mcp.d/mem.json`, which agent CLIs see as `panda-mem`. The
compat `/etc/panda/mcp.json` still carries it:

```json
{"mcpServers": {"panda-mem": {"command": "panda-mem", "args": ["mcp", "-socket", "/run/panda-mem/sock"]}}}
```

Tools: `mem_write`, `mem_search`, `mem_read`, `mem_list`, `mem_history`.

`/mem` is its own btrfs subvolume and is snapshotted hourly (new in Unreleased), see
[updates-and-rollback.md](updates-and-rollback.md#snapshots-of-mem-and-home).

## panda-browser (optional)

> **New in Unreleased.** Off by default.

```nix
services.panda-browser.enable = true;
```

Runs headless Chromium with the Chrome DevTools Protocol on `127.0.0.1:9222`. It listens on
localhost only, so only processes on the box (agents, jobs) can drive it. Jobs are allowed to reach
localhost on purpose, for this. Point a CDP or Playwright
client at `http://127.0.0.1:9222`. To use it from another machine, forward the port over ssh:

```sh
ssh -L 9222:127.0.0.1:9222 you@box
```

It runs as `panda-browser.service` with a fresh profile on every start, and writes the endpoint to
`/etc/panda/browser.json` (`{"cdp":"http://127.0.0.1:9222"}`). CDP has no authentication, so every
local user and process can drive it. It costs extra RAM and disk, so it is not in the measured
numbers in the README. Details: [components/browser/README.md](../components/browser/README.md).

No browser MCP server ships. If you add one, set `services.panda-browser.mcp` and the browser registers
it in `mcp.d` as `panda-browser`:

```nix
services.panda-browser.mcp = { command = "chrome-devtools-mcp"; args = [ "--browserUrl" "http://127.0.0.1:9222" ]; };
```
