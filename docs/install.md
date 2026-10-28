# Install

> `panda-install` is **new in Unreleased**. v0.1.0 had no installer.

## What you need

- An x86_64 machine that boots in UEFI mode. The installer sets up systemd-boot, which is UEFI only.
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

The interactive installer asks for the target disk, the profile, a hostname, a user, a password and
an optional ssh public key.

To script it, pass everything and `--yes`:

```sh
sudo panda-install \
  --disk /dev/nvme0n1 \
  --profile headless \
  --hostname shed-box \
  --user alice \
  --password-hash "$(mkpasswd -m sha-512)" \
  --ssh-key "ssh-ed25519 AAAA... alice@laptop" \
  --yes
```

| Flag | Meaning |
|---|---|
| `--disk` | Target disk. **All of it is erased.** |
| `--profile` | `headless` or `desktop` |
| `--hostname` | Machine name |
| `--user` | Your login user |
| `--password-hash` | A crypt(3) hash, not the plain password |
| `--ssh-key` | Optional. An authorized ssh public key for the user |
| `--yes` | Do not ask for confirmation |

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
| `@log` | logs |
| `@snapshots` | btrfs snapshots of `/mem` and `/home` |

Keeping `/mem` and `/home` in their own subvolumes is what lets them be snapshotted hourly and
restored without touching the rest of the system.

**Bootloader.** systemd-boot. Each NixOS generation gets a boot entry, so you can boot an older one
(see [updates-and-rollback.md](updates-and-rollback.md)).

**Local flake.** `/etc/pandaos` holds a flake with `nixosConfigurations.pandaos`. It tracks
`github:AICodeKing/pandaos` and holds your machine settings. The daily update and any manual rebuild
use it:

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

## After the first boot

1. Connect to the network: wired works out of the box, use `nmtui` for wifi.
2. Pair the agent with your relay, see [agents.md](agents.md#pairing).
3. Check the state with `panda-agentd status`.
