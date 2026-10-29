# PandaOS compared with other systems

This page has two tables. The first one is measured: each OS boots as a VM in the same CI job, and
every number comes from that run. The second one compares features. It is our reading of each
system's defaults, and nothing in it is measured.

## Measured: idle footprint in identical VMs

Run [40909717381](https://github.com/AICodeKing/pandaos/actions/runs/40909717381) on 2026-10-29,
from [`.github/workflows/compare.yml`](../.github/workflows/compare.yml) (manual and weekly). The raw
results are in [`bench/compare/results/2026-10-29.json`](../bench/compare/results/2026-10-29.json).
Sizes are MiB.

| OS | Idle RAM, `free` used + shmem | Idle RAM, MemTotal − MemAvailable | Processes, all (userland) | Running services | Disk used on `/` | Image download | Boot to ssh | First-boot setup done |
|---|---|---|---|---|---|---|---|---|
| **PandaOS headless** (26.05, kernel 6.18) | 148 MB | 282 MB | 119 (26) | 14 | 1889 MB | 1009 MB¹ | 16.6 s | 25.4 s |
| Ubuntu Server 24.04.5 LTS (6.8) | 159 MB | 305 MB | 123 (23) | 18 | 1660 MB | 593 MB | 16.8 s | 17.8 s |
| Ubuntu Server 26.04.1 LTS (7.0) | 188 MB | 335 MB | 133 (26) | 19 | 1950 MB | 819 MB | 17.1 s | 18.6 s |
| Debian 13 genericcloud (6.12 cloud kernel) | 86 MB | 219 MB | 110 (17) | 12 | 595 MB | 325 MB | 14.5 s | 15.0 s |
| Fedora Cloud 44 (6.19, btrfs root) | 171 MB | 307 MB | 130 (24) | 15 | 679 MB | 557 MB | 14.4 s | 14.9 s |
| Alpine 3.24.3 cloud (6.18 virt, musl, OpenRC) | 78 MB | 189 MB | 98 (20) | 25² | 114 MB | 150 MB | 25.2 s | 26.3 s |

¹ PandaOS has no cloud image to download. This number is the size of the compressed qcow2 built for
this benchmark. The headless ISO, which is what you actually download, is 855 MB.
² This is OpenRC's `started` count from `rc-status -a`. It includes boot-runlevel one-shots, which
systemd does not list as running, so it cannot be compared with the systemd rows.

**Where PandaOS stands.** On RAM it is in the middle of the pack. It uses less than both Ubuntu
releases and Fedora, and clearly more than Debian genericcloud and Alpine. Debian uses about 60 MB
less by the `free` measure and Alpine about 70 MB less. On disk, PandaOS is the second largest after
Ubuntu 26.04. It is about 3× Debian or Fedora and about 16× Alpine. Its Nix store keeps a full
closure (agent services, PandaCode Connect, curated firmware) and is not compressed on this ext4
image. For a small footprint, Debian or Alpine is the better choice.

### How it is measured

- **Same VM for every OS:** QEMU + KVM on one GitHub-hosted `ubuntu-latest` runner, in one job and
  one after another. Each VM gets 2 vCPU (`-cpu host`), 2048 MB RAM, a q35 machine, UEFI (OVMF,
  Secure Boot off), a 20 GB virtio qcow2 overlay on the pristine image, and qemu user-mode
  networking with ssh forwarded to the host.
- **Stock images,** pinned by URL and checked against each project's published SHA256 or SHA512
  sums ([`bench/compare/images.json`](../bench/compare/images.json)). Every VM gets the same
  cloud-init NoCloud seed, which only installs an ssh key for the image's default user. Nothing else
  is changed.
- **PandaOS headless** is `nixosConfigurations.headless`, the system that panda-install copies to
  disk, built into a qcow2 by [`bench/compare/pandaos-image.nix`](../bench/compare/pandaos-image.nix).
  It differs from an installed box in three ways. The disk is ext4 + ESP instead of btrfs
  subvolumes. The interactive first-boot wizard is off, as in the VM tests. The ssh key is set in
  Nix, because PandaOS has no cloud-init. PandaCode Connect (`pandacode-host`) is enabled but **not
  signed in**, and the optional `panda-agentd` is off (its default).
- **Boot to ssh:** wall-clock time from starting qemu until `ssh … true` succeeds.
  **First-boot setup done:** the time when cloud-init has finished, if the OS has it, and
  `systemctl is-system-running --wait` has returned. PandaOS reaches this point about 9 s after ssh,
  because its startup jobs are still finishing.
- **RAM:** after setup is done, the VM idles for 60 s. The harness then takes 3 readings of
  `/proc/meminfo`, 10 s apart, and reports the median. Both figures are computed on the host from
  the raw meminfo text, the same way for every OS: `MemTotal − MemFree − Buffers − Cached −
  SReclaimable + Shmem`, and `MemTotal − MemAvailable`.
- **Processes:** every `/proc/<pid>`. The number in brackets counts only the ones with a non-empty
  cmdline, which leaves out kernel threads.
  **Services:** `systemctl list-units --type=service --state=running` on the systemd systems.
  **Disk:** the Used column of `df -Pk /`.
- None of the systemd systems had a failed unit.

### Caveats

- **These are VMs, not hardware.** The host is a shared cloud runner, so timings can vary from run
  to run by a second or more. On this host, everything except Alpine boots to ssh in 14 to 17 s.
- **One run.** The weekly schedule will show how much the numbers vary. Ubuntu 24.04 was still
  settling during sampling: its 3 readings were 172, 159 and 145 MB.
- **The cloud images are minimal server builds.** They have cloud kernels (Debian), no firmware
  packages, and no agent stack. PandaOS includes its agent services (panda-mem, panda-ckpt,
  panda-sys, pandacode-host), NetworkManager, curated firmware and a repair toolkit. The comparison
  is "what you get by default", not "the same software on different distros".
- **Alpine** uses musl and OpenRC, not glibc and systemd. Its service count is not comparable (see
  note ²).
- **Fedora** uses btrfs with zstd compression on `/`, which makes its disk number smaller. PandaOS
  installs also use btrfs with zstd (`compress=zstd:1`), but this benchmark image uses ext4, so
  PandaOS's `/` on a real install should be smaller than 1889 MB. That has not been measured here.
- **PandaOS's own CI** reports 113 MB / 233 MB idle in the NixOS test VM (1 GB, direct kernel boot,
  host store over 9p, agentd on). The numbers on this page are higher (148 / 282 MB) because the VM
  boots from a real disk image through UEFI and has 2 GB of RAM. Compare PandaOS with the other
  systems using this page, not those CI numbers.

