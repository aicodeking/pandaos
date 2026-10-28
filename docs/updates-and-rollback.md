# Updates and rollback

> Daily auto-updates with a health check and automatic rollback, and hourly snapshots, are
> **new in Unreleased**. v0.1.0 does not update itself.

PandaOS is NixOS: every update builds a complete new system *generation* next to the old one. The old
one stays on disk and in the boot menu until garbage collection removes it (weekly, for generations
older than 14 days).

## The daily update

Once a day the box:

1. updates `/etc/pandaos` to the latest `github:AICodeKing/pandaos`,
2. builds and switches to the new generation,
3. runs a health check,
4. if the health check fails, **rolls back automatically** to the previous generation.

To see when it runs next, look at the timers:

```sh
systemctl list-timers
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

Every hour, btrfs snapshots of `/mem` and `/home` are taken into the `@snapshots` subvolume.
Snapshots are copy-on-write, so they cost disk only for data that changed.

System rollback (generations) and data snapshots are separate: rolling back a generation never
changes `/mem` or `/home`, and restoring a snapshot never changes the system.

To restore files, see [troubleshooting.md](troubleshooting.md#recover-mem-from-a-snapshot).
