# panda-install

Installs PandaOS on a whole disk from the live ISO. **No network is needed.**
Packaged as `packages.x86_64-linux.panda-install` and shipped on both ISOs.

| File | What |
|---|---|
| `panda-install.sh` | the installer (bash, shellcheck-clean) |
| `package.nix` | `writeShellApplication` with pinned runtime tools |
| `target.nix` | extra settings of the prebuilt system that gets installed |

## Usage

Interactive (asks for anything not given):

```sh
sudo panda-install
```

Non-interactive, for automation and VM tests:

```sh
panda-install --disk /dev/vda --profile headless --hostname X --user panda \
  --password-hash H [--ssh-key K] [--timezone TZ] --yes
```

| Flag | Required with `--yes` | Default | Meaning |
|---|---|---|---|
| `--disk DEV` | yes | | Whole disk to erase (`/dev/sda`, `/dev/nvme0n1`, `/dev/vda`, or a `/dev/disk/by-id/...` link) |
| `--profile NAME` | no | `headless` | `headless`, or `desktop` (desktop ISO only) |
| `--hostname NAME` | no | `pandaos` | `a-z`, `0-9`, `-`; max 63 chars |
| `--user NAME` | no | `panda` | Login user, in `wheel`, `networkmanager`, `panda-mem` |
| `--password-hash H` | yes | | crypt(3) hash, e.g. `mkpasswd -m yescrypt` (charset `[A-Za-z0-9./$=]`) |
| `--ssh-key K` | no | none | One public key line (`ssh-ed25519 AAAA... me@host`) |
| `--timezone TZ` | no | `UTC` | Must exist under `/etc/zoneinfo` |
| `--yes` | | | No prompts and no typed disk confirmation |

Without `--yes`, the disk is wiped only after you type its name (`vda` or `/dev/vda`).

It must run as root on a UEFI boot (systemd-boot is UEFI only). It refuses a
disk that is not a whole disk, is under 8 GiB, or has anything mounted or used
as swap, and it refuses if `/mnt` is already a mountpoint. Errors go to stderr
as `panda-install: error: ...` with exit code 1. On success it unmounts `/mnt`
and prints `==> Done.`

## What it does

1. GPT on the disk: partition 1 is a 512M ESP (vfat, label `PANDABOOT`).
   Partition 2 is the rest (btrfs, label `PANDAOS`). Names ending in a digit
   get a `p` (`nvme0n1p1`).
2. Subvolumes `@ @nix @home @mem @log @snapshots`, mounted at
   `/ /nix /home /mem /var/log /.snapshots` with `compress=zstd:1,noatime`.
   `nixos/modules/generic-disk.nix` mounts the same layout by label.
3. Writes the local flake `/etc/pandaos`:
   - `flake.nix`: input `pandaos` (`github:AICodeKing/pandaos`),
     `nixosConfigurations.pandaos = pandaos.lib.mkPandaSystem { profile; modules = [ ./hardware-configuration.nix ./local.nix ]; }`
   - `hardware-configuration.nix`: `nixos-generate-config --no-filesystems`
   - `local.nix`: hostname, time zone, user + `hashedPassword`, ssh key
4. Installs the system offline (see below) with systemd-boot (`configurationLimit = 10`).
5. Applies the same hostname, time zone, password and ssh key to the new system.

## Offline: how it works

Evaluating and building `/etc/pandaos#pandaos` on the ISO can't work offline:
the `pandaos` input is on GitHub, and a new hostname, user or hardware config
changes derivations (etc, initrd, toplevel) whose build tools are not in a
runtime closure.

So the ISO carries prebuilt toplevels, `nixosConfigurations.{headless,desktop}`
(desktop only on the desktop ISO), in its store, and the installer runs:

```sh
nixos-install --root /mnt --system <prebuilt toplevel> --no-root-passwd \
  --no-channel-copy --option substituters ""
```

That is a store copy, with no evaluation, build or download. The prebuilt system
(`target.nix`) has the all-hardware initrd drivers, so it boots without a
hardware config. Its hostname is left empty, so `/etc/hostname` is a plain file.
Per-machine settings are then written to its mutable state via `nixos-enter`:
`/etc/hostname`, `/etc/localtime`, `usermod -p` (plus `useradd` if `--user`
isn't `panda`, which also locks `panda`), and `~/.ssh/authorized_keys`.
`users.mutableUsers` (the default) keeps them.

`local.nix` declares the same values, so the first online
`sudo nixos-rebuild switch --flake /etc/pandaos#pandaos` gives the same machine
as a fully declarative config (that rebuild also creates `flake.lock`).

## Known limits

- No linux-firmware on the ISO or the offline install (size). Wired ethernet
  and VMs work; wifi chips that need firmware get it after the first online
  rebuild, because `hardware-configuration.nix` turns firmware on.
- UEFI only.
- Whole-disk only: no dual boot, no LUKS yet.
