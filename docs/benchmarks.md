# Benchmarks

The OS footprint, boot, round-trip and installer numbers on this page come from the NixOS VM tests and
host-side checks in `tests/`, run by CI with `nix flake check` on nixos-26.05 inside QEMU VMs on GitHub
runners. Each test prints a `METRIC {json}` line in its build log and writes the same object to
`$out/metrics.json`; `bench/collect.py` gathers them into a table.

For benchmarks measured outside `tests/`:
- **Dedicated server hardware (`boat` / server profile):** [`cost.md`](cost.md) and
  [`bench/boatbench/results/`](../bench/boatbench/results/) publish real-hardware Firecracker microVM
  CPU, disk, network, lifecycle and multi-VM density measurements on a dedicated Hetzner AX42 (both
  Ubuntu loop-file and native-btrfs PandaOS 26.05 installs). **Provenance & caveats:** (1) the checked-in
  AX42 `trusted` lifecycle baselines (`0d160dd`: `0.15s` resume, `0.30s` fork, `0.32s` warm create,
  `0.89s` cold create, with `Unsafe` drive cache / `warmPool`) predate `-ssh-policy-helper-file` across
  presets — Round 23–24 batch SSH policy (`ssh-policy-batch`) and non-disruptive `rotate-token` are
  unit-tested to avoid `sshd` restarts and `nft` reloads on steady-state token refreshes, while live
  AX42 timing for both initial cold/restore SSH policy bootstrap and `rotate-token` awaits a fresh
  AX42 capture; (2) `bench/boatbench/presets.json` and `bench/boatbench/stack.sh` `untrusted`
  configuration (now setting `memOvercommit: 1` alongside `jailGid` `io_uring`, `losetup --direct-io=on`,
  and online SSH quarantine) is fixed and validated offline against `boatd`'s `loadConfig()` checks
  (alongside `components/boat/boatd/e2e-perf.sh`), while the live rebooted PandaOS AX42 `untrusted` run
  (`nosmt=force`, `memOvercommit: 1`, `blk-iocost`, `jailGid` `io_uring`, online SSH quarantine) remains
  pending an owner-approved hardware reboot; and (3) live `boat.dev` remains unmeasured because
  `BOATDEV_API_KEY` was not provided.
- **Cross-distro idle footprint comparison:** [`comparison.md`](comparison.md) and
  [`bench/compare/`](../bench/compare/) measure PandaOS headless alongside Ubuntu Server 24.04/26.04,
  Debian 13, Fedora Cloud 44 and Alpine 3.24 in identical QEMU/KVM VMs.

## Current results

| Metric | headless | desktop |
|---|---|---|
| Idle RAM, `free` used + shmem (`idleRamMB`) | 128 MB | 138 MB at the greeter |
| Idle RAM, `MemTotal - MemAvailable` (`memTotalMinusAvailableMB`) | 247 MB | 279 MB at the greeter |
| Idle RAM in a logged-in labwc session (dock running) | n/a | pending CI |
| Test VM system closure (`boot-*` tests, `closureMB`) | 1321 MB | 2847 MB |
| Installed system closure (`nixosConfigurations.*`, `closures` check) | pending CI | pending CI |
| ISO (`isoMB`, CI `iso` job) | 608 MB | 993 MB |

| Agent job round trip (`roundtripSec`) | |
|---|---|
| `agentd-roundtrip`, normal VM network | 3.7 s |
| `slow-network`, 1 Mbit/s, 300 ms, 2% loss, box's outgoing traffic only | 8.6 s |
| `slow-network`, the same, shaped in both directions | pending CI |

| Install (`installSec`) | |
|---|---|
| `install`: `panda-install --yes` of the headless system onto an empty 10 GiB virtio disk | 208 s |

