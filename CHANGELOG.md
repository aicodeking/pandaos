# Changelog

All notable changes to PandaOS are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Agent CLI shims are also first on the `agent` user's login PATH (`bash -lc claude`), and every
  shimmed task raises its `oom_score_adj` by 500; the host's `host/` cgroup has `memory.oom.group=0`.
- `panda-boot-check` repairs the system profile after a fallback boot: it points
  `/nix/var/nix/profiles/system` back at the running (good) generation and makes it the bootloader
  default again, so garbage collection keeps it and the next update starts from it.
- Tests: `agent-env` runs the shim inside PandaCode's real FHS/bubblewrap environment (cgroup fs
  writable, task cgroups, OOM kill there); `boot-fallback` checks the profile repair;
  `update-rollback` checks that a shutdown during an update starts no rollback; the new `update-e2e`
  runs the real `nixos-upgrade` flake path offline against a local `release` branch (lock bump,
  build, switch, health).
- **Agent CLIs:** `pandaos.agents.clis` (claude-code, codex, gemini-cli, opencode; claude-code allowed
  by name, no global allowUnfree) and `sudo panda-agents-install <cli>...`, which writes
  `/etc/pandaos/agents.nix` and applies it through `panda-update`. The installer's `local.nix` imports
  that file.
- **First boot runs once on every box**, installed ones included: an agent-CLI picker and the
  PandaCode Connect offer on tty8 (plus the password step on boxes without `local.nix` and without a
  password), then `/var/lib/pandaos/firstboot-done`.
- **Per-task cgroups:** `pandacode-host` gets a delegated cgroup. `pandacode-host-enter` puts the host
  in an oomd-avoided `host/` leaf, and PATH shims for the agent CLIs (`pandaos.agents.wrapCommands`)
  give every run its own `task-*` cgroup, so oomd kills one task, not the host.
  `sudo panda-connect restart`.
- **panda-ckpt:** `init`/`adopt`/`discover` (CLI and MCP), checkpoints of git checkouts outside
  `~/work` (reflink copies in `~/.checkpoints/_ext`, restored in place), `list --limit`, and MCP tool
  annotations.
- **panda-mem:** `mem_write` refuses to replace an existing title without `overwrite`; new
  `mem_append`, `mem_delete` (with confirm) and `mem_log_episode` (the end-of-task episode
  convention), and tool annotations. The three MCP servers share one `mcpserve.go`.
- **panda-sys:** `sys_updates action=run` starts `nixos-upgrade.service` through a polkit rule for the
  group `panda-sys-update` (only with `allowMutations`), instead of `sudo`.
- **Snapshots of the agent:** `btrbk-agent` snapshots `/var/lib/agent` (now a btrfs subvolume),
  `~/work` and each workspace (`/.snapshots/agent-workspaces/`).
- Tests: `boot-fallback` (UEFI one-shot trial falls back to the last good generation),
  `panda-mem-tools`, `agent-clis`; `agent-env` checks per-task oomd kills, `panda-ckpt` external
  repos, and `panda-sys` the polkit path. The install test checks the first boot runs once and the
  agent snapshots. CI runs the `panda-agents-install` and desktop shell tests and the closure script
  tests, retries Nix on cache throttling, and idle RAM is a median of 3 samples.
- **Agent runtime** (`pandaos.agents`, on by default): `pandacode-host` and the agent CLIs it starts
  run in `agents.slice` (MemoryMax 75%, no MemoryHigh throttle, MemorySwapMax 10%, CPUWeight 50,
  TasksMax 8192) with systemd-oomd killing inside the slice under pressure; system daemons get
  `OOMScoreAdjust=-500`. `/etc/panda/AGENTS.md` briefs agents on the box.
- **MCP registry** `/etc/panda/mcp.d/<name>.json` (`pandaos.agents.mcpServers`), and `panda-agents`
  (`mcp list`, `mcp sync`, `sync`, `doctor`), which merges it into Claude Code, Codex, opencode,
  Gemini CLI and Kimi CLI configs as `panda-*` entries; synced at boot and after rebuilds.
