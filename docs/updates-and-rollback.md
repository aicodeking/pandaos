# Updates and rollback

> Daily auto-updates with a health check and automatic rollback, and hourly snapshots, are
> **new in Unreleased**. v0.1.0 does not update itself.

PandaOS is NixOS: every update builds a complete new system *generation* next to the old one. The old
one stays on disk and in the boot menu until garbage collection removes it (weekly, for generations
older than 14 days).

## The daily update

Once a day (`nixos-upgrade.timer`, with up to 1 h of random delay) the box:

1. bumps the `pandaos` input of `/etc/pandaos` to the latest `github:AICodeKing/pandaos`,
2. builds and switches to the new generation. It only downloads from the binary cache: if a download
   fails, the update fails instead of compiling on the box,
3. runs a health check: within 90 s, `panda-agentd` and `panda-mem-init` must be active and there
   must be a default route,
4. if the health check fails, **rolls back automatically** with `nixos-rebuild switch --rollback`.

It never reboots. The units only run on installed systems (those with `/etc/pandaos/flake.nix`).
To turn it off, set `pandaos.updates.enable = false` in `/etc/pandaos/local.nix` and rebuild.

To see when it runs next and what the last run did:

```sh
systemctl list-timers nixos-upgrade.timer
journalctl -u nixos-upgrade.service
```

## Updating by hand

```sh
sudo nix flake update --flake /etc/pandaos
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

To try a generation without making it the boot default, use `nixos-rebuild test` instead of `switch`.

## Rolling back by hand

From a running system:

```sh
sudo nixos-rebuild switch --rollback
```

List the generations:

```sh
sudo nix-env --list-generations --profile /nix/var/nix/profiles/system
```

If the box does not boot or has no network after an update, reboot and pick an older generation in
the systemd-boot menu (press a key, or hold Space, while it starts). Once you are in, run
`sudo nixos-rebuild switch --rollback` so it stays the default.

On the desktop, the `panda-menu` repair menu also offers recovery actions.

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
