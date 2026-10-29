# Changelog

All notable changes to PandaOS are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **PandaCode Connect:** `services.pandacode-host` (on by default) runs PandaCode's headless host
  (`pandacode serve`, pinned release AppImage, under `xvfb-run`) as `panda`, so the box shows up as a
  computer in the PandaCode app. `panda-connect sign-in|sign-out|status [--json]|full-access on|off|logs`
  manages it; Full access is off by default. Until signed in the service is skipped and the health check
  ignores it. First boot offers `panda-connect sign-in` when a network is up. Desktop: a *PandaCode*
  top-bar module and a *PandaCode Connect* menu in `panda-menu`.
- `pandacode-host` VM test: the AppImage runs, an unsigned box stays idle and healthy, `full-access`
  works, and a stale sign-in marker is dropped instead of crash-looping. Real sign-in needs GitHub and
  is not tested in CI.

- `panda-install`, an offline installer (interactive, or scripted with
  `--disk --profile headless|desktop --hostname --user --password-hash [--ssh-key] [--timezone] [--relay URL] --yes`).
  It lays out btrfs subvolumes `@`, `@nix`, `@home`, `@mem`, `@log`, `@snapshots`, installs
  systemd-boot, writes filesystem UUIDs to `/etc/pandaos/disk.nix` (mounts by UUID), and writes a
  local flake at `/etc/pandaos` (`nixosConfigurations.pandaos`) with a **pinned `flake.lock`**, so
  rebuilds work offline right after install, and `/etc/pandaos/VERSION`. The desktop ISO defaults
  to the desktop profile. `--relay` writes `/var/lib/panda-agentd/relay`.
- **Release channel:** installed boxes track `github:AICodeKing/pandaos/release`, which
  `release.yml` moves only after a tagged release passes all checks.
- Daily automatic updates with a health check (`panda-health`) and automatic rollback.
- `panda-update run|hold|unhold|rollback|status`. The hold file is `/var/lib/pandaos/update-hold`; rolling
  back from `panda-menu` sets it.
- Boot assessment: a new generation is tried once (one-shot boot entry) and becomes the default only
  after `panda-boot-check` finds it healthy; a bad boot falls back to the last known-good generation.
- Nix `min-free`/`max-free` (1 GiB / 3 GiB): the store collects garbage by itself when disk space runs low during builds.
- A curated firmware set, so common wifi chips work offline right after install.
- `pandaos.binaryCache.url` / `.publicKey` options for a PandaOS binary cache (no cache exists yet; owner action).
- Hourly btrfs snapshots of `/mem` and `/home`; `panda-rollback-mem` restores `/mem`.
- `panda-agentd status` and `/run/panda-agentd/status.json`, with `pairing`, `lastError` and
  `lastErrorAt`.
- `panda-agentd` job cancel (via the relay's heartbeat reply), `keepWorkdir`, a per-job `cpuQuota`
  option (default `200%`), `-min-free-mb` (default 1024: refuse jobs on a nearly full disk),
  `-relay-file`, and reports of jobs interrupted by a daemon restart.
- First boot asks for the `panda` password on tty8 when none is set (`pandaos.firstboot.enable`).
- Optional `services.panda-browser`: headless Chromium with CDP on `127.0.0.1:9222`.
- Desktop: a Panda dock, the `panda-menu` repair menu (network, disks, logs,
  PandaCode Connect, memory browser, `/mem` restore, updates, rollback, lock), screen lock and idle lock
  (swaylock, swayidle), notifications (fnott), volume and brightness keys, and a
  `pandaos.desktop.gpu` option.
- `flake.lock` is committed, and CI fails without it. A `lock` workflow regenerates it.
- VM tests: `install` (installs to an empty disk, then boots it), `security`, `browser`, job cancel,
  desktop scripts, and snapshots; `closures` and `mem-security` are being added.
- Release workflow: tagged `v*` builds publish both ISOs, `SHA256SUMS` and build provenance, then
  move the `release` branch. Optional Cachix push in CI and release.
- Docs (including `docs/desktop.md`), MIT license, security policy, contributing guide and issue templates.

### Changed

- `panda-agentd` is **off by default** (PandaCode Connect replaces it). `panda-install --relay URL`
  enables it. Tests that use it enable it explicitly; the closure check's simulated update is now a
  new `panda-mem`.

- `panda-agentd` runs jobs sandboxed (`ProtectSystem=strict`, read-only `/home`, private `/tmp`
  and devices, `ProtectProc=invisible`, `SystemCallFilter=@system-service`, `NoNewPrivileges`,
  `TasksMax=512`, default `MemoryMax` 80% of RAM).
- `panda-agentd` is no longer restarted by updates (`restartIfChanged = false`), so updates do not
  kill running jobs.
- `panda-agentd` requires an `https://` relay. `services.panda-agentd.allowInsecure` permits http to
  local hosts only.
- `panda-mem` is a single-writer service (`panda-mem.service`, own system user, socket
  `/run/panda-mem/sock`). `/mem` is no longer group-writable; clients and jobs go through the socket.
  Writes are serialised across processes with a lock. Search is multi-term, ranked by distinct terms matched.
- Desktop: GPU acceleration is off by default (pixman software rendering, about 800 MB smaller);
  enable it with `pandaos.desktop.gpu = true`.
- The ISOs are built on nixpkgs' bare `iso-image.nix`, not `installation-cd-minimal.nix`, and carry
  the prebuilt systems for the offline install. The headless ISO went from 1.4 GB in v0.1.0 to 608 MiB.
  The desktop ISO is 993 MiB.
- CI: every action pinned to a commit SHA; least-privilege `permissions` per job.
- Benchmarks: desktop RAM is labelled as measured at the greeter; the slow-network result is labelled
  as shaped in one direction only. Installed-system closures, a logged-in desktop session and
  two-way netem are pending CI.

### Security

- No default password. v0.1.0 shipped user `panda` with the well-known password `panda`; the
  password (and optionally an ssh key) is now set at install time.
- Jobs could plant git hooks or config in the group-writable `/mem` repo and get code run as another
  user. Fixed by the single-writer `panda-mem` service and by running git with hooks and
  non-allow-listed config refused.
- The pairing code is no longer in the world-readable `status.json`; it is only in
  `/run/panda-agentd/pairing-code` (0640 root:wheel) and the journal.
- The firewall allows only ssh, and ssh is key-only (as in 0.1.0).
- nftables firewall backend, `sshguard`, `MaxAuthTries 3`, kernel sysctl hardening, and `sudo`
  limited to `wheel`.

## [0.1.0]

The first version (v0).

### Added

- NixOS 26.05 flake with `headless` and `desktop` (labwc, greetd/tuigreet) profiles,
  `iso-headless`, `iso-desktop` and `vm-headless` packages.
- `panda-agentd`: dials out to a relay (placeholder protocol), pairs with a code, heartbeats,
  and runs jobs as the `panda-agent` user in transient systemd units with memory and time limits.
- `panda-mem`: shared agent memory at `/mem` (markdown in git), a CLI and an MCP server
  registered in `/etc/panda/mcp.json`.
- Lean base: zram swap, tmpfs `/tmp`, 50 MB journal cap, weekly Nix GC, a repair-kit package set.
- NixOS VM tests for boot, idle RAM and closure budgets, the agent round trip, a slow-network
  round trip and `panda-mem`; CI and a nightly metrics job.

### Known issues

- The `panda` user has the default password `panda`. Change it with `passwd`. Fixed in Unreleased.

[Unreleased]: https://github.com/AICodeKing/pandaos/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/AICodeKing/pandaos/releases/tag/v0.1.0
