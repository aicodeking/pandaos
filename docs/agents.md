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
  slots = 2;                                  # max jobs at once
};
```

State lives in `/var/lib/panda-agentd` (root only); runtime files live in `/run/panda-agentd`.

### Pairing

On first start (no token yet) the daemon asks the relay for a pairing code and shows it:

```sh
journalctl -u panda-agentd | grep 'pairing code:'
cat /run/panda-agentd/pairing-code        # removed once paired
```

Enter the code on the relay side. The daemon then stores its token in `/var/lib/panda-agentd/token`.
If the relay later rejects the token, the daemon deletes it and pairs again. To re-pair by hand, see
[troubleshooting.md](troubleshooting.md#re-pairing).

### Status

> **New in Unreleased.**

```sh
panda-agentd status
cat /run/panda-agentd/status.json
```

The desktop dock shows the same agent status.

### Jobs

A job from the relay is an argv plus limits:

```json
{"id": "echo-hi", "argv": ["echo", "hi"], "timeoutSec": 30, "memoryMaxMB": 64}
```

The daemon runs it in a transient systemd unit as the `panda-agent` user, with `RuntimeMaxSec` set
from `timeoutSec` (default 1 hour) and `MemoryMax` from `memoryMaxMB` (when set), and sends back
`exitCode`, `stdout` and `stderr` (each capped at 64 KiB). A job that times out gets exit code 124.
At most `slots` jobs run at once. Jobs run sandboxed (new in Unreleased); see
[security.md](security.md#what-jobs-can-and-cannot-touch).

While working, the daemon sends a heartbeat to the relay every 30 s with hostname, CPUs, total and
available memory, and free slots.

## panda-mem: shared memory at /mem

`/mem` is a git repo of markdown files, shared by every agent on the box. Each write is a git commit,
so every change has history. Memories have a kind: `episodic`, `semantic` or `procedural`.

```sh
panda-mem write -dir /mem -kind semantic -title "build host" -body "Use the shed box for Nix builds"
panda-mem search -dir /mem "build host"
panda-mem list -dir /mem
panda-mem read -dir /mem semantic/<file>.md
panda-mem history -dir /mem
```

Members of the `panda-mem` group can write. The user you create at install is a member.

### MCP

`panda-mem mcp -dir /mem` is an MCP server on stdio. It is registered for MCP clients in
`/etc/panda/mcp.json`:

```json
{"mcpServers": {"panda-mem": {"command": "panda-mem", "args": ["mcp", "-dir", "/mem"]}}}
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
localhost only, so only processes on the box (agents, jobs) can drive it. Point a CDP or Playwright
client at `http://127.0.0.1:9222`. To use it from another machine, forward the port over ssh:

```sh
ssh -L 9222:127.0.0.1:9222 you@box
```

It costs extra RAM and disk, so it is not in the measured numbers in the README. Details: [components/browser/README.md](../components/browser/README.md).