Sizes are MiB. These numbers are from CI run
[40733737472](https://github.com/AICodeKing/pandaos/actions/runs/40733737472), nixpkgs `718f670`.

"Pending CI" marks a measurement whose test is being changed or added and has no CI result yet.
The tests fail when a metric goes over its budget in `tests/budgets.json`. The budgets are
deliberately loose for now.

## Idle RAM: two metrics

Both are read from `/proc/meminfo` after `multi-user.target` and a **30 s settle**. The headless test
runs in the default 1 GB test VM; the desktop test runs in a 2 GB VM.

**The desktop numbers above were measured at the greeter**: `greetd` and `tuigreet` up, nobody
logged in, so no labwc, waybar or dock was running. They are not the RAM of a desktop in use. A
measurement of a logged-in labwc session is being added; until CI reports it, it is listed as pending.

**`idleRamMB`, `free` used + shmem (budgeted):**

```
MemTotal - MemFree - Buffers - (Cached - Shmem) - SReclaimable
```

This is what `free(1)` calls *used*, plus shmem/tmpfs (which `free` counts as cache, but which cannot
be dropped). It counts anonymous memory, unreclaimable slab, kernel stacks and page tables, and leaves
out reclaimable page cache.

**`memTotalMinusAvailableMB`, `MemTotal - MemAvailable` (reported):** the common "used" figure that
other tools print. It is higher because it also counts the kernel's free-page watermark reserve
(about 80 MB on a 1 GB VM, and it grows with RAM). So it partly measures the VM size, not the OS,
and it is not budgeted.

Report both when you compare with another system, and state the RAM size of the VM.

## Closure size

The published `closureMB` so far is the NAR size of the **test VM's** system closure, the same number
as `nix path-info -S`. It is computed on the host from `closureInfo`, because the test VM does not
register its own closure. It includes the NixOS test-driver instrumentation, so it is not the
installed size.

The `closures` check (being added) measures what `panda-install` actually puts on disk: the closures
of `nixosConfigurations.headless` and `.desktop`, with no test instrumentation. It also reports
`updateBytes`, the NAR size of the store paths a box would fetch for an update that only changes
panda-agentd (an upper bound, since downloads are compressed). A nixpkgs bump fetches far more and is
not measured.
The CI `iso` job prints each ISO's size as a `METRIC {"test": "iso-<profile>", "isoMB": ...}` line.
The desktop ISO is bigger partly because it carries both prebuilt systems, so it can install either. On failure, the test also prints the 40
largest store paths and what refers to them.

## Round trip and netem

Both tests use two VMs on a test VLAN: `relay` runs `fake-relay` on port 8787, and `box` runs
`panda-agentd` pointed at `http://relay:8787`. The timer starts when `box` reaches `multi-user.target`
and stops when the relay has the job's result. It covers **pairing plus one job** (`echo hi`).

`slow-network` holds `panda-agentd` back, applies netem on the box's test interface, and only then
starts the agent, so pairing also runs over the bad link:

```sh
tc qdisc add dev eth1 root netem delay 300ms loss 2% rate 1mbit
```

**netem on `eth1` shapes only the box's outgoing traffic.** The published 8.6 s was measured that
way: requests from the box were delayed, rate-limited and dropped, but replies from the relay were
not. The test is being changed to shape both directions (for example netem on the relay's interface
too); its result is pending CI. The test fails if the round trip takes longer than 120 s.

## Reproduce

```sh
nix build .#checks.x86_64-linux.boot-headless -L -o result-boot-headless 2>&1 | tee boot-headless.log
python3 bench/collect.py result-* *.log
```

The nightly workflow (`.github/workflows/nightly.yml`) runs every check one by one and uploads the
results as an artifact. VM numbers vary a little between runs and runners.

## Cross-distro and dedicated-hardware benchmarks

- **Headless OS comparison (built and published):** [`bench/compare/`](../bench/compare/) boots stock
  cloud images of Ubuntu Server 24.04 LTS, Ubuntu Server 26.04 LTS, Debian 13, Fedora Cloud 44 and
  Alpine 3.24 alongside `nixosConfigurations.headless` in identical 2 vCPU / 2 GiB KVM VMs and records
  both idle-RAM metrics, process/service counts, root disk usage and boot-to-SSH / first-boot times.
  See [`comparison.md`](comparison.md) and [`bench/compare/results/`](../bench/compare/results/).
  Desktop cross-distro runs (PandaOS desktop vs Ubuntu Desktop) are still planned.
- **Dedicated server microVM benchmarks (built and published):** [`bench/boatbench/`](../bench/boatbench/)
  measures CPU, disk (`Unsafe` vs `Writeback` durable flushes, pinned vs unpinned virtio-blk IRQ),
  network, VM lifecycle and packed-host density on a dedicated Hetzner AX42, plus TCO / retention
  tables from [`bench/boatbench/prices.json`](../bench/boatbench/prices.json). Caveats: the committed
  AX42 `trusted` lifecycle baselines (`0d160dd`: `0.15s` resume, `0.30s` fork, `0.32s` warm create)
  predate `-ssh-policy-helper-file` across presets (Round 23–24 batch SSH policy + non-disruptive
  `rotate-token` are unit-tested with live AX42 timing awaiting a fresh capture); `stack.sh`'s `untrusted`
  config (including `memOvercommit: 1`) is fixed and validated offline against `boatd`'s `loadConfig()`,
  while live rebooted PandaOS `untrusted` (`nosmt=force`, `memOvercommit: 1`, `blk-iocost`, `jailGid`
  `io_uring`, online SSH quarantine) remains pending hardware re-run; and live `boat.dev` remains
  unmeasured (`BOATDEV_API_KEY` not provided). See [`cost.md`](cost.md) for full
  methodology and `trusted` vs `untrusted` provenance notes.
  Vendor reference (published by boat.dev / StarSling, not measured by us): the Node.js web-tooling
  figure is now **28.06 index units** (StarSling run 37073680030, 2026-10-03, 4 vCPU / 8 GiB guest on a
  Ryzen 9 9950X, median of 10 trials on 5 sandboxes, 95% interval 23.27-28.22, run marked partial,
  [leaderboard](https://github.com/starslingdev/hpc-sandbox-benchmarks/blob/297198a96ee827df5edcd918bd37b949526cfe20/LEADERBOARD.md)).
  It replaces **27.67 index units** (AnicetNgrt harness, 2026-08-24, 21 trials, CI 26.01-28.34), kept as a
  dated historical note in `published.json`; the two are within each other's intervals. The three
  fio disk figures are still from the August run. The cost-per-index-unit reference is now $0.000356 per
  1000 index units (was $0.000361). boat.dev's AI credentials proxy fee ($1 per UTC day on + $1 per 1,000
  requests; PandaOS's proxy is free) is modeled in `cost.md`.
