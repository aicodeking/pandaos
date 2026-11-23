# Set up PandaOS with your coding agent

> **How to use this file**
>
> Copy this prompt into PandaCode, Claude Code, Codex, or another coding agent:
>
> `Read and follow https://raw.githubusercontent.com/AICodeKing/pandaos/v0.2.0/AGENT_SETUP.md (or the local AGENT_SETUP.md in my PandaOS checkout). Help me install PandaOS v0.2.0 and connect it to PandaCode. Start with Step 0. Pause for my explicit confirmation of each disk and for every human-only step.`
>
> The URL is pinned to the `v0.2.0` tag. Check `main` for a newer version of this runbook, or
> replace `v0.2.0` in the URL with a specific commit SHA to pin an exact revision.

## Step 0 — Agent intake

Ask the user these questions first. Wait for their answers before choosing an install path.

| Ask | Record |
|---|---|
| Which profile? | `headless` for a console-only agent box, `desktop` for a GUI and computer use, `server` for a self-hosted `boat` microVM host, or `connect-only` for an already-installed **PandaOS** machine (it has `sudo panda-connect`). For `connect-only`, go straight to [Track C](#track-c--other-pandacode-instances-teammates-and-sdks). Non-PandaOS Linux or macOS machines do not have `panda-connect`: use the PandaCode desktop app on them directly, or connect to boat VMs over SSH as in Track C. |
| Which machine and access method? | Local x86_64 PC and USB stick, a dedicated server with a virtual ISO console, or a dedicated server with SSH and a rescue system for kexec. Ask about backups, the intended OS and optional data disks, and a login SSH public key if remote access is needed. |
| Which domain and DNS provider for `server`? | Base domain, public IPv4/IPv6 addresses, DNS zone access, tenant port and desktop domains, and ACME contact email. |
| Who will run VMs on `server`? | `services.boat.untrustedTenants = false` for your own trusted VMs, or `true` for multi-tenant isolation settings. Serving untrusted tenants also requires the [operator checklist](docs/boat.md#operator-checklist) and an external penetration test. |
| Which host agent CLIs? | Any of `claude-code`, `codex`, `gemini-cli`, and `opencode`, or none. |

## Hard safety gates for the agent

- **Before any disk write that erases, formats, or flashes a device**, run
  `lsblk -o PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,MOUNTPOINT` on the machine that will do the write
  (`diskutil list` on macOS; the device picker in Rufus or balenaEtcher on Windows). The singular
  `MOUNTPOINT` column works with both older (`< 2.37`) and newer `util-linux`.
  Show the output to the user, identify the whole-disk paths, and wait for explicit confirmation
  of each exact `/dev/...` path. Confirm separately for the USB stick, OS disk, and optional
  data disk. This also applies to clearing RAID metadata. Run `lsblk` again after a boot or
  device change. Use scripted `--yes` only after this gate.
- **Verify x86_64 and UEFI on the install target.** On Linux, `uname -m` must report `x86_64`
  and `test -d /sys/firmware/efi/efivars` must succeed. With efivars available, run
  `sudo od -An -j4 -N1 -tu1 /sys/firmware/efi/efivars/SecureBoot-*`. The Secure Boot byte
  must be `0`. If the byte is unreadable, have the user confirm the setting in firmware.
  For `server`, also require `test -c /dev/kvm` on the host. Pause if the checks fail and
  have the user check firmware settings and hardware virtualization.
- **Keep credentials out of agent output.** Never echo API keys, tokens, passwords or password
  hashes into logs, chat, command traces, or commits. Use a private human terminal for sign-ins
  and a password manager or protected stdin for key handoffs. Delete `/root/boat-owner-token`
  once the user has stored its token safely.
- **Never edit or print `/etc/pandaos/local.nix`.** It is mode `0600` and holds the user's
  `hashedPassword`. It already imports `./agents.nix` and `./extra.nix` from `/etc/pandaos` when
  they exist. Write agent CLI selections to `/etc/pandaos/agents.nix` and any post-install
  server, network, image or update settings to `/etc/pandaos/extra.nix` instead.
- **Plan for `sudo` password prompts.** On the live ISO, `panda` runs `sudo` without a password.
  On an installed system, `sudo` prompts for the user's password unless the install used
  `--build --sudo-nopasswd --ssh-key ...`. The agent must never see that password, so choose one:
  - For agent-managed boxes, install with `--build --sudo-nopasswd --ssh-key '<public key>'`
    (key-only login; the account password stays locked). `--build` needs network on the live ISO.
  - Have the human run each `sudo` command, or type the password, on the console, in a private
    terminal, or in a shared `tmux` session whose output the agent does not capture.
  - For scripted installs that need a password, have the human generate the `--password-hash`
    privately (for example `mkpasswd -m yescrypt` in their own terminal), outside agent output.

  Nix copies `/etc/pandaos/local.nix`, including the password hash, into the world-readable
  `/nix/store` during `--build` and every `nixos-rebuild`. If the hash must stay secret from
  other local users, prefer key-only login (`--ssh-key` with `--sudo-nopasswd`), or set
  `users.users.<user>.hashedPasswordFile` in `extra.nix` to a root-only file.
- **Hand off human-only steps and wait.** Ask the user to handle BIOS/UEFI settings and boot
  selection, GitHub device-flow approval, and DNS record creation. Resume once they confirm
  completion and you can verify the result.

## Track A — Personal or team agent box

### A1. Download, verify, and install

Choose `headless` or `desktop`. From the [v0.2.0 release][release] or [latest releases page][latest]
when it points to v0.2.0, download the matching ISO and `SHA256SUMS` into an empty directory.
Use the same commands with `profile=server` for Track B's ISO install.

```sh
profile=headless   # or desktop or server
iso="pandaos-v0.2.0-${profile}-x86_64-linux.iso"
base=https://github.com/AICodeKing/pandaos/releases/download/v0.2.0
curl -fLO "$base/$iso" &&
  curl -fLO "$base/SHA256SUMS" &&
  sha256sum -c --ignore-missing SHA256SUMS
```

Proceed only if the output names the chosen ISO and reports `OK`.

**Provenance attestation (required when `gh` is installed).** If `command -v gh` succeeds, this
check is mandatory; stop if it fails:

```sh
gh attestation verify "$iso" --repo AICodeKing/pandaos \
  --signer-workflow AICodeKing/pandaos/.github/workflows/release.yml \
  --source-ref refs/tags/v0.2.0
```

If `gh` is not installed, tell the user that checksum-only verification proves the download
matches `SHA256SUMS` from the same release (transfer integrity), but not that GitHub Actions
built it from the `v0.2.0` tag (build provenance). Get their explicit confirmation before
proceeding on checksums alone, or install `gh` and run the check. The same rule applies to the
guest images in [B3](#b3-install-and-verify-guest-images) and the CLI in
[B4](#b4-create-the-first-owner-key-and-test-one-vm).

If the tagged assets are unavailable (for example, while release assets are still publishing),
use the source-build steps in the [install guide](docs/install.md#1-get-the-iso). Explicitly choose
and check out the same tag or verified commit for the ISO, guest images, and CLI source builds.
After `nix build .#iso-headless`, `nix build .#iso-desktop`, or `nix build .#iso-server`,
select the revision-stamped source-built ISO before continuing to the flash step below
(the server build uses `headless` in its source filename):

```sh
iso=$(find -L result/iso -name '*.iso' | head -n 1)
test -n "$iso"
```

Proceed only if `test -n "$iso"` succeeds.

1. Complete the disk gate for the USB stick on the workstation that writes it, then flash the ISO.
   Substitute the confirmed path for the example device.

   - **Linux:** identify the stick with `lsblk` (as in the disk gate), unmount its partitions,
     then write it:

     ```sh
     sudo umount /dev/sdX?* 2>/dev/null || true
     sudo dd if="$iso" of=/dev/sdX bs=4M status=progress conv=fsync
     ```

   - **macOS:** if `sha256sum` is missing, verify with
     `grep " $iso\$" SHA256SUMS | shasum -a 256 -c` instead. Identify the stick with
     `diskutil list` and confirm the exact `/dev/diskN` with the user, then:

     ```sh
     diskutil unmountDisk /dev/diskN
     sudo dd if="$iso" of=/dev/rdiskN bs=4m
     ```

   - **Windows:** compare `Get-FileHash .\pandaos-v0.2.0-<profile>-x86_64-linux.iso -Algorithm SHA256`
     with the ISO's line in `SHA256SUMS`, then have the user write the ISO with
     [Rufus](https://rufus.ie/) (DD image mode) or [balenaEtcher](https://etcher.balena.io/),
     selecting the confirmed USB device.

2. Ask the user to boot the stick in UEFI mode with Secure Boot disabled. **A remote coding agent
   cannot type on the local USB console.** Choose one:
   - The human runs the checks and `sudo panda-install` on the console, with the agent guiding.
   - The human gives the agent SSH access to the live system. On the console, they run
     `mkdir -p ~/.ssh && chmod 700 ~/.ssh`, append the agent's SSH public key to
     `~/.ssh/authorized_keys` (for example
     `echo 'ssh-ed25519 AAAA... agent' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys`),
     and read the address with `ip -brief addr`. The live session runs as `panda` with key-only
     SSH, so the agent then connects with `ssh panda@<address>`.

   On the live system, verify the firmware checks and complete the disk gate for the OS disk. Run
   `sudo panda-install`, select the agreed profile, and use the private console for the password
   prompt. Add an SSH public key if needed. If the agent will manage the installed box over SSH,
   prefer `--build --sudo-nopasswd --ssh-key '<public key>'` (see the `sudo` gate above). For
   unattended installs, see the
   [scripted flags](docs/install.md#3-boot-and-install).
3. Reboot into the installed system and remove the USB stick. Use wired networking or run
   `nmtui` for Wi-Fi. The ISO installs offline. PandaCode Connect, CLI installation, and every
   `nixos-rebuild` need a network.

### A2. Connect to PandaCode

Replace `workbench` with the agreed device name. Have the human run this on the box in
their private terminal; the waiting command follows the journal and prints the GitHub device
code. The agent must never run it with captured output:

```sh
sudo panda-connect sign-in --device-name workbench
```

Alternatively, the agent may start the unit without printing the code:

```sh
sudo panda-connect sign-in --device-name workbench --no-wait
```

Then have the human run `sudo journalctl -u pandacode-sign-in -o cat -b` in their private
terminal to get the link and code. The agent must not capture that journal output. Have the
human open the GitHub link and approve the current device code with the **same GitHub account
they use in PandaCode**. After the human confirms approval, the agent checks only the machine-readable
status:

```sh
connect_json=$(panda-connect status --json)
# compact JSON: {"signedIn":true,"service":"active","deviceName":"workbench",...}
grep -q '"signedIn":true' <<<"$connect_json" && grep -q '"service":"active"' <<<"$connect_json"
```

`jq` is not installed on PandaOS; these checks need only bash and `grep`.

Have the human verify the new computer appears by name in PandaCode.

### A3. Install and sign in to the selected agent CLIs

Use the first-boot picker on tty8 or run `sudo panda-agents-install` with the names chosen in
Step 0. For example, `sudo panda-agents-install claude-code codex`. These use `panda-update`, so
they work once `v0.2.0` is published on `origin/release`: v0.2.0 verifies it with the
[release-signing key](SECURITY.md#release-signing-key) pre-configured in
`pandaos.updates.allowedSigners`.

**Locked-version fallback.** If the release branch is not yet published, or the release signature
check refuses the update, install from the current pinned lock instead (no channel bump). Write
the selection to `/etc/pandaos/agents.nix`, which `local.nix` already imports (do not edit
`local.nix`):

```sh
printf '%s\n' '{ ... }: { pandaos.agents.clis = [ "claude-code" "codex" ]; }' |
  sudo tee /etc/pandaos/agents.nix >/dev/null
sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
```

This still needs a network: the installed system does not bundle nixpkgs, so `nixos-rebuild`
fetches the locked flake inputs. Keep release signature verification enabled.

For each selected CLI, open it as the `agent` user and let the user complete any provider
sign-in in a private session.

| Package | Launch as `agent` |
|---|---|
| `claude-code` | `sudo -iu agent claude` |
| `codex` | `sudo -iu agent codex` |
| `gemini-cli` | `sudo -iu agent gemini` |
| `opencode` | `sudo -iu agent opencode` |

```sh
sudo panda-agents sync --user agent && sudo panda-agents doctor --user agent
```

Full access is optional. If the user explicitly wants PandaCode devices to be able to select a
mode without approval prompts, run `sudo panda-connect full-access on`.

## Track B — Self-hosted boat microVM server

### B1. Set up DNS and the trust model

Ask the user to create the records and confirm they resolve to the server. Use A for a public
IPv4 address and AAAA only for a working public IPv6 address. Use DNS-only service for the SSH
record so OpenSSH can reach port 2222.

| DNS name | Type | Purpose |
|---|---|---|
| `api.<domain>` | A / AAAA | HTTPS API and dashboard |
| `desktop.<domain>` | A / AAAA | Default desktop entry point |
| `*.desktop.<domain>` | A / AAAA | Default per-VM desktop origins |
| `ssh.<domain>` | A / AAAA | SSH jump host on TCP 2222 |
| `*.<vmDomain>` | A / AAAA | Guest port routes, with `vm.<domain>` as the default `vmDomain` |

For a multi-tenant host, put `vmDomain` and `desktopDomain` on a registrable domain separate
from the API's, such as `ports.example.net` and `desktops.example.net` for `api.boat.example.com`.
Replace the two default desktop DNS entries with `<desktopDomain>` and `*.<desktopDomain>`.
Configure `services.boat.gateway.dnsProvider` and a matching Caddy DNS plugin for DNS-01 wildcard
TLS. Use a protected `gateway.environmentFile` for DNS credentials. Follow the
[operator checklist](docs/boat.md#operator-checklist) and [TLS setup](docs/boat.md#tls), including
the Public Suffix List caveat for sibling tenant cookies. Complete the checklist and external
penetration test before handing keys to untrusted tenants.

For a trusted single-owner host, set `services.boat.untrustedTenants = false`. The installer can
use HTTP-01 on-demand TLS on a trusted host without a DNS plugin. Its issuance limits and
Certificate Transparency exposure are described in the [TLS setup](docs/boat.md#tls).

### B2. Install the server

- **Local or virtual ISO console.** Download and verify `pandaos-v0.2.0-server-x86_64-linux.iso`
  as in A1. As in A1, a remote agent cannot type on the virtual ISO console: either the human runs
  the commands there, or the human adds the agent's SSH public key on the live ISO
  (`mkdir -p ~/.ssh && chmod 700 ~/.ssh`, append the key to `~/.ssh/authorized_keys`, check the
  address with `ip -brief addr`) so the agent can `ssh panda@<address>`. Boot it in UEFI mode,
  verify KVM, and confirm the exact disks. Use the server ISO's interactive installer with flags for the known
  values. Add `--data-disk /dev/nvme1n1` only if the user approved that device.

  **Trusted single-owner host:**

  ```sh
  sudo panda-install --profile server --disk /dev/nvme0n1 \
    --domain boat.example.com --acme-email ops@example.com \
    --untrusted-tenants false \
    --ssh-key 'ssh-ed25519 AAAA... operator@workstation'
  ```

  Without `--build`, this copies the prebuilt generic server system from the ISO: the
  placeholder `api.boat.localhost` domain, the untrusted-tenant `nosmt` kernel default, and no
  data-disk pin. Your flags take effect only after the **mandatory** first-boot rebuild in
  [After first boot](#after-first-boot-mandatory-rebuild-and-runtime-checks).

  **Multi-tenant host.** Use `--build` so DNS-01 TLS works from the first boot. First write
  `extra.nix` on the live ISO (for example `/root/extra.nix` or `~/extra.nix`). This complete
  skeleton uses Cloudflare; pick the [caddy-dns](https://github.com/caddy-dns) plugin matching
  the provider:

  ```nix
  { lib, pkgs, ... }:
  {
    services.boat.gateway.caddyPackage = pkgs.caddy.withPlugins {
      plugins = [ "github.com/caddy-dns/cloudflare@v0.2.1" ];
      hash = lib.fakeHash;   # replace with the SRI hash from the dry build below
    };
    services.boat.gateway.environmentFile = "/var/lib/secrets/caddy-dns.env";
  }
  ```

  **Compute the real `hash` before running `panda-install --build`**, so a wrong hash can never
  abort the install after the disk has been erased. On the live ISO (with network), run a dry
  build of the same plugin list against the release's pinned nixpkgs:

  ```sh
  nix --extra-experimental-features "nix-command flakes" build --impure --no-link --expr '
    let pkgs = (builtins.getFlake "github:AICodeKing/pandaos/v0.2.0").nixosConfigurations.server.pkgs;
    in pkgs.caddy.withPlugins {
      plugins = [ "github.com/caddy-dns/cloudflare@v0.2.1" ]; hash = pkgs.lib.fakeHash; }'
  ```

  It fails with `hash mismatch` and prints `got: sha256-...`. Put that SRI hash in place of
  `lib.fakeHash` in `extra.nix`, then rerun the command with the real hash; it must now succeed.
  Then install:

  ```sh
  sudo panda-install --profile server --disk /dev/nvme0n1 --build \
    --domain boat.example.com --acme-email ops@example.com \
    --untrusted-tenants true \
    --vm-domain ports.example.net --desktop-domain desktops.example.net \
    --dns-provider 'cloudflare {env.CF_API_TOKEN}' \
    --extra-config /path/to/extra.nix \
    --ssh-key 'ssh-ed25519 AAAA... operator@workstation'
  ```

  The DNS credentials file lives on the installed disk, which `panda-install` erases, so create
  it only **after the first boot**: the human writes `/var/lib/secrets/caddy-dns.env` (directory
  `0700`, file `0600`, containing `CF_API_TOKEN=...`) in a private terminal, then runs
  `sudo systemctl restart caddy`.

  If you instead install without `--build` and add DNS-01 later in `/etc/pandaos/extra.nix`
  (never `local.nix`), the installer enabled `services.boat.gateway.allowOnDemandTLS`; set
  `services.boat.gateway.allowOnDemandTLS = lib.mkForce false;` in `extra.nix` in the same rebuild.

  **Static-IP servers (no DHCP).** Prepare `extra.nix` with the static network configuration
  matching the uplink MAC, as in
  [Hosted server over SSH (kexec)](docs/install.md#hosted-server-over-ssh-kexec), so the server
  stays reachable from its first boot. `--build` and every `nixos-rebuild` download flake inputs,
  but the live ISO has no network without DHCP. Use NetworkManager's `nmcli` for temporary
  networking, so NetworkManager does not overwrite hand-set addresses or `/etc/resolv.conf`
  mid-build. Find the uplink with `nmcli device status`, then:

  ```sh
  sudo nmcli con add type ethernet con-name live-static ifname <iface> \
    ipv4.method manual ipv4.addresses 203.0.113.10/32 ipv4.gateway 203.0.113.1 \
    ipv4.dns "1.1.1.1 9.9.9.9"
  sudo nmcli con up live-static
  ```

  Use the provider's address, prefix, gateway and DNS. Then either:
  - **(a)** configure this temporary networking on the live ISO, then run `panda-install` with
    `--build --extra-config /path/to/extra.nix`; or
  - **(b)** install offline from the ISO without `--build`, keep the (virtual) console attached,
    and on the first boot log in on the console, configure the same temporary networking with
    `nmcli`, write the static settings to `/etc/pandaos/extra.nix`, and run
    `sudo nixos-rebuild switch --flake /etc/pandaos#pandaos` (this needs the temporary network).
    Confirm `ip addr` and `ip route` before detaching the console.
- **SSH and kexec.** Follow [Hosted server over SSH (kexec)](docs/install.md#hosted-server-over-ssh-kexec).
  Confirm the provider's rescue path and that your SSH public key is already in root's
  `authorized_keys` on the rescue/provider OS so you can reconnect after kexec. The kexec
  `--build` runs in a RAM-backed tmpfs: require **at least 8 GiB of RAM (16 GiB or more
  recommended)** for `server` or `desktop` builds. Record `ip addr`,
  `ip route`, the uplink MAC, and DNS. Prepare `extra.nix` on the operator's workstation with
  these network settings, or write it after reconnecting. The kexec installer boots into RAM
  (tmpfs); an `extra.nix` left on the old OS root is not carried over. Boot with kexec, reconnect, check
  UEFI and KVM again, and show the new `lsblk` output before touching disks or RAID metadata.
  After reconnecting, upload it from the workstation with `scp extra.nix root@<ip>:/root/extra.nix`
  (or write `/root/extra.nix` in the live installer) before running `panda-install`; a missing
  extra-config file fails the install. Build the installer from the pinned release source and
  use `--build --extra-config /root/extra.nix`.
  Before building as root, verify the tag commit's signature or pin `?rev=<commit-sha>` as that
  guide describes. Include the agreed trust settings: `--untrusted-tenants false` for a trusted
  host, or the multi-tenant flags plus the `caddyPackage` (with its real hash computed by the dry
  build above) and `environmentFile` lines in `/root/extra.nix` before that build. Create the
  credentials file after the first boot, as above.
  Verify the network and boot entry before rebooting. Verify the new SSH host fingerprint with
  the user before replacing a known-hosts entry.

#### After first boot: mandatory rebuild and runtime checks

Log in with the configured user.

1. **Rebuild (mandatory for every `server` installed without `--build`).** The running system
   is still the prebuilt ISO system with the placeholder domain until this succeeds. It needs a
   network to fetch the locked flake inputs:

   ```sh
   sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
   ```

   Then reboot if the kernel parameters changed, for example when `--untrusted-tenants false`
   drops the prebuilt `nosmt` default, or boot arguments changed. Schedule the reboot with the
   user first:

   ```sh
   cmp -s /run/booted-system/kernel-params /run/current-system/kernel-params || sudo reboot
   ```

   `--build` installs already apply the flags; rebuild them only after changing `extra.nix`.
2. **Inspect the configuration.** `sudo nix eval` reads the *written configuration files*, not
   the running system, so it is not proof the settings are live. Evaluate options rather than
   reading `local.nix` (PandaOS omits `nixos-option`; `sudo` is needed because the flake imports
   the `0600` `local.nix`):

   ```sh
   cfg=/etc/pandaos#nixosConfigurations.pandaos.config
   sudo nix eval "$cfg.services.boat.untrustedTenants"
   sudo nix eval "$cfg.services.boat.gateway.dnsProvider"
   sudo nix eval "$cfg.pandaos.updates.mode"
   cat /etc/pandaos/extra.nix   # only your own overrides, if present; never print local.nix
   ```

3. **Verify the running system.** Substitute the chosen domain. Each command must succeed:

   ```sh
   systemctl is-active caddy boatd boat-api boat-gateway
   systemctl cat boat-gateway | grep -Fq "boat.example.com"
   cmp -s /run/booted-system/kernel-params /run/current-system/kernel-params
   curl -fsS https://api.boat.example.com/healthz   # once DNS points at the server
   ```

   A failing `grep` means the placeholder system is still active: rerun step 1.

Put any further server, network or update settings in `/etc/pandaos/extra.nix`, then repeat
steps 1 and 3. `extra.nix` is mode `0644` and is copied into the world-readable `/nix/store`:
**never put raw passwords, tokens, or API keys in it.** Reference root-only files instead, such
as `services.boat.gateway.environmentFile = "/var/lib/secrets/caddy-dns.env";`.

### B3. Install and verify guest images

On the server, download the guest kernel, one root filesystem, and the checksum manifest from
the [v0.2.0 release][release] into an empty directory. Choose `full` for browser desktops or
`slim` for headless guests. `boat_guest_SHA256SUMS` lists `boat-guest-vmlinux`,
`boat-guest-rootfs-slim.ext4.zst`, every `boat-guest-rootfs-full.ext4.zst.part*` and the
reassembled `boat-guest-rootfs-full.ext4.zst`. **When `gh` is installed, provenance is
required:** before importing, run
`gh attestation verify <file> --repo AICodeKing/pandaos --signer-workflow AICodeKing/pandaos/.github/workflows/release.yml --source-ref refs/tags/v0.2.0`
for `boat_guest_SHA256SUMS` and each downloaded `boat-guest-*` file, and stop if any fails.
Without `gh`, follow the checksum-only confirmation rule in [A1](#a1-download-verify-and-install).

**Slim (single file):**

```sh
base=https://github.com/AICodeKing/pandaos/releases/download/v0.2.0
curl -fLO "$base/boat-guest-vmlinux" &&
  curl -fLO "$base/boat-guest-rootfs-slim.ext4.zst" &&
  curl -fLO "$base/boat_guest_SHA256SUMS" &&
  sha256sum -c --ignore-missing boat_guest_SHA256SUMS &&
  sudo boat-admin images import boat-guest-vmlinux boat-guest-rootfs-slim.ext4.zst
```

**Full (2 split parts, each `< 2 GiB`, because of GitHub's release asset limit):**

```sh
base=https://github.com/AICodeKing/pandaos/releases/download/v0.2.0
curl -fLO "$base/boat-guest-vmlinux" &&
  curl -fLO "$base/boat-guest-rootfs-full.ext4.zst.part00" &&
  curl -fLO "$base/boat-guest-rootfs-full.ext4.zst.part01" &&
  curl -fLO "$base/boat_guest_SHA256SUMS" &&
  sha256sum -c --ignore-missing boat_guest_SHA256SUMS &&
  cat boat-guest-rootfs-full.ext4.zst.part* > boat-guest-rootfs-full.ext4.zst &&
  sha256sum -c --ignore-missing boat_guest_SHA256SUMS &&
  sudo boat-admin images import boat-guest-vmlinux boat-guest-rootfs-full.ext4.zst
```

Proceed only if every listed file reports `OK`; for `full`, the second check must also report
`OK` for the reassembled `boat-guest-rootfs-full.ext4.zst`. Then run `sudo boat-admin images list`.
After a successful import, free the downloaded files (about 5 GB for `full`) from the download
directory:

```sh
rm -f boat-guest-rootfs-full.ext4.zst* boat-guest-rootfs-slim.ext4.zst \
  boat-guest-vmlinux boat_guest_SHA256SUMS
```

Alternatively, declare the HTTPS URLs and their hashes under `services.boat.images` in
`/etc/pandaos/extra.nix` and rebuild. The `slim` release URL works there directly; for `full`,
host the reassembled file on your own HTTPS server. The
[guest-image guide](docs/boat.md#guest-images) has the declarative settings, source build,
and CI artifact options. If release assets are unavailable, explicitly check out the same
chosen tag or verified commit used for the ISO and CLI before building guest images from source.
From the repository root, `components/boat/guest/build.sh all` writes
`components/boat/guest/out/vmlinux` and
`components/boat/guest/out/rootfs-{slim,full}.ext4.zst`; the `boat-guest-*` names are
created by CI release packaging. If Docker ran off-server, transfer the kernel and chosen
rootfs to the server first, preserving these relative paths (or adjust the import paths).
On the server, import the source-built files:

```sh
sudo boat-admin images import components/boat/guest/out/vmlinux components/boat/guest/out/rootfs-full.ext4.zst
```

Use `rootfs-slim.ext4.zst` instead for headless guests.

### B4. Create the first owner key and test one VM

1. On the server, substitute the chosen names and run
   `sudo boat-admin init -user alice -team ops >/dev/null`. The redirection keeps the JSON
   containing the owner key out of captured stdout. The bare key is written to
   `/root/boat-owner-token` with mode `0600`. Have the user move the key into their password
   manager through a private terminal, confirm it was stored, then remove the temporary
   server copy with `sudo rm /root/boat-owner-token`.
2. On the user's Linux or macOS workstation, install the CLI from the same release. The script
   verifies the matching `boat_<os>_<arch>` binary against `boat_SHA256SUMS` before installing
   it to `~/.local/bin`. **When `gh` is installed, provenance is required:** download
   `boat_<os>_<arch>` (add `.exe` on Windows) and `boat_SHA256SUMS` from the
   [v0.2.0 release][release], run
   `gh attestation verify <file> --repo AICodeKing/pandaos --signer-workflow AICodeKing/pandaos/.github/workflows/release.yml --source-ref refs/tags/v0.2.0`
   on both, and stop if either fails. Without `gh`, follow the checksum-only confirmation rule
   in [A1](#a1-download-verify-and-install). This snippet works in `sh`, `bash`, and `zsh`:

   ```sh
   tmp=$(mktemp) &&
     curl -fsSL -o "$tmp" https://raw.githubusercontent.com/AICodeKing/pandaos/v0.2.0/components/boat/cli/install.sh &&
     BOAT_VERSION=v0.2.0 sh "$tmp"
   rc=$?
   rm -f "$tmp"
   [ "$rc" -eq 0 ]
   ```

   **Windows (untested).** The `boat_windows_{amd64,arm64}.exe` binaries and `install.ps1` are
   cross-compiled in CI and have not yet been run on Windows (see the
   [boat guide's honest gaps](docs/boat.md#honest-gaps)). Tell the user, then in PowerShell:

   ```powershell
   Invoke-WebRequest -UseBasicParsing -Uri https://raw.githubusercontent.com/AICodeKing/pandaos/v0.2.0/components/boat/cli/install.ps1 -OutFile .\install.ps1
   $env:BOAT_VERSION = "v0.2.0"
   powershell -ExecutionPolicy Bypass -File .\install.ps1
   ```

   The script checks the binary against `boat_SHA256SUMS`. To verify manually instead, download
   `boat_windows_<arch>.exe` and `boat_SHA256SUMS` from the [v0.2.0 release][release] and
   compare `(Get-FileHash .\boat_windows_<arch>.exe -Algorithm SHA256).Hash` (case-insensitive)
   with that file's line in `boat_SHA256SUMS`.

   If the release binaries are unavailable, [build the CLI from source](components/boat/cli/README.md),
   checking out the same explicitly chosen tag or verified commit used for the ISO and guest images.
3. Ensure the CLI is on PATH. Have the user feed the stored owner key to
   `boat login --url https://api.boat.example.com --key-stdin` from a trusted secret manager in
   a private shell. Substitute the actual API hostname. Run `boat whoami` to confirm the team.
4. Check `boat list` first. Use `smoke` only if that name is free, or substitute a fresh name
   throughout. Run the steps individually in a private terminal because `boat host` prints a
   signed URL. Use the VM ID from `boat new` for cleanup if name resolution is ambiguous.

   ```sh
   boat new --name smoke --no-auto-stop
   boat status smoke
   boat exec smoke -- uname -a
   boat host smoke 3000
   boat delete smoke --yes
   ```

   `boat host` verifies that a route can be created. To test the URL end to end, first start a
   listener on `0.0.0.0:3000` inside that VM and open the signed URL privately before deleting it.
   If a smoke check fails after creation, clean up the VM you created.

If the user also wants to use the server itself as a PandaCode computer or install host agent
CLIs, follow A2 and A3.

## Track C — Other PandaCode instances, teammates, and SDKs

- **More PandaOS computers in PandaCode.** On each box, run
  `sudo panda-connect sign-in --device-name <unique-name>` and complete the GitHub approval in A2.
  Use the same GitHub account in each PandaCode app you want to see the computers from. Connect
  uses outbound HTTPS and needs no inbound ports.
- **PandaCode over SSH to a boat VM.** On the workstation running PandaCode (which may differ
  from the B4 workstation), first install the `boat` CLI as in B4 and log in with
  `boat login --url https://api.<domain> --key-stdin`, supplying the key privately. Ensure this
  workstation has a local SSH public key, such as `~/.ssh/id_ed25519.pub`; if none exists,
  generate one with `ssh-keygen -t ed25519`. Use `boat list` to select an existing **running** VM,
  start a stopped VM, or create one with `boat new --name dev --no-auto-stop` using a free name.
  B4 deletes `smoke`, so it cannot be assumed to exist. With these prerequisites met, run
  `boat ssh-config <vm>`. It writes an entry under `~/.ssh/boat/`. Add
  `Include ~/.ssh/boat/*.conf` at the top of `~/.ssh/config` once. For a one-time direct
  append instead, use `boat ssh-config <vm> --print >> ~/.ssh/config`. The entry tunnels
  through `ssh.<domain>:2222`. Test `ssh <vm-name>` (the entry is `Host <id> <name>`), then add that same SSH host in
  PandaCode. Regenerate the config after a resume if the VM address changes.
- **Dashboard and team keys.** Open `https://api.<domain>/`. Create scoped keys with
  `boat api-key create ci-reader --preset read-only --ttl 24h` or the appropriate preset
  for the task. Run key creation in a private terminal and store each new key securely.
  [Review the tenant model](docs/boat.md#tenant-model) before granting other people access.
- **SDKs.** Set the official Python `boat-sdk` configuration's `host` or TypeScript
  `@boatdev/sdk` configuration's `basePath` to `https://api.<domain>/api/v1`. Supply
  a scoped API key from a secret manager. See the [SDK examples](docs/boat.md#quick-start)
  and [API compatibility limits](docs/boat.md#boatdev-api-compatibility-apiv1).
- **Updates.** v0.2.0 ships with the published [release-signing key](SECURITY.md#release-signing-key)
  pre-configured in `pandaos.updates.allowedSigners`. The daily signed update is verified and
  applied automatically on `headless`, `desktop`, and trusted `server` hosts
  (`--untrusted-tenants false`). Multi-tenant servers (`--untrusted-tenants true`) default to
  `pandaos.updates.mode = "staged"`: the update is verified and built daily but applied only when
  the operator runs `sudo panda-update run`. Verify its fingerprint against `SECURITY.md`
  (`SHA256:YSNtMOh+z7ZRA5E0sH2yAtbY+uxUP2cCbEzpbqa3L3o`) with
  `printf '%s\n' 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO8zyQo+fbC0tMzI4gvW6ZICS0p8OVcmeYNGJREXBlNG' | ssh-keygen -lf -`.
  Set `pandaos.updates.mode = "manual";` in `/etc/pandaos/extra.nix` to disable the timer while
  managing updates by hand. Manual channel updates still require a valid signature by default.
  See [Updates and rollback](docs/updates-and-rollback.md#signed-releases-required).

## Verification and troubleshooting

Run the checks for the selected tracks. Share a redacted result for each, the installed profile,
the source revision recorded in `/etc/pandaos/VERSION`, and any pending human steps. Confirm
settings with `sudo nix eval` as in [B2](#b2-install-the-server) or from `/etc/pandaos/extra.nix`;
never print `/etc/pandaos/local.nix`.

| Check | Expected result and where to look |
|---|---|
| `panda-health` | Core units are healthy. Use `panda-health --route` to also check the default route and `systemctl --failed` to identify failed units. |
| `panda-connect status --json` | `signedIn` is `true`, `service` is `active`, and `deviceName` is the chosen name. Check with the `grep` commands in [A2](#a2-connect-to-pandacode) (`jq` is not installed). For sign-in, the user can check `sudo journalctl -u pandacode-sign-in -o cat -b` privately. For host errors, use `sudo journalctl -u pandacode-host -b`. |
| `sudo panda-agents doctor --user agent` | MCP registrations and runtime files are ready. After configuration changes, run `sudo panda-agents sync --user agent` and check `sudo journalctl -u panda-agents-sync -b` if needed. |
| `boat status <vm>` | The test VM is running before deletion. For VM or edge failures, check `sudo journalctl -u boatd -u boat-api -u boat-gateway -u caddy -b` on the server. |
| `sudo boat-admin images list` | The image is present and offered by the API. For declarative images, check `sudo journalctl -u boat-images -b`. |
| `panda-update status` | Verify the update mode (`automatic`, or `staged` on multi-tenant servers, which also shows any staged release to apply with `sudo panda-update run`), signing state and last update. Check `sudo journalctl -u nixos-upgrade -b` for failures. |

[release]: https://github.com/AICodeKing/pandaos/releases/tag/v0.2.0
[latest]: https://github.com/AICodeKing/pandaos/releases/latest
