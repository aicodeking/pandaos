# Security model

## Threat model

PandaOS's `headless` and `desktop` profiles assume a **single-tenant box**: one owner, on their own
hardware, running agent tasks that the owner started from PandaCode (or, with the optional
`panda-agentd`, jobs their relay sent). They are not multi-user or hostile multi-tenant profiles. For
dedicated servers hosting isolated Firecracker microVMs across teams (`nixos/profiles/server.nix`,
`services.boat`), see [Multi-tenant server mode (`services.boat.untrustedTenants`)](#multi-tenant-server-mode-servicesboatuntrustedtenants)
and [`boat.md`](boat.md#security-model).

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
| Outbound | `pandacode-host` (once signed in) dials out to PandaCode Connect; `t3code-host` (once signed in to T3 Code Connect) dials out to the T3 Connect relay through `cloudflared`; the optional `panda-agentd` dials out to its relay. Nothing has to be opened or forwarded for agents. |
| Local only | `panda-browser`, if enabled, serves CDP on `127.0.0.1:9222` only. `t3code-host`, once started, listens on `127.0.0.1:3773` only (`lan` and `openFirewall` are off by default); reach it through Connect, an SSH tunnel or Tailscale Serve ([t3code.md](t3code.md#security-model)). |

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
in the `agent` user's PandaCode data directory, in files mode 0600.

**Full access is off by default.** Remote tasks need your approval for each action that asks for it.
`sudo panda-connect full-access on` (or `services.pandacode-host.fullAccess = true`) lets your devices
pick Full access, with no approval prompts; turn it back off with `full-access off`. Both need `sudo`.

**Agent user separation (new in Unreleased).** The host and the agent CLIs it starts run as the system
user `agent`: home `/var/lib/agent` (mode 0700), not in `wheel`, no password, no ssh keys. Before
this, they ran as `panda`, the owner's login and `wheel` user. A task could then have written
`~/.bashrc` (for example a fake `sudo` that captures the password), `~/.ssh` or the owner's git
config, and the owner would run it at the next login. Now agents cannot read or write `/home/panda`
(mode 0700), and they cannot use `sudo` at all: the service has `NoNewPrivileges`, and `agent` is not
in `wheel`.

**What agents can touch.** The agent home (checkouts, CLI logins, PandaCode's data, `~/.ssh` of
`agent`), the network, `panda-mem` through its socket, and anything world-readable. The rest of the
system is read-only to them (`ProtectSystem=strict`, private `/tmp` and devices). Treat everything in
`/var/lib/agent` as reachable by every task, including the tokens of the agent CLIs logged in there.
The `agent-env` VM test checks that `agent` cannot list `/home/panda` or touch `/home/panda/.bashrc`.

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

**T3 Code shares the `agent` user.** `t3code-host` runs as the same `agent` user, with the same
sandbox (`NoNewPrivileges`, not in `wheel`). T3 Code sessions, PandaCode tasks, the agent CLI logins
and T3 Code's credentials in `/var/lib/agent/.t3` are one trust boundary: a prompt-injected task in
either host can run `t3` or read `/var/lib/agent/.t3`, but neither can `sudo` or touch the owner's
home. The unit redacts pairing tokens from journald, which keeps them from journal readers such as
`panda-sys`, not from processes running as `agent`. The box-wide sysctl `dev.tty.legacy_tiocsti = 0`
stops a process running as `agent` from pushing keystrokes into a terminal it shares (for example
after `sudo -iu agent claude`). See [t3code.md](t3code.md#security-model).

Members of the `panda-mem` group can still write any memory through the socket, so a job can change
or flood shared memory. Every change is a git commit, and `/mem` is snapshotted hourly.

## Agent limits and OOM

New in Unreleased (`pandaos.agents`, see [agents.md](agents.md#agent-runtime-new-in-unreleased)).
`pandacode-host`, `t3code-host` and every agent CLI they start run in `agents.slice`:

| Limit | Default | Effect |
|---|---|---|
| `MemoryHigh` | none (`infinity`) | no throttle, see below |
| `MemoryMax` | 75% of RAM | the kernel OOM-kills a process inside the slice |
| `MemorySwapMax` | 10% of RAM | caps how much the slice can push into zram, which `MemoryMax` does not count |
| `CPUWeight` | 50 | system units (weight 100) win under CPU contention |
| `TasksMax` | 8192 | stops fork bombs |
| systemd-oomd | swap > 90% only (`ManagedOOMSwap` on `pandacode-host` and `t3code-host`) | kills the task cgroup using the most swap |

zram stores incompressible pages at about their full size, so the slice can hold up to
`MemoryMax + MemorySwapMax` (85%) of physical RAM. That leaves 15% for the system. In an early CI run,
85% + 25% did not fit, and a 6 GB random-data hog made the whole box run out of memory instead of the
slice alone. Keep the sum well under 100% if you change these values. On boxes with little RAM (about
2 GB or less), the system's own share can be more than 15%.

Inside the slice, each agent run gets its own cgroup under its host's delegated cgroup (both hosts
have `Delegate=yes`; the CLI
shims, see [agents.md](agents.md#agent-runtime-new-in-unreleased)). Each task cgroup has
`memory.oom.group=1`. When the slice reaches `MemoryMax`, the kernel kills the process with the
highest OOM score (in practice the biggest task) and, because of `oom.group`, its whole task with it.
That choice is deterministic. Every shimmed task also raises its own `oom_score_adj` by 500 (to
1000), and `host/` has `memory.oom.group=0`, so even when the host itself is large, a task is the
preferred victim and an OOM kill in `host/` never takes the whole host. systemd-oomd watches
`pandacode-host.service` and `t3code-host.service` for **swap exhaustion only** (`ManagedOOMSwap=kill`, the task using the
most swap), and never kills the `host/` leaf, which is marked `user.oomd_avoid`. Pressure-based oomd
(`ManagedOOMMemoryPressure`) is off on purpose: it picks the task cgroup with the highest page-scan
(reclaim) rate, which can be an idle bystander whose pages are being reclaimed. In CI run
40876914383 it killed the idle task first. For the same reason the slice has no `MemoryHigh`: the
throttle put the hog to sleep, so its scan rate dropped to almost 0. The agent CLIs own their task cgroups, so a task could move itself
into `host/`, or set its own memory limits. This setup contains runaway tasks. It does not contain a
task that tries to dodge the OOM killer on purpose. `MemoryMax` on the slice still holds for every task.

`pandacode-host` and `t3code-host` have `OOMPolicy=continue` (a killed agent does not stop the host) and
`OOMScoreAdjust=500`. sshd, journald, NetworkManager and panda-mem get `OOMScoreAdjust=-500`,
so the global OOM killer picks agents first. They do not get -1000, because ssh sessions inherit the
value. The `agent-env` VM test starts an idle task and a 6 GB memory hog through the shims, from a
stub host running inside PandaCode's real FHS/bubblewrap environment. It checks that the kernel kills
the hog's task while the host, the idle task and sshd keep their PIDs. There is no earlyoom: it would watch the whole box and could kill a system daemon.

These limits protect the box from runaway agents, from either host. File isolation comes from the
separate `agent` user (above), which both hosts share.

## MCP tools and their permissions

Each MCP server runs as the agent that starts it (stdio), so it has that user's rights and no more.
None of them runs as root or has a setuid helper.

| Server | Can change | Guard |
|---|---|---|
| `panda-mem` | memories, through the panda-mem socket | the socket's group (`panda-mem`), and every change is a git commit |
| `panda-ckpt` | the caller's own `~/work` and `~/.checkpoints` | `ckpt_restore` needs `confirm: true` and takes a safety checkpoint first. The auto timer runs as the user, with no network and write access to that user's home only |
| `panda-sys` | nothing by default | `sys_updates action=run` needs `services.panda-sys.allowMutations = true` (in root-owned `/etc/panda/sys.json`), `confirm: true`, and a caller in the group `panda-sys-update` |

**The polkit update group.** `sudo` cannot work for agents (`NoNewPrivileges`, and `agent` is not in
`wheel`), so `sys_updates action=run` starts `nixos-upgrade.service` over D-Bus. With
`allowMutations = true`, PandaOS puts `services.panda-sys.updateUsers` (default `[ "agent" ]`) into
the group `panda-sys-update`, and adds one polkit rule. The rule lets that group **start** exactly
`nixos-upgrade.service` and nothing else: no other unit, and no stop, restart or edit. The update
itself runs as root under `panda-update`'s health check and rollback, from the `release` channel the
box already trusts. An agent can choose *when* to update, but not *what* gets installed. With
`allowMutations = false` (the default), the group has no members and there is no rule. The
`panda-sys` VM test covers both cases.

`panda-sys` read limits: callers outside `wheel`, which includes `agent`, can read the journal only
for an allow-list of PandaOS units (pandacode-host, panda-mem, panda-browser,
panda-ckpt-auto-agent, panda-agents-sync, systemd-oomd, nixos-upgrade, sshd, NetworkManager).
Unit names and `since` are validated, and `lines` is 1..500. `sys_net` sends one HEAD request to `connectivityUrl`, and
nothing else in these servers uses the network.

`confirm: true` protects against mistakes. It is not a security boundary, because an agent can always
send it. The brief in `/etc/panda/AGENTS.md` is guidance only, and it is not enforced.

`/etc/panda/mcp.d` is written by Nix and owned by root. `panda-agents sync` copies its entries into
the user's CLI configs, and it writes only `panda-*` keys there.

## System hardening

- No default password. The installer sets yours. A system without one (for example, built
  directly from this flake) prompts for one on tty8 at first boot (`panda-firstboot.service`, which
  runs once and then writes `/var/lib/pandaos/firstboot-done`).
- `sudo` only for `wheel`, always with a password (the live ISO is the exception: no password).
- Kernel sysctls: `kptr_restrict=2`, `dmesg_restrict=1`, no unprivileged eBPF, BPF JIT hardening,
  strict reverse-path filtering, no ICMP redirects or source routing, protected links, FIFOs and
  regular files in sticky directories.
- AppArmor is off. It ships no profiles for PandaOS's services, and it would add about 100 MB.

## Multi-tenant server mode (`services.boat.untrustedTenants`)

On the `server` profile (`nixos/profiles/server.nix`), PandaOS runs `boat` (`services.boat`, packaged
as `boatd`, `boat-api`, and `boat-gateway`) to host untrusted tenant code inside jailed Firecracker
microVMs. Three enforcement layers govern untrusted multi-tenant deployments:

1. **Two-tier NixOS evaluation (`server.nix` `mkDefault` warnings vs explicit `untrustedTenants = true` assertions).**
   `nixos/profiles/server.nix` sets `services.boat.untrustedTenants = lib.mkDefault true`, which turns
   on SMT disablement (`nosmt=force`), `memOvercommit = 1.0`, `storageOvercommit = 1`, `warmPool = false`,
   `ksmTeam = ""`, `swapDevices = []`, `zramSwap.enable = false`, finite per-team quotas, `blk-iocost`
   latency-targeted disk sharing (`boat-iocost-model.service` / `services.boat.ioCostModel`) with per-VM
   `io.max` ceilings (4,000 MB/s, 200,000 IOPS), and `kernel.io_uring_disabled = 1` scoped via
   `kernel.io_uring_group` to `boat-vmm` (`jailGid`) so jailed Firecracker VMMs retain `Async`
   (`io_uring`) disk I/O while unprivileged host processes cannot invoke `io_uring`.
   - When `untrustedTenants` remains at the profile's `mkDefault` (`priority 1000`), single-domain
     layouts (`vmDomain` or `desktopDomain` sharing a registrable domain with `domain`) or unset
     `gateway.dnsProvider` (on-demand HTTP-01 TLS, as written by `panda-install --profile server` into
     `local.nix` and checked by `tests/boat-eval.nix` `installDefault`) emit evaluation **`warnings`**
     so fresh installs evaluate cleanly before wildcard DNS-01 credentials are configured.
   - Setting `services.boat.untrustedTenants = true` explicitly (`priority < 1000`) promotes those
     domain-isolation and DNS-01 wildcard checks to hard build-time **`assertions`** (unless
     `services.boat.untrustedTenantsAcceptRisks = true` is explicitly set). At runtime, `boatd` also
     fails closed at startup if any mandatory untrusted invariant — cgroup v2 `+io` delegation, a
     resolvable whole block `stateDisk` (including loop/dm backing devices), `ioCost` + `ioCostQoS`,
     per-VM **`ioMax` (`io.max` hard BPS/IOPS ceiling)**, or `storageOvercommit = 1` — cannot be
     enforced; `ioCostModel` (`blk-iocost` proportional weight sharing) is enabled automatically once
     hardware model parameters are measured or configured (`boat-iocost-model.service` runs
     `boatd -measure-disk` once into `/var/lib/boat/iocost-model`, or set `services.boat.ioCostModel`)
     and logs a startup warning while sharing under `io.max` ceilings rather than blocking `boatd`
     startup if unset.

2. **Zero-exception nftables quarantine and `hostHttpsAddrs` (`components/boat/boatd/netw.go`).**
   Every VM starts network-quarantined until its guest SSH policy helper and sshd configuration have
   been installed and attested for the current host boot epoch (`sshNetworkReady()`). Because initial
   policy installation runs offline over Firecracker vsock, `quarantineRules` are emitted at the top of
   `inet boat` `input`, `forward`, and `output` chains **before** `ct state established,related` or
   DNAT acceptance (alongside L2 drops in `bridge boat`), giving unverified or quarantined VMs **zero**
   IPv4 or IPv6 network exceptions. Quarantined VMs are also excluded from DNAT/published routes and
   listed under `"quarantined"` in `/run/boat/routes.json`, causing `boat-gateway` to reject any
   request associated with a quarantined VM IP with HTTP 403.
   Once a VM is unquarantined, guest connections to `hostHttpsPort` (used for online guest SSH
   authorization callbacks to `POST /v1/vms/{id}/ssh/authorize` and public-URL hairpinning) are
   accepted **literally** to `hostHttpsAddrs` when configured (`10.200.0.1` is accepted **only** when
   explicitly listed, for example in `stack.sh` or operator `boatdSettings` — `boatd` never silently
   injects the bridge gateway IP); when `hostHttpsAddrs` is unset, default `:443` (`hostHttpsPort`)
   input in `inet boat input` explicitly drops RFC 1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`),
   `0.0.0.0/8`, loopback (`127.0.0.0/8`), link-local (`169.254.0.0/16`), CGNAT (`100.64.0.0/10`),
   `192.0.0.0/24`, `198.18.0.0/15`, and multicast/reserved (`224.0.0.0/3`), plus IPv6 unspecified/loopback
   (`::/128`, `::1/128`), IPv4-mapped (`::ffff:0:0/96`), NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`),
   discard (`100::/64`), Teredo/6to4 (`2001::/32`, `2002::/16`), documentation (`2001:db8::/32`), ULA
   (`fc00::/7`), link-local/site-local (`fe80::/10`, `fec0::/10`), and multicast (`ff00::/8`) before
   matching `fib daddr type local`. Inbound DNAT on `PublicIP` (`publicIPv4`) in `inet boat prerouting`
   is likewise restricted strictly to published TCP ports (`HostPort > 0`, forwarding `PublicIP:GuestPort`
   to `v.IP:GuestPort` rather than exposing all guest ports 1:1).

3. **`APIKey.Gen` secret generation, `overlap_s = 0` revocation, and scope/replay boundaries (`components/boat/api`).**
   Each `APIKey` carries a monotonic secret generation counter (`APIKey.Gen`) incremented whenever its
   secret is rotated (`POST /v1/keys/{id}/rotate`, and `/api/v1` key rotation which enforces
   `overlap_s = 0`). Authenticated API principals and derived credentials — signed file upload/download
   grants, private port and desktop route tokens (`PortMinter`), **both** `bsh_` share links and
   cross-team `team_id` snapshot grants (`SnapShare`, which bind `KeyID` + `MintedByKeyGen` and cap
   `Expires` to the minting `admin` key's expiry), and managed SSH grants (`SSHGrant`) — record and
   verify the issuing key's `Gen` alongside key membership and scopes. Rotating a key with
   `overlap_s = 0` clears `PrevHash`/`PrevUntil` immediately (zero overlap window), revoking the old
   bearer secret and invalidating prior-generation grants at once without deleting the key's ID or team
   bindings; a positive `overlap_s` permits the previous secret generation only until `PrevUntil`.
   Scoped `vms:write` keys without `exec` cannot mint `exec` authority via `self_api` (`BOAT_API_KEY`
   issuance), caller `env` overrides, or unbuilt `Environment.SetupScript` execution, and
   `Idempotency-Key` cached response replay verifies both the minting key's current authority and
   current `RouteGens` (for `hostedPorts` URLs) before replaying.

### boat-api: narrowed resumes and durable secret selections (r48)

- **A narrowed resume's self-token cannot read earlier runs.** When an environment
  removes secrets from the previous run, start cold-boots and records the token's
  `NarrowGen` and run-start time. The no_env self-token allowlist also applies to
  that generation: snapshot listing and download, command history, and prompt
  records are refused with 403; individual commands are reachable only if they
  started in the new run. New commands and their status/logs still work. Stopping
  the VM or rotating its token for a snapshot does not release this restriction.
  The sandbox still receives the selected environment's secrets; it is not no_env.
- **Create and fork persist no_env in boatd.** Every no_env create or fork child
  carries reserved labels `boat.no_env="1"` and `boat.secrets=""`. These labels are
  durable before the API answers, so losing an asynchronous API mark or selection
  write cannot turn a later start, command or fork into one with team secrets.
  The no_env check reads boatd when its API mark is missing; an unavailable policy
  fails closed. Users cannot set or replace reserved `boat.*` labels.
- **New VMs fail closed after lost selection records.** Every new VM, including a
  fork child and an environment builder, carries `boat.sel="1"`. If both the API
  selection record and `boat.secrets` are absent, this marker selects no secrets.
  Only legacy VMs without the marker retain the fallback to all team secrets.

### boat-api: no_env is sticky (r46)

Credential policy also follows authenticated share imports. The API resolves a
share's source record before selecting any environment, including the team
default. Sticky disk policy is stored separately in `State.VMSecrets` under
`snap:policy:<id>` and persisted before snapshot publication. Unknown or nonempty
run inventory, failed environment delivery, and pending transitions cannot drop
that policy. A strict legacy `@no_env` source record also preserves policy;
restoring copied memory requires a separate protected-lineage certificate. It is issued only from a
positive in-lock VM record, protected creation lineage, known-empty team run
names and no pending credential transition. Legacy conversions and their
descendants retain sticky policy but do not gain this restore eligibility.
The certificate makes no claim about arbitrary files on a tenant disk. Missing
certificates make same-team protected copies cold-boot and refuse protected
foreign copies. A lost batched fork run record cannot certify RAM restoration. A protected clone without
that evidence cold-boots; a foreign disk without protected-source evidence is
refused when asked to become protected. Imported share policy is persisted before
the import returns.

Creation records carry reserved `boat.foreign`, `boat.restricted` and
`boat.no_self_api` labels where applicable, in addition to selection and no_env
labels. Forks preserve these restrictions. Startup adoption rehydrates them before
recovering credentials. Labels add restrictions; unlabeled legacy VMs retain their
existing selection behavior. Losing a foreign VM's API cache does not authorize
new self API access or raw AI credentials.

Cold policy transitions persist pending intent before changing the no_env mark or
asking boatd to discard memory. The intent survives failed calls and API restarts.
Commands and prompts remain gated; a retry repeats the fresh boot until both boot
and guest environment application succeed. A narrowed start whose guest update
fails is force-stopped, keeps its pending intent and can be retried. Pending
transitions never count as protected snapshot evidence. Start rechecks current
caller authority, VM ownership, builder policy and named environment after taking
the lifecycle lock. A pending retry keeps its saved environment version, secret
selection and custom variables. Successful transition completion saves effective
policy and clears pending intent in one durable write. A plain cold start with
unknown run inventory discards old memory before establishing a new inventory.

Both prompt routes and running environment updates record the conservative union
of possibly delivered team-secret names before dispatch. New names require a
durable write; an unchanged inventory adds no disk barrier. Ambiguous replies
retain the union, and unknown inventory stays unknown. Prompt dispatch holds the
shared lifecycle lock through request acceptance, while synchronous prompt output
streams after releasing it. These records describe possible credential delivery,
not inspection of guest processes or files. Startup reconciliation confirms each
missing-inventory candidate with a current boatd lookup under its lifecycle lock
and retires credentials within that same critical section; an unavailable lookup
aborts retirement.

Environment upgrades accept optional `agentIds` on native and compatibility
routes. The API validates caller authority, team ownership and environment
membership before changing the target set. An empty list upgrades none; null is
rejected. An already-current pin is skipped before inspecting custom secret
selection. Full-map credential refresh serializes with lifecycle changes and
upgrades. Before a live push, a durable union records all possibly delivered
secret names; an ambiguous reply retains that union and leaves the pin unmoved.
Reconciliation locks each lifecycle and rereads pending intent and VM state
before stopping it. A live
success means the guest accepted the new environment; the pin and effective
secret selection are then saved. Custom sandbox vars and foreign credential
restrictions still apply. A stopped upgrade saves its effective selection and pin
for resume; a removal also saves durable cold-boot intent. Live removals fail
without moving the pin because an existing process can retain its inherited
credentials. Foreign disks requiring a removal also fail because their boot-time
cleanup cannot be certified. A live legacy sandbox without a recorded pin also
fails, because the API cannot infer its previous variable set. Its stopped
upgrade requires a fresh boot on resume. Named resume and fork also cold-boot
when effective pinned plain variables are removed, accounting for secret and
per-sandbox overrides. Deleting an environment keeps existing pins available
for plain resume while preventing new named selection. Environment deletion
retains its independent default and minimum-environment restrictions.

**Legacy preboot limitation.** Fresh boot confirms disposal of the old memory;
it does not prove that an older guest disk was clean before its boot services
ran. The new guest source moves managed env to `/run/boat/env` and installs a
cleanup unit before sysinit. Existing disks and snapshots without that unit do
not gain it through an API upgrade. There is currently no trusted image/disk
capability metadata by which the API can certify this preboot boundary. A later
`set_env` acknowledgement is not such evidence. Ordinary same-team cold starts
and narrowing retain their existing behavior in this change: their memory is
discarded where required and guest failures stop the VM, but no preboot disk
cleanup guarantee is made for old images. Protected-from-creation sandboxes and
proven protected snapshots remain supported. The API does not inspect tenant
filesystems or claim that a new image has been deployed. Complete legacy preboot
protection requires a separately established trusted image/disk provenance
contract and migration of existing guest disks.

A sandbox created, started or forked with `no_env` is `no_env` **for good**, as on
boat.dev: the mark (`State.NoEnvRuns`, persisted) is not ended by a stop, a plain
start or resume, a type change, a recover, a racing stop or a restart of boat-api; it
ends only with the sandbox. What follows:

- **Every later start pushes no team secrets.** A plain start of a marked sandbox is a
  `no_env` start: the old secret selection is not restored, prompts and commands carry
  none, and the stored per-sandbox `env` is not replayed. A start, resume or fork that
  names an `environment` (or sends `no_env: false`) on it is **400 `no_env_sticky`**
  before any side effect (so is a create with `no_env: true`, or from a `no_env`
  snapshot, that names a non-empty `secrets` list; `secrets` absent or `[]` is fine): an environment would put team secrets back on a disk that
  was made to never hold them again, so the caller creates a new sandbox. A fork, clone
  or create-from-snapshot of a `no_env` sandbox is `no_env`, with no environment (naming
  one is the same 400, as is naming `secrets`).
- **`@no_env` snapshot records are strict.** A snapshot is recorded `@no_env` only when
  the run it was taken from pushed no team secret (its recorded secret names are empty;
  a `no_env` cold start records that). A `no_env`-marked run whose names are not empty,
  or unknown, is recorded by its names or not at all, never as secret-free. Its
  separate sticky disk policy is still saved durably and follows share imports,
  including across teams. That policy alone never authorizes RAM restoration;
  an uncertified foreign protected copy is refused.
- **AI credit is still spendable.** A `no_env` run still gets per-VM AI proxy tokens
  (never a raw provider key), so the team's AI credit can be spent from it. This is
  unchanged: `no_env` withholds team secrets, not AI proxy access.
- **A `no_env` start or fork never replays the stored `env`.** The per-sandbox `env` a
  caller stored earlier (it is sealed in the state) may hold secrets the caller meant for
  the old runs; a `no_env` start, and a fork of a `no_env` parent or with `no_env: true`,
  uses only the `env` sent on that request and does not save it as reusable
  per-sandbox env. A pending cold transition temporarily retains that request's
  variables for an exact retry; successful completion removes the pending intent.
- **The self-token allowlist applies while the sandbox is `no_env`** (see boat.md,
  "A `no_env` sandbox's own token"), not only for the run that set the mark, so the
  token a later start mints is limited the same way; a token minted for a `no_env`
  run stays limited as before (`VMToken.NoEnvGen`).
- **Known limit: a `no_env` run can plant files on its own disk.** Everything a `no_env`
  run (or a command through its own token) writes to its disk stays there. Because the
  sandbox is `no_env` for good, that disk never receives team secrets again: planted
  files cannot read team-injected secrets from a later run (there are none), and
  its snapshot retains sticky policy. Only a known-empty inventory records `@no_env`. What this does not protect is the earlier history: its snapshots,
  command and prompt history from before the mark may hold team secrets, which is what the
  self-token allowlist withholds. Do not share a `no_env` sandbox's disk as proof that it
  never held a secret before it became `no_env` (a conversion by a `no_env` start boots
  fresh, but old blocks may remain, see "Known limit" below).

The paragraphs below were written for the per-run behaviour of r33-r45 (a stop followed
by a normal start ended the run); where they say so, read "ended" as "never ended":

- **Type change.** Compat `POST /sandboxes/{id}/resume` with `type` changes the size of a
  *stopped* sandbox (a running one is 409 `sandbox_running_stop_first`, nothing stopped);
  the start after it is `no_env` whenever the sandbox is, even if the request did not send
  `noEnv`. A compat fork with `type` restarts the child the same way.
- **Fork and clone.** A fork or clone of a `no_env` sandbox (running or stopped) is a
  `no_env` child (no team secrets released, run marked `no_env_run`, also through the
  compat `type` restart); `no_env: false` / `noEnv: false` or an `environment` on it is 400
  `no_env_sticky`. A fork without a `ttl_s` / `ttlSeconds` gets one hour, not its parent's
  lifetime (a `no_env` parent with auto-stop off does not leave an immortal child).
- **Racing stop.** A stop that completes while a `no_env` cold start is still booting
  the VM does not end the mark (no stop does, since r46).
- **Restart paths.** Compat `recover` (force stop, then start) reads the run's mark
  before the stop and restarts the VM `no_env`: no team secrets reach the guest and
  the run stays marked. The start half of any restart the server performs itself
  (recover, type change, restore after a failed one) is a *managed restart*
  (`withManagedRestart`): `startVM` keeps a still-marked `no_env` run `no_env` even
  if the restarting path forgot to say so, so a new restart path cannot drop it.
- **Start in flight.** `startVM` takes the not-ready gate (the env push mark) first:
  right after the VM's start lock, *before* it reads the VM's state or the run's
  `no_env` mark, and holds it until the start has settled. `noEnvRun` neither ends
  nor drops a run's mark while the gate is held, so a stop plus a read racing the
  start can no longer remove the mark between the start's read and its gate. A stop
  that completes before the start took the gate ended the previous run, and the
  start reads the VM as it is then (a stopped VM cold-starts as a new, normal run).
  While the gate is held `POST /v1/vms/{id}/prompt` and `/prompts` answer 409
  `sandbox_not_ready` (as the compat commands do), whatever state the VM was read
  in, until the start has re-marked the run. Every start holds the gate, also a start
  of a VM read as running: boatd may serve it after a stop took the VM lock first
  (restoring the VM). Since r46 no stop ends the mark and no start clears it, so there
  is no re-marking after boatd answers. The compat sandbox reads `provisioning` whenever the gate is held, in
  any boatd state (also the pre-start window of a resume, where boatd still says
  stopped).
- **Prompts re-check right before the exec.** A prompt can pass the not-ready check and
  choose its secrets just before a `no_env` cold start marks the run. `/prompts`
  (and steer) and the legacy `/prompt` therefore look again after the secrets were
  chosen and just before the exec: if the gate is held or the run is now `no_env`
  while team secrets were selected, the request answers 409 `sandbox_not_ready` and
  nothing reaches the guest. `env_pending` on a VM is true whenever the gate is held,
  in any state.
- **A `no_env` cold start boots fresh when the previous run had secrets.** The guest
  keeps the previous run's `/etc/boat/env` on its disk (native exec and SSH source it),
  and a default stop keeps a memory snapshot whose processes resume with the team
  secrets in their environ. So a `no_env` cold start of a VM whose previous run was
  *not* `no_env` (no mark) sends `"fresh_boot": true` to boatd: it cold-boots and
  ignores and discards the stopped VM's memory snapshot. A start after a `no_env` run
  with known-empty run inventory resumes as usual. Unknown or nonempty inventory
  and pending transitions force another fresh boot; ordinary secret-bearing cold
  starts also discard memory when their inventory is unknown.
- **A protected fork restores memory only with positive lineage evidence.** A
  `no_env` child cold-boots unless its parent has protected creation lineage,
  known-empty team run names and no pending transition. Legacy conversions and
  their descendants remain `no_env` but cold-boot their children. The fork records
  its known-empty team run and sticky policy independently of restore eligibility.
  A failed release fails closed; the child retains sticky policy, and an
  env-never-landed mark forces another fresh boot on retry.
- **A fork reads its parent's run under the parent's start lock.** Whether the child is
  released by a cold boot depends on the parent's run (its `no_env` mark) and the prepare
  copies the parent's memory, so both happen while the fork holds the parent's start lock
  (shared with other forks of that parent, exclusive against a start of it). A start of the parent cannot slip between the
  mark read and the prepare and leave a `no_env` child resuming the memory of a run that
  has secrets. The lock is released as soon as boatd's prepare answers. The parent is read
  from boatd again once the lock is held (the read that authorized the fork may predate a
  start that held it): the `no_env` decision, the child's labels and its secret selection
  are all the post-start parent's, like `startVM`'s own re-read. Selection, policy,
  environment pin, custom variables and run names come from an immutable store
  snapshot captured before prepare under that same lock. Child preparation after
  unlock keeps this view even when a later parent start changes its metadata.
  Reserved selection labels describe this captured effective selection too.
- **A create from a snapshot with a narrower secret selection boots fresh.** A memory
  snapshot resumes the processes of the run it was taken from, which hold that run's
  secrets in their environ, and its disk holds that run's `/etc/boat/env`. When a snapshot
  is created (native, compat saved snapshots, environment builds) boat-api records the
  selection of the source run (secret names, `@default`, all, or `@no_env`) next to the
  snapshot (in `State.VMSecrets` under `snap:<id>`; dropped with the snapshot or its VM).
  A create from it (also an environment's build snapshot, a share, compat `from`) whose
  requested selection is not a superset of the recorded one sends `"fresh_boot": true` to
  boatd: the snapshot's disk only, a cold boot, its memory never loaded. A snapshot with no
  record (an import, one taken before this was kept, a lost write) may have held any team
  secret: it resumes only for a create whose selection covers **every** secret of the team;
  any other selection, `noEnv` included, boots fresh. A share import copies the sender's
  record to the new snapshot (see below). Sticky `no_env` policy does not waive a
  fresh boot: copied RAM is restored only with a matching positive certificate. A fresh-boot VM always gets its environment pushed, even when empty (`BOAT_ID`
  is always in it), and fails closed like a fork: if the push fails it is force-stopped
  (deleted when it cannot be stopped; 500 `cleanup_failed` when neither works) and the
  create answers 502 `env_start_failed` with the stopped VM in `vm` (a batch: all of the
  batch in `vms`; compat: `sandbox` / `sandboxes`).
- **A failed-closed answer describes what is left.** The 502 `env_start_failed` `vm` is the
  VM as boatd reports it after the stop (state `stopped`, no `expires_at`, no `env_error`),
  not the running VM the start or fork saw. The `sandbox.error` webhook is published from
  the VM in hand before the stop (a later read would find it stopped and send nothing).
  Through compat the error envelope carries the kept VM as `sandbox`, so a client that
  forked keeps the child's id.
- **Known limit: the old env stays on the disk after a fresh boot.** A fresh boot discards
  the memory but not the disk. The source run's `/etc/boat/env` is replaced by the new
  push, yet its old blocks may remain recoverable by guest root until overwritten (no
  secure erase of a copy-on-write disk). A VM whose disk once held a secret must be
  treated as having held it: rotate the secret if that matters, and do not share such a
  disk with a party the secret is hidden from.
- **No in-place conversion to `no_env`.** A start with `no_env` on a VM that is already
  running and whose run is not `no_env` is refused (409 `stop_required`) before anything is
  pushed or marked: its processes and its `/etc/boat/env` already hold the team's secrets,
  and a push cannot take them back. Only a cold start (after a stop) begins a `no_env` run,
  and one after a run with secrets boots fresh. A running `no_env` run still accepts a start
  with `no_env` (it replaces the guest env, never adds team secrets).
- **A VM's own token never starts its VM.** The start route refuses a self-token (403) before
  any token generation is rotated, so a refused start changes no credential.
- **A `no_env` start fails closed.** If boatd answers the cold start but its env did not
  land (`agent_ready=false` / `error`), or the start call errors while the VM may be
  running, the VM is force-stopped (a managed stop: no stop operation is recorded, so
  nothing reads it as ending a run) and the start answers 502 `env_start_failed` (with
  the stopped VM in `vm`; `env_update_failed` only when that stop failed too); it
  never answers 200 with a running VM whose env is unknown. The failure is recorded
  (`env_error`, an env-error span, `sandbox.error`) on the error return as well as on
  the success path. When the VM is stopped, the previous run's mark is put back (so
  the next `no_env` start boots fresh again); when the stop itself fails the `no_env`
  mark stays and the answer says the VM could not be stopped. A start of an already
  running `no_env` run whose call errors re-reads the VM under the gate and re-marks
  the run when a racing stop took the mark, instead of dropping it.
- **Run ends are ordered by sequence, not by the clock.** A stop operation and the run's
  mark each carry a number from the store's monotonic counter (`State.Seq`, taken in the
  same mutation that records them and persisted). A stop ended the run only if its
  number is greater than the mark's, so a clock that steps back (NTP, a restored VM)
  cannot make a stop look older or newer than the mark. The mark written when boatd
  answers a start takes the counter as read then, so a stop requested while boatd was
  starting the VM, whenever its handler completes, does not end the run. Records that
  predate sequences still compare by time. The lazy removal of an ended mark is a
  compare-and-delete inside the store mutation: it removes the mark only if it is still
  the one that was read and no start holds the gate.
- **What a `no_env` run still receives.** `no_env` withholds the team's *secrets*, not
  its AI credentials. With the team's AI proxy on, the run gets a proxy token
  (`<PROVIDER>_API_KEY` = a per-VM token and `<PROVIDER>_BASE_URL` = the proxy); with
  the proxy off, it used to get the raw provider keys. (r42: a `no_env` run never gets a raw
  key: with the proxy off it gets no AI credentials, with it on it keeps the per-VM proxy
  token, which is revocable.)

### boat-api: snapshot records, builders and fail-closed leftovers (r40)

- **A snapshot is recorded under its VM's start lock.** `captureSnapshot` holds the VM's
  start lock shared from boatd's prepare until the record is made (an exclusive start waits;
  snapshots and forks of one VM do not exclude each other). A snapshot of a stopped
  secrets-run VM racing a `no_env` cold start can therefore no longer see the start's mark
  and be recorded `@no_env` (which would let a create resume memory that holds secrets).
  The record uses the VM read under that lock and concrete run inventory.
  Known-empty protected creation lineage, a valid VM state and no pending or
  failed delivery are all required to certify copied RAM. Missing VM state or
  uncertain inventory never certifies memory. Sticky policy remains independently
  durable whenever the API mark or VM label establishes it.
- **Records hold concrete names.** The source inventory includes every possibly
  delivered team-secret name. The `@default` marker is retained when applicable,
  so a later default change cannot silently narrow a resumed run. An unknown
  inventory remains unknown; it is never replaced by the team's current defaults
  when taking a snapshot.
- **A share import carries the sender's records.** The imported records name no
  source VM, so deleting the sender does not remove them. Sticky disk policy and
  any valid protected-memory certificate follow the import independently. Only
  same-team copies retain ordinary secret names; across teams a strict empty-run
  record can carry over, while uncertain or nonempty inventories cannot certify
  protected memory. Policy is durable before the import returns.
- **An environment builder holds only its environment's secrets, and only an admin uses it.**
  The builder VM is created with the environment's selection (its `secrets`, or `@default`)
  on its `boat.secrets` label and in the store; before this a start or fork of it injected
  every team secret. A builder from before this (label `boat.env`, no recorded selection)
  falls back to its environment's selection, or none when the environment is gone. Start,
  fork and exec of a VM labelled `boat.env` are refused with 403 `forbidden` for a key without
  scope `admin` (the builds are admin-only already). The build snapshot's record is the
  builder's selection as names. (r41 widened the refusal to every route: see below.)
- **A VM whose env never landed fails its next start closed.** A fresh-boot create or fork
  that fails closed leaves a stopped VM whose disk still holds `/etc/boat/env` of its
  snapshot or parent. A mark (`State.VMSecrets["envnever:<vm>"]`, dropped with the VM) is
  written there, and a cold start of that VM fails closed like a `no_env` one: if its env does
  not land the VM is force-stopped again and the start answers 502 `env_start_failed` with the
  stopped VM. The mark is cleared by the first start whose env landed; later starts are
  ordinary. A mark that cannot be written is logged and the VM is still stopped. (r41: the
  mark follows the disk into forks and snapshots: see below.)
- **A fail-closed answer is not "provisioning".** The create and fork gates are released
  before the 502 is written, so its `vm` (every VM of a batch) reports `env_pending: false`,
  as a failed start does.

### boat-api: builder gate on every route, the env-never mark follows the disk (r41)

- **An environment builder is closed to non-admin keys on every route.** `ownVM` (which
  every route that resolves a VM goes through, native and `/api/v1`, and the body guard) refuses
  a VM labelled `boat.env` for a key without scope `admin` with 403 `forbidden`, unless the
  route is one of a short allow-list of metadata reads (`builderReadRoutes`: `GET` of the VM,
  usage, AI usage, snapshot lists, ports, SSH grants, and revoking an SSH grant). Anything else,
  including a route added later, is refused: start, stop, fork, clone, delete, patch, resize,
  exec, commands, prompts and the legacy prompt, files, ports, SSH key and grant routes, desktop,
  snapshot create, download and delete, the compat sandbox snapshot delete, compat commands,
  prompt and files. The builder's own VM token (it acts on that VM only) and admin keys pass.
  A fix found while testing: `DELETE /api/v1/sandboxes/{id}/snapshots` did not resolve the VM
  and deleted a builder's unnamed snapshots for any key; it now goes through `ownVM`.
  Test: `TestR41BuilderGateCoversEveryRoute` probes every `{id}` route of the table.
- **The env-never-landed mark follows the disk.** A fork of a VM marked `envnever:<vm>`
  copies a disk that still holds another run's `/etc/boat/env`, so the child is released like a
  fresh boot that fails closed (`fresh_boot`, 502 `env_start_failed` with the stopped child when
  its env does not land) even when it is not `no_env`, and it inherits the mark before the
  release, so a crash or lost answer leaves it marked. A push that lands clears the child's
  mark; the parent keeps its own. A snapshot of a marked VM gets no memory-inventory
  record (unknown source), but retains any sticky disk policy, so a
  create from it resumes its memory only when the selection covers every team secret.
- **`resume_on_boot` is the host's.** A tenant `PATCH /v1/vms/{id}` carrying it is refused
  with 400 `invalid_request` ("resume_on_boot is managed by the host"): boatd restarts such VMs
  at its own start with none of the API's gates (env, no_env, builder, fail-closed).
- **A normal cold start drops a stale `no_env` mark first.** Before boatd is asked to start
  the VM (inside the not-ready gate), a cold start that is not `no_env` deletes the run's mark;
  a start whose boatd call errors may leave the VM running with the team's secrets, and a
  surviving mark would have called that run `no_env` (and let a snapshot of it be recorded
  `@no_env`). A mark that cannot be dropped (the store write fails) stops the start with 500
  `store_error` before boatd is called, instead of starting with the stale mark.
- **A snapshot capture holds the start lock only for the record.** `captureSnapshot` takes the
  VM's shared start lock for boatd's prepare and the record, then releases it before the
  credential revoke (a synchronous state write plus an SSH refresh). Sticky policy
  requires its own durable write while this shared lock is held; ordinary source
  bookkeeping remains asynchronous. The revoke is also durable before
  boatd releases the snapshot, as in fork; a failed revoke deletes the pending snapshot and its
  record. Test: `TestR41SnapshotRevokeDoesNotBlockStart`.
- **Conversations per VM are capped at 200** (the least recently updated is dropped), like
  prompts; before, a VM could grow its state without bound.
- **The per-team AI body budget is 32 MiB**, a quarter of the 128 MiB global one (it was
  64 MiB, half), so one team cannot take more than a quarter and at least four teams can hold
  their full budget at once. The per-VM budget follows (32 MiB: one maximum body).

### boat-api: builder gate on team snapshot routes, run-start names, no_env AI keys (r42)

- **The builder gate covers the team-level snapshot routes.** `deleteTeamSnapshot` resolved a
  snapshot by team only, so a non-admin key could delete a builder VM's snapshot, or an
  environment's build snapshot (the fast-start image of its secrets' setup), and so make every
  later create rebuild or fail. It now answers the builder 403 for a key without scope `admin`
  when the snapshot's `vm_id` is a builder VM (`refuseBuilder`, the lookup fails closed on an
  upstream error and passes only when the VM is gone) or its id is any `Environment.Build.
  SnapshotID` of the team. Every route reaches it: `DELETE /v1/snapshots/{sid}`,
  `DELETE /api/v1/snapshots/{sid}`, `DELETE /api/v1/saved-snapshots/{name}` and its
  `named-snapshots` twin (all same-name snapshots are checked before any is deleted), and
  `POST /api/v1/saved-snapshots` (and `named-snapshots`), which replaces same-name snapshots
  and now refuses, before taking the new one, when one of them is protected. Tests:
  `TestR42TeamSnapshotRoutesRefuseBuilderSnapshot`, `TestR42EnvBuildSnapshotProtected`.
- **A `no_env` run never gets a raw AI key.** With the team's AI proxy off, the raw provider keys
  went into every run, a `no_env` one included; a raw key cannot be revoked per VM. A `no_env`
  run (start, retype restart, prompts, legacy prompt, fork child) now gets none; with the proxy
  on it keeps its per-VM token. Test: `TestR42NoEnvRunGetsNoRawAIKey`.
- **A snapshot's record is the secrets the run was started with.** The concrete names pushed
  at run start (create, fork release, cold start, environment build; a start of a running VM
  adds to them) are kept in `State.VMSecrets["run:<vm>"]` (dropped with the VM) and the
  snapshot's record is made from them, not from `@default` expanded at capture time: a secret
  un-defaulted after the run began is still in the record, so a create with the default
  selection boots fresh. A VM with no recorded names (a run from before this, a lost write)
  gives a snapshot of unknown source, which fails towards a fresh boot until its next start
  records them. Test: `TestR42SnapshotRecordUsesRunStartNames`,
  `TestR42SnapshotOfRunWithoutNamesIsUnknown`. (The residual this section had, a resumed VM
  recording only its new start's names, is closed in r43.)
- **Lifetime AI usage is capped at 2000 rows per team** (`State.AIUsage`; it was unbounded
  in VMs created). The least recently used rows are folded into the team's `_deleted`
  aggregate on the meter's flush (totals unchanged; per-VM detail of the folded VMs is lost).
  Test: `TestR42AIUsageLifetimeRowCap`.
- **Compat fork with `type` forwards `failFast`** to the post-resize start, as resume does.
- **Residual: four teams can pin the global AI body budget.** The global budget (128 MiB) is
  four times the per-team one (32 MiB), so four teams can hold all of it and a fifth team's
  bodies get 503 until theirs drain. Team creation is operator-only, so this needs
  operator-created teams that are hostile or compromised; it is accepted and documented, not
  closed.

### boat-api: a no_env run's token, builder gate on downloads and creates, run names on resume (r43)

- **A `no_env` run's own token is limited to an allowlist (r45 supersedes the r43 deny list).**
  A VM started with `no_env` still gets `BOAT_API_KEY` (its self-token). The r43 fix refused a
  fixed list of snapshot and history routes and left routes that address one command by id
  open on the claim that the id has to come from a listing. That claim was false: a compat
  `processId` is a per-VM counter from 1, so `GET /commands/{pid}`, `/commands/{pid}/stream` and
  `DELETE /commands/{pid}` read (or stopped) earlier runs' commands by guessing. Now, while the
  VM's current run is `no_env` (or its token generation was minted for one, see r44), `route`
  AND `callNative` (`refuseSelfNoEnv`, `r43_selfgate.go`) answer 403 to a VM-token principal on
  every route not in `selfNoEnvAllowed`: GET of its own VM, `exec`, `commands` (start, status,
  logs, signal, stop, stream), the files routes, `ai-usage` and `stop` (native and compat). The AI
  proxy is not a route of this API (the token reaches it directly). A command addressed by id
  is allowed only if its record started at or after the current run's no_env mark
  (`State.NoEnvRuns`); a record of an earlier run, an unknown id, or a run whose mark was
  dropped by a stop is 403 (the answer does not tell a guessed id from one that never existed).
  API keys are not limited. Tests: `TestR43SelfTokenNoEnvRunRefusedSnapshotsAndHistory`,
  `TestR45NoEnvSelfTokenCannotReadEarlierRunCommands`, `TestR45NoEnvSelfTokenAllowlist`,
  `TestR45CallNativeRefusesNoEnvSelfToken`. Residual: within the run the token reaches every
  command record of the run, including ones another key started.
- **A `no_env` or `RestrictSelf` self-token cannot reshape its VM's lifetime (r45).** Such a
  token gets 403 on `PATCH` with `ttl_s`, `extend_s` or `idle_stop_s` (`patchVM`), on resize,
  and on `DELETE` of the VM (native and compat; `selfLifecyclePatterns`, enforced in `route`
  and `callNative`). It may still rename the VM. An ordinary self-token is unchanged: it can do
  all of these. Test: `TestR45RestrictedSelfTokenLifecycle`.
- **`keyAliveIn` honours `NoSelfAPI` for `vm_` keys (r45).** A signed-URL minter key `vm_<id>`
  stood while the token merely existed and was not retired; a VM whose self API was turned off
  (`NoSelfAPI`) still had its minted URLs alive. It now stands only while the token carries the
  self API. Test: `TestR45KeyAliveInHonoursNoSelfAPI`.
- **The builder gate covers compat snapshot download and creates from a snapshot.**
  `compatSnapshotDownload` resolved the snapshot by team only and the chunk URL's route
  (`compatFileDownload`, no key) resolved the VM only, so a member could download a builder
  VM's snapshot or an environment's build snapshot. Both now run `refuseBuilderSnapshot` (the
  chunk URL re-checks at fetch time with the minting key's own scopes, so a snapshot that
  became a build snapshot after the URL was minted is refused). `POST /v1/vms` and compat
  create with `snapshot` / `from` naming such a snapshot are the builder 403 for a key
  without `admin` (`refuseCreateFromBuilder`); an environment's own build snapshot, which
  `createVM` substitutes itself on the environment path, is not affected. Tests:
  `TestR43CompatSnapshotDownloadBuilderGate`, `TestR43CreateFromBuilderSnapshotRefused`.
- **A cold start keeps the secrets its resumed memory holds.** `recordRunNames` replaced the
  run's names with the new start's, so a secret un-defaulted since the run began, still in the
  resumed memory's processes, was missing from a later snapshot's record (a create with the
  default selection then resumed that memory). A cold start now unions the new names with the
  previous run's, except after a `no_env` run, whose memory holds no team secrets. Test:
  `TestR43ColdStartKeepsResumedRunNames`.
- **`no_env` with `secrets` absent selects none.** A create with `no_env` and no `secrets`
  recorded the `@default` selection for the VM's later starts and prompts; it is now `[]`.
  Test: `TestR43NoEnvCreateWithoutSecretsSelectsNone`.
- **Resume and fork with an environment** (boat.dev's `--environment`) apply that environment's
  vars and secret selection as create does and make them the VM's (or the child's), stored in
  the same records (`VMSecrets`, `VMEnvs`); an unknown name is refused before anything is
  stopped, resized or copied, and a fork whose selection does not cover the parent's run
  recorded names is cold-booted so it does not carry the parent's memory. A running VM cannot
  switch in place (409). The environment's own secret selection is trusted as at create (only
  an admin can write an environment). Tests: `TestR43NativeStartWithEnvironment`,
  `TestR43NativeForkWithEnvironment`, `TestR43CompatResumeAndForkWithEnvironment`.
- **Compat create:** `count` above 1 without `from` is 400 `count_requires_from`, and with
  `failFast` 400 `count_with_fail_fast`, both before `createVM`. Test: `TestR43CompatCountRules`.

### boat-api: no_env token generation, foreign VMs and environments, one `base` resolver (r44)

- **A `no_env` run's token stays restricted after the run is stopped.** The r43 gate keyed on
  the live run mark (`noEnvRun`), which a completed stop clears, while the self-token stays
  valid until the next start mints a new generation, and a VM token may stop its own VM. So an
  exfiltrated `no_env` token could `POST /stop` and then list and download the earlier
  secret-bearing snapshots and read command history. The restriction is now bound to the token
  generation: `VMToken.NoEnvGen` records the generation minted for a `no_env` run (set where the
  mark is written, `markNoEnvRunAt` / `markNoEnvRunAtSeq`, and when a stop ends a mark that
  pre-dates the field), and `refuseSelfNoEnv` refuses while the run is `no_env` **or** the
  token's current generation is that one. A new generation (any start) or a normal cold start
  clears it; a failed `no_env` push on a running VM puts it back with the mark
  (`restoreRunMark`). A self-token `stop` is still allowed (a VM may power itself off; the
  stop's memory snapshot of a `no_env` run holds no team secrets) because nothing after it is
  open to that token. Test: `TestR44SelfStopKeepsNoEnvTokenRestricted` (every step after the
  stop is 403/401), `TestR44TokenFlagRestoredWithRunMark`.
- **A named environment never gives a foreign VM team secrets.** `start`/`resume` and `fork`
  with `environment` applied the environment's secret selection to any VM, including an
  imported or shared (foreign) one, bypassing the create rule (`foreignSecrets`). For a VM (or
  fork parent) in `ForeignVMs`, `namedStartEnv` now returns the environment with `keepSel`: its
  vars apply, its selection does not (`namedEnv.selection`), `recordStartEnv` leaves the VM's
  recorded selection alone (so the choice is not made permanent), a fork keeps the parent's
  selection label and is never cold-booted for a "narrower" selection. Native and compat.
  Tests: `TestR44NativeForeignStartAndForkEnvironmentVarsOnly`,
  `TestR44CompatForeignResumeAndForkEnvironmentVarsOnly`.
- **`base` means one thing everywhere.** Create (native) looked `base` up as a literal name
  while create (compat) dropped it and start/fork resolved it as the team default. All of
  create, start, fork and clone, native and compat, use `resolveEnvRef`: an explicit
  reference first matches a same-team environment ID, then an exact name. A literal
  environment named `base` takes precedence; only an unmatched `base` falls back
  to the team default (none if the team has none). Omitted create references choose
  the default; a plain start or fork keeps the sandbox's existing environment. Test:
  `TestR44BaseEnvironmentOneResolver` (default `prod`, another environment named `base`).
- **`resume`/`fork` with `type` and an environment on a `no_env` run.** `retypeStartBody`
  forced `no_env` on the restart, so the named environment then got 400 after the stop and
  resize had happened. A start that names an environment is no longer forced (nor is a
  managed restart in `startVM`): the environment applies on the post-resize start and ends the
  `no_env` run. A still invalid combination (`noEnv` with an environment, an unknown name, a
  `no_env` parent without `noEnv: false`) is refused before any stop, resize or copy. Tests:
  `TestR44CompatResumeTypeWithEnvironmentOnNoEnvRun`,
  `TestR44CompatForkTypeWithEnvironmentOfNoEnvParent`.

### boat-api: revocation of open streams, guest SSH lookups, and legacy tokens

- **Open streams follow credential revocation.** `boat-api` authenticates a request once, so a
  long-lived response would otherwise outlive its key. Every streaming request now carries a derived
  context and re-resolves its principal against the live store every 3 seconds
  (`streamRecheckEvery`, `stream_revoke.go`). When the key is deleted, rotated past its overlap
  window, expired, or its user leaves the team (or the key's scope for the route is gone), the
  context is cancelled, the upstream boatd call is aborted, and the client's stream ends within about
  one interval. A request is treated as a stream when its route is one of: `POST /v1/vms/{id}/exec`,
  `POST /v1/vms/{id}/prompt`, `POST /v1/vms/{id}/prompts`, `GET /v1/events`, the log routes
  (`GET /v1/vms/{id}/commands/{cid}/logs`, `GET /v1/vms/{id}/prompts/{pid}/logs`), file transfers
  (`GET|PUT /v1/vms/{id}/files`, snapshot downloads and imports), the compat routes
  `POST /api/v1/sandboxes/{id}/commands`, `GET /api/v1/sandboxes/{id}/commands/{pid}/stream`,
  `GET …/commands/{cid}/logs`, `GET …/command-history/{cid}/logs`, `GET /api/v1/sandboxes/{id}/events`,
  `POST /api/v1/sandboxes/{id}/prompt`, `GET|PUT …/files`, `GET …/artifacts`; or any request with
  `Accept: text/event-stream`, an `Upgrade` header, or `?follow=` / `?stream=` set to `1`/`true`.
  Bytes already delivered cannot be recalled, and a command already started in the VM keeps running;
  revocation cuts the client's view of it, not the process.
- **Signed download links follow revocation too.** The unauthenticated snapshot link
  (`GET /v1/snapshot-links/{code}/download`) and the compat file download URL re-check, on the same
  3-second tick (`watchAlive`, `stream_revoke.go`), that the share is still active
  (`shareActiveIn`: not expired or deleted, minting key alive at its generation) or that the minting
  key is still alive (`keyAliveGenIn`). A revoked share or key ends a transfer already in progress
  instead of only blocking new ones.
- **`POST /v1/vms/{id}/ssh/authorize` (guest SSH lookup, unauthenticated route).** It is rate-limited
  per client IP, validates the VM id, and gives one `401` for an unknown VM, a malformed id, a retired
  token and a wrong bearer, so it is not a VM-existence oracle. boatd is asked about the VM only after
  the bearer verified against locally known token material: the attested boot epoch, plus, while a
  policy push for that VM is in flight, the epoch(s) that push is installing (recorded by the push
  itself, `notePendingEpoch`). An in-flight push therefore never lets an unverified bearer reach
  boatd; the decision itself still re-checks the bearer against boatd's epoch. The in-flight flag for
  `POST /v1/vms/{id}/ssh` is raised only after the ownership check, so a non-owner cannot set it. When
  throttled the route answers `429` with `Retry-After`.
- **Revoking an SSH grant** (`DELETE /v1/vms/{id}/ssh/{fingerprint}`) revalidates the caller inside the
  store transaction, so a key revoked between authentication and commit cannot revoke anything.
  Revoking a grant minted by another member needs both an owner/admin role and the `admin` scope.
- **Open gateway connections follow revocation.** `boat-gateway` checks a desktop link or private-port
  token when a request starts, but an SSE stream, WebSocket or long download can run for hours. Every
  proxied request (including upgrades) is therefore registered with what admitted it: the VM, upstream
  IP and port, whether it passed as a public port, and the revocation generation its token or link was
  verified against. After every successful poll of `/internal/route-gens` (2 s) and on a 2 s timer, the
  registry is re-checked against the current route table and generation table, and a request is
  cancelled (its context ends, which also closes the upstream leg of a WebSocket, and any hijacked
  client connection is closed) when its VM left `routes.json` or moved to another IP or is quarantined,
  its port or desktop port is gone or changed, a request that passed as a *public* port now needs a
  token, its VM's generation no longer equals the verified one (credential revocation), or the
  generation table is older than 5 s (API outage: fail closed, so open private streams are also cut
  by a boat-api outage longer than that). A request is also refused if it lapses between its check and
  registration. A request admitted by a token, link or session cookie is also cancelled once that
  credential's own expiry passes (checked on the same 2 s timer); public-port requests carry no
  credential and are unaffected. So an open stream ends within about one poll (2 s, at most two) of a revocation, not
  when the client disconnects. Bytes already sent cannot be recalled. Without `-api` (no revocation
  source) only the route-table checks apply.
- **Internal routes** (`/internal/route-gens`, `/internal/ssh/authorize`) answer only direct loopback
  peers: any `Forwarded`, `X-Forwarded-*` or `X-Real-Ip` header makes them refuse, via one shared
  helper.
- **Gateway legacy master-key tokens are off by default.** `boat-gateway -legacy-hmac-until` now
  defaults to disabled (`0` or `-1`); a start-relative default re-armed the window on every restart.
  To migrate, pass an absolute unix time once (`now + 8 days` covers the longest port URL).
- **Open jump-host sessions are ended when their authorization goes away.** sshd authorizes the
  `jump` user once per connection (`AuthorizedKeysCommand`, which issues `permitopen="<vm ip>:22"`),
  so without more an `ssh -J` session already open would survive grant revocation, API key
  deletion, member removal, VM deletion and IP reuse. Two limits apply (`components/boat/gateway/module.nix`,
  `jumpreap.go`):
  1. *Session limits* (`Match User jump`, OpenSSH 9.2 or newer): `ClientAliveInterval 15` with
     `ClientAliveCountMax 2` drops dead clients, `UnusedConnectionTimeout 120` closes a connection with
     no open channel, `ChannelTimeout direct-tcpip=30m session=30s` closes idle forwarded and session
     channels, `MaxSessions 2`. These bound idle connections; they do **not** by themselves end an
     active session.
  2. *Active termination.* `boat-gateway jump-keys` (run by sshd as `boat-ssh`) records, in
     `/run/boat-jump/<sshd pid>.json`, the sshd connection (parent pid plus its start time) and the exact
     authorized_keys lines granted for every key offered on it. It **fails closed**: if it cannot record,
     or its parent cannot be positively identified as a connection `sshd` (an `sshd` process whose own
     parent is not pid 1), it prints no keys and the authentication is denied. The only opt-out is
     `BOAT_JUMP_ALLOW_UNTRACKED=1` in the helper's environment, for manual runs and tests (sshd runs the
     helper with an empty environment and the client cannot set it). The root `boat-jump-reaper` service
     re-asks boat-api (`/internal/ssh/authorize`, the same lookup sshd made) for each recorded key every
     3 seconds, once per distinct key per pass (identical keys across sessions share one request, at most
     4 in flight, and a pass stops starting new requests after 15 s), and sends `SIGTERM` (then `SIGKILL`
     on the next pass) to that connection's sshd processes when:
     - boat-api answers **204**, meaning the authorization is gone (grant revoked, key deleted or
       expired, member removed, VM deleted). This kills on the *first* pass after the API commits it, so
       about 3 s, not after the staleness window; or
     - a `permitopen` target the connection was granted is **no longer offered** *and* boatd's
       `/run/boat/routes.json` (read by the reaper, `-routes`) still lists a running VM, or a
       quarantined one, at that address. boat-api only offers running, policy-current VMs the key is
       entitled to, so an address that is occupied but not offered means this VM's grant was revoked
       (the key may keep grants on other VMs) or the IP now belongs to another VM. A target that merely
       vanished because its VM was **stopped or deleted** is in no `routes.json` entry and never ends a
       session, and neither do targets that were only *added*: a teammate stopping an unrelated VM
       cannot kill your session. If `routes.json` cannot be read while a target was removed, the
       reaper cannot tell the cases apart and applies the `-max-stale` rule below instead.

     Transport errors, 5xx answers and malformed bodies are "cannot verify", not revocations: the session
     is kept, but only for `-max-stale` (2 minutes) since its last successful check. A dead reaper is
     detected too: every pass stamps `/run/boat-jump/reaper.alive` (start and end), and the helper
     denies authentication while that file is missing, not a regular file, or older than 30 s (the
     reaper refuses `-interval` above 10 s), with the same `BOAT_JUMP_ALLOW_UNTRACKED=1` opt-out as
     recording. Per pass the reaper scans `/proc` once, for the parent-to-children map used to identify
     and signal sessions, and re-checks a child's parent right before signalling it. The reaper treats the
     records as untrusted, because they are written by an unprivileged user: strict file names, a size
     cap, the pid must still have the recorded start time, be an `sshd` process whose parent is not pid 1
     (the listener is never signalled), and own a child process running as the `jump` user, so a forged
     record cannot be used to kill another user's `sshd`; the start time is checked again immediately
     before signalling. boat-api has nothing to push, because the reaper reads the same committed state
     that new authentications use. A client that offers several authorized keys on one connection is
     ended when *any* of them loses authorization (fail closed). Bytes already forwarded cannot be
     recalled, and a process started in the VM keeps running.
- **Tenant-planted SSH keys outlive API grant revocation.** Guest sshd reads only
  `~/.ssh/authorized_keys.tenant` (plus the managed lookup). Keys a tenant, or guest root, wrote there
  are not API grants: revoking a grant, deleting an API key or removing a member does not remove them,
  and they travel with snapshots, forks and shared snapshots. Port 22 stays unreachable from outside
  without a jump grant or a published port, so such a key is useful only to someone who can already
  reach the VM's SSH port. Treat a snapshot you share as carrying its tenant-enrolled SSH keys, and
  clear `authorized_keys.tenant` before sharing or when offboarding a member.

## Hardening checklist for owners

- Install with `--ssh-key`, or add a key to `~/.ssh/authorized_keys` on first login.
- Leave `panda-browser` off unless every local user and every job is trusted (its CDP port has no
  authentication, and jobs can reach localhost).
- Keep PandaCode Full access off unless you watch the tasks; protect the GitHub account you sign in
  with (two-factor authentication).
- If you enable `panda-agentd`: use an `https://` relay you control.
- Keep auto-updates on: security fixes only reach boxes through updates.
- Report vulnerabilities privately: [SECURITY.md](../SECURITY.md).