## Features (qualitative, not measured)

"DIY" means the system has the building blocks, but you set them up yourself. The Windows 11 and
macOS columns are for reference only. We did not measure them.

| | PandaOS | Ubuntu Server | Debian | Fedora Cloud/Server | Alpine | Windows 11 | macOS |
|---|---|---|---|---|---|---|---|
| Agents run as a separate, non-admin user | Yes (`agent`, not in wheel) | DIY | DIY | DIY | DIY | DIY | DIY |
| Per-task cgroups, OOM kills one task and not the host | Yes (task-* cgroups, oomd) | DIY (systemd-run) | DIY | DIY | DIY | No | No |
| Workspace checkpoints / restore | Yes (panda-ckpt on btrfs, MCP) | DIY (LVM/ZFS/btrfs snapshots) | DIY | DIY (btrfs) | DIY | No (System Restore is system-level) | Time Machine / APFS snapshots, not per workspace |
| Shared agent memory over MCP | Yes (panda-mem) | No | No | No | No | No | No |
| MCP servers synced into agent CLIs | Yes (mcp.d) | No | No | No | No | No | No |
| Automatic updates | Daily, whole system | Security updates (unattended-upgrades) | Optional (unattended-upgrades) | Optional (dnf-automatic) | No | Yes (Windows Update) | Yes |
| Rollback after a bad update | Automatic, health-checked (systemd-boot fallback) | Manual (old kernel in GRUB) | Manual | Manual (Atomic/CoreOS editions: yes) | Manual | Manual (uninstall update, recovery) | Manual (recovery) |
| Offline installer | Yes (ISO, `panda-install`) | Yes | Yes | Yes | Yes | Yes | Recovery / USB installer |
| Remote agent access (PandaCode Connect) | Built in (sign-in required) | Install it yourself | Install it yourself | Install it yourself | Install it yourself (musl) | Install it yourself | Install it yourself |
| Legacy BIOS boot | **No** (UEFI only, Secure Boot off) | Yes | Yes | Yes | Yes | No (UEFI + TPM 2.0) | n/a (Apple hardware) |
| Secure Boot | **No** | Yes | Yes | Yes | No | Required | Apple's own |
| Maturity | **Pre-release:** no release tag, tested only in QEMU VMs | LTS, 5+ years of support, certified hardware | Long-standing stable releases | About 13 months per release | Long-standing stable releases | Very broad hardware support | Apple hardware only |
| Idle RAM | measured above | measured above | measured above | measured above | measured above | 4 GB minimum (Microsoft's published requirement, not a measurement) | No published minimum; runs only on supported Macs |

**Where PandaOS loses.** It is pre-release software. It has no release tag, it is not tested on
real hardware, and it has no BIOS or Secure Boot support. Every other system here has years of
releases and wide hardware coverage behind it. On idle RAM and disk, Debian and Alpine are clearly
smaller. PandaOS's advantage is the agent features that ship by default. On the other systems, you
would assemble those features yourself.
