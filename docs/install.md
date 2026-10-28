# Install

> `panda-install` is **new in Unreleased**. v0.1.0 had no installer.

## What you need

- An x86_64 machine that boots in UEFI mode. The installer sets up systemd-boot, which is UEFI only.
- A target disk of at least 8 GiB.
- A USB stick at least as big as the ISO.
- A target disk you are happy to **erase completely**.
- No network: the installer works offline from the ISO.

## 1. Get the ISO

Download from [Releases](https://github.com/AICodeKing/pandaos/releases):

- `pandaos-<version>-headless-x86_64-linux.iso`: console only, for an agent server.
- `pandaos-<version>-desktop-x86_64-linux.iso`: adds the labwc desktop.
- `SHA256SUMS`

Check it, and optionally check the build provenance with the GitHub CLI:

```sh
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify pandaos-<version>-headless-x86_64-linux.iso --repo AICodeKing/pandaos
```

Or build it yourself: `nix build .#iso-headless` (or `.#iso-desktop`), see [development.md](development.md).

## 2. Write the USB stick

Find the stick with `lsblk`, then (this erases `/dev/sdX`):

```sh
sudo dd if=pandaos-<version>-headless-x86_64-linux.iso of=/dev/sdX bs=4M status=progress conv=fsync
```

## 3. Boot and install

Boot the machine from the stick, then run:

```sh
sudo panda-install
```

The interactive installer asks for the target disk, the profile, a hostname, a user, a password, an
optional ssh public key and a time zone. Before it erases anything, it asks you to type the disk's
name. The live ISO logs you in as `panda`, and `sudo` needs no password there.

To script it, pass everything and `--yes`:

```sh
sudo panda-install \
  --disk /dev/nvme0n1 \
  --profile headless \
  --hostname shed-box \
  --user alice \
  --password-hash "$(mkpasswd -m yescrypt)" \
  --ssh-key "ssh-ed25519 AAAA... alice@laptop" \
  --timezone Europe/Berlin \
  --yes
```

| Flag | Default | Meaning |
|---|---|---|
| `--disk DEV` | (required with `--yes`) | Whole target disk, e.g. `/dev/sda`, `/dev/nvme0n1`. **All of it is erased.** |
| `--profile NAME` | `headless` | `headless`, or `desktop` (desktop ISO only) |
| `--hostname NAME` | `pandaos` | `a-z`, `0-9` and `-`, at most 63 characters |
| `--user NAME` | `panda` | Your login user, in `wheel`, `networkmanager` and `panda-mem`. Any other name gets created, and `panda` is then locked |
| `--password-hash H` | (required with `--yes`) | A crypt(3) hash, not the plain password |
| `--ssh-key K` | none | One authorized ssh public key line for the user |
| `--timezone TZ` | `UTC` | A zone under `/etc/zoneinfo`, e.g. `Europe/Berlin` |
| `--yes` | | No questions and no typed disk confirmation |

The installer refuses a disk that is a partition, is smaller than 8 GiB, or has anything mounted or
used as swap. Errors are printed as `panda-install: error: ...`. Full details are in
[nixos/installer/README.md](../nixos/installer/README.md).

There is **no default password**. ssh accepts keys only, so without `--ssh-key` you log in on the
console and add a key later (see [security.md](security.md)).

## What gets installed

**Disk layout.** One btrfs filesystem with these subvolumes:

| Subvolume | Holds |
|---|---|
| `@` | the root filesystem |
| `@nix` | `/nix`, the Nix store |
| `@home` | `/home` |
| `@mem` | `/mem`, shared agent memory |
| `@log` | `/var/log` |
| `@snapshots` | `/.snapshots`: btrfs snapshots of `/mem` and `/home` |

The disk is GPT: a 512 MB EFI partition labelled `PANDABOOT`, then the btrfs partition labelled
`PANDAOS`. The subvolumes are mounted with `compress=zstd:1,noatime`.

Keeping `/mem` and `/home` in their own subvolumes is what lets them be snapshotted hourly and
restored without touching the rest of the system.

**Bootloader.** systemd-boot. Each NixOS generation gets a boot entry, so you can boot an older one
(see [updates-and-rollback.md](updates-and-rollback.md)).

**Offline install.** The ISO carries the prebuilt system, so the installer only copies it from the
ISO's Nix store to the disk. Nothing is built or downloaded. The installed system has the storage
drivers for most machines, but no `linux-firmware`. That firmware arrives with the first online
rebuild, so wifi chips that need it only work after that.

**Local flake.** `/etc/pandaos` holds `flake.nix`, `hardware-configuration.nix` and `local.nix`
(your hostname, time zone, user, password hash and ssh key). `flake.nix` defines
`nixosConfigurations.pandaos`, which tracks `github:AICodeKing/pandaos`. The daily update and any
manual rebuild use it. The first rebuild needs the network and creates `/etc/pandaos/flake.lock`:

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

## After the first boot

1. Connect to the network: wired works out of the box, use `nmtui` for wifi.
2. Pair the agent with your relay, see [agents.md](agents.md#pairing).
3. Check the state with `panda-agentd status`.
