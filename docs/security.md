# Security model

## Threat model

PandaOS assumes a **single-tenant box**: one owner, on their own hardware, running agent jobs that
the owner (through their relay) chose to send. It is not a multi-user or hostile multi-tenant system.

What we defend against:

- **The network.** Nothing listens except ssh, and ssh needs a key.
- **Default credentials.** There are none (new in Unreleased).
- **A misbehaving job** damaging the system, the agent daemon or other users' files, or using
  shared memory (`/mem`) to get code run as another user (see [below](#mem-and-git)).
- **A local, non-admin process** reading the pairing code and pairing the box with someone else's relay.
- **A bad update** leaving the box broken: the health check, boot assessment and rollback undo it, and
  updates only come from the tested `release` branch.

What we do not defend against:

- Someone with physical access. There is no disk encryption by default.
- A malicious or compromised relay. The relay decides which jobs run, so it can run any command the
  job user is allowed to run. Pair only with a relay you trust.
- Anything you run yourself as your login user, which is in `wheel` and can use `sudo`.
- A compromised GitHub account or release pipeline. Boxes trust whatever lands on the `release`
  branch of `github:AICodeKing/pandaos`. Releases carry build provenance, but boxes do not check it.
- Secure Boot. It must be off; PandaOS's boot files are not signed.

## Network exposure

| Direction | What |
|---|---|
| Inbound | nftables firewall on. Only ssh (22) is open. ssh: keys only, no passwords, no root login, `MaxAuthTries 3`, and `sshguard` blocks repeat offenders. |
| Outbound | `panda-agentd` dials out to the relay. Nothing has to be opened or forwarded for agents. |
| Local only | `panda-browser`, if enabled, serves CDP on `127.0.0.1:9222` only. |

> v0.1.0 already had the firewall on with only ssh open and ssh key-only, but it shipped the user
> `panda` with the password `panda`. Setting credentials at install time is **new in Unreleased**.

**The agent channel is outbound only.** `panda-agentd` makes HTTP(S) requests to the relay URL:
it pairs, sends heartbeats, long-polls for the next job and posts results. The relay never connects
to the box. The relay protocol is a **placeholder** until PandaConnect is defined, and the default
relay URL is a placeholder (`https://connect.pandacode.invalid`). Use an `https://` relay URL in production.

The relay URL must be `https://`. `allowInsecure` permits plain http only to loopback, private,
`.local` or single-label hosts, for tests and LAN development.

The pairing token is stored in `/var/lib/panda-agentd/token`, mode 0600 (root only). The directory
is 0711 so the job user can reach its own `jobs/<id>` work directory, but it cannot list the
directory or read the token.
If the relay rejects the token, the daemon throws it away and pairs again.

While pairing, the code is only in `/run/panda-agentd/pairing-code`, mode **0640 root:wheel**, and in
the daemon's journal. `status.json` is world-readable (the dock reads it), so it holds no secrets:
it says `"pairing": true` but no longer contains the code (it did in earlier Unreleased builds). Jobs
run as `panda-agent`, which is not in `wheel`, so they cannot read the code.

## What jobs can and cannot touch

The daemon (`panda-agentd`) runs as root under a systemd sandbox (read-only system, no access to
`/home`, private `/tmp` and devices, no kernel tuning). It runs as root only so it can start job units.

Each job runs in its **own transient systemd unit**, `panda-job-<id>.service`, as the unprivileged
system user `panda-agent`:

| Jobs can | Jobs cannot |
|---|---|
| run programs installed on the system | run as root or as your login user, or gain privileges (`NoNewPrivileges`, `RestrictSUIDSGID`) |
| use the network, including localhost (for example the browser's CDP port) | read the agent's pairing token or pairing code |
| write their own work directory `/var/lib/panda-agentd/jobs/<id>`, and memories through `panda-mem` | write `/mem` files directly (only the `panda-mem` service writes the repo), or anywhere else: the system is read-only to them (`ProtectSystem=strict`), `/home` is read-only (`ProtectHome=read-only`), and `/tmp` is private |
| see their own processes | see other users' processes (`ProtectProc=invisible`, `ProcSubset=pid`) or use devices (`PrivateDevices`) |
| make normal service system calls (`SystemCallFilter=@system-service`) | load kernel modules, change sysctls or cgroups, or read the kernel log |
| use memory, CPU, tasks and time up to their limits | go past `MemoryMax` (default 80% of RAM), `CPUQuota` (default 200%), `TasksMax=512` or `RuntimeMaxSec` (default 1 hour) |

Output kept per job is capped at 64 KiB for stdout and for stderr. Jobs are refused while the state
directory has less than 1 GiB free (`-min-free-mb`, default 1024), so jobs cannot fill the disk past
that point by starting new work.

Job units are bound to `panda-agentd` (`BindsTo=`). `panda-agentd` is **not restarted by updates**
(`restartIfChanged = false`), so a daily update does not kill running jobs; a new daemon takes over on
the next reboot or `systemctl restart panda-agentd`. If the daemon does restart, jobs whose units
outlived it are reported to the relay as interrupted.

### /mem and git

`/mem` is a git repo, and v0.1.0 and early Unreleased builds made it group-writable for every
`panda-mem` group member, including the job user. A job could then plant a git hook or a config entry
(`core.fsmonitor`, `core.pager`, a diff or filter driver) in `/mem/.git` that ran code as whoever next
ran git there, such as your login user or root. **New in Unreleased, this is closed in two layers:**

1. **Single writer.** `panda-mem.service` runs as its own `panda-mem` system user and is the only
   writer. `/mem`, including `.git`, belongs to that user with mode 0750, so group members can read it
   but not write it. Clients (the `panda-mem` CLI, the MCP server, agents and jobs) talk to the service
   over the Unix socket `/run/panda-mem/sock` (mode 0660, group `panda-mem`). The service itself is
   sandboxed: no network, no capabilities, read-only system except `/mem`.
2. **Hardened git.** The service runs git with hooks disabled (`core.hooksPath=/dev/null`), ignores
   system and global git config, and refuses to work on the repo if `.git/hooks` contains a hook or
   `.git/config` has a key it did not write. Memory files are opened so that a swapped-in symlink
   cannot redirect them, and a lock (`.git/panda-mem.lock`) serialises writes.

Members of the `panda-mem` group can still write any memory through the socket, so a job can change
or flood shared memory. Every change is a git commit, and `/mem` is snapshotted hourly.

## System hardening

- No default password. The installer sets yours. A system without one (for example, built
  directly from this flake) prompts for one on tty8 at boot (`panda-firstboot.service`).
- `sudo` only for `wheel`, always with a password (the live ISO is the exception: no password).
- Kernel sysctls: `kptr_restrict=2`, `dmesg_restrict=1`, no unprivileged eBPF, BPF JIT hardening,
  strict reverse-path filtering, no ICMP redirects or source routing, protected links, FIFOs and
  regular files in sticky directories.
- AppArmor is off. It ships no profiles for PandaOS's services, and it would add about 100 MB.

## Hardening checklist for owners

- Install with `--ssh-key`, or add a key to `~/.ssh/authorized_keys` on first login.
- Leave `panda-browser` off unless every local user and every job is trusted (its CDP port has no
  authentication, and jobs can reach localhost).
- Set an `https://` relay at install (`panda-install --relay URL`).
- Use an `https://` relay you control.
- Keep auto-updates on: security fixes only reach boxes through updates.
- Report vulnerabilities privately: [SECURITY.md](../SECURITY.md).
