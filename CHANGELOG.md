# Changelog

All notable changes to PandaOS are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `panda-install`, an offline installer (interactive, or scripted with
  `--disk --profile headless|desktop --hostname --user --password-hash [--ssh-key] --yes`).
  It lays out btrfs subvolumes `@`, `@nix`, `@home`, `@mem`, `@log`, `@snapshots`, installs
  systemd-boot, and writes a local flake at `/etc/pandaos` (`nixosConfigurations.pandaos`) that
  tracks `github:AICodeKing/pandaos`.
- Daily automatic updates with a health check and automatic rollback.
- Hourly btrfs snapshots of `/mem` and `/home`.
- `panda-agentd status` and `/run/panda-agentd/status.json`.
- Optional `services.panda-browser`: headless Chromium with CDP on `127.0.0.1:9222`.
- Desktop: a Panda dock with agent status, and the `panda-menu` repair menu.
- Release workflow: tagged `v*` builds publish both ISOs, `SHA256SUMS` and build provenance.
- Docs, MIT license, security policy, contributing guide and issue templates.

### Changed

- `panda-agentd` runs jobs sandboxed.

### Security

- No default password. v0.1.0 shipped user `panda` with the well-known password `panda`; the
  password (and optionally an ssh key) is now set at install time.
- The firewall allows only ssh, and ssh is key-only (as in 0.1.0), now documented in `docs/security.md`.

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
