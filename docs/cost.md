# Cost: self-hosted boat vs boat.dev

All prices were read from public list-price pages on **2026-11-02**. They exclude VAT and are in USD unless marked. EUR prices are converted at **1.12 USD/EUR**, an approximate October 2026 spot rate ([30rates](https://30rates.com/eur-usd-forecast-tomorrow-this-week)) that was not checked for today. Unverified numbers are marked *(unverified)*. The machine-readable copy is [`bench/boatbench/prices.json`](../bench/boatbench/prices.json).

## Headline

One `default` VM (4 vCPU / 8 GiB), cost per VM-hour. Self-hosted figures include amortized setup and backup.
- **Conservative / untrusted** rows use `memOvercommit` 1.0. That is the setting `services.boat.untrustedTenants` forces, so **a public boat.dev alternative, which hosts strangers, runs these rows.**
- **Default** rows use boatd's default of 1.5, which is a bet that guests stay idle (see [Load](#what-happens-under-load)). They are only for one trust domain (your own agents).

No server in this table has been benchmarked. The VM counts are boatd's admission rule applied to an *estimated* `MemTotal` ([§4](#4-model)), not measurements on that hardware.

| | 10% util. | 25% | 50% | 100% | + operator time, 100% | Break-even vs boat.dev |
|---|---|---|---|---|---|---|
| **boat.dev** (pay per second) | $0.036 | $0.036 | $0.036 | $0.036 | n/a | n/a |
| **Hetzner AX102**, conservative / untrusted (15 VMs) | $0.279 | $0.112 | $0.056 | **$0.028** | $0.042 | 8,492 VM-h/mo (78% full); **never** with operator time (116%) |
| **Hetzner AX102**, default (22 VMs) | $0.190 | $0.076 | $0.038 | **$0.019** | $0.028 | 53% full; 79% with operator time |
| **OVH US ADVANCE-3**, conservative / untrusted (7 VMs) | $0.552 | $0.221 | $0.110 | $0.055 | $0.085 | never (153%) |
| **OVH US ADVANCE-3**, default (11 VMs) | $0.351 | $0.141 | $0.070 | $0.035 | $0.054 | 98% full; never with operator time (150%) |
| **Latitude.sh m4.metal.medium**, conservative / untrusted (15 VMs) | $0.422 | $0.169 | $0.084 | $0.042 | $0.056 | never (117%) |
| **Latitude.sh m4.metal.medium**, default (22 VMs) | $0.288 | $0.115 | $0.058 | $0.029 | $0.038 | 80% full; never with operator time (106%) |
| Hetzner AX42, conservative / untrusted (7 VMs) | $0.233 | $0.093 | $0.047 | $0.023 | $0.053 | 3,307 VM-h/mo (65% full); never with operator time (146%) |
| Hetzner AX42, default (11 VMs) | $0.148 | $0.059 | $0.030 | $0.015 | $0.034 | 41% full; 93% with operator time |
| E2B / Daytona / Modal / Vercel | | | | | | $0.22–$0.68/h, see below |

**What the table shows.** At $0.036/h for 4 vCPU / 8 GB, boat.dev costs about as much as running the hardware yourself. In untrusted mode (no RAM overcommit), self-hosting comes out cheaper only on Hetzner, only when the box is more than 65–78% full around the clock, and only if your own time is free. At the default overcommit, which is for trusted guests only, the AX102 and AX42 beat boat.dev even with operator time, at 79% and 93% full, but those VM counts hold only while guests stay mostly idle. Compared with the other sandbox providers (about $0.33–$0.48 per hour), an AX102 is about 6–9× cheaper at 50% utilization with no overcommit. You self-host for control, data locality, no per-start or plan limits, and custom images and kernels, not to undercut boat.dev. The [verdict](#verdict) says where the measurements stand.

## 1. boat.dev pricing

Source: <https://docs.boat.dev/pricing.md>, accessed 2026-11-02. (`https://boat.dev/pricing` returns 404.) Billing is per second while a sandbox runs, at these hourly rates:

| Size | vCPU / RAM / disk | $/hour | $/second | 730 h month |
|---|---|---|---|---|
| small | 2 / 4 GB / 51 GB | $0.018 | $0.000005 | $13.14 |
| default | 4 / 8 GB / 51 GB | $0.036 | $0.00001 | $26.28 (page: "$26") |
| large | 8 / 16 GB / 111 GB | $0.072 | $0.00002 | $52.56 |
| xlarge | 16 / 32 GB / 251 GB | $0.200 | $0.0000556 | $146.00 (requires the $100 plan or higher) |

- **Plans.** Monthly plan prices are $20, $100, $500 and $2,000. The $20 plan is the minimum, and each plan's price comes back as included VM time: 555 / 2,777 / 13,888 / 55,555 `default`-hours, or about 2.0M / 10.0M / 50.0M / 200M default VM-seconds. Included time expires at the end of each period. Organizations pay the plan price per seat.
- **Limits per plan** ($20 / $100 / $500 / $2,000):
  - Concurrent sandboxes: 100 / 300 / 1,000 / 2,000.
  - Starts per minute/hour/day: 12/60/200, 30/210/840, 65/420/1,680 and 90/600/2,400.
  - Saved snapshots included: 10 / 50 / 250 / 1,000.
- **Overage** (paid from the wallet):
  - VM time is billed at the size rates above.
  - Saved snapshots beyond the included count cost $0.056 each per day ($1.68 per 30 days).
  - Extra starts cost $0.03–$0.07 each.
  - The AI credentials proxy costs $1 per day plus $1 per 1,000 requests.
- **Included at no charge:**
  - Automatic snapshots, kept 14 days after a stop.
  - Egress.
  - Public IPv6.
  - Public URLs.
- **Not stated:** a per-GB snapshot storage price. A stopped sandbox keeps its disk for 14 days.
- **Other terms:** No GPUs. 7-day trial (card required). Wallet bonus of 2–25% on payments of $200 or more.

## 2. Other sandbox providers, about 4 vCPU / 8 GB

| Provider | List rates | 4 vCPU / 8 GB per hour | Plan fee, caveats |
|---|---|---|---|
| [E2B](https://e2b.dev/pricing) | $0.000014/vCPU-s, $0.0000045/GiB-s, disk free | **$0.331** | Pro costs $150/mo and is needed for sessions over 1 h (Hobby max 1 h, 20 concurrent). |
| [Daytona](https://www.daytona.io/pricing) | $0.0504/vCPU-h, $0.0162/GiB-h, $0.000108/GiB-h disk after 5 GiB | **$0.336** with 51 GB disk ($0.331 without) | $200 free compute. Limits are not published. |
| [Modal Sandbox](https://modal.com/pricing) | $0.00003942/core-s (1 core = 2 vCPU), $0.00000667/GiB-s | **$0.476** (2 cores + 8 GiB) | Starter $0 with $30/mo credit, Team $250/mo. Sandbox rates are 3× function rates. |
| [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox/pricing) (Pro, iad1) | $0.128/active-CPU-h, $0.0212/GB-h provisioned memory, $0.60/1M creations | **$0.221** at 10% CPU, **$0.682** at 100% | **Not like-for-like:** CPU is billed only while busy, so cost depends on the workload. Pro $20/mo credit. 8 vCPU / 16 GB max on Pro. 24 h session max. |

E2B and Daytona bill by the resource you size, so they are roughly like-for-like. Modal "cores" are physical cores. Vercel ties memory to vCPUs (2 GB each) and charges CPU only while it is active. None of these four bills idle time differently from boat.dev except Vercel's CPU.

## 3. Dedicated servers

| Server | CPU | Cores/threads | RAM | NVMe | Network | Price/mo | Setup | Source |
|---|---|---|---|---|---|---|---|---|
| Hetzner AX42 | Ryzen 7 PRO 8700GE | 8 / 16¹ | 64 GB DDR5 ECC | 2× 512 GB | 1 Gbit/s, unlimited traffic | €99.00 ($110.88) | €49 ($54.88) | [ax42](https://www.hetzner.com/dedicated-rootserver/ax42/) |
| **Hetzner AX102** | Ryzen 9 7950X3D | 16 / 32¹ | 128 GB DDR5 ECC | 2× 1.92 TB DC | 1 Gbit/s, unlimited | €259.00 ($290.08) | €129 ($144.48) | [ax102](https://www.hetzner.com/dedicated-rootserver/ax102/) |
| Hetzner AX162 | EPYC 9454P | 48 / 96¹ | 128 GB (up to 512) | 2× 1.92 TB DC | 1 Gbit/s, unlimited | €614.00 ($687.68) | €304 ($340.48) | [ax162](https://www.hetzner.com/dedicated-rootserver/ax162/) |
| **OVH US ADVANCE-3 (2024)** | EPYC 4464P | 12 / 24 | 64 GB DDR5 ECC | 2× 960 GB | 3 Gbit/s unmetered | $255.00 | $255.00 | [us.ovhcloud.com](https://us.ovhcloud.com/bare-metal/advance/) |
| **Latitude.sh m4.metal.medium** | EPYC 9124 | 16 / 32¹ | 128 GB | 2× 480 GB + 2× 1.9 TB | 2× 10 Gbit/s, 20 TB egress | $456.00 | $0 *(unverified)* | [latitude.sh/pricing](https://www.latitude.sh/pricing) |

¹ Thread counts come from AMD's SMT specifications. The vendor pages say only "SMT".

**Bold** rows are the three representative picks: EU value, US budget and US premium.

Other notes:
- **Hetzner in 2026.** Hetzner raised prices on 2026-04-01, changed setup fees on 2026-04-29, and restructured its catalog on 2026-06-15 ([statement](https://www.hetzner.com/pressroom/standardization-and-price-adjustment-of-our-server-products/), [price table](https://docs.hetzner.com/de/general/infrastructure-and-availability/price-adjustment/)). The table uses the live product-page prices. They match the official table's AX42-1 / AX102-1 prices (€115.79 / €306.19 including 19% VAT and excluding IPv4) plus €1.70 for the primary IPv4. The matrix page also shows lower "from" prices (€79 / €159 / €319), which are presumably the `-Ltd` variants *(unverified)*.
- **OVH EU.** The EU Advance page shows only VAT-inclusive prices, for example ADVANCE-3 at $283.20 including VAT ([ovhcloud.com](https://www.ovhcloud.com/en/bare-metal/advance/)). OVH's setup fee equals one month's price.
- **Hetzner US.** Hetzner's US locations are cloud-only, with no dedicated servers.
- **Extra IPv4.** Hetzner charges €1.70/mo plus €4.90 setup per address (AX102 page). OVH US charges $2.00/mo with no setup fee ([OVH US blog](https://us.ovhcloud.com/resources/blog/internet-protocol-version-4-ipv4-pricing-adjustments)). The model needs none: VMs are reached over IPv6 and gateway URLs, as on boat.dev. If you give each VM a public IPv4, an AX102 with 15 VMs costs 15 × €1.70 = €25.50/mo more (about +$0.003 per VM-hour at 100% utilization).
- **Backup.** Hetzner charges €3.20/mo for a 1 TB Storage Box BX11 ([storage-box](https://www.hetzner.com/storage/storage-box/)). For the US servers I assumed $6/mo for 1 TB of S3-compatible storage *(unverified)*.

## 4. Model

**VM count.** The counts follow boatd's own admission check ([`manager.go`](../components/boat/boatd/manager.go) `quotaLocked`, [`perf.go`](../components/boat/boatd/perf.go) `realMemCheck`). [`bench/boatbench/admission.py`](../bench/boatbench/admission.py) reads the constants from boatd's `defaultConfig()` in [`config.go`](../components/boat/boatd/config.go), and `test_boatbench.py` fails if they or the formula in `manager.go` change, so the numbers here cannot drift from boatd again:
- RAM: each active VM counts its RAM **plus `vmOverheadMiB` (256)**, the VMM's share of its cgroup. The sum may not exceed (`MemTotal` − `hostReserveMiB` 1024) × `memOvercommit`. A `default` VM therefore counts 8,448 MiB, and VMs = ⌊(MemTotal − 1024) × overcommit / 8448⌋.
- `MemTotal` is not the RAM sold. The kernel's `struct page` array (64 bytes per 4 KiB page, 1/64 of RAM) and firmware-reserved ranges are never in it. The model takes **MemTotal ≈ RAM × 63/64 − 256 MiB**, so 128,768 MiB on a 128 GB box. The firmware part is an *(unverified)* estimate. Nothing was measured on these servers, so check `/proc/meminfo` on yours.
- CPU: active vCPUs may not exceed host threads × `cpuOvercommit` (default 4.0).
- Above overcommit 1.0, a new VM also needs `MemAvailable` ≥ 1024 + 8448 / overcommit MiB (6,656 MiB at 1.5). boatd refuses a create (503) if the host is short of real memory, however few VMs are running.

| Server | VMs at 1.0 (conservative / untrusted) | VMs at 1.5 (default) | RAM as sold, 1.0 / 1.5 (upper bound) | MemTotal needed for 1.0 / 1.5 count | VMs by CPU at 1× / 2× / 4×, SMT on | ... SMT off (untrusted) | Monthly cost¹ | + operator² |
|---|---|---|---|---|---|---|---|---|
| Hetzner AX42 | **7** | **11** | 7 / 11 | 60,160 / 62,976 MiB | 4 / 8 / 16 | 2 / 4 / 8 | $119.04 | $269.04 |
| Hetzner AX102 | **15** | **22** | 15 / 23 | 127,744 / 124,928 MiB | 8 / 16 / 32 | 4 / 8 / 16 | $305.70 | $455.70 |
| Hetzner AX162 | **15** | **22** | 15 / 23 | 127,744 / 124,928 MiB | 24 / 48 / 96 | 12 / 24 / 48 | $719.64 | $869.64 |
| OVH US ADVANCE-3 | **7** | **11** | 7 / 11 | 60,160 / 62,976 MiB | 6 / 12 / 24 | 3 / 6 / 12 | $282.25 | $432.25 |
| Latitude m4.metal.medium | **15** | **22** | 15 / 23 | 127,744 / 124,928 MiB | 8 / 16 / 32 | 4 / 8 / 16 | $462.00 | $612.00 |

¹ Monthly cost = price + setup/12 + backup.
² Operator time is assumed at **2 h/month × $75/h = $150/month** for patching, the weekly reboot window, abuse email, restore drills and disk alerts. This is a guess, so plug in your own number. A bad month (hardware swap, incident) is easily 10 hours.

**What changed from the previous version.** It used ⌊(RAM − 1024) × overcommit / 8192⌋ with RAM as sold, which leaves out the 256 MiB per VM that `manager.go` counts and the RAM that never reaches `MemTotal`. The 128 GB boxes drop from 23 to **22** VMs at 1.5; the other counts stay. 23 needs `MemTotal` ≥ 130,560 MiB, and a 128 GiB box cannot report that (131,072 MiB less at least 2 GiB of `struct page`). The 1.0 count of 15 is close to the edge: it needs `MemTotal` ≥ 127,744 MiB, 1 GiB under the estimate, so a board that reserves more than 1.25 GiB of firmware memory (the estimate assumes 256 MiB) admits 14.

**Why not "AX102 fits 13".** An earlier version used ⌊(128 − 6) / 8.75⌋ = 13: a 6 GB host reserve and 8.75 GB per VM (8 GiB guest, 0.25 GiB VMM overhead, 0.5 GiB page-cache headroom, the cgroup's `memory.max`). That answers a different question. 15 is what boatd accepts. 13 is what still leaves the host 6 GB if every VM reaches its `memory.max`. At 15 with every guest full, the VMs hold 15 × 8.75 = 131.25 GiB of `memory.max` against about 125.8 GiB of `MemTotal`, so the page-cache headroom is not all there at once. If you want that cushion, set `maxVMs` or use 13: the AX102 then costs $0.032 per VM-hour at 100%, and breaks even at 89% full.

**Idle memory does not set the VM count.** [`PROTOCOL.md`](../components/boat/PROTOCOL.md#performance-and-density) measured idle RSS at **528 MiB** per cold-booted `default` VM and **264 MiB** from a `warmPool` template, on a GitHub runner (e2e-perf, [run 41609413499](https://github.com/aicodeking/pandaos/actions/runs/41609413499)). Those are idle VMs, and an 8 GiB VM is still promised 8 GiB. boatd admits by that promise, so dividing host RAM by idle RSS gives a number that boatd will not run and that the host could not honor. A template halves idle RSS but leaves the counts above unchanged.

### What happens under load

- At overcommit 1.5, an AX102 promises 22 × 8 = 176 GiB of guest RAM on 128 GiB. That works only while guests leave most of it unused.
- When `MemAvailable` falls below the reserve, boatd inflates balloons, which makes guests give up page cache. A guest's own working memory stays. A compromised guest driver can ignore the balloon.
- If that is not enough, **the host OOM killer takes VMs**. They run at `oom_score_adj` 500 so that the kernel picks a VM and not the host. A killed VM loses whatever it was doing in memory.
- No test here measures how many VMs a host holds under load. PROTOCOL says the same. The default rows are therefore an upper bound on density, and agents that fill their RAM belong on the conservative rows.
- CPU is shared too. 22 VMs are 88 vCPUs on an AX102's 32 threads. All of them can run, but none gets a full vCPU's throughput when they are all busy. No load test here gives a number for that either.

At 1× CPU (no overcommit) the AX42, AX102, ADVANCE-3 and Latitude boxes become CPU-bound at 4, 8, 6 and 8 VMs, and their cost per VM-hour roughly doubles. The AX162 is the only box with CPU to spare, but at its 128 GB base RAM it holds the same VMs as an AX102 for 2.4× the price. It only pays off with a RAM upgrade (512 GB would admit 60 VMs at 1.0 with the same MemTotal estimate; upgrade price not checked).

**Cost per VM-hour** = monthly cost / (VMs × 730 × utilization). The headline table shows the results. boat.dev charges $0.036 for every hour that actually runs, whatever the utilization.

**Break-even.** boat.dev costs max($20, $0.036 × VM-hours) per month. Self-hosting is cheaper once VM-hours exceed monthly cost / $0.036. Percentages are of capacity at 1.0 / at 1.5 (capacity = VMs × 730); above 100% means never.

| Server | Break-even, no operator time | With operator time | Capacity at 1.0 / 1.5 |
|---|---|---|---|
| Hetzner AX42 | 3,307 h (65% / 41%) | 7,473 h (146% / 93%) | 5,110 / 8,030 h |
| Hetzner AX102 | 8,492 h (78% / 53%) | 12,658 h (116% / 79%) | 10,950 / 16,060 h |
| Hetzner AX162 | 19,990 h (183% / 124%) | 24,157 h (221% / 150%) | 10,950 / 16,060 h |
| OVH US ADVANCE-3 | 7,840 h (153% / 98%) | 12,007 h (235% / 150%) | 5,110 / 8,030 h |
| Latitude m4.metal.medium | 12,833 h (117% / 80%) | 17,000 h (155% / 106%) | 10,950 / 16,060 h |

**When self-hosting does win:**
- **Against E2B, Daytona, Modal or Vercel.** At about $0.33/h, an AX102 breaks even at 924 VM-hours/month (8% of capacity at 1.0).
- **When guests really do stay idle.** The default rows then apply: an AX102 at 100% utilization costs $0.019/h, about half of boat.dev's rate. That is for trusted guests only: untrustedTenants forbids the overcommit. boat.dev's rate has no such condition.
- **When you need something boat.dev doesn't sell.** That includes your own kernel or images, data on your own hardware, more than 12 starts per minute on the $20 plan, or no per-start and per-snapshot fees.

## 4b. Untrusted tenants (a public boat.dev alternative)

`services.boat.untrustedTenants` ([`module.nix`](../components/boat/module.nix)) is the mode a service that hosts strangers must run. What it changes for cost:

| | Trusted (defaults) | Untrusted |
|---|---|---|
| `memOvercommit` | 1.5 | **1.0**: the conservative rows. AX102 **15** VMs, not 22 |
| SMT | on | **off** (`nosmt=force`): an AX102 has 16 threads, not 32 |
| VMs by CPU at `cpuOvercommit` 4 | 32 on an AX102 | **16**: RAM (15) still binds first, but 15 VMs are 60 vCPUs on 16 cores, 3.75 per core instead of 1.9 per thread |
| VMs at 1× CPU (no CPU overcommit) | 8 | **4** |
| Disk | `auto` engine (io_uring), no I/O cap | `Sync` engine, io_uring off; busy VMs share the disk by weight (blk-iocost with the disk's measured model and p95 latency targets), `io.max` 4,000 MB/s and 200,000 IOPS per VM as a ceiling. Until 2026-11-10: `io.max` 500 MB/s and 20,000 IOPS (the run below) |
| Network | no cap | 1,000 Mbit/s egress and ingress per VM, 4,096 tracked connections |
| Storage | `storageOvercommit` 10 | **1**: committed storage (51 GiB disk + 8 GiB memory + logs, about 59 GiB per VM, running or stopped, plus snapshots) must fit the disk. About 30 kept VMs on an AX102's 1.92 TB mirror and about 8 on an AX42's 512 GB mirror *(estimate: usable size after RAID 1 and btrfs not measured)* |
| `warmPool`, `ksmTeam` | allowed (off by default) | refused |
| Cost per VM-hour, AX102, 100% | $0.019 (22 VMs) | **$0.028** (15 VMs), 78% full to break even with boat.dev, never with operator time |

So against boat.dev, a public PandaOS service is priced by the conservative rows: **$0.028 per VM-hour on an AX102 at 100% utilization versus boat.dev's $0.036, and more expensive than boat.dev below 78% utilization or once operator time counts.** SMT off does not lower the VM count at `cpuOvercommit` 4, but it halves the CPU behind each busy vCPU. [Measured](#measured-boatbench-on-a-github-runner) below on a runner, not on these servers.

### Untrusted disk: the old caps and what replaced them

Same-runner disk matrix ([42769967285](https://github.com/aicodeking/pandaos/actions/runs/42769967285), `boatd/disk-matrix.sh`, 4 vCPU / 8 GiB VM, `Unsafe` cache; full table in [PROTOCOL.md "Disk: measured"](../components/boat/PROTOCOL.md)):

| | VM, trusted defaults | untrusted, old (`Sync`, `io.max` 500 MB/s / 20k IOPS) | untrusted, now (`Sync`, iocost model + QoS, `io.max` 4,000 / 200k) |
|---|---|---|---|
| seq write, MB/s | 919-1006 | **564** (-41%) | 858 |
| seq read, MB/s | 2539-3497 | 3192 | 2142 |
| rand write, IOPS | 19.0-20.3k | 20.7k | 19.6k |
| rand read, IOPS | 49.6-56.7k | 79.8k | 82.9k |

- **The old caps were per VM and always on**: 41% of sequential write on a runner whose disk does 400-750 MB/s, and on an NVMe server (3-7 GB/s, 500k-1M IOPS) a ceiling 6-14x below the disk on bandwidth and 25-50x on IOPS, busy disk or idle.
- **What replaced them protects only modestly.** A victim VM next to one flooding 1 MiB writes kept 24-26% of its fsync'd 4k writes with iocost + QoS, against 11-17% unprotected, and 40-53% of its sequential write against 46-61% (same run). iocost needs the disk's model: `services.boat.ioCostModel`, or `boat-iocost-model.service` measures it once (`boatd -measure-disk`); without either, boatd refuses to start in untrusted mode and names that command.
- **`Writeback` cannot protect fsync.** With honest flushes, a flooding neighbour stalled the victim's fsync loop to 0 IOPS with or without iocost (runs 42759114633, 42769967285): both VMs' I/O passes through the host's page cache and one writeback queue. Tenants who need durable writes next to hostile neighbours need their own disk; price that in.

## Measured: boatbench on a GitHub runner

Run [42677142859](https://github.com/aicodeking/pandaos/actions/runs/42677142859) (commit `7358286`, 2026-11-09): all workloads a–f, 5 fresh VMs per preset, one GitHub runner per preset. The `boatdev` job printed SKIPPED (no `BOATDEV_API_KEY` secret), so **this run has no boat.dev measurement**. Every boat.dev number below is boat.dev's own published figure. Reports: the run's `boatbench-report` artifact (`presets.md`, `report-<preset>.md`).

| Metric | trusted | trusted-warm ¹ | untrusted ² |
|---|---|---|---|
| Runner CPU, region | EPYC 9V74, IAD | **Xeon Platinum 8370C**, LAX | EPYC 9V74, ORD, **SMT off: 2 CPUs online** |
| (a) Node loop, VM, runs/s | 14.07 [14.02–14.18] | 10.70 [10.50–10.77] | 12.90 [12.71–13.14] |
| (a) Node loop, same runner without a VM, runs/s | 15.70 | 12.64 | 14.86 |
| (a) VM / host | **0.896** | **0.847** | **0.868** |
| (b) create → exec-ready, s | 2.36 | **1.04** | 2.75 |
| (b) fork → exec-ready, s | 1.36 | 1.43 | 2.54 |
| (b) resume → exec-ready, s | 0.499 | 0.514 | 0.984 |
| (b) snapshot until usable, s | 5.28 | 10.13 | 5.75 |
| (c) 10 concurrent creates, s | 18.28 | 8.41 | **failed** ³ |
| (d) seq write 1M, MB/s † | 847.7 | 782.9 | 275.9 |
| (d) rand write 4k, IOPS † | 65,487 | 53,746 | 28,179 |
| (d) PTS fio rand read 4k, IOPS | 69,924 | 62,316 | 62,353 |
| (e) Go tarball download, Mbit/s | 2,450 | 1,482 | 386.3 |
| (f) cold `npm ci`, s | 5.18 | 8.73 | 5.14 |
| Cost per 1000 Node runs at $0.0279/h | $0.000551 **MIXED CPUs** | $0.000724 **MIXED CPUs** | $0.000601 **MIXED CPUs** |

Medians; brackets are IQR. n = 15 passes for (a), 10 creates for the first row of (b), 5 for the rest. Full tables with IQRs and the runner-vs-VM disk ratios are in the reports.

¹ GitHub gave the trusted-warm job an Intel runner, so its CPU-bound rows (a, f, exec round trip) cannot be compared with the other two columns. Its VM / host ratio and its lifecycle rows can: a `warmPool` create is 2.3× faster than a cold one, and its snapshot is slower.
² `stack.sh` turned SMT off as `nosmt=force` would, so the runner had 2 CPUs online, and the untrusted VM's 4 vCPUs and the host-side runs shared those 2. `memOvercommit` stayed at the bench's 8 (see the [bench README](../bench/boatbench/README.md#boatd-presets-options-and-the-warm-pool)). The disk rows show the `Sync` engine and the old `io.max` caps (500 MB/s, 20,000 IOPS; replaced since, see [Untrusted disk](#untrusted-disk-the-old-caps-and-what-replaced-them)): seq write is a third of trusted. Random-write IOPS (28k) are above the 20,000 `ioMaxIOPS` cap, and reads (62–91k) are well above it. My guess, *not checked*, is that the host page cache absorbs or serves that I/O (`Unsafe` cache, below) before the cgroup's `io.max` counts it. The download is well under the 1,000 Mbit/s cap and on a different runner and region, so this run does not show what the cap costs.
³ All 5 repeats: 4 of 10 creates got `503 host_capacity: 24 vCPUs already active (limit 24)`, which is 2 CPUs × the bench's `cpuOvercommit` 12. This is a harness limit (SMT off halves the runner's CPUs), not a boatd bug. `stack.sh` should raise `cpuOvercommit` for the untrusted preset. That is not fixed yet.

**Caveats for every number above:**
- **† Unsafe cache.** All three presets ran with Firecracker's drive `cache_type` `Unsafe`, which is Firecracker's default because boatd sets none. Guest flushes stop at the host page cache, so the write rows are not durable-write numbers. They are also not the same kind of number as boat.dev's published fio results. boatd has since got `diskCache` (`Writeback` honours flushes); `Unsafe` stays the default because `Writeback` costs 5-10x on fsync-heavy writes (PROTOCOL.md "Disk: measured").
- **Three CPUs, none of them the one priced.** boat.dev's published 27.67 runs/s is a VM on an **AMD Ryzen 9 9950X** (Zen 5). The self-hosted price above is a Hetzner AX102, a **Ryzen 9 7950X3D** (Zen 4). The speeds were measured on an EPYC 9V74 or a Xeon 8370C runner. A cost per run that multiplies one CPU's price by another's speed is no machine's cost, and the report marks it **MIXED CPUs**. The only CPU-consistent figure is boat.dev's price at boat.dev's published speed: **$0.000361 per 1000 runs**. PandaOS has none until someone runs boatbench on a 7950X3D.
- **Projection, not measurement.** The report scales the VM / host ratio to a 9950X: 24.8–25.8 runs/s (trusted) and 24.0–25.0 (untrusted), against boat.dev's 27.67. That is arithmetic on public averages for a CPU nobody ran PandaOS on. It is also not the AX102's CPU, and boatbench has no 7950X3D projection.
- **Runner, not server.** These are shared 4-vCPU cloud VMs with nested KVM and a loop-file disk. Each preset ran on its own runner, so runner-to-runner variance is in the column differences too.

## 5. What self-hosting does not give you

- **Multi-region and redundancy.** One box is one failure domain. A disk, PSU or network fault takes every VM down until you fix it. Backups are nightly and crash-consistent, not live replicas.
- **Managed ops and an SLA.** You do the patching, kernel updates, reboots, monitoring and restores. Hetzner and OVH offer only hardware replacement and network SLAs, not uptime for your VMs.
- **Measured density and speed on your server.** Every count here comes from an admission formula, and every benchmark from a shared GitHub runner. Nothing has been measured on these servers or under sustained load, so test your own workload first.
- **Elastic burst.** Capacity is fixed at 15 `default` VMs on an AX102 (22 if you accept the OOM risk above). boat.dev's $20 plan allows 100 concurrent sandboxes. A second server means another month of rent and another setup fee, plus your own scheduling across boxes, which boat does not do.
- **Support.** Nobody answers for your VM layer but you.
- **Abuse handling.** You are the hosting provider for whatever tenants run. Complaints go to you, and the provider can null-route your IP (see `docs/boat.md`, *Abuse policy*).
- **Fast creates without a trade-off.** `warmPool` makes creates faster and halves idle RSS, but VMs from one template share ASLR offsets across tenants. `ksmTeam` saves more memory and is a side channel across whatever it spans. Both are off by default, for those reasons.
- **Hourly exit.** You pay monthly plus a setup fee, and OVH's setup fee equals a full month. boat.dev is per second with a $20 floor.

## Verdict

As of run 42677142859 and the 2026-11-02 prices:

1. **Price: a public PandaOS service is not cheaper than boat.dev.** It must run untrusted mode (no RAM overcommit). On an AX102 that is $0.028 per VM-hour at 100% utilization against boat.dev's $0.036. It is dearer than boat.dev below 78% utilization, and dearer at any utilization once operator time is counted. The default-overcommit figure ($0.019) applies only to your own trusted agents and only while they stay mostly idle.
2. **Speed: no head-to-head measurement exists.** The boat.dev job has never run (no API key), and PandaOS has never run on the CPU it is priced on. What was measured: on the same runner, a PandaOS VM keeps 85–90% of the host's Node throughput in every preset, and untrusted mode costs about 3 points of that. Projected to a 9950X, that is 24–26 runs/s against boat.dev's published 27.67. This is a projection, not a result, and suggests at best parity per vCPU, not a win.
3. **Untrusted mode's measured cost is mostly disk and CPU.** On the runner its old caps cut sequential writes to a third and random writes to under half; in a same-runner matrix they took 41% of a VM's sequential write, and on an NVMe server they would sit 6-14x below the disk's bandwidth and 25-50x below its IOPS. They are replaced by iocost (below), which keeps a single VM at the trusted speed. SMT off halves the threads behind each busy vCPU, and on the runner resume and fork took twice as long. All write numbers are `Unsafe`-cache numbers.
4. **Where self-hosting does win** is unchanged: against E2B, Daytona, Modal and Vercel (6–9× cheaper at 50% utilization), for trusted workloads that tolerate overcommit, and for control (kernels, images, data locality, no start or snapshot fees).

**What would change this verdict:**
- A `BOATDEV_API_KEY` run (on the $100 plan, or with fewer repeats) for a measured boat.dev column.
- `run-on-server.sh` on an AX102 for a CPU-consistent PandaOS cost per run.
- A run after the `diskCache` change, for durable-write numbers.

## Reproduce

All derived numbers come from the inputs in `bench/boatbench/prices.json` and boatd's source:
- monthly cost = `usd_per_month + setup_usd/12 + backup_usd_per_month`
- VMs = `⌊(MemTotal − hostReserveMiB)·memOvercommit / (8192 + vmOverheadMiB)⌋`, capped by `⌊threads·cpuOvercommit/4⌋`, with MemTotal ≈ RAM·63/64 − 256 MiB and the constants read from `components/boat/boatd/config.go` by `bench/boatbench/admission.py`. Untrusted: overcommit 1.0, threads = cores. `bench/boatbench/test_boatbench.py` recomputes every count in `prices.json` and fails if boatd's constants or its admission formula change.
- cost per VM-hour = cost / (VMs · 730 · utilization)
- break-even VM-hours = cost / 0.036

boatbench reads `pandaos.usd_per_hour` (AX102, 15 VMs, 100% utilization, no operator time: $0.0279, also the untrusted figure) and `boatdev.usd_per_hour` ($0.036), and computes cost per run as `usd_per_hour × seconds / 3600`. Each price carries the CPU it buys (`cpu`); the report marks a per-run cost **MIXED CPUs** when that is not the CPU the speed was measured on.
