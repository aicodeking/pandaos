# PandaOS

A lightweight, agent-first Linux built on NixOS. Install it on an old PC and it becomes an agent box:
it dials out to a relay, runs sandboxed agent jobs, keeps shared agent memory in `/mem`, updates itself
every day and rolls back if an update breaks it.

## Measured numbers

These come from the NixOS VM tests and ISO builds in CI on nixos-26.05 (`nix flake check`; see
[docs/benchmarks.md](docs/benchmarks.md) for how each one is measured). Sizes are MiB. They are
PandaOS's own numbers in a VM, not a comparison, and **not yet measured on real hardware**.

| | headless | desktop |
|---|---|---|
| Idle RAM, `free` used + shmem | 128 MB | 138 MB at the login screen (greeter); logged-in labwc session: pending CI |
| Idle RAM, `MemTotal - MemAvailable` | 247 MB | 279 MB at the greeter; logged-in session: pending CI |
| Closure of the test VM system | 1.32 GB | 2.85 GB |
| Closure of the installed system (`nixosConfigurations`) | pending CI | pending CI |
| ISO size (the desktop ISO also carries the headless system) | 608 MB | 993 MB |

| Agent job round trip (pair, then run `echo hi`) | |
|---|---|
| Normal VM network | 3.7 s |
| 1 Mbit/s, 300 ms delay, 2% loss (netem, box's outgoing traffic only) | 8.6 s |
| The same, shaped in both directions | pending CI |
| Offline install to an empty disk (`panda-install --yes`, VM) | 208 s |

A side-by-side comparison with Ubuntu and Debian on identical VMs is **planned, not built yet**. Until it
exists, we make no claims about how PandaOS compares with other systems.

## Quick start

**The first release is pending:** there is no release tag yet, so there are no ISOs to download.
Until then, build the ISO from source (on x86_64 Linux with Nix, flakes enabled):

```sh
git clone https://github.com/AICodeKing/pandaos && cd pandaos
nix build .#iso-headless          # or .#iso-desktop; the ISO is in result/iso/
```

Once a release exists, download `pandaos-<version>-headless-x86_64-linux.iso` (or `-desktop-`) and
`SHA256SUMS` from [Releases](https://github.com/AICodeKing/pandaos/releases) and check it with
`sha256sum -c --ignore-missing SHA256SUMS`.

1. Write the ISO to a USB stick (this erases the stick; check the device name with `lsblk`):
   ```sh
   sudo dd if=result/iso/<name>.iso of=/dev/sdX bs=4M status=progress conv=fsync
   ```
2. In the firmware setup, **boot in UEFI mode and disable Secure Boot**. BIOS/legacy boot is not supported.
3. Boot the target machine from the stick.
4. Run `sudo panda-install` and answer the questions. No network is needed. See [docs/install.md](docs/install.md).

There is no default password: you set one (and optionally an ssh key) during the install.

## Hardware

- x86_64 PC that boots in **UEFI mode, with Secure Boot off**. The installer uses systemd-boot; BIOS
  is not supported. ARM is not built yet.
- RAM: the measured idle use is above. Leave room for your agent jobs on top of it. The tests run
  the headless profile in the default 1 GB VM and the desktop profile in a 2 GB VM.
- Disk: at least 8 GiB; the whole target disk is erased. Leave room for snapshots, your data and
  updates on top of the system closure.
- Network: wired or wifi (NetworkManager; `nmtui` for wifi). A curated firmware set is installed so
  common wifi chips work without a network (new in Unreleased). The network is only needed after install.
- Not yet tested on real hardware: so far everything is tested in QEMU VMs.

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
  /etc/pandaos flake -> github:AICodeKing/pandaos/release, daily update + rollback
```

| Path | What |
|---|---|
| `flake.nix` | ISOs, VM, NixOS configs, `lib.mkPandaSystem`, checks (`flake.lock` pins nixpkgs) |
| `nixos/modules` | base, security, updates, first boot, snapshots, disk layout, ISO |
| `nixos/profiles` | `headless` and `desktop` (labwc) |
| `nixos/installer` | `panda-install`, the offline installer |
| `components/agentd` | `panda-agentd`: pairs with the relay (dials out) and runs jobs |
| `components/mem` | `panda-mem`: the `/mem` store and MCP server |
| `components/browser` | optional `panda-browser` (headless Chromium, CDP on localhost) |
| `components/desktop` | labwc/waybar config, `panda-menu`, `panda-agents-status` |
| `tests/` | NixOS VM tests and RAM/disk budgets |
| `bench/` | metric collection |

## Docs

- [Install](docs/install.md)
- [Updates and rollback](docs/updates-and-rollback.md)
- [Security model](docs/security.md)
- [Agents: agentd, jobs, memory, browser](docs/agents.md)
- [Desktop](docs/desktop.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Benchmarks](docs/benchmarks.md)
- [Development](docs/development.md)
- [Changelog](CHANGELOG.md) · [Contributing](CONTRIBUTING.md) · [Security policy](SECURITY.md)

## Known limitations / owner actions

- **PandaConnect is undefined.** `panda-agentd` speaks a placeholder relay protocol, and the default
  relay URL is a placeholder (`https://connect.pandacode.invalid`). Point it at your own relay with
  `panda-install --relay URL`.
- **No binary cache yet.** Until the owner sets up a cache and `pandaos.binaryCache`, boxes build
  panda-agentd, panda-mem and the other PandaOS packages locally on every update that changes them.
  See [docs/updates-and-rollback.md](docs/updates-and-rollback.md#what-an-update-downloads-and-builds).
- **No release tag yet.** There are no downloadable ISOs, and the `release` branch that installed
  boxes update from only appears with the first tag. Build from source meanwhile.
- **UEFI only**, with Secure Boot off.
- **Not yet tested on real hardware.** All numbers and tests come from VMs.

Everything new since v0.1.0 is listed under Unreleased in the [changelog](CHANGELOG.md).

## License

[MIT](LICENSE)
