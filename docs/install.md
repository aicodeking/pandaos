# Install

> To have a coding agent guide the install and pair the box with PandaCode, give it the
> [Agent Setup runbook](../AGENT_SETUP.md).

## What you need

- An x86_64 machine that boots in **UEFI mode with Secure Boot disabled**. The installer sets up
  systemd-boot, which is UEFI only, and PandaOS's boot files are not signed. BIOS/legacy (CSM) boot
  is not supported.
- A target disk of at least 8 GiB.
- A USB stick at least as big as the ISO.
- A target disk you are happy to **erase completely**.
- The ISO installer works offline. The SSH/kexec path and PandaCode setup require a network.
- For the `server` profile, a dedicated host with `/dev/kvm`. See the [boat guide](boat.md).

## 1. Get the ISO

Download your ISO and `SHA256SUMS` from the
[v0.2.0 GitHub Release](https://github.com/AICodeKing/pandaos/releases/tag/v0.2.0):

- `pandaos-v0.2.0-headless-x86_64-linux.iso` for a console-only agent box.
- `pandaos-v0.2.0-desktop-x86_64-linux.iso` for a labwc desktop and computer use.
- `pandaos-v0.2.0-server-x86_64-linux.iso` for a self-hosted boat microVM server.

Check it, and optionally check the build provenance with the GitHub CLI:

```sh
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify pandaos-v0.2.0-headless-x86_64-linux.iso --repo AICodeKing/pandaos
```

Confirm the checksum output names your ISO and reports `OK`. To build from source on x86_64
Linux with Nix and flakes enabled, explicitly choose the same tag or verified commit for
the ISO, guest images, and CLI. See [development.md](development.md) or run:

```sh
git clone https://github.com/AICodeKing/pandaos && cd pandaos
git checkout v0.2.0              # or the same explicitly chosen, verified commit
nix build .#iso-headless          # or .#iso-desktop or .#iso-server
```

Source builds put the ISO in `result/iso/`.

## 2. Write the USB stick

Find the stick with `lsblk`, then (this erases `/dev/sdX`):

```sh
sudo dd if=pandaos-v0.2.0-headless-x86_64-linux.iso of=/dev/sdX bs=4M status=progress conv=fsync
```

## 3. Boot and install

In the firmware setup, choose UEFI boot and turn Secure Boot off. Boot the machine from the stick,
then run:

```sh
sudo panda-install
```

The interactive installer asks for the target disk, the profile, a hostname, a user, a password, an
optional ssh public key, an optional relay URL and a time zone. On the desktop ISO the default
profile is `desktop`; on the headless ISO it is `headless`. Pass `--profile server` on the
server ISO and follow the [dedicated-server steps](boat.md#install-on-a-dedicated-server-hetzner-ovh).
Before it erases anything, it asks you to type the disk's name. The live ISO logs you in as
`panda`, and `sudo` needs no password there.

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
  --relay https://relay.example.com \
  --yes
```

| Flag | Default | Meaning |
|---|---|---|
| `--disk DEV` | (required with `--yes`) | Whole target disk, e.g. `/dev/sda`, `/dev/nvme0n1`. **All of it is erased.** |
| `--profile NAME` | `desktop` on the desktop ISO, `headless` on the other ISOs | `headless`, `desktop` (desktop ISO only), or `server` (server ISO only) |
| `--hostname NAME` | `pandaos` | `a-z`, `0-9` and `-`, at most 63 characters |
| `--user NAME` | `panda` | Your login user, in `wheel`, `networkmanager` and `panda-mem`. Any other name gets created, and `panda` is then locked |
| `--password-hash H` | (required with `--yes` unless `--sudo-nopasswd`) | A crypt(3) hash, not the plain password |
| `--ssh-key K` | none | One authorized ssh public key line for the user; repeat the flag for more keys |
| `--sudo-nopasswd` | off | Key-only box: `sudo` without a password; the account password is locked unless `--password-hash` is given |
| `--timezone TZ` | `UTC` | A zone under `/etc/zoneinfo`, e.g. `Europe/Berlin` |
| `--relay URL` | none (agentd off) | Enables the optional `panda-agentd` with this `https://` relay. Written to `/var/lib/panda-agentd/relay`, which overrides `services.panda-agentd.relayUrl`. Most boxes skip it and use PandaCode Connect |
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
`PANDAOS`. The subvolumes are mounted with `compress=zstd:1,noatime`. The installer writes the
filesystems' UUIDs into `/etc/pandaos/disk.nix`, and the system mounts them by UUID, so a second disk
or USB stick with the same label cannot be mounted by mistake.

Keeping `/mem` and `/home` in their own subvolumes is what lets them be snapshotted hourly and
restored without touching the rest of the system.

**Bootloader.** systemd-boot. Each NixOS generation gets a boot entry, so you can boot an older one
(see [updates-and-rollback.md](updates-and-rollback.md)).

**Offline install.** The ISO carries the prebuilt system, so the installer only copies it from the
ISO's Nix store to the disk. Nothing is built or downloaded. The installed system has the storage
drivers for most machines and a **curated firmware set** for common wifi chips, so
wifi works right after install without a network. It is not the full
`linux-firmware`; a chip outside the set may still need firmware you add yourself.

**Local flake.** `/etc/pandaos` holds `flake.nix`, a pinned `flake.lock`, `hardware-configuration.nix`,
`disk.nix`, `local.nix` (your hostname, time zone, user, password hash and ssh key) and `VERSION`.
`flake.nix` defines `nixosConfigurations.pandaos`, which tracks the release channel
`github:AICodeKing/pandaos/release` (see [updates-and-rollback.md](updates-and-rollback.md#the-release-channel)).
`flake.lock` pins the exact revisions the ISO was built from, so a rebuild works offline right after
install. The daily update and any manual rebuild use this flake:

```sh
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

## After the first boot

1. Connect to the network: wired works out of the box, use `nmtui` for wifi.
2. Connect the box to PandaCode: `sudo panda-connect sign-in`, open the GitHub link it prints and
   approve it with the GitHub account you use in PandaCode (see
   [agents.md](agents.md#pandacode-connect-pandacode-host)). On the desktop, click *PandaCode* in the dock.
   No terminal is needed: the sign-in runs as `pandacode-sign-in.service` (the host's user and
   sandbox) and every line it prints, the GitHub link and code included, goes to the journal. Over
   ssh without a tty or from a script: `sudo panda-connect sign-in --no-wait`, then
   `journalctl -u pandacode-sign-in -o cat` for the code. After you approve it, the unit records the
   sign-in and starts `pandacode-host` by itself; codes expire after 15 minutes.
3. Check the state with `panda-connect status`. The box now shows up as a computer in PandaCode.
4. Only if you passed `--relay`: pair `panda-agentd`, see [agents.md](agents.md#pairing).

## Hosted server over ssh (kexec)

For a rented dedicated server with no ISO or console access: you only have ssh into the provider's
OS (for example Hetzner's Ubuntu image or its rescue system). Tested on a Hetzner AX42 (2x NVMe,
static IPv4 `/32` with an on-link gateway, static IPv6 `/64`). **If the network config is wrong
the box is unreachable, and only the provider's rescue system gets it back.**

1. Before kexec, confirm your SSH public key is already in root's `authorized_keys` on the
   rescue/provider OS so you can reconnect. Record the live `ip addr`, `ip route`, uplink MAC,
   and DNS details. Keep `extra.nix` on the operator's workstation (or write it after reconnecting),
   using these network details. Match the uplink by MAC, not by name:
   ```nix
   { ... }:
   let mac = "aa:bb:cc:dd:ee:ff"; dns = [ "DNS1" "DNS2" ]; in
   {
     networking.networkmanager.unmanaged = [ "mac:${mac}" ];
     networking.nameservers = dns;
     systemd.network.enable = true;
     systemd.network.wait-online.anyInterface = true;
     systemd.network.networks."10-uplink" = {
       matchConfig.MACAddress = mac;
       address = [ "203.0.113.10/32" "2001:db8:1:2::2/64" ];
       routes = [
         { Gateway = "203.0.113.1"; GatewayOnLink = true; }
         { Gateway = "fe80::1"; }
       ];
       inherit dns;
       networkConfig.IPv6AcceptRA = false;
     };
     # Leave the timer off until you configure a trusted release signer.
     pandaos.updates.mode = "manual";
   }
   ```
2. kexec into a NixOS installer from the running OS. This keeps the IP addresses, the ssh host keys
   and root's `authorized_keys`, and does not touch the disks. The installer boots into RAM
   (tmpfs): files such as `extra.nix` on the old OS root are not carried into the live installer:
   ```sh
   set -eu
   KEXEC_TAG=nixos-25.05
   KEXEC_ARCHIVE=nixos-kexec-installer-noninteractive-x86_64-linux.tar.gz
   KEXEC_SHA256=8c08e3459f28ad2d7cf931ea6075415ef92f5b59ba5d15a10ac60b093f4c1c52
   cd "$(mktemp -d)"
   curl -fL -o "$KEXEC_ARCHIVE" "https://github.com/nix-community/nixos-images/releases/download/$KEXEC_TAG/$KEXEC_ARCHIVE"
   printf '%s  %s\n' "$KEXEC_SHA256" "$KEXEC_ARCHIVE" | sha256sum -c -
   tar -xzf "$KEXEC_ARCHIVE" -C /root
   /root/kexec/run
   ```
   Run these commands as root. The SHA-256 above is the archive digest recorded in the
   [nixos-25.05 release metadata](https://api.github.com/repos/nix-community/nixos-images/releases/tags/nixos-25.05).
   Check the [upstream releases](https://github.com/nix-community/nixos-images/releases) when
   choosing another version, and update both the tag and its expected digest together. Do not
   extract or run the archive if verification fails.

   ssh back in as root after about a minute. Check `ls /sys/firmware/efi/efivars` (needed in step 5).
   After reconnecting, upload `extra.nix` from the workstation with
   `scp extra.nix root@<ip>:/root/extra.nix`, or write `/root/extra.nix` in the live installer.
3. Stop anything holding the disks, e.g. the provider's software RAID (`panda-install` refuses held
   disks). Review `lsblk` and confirm the exact target paths before erasing RAID metadata.
   Run `mdadm --stop` for the identified array and `mdadm --zero-superblock` on each of its
   confirmed member partitions.
4. Confirm `/root/extra.nix` is present in the live kexec installer before running `panda-install`;
   `--extra-config` fails if its file is missing. Build `panda-install` (server system only) and
   install with `--build`, so `local.nix` and `extra.nix` apply from the first boot:
   ```sh
   nix --extra-experimental-features "nix-command flakes" build --impure --out-link pi --expr '
     let f = builtins.getFlake "github:AICodeKing/pandaos/v0.2.0"; in
     f.packages.x86_64-linux.panda-install.override {
       systems.server = f.nixosConfigurations.server.config.system.build.toplevel; }'
   ./pi/bin/panda-install --profile server --build --yes \
     --disk /dev/nvme0n1 --data-disk /dev/nvme1n1 \
     --domain boat.example.com --ipv6-prefix 2001:db8:1:2:b0a7::/80 \
     --sudo-nopasswd \
     --ssh-key "ssh-ed25519 AAAA... one" --ssh-key "ssh-ed25519 AAAA... two" \
     --extra-config /root/extra.nix
   ```
   ssh accepts keys only and root cannot log in. `--sudo-nopasswd` lets the key holder use `sudo`
   without a password (the account password stays locked); or pass `--password-hash` for a sudo password.
   For a source build before the tag is available, replace `v0.2.0` with the exact trusted commit
   you want to install.
   For a trusted single-owner host, add `--untrusted-tenants false`. For multi-tenant use, configure
   separate tenant domains and DNS-01 before the build. See the [boat TLS setup](boat.md#tls).
5. Before rebooting, check the built network unit, and register the boot entry (PandaOS leaves EFI
   variables alone). Keep the provider's PXE entry first so its rescue system still works:
   ```sh
   mount -o subvol=@ /dev/nvme0n1p2 /mnt   # also mount @nix on /mnt/nix to read the system's etc
   efibootmgr -c -d /dev/nvme0n1 -p 1 -L PandaOS -l '\EFI\systemd\systemd-bootx64.efi'
   efibootmgr -o 0001,0002      # replace with PXE,PandaOS boot numbers from efibootmgr -v
   # delete entries of the old OS with efibootmgr -b N -B
   ```
6. Reboot. Verify the new ssh host key using the provider's console or another trusted channel
   before replacing the `known_hosts` entry, then `ssh panda@<ip>`.
