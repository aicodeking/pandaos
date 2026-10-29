# PandaOS

A lightweight, agent-first Linux built on NixOS. Install it on an old PC and it becomes an agent box:
it connects through PandaCode Connect, so the PandaCode app on your other devices can start, watch and
approve agent tasks on it.

- **Agents are kept apart from you.** They run as a separate non-wheel user, `agent`, that cannot touch
  your home or use `sudo`.
- **A runaway task dies alone.** Each agent run gets its own cgroup in a capped `agents.slice`, so
  the kernel's cgroup OOM killer kills the offending task, not the host, the other tasks or the box.
  systemd-oomd steps in only when swap runs out.
- **Mistakes are one restore away.** panda-ckpt checkpoints agent workspaces and git checkouts, and
  root-owned hourly btrfs snapshots back them up where agents cannot delete them.
- **Shared memory.** `/mem` is a git-backed store with an episode log, so every agent learns from
  the last one.
- **Wired for MCP.** Every MCP server on the box is synced into the agent CLIs you pick at first boot.
- **Self-updating.** It updates itself every day and rolls back if an update breaks it.

## Measured numbers

These come from the NixOS VM tests and ISO builds in CI on nixos-26.05 (`nix flake check`; see
[docs/benchmarks.md](docs/benchmarks.md) for how each one is measured). Sizes are MiB. They are
PandaOS's own numbers in a VM, not a comparison, and **not yet measured on real hardware**. Idle RAM
is the median of 3 readings.

| | headless | desktop |
|---|---|---|
| Idle RAM, `free` used + shmem | 115 MB (with the optional agentd on) | 175 MB in a logged-in labwc session (172 MB at the greeter) |
| Idle RAM, `MemTotal - MemAvailable` | 235 MB | 330 MB in session (328 MB at the greeter) |
| Closure of the installed system (`nixosConfigurations`, incl. curated firmware) | 1.69 GB | 2.13 GB |
| ISO size (the desktop ISO also carries the headless system) | 855 MB | 975 MB |

| Optional agentd job round trip (pair, then run `echo hi`) | |
|---|---|
| Normal VM network | 3.7 s |
| 1 Mbit/s, ~340 ms RTT, 1% loss each way (netem on both nodes) | 6.1 s |
| Offline install to an empty disk (`panda-install --yes`, VM) | 98 s |
| A panda-mem-only update (uncompressed bytes substituted) | 8 MB |

PandaCode Connect is the pinned PandaCode AppImage (289 MB extracted) in a slim FHS environment that
holds only the libraries the AppImage expects from the host. With `appimageTools`' default
environment, the closures were 2.83 GB and 2.92 GB and the ISOs 1137 MB and 1160 MB. Before
PandaCode Connect, they were 1.64 GB and 2.11 GB, and the ISOs 803 MB and 929 MB. Dropping the pinned
nixpkgs source and ModemManager then took the closures from 1.97 / 2.41 GB to 1.69 / 2.13 GB. Agent
CLIs are not included: each one you pick adds 200 to 520 MB.

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
5. After the first boot, with a network: run `sudo panda-connect sign-in`, open the GitHub link it
   prints, and approve the box with the GitHub account you use in PandaCode. The box then shows up
   as a computer in PandaCode. On the desktop, the dock's *PandaCode* button does the same.

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
     your devices (PandaCode app) <-> PandaCode Connect
                 ^  outbound only, encrypted between devices
                 |
  +--------------+--------------------------------------------------+
  | agents.slice  (MemoryMax 75%, CPUWeight 50; kernel cgroup OOM)  |
  |   pandacode-host  `pandacode serve` as user `agent` (not wheel) |
  |     host/         the host itself, oomd_avoid                   |
  |     task-*        one cgroup per agent run (CLI shims);         |
  |                   the OOM killer takes a task, never the host   |
  +-----------------------------------------------------------------+
  | panda-mem      /mem (markdown + git), MCP; single writer        |
  | panda-ckpt     ~/work + repo checkpoints, forks, MCP            |
  | panda-sys      box status MCP; updates only via a polkit group  |
  | mcp.d          MCP registry, synced into agent CLIs             |
  | agent CLIs     claude-code/codex/gemini-cli/opencode, opt-in    |
  | panda-agentd   optional, off: generic relay job runner          |
  | panda-browser  optional headless Chromium, CDP :9222            |
  | desktop        optional labwc + Panda dock + panda-menu         |
  +-----------------------------------------------------------------+
  NixOS 26.05 | btrfs (@ @nix @home @mem @log @snapshots) | systemd-boot
  /var/lib/agent subvolume, hourly root snapshots (btrbk) incl. workspaces
  /etc/pandaos flake -> github:AICodeKing/pandaos/release, daily update + rollback
