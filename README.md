# PandaOS

A lightweight, agent-first Linux built on NixOS. Install it on an old PC and it becomes an agent box:
it dials out to a relay, runs sandboxed agent jobs, keeps shared agent memory in `/mem`, updates itself
every day and rolls back if an update breaks it.

## Measured numbers

These come from the NixOS VM tests in CI on nixos-26.05 (`nix flake check`, see [docs/benchmarks.md](docs/benchmarks.md)
for how each one is measured). They are PandaOS's own numbers, not a comparison.

| | headless | desktop |
|---|---|---|
| Idle RAM, `free` used + shmem | 117 MB | 135 MB |
| Idle RAM, `MemTotal - MemAvailable` | 235 MB | 274 MB |
| System closure | 1.27 GB | 2.8 GB |

| Agent job round trip (pair, then run `echo hi`) | |
|---|---|
| Normal VM network | 4.4 s |
| 1 Mbit/s, 300 ms delay, 2% loss (netem) | 5.4 s |

A side-by-side comparison with Ubuntu and Debian on identical VMs is **planned, not built yet**. Until it
exists, we make no claims about how PandaOS compares with other systems.

## Quick start

1. Download `pandaos-<version>-headless-x86_64-linux.iso` (or `-desktop-`) and `SHA256SUMS` from
   [Releases](https://github.com/AICodeKing/pandaos/releases), then check it: `sha256sum -c --ignore-missing SHA256SUMS`.
2. Write it to a USB stick (this erases the stick; check the device name with `lsblk`):
   ```sh
   sudo dd if=pandaos-<version>-headless-x86_64-linux.iso of=/dev/sdX bs=4M status=progress conv=fsync
   ```
3. Boot the target machine from the stick (UEFI).
4. Run `sudo panda-install` and answer the questions. No network is needed. See [docs/install.md](docs/install.md).

There is no default password: you set one (and optionally an ssh key) during the install.

## Hardware

- x86_64 PC that boots in UEFI mode (the installer uses systemd-boot). ARM is not built yet.
- RAM: the measured idle use is above. Leave room for your agent jobs on top of it. The tests run
  the headless profile in the default 1 GB VM and the desktop profile in a 2 GB VM.
- Disk: the whole target disk is erased. The installed system is the closure above plus the
  btrfs snapshots and your data, so give it a disk well beyond the closure size.
- Network: wired or wifi (NetworkManager; `nmtui` for wifi). Only needed after install.

## Architecture

```
            relay (PandaConnect, placeholder protocol)
                 ^  outbound HTTPS only
                 |
  +--------------+------------------------------------------+
  | panda-agentd   pairs, heartbeats, pulls jobs            |
  |   '-> jobs     transient systemd units, user panda-agent|
  | panda-mem      /mem (markdown + git), MCP server        |
  | panda-browser  optional headless Chromium, CDP :9222    |
  | desktop        optional labwc + Panda dock + panda-menu |
  +---------------------------------------------------------+
  NixOS 26.05 | btrfs (@ @nix @home @mem @log @snapshots) | systemd-boot
  /etc/pandaos flake -> github:AICodeKing/pandaos, daily update + rollback
```

| Path | What |
|---|---|
| `flake.nix` | ISOs, VM, NixOS configs, checks |
| `nixos/` | base module, `headless` and `desktop` (labwc) profiles |
| `components/agentd` | `panda-agentd`: pairs with the relay (dials out) and runs jobs |
| `components/mem` | `panda-mem`: the `/mem` store and MCP server |
| `tests/` | NixOS VM tests and RAM/disk budgets |
| `bench/` | metric collection |

## Docs

- [Install](docs/install.md)
- [Updates and rollback](docs/updates-and-rollback.md)
- [Security model](docs/security.md)
- [Agents: agentd, jobs, memory, browser](docs/agents.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Benchmarks](docs/benchmarks.md)
- [Development](docs/development.md)
- [Changelog](CHANGELOG.md) · [Contributing](CONTRIBUTING.md) · [Security policy](SECURITY.md)

## Status

Early. The relay protocol is a placeholder until PandaConnect is defined. The installer, auto-updates,
snapshots, browser service and desktop dock are new in the Unreleased section of the [changelog](CHANGELOG.md).

## License

[MIT](LICENSE)
