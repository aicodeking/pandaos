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

- No interface at all: check that the hardware is seen with `lspci` / `lsusb`. Some wifi chips need
  firmware that may not be included.
- The agent shows as disconnected but the network works: check the relay URL and
  `journalctl -u panda-agentd`. The agent only needs outbound access to the relay.

## A failed update

The daily update rolls back by itself when its health check fails. To see what happened:

```sh
systemctl list-timers nixos-upgrade.timer
journalctl -u nixos-upgrade.service --since yesterday   # includes the panda-update health check
```

To retry by hand and see the error:

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

A build that fails on the box is often a full disk (below) or no network (above).

## Rollback

```sh
sudo nixos-rebuild switch --rollback
```

If it does not boot, choose an older generation in the systemd-boot menu, then run the command above.
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

On btrfs, `df` can show free space while writes still fail because metadata is full. A balance can help:
`sudo btrfs balance start -dusage=50 /`.

## Re-pairing

To pair the box with a relay again (new relay, or a revoked token):

```sh
sudo systemctl stop panda-agentd
sudo rm /var/lib/panda-agentd/token
sudo systemctl start panda-agentd
journalctl -u panda-agentd -f | grep 'pairing code:'
```

Enter the new code on the relay side. To change the relay, set `services.panda-agentd.relayUrl` in
`/etc/pandaos/local.nix` and rebuild first.

## Recover /mem from a snapshot

`/mem` is a git repo, so first check whether git alone can fix it:

```sh
git -C /mem log --oneline
git -C /mem checkout <commit> -- <file>
```

If the files or the repo itself are gone or damaged, use the hourly btrfs snapshots in
`/.snapshots` (`mem.<time>` and `home.<time>`, read-only):

```sh
ls /.snapshots
sudo panda-rollback-mem mem.20261028T1400     # restore all of /mem; the current state is saved first
```

To copy back single files instead, keeping the group ownership `/mem` uses:

```sh
sudo rsync -a /.snapshots/mem.20261028T1400/<file> /mem/<file>
sudo chgrp -R panda-mem /mem
git -C /mem status
```

The same works for `/home` (`/.snapshots/home.<time>`). Copy files back rather than replacing the
subvolume, so the running system does not lose its mount.
