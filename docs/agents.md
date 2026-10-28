# Agents

Three services make a PandaOS box an agent box: `panda-agentd` runs jobs, `panda-mem` keeps
shared memory, and the optional `panda-browser` gives agents a headless browser.

## panda-agentd

`panda-agentd` dials **out** to a relay. Nothing on the box listens for it (see [security.md](security.md)).
The relay protocol is a placeholder (HTTP/JSON) until PandaConnect is defined.

NixOS options:

```nix
services.panda-agentd = {
  enable = true;                              # on by default
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
The desktop dock (`panda-agents-status`) shows the same status, and the pairing code when it can read it.

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

`panda-mem mcp` is an MCP server on stdio that forwards to the socket. It is registered for MCP clients
in `/etc/panda/mcp.json`:

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
