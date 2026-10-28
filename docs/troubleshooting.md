# Troubleshooting

Start with the logs for the current boot:

```sh
journalctl -b -p warning
panda-agentd status        # new in Unreleased
```

On the desktop, the `panda-menu` repair menu (new in Unreleased) is the first place to look. Everything below also works from a console or ssh.

## No network

```sh
nmcli device status        # is the interface there and connected?
nmtui                      # join wifi, or edit a wired connection
ip addr; ip route
ping -c 3 1.1.1.1          # IP connectivity
resolvectl query github.com || getent hosts github.com   # DNS
```

- No interface at all: check that the hardware is seen with `lspci` / `lsusb`, and look for firmware
  errors with `journalctl -k | grep -i firmware`. PandaOS ships a curated firmware set for common wifi
  chips, not all of `linux-firmware`, so a rarer chip may need firmware added to
  `hardware.firmware` in `/etc/pandaos/local.nix` (a wired link is needed for that rebuild).
- The agent shows as disconnected but the network works: `panda-agentd status` shows the relay and
  the last error (`lastError`). Check the relay URL and `journalctl -u panda-agentd`. The agent only
  needs outbound access to the relay.

## A failed update

The daily update rolls back by itself when its health check fails. To see what happened:

```sh
panda-update status
systemctl list-timers nixos-upgrade.timer
journalctl -u nixos-upgrade.service --since yesterday   # includes the panda-update health check
```

To retry by hand and see the error:

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

A build that fails on the box is often a full disk (below) or no network (above). Without a binary
cache, PandaOS's own packages compile on the box, which can take long on an old PC, see
[updates-and-rollback.md](updates-and-rollback.md#what-an-update-downloads-and-builds).

No updates at all? Check for a hold (`panda-update status`; `sudo panda-update unhold` to clear it),
and note that until the first release there is no `release` branch to update from.

## Rollback

```sh
sudo panda-update rollback  # sets the update hold, then nixos-rebuild switch --rollback
```

On the desktop, `panda-menu` → *Roll back system* does both. Clear the hold later with
`sudo panda-update unhold`.

A generation that fails to boot, or boots unhealthy, is undone by boot assessment, see
[updates-and-rollback.md](updates-and-rollback.md#boot-assessment-rollback-of-a-bad-boot). If it does not boot, choose an older generation in the systemd-boot menu, then run the command above.
See [updates-and-rollback.md](updates-and-rollback.md).

## Disk full

```sh
df -h /
sudo btrfs filesystem usage /
sudo ncdu -x /
```

Free space, in order of safety:

```sh
sudo journalctl --vacuum-size=20M
sudo nix-collect-garbage --delete-older-than 7d   # removes old generations: you cannot roll back to them
sudo nix-store --optimise
```

Snapshots keep deleted data alive. btrbk already keeps only 24 hourly and 7 daily ones. To free
more, list them and delete old ones you no longer need:

```sh
ls /.snapshots
sudo btrfs subvolume delete /.snapshots/<old-snapshot>
```

While the disk is nearly full, Nix collects garbage by itself during builds (its `min-free` setting),
and `panda-agentd` refuses new jobs below 1 GiB free (`-min-free-mb`).

On btrfs, `df` can show free space while writes still fail because metadata is full. A balance can help:
`sudo btrfs balance start -dusage=50 /`.

## Re-pairing

To pair the box with a relay again (new relay, or a revoked token):

```sh
sudo systemctl stop panda-agentd
sudo rm /var/lib/panda-agentd/token
echo https://relay.example.com | sudo tee /var/lib/panda-agentd/relay   # only to change the relay
sudo systemctl start panda-agentd
sudo cat /run/panda-agentd/pairing-code
```

Enter the new code on the relay side (or use `panda-menu` → *Pairing code* on the desktop). The relay
file overrides `services.panda-agentd.relayUrl`; delete it to go back to the configured URL.

## Recover /mem from a snapshot

`/mem` is a git repo, so first check its history:

```sh
panda-mem history
panda-mem history semantic/<file>.md
```

`/mem` belongs to the `panda-mem` service user, so restore through that user and with the service
stopped. Fix a single file from an older commit:

```sh
sudo systemctl stop panda-mem
sudo -u panda-mem git -C /mem checkout <commit> -- semantic/<file>.md
sudo -u panda-mem git -C /mem commit -m "restore <file>"
sudo systemctl start panda-mem
```

If the files or the repo itself are gone or damaged, use the hourly btrfs snapshots in
`/.snapshots` (`mem.<time>` and `home.<time>`, read-only):

```sh
ls /.snapshots
sudo panda-rollback-mem mem.20261028T1400     # restore all of /mem; the current state is saved first
```

On the desktop, `panda-menu` → *Restore /mem from snapshot* does the same. To copy back single files
instead:

```sh
sudo systemctl stop panda-mem
sudo rsync -a /.snapshots/mem.20261028T1400/<file> /mem/<file>
sudo chown -R panda-mem:panda-mem /mem
sudo systemctl start panda-mem
```

`panda-mem` refuses a repo whose `.git` holds hooks or unexpected config, so do not copy a `.git`
directory from elsewhere into `/mem`.

The same works for `/home` (`/.snapshots/home.<time>`, keep the user's ownership). Copy files back
rather than replacing the subvolume, so the running system does not lose its mount.
