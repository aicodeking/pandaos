# Security model

## Threat model

PandaOS assumes a **single-tenant box**: one owner, on their own hardware, running agent tasks that
the owner started from PandaCode (or, with the optional `panda-agentd`, jobs their relay sent). It is
not a multi-user or hostile multi-tenant system.

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
- A compromised GitHub account of the owner. Whoever controls it can sign a device in to the
  owner's PandaCode Connect and start tasks on the box (see [PandaCode Connect](#pandacode-connect)).
- A malicious or compromised `panda-agentd` relay (optional, off by default). The relay decides which jobs run, so it can run any command the
  job user is allowed to run. Pair only with a relay you trust.
- Anything you run yourself as your login user, which is in `wheel` and can use `sudo`.
- A compromised GitHub account or release pipeline. Boxes trust whatever lands on the `release`
  branch of `github:AICodeKing/pandaos`. Releases carry build provenance, but boxes do not check it.
- Secure Boot. It must be off; PandaOS's boot files are not signed.

## Network exposure

| Direction | What |
|---|---|
| Inbound | nftables firewall on. Only ssh (22) is open. ssh: keys only, no passwords, no root login, `MaxAuthTries 3`, and `sshguard` blocks repeat offenders. |
| Outbound | `pandacode-host` (once signed in) dials out to PandaCode Connect; the optional `panda-agentd` dials out to its relay. Nothing has to be opened or forwarded for agents. |
| Local only | `panda-browser`, if enabled, serves CDP on `127.0.0.1:9222` only. |

> v0.1.0 already had the firewall on with only ssh open and ssh key-only, but it shipped the user
> `panda` with the password `panda`. Setting credentials at install time is **new in Unreleased**.

## PandaCode Connect

**Trust model: the owner's GitHub account.** `panda-connect sign-in` runs PandaCode's GitHub device
flow (identity only, no repository scopes). The box joins the PandaCode Connect account of whoever
approves it, and every device signed in to that account can see the box and start, steer and approve
tasks on it. Only approve the code with your own account, and sign out (`panda-connect sign-out`)
before handing the box on. PandaCode keeps the GitHub token only long enough to sign in.

**Encryption between devices.** Each device has its own X25519 key; PandaCode encrypts the frames
between your devices, and the Connect relay forwards them and stores no task content (see PandaCode's
[remote-access docs](https://github.com/aicodeking/pandacode/blob/main/docs/remote-access.md)). The
connection is outbound only: nothing listens on the box for it. The relay token and device key live
in the `panda` user's PandaCode data directory, in files mode 0600.

**Full access is off by default.** Remote tasks need your approval for each action that asks for it.
`sudo panda-connect full-access on` (or `services.pandacode-host.fullAccess = true`) lets your devices
pick Full access, with no approval prompts; turn it back off with `full-access off`. Both need `sudo`.

**What agents can touch.** The host and the agent CLIs it starts run as `panda`, with that user's
home (checkouts, CLI logins, `~/.ssh`), the network, and `panda-mem` through its socket. The service
has `NoNewPrivileges`, so agents cannot use `sudo` even though `panda` is in `wheel`; the rest of the
system is read-only to them (`ProtectSystem=strict`, private `/tmp` and devices). This is weaker
isolation than `panda-agentd`'s job units: treat anything in `panda`'s home as reachable by your tasks.

## panda-agentd (optional)

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

## Agent limits and OOM

New in Unreleased (`pandaos.agents`, see [agents.md](agents.md#agent-runtime-new-in-unreleased)).
`pandacode-host` and every agent CLI it starts run in `agents.slice`:

| Limit | Default | Effect |
|---|---|---|
| `MemoryHigh` | 75% of RAM | the slice is throttled and reclaimed above this |
| `MemoryMax` | 85% of RAM | the kernel OOM-kills a process inside the slice, never outside it |
| `MemorySwapMax` | 25% of RAM | caps how much the slice can push into zram, which `MemoryMax` does not count |
| `CPUWeight` | 50 | system units (weight 100) win under CPU contention |
| `TasksMax` | 8192 | stops fork bombs |
| systemd-oomd | pressure > 50% for 30 s, or swap > 90% | kills the slice's cgroup with the most reclaim |

`pandacode-host` has `OOMPolicy=continue` (a killed agent does not stop the host) and
`OOMScoreAdjust=500`. sshd, journald, NetworkManager and panda-mem get `OOMScoreAdjust=-500`,
so the global OOM killer picks agents first. They do not get -1000, because ssh sessions inherit the
value. The `agent-env` VM test runs a 6 GB memory hog in the slice and checks that it is killed while
sshd keeps its PID. There is no earlyoom: it would watch the whole box and could kill a system daemon.

These limits protect the box from runaway agents. They do not isolate agents from your files:
agents run as `panda`, the login user.

## MCP tools and their permissions

Each MCP server runs as the agent that starts it (stdio), so it has that user's rights and no more.
None of them runs as root or has a setuid helper.

| Server | Can change | Guard |
|---|---|---|
| `panda-mem` | memories, through the panda-mem socket | the socket's group (`panda-mem`), and every change is a git commit |
| `panda-ckpt` | the caller's own `~/work` and `~/.checkpoints` | `ckpt_restore` needs `confirm: true` and takes a safety checkpoint first. The auto timer runs as the user, with no network and write access to that user's home only |
| `panda-sys` | nothing by default | `sys_updates action=run` needs `services.panda-sys.allowMutations = true` (in root-owned `/etc/panda/sys.json`), `confirm: true`, a root or `wheel` caller, and a passwordless `sudo -n panda-update run`. PandaOS does not grant that sudo rule, so add one if you want agents to update the box |

`panda-sys` read limits: callers outside `wheel` can read the journal only for an allow-list of
PandaOS units (pandacode-host, panda-mem, panda-browser, panda-ckpt-auto-panda, panda-agents-sync,
systemd-oomd, nixos-upgrade, sshd, NetworkManager). Unit names and `since` are validated, and `lines`
is 1..500. **`panda` is in `wheel`, so agents running as `panda` can read every journal.** Run agents
as a non-wheel user if they must not. `sys_net` sends one HEAD request to `connectivityUrl`, and
nothing else in these servers uses the network.

`confirm: true` protects against mistakes. It is not a security boundary, because an agent can always
send it. The brief in `/etc/panda/AGENTS.md` is guidance only, and it is not enforced.

`/etc/panda/mcp.d` is written by Nix and owned by root. `panda-agents sync` copies its entries into
the user's CLI configs, and it writes only `panda-*` keys there.

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
- Keep PandaCode Full access off unless you watch the tasks; protect the GitHub account you sign in
  with (two-factor authentication).
- If you enable `panda-agentd`: use an `https://` relay you control.
- Keep auto-updates on: security fixes only reach boxes through updates.
- Report vulnerabilities privately: [SECURITY.md](../SECURITY.md).
