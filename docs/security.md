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
| Inbound | Firewall on. Only ssh is open. ssh: keys only, no passwords, no root login. |
| Outbound | `panda-agentd` dials out to the relay. Nothing has to be opened or forwarded for agents. |
| Local only | `panda-browser`, if enabled, serves CDP on `127.0.0.1:9222` only. |

> v0.1.0 already had the firewall on with only ssh open and ssh key-only, but it shipped the user
> `panda` with the password `panda`. Setting credentials at install time is **new in Unreleased**.

**The agent channel is outbound only.** `panda-agentd` makes HTTP(S) requests to the relay URL:
it pairs, sends heartbeats, long-polls for the next job and posts results. The relay never connects
to the box. The relay protocol is a **placeholder** until PandaConnect is defined, and the default
relay URL is a placeholder (`https://connect.pandacode.invalid`). Use an `https://` relay URL in production.

The pairing token is stored in `/var/lib/panda-agentd/token`, in a directory only root can read.
If the relay rejects the token, the daemon throws it away and pairs again.

## What jobs can and cannot touch

The daemon (`panda-agentd`) runs as root under a systemd sandbox (read-only system, no access to
`/home`, private `/tmp` and devices, no kernel tuning). It runs as root only so it can start job units.

Each job runs in its **own transient systemd unit** as the unprivileged system user `panda-agent`:

| Jobs can | Jobs cannot |
|---|---|
| run programs installed on the system | run as root or as your login user |
| use the network (outbound) | read the agent's pairing token |
| use memory and time up to their limits | go past their `MemoryMax` (if the job sets one) or `RuntimeMaxSec` (job timeout, default 1 hour) |
| write where the `panda-agent` user has Unix permission | write to other users' files, `/etc` or the Nix store |

Output kept per job is capped at 64 KiB for stdout and for stderr.

`/mem` is owned by the `panda-mem` group. Your login user is a member; `panda-agent` is not, so jobs
cannot change shared memory directly.

> **New in Unreleased:** jobs run sandboxed. Treat the table above as the v0.1.0 baseline; the
> sandbox restricts jobs further.

## Hardening checklist for owners

- Install with `--ssh-key`, or add a key to `~/.ssh/authorized_keys` on first login.
- Use an `https://` relay you control.
- Keep auto-updates on: security fixes only reach boxes through updates.
- Report vulnerabilities privately: [SECURITY.md](../SECURITY.md).
