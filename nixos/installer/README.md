# panda-install

Installs PandaOS on a whole disk from the live ISO. **No network is needed.**
Packaged as `packages.x86_64-linux.panda-install` and shipped on both ISOs.

**UEFI only.** There is no legacy BIOS/CSM install. Secure Boot must be
disabled, because the boot loaders are unsigned. On a BIOS boot the installer
stops with an error and changes nothing.

| File | What |
|---|---|
| `panda-install.sh` | the installer (bash, shellcheck-clean) |
| `package.nix` | `writeShellApplication` with pinned runtime tools and baked-in ISO data |
| `target.nix` | extra settings of the prebuilt system that gets installed |
| `../modules/firmware.nix` | curated firmware, on the ISO and in the installed systems |

## Usage

Interactive (asks for anything not given):

```sh
sudo panda-install
```

Each answer is checked as soon as you enter it and asked again if invalid.
The password is asked last.

Non-interactive, for automation and VM tests:

```sh
panda-install --disk /dev/vda --profile headless --hostname X --user panda \
  --password-hash H [--ssh-key K] [--timezone TZ] \
  [--relay URL [--allow-insecure-relay]] --yes
```

| Flag | Required with `--yes` | Default | Meaning |
|---|---|---|---|
| `--disk DEV` | yes | | Whole disk to erase (`/dev/sda`, `/dev/nvme0n1`, `/dev/vda`, or a `/dev/disk/by-id/...` link) |
| `--profile NAME` | no | the ISO's own: `headless` ISO → `headless`, desktop ISO → `desktop` | `headless`, or `desktop` (desktop ISO only) |
| `--hostname NAME` | no | `pandaos` | `a-z`, `0-9`, `-`; max 63 chars |
| `--user NAME` | no | `panda` | Login user, in `wheel`, `networkmanager`, `panda-mem` |
| `--password-hash H` | yes | | crypt(3) hash, e.g. `mkpasswd -m yescrypt` (charset `[A-Za-z0-9./$=]`) |
| `--ssh-key K` | no | none | One public key line (`ssh-ed25519 AAAA... me@host`) |
| `--timezone TZ` | no | `UTC` | Must exist under `/etc/zoneinfo` |
| `--relay URL` | no | none | PandaConnect relay for panda-agentd: `https://host[:port][/path]` |
| `--allow-insecure-relay` | | | Accept an `http://` relay URL (sets `allowInsecure` in `local.nix`) |
| `--yes` | | | No prompts and no typed disk confirmation |

Without `--yes`, the disk is wiped only after you type its name (`vda` or `/dev/vda`).

Checks: the installer must run as root on a UEFI boot. It refuses a disk that
is not a whole disk, is under 8 GiB, or has anything mounted or used as swap.
If `/mnt` is still mounted from an earlier failed run, it offers to unmount it.
With `--yes` it unmounts it without asking.
Errors go to stderr as `panda-install: error: ...` with exit code 1. A failed
run never leaves `/mnt` mounted: the unmount trap is set before the first
mount. On success it unmounts `/mnt` and prints `==> Done.`

## What it does

1. GPT on the disk: partition 1 is a 512M ESP (vfat, label `PANDABOOT`).
   Partition 2 is the rest (btrfs, label `PANDAOS`). Names ending in a digit
   get a `p` (`nvme0n1p1`).
2. Subvolumes `@ @nix @home @mem @log @snapshots`, mounted at
   `/ /nix /home /mem /var/log /.snapshots` with `compress=zstd:1,noatime`.
3. Writes the local flake `/etc/pandaos`:
   - `flake.nix`: input `pandaos` = `github:AICodeKing/pandaos/release`,
     `nixosConfigurations.pandaos = pandaos.lib.mkPandaSystem { profile; modules = [ ./hardware-configuration.nix ./local.nix ]; }`
   - `flake.lock`: written offline. It pins `pandaos` to the exact revision the
     ISO was built from, and nixpkgs to that revision's nixpkgs. Only ISOs built
     from a clean git tree have one; otherwise the first online rebuild makes it.
   - `VERSION`: the ISO's git revision (`-dirty` suffix if built from a dirty tree).
   - `hardware-configuration.nix`: `nixos-generate-config --no-filesystems`
   - `local.nix`: hostname, time zone, user + `hashedPassword`, ssh key,
     optional `services.panda-agentd.relayUrl`; imports `disk.nix`, and `agents.nix` once
     `panda-agents-install` has written it
   - `disk.nix`: `fileSystems.<mount>.device = lib.mkForce "/dev/disk/by-uuid/..."`
     for every mount, from `blkid`
4. Installs the system offline (see below) with systemd-boot (`configurationLimit = 10`).
5. Applies the same hostname, time zone, password and ssh key to the new
   system. With `--relay`, it also writes `/var/lib/panda-agentd/relay`
   (mode 0600, root), which panda-agentd reads as a relay override.

## Offline: how it works

Evaluating and building `/etc/pandaos#pandaos` on the ISO can't work offline:
a new hostname, user or hardware config changes derivations (etc, initrd,
toplevel) whose build tools are not in a runtime closure.

So the ISO carries prebuilt toplevels, `nixosConfigurations.{headless,desktop}`
(desktop only on the desktop ISO), in its store, and the installer runs:

```sh
nixos-install --root /mnt --system <prebuilt toplevel> --no-root-passwd \
  --no-channel-copy --option substituters ""
```

That is a store copy, with no evaluation, build or download. The prebuilt system
(`target.nix`) has the all-hardware initrd drivers and the curated firmware, so
it boots without a hardware config. Its hostname is left empty, so
`/etc/hostname` is a plain file. Per-machine settings are then written to its
mutable state via `nixos-enter`: `/etc/hostname`, `/etc/localtime`,
`usermod -p` (plus `useradd` if `--user` isn't `panda`, which also locks
`panda`), and `~/.ssh/authorized_keys`. `users.mutableUsers` (the default)
keeps them.

`local.nix` declares the same values, so the first online
`sudo nixos-rebuild switch --flake /etc/pandaos#pandaos` gives the same machine
as a fully declarative config.

**Until that first rebuild**, the prebuilt system mounts by label
(`/dev/disk/by-label/PANDAOS`, `PANDABOOT`), not by UUID. `disk.nix` only takes
effect at the rebuild. Don't boot it with a second disk carrying those labels
attached. Likewise, an `http://` relay only works after the rebuild: the
prebuilt system's agentd has `allowInsecure` off.

## Firmware

`nixos/modules/firmware.nix` copies a curated subset out of linux-firmware into
a small derivation:

- wifi: iwlwifi (newest 3 API versions per device), ath9k_htc, ath10k, ath11k
  (PC chips only), rtw88, rtw89, rtlwifi, brcm/cypress, mt76/mediatek
- Realtek NICs (`rtl_nic`)
- GPUs: amdgpu, radeon, i915
- the wireless regulatory db, and Intel/AMD CPU microcode

For linux-firmware 20261019 the subset is about 380 MB uncompressed, measured
from the cache.nixos.org file listing. NixOS zstd-compresses firmware, so it
should come to about 130 MB (sampled ratio 0.35). The full package is about
1.9 GB. Chips outside the list (e.g. Qualcomm ath12k wifi 7) get firmware at
the first online rebuild, because `hardware-configuration.nix` enables
`enableRedistributableFirmware` on real hardware.

## Known limits

- UEFI only, with Secure Boot off. No BIOS support.
- Whole-disk only: no dual boot, no LUKS yet.
