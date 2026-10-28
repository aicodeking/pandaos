# Security model

## Threat model

PandaOS assumes a **single-tenant box**: one owner, on their own hardware, running agent jobs that
the owner (through their relay) chose to send. It is not a multi-user or hostile multi-tenant system.

What we defend against:

- **The network.** Nothing listens except ssh, and ssh needs a key.
- **Default credentials.** There are none (new in Unreleased).
- **A misbehaving job** damaging the system, the agent daemon or other users' files.
- **A bad update** leaving the box broken: it is rolled back automatically.

What we do not defend against:

- Someone with physical access. There is no disk encryption by default.
- A malicious or compromised relay. The relay decides which jobs run, so it can run any command the
  job user is allowed to run. Pair only with a relay you trust.
- Anything you run yourself as your login user, which is in `wheel` and can use `sudo`.

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

## What jobs can and cannot touch

The daemon (`panda-agentd`) runs as root under a systemd sandbox (read-only system, no access to
`/home`, private `/tmp` and devices, no kernel tuning). It runs as root only so it can start job units.

Each job runs in its **own transient systemd unit** as the unprivileged system user `panda-agent`:

| Jobs can | Jobs cannot |
|---|---|
| run programs installed on the system | run as root or as your login user, or gain privileges (`NoNewPrivileges`) |
| use the network (outbound) | read the agent's pairing token |
| write their own work directory `/var/lib/panda-agentd/jobs/<id>` and `/mem` | write anywhere else: the system is read-only to them (`ProtectSystem=strict`), `/home` is read-only (`ProtectHome=read-only`), and `/tmp` is private |
| use memory, CPU, tasks and time up to their limits | go past `MemoryMax` (default 80% of RAM), `CPUQuota` (default 200%), `TasksMax=512` or `RuntimeMaxSec` (default 1 hour) |

Output kept per job is capped at 64 KiB for stdout and for stderr.

`/mem` is owned by the `panda-mem` group. Your login user and the job user `panda-agent` are members,
so **jobs can change shared memory**. Every change is a git commit, and `/mem` is snapshotted hourly.

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
- Leave `panda-browser` off unless every local user is trusted (its CDP port has no authentication).
- Use an `https://` relay you control.
- Keep auto-updates on: security fixes only reach boxes through updates.
- Report vulnerabilities privately: [SECURITY.md](../SECURITY.md).