- **Checkpoints:** `panda-ckpt` (on by default) keeps instant btrfs checkpoints of `~/work/<name>`
  workspaces: create, list, diff, restore (with a safety checkpoint), fork, prune, auto-checkpoints
  every 10 min, and an MCP server. See [docs/checkpoints.md](docs/checkpoints.md).
- **`panda-sys`** (on by default): an MCP server for box status (status, services, logs, top, net,
  ports, updates, checkpoint space). Read-only unless `services.panda-sys.allowMutations` is set.
- `services.panda-browser.mcp` registers a browser MCP server in `mcp.d`.
- Desktop: `panda-menu` → *Checkpoints* (list, create, restore, fork) and *Agents doctor*.
- VM tests `panda-ckpt`, `agent-env` and `panda-sys`; the install test checks checkpoints on the
  installed btrfs disk. CI runs `go vet` and `go test -race` for every Go component.
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

- oomd no longer kills on memory pressure (`ManagedOOMMemoryPressure` removed from `pandacode-host`):
  it ranks by reclaim rate and picked an idle bystander. The kernel's cgroup OOM killer at the
  slice's `MemoryMax` kills the offending task; oomd acts only on swap exhaustion.
- Installed systems (`lib.mkPandaSystem`) set `system.disableInstallerTools`, keeping
  `nixos-rebuild` and `nixos-version`. The live ISO keeps the installer tools. This saves only about
  1 MB (headless 1691 to 1690 MB; budgets 1859 / 2346 MB). perl (55 MB) stays for the activation
  scripts, and python3 (127 MB) stays for systemd-boot's installer.
- **Agents run as the new non-wheel system user `agent`** (home `/var/lib/agent`), not as `panda`.
  Existing boxes must sign in again (`sudo panda-connect sign-in`) and log their CLIs in as `agent`;
  see [troubleshooting](docs/troubleshooting.md). `services.panda-ckpt.users` now defaults to
  `[ "agent" ]`.
- Rebuilds no longer restart `pandacode-host` (`restartIfChanged = false`), so updates do not kill
  running tasks. A new host version applies at reboot or `panda-connect restart`.
- Closure cuts: the nixpkgs source is no longer pinned into the system (`nix-shell -p` and
  `<nixpkgs>` no longer resolve; use `nix shell nixpkgs#...`), and NetworkManager is built without
  ModemManager. That NM is not on cache.nixos.org, so CI and each box build it once (about 5 to 10 min).
  Closures went from 1971 / 2413 MB to 1691 / 2133 MB (headless / desktop), the ISOs from 901 / 1020 MB to
  855 / 975 MB. Budgets are now 1860 / 2346 MB (measured + 10%). python3 (127 MB, systemd-boot's
  installer) and perl (55 MB, activation) remain.
- `agents.slice` no longer has a `MemoryHigh` throttle (`pandaos.agents.memoryHigh = "infinity"`):
  throttling stalled the hog, so oomd picked an idle task instead. The kernel now kills the biggest
  task at `MemoryMax` (75%).
- `panda-menu` uses the configured NetworkManager for nmtui, so the desktop no longer pulls the stock
  NetworkManager with ModemManager/libqmi.
- `panda-update` does not start a rollback while the box is shutting down; the boot-time one-shot
  trial handles it.
- `/etc/panda/mcp.json` is now generated from `mcp.d` (compat); panda-mem registers in `mcp.d`.
- The btrfs mounts carry `user_subvol_rm_allowed`, so `panda-ckpt` can delete checkpoints as the
  owner. Existing installs pick it up on the next rebuild (`disk.nix` overrides only devices).
- PandaCode Connect runs in a slim FHS environment: the headless closure went from 2834 to 1971 MB,
  desktop from 2915 to 2413 MB, the ISOs from 1137/1160 to 901/1020 MB. Closure budgets are now
  2168 / 2654 MB (measured + 10%). `useSlim = false` in `components/pandacode-host/package.nix`
  restores the default environment.
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

- `panda-sys`'s only mutating tool (`sys_updates action=run`) needs `allowMutations`, `confirm: true`,
  a root or wheel caller and a passwordless sudo rule PandaOS does not ship; non-wheel callers read
  only PandaOS units' logs. See [docs/security.md](docs/security.md#mcp-tools-and-their-permissions).
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
