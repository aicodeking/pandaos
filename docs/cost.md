# Cost: self-hosted boat vs boat.dev

All prices were read from public list-price pages on **2026-11-02**. They exclude VAT and are in USD unless marked. EUR prices are converted at **1.12 USD/EUR**, an approximate October 2026 spot rate ([30rates](https://30rates.com/eur-usd-forecast-tomorrow-this-week)) that was not checked for today. Unverified numbers are marked *(unverified)*. The machine-readable copy is [`bench/boatbench/prices.json`](../bench/boatbench/prices.json).

## Headline

One `default` VM (4 vCPU / 8 GiB), cost per VM-hour. Self-hosted figures include amortized setup and backup. **Conservative** rows use `memOvercommit` 1.0. **Default** rows use boatd's default of 1.5, which is a bet that guests stay idle (see [Load](#what-happens-under-load)). No server in this table has been benchmarked: these are prices divided by what boatd's admission allows, not measurements on that hardware.

| | 10% util. | 25% | 50% | 100% | + operator time, 100% | Break-even vs boat.dev |
|---|---|---|---|---|---|---|
| **boat.dev** (pay per second) | $0.036 | $0.036 | $0.036 | $0.036 | n/a | n/a |
| **Hetzner AX102**, conservative (15 VMs) | $0.279 | $0.112 | $0.056 | **$0.028** | $0.042 | 8,492 VM-h/mo (78% full); **never** with operator time (116%) |
| **Hetzner AX102**, default (23 VMs) | $0.182 | $0.073 | $0.036 | **$0.018** | $0.027 | 51% full; 75% with operator time |
| **OVH US ADVANCE-3**, conservative (7 VMs) | $0.552 | $0.221 | $0.110 | $0.055 | $0.085 | never (153%) |
| **OVH US ADVANCE-3**, default (11 VMs) | $0.351 | $0.141 | $0.070 | $0.035 | $0.054 | 98% full; never with operator time (150%) |
| **Latitude.sh m4.metal.medium**, conservative (15 VMs) | $0.422 | $0.169 | $0.084 | $0.042 | $0.056 | never (117%) |
| **Latitude.sh m4.metal.medium**, default (23 VMs) | $0.275 | $0.110 | $0.055 | $0.028 | $0.036 | 76% full; never with operator time (101%) |
| Hetzner AX42, conservative (7 VMs) | $0.233 | $0.093 | $0.047 | $0.023 | $0.053 | 3,307 VM-h/mo (65% full); never with operator time (146%) |
| Hetzner AX42, default (11 VMs) | $0.148 | $0.059 | $0.030 | $0.015 | $0.034 | 41% full; 93% with operator time |
| E2B / Daytona / Modal / Vercel | | | | | | $0.22–$0.68/h, see below |

**What the table shows.** At $0.036/h for 4 vCPU / 8 GB, boat.dev costs about as much as running the hardware yourself. With guest RAM not overcommitted, self-hosting comes out cheaper only on Hetzner, only when the box is more than 65–78% full around the clock, and only if your own time is free. At the default overcommit, the AX102 and AX42 beat boat.dev even with operator time, at 75% and 93% full, but those VM counts hold only while guests stay mostly idle. Compared with the other sandbox providers (about $0.33–$0.48 per hour), an AX102 is about 6–9× cheaper at 50% utilization with no overcommit. You self-host for control, data locality, no per-start or plan limits, and custom images and kernels, not to undercut boat.dev.

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

**VM count.** The counts follow boatd's own admission check ([`manager.go`](../components/boat/boatd/manager.go), described in [`PROTOCOL.md`](../components/boat/PROTOCOL.md)):
- RAM: active VM RAM may not exceed (MemTotal − `hostReserveMiB` 1024) × `memOvercommit`. A `default` VM counts as 8192 MiB, so VMs = ⌊(RAM MiB − 1024) × overcommit / 8192⌋. RAM is taken as sold. A real `MemTotal` is lower and was not measured on these servers.
- CPU: active vCPUs may not exceed host threads × `cpuOvercommit` (default 4.0).
- Above overcommit 1.0, a new VM also needs `MemAvailable` ≥ 1024 + 8192 / overcommit MiB (6,485 MiB at 1.5). boatd refuses a create (503) if the host is short of real memory, however few VMs are running.

| Server | VMs at 1.0 (conservative) | VMs at 1.5 (default) | VMs by CPU at 1× / 2× / 4× | Monthly cost¹ | + operator² |
|---|---|---|---|---|---|
| Hetzner AX42 | **7** | **11** | 4 / 8 / 16 | $119.04 | $269.04 |
| Hetzner AX102 | **15** | **23** | 8 / 16 / 32 | $305.70 | $455.70 |
| Hetzner AX162 | **15** | **23** | 24 / 48 / 96 | $719.64 | $869.64 |
| OVH US ADVANCE-3 | **7** | **11** | 6 / 12 / 24 | $282.25 | $432.25 |
| Latitude m4.metal.medium | **15** | **23** | 8 / 16 / 32 | $462.00 | $612.00 |

¹ Monthly cost = price + setup/12 + backup.
² Operator time is assumed at **2 h/month × $75/h = $150/month** for patching, the weekly reboot window, abuse email, restore drills and disk alerts. This is a guess, so plug in your own number. A bad month (hardware swap, incident) is easily 10 hours.

**Why this no longer says "AX102 fits 13".** Earlier versions of this page used ⌊(128 − 6) / 8.75⌋ = 13: a 6 GB host reserve, and 8.75 GB per VM (8 GiB guest, 0.25 GiB VMM overhead, 0.5 GiB page-cache headroom). boatd's admission is looser: it reserves 1 GiB and counts only guest RAM, so it admits ⌊(131,072 − 1024) / 8192⌋ = 15. The two answer different questions. 15 is what boatd accepts. 13 is what still leaves the host 6 GB if every VM reaches its cgroup `memory.max` (RAM + 256 MiB + 512 MiB). At 15 with every guest full, the VMs hold 123.75 GiB before page cache (8.25 GiB each), which leaves about 3 GiB of the 127 GiB boatd counts. If you want that cushion, set `maxVMs` or use 13: AX102 then costs $0.032 per VM-hour at 100%, and breaks even at 89% full. The 15 and 23 counts need `MemTotal` ≥ 123,904 and 126,635 MiB on a 128 GB box.

**Idle memory does not set the VM count.** [`PROTOCOL.md`](../components/boat/PROTOCOL.md#performance-and-density) measured idle RSS at **528 MiB** per cold-booted `default` VM and **264 MiB** from a `warmPool` template, on a GitHub runner (e2e-perf, [run 41609413499](https://github.com/aicodeking/pandaos/actions/runs/41609413499)). Those are idle VMs, and an 8 GiB VM is still promised 8 GiB. boatd admits by that promise, so dividing host RAM by idle RSS gives a number that boatd will not run and that the host could not honor. A template halves idle RSS but leaves the counts above unchanged.

### What happens under load

- At overcommit 1.5, an AX102 promises 23 × 8 = 184 GiB of guest RAM on 128 GiB. That works only while guests leave most of it unused.
- When `MemAvailable` falls below the reserve, boatd inflates balloons, which makes guests give up page cache. A guest's own working memory stays. A compromised guest driver can ignore the balloon.
- If that is not enough, **the host OOM killer takes VMs**. They run at `oom_score_adj` 500 so that the kernel picks a VM and not the host. A killed VM loses whatever it was doing in memory.
- No test here measures how many VMs a host holds under load. PROTOCOL says the same. The default rows are therefore an upper bound on density, and agents that fill their RAM belong on the conservative rows.
- CPU is shared too. 23 VMs are 92 vCPUs on an AX102's 32 threads. All of them can run, but none gets a full vCPU's throughput when they are all busy. No load test here gives a number for that either.

At 1× CPU (no overcommit) the AX42, AX102, ADVANCE-3 and Latitude boxes become CPU-bound at 4, 8, 6 and 8 VMs, and their cost per VM-hour roughly doubles. The AX162 is the only box with CPU to spare, but at its 128 GB base RAM it holds the same VMs as an AX102 for 2.4× the price. It only pays off with a RAM upgrade (512 GB would admit 63 VMs at 1.0; upgrade price not checked).

**Cost per VM-hour** = monthly cost / (VMs × 730 × utilization). The headline table shows the results. boat.dev charges $0.036 for every hour that actually runs, whatever the utilization.

**Break-even.** boat.dev costs max($20, $0.036 × VM-hours) per month. Self-hosting is cheaper once VM-hours exceed monthly cost / $0.036. Percentages are of capacity at 1.0 / at 1.5 (capacity = VMs × 730); above 100% means never.

| Server | Break-even, no operator time | With operator time | Capacity at 1.0 / 1.5 |
|---|---|---|---|
| Hetzner AX42 | 3,307 h (65% / 41%) | 7,473 h (146% / 93%) | 5,110 / 8,030 h |
| Hetzner AX102 | 8,492 h (78% / 51%) | 12,658 h (116% / 75%) | 10,950 / 16,790 h |
| Hetzner AX162 | 19,990 h (183% / 119%) | 24,157 h (221% / 144%) | 10,950 / 16,790 h |
| OVH US ADVANCE-3 | 7,840 h (153% / 98%) | 12,007 h (235% / 150%) | 5,110 / 8,030 h |
| Latitude m4.metal.medium | 12,833 h (117% / 76%) | 17,000 h (155% / 101%) | 10,950 / 16,790 h |

**When self-hosting does win:**
- **Against E2B, Daytona, Modal or Vercel.** At about $0.33/h, an AX102 breaks even at 924 VM-hours/month (8% of capacity at 1.0).
- **When guests really do stay idle.** The default rows then apply: an AX102 at 100% utilization costs $0.018/h, half of boat.dev's rate. boat.dev's rate has no such condition.
- **When you need something boat.dev doesn't sell.** That includes your own kernel or images, data on your own hardware, more than 12 starts per minute on the $20 plan, or no per-start and per-snapshot fees.

## 5. What self-hosting does not give you

- **Multi-region and redundancy.** One box is one failure domain. A disk, PSU or network fault takes every VM down until you fix it. Backups are nightly and crash-consistent, not live replicas.
- **Managed ops and an SLA.** You do the patching, kernel updates, reboots, monitoring and restores. Hetzner and OVH offer only hardware replacement and network SLAs, not uptime for your VMs.
- **Measured density and speed on your server.** Every count here comes from an admission formula, and every benchmark from a shared GitHub runner. Nothing has been measured on these servers or under sustained load, so test your own workload first.
- **Elastic burst.** Capacity is fixed at 15 `default` VMs on an AX102 (23 if you accept the OOM risk above). boat.dev's $20 plan allows 100 concurrent sandboxes. A second server means another month of rent and another setup fee, plus your own scheduling across boxes, which boat does not do.
- **Support.** Nobody answers for your VM layer but you.
- **Abuse handling.** You are the hosting provider for whatever tenants run. Complaints go to you, and the provider can null-route your IP (see `docs/boat.md`, *Abuse policy*).
- **Fast creates without a trade-off.** `warmPool` makes creates faster and halves idle RSS, but VMs from one template share ASLR offsets across tenants. `ksmTeam` saves more memory and is a side channel across whatever it spans. Both are off by default, for those reasons.
- **Hourly exit.** You pay monthly plus a setup fee, and OVH's setup fee equals a full month. boat.dev is per second with a $20 floor.

## Reproduce

All derived numbers come from the inputs in `bench/boatbench/prices.json`:
- monthly cost = `usd_per_month + setup_usd/12 + backup_usd_per_month`
- VMs = `⌊(ram_gb·1024 − 1024)·memOvercommit / 8192⌋`, capped by `⌊vcpu_threads·4/4⌋` (`bench/boatbench/test_boatbench.py` checks these against `prices.json`)
- cost per VM-hour = cost / (VMs · 730 · utilization)
- break-even VM-hours = cost / 0.036

boatbench reads `pandaos.usd_per_hour` (AX102, 15 VMs, 100% utilization, no operator time: $0.0279) and `boatdev.usd_per_hour` ($0.036), and computes cost per run as `usd_per_hour × seconds / 3600`.