```

| Path | What |
|---|---|
| `flake.nix` | ISOs, VM, NixOS configs, `lib.mkPandaSystem`, checks (`flake.lock` pins nixpkgs) |
| `nixos/modules` | base, security, updates, first boot, snapshots, disk layout, ISO |
| `nixos/profiles` | `headless` and `desktop` (labwc) |
| `nixos/installer` | `panda-install`, the offline installer |
| `components/pandacode-host` | PandaCode's headless host (`pandacode serve`) and `panda-connect` |
| `components/agentd` | optional `panda-agentd`: pairs with a relay (dials out) and runs jobs |
| `components/mem` | `panda-mem`: the `/mem` store and MCP server |
| `components/agent-env` | `agents.slice`, per-task cgroup shims, OOM settings, the `/etc/panda/mcp.d` registry, `panda-agents`, `/etc/panda/AGENTS.md` |
| `components/agent-clis` | `pandaos.agents.clis` and `panda-agents-install` |
| `components/checkpoint` | `panda-ckpt`: btrfs workspace checkpoints, forks, auto-checkpoints, MCP |
| `components/sysmcp` | `panda-sys`: box status/ops MCP server |
| `components/browser` | optional `panda-browser` (headless Chromium, CDP on localhost) |
| `components/desktop` | labwc/waybar config, `panda-menu`, `panda-connect-status` |
| `tests/` | NixOS VM tests and RAM/disk budgets |
| `bench/` | metric collection |

## Docs

- [Install](docs/install.md)
- [Updates and rollback](docs/updates-and-rollback.md)
- [Security model](docs/security.md)
- [Agents: runtime, MCP registry, PandaCode Connect, memory, browser, agentd](docs/agents.md)
- [Checkpoints](docs/checkpoints.md)
- [Desktop](docs/desktop.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Benchmarks](docs/benchmarks.md)
- [Development](docs/development.md)
- [Changelog](CHANGELOG.md) · [Contributing](CONTRIBUTING.md) · [Security policy](SECURITY.md)

## Known limitations / owner actions

- **Connects through PandaCode Connect.** Sign in once with `panda-connect sign-in` (GitHub device
  flow) and approve the box in PandaCode. That sign-in is not tested in CI (it needs GitHub); the VM
  tests cover the host up to it. The older `panda-agentd` is off by default and still speaks a
  placeholder relay protocol.
- **No binary cache yet.** Until the owner sets up a cache and `pandaos.binaryCache`, boxes build
  panda-mem and the other PandaOS packages locally on every update that changes them.
  See [docs/updates-and-rollback.md](docs/updates-and-rollback.md#what-an-update-downloads-and-builds).
- **No release tag yet.** There are no downloadable ISOs, and the `release` branch that installed
  boxes update from only appears with the first tag. Build from source meanwhile.
- **UEFI only**, with Secure Boot off.
- **Not yet tested on real hardware.** All numbers and tests come from VMs.

Everything new since v0.1.0 is listed under Unreleased in the [changelog](CHANGELOG.md).

## License

[MIT](LICENSE)
