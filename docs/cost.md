# Cost: self-hosted boat vs boat.dev

All prices were read from public list-price pages on **2026-11-02**. They exclude VAT and are in USD unless marked. EUR prices are converted at **1.12 USD/EUR**, an approximate October 2026 spot rate ([30rates](https://30rates.com/eur-usd-forecast-tomorrow-this-week)) that was not checked for today. Unverified numbers are marked *(unverified)*. The machine-readable copy is [`bench/boatbench/prices.json`](../bench/boatbench/prices.json).

## Headline

One `default` VM (4 vCPU / 8 GiB), cost per VM-hour. Self-hosted figures include amortized setup and backup.
- **Conservative / untrusted** rows use `memOvercommit` 1.0. That is the setting `services.boat.untrustedTenants` forces, so **a public boat.dev alternative, which hosts strangers, runs these rows.**
- **Default** rows use boatd's default of 1.5, which is a bet that guests stay idle (see [Load](#what-happens-under-load)). They are only for one trust domain (your own agents).

Only the AX42 has been benchmarked ([measured on a real dedicated server](#measured-on-a-real-dedicated-server-hetzner-ax42)): its `MemTotal` (63,381 MiB) gives the 7 and 11 VMs below, and 11 VMs ran the Node loop on it at once. Every other row's VM count is boatd's admission rule applied to an *estimated* `MemTotal` ([§4](#4-model)). These are prices per VM-hour, not per unit of work. A packed host's VMs run slower than one VM alone: on the AX42, 52–55% of solo speed at 7 VMs. **Per unit of work, a full AX42 measured 1.44× (trusted) to 1.65× (untrusted) boat.dev's published cost per run** ([cost per run](#cost-per-run-measured-on-the-ax42)).

| | 10% util. | 25% | 50% | 100% | + operator time, 100% | Break-even vs boat.dev |
|---|---|---|---|---|---|---|
| **boat.dev** (pay per second) | $0.036 | $0.036 | $0.036 | $0.036 | n/a | n/a |
| **Hetzner AX102**, conservative / untrusted (15 VMs) | $0.279 | $0.112 | $0.056 | **$0.028** | $0.042 | 8,492 VM-h/mo (78% full); **never** with operator time (116%). **Only if stopped VMs are deleted at once**: see the next two rows |
| **Hetzner AX102**, untrusted, stopped VMs kept 14 days like boat.dev, 24 h sessions (disk-bound: 1,460 VM-h/mo) | $2.094 | $0.838 | $0.419 | **$0.209** | $0.312 | **never** (582% of capacity) |
| **Hetzner AX102**, untrusted, stopped VMs kept 14 days, 8 h sessions (disk-bound: 509 VM-h/mo) | $6.006 | $2.402 | $1.201 | **$0.600** | $0.895 | **never** (1,667%) |
| **Hetzner AX102**, default (22 VMs) | $0.190 | $0.076 | $0.038 | **$0.019** | $0.028 | 53% full; 79% with operator time |
| **OVH US ADVANCE-3**, conservative / untrusted (7 VMs) | $0.552 | $0.221 | $0.110 | $0.055 | $0.085 | never (153%) |
| **OVH US ADVANCE-3**, default (11 VMs) | $0.351 | $0.141 | $0.070 | $0.035 | $0.054 | 98% full; never with operator time (150%) |
| **Latitude.sh m4.metal.medium**, conservative / untrusted (15 VMs) | $0.422 | $0.169 | $0.084 | $0.042 | $0.056 | never (117%) |
| **Latitude.sh m4.metal.medium**, default (22 VMs) | $0.288 | $0.115 | $0.058 | $0.029 | $0.038 | 80% full; never with operator time (106%) |
| Hetzner AX42, conservative / untrusted (7 VMs) | $0.233 | $0.093 | $0.047 | $0.023 | $0.053 | 3,307 VM-h/mo (65% full); never with operator time (146%) |
| Hetzner AX42, default (11 VMs) | $0.148 | $0.059 | $0.030 | $0.015 | $0.034 | 41% full; 93% with operator time |
| E2B / Daytona / Modal / Vercel | | | | | | $0.22–$0.68/h, see below |

**Disk binds before RAM in untrusted mode.** `untrustedTenants` forces `storageOvercommit` 1: every VM, running or stopped, commits its whole disk plus its memory snapshot (51 + 8 GiB ≈ 59 GiB) against the store's size ([`snapshot.go`](../components/boat/boatd/snapshot.go) `hostStorageCheck`). An AX102's 1.92 TB mirror (1,788 GiB before btrfs and the OS) holds about **30 kept VMs** *(estimate: usable size not measured)*, and RAM runs 15. boat.dev keeps a stopped sandbox's disk **14 days at no charge**. A PandaOS host that offers the same must hold each sandbox's slot for its run time plus 336 h, so its running VM-hours per month are at most 30 × 730 × H / (H + 336) for sessions of H hours: 1,460 at 24 h, 509 at 8 h. The $0.028 row assumes stopped VMs are deleted immediately. With boat.dev's retention an AX102 breaks even only if each sandbox runs at least **213 hours** before it stops (460 h with operator time). See [Stopped-VM retention](#stopped-vm-retention-what-boatdevs-free-14-days-cost-on-own-hardware).

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
- `MemTotal` is not the RAM sold. The kernel's `struct page` array (64 bytes per 4 KiB page, 1/64 of RAM) and firmware-reserved ranges are never in it. The model takes **MemTotal ≈ RAM × 63/64 − 256 MiB**, so 128,768 MiB on a 128 GB box. The firmware part is an *(unverified)* estimate. On the one box measured, the AX42, `MemTotal` was **63,381 MiB, 875 MiB below the estimate** (the 8700GE's integrated GPU may reserve memory; not verified). Its counts do not change. The same shortfall on an AX102 would leave 127,893 MiB, only 149 MiB above what 15 VMs at 1.0 need, so check `/proc/meminfo` on yours.
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
- No test here measures how many VMs a host holds under *memory* load. PROTOCOL says the same. The default rows are therefore an upper bound on density, and agents that fill their RAM belong on the conservative rows.
- CPU is shared too. 22 VMs are 88 vCPUs on an AX102's 32 threads. All of them can run, but none gets a full vCPU's throughput when they are all busy. [Measured on the AX42](#density-under-load-ax42): at 1.02 Node loops per thread (11 VMs on 16 threads) a VM keeps 38% of its solo speed, and at 7 VMs 55% (52% with SMT off). The AX102 figures are projections from that curve.

At 1× CPU (no overcommit) the AX42, AX102, ADVANCE-3 and Latitude boxes become CPU-bound at 4, 8, 6 and 8 VMs, and their cost per VM-hour roughly doubles. The AX162 is the only box with CPU to spare, but at its 128 GB base RAM it holds the same VMs as an AX102 for 2.4× the price. It only pays off with a RAM upgrade (512 GB would admit 60 VMs at 1.0 with the same MemTotal estimate; upgrade price not checked).

**Cost per VM-hour** = monthly cost / (VMs × 730 × utilization). The headline table shows the results. boat.dev charges $0.036 for every hour that actually runs, whatever the utilization.

**Break-even.** boat.dev costs max($20, $0.036 × VM-hours) per month. Self-hosting is cheaper once VM-hours exceed monthly cost / $0.036. Percentages are of capacity at 1.0 / at 1.5 (capacity = VMs × 730); above 100% means never.

| Server | Break-even, no operator time | With operator time | Capacity at 1.0 / 1.5 |
|---|---|---|---|
| Hetzner AX42 | 3,307 h (65% / 41%) | 7,473 h (146% / 93%) | 5,110 / 8,030 h |
| Hetzner AX102 | 8,492 h (78% / 53%) | 12,658 h (116% / 79%) | 10,950 / 16,060 h |
| Hetzner AX102, untrusted, 14-day retention, 24 h / 8 h sessions | 8,492 h (582% / 1,667%): never | never | 1,460 / 509 h (disk-bound, not RAM) |
| Hetzner AX162 | 19,990 h (183% / 124%) | 24,157 h (221% / 150%) | 10,950 / 16,060 h |
| OVH US ADVANCE-3 | 7,840 h (153% / 98%) | 12,007 h (235% / 150%) | 5,110 / 8,030 h |
| Latitude m4.metal.medium | 12,833 h (117% / 80%) | 17,000 h (155% / 106%) | 10,950 / 16,060 h |

**When self-hosting does win:**
- **Against E2B, Daytona, Modal or Vercel.** At about $0.33/h, an AX102 breaks even at 924 VM-hours/month (8% of capacity at 1.0).
- **When guests really do stay idle.** The default rows then apply: an AX102 at 100% utilization costs $0.019/h, about half of boat.dev's rate. That is for trusted guests only: untrustedTenants forbids the overcommit. boat.dev's rate has no such condition.
- **When you need something boat.dev doesn't sell.** That includes your own kernel or images, data on your own hardware, more than 12 starts per minute on the $20 plan, or no per-start and per-snapshot fees.

### Stopped-VM retention: what boat.dev's free 14 days cost on own hardware

In untrusted mode a stopped VM keeps its full ~59 GiB commitment (disk + memory snapshot), so it uses one of the host's ~30 storage slots on an AX102 (~8 on an AX42's 512 GB mirror) exactly as a running VM does, while using none of its 15 RAM slots.

- **Marginal cost of keeping one stopped VM** = monthly cost / storage slots: AX102 $305.70 / 30 = **$10.19 per month, $0.014 per hour** (AX42: $119.04 / 8 = $14.88 per month, $0.020 per hour). Keeping it the 14 days boat.dev includes costs **$4.69** per stopped sandbox on an AX102.
- **Capacity.** Running VM-hours per month ≤ min(RAM slots × 730, storage slots × 730 × H / (H + R)) for sessions of H hours kept R hours after stopping:

| AX102, untrusted | R = 0 (delete on stop) | R = 1 day | R = 14 days (boat.dev) |
|---|---|---|---|
| H = 1 h | 10,950 (RAM-bound), $0.028/h | 876, $0.349/h | 65, $4.70/h |
| H = 8 h | 10,950, $0.028/h | 5,475, $0.056/h | 509, $0.600/h |
| H = 24 h | 10,950, $0.028/h | 10,950, $0.028/h | 1,460, $0.209/h |
| H = 168 h | 10,950, $0.028/h | 10,950, $0.028/h | 7,300, $0.042/h |

Cost per running VM-hour at 100% of that capacity, no operator time. **This is a model (a projection)**: it assumes every sandbox runs one session and is then kept the full R hours; a sandbox resumed within R reuses its slot and does better. Ways out, none built into boatd today: a bigger store (more NVMe; price not checked), archiving stopped VMs off the host (a Storage Box costs €3.20/TB-month, but boatd has no archive tier), or `storageOvercommit` above 1, which untrusted mode refuses because a tenant could then fill the disk. Trusted mode (`storageOvercommit` 10) does not bind here.

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
| Storage | `storageOvercommit` 10 | **1**: committed storage (51 GiB disk + 8 GiB memory + logs, about 59 GiB per VM, running or stopped, plus snapshots) must fit the disk. About 30 kept VMs on an AX102's 1.92 TB mirror and about 8 on an AX42's 512 GB mirror *(estimate: usable size after RAID 1 and btrfs not measured)*. **This binds before RAM as soon as stopped VMs are kept** ([retention](#stopped-vm-retention-what-boatdevs-free-14-days-cost-on-own-hardware)) |
| `warmPool`, `ksmTeam` | allowed (off by default) | refused |
| Cost per VM-hour, AX102, 100% | $0.019 (22 VMs) | **$0.028** (15 VMs), 78% full to break even with boat.dev, never with operator time |

So against boat.dev, a public PandaOS service is priced by the conservative rows: **$0.028 per VM-hour on an AX102 at 100% utilization versus boat.dev's $0.036, and more expensive than boat.dev below 78% utilization or once operator time counts.** SMT off does not lower the VM count at `cpuOvercommit` 4, but it halves the CPU behind each busy vCPU. [Measured on an AX42](#measured-on-a-real-dedicated-server-hetzner-ax42) below: SMT off cost 6% of host throughput at 7 VMs. The runner measurements follow it.

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

## Measured on a real dedicated server (Hetzner AX42)

**2026-11-11. These are the first boatbench numbers not from a CI runner.** The repo owner lent a Hetzner AX42: AMD Ryzen 7 PRO 8700GE (Zen 4, 8 cores / 16 threads, a 35 W desktop APU), 64 GB, 2 × 512 GB NVMe in mdadm RAID1, stock Ubuntu 24.04 (kernel 6.8), `/dev/kvm`. boatd and boat-api were built on the box from main at `bcf0cda`, with this commit's `stack.sh` changes (`BENCH_STATE`; the stale-socket fix from the restart onwards). The slim guest image was built on the box with `components/boat/guest/build.sh`, using a temporary static `dockerd` that was removed afterwards. The harness ran on the same box, against boat-api on 127.0.0.1 (nothing was exposed publicly). **boat.dev was not measured** (no API key), so every boat.dev number here is still its own published figure.

What ran, per preset (`stack.sh up` before each and `stack.sh down` after):

```sh
git clone https://github.com/AICodeKing/pandaos.git /opt/boatbench/pandaos            # bcf0cda
OUT=/opt/boatbench/guest-out COMPRESS=0 components/boat/guest/build.sh kernel
OUT=/opt/boatbench/guest-out COMPRESS=0 components/boat/guest/build.sh rootfs slim
export BENCH_WORK=/opt/boatbench/work BENCH_STATE=/opt/boatbench/state
unset BOATBENCH_BOATD_OPTIONS          # the previous preset's env file sets it; stack.sh would merge it in
BOATBENCH_PRESET=$PRESET bench/boatbench/stack.sh up /opt/boatbench/guest-out
set -a; . $BENCH_WORK/env; set +a      # BOAT_URL, BOAT_KEY
B=bench/boatbench/boatbench.py
# trusted and untrusted: 3 host / VM pairs (as CI), then the rest
for i in 1 2 3; do
  BOATBENCH_HOST_DIR=$BENCH_STATE/host-home python3 $B run --driver host --repeats 1 --only ad --out R
  python3 $B run --driver pandaos --repeats 1 --only ad --out R
done
python3 $B run --driver pandaos --repeats 3 --only bcef --out R
BOATBENCH_DENSITY_LEVELS=1,2,4,7,11 python3 $B run --driver pandaos --repeats 2 --only g --out R   # untrusted: 1,2,4,7
# trusted-warm: --repeats 3 --only bc        trusted-writeback: --repeats 3 --only d
```

- **Density levels follow the box.** boatd admits **7** `default` VMs on it at `memOvercommit` 1.0 (conservative / untrusted) and **11** at the default 1.5. Measured `MemTotal` is **63,381 MiB**, 875 MiB under this page's estimate of 64,256, which still gives 7 and 11 (11 needs 62,976). Untrusted stops at 7, its admission count. As on CI, `memOvercommit` stayed at the bench's 8, which only matters for admission (workload c's 10 VMs).
- **Untrusted ran with SMT off** at runtime (`/sys/devices/system/cpu/smt/control`, 8 CPUs online), as `nosmt=force` would give. SMT was switched back on afterwards.
- trusted-warm ran only b and c (the warm pool changes lifecycle, not steady-state speed) and trusted-writeback only d (the cache mode changes disk only). The other presets ran a–g.
- Two of trusted's three host / VM pairs came from a first session. The stack was restarted before pair 3 (same build, same config). A first pass of trusted-warm and trusted-writeback was thrown away because the previous preset's options had leaked into it (see `unset` above).
- `Unsafe` cache (†) on every column except trusted-writeback: the guest's flushes are dropped, as everywhere on this page.

### Per preset, AX42

Medians, n = 9 passes for (a) and 3 for the rest (8 for trusted and untrusted creates, which every repeat does).

| Metric | trusted | trusted-warm | trusted-writeback | untrusted, SMT off |
|---|---|---|---|---|
| (a) Node loop, VM, runs/s | **21.13** [21.09–21.19] | | | **21.21** [21.18–21.30] |
| (a) Node loop, same box without a VM, runs/s | 23.06 | | | 23.02 |
| (a) VM / host | **0.916** | | | **0.921** |
| (b) API round trip, p50 of 25, ms | 0.283 | 0.240 | | 0.286 |
| (b) exec `true` p50 / p95, ms | 2.29 / 3.06 | 2.37 / 2.97 | | 2.21 / 2.65 |
| (b) create → exec-ready, s | 1.40 | **0.318** | 1.73 | 1.39 |
| (b) fork → exec-ready, s | 1.24 | 0.626 | | 0.556 |
| (b) stop, s | 0.617 | 0.604 | | 0.627 |
| (b) resume → exec-ready, s | 0.256 | 0.238 | | 0.211 |
| (b) snapshot until usable, s | 1.79 | 0.662 | | 1.67 |
| (b) new-from-snapshot → exec-ready, s | 0.220 | 0.246 | | 0.227 |
| (c) 10 concurrent creates, s | 2.99 | **1.70** | | 4.25 |
| (d) seq write 1M, MB/s † | 1,863 | | 2,819 | 351.1 |
| (d) seq read 1M, MB/s | 5,903 | | 9,959 | 5,265 |
| (d) rand write 4k, IOPS † | 147,933 | | 143,135 | 84,281 |
| (d) rand read 4k, IOPS | 82,523 [81,494–129,108] | | 174,697 | 200,768 |
| (d) PTS rand read 4k, IOPS | 155,795 | | 179,819 | 202,589 |
| (d) PTS rand write 4k, IOPS † | 171,442 | | 168,718 | 159,704 |
| (d) PTS seq write 1M, MiB/s † | 2,740 | | 4,056 | 2,476 |
| (d) 4k rand write, fsync after each, IOPS † | 23,942 | | **38** | 25,190 |
| (d) seq write 1M, fsync at the end, MB/s † | 2,640 | | 3,517 | 686.9 |
| (e) Go tarball download, Mbit/s | 812.4 | | | 519.0 |
| (f) cold `npm ci`, s | 2.56 | | | 2.51 |

Lifecycle "net of API" values differ from raw by under 1 ms here (the API round trip is 0.24–0.29 ms) and are left out.

### Host vs VM disk, same box (trusted)

| Median of 3 | Host, no VM | VM (`Unsafe`) | VM / host |
|---|---|---|---|
| seq write 1M, MB/s | 4,539 | 1,863 | 0.41 |
| seq read 1M, MB/s | 7,238 | 5,903 | 0.82 |
| rand write 4k, IOPS | 125,043 | 147,933 | 1.18 |
| rand read 4k, IOPS | 252,582 | 82,523 | 0.33 |
| PTS rand read 4k, IOPS | 253,216 | 155,795 | 0.62 |
| PTS seq write 1M, MiB/s | 4,776 | 2,740 | 0.57 |
| 4k rand write, fsync after each, IOPS | **82** | 23,942 (flushes dropped) | n/a |
| 4k rand write, fsync after each, `Writeback` VM, IOPS | 82 | **38** | 0.46 |

### Density under load, AX42

All N VMs ran 2 passes of the Node loop at once, on this one host, 2 repeats (they agreed within 1%). Every VM was set up and measured at every level, with no errors.

| N | trusted, 16 threads: per-VM / host runs/s | efficiency | CPU per loop | untrusted, SMT off, 8 cores: per-VM / host runs/s | efficiency | CPU per loop |
|---|---|---|---|---|---|---|
| 1 | 20.79 / 20.79 | 1.00 | 1.49 | 20.96 / 20.96 | 1.00 | 1.48 |
| 2 | 18.94 / 37.89 | 0.911 | 1.50 | 19.38 / 38.76 | 0.925 | 1.49 |
| 4 | 15.48 / 62.01 | 0.744 | 1.48 | 15.98 / 63.96 | 0.763 | 1.38 |
| 7 | 11.51 / 80.47 | 0.553 | 1.47 | 10.82 / **75.91** | 0.516 | 1.08 |
| 11 | 7.92 / **87.06** | 0.381 | 1.27 | | | |

- **A real server scales far better than the runners did.** Host throughput still rises at 11 VMs on 16 threads (87.1 runs/s, 4.2× one VM), where every 4-vCPU runner had peaked by N = 4. The runners' curves were 2–4 CPU artefacts, and this page no longer uses them for any server projection.
- **SMT off costs little at 7 VMs:** 75.9 against 80.5 runs/s (−6%) on half the threads. The single-VM speed is the same (20.96 vs 20.79).
- **Per-VM speed still falls under load:** a packed VM runs at 52–55% of its solo speed at 7 VMs and 38% at 11. A VM-hour on a full host buys about half the work of one on an idle host.

### Cost per run, measured on the AX42

The first CPU-consistent PandaOS figure: price and speed from the same machine. AX42 = **$119.04 / month** (`prices.json`: €99 + €49 setup / 12 + €3.20 backup, at 1.12 USD/EUR) = **$0.1631 / h**; with 2 h of operator time at $75, $269.04 / month = $0.3686 / h. USD per 1000 runs = $ / h ÷ 3.6 ÷ measured host runs/s, at 100% busy, no retention.

| AX42 mode | VMs at once | Host runs/s (measured) | USD per 1000 runs | + operator time | vs boat.dev's $0.000361 (its published solo speed) |
|---|---|---|---|---|---|
| trusted, 1 VM | 1 | 20.79 | $0.002179 | $0.004924 | 6.0× dearer |
| trusted, 4 VMs | 4 | 62.01 | $0.000730 | $0.001651 | 2.0× |
| trusted, `memOvercommit` 1.0 count | 7 | 80.47 | $0.000563 | $0.001272 | 1.56× |
| **trusted, default 1.5 count** | 11 | **87.06** | **$0.000520** | $0.001176 | **1.44×** (3.3× with operator time) |
| untrusted, SMT off | 4 | 63.96 | $0.000708 | $0.001601 | 1.96× |
| **untrusted, SMT off, full** | 7 | **75.91** | **$0.000597** | $0.001349 | **1.65×** (3.7× with operator time) |

- **Per run, a full AX42 costs 1.4–1.7× boat.dev's published price**, before operator time. To match it, the box would need **125 host runs/s** (284 with operator time). It reached 87.
- Per dollar: boat.dev 2.77 million runs (published solo speed), AX42 1.92 million (trusted, full) or 1.68 million (untrusted).
- **Per VM-hour the AX42 is still cheaper** ($0.023 untrusted at 7 VMs, 100% utilization, against $0.036). The two statements do not conflict. A packed AX42 VM runs at 10.8–11.5 runs/s, and boat.dev's figure is a solo VM at 27.67. boat.dev's speed under load has never been measured, so this comparison flatters boat.dev by an unknown amount, just as "100% busy" flatters the AX42.

### Against boat.dev's published figures (different CPU)

boat.dev's 27.67 runs/s is a 4 vCPU guest on an **AMD Ryzen 9 9950X (Zen 5**, 16 cores, 170 W). The AX42 is a **Ryzen 7 PRO 8700GE (Zen 4**, 8 cores, 35 W). The CPUs differ by a generation and in power budget, so a gap in single-VM speed is mostly hardware.

| | boat.dev PUBLISHED (9950X guest) | PandaOS MEASURED (AX42 guest) |
|---|---|---|
| Node loop, one 4 vCPU VM, runs/s | 27.67 | 21.13 (trusted), 21.21 (untrusted) |
| Same CPU without a VM, runs/s | not published (OpenBenchmarking's 9950X average: 28.8) | 23.06 |
| VM / host | about 0.96 *if* their host scores the public average (inference) | **0.916–0.921** (measured) |
| PTS rand read 4k, IOPS | 433,500 | 155,795–202,589 |
| PTS rand write 4k, IOPS | 412,500 | 159,704–171,442 † |
| PTS seq write 1M, MiB/s | 5,838 | 2,476–2,740 †; 4,056 with `Writeback` |

- **CPU:** the AX42 VM is 24% slower than boat.dev's published VM, and the AX42 host is 20% below the 9950X's public average. Almost all of the gap is the CPU. *Projection, not a measurement:* a PandaOS VM on a 9950X would score about 28.8 × 0.92 ≈ 26.4 runs/s.
- **Disk: boat.dev's published numbers are 2–3× higher.** Part of that is our storage stack (below). Part may be boat.dev's, whose fio target and cache mode are not published.

### What the disk numbers are, and how a real btrfs partition differs

boat's store here is **a sparse btrfs loop file on the box's ext4 root, on md RAID1** (what `stack.sh` does, so the disks were never touched). A real PandaOS install puts btrfs directly on the disks. Compared with that:
- **Every write passes two filesystems and the loop driver.** btrfs writes into a file that ext4 journals, and the page cache can hold the same data twice. That inflates cached reads and buffered writes and costs CPU on direct I/O.
- **An fsync goes btrfs → loop → ext4 fsync of the image → md → a flush on both NVMe drives.** The host managed **82 fsync'd 4k writes per second**, and a `Writeback` VM 38. Those are the honest durable-write rates of *this* stack. They say little about the drives, which are not enterprise drives with power-loss protection (model not recorded), and they do not measure a native btrfs.
- RAID1 doubles every write. A native btrfs `raid1` profile does the same, so that part carries over.
- **Not measured: native btrfs on this hardware.** If the box is reinstalled with PandaOS, rerun workload d there; until then, every AX42 disk row is a loop-file number.

### What is measured now, and what is still a projection

- **Measured on real hardware:** single-VM speed and VM / host overhead (0.92), lifecycle latencies, density under CPU load up to 11 VMs (7 untrusted), host throughput, cost per run on the AX42 at its list price, `MemTotal` and admission counts on a 64 GB box, the cost of SMT off (−6% at 7 VMs).
- **Still projections:** everything about the AX102 and the other servers (no run there; the AX102 is also Zen 4, so the AX42 curve is the best proxy, see [below](#cost-per-run-from-host-throughput)); PandaOS on a 9950X; boat.dev under load and its lifecycle times (no key); density under *memory* load (the Node loop uses little RAM, so 11 × 8 GiB on 64 GB worked); native-btrfs disk; utilization below 100%.

## Measured: boatbench on a GitHub runner

Three runs, 2026-11-10 and 2026-11-11. One GitHub runner per preset, and **GitHub gave each job a different CPU, so columns are not directly comparable on CPU-bound rows.**

- [42818758694](https://github.com/aicodeking/pandaos/actions/runs/42818758694) (commit `8cf09f8`): workloads a–f, 5 fresh VMs per preset. **Its overall conclusion is `cancelled`:** every job timed out later, in a first version of the density step, which is discarded here. The a–f steps had finished and their results were uploaded.
- [42860774575](https://github.com/aicodeking/pandaos/actions/runs/42860774575) (commit `2a9f90b`, conclusion `success`): workload d with the flushing jobs (3 repeats) and workload g, density (2 repeats), all four presets.
- [42937179139](https://github.com/aicodeking/pandaos/actions/runs/42937179139) (main at `bcf0cda`, which includes the boatd-r2 vCPU-cgroup fix, conclusion `success`): untrusted only, workloads a–g (a and d alternating host / VM, 3 repeats; g 2 repeats). It replaces the branch run 42865507357 that this page cited before.

The `boatdev` job printed SKIPPED or was skipped (no `BOATDEV_API_KEY` secret), so **no run has a boat.dev measurement**. Every boat.dev number below is boat.dev's own published figure.

### a–f, run 42818758694

| Metric | trusted | trusted-warm ¹ | trusted-writeback | untrusted ² |
|---|---|---|---|---|
| Runner CPU, region | EPYC 7763, IAD | EPYC 9V74, SEA | **Xeon 6973P-C**, MSP | EPYC 9V74, LAX, **SMT off: 2 CPUs online** |
| Firecracker cache | Unsafe | Unsafe | **Writeback** | Unsafe |
| (a) Node loop, VM, runs/s | 10.49 [10.41–10.54] | 13.49 [13.28–13.74] | 16.45 [16.30–16.63] | 13.41 [13.08–13.81] |
| (a) Node loop, same runner without a VM, runs/s | 11.88 | 15.49 | 19.98 | 15.11 |
| (a) VM / host | **0.883** | **0.871** | **0.823** | **0.887** |
| (b) API round trip, authenticated GET, p50 of 25, ms | 0.759 | 0.494 | 0.616 | 0.576 |
| (b) exec `true` p50, raw / net of API, ms | 6.52 / 5.74 | 5.14 / 4.64 | 5.19 / 4.54 | 6.52 / 6.00 |
| (b) create → exec-ready, raw / net, s | 3.06 / 3.05 | 0.860 / 0.860 | 2.22 / 2.22 | 2.75 / 2.75 |
| (b) fork → exec-ready, raw / net, s | 1.70 / 1.70 | 1.40 / 1.40 | 1.20 / 1.20 | 1.25 / 1.24 |
| (b) resume → exec-ready, raw / net, s | 0.588 / 0.587 | 0.548 / 0.547 | 0.475 / 0.474 | 0.507 / 0.506 |
| (b) stop, raw / net, s | 0.780 / 0.779 | 0.597 / 0.597 | 0.398 / 0.398 | 0.764 / 0.763 |
| (b) snapshot until usable, raw / net, s | 4.57 / 4.57 | 16.19 / 16.19 | 2.56 / 2.56 | 4.97 / 4.97 |
| (c) 10 concurrent creates, raw / net, s | 25.52 / 25.52 | 6.63 / 6.63 | 21.77 / 21.77 | **26.69 / 26.69** ³ |
| (d) seq write 1M, MB/s † | 610.8 | 757.7 | 652.7 | 304.1 |
| (d) rand write 4k, IOPS † | 43,178 | 63,412 | 44,322 | 41,578 |
| (d) PTS fio rand read 4k, IOPS | 50,176 | 68,166 | 75,114 | 116,897 |
| (e) Go tarball download, Mbit/s | 1,453 | 1,351 | 402.2 | 1,037 |
| (f) cold `npm ci`, s | 7.43 | 5.92 | 6.33 | 4.91 |

Medians; brackets are IQR. n = 15 passes for (a), 5 for the rest. Lifecycle rows use the 5 repeats that ran (b).

¹ A `warmPool` create is about 3× faster than a cold one; its snapshot and new-from-snapshot are slower.
² `stack.sh` turned SMT off as `nosmt=force` would, so the runner had 2 CPUs online, and the VM's 4 vCPUs and the host-side runs shared those 2. `memOvercommit` stayed at the bench's 8 (see the [bench README](../bench/boatbench/README.md#boatd-presets-options-and-the-warm-pool)).
³ Fixed: earlier runs failed here with `503 host_capacity` at `cpuOvercommit` 12. `stack.sh` now derives it from the CPUs online (24 on 2 CPUs).

**API baseline.** `net` = raw − k × that side's API round trip, with k the fewest sequential API calls in the timing (`drivers.py` `ROUND_TRIPS`). On localhost the baseline is under 1 ms, so net and raw agree to within about 2 ms on every lifecycle row. It matters only for boat.dev, which is reached over the WAN, and **boat.dev's side has not been measured**, so there is still no equal-footing lifecycle comparison. The harness will produce one when a key exists.

### Density under load, run 42860774575

N VMs ran the Node loop at the same time, on one runner, from one barrier. Fresh VMs, all N measured, no errors in any level. Two repeats per preset agreed within 5%; the table shows their median.

| N | trusted, EPYC 9V74, 4 CPUs: per-VM / host runs/s | efficiency | trusted-warm, Xeon 8573C, 4 CPUs | efficiency | trusted-writeback, EPYC 7763, 4 CPUs | efficiency | untrusted, Xeon 8370C, **2 CPUs** | efficiency |
|---|---|---|---|---|---|---|---|---|
| 1 | 13.62 / 13.62 | 1.00 | 9.42 / 9.42 | 1.00 | 10.89 / 10.89 | 1.00 | 6.91 / 6.91 | 1.00 |
| 2 | 10.31 / 20.62 | 0.757 | 4.72 / 9.44 ⁴ | 0.501 | 8.44 / 16.88 | 0.775 | 6.85 / 13.70 | 0.992 |
| 4 | 5.55 / **22.24** | 0.408 | 5.26 / **21.00** | 0.558 | 4.37 / **17.46** | 0.401 | 2.62 / 10.47 | 0.379 |
| 6 | 3.49 / 20.94 | 0.256 | 2.40 / 14.39 | 0.255 | 2.82 / 16.97 | 0.259 | 1.42 / 8.49 | 0.205 |

Efficiency = per-VM runs/s at N / at N = 1. One loop uses 1.49–1.53 CPUs when it has them (1.23 on 2 CPUs): V8's GC and compiler threads run beside the main thread.

⁴ Reproduced in both repeats and not explained.

What was measured:
- **Host throughput plateaus early and then falls.** On a 4-thread runner (2 cores with SMT) the peak is 1.6–2.2× one VM, at N = 4, and at N = 6 it is no higher. On 2 cores with SMT off the peak is at N = 2, and N = 6 gives 38% less than that peak.
- **Per-VM speed collapses under oversubscription:** at N = 6 every VM runs at 21–26% of its solo speed.
- The workload is one Node process per VM, mostly one thread. A workload that keeps all 4 vCPUs busy would contend sooner.

### Cost per run from host throughput

The fair figure is the host's price over the host's throughput: **USD per 1000 runs = host $/h ÷ 3.6 ÷ host runs/s**, at 100% busy, no operator time, no retention. **The AX42 rows are measured** ([above](#cost-per-run-measured-on-the-ax42)): $0.000520 trusted and $0.000597 untrusted, against boat.dev's published $0.000361.

**The AX102 rows below are projections.** Nobody has run PandaOS on an AX102. They now use the AX42's measured curve instead of the runners', because the AX42 is a dedicated Zen 4 box like the AX102 (Ryzen 9 7950X3D), while the runners' 2–4 CPU curves turned out not to carry over to a real server.

| AX102 mode ($0.4188/h) | VMs on CPUs | Load, loops per CPU | AX42 point used | Host runs/s, projected = CPUs × AX42 runs/s per CPU | USD per 1000 runs (PROJECTION) | vs boat.dev's $0.000361 |
|---|---|---|---|---|---|---|
| trusted, default overcommit | 22 on 32 threads | 1.02 | N = 11, same load: 5.44 runs/s per thread | 174 | $0.000667 | 1.85× dearer |
| untrusted | 15 on 16 cores | 1.39 | N = 7, load 1.30 (the highest measured): 9.49 runs/s per core | 152 | $0.000766 | 2.1× dearer |

The 7950X3D clocks higher than the 35 W 8700GE and has more L3, so these projections are likely low. The speed-independent requirement stands: **to match boat.dev's published cost per run, an AX102 must sustain 322 host runs/s, about 20 per core** (480 and 30 with operator time). The AX42 measured **10.9 runs/s per core** with SMT (87.06 on 8 cores) and **9.5** without, so an AX102 would need roughly twice the AX42's per-core throughput.

boat.dev's figure is its vendor-measured solo speed. How boat.dev behaves under load is unmeasured, so every comparison here is generous to boat.dev by an unknown amount, just as "100% busy" is generous to the self-hosted box.

### Writeback vs Unsafe, run 42860774575

The older fio jobs never flush, so they cannot tell the two caches apart. Two flushing jobs now can:

| Median of 3 | Runner's own disk, no VM | VM, `Unsafe` (trusted, EPYC 9V74) | Runner's own disk, no VM | VM, `Writeback` (EPYC 7763) |
|---|---|---|---|---|
| 4k rand write, fsync after each, IOPS | 1,157 | **10,929** | 1,534 | **439** |
| 1M seq write, fsync at the end, MB/s | 3,437 | 1,094 | 2,497 | **430** |

- **`Unsafe` drops flushes, and this shows it**: the VM's fsync'd writes are 9.4× faster than the disk underneath can fsync. Only `Writeback` numbers are durable-write numbers.
- **Durable 4k writes cost about 25×** (439 vs 10,929 IOPS) and durable sequential writes about 2.5×. The two columns are on different runners, but their host-disk fsync rates are close.
- `Unsafe` is still boatd's default, in untrusted mode too (fsync'd 4k in the untrusted VM: 7,412 IOPS against 2,019 on its host disk). A guest's `fsync` there does not survive a host crash.
- The non-flushing write rows (seq write 1M, rand write 4k) show no consistent Writeback penalty across the two runs: differences are within runner-to-runner variance.

### Untrusted after the vCPU-cgroup fix, run 42937179139

Run [42937179139](https://github.com/aicodeking/pandaos/actions/runs/42937179139), main at `bcf0cda` (the boatd-r2 merge: vCPU threads get their own cgroup, apart from the VMM thread). Untrusted preset, **EPYC 9V45 runner, SMT off: 2 CPUs online**, `Unsafe` cache. Medians, IQR in brackets.

| Metric | Runner, no VM | Untrusted VM | VM / host | Old untrusted (run 42818758694, EPYC 9V74, before the fix) |
|---|---|---|---|---|
| (a) Node loop, runs/s (n = 9) | 18.26 | 16.54 [16.27–16.76] | **0.906** | 13.41 (VM / host 0.887) |
| (d) seq write 1M, MB/s † | 8,514 | 297.1 [284.1–317.2] | 0.035 | 304.1 |
| (d) PTS seq write 1M, MiB/s † | 4,884 | 757.2 | 0.155 | n/a |
| (d) rand write 4k, IOPS † | 71,382 | 52,798 | 0.740 | 41,578 |
| (d) PTS rand read 4k, IOPS | 180,802 | 370,331 | 2.05 | 116,897 |
| (d) 4k rand write, fsync each, IOPS † | 1,446 | 13,661 | (Unsafe drops flushes) | n/a |
| (b) create → exec-ready, s | | 2.34 | | 2.75 |
| (b) fork / resume / new-from-snapshot → exec-ready, s | | 1.24 / 0.581 / 0.811 | | 1.25 / 0.507 / n/a |
| (c) 10 concurrent creates, s | | 22.02 | | 26.69 |
| (e) Go tarball download, Mbit/s | | 1,167 (above the 1,000 Mbit/s ingress cap; not investigated) | | 1,037 |
| (f) cold `npm ci`, s | | 3.82 | | 4.91 |

Density (g) on those 2 CPUs: per-VM / host runs/s 16.17 / 16.17 at N = 1, 10.96 / 21.92 at N = 2 (efficiency 0.678), 4.62 / 18.52 at N = 4 (0.286), 3.06 / 18.33 at N = 6 (0.189).

What this run settles and what it does not:
- **The 1M sequential write did not recover on main.** The branch run's 626.6 MB/s (n = 2, EPYC 9V74) is not reproduced: 297.1 MB/s here (n = 3, EPYC 9V45), the same as before the fix. The PTS job (iodepth 64) reaches 757 MiB/s. Different runner CPUs, so this does not prove the fix does nothing; it does mean the branch number should not be quoted.
- **The Node loop keeps 0.906 of the host** in untrusted mode, in line with the trusted presets (0.82–0.88).
- **The untrusted density curve no longer looks flat to N = 2.** The earlier untrusted curve (Xeon 8370C, run 42860774575) gave efficiency 0.992 at N = 2; this one gives 0.678. Runner curves on 2 CPUs do not settle untrusted efficiency. The [measured AX42 curve](#density-under-load-ax42) replaces both for a real server, and the AX102 projection now uses it.

**Caveats for every number above:**
- **† Unsafe cache** on every column except trusted-writeback, as above. The write rows are also not the same kind of number as boat.dev's published fio results.
- **None of these CPUs is the one priced.** boat.dev's published 27.67 runs/s is a VM on an **AMD Ryzen 9 9950X** (Zen 5). The self-hosted price is a Hetzner AX102, a **Ryzen 9 7950X3D** (Zen 4). The speeds come from EPYC and Xeon runners. A cost per run that multiplies one CPU's price by another's speed is no machine's cost, and is marked **MIXED CPUs**. The CPU-consistent figures are boat.dev's price at boat.dev's published speed (**$0.000361 per 1000 runs**) and the [AX42's measured cost](#cost-per-run-measured-on-the-ax42) ($0.000520–$0.000597).
- **Runner, not server.** These are shared 4-vCPU cloud VMs with nested KVM and a loop-file disk. Each preset ran on its own runner, so runner-to-runner variance is in the column differences too.

## 5. What self-hosting does not give you

- **Multi-region and redundancy.** One box is one failure domain. A disk, PSU or network fault takes every VM down until you fix it. Backups are nightly and crash-consistent, not live replicas.
- **Managed ops and an SLA.** You do the patching, kernel updates, reboots, monitoring and restores. Hetzner and OVH offer only hardware replacement and network SLAs, not uptime for your VMs.
- **Measured density and speed on your server.** One dedicated box has been measured (the AX42, one Node workload, CPU load only, a loop-file store). The other servers' counts come from an admission formula, and nothing measured memory pressure, so test your own workload first.
- **Elastic burst.** Capacity is fixed at 15 `default` VMs on an AX102 (22 if you accept the OOM risk above). boat.dev's $20 plan allows 100 concurrent sandboxes. A second server means another month of rent and another setup fee, plus your own scheduling across boxes, which boat does not do.
- **Support.** Nobody answers for your VM layer but you.
- **Abuse handling.** You are the hosting provider for whatever tenants run. Complaints go to you, and the provider can null-route your IP (see `docs/boat.md`, *Abuse policy*).
- **Fast creates without a trade-off.** `warmPool` makes creates faster and halves idle RSS, but VMs from one template share ASLR offsets across tenants. `ksmTeam` saves more memory and is a side channel across whatever it spans. Both are off by default, for those reasons.
- **Hourly exit.** You pay monthly plus a setup fee, and OVH's setup fee equals a full month. boat.dev is per second with a $20 floor.

## Verdict

As of the AX42 run (2026-11-11), CI runs 42818758694, 42860774575 and 42937179139, and the 2026-11-02 prices. Items marked *projection* are arithmetic, not runs.

**Where PandaOS wins**
1. **Lifecycle latency** (measured on the AX42; boat.dev's side unmeasured): create → exec-ready 1.40 s cold and **0.32 s** from the warm pool, resume 0.21–0.26 s, new-from-snapshot 0.22 s, 10 concurrent creates in 3.0 s (1.7 s warm, 4.3 s untrusted), exec round trip 2.2–2.4 ms.
2. **Price per VM-hour at high utilization:** an AX42 at 7 VMs costs $0.023 per VM-hour at 100%, against boat.dev's $0.036. The break-even is 65% full with no operator time, and never with it (146%).
3. **Against E2B, Daytona, Modal and Vercel** (6–9× cheaper at 50% utilization, retention aside), for trusted workloads that tolerate overcommit, and for control: kernels, images, data locality, no start or snapshot fees.

**Where it ties**
4. **Virtualization overhead:** a PandaOS VM keeps **0.92 of the host's Node throughput** on the AX42 (0.82–0.91 on runners). boat.dev's published VM sits at about 0.96 of the public 9950X average, which is an inference, since its host was not measured. Within the noise of different hardware, they are on par.
5. **SMT off costs untrusted mode little CPU:** −6% host throughput at the AX42's 7 VMs.

**Where it loses**
6. **Cost per unit of work.** Measured on the AX42 at its list price, a full box costs **$0.000520 per 1000 Node runs (trusted, 11 VMs) and $0.000597 (untrusted, 7 VMs): 1.44× and 1.65× boat.dev's published $0.000361**, and 3.3–3.7× with operator time. A packed VM runs at about half its solo speed (52–55% at 7 VMs, 38% at 11). An AX102 (*projection* from the AX42 curve) lands at 1.85–2.1×. boat.dev's figure is a solo VM, and its behaviour under load is unknown.
7. **Single-VM speed on the hardware tested:** 21.1 runs/s on the AX42 (Zen 4, 35 W) against boat.dev's published 27.67 on a 9950X (Zen 5). That gap is mostly CPU. *Projection:* PandaOS on a 9950X would score about 26.4.
8. **Disk, as measured:** the AX42's loop-file store reached 156–203k random-read and 160–171k random-write IOPS (PTS job), against boat.dev's published 433k / 413k. Native btrfs has not been measured. Durable writes are the weak point: the host fsyncs 82 4k writes per second through this stack, and a `Writeback` VM 38. Every `Unsafe` (default) write number drops flushes.
9. **Retention:** in untrusted mode, keeping stopped VMs for boat.dev's free 14 days limits an AX102 to 509–1,460 running VM-hours a month (about $0.21–$0.60 per VM-hour); an AX42 (~8 storage slots) fares worse.

**What would change this verdict:**
- A `BOATDEV_API_KEY` run, for a measured boat.dev column: API baseline, lifecycle, disk and a load curve. Every boat.dev number here is still published, not measured.
- workload d on native btrfs (if the AX42 is reinstalled with PandaOS), and on drives with power-loss protection for durable writes.
- An AX102 (or 9950X) run with `BOATBENCH_DENSITY_LEVELS=1,4,8,15,22`, to replace the AX102 projection.
- An archive tier for stopped VMs, or a bigger store, which would lift the retention limit.

## Reproduce

All derived numbers come from the inputs in `bench/boatbench/prices.json` and boatd's source:
- monthly cost = `usd_per_month + setup_usd/12 + backup_usd_per_month`
- VMs = `⌊(MemTotal − hostReserveMiB)·memOvercommit / (8192 + vmOverheadMiB)⌋`, capped by `⌊threads·cpuOvercommit/4⌋`, with MemTotal ≈ RAM·63/64 − 256 MiB and the constants read from `components/boat/boatd/config.go` by `bench/boatbench/admission.py`. Untrusted: overcommit 1.0, threads = cores. `bench/boatbench/test_boatbench.py` recomputes every count in `prices.json` and fails if boatd's constants or its admission formula change.
- cost per VM-hour = cost / (VMs · 730 · utilization)
- break-even VM-hours = cost / 0.036
- untrusted storage slots = ⌊store GiB / 59⌋ (1,788 GiB on an AX102: 30); running VM-hours ≤ min(VMs·730, slots·730·H/(H+R)); a kept stopped VM costs cost / slots per month

boatbench reads `pandaos.usd_per_hour` (AX102, 15 VMs, 100% utilization, no operator time: $0.0279, also the untrusted figure) and `boatdev.usd_per_hour` ($0.036), and computes cost per run as `usd_per_hour × seconds / 3600`. Its density section uses `pandaos.density_model` ($0.4188/h per host): USD per 1000 runs = host price ÷ 3.6 ÷ (VMs × solo runs/s × efficiency), the efficiency interpolated as host throughput per CPU at load = VMs × CPU per loop ÷ CPUs. Each price carries the CPU it buys (`cpu`); the report marks a per-run cost **MIXED CPUs** when that is not the CPU the speed was measured on.
