# Updates and rollback

> Daily auto-updates with a health check and automatic rollback, the release channel, `panda-update`,
> boot assessment and hourly snapshots are all **new in Unreleased**. v0.1.0 does not update itself.

PandaOS is NixOS: every update builds a complete new system *generation* next to the old one. The old
one stays on disk and in the boot menu until garbage collection removes it (weekly, for generations
older than 14 days).

## The release channel

The local flake at `/etc/pandaos` tracks **`github:AICodeKing/pandaos/release`**, not `main`. The
`release` branch only moves when a `v*` tag passes the release workflow (`nix flake check`, both ISO
builds, and a published GitHub release), so boxes never update to an untested commit on `main`.

**There is no release tag yet**, so the `release` branch does not exist yet and the daily update has
nothing to fetch. Until the first release, boxes stay on the version they were installed with.

The installer writes a **pinned `flake.lock`** into `/etc/pandaos`, locking the exact PandaOS and
nixpkgs revisions the ISO was built from. So a rebuild right after install works offline, and
nothing changes until an update moves the lock. `/etc/pandaos/VERSION` records the installed
PandaOS version:

```sh
cat /etc/pandaos/VERSION
```

## The daily update

Once a day (`nixos-upgrade.timer`, with up to 1 h of random delay) the box:

1. moves the `pandaos` input of `/etc/pandaos/flake.lock` to the head of the `release` branch,
2. builds the new generation and switches to it,
3. runs a health check (`panda-health`): within 90 s, the core services must be active and, if the
   box had a default route before the update, it must have one again,
4. if the switch fails half-way or the health check fails, **rolls back automatically**: it
   reactivates the previous generation and restores the previous `flake.lock`. A failure before
   anything was activated (for example no network) only restores the lock.

It never reboots. The units only run on installed systems (those with `/etc/pandaos/flake.nix`).
The timer is skipped while an update hold is set (see [`panda-update`](#panda-update)). To turn updates off
for good, set `pandaos.updates.enable = false` in `/etc/pandaos/local.nix` and rebuild.

```sh
systemctl list-timers nixos-upgrade.timer
journalctl -u nixos-upgrade.service
```

### What an update downloads and builds

Be aware of what "building the new generation" costs on the box:

- **Without a binary cache, PandaOS's own packages build on the box.** panda-agentd, panda-mem,
  the installer and the other custom packages are not in the NixOS cache. Unless the owner has set
  up a binary cache and configured it with `pandaos.binaryCache.url` and `.publicKey`, every update that changes
  one of them compiles it locally. That needs CPU time, RAM and disk space, and on an old PC it can be
  slow. Everything that comes from nixpkgs is still downloaded from `cache.nixos.org`.
- **A nixpkgs bump downloads and evaluates nixpkgs.** When a release moves the pinned nixpkgs, the
  box downloads the nixpkgs source (about 40 MB) and evaluates it before it downloads the changed
  packages.
- If a download from a cache fails, the update fails instead of compiling that package
  (`fallback = false`). Paths that are in no cache at all are still built.
- Updates are tuned for slow links: a 5 s connect timeout and 3 download attempts per file.

There is no binary cache yet; see [development.md](development.md#binary-cache-cachix) for how the owner
sets one up.

### Disk space during updates

Nix collects garbage by itself during a build when free space drops below 1 GiB (`min-free`), and
stops once 3 GiB are free (`max-free`), so an update is less likely to fill the disk. Old generations still in the
14-day window are kept.

## Boot assessment: rollback of a bad boot

The update health check catches a generation that boots but does not work. Boot assessment catches
one that does not boot properly. nixos-26.05's systemd-boot has no boot-counting option, so PandaOS
does the same with systemd-boot's EFI variables:

- The last generation that booted healthy is the **known-good** default boot entry.
- A newly installed generation is set as a **one-shot** entry: the next boot tries it once. If the
  kernel panics or the initrd fails, the machine reboots after 10 s (`panic=10`, `boot.panic_on_fail`)
  and the firmware boots the known-good entry again.
- If the trial boot comes up, `panda-boot-check.service` runs `panda-health` for up to 120 s (core
  services active). If it passes, the entry becomes the new known-good default. If not, the box
  announces it and reboots into the known-good entry after 1 minute.

Limits: the first boot after install has no known-good entry yet, so it never reboots by itself. With
a dead disk every entry fails, so the box reboots in a loop, showing the error for about 10 s each time.
This needs a UEFI firmware that lets the OS set EFI variables. It is skipped in VM tests that boot the
kernel directly.

You can run the same check yourself: `panda-health` (add `--route` to also require a default route).

## panda-update

`panda-update` is the manual control for updates:

| Command | What it does |
|---|---|
| `sudo panda-update run` | Update now (starts `nixos-upgrade.service`), with the same health check and rollback. Refuses while a hold is set |
| `sudo panda-update hold` | Stop automatic updates (creates `/var/lib/pandaos/update-hold`) |
| `sudo panda-update unhold` | Allow automatic updates again (removes the hold file) |
| `sudo panda-update rollback` | Sets the hold, then `nixos-rebuild switch --rollback` |
| `panda-update status` | Whether a hold is set (and why), the current system, the last update's result and the next run |

**Rolling back with `panda-update rollback` or from the `panda-menu` repair menu sets the hold**, so the next daily update does not
immediately reinstall the generation you just rolled back from. Run `sudo panda-update unhold` once a
fixed release is out.

## Rolling back by hand

From a running system:

```sh
sudo panda-update rollback    # hold + nixos-rebuild switch --rollback
```

List the generations:

```sh
sudo nix-env --list-generations --profile /nix/var/nix/profiles/system
```

If the box does not boot or has no network after an update, reboot and pick an older generation in
the systemd-boot menu (press a key, or hold Space, while it starts). Once you are in, run
`sudo nixos-rebuild switch --rollback` so it stays the default.

On the desktop, the `panda-menu` repair menu offers the rollback too.

## Snapshots of `/mem` and `/home`

Every hour, `btrbk` (`btrbk-panda.timer`) takes read-only btrfs snapshots of `/mem` and `/home` into
`/.snapshots` (the `@snapshots` subvolume), named like `mem.20261028T1400`. It keeps 24 hourly and
7 daily snapshots. Snapshots are copy-on-write, so they cost disk only for data that changed.
They only run when `/.snapshots` is a mountpoint, that is, on the installer's disk layout.

System rollback (generations) and data snapshots are separate: rolling back a generation never
changes `/mem` or `/home`, and restoring a snapshot never changes the system.

To restore `/mem` as a whole, run `sudo panda-rollback-mem` to list the snapshots, then
`sudo panda-rollback-mem mem.20261028T1400`. It first snapshots the current `/mem` as
`/.snapshots/mem-pre-rollback.<time>`, so the restore can be undone. To restore single files, see
[troubleshooting.md](troubleshooting.md#recover-mem-from-a-snapshot).
