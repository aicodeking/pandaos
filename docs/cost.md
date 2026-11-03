# Cost: self-hosted boat vs boat.dev

All prices were read from public list-price pages on **2026-11-02**. They exclude VAT and are in USD unless marked. EUR prices are converted at **1.12 USD/EUR**, an approximate October 2026 spot rate ([30rates](https://30rates.com/eur-usd-forecast-tomorrow-this-week)) that was not checked for today. Unverified numbers are marked *(unverified)*. The machine-readable copy is [`bench/boatbench/prices.json`](../bench/boatbench/prices.json).

## Headline

One `default` VM (4 vCPU / 8 GB), cost per VM-hour. Self-hosted figures include amortized setup and backup.

| | 10% util. | 25% | 50% | 100% | + operator time, 100% | Break-even vs boat.dev |
|---|---|---|---|---|---|---|
| **boat.dev** (pay per second) | $0.036 | $0.036 | $0.036 | $0.036 | n/a | n/a |
| **Hetzner AX102** (13 VMs) | $0.322 | $0.129 | $0.064 | **$0.032** | $0.048 | 8,492 VM-h/mo (89% full); **never** with operator time |
| **OVH US ADVANCE-3** (6 VMs) | $0.644 | $0.258 | $0.129 | $0.064 | $0.099 | never (would need 179% of capacity) |
| **Latitude.sh m4.metal.medium** (13 VMs) | $0.487 | $0.195 | $0.097 | $0.049 | $0.065 | never (135%) |
| Hetzner AX42 (6 VMs) | $0.272 | $0.109 | $0.054 | $0.027 | $0.061 | 3,307 VM-h/mo (75% full); never with operator time |
| E2B / Daytona / Modal / Vercel | | | | | | $0.22–$0.68/h, see below |

**What the table shows.** At $0.036/h for 4 vCPU / 8 GB, boat.dev costs about as much as running the hardware yourself. Self-hosting comes out cheaper only on Hetzner, only when the box is more than 75–89% full around the clock, and only if your own time is free. Memory is the limit, because guest RAM is not overcommitted (1.0). CPU overcommit adds nothing for `default` VMs, since RAM runs out first. Compared with the other sandbox providers (about $0.33–$0.48 per hour), self-hosting is 5–15× cheaper at 50% utilization or above. You self-host for control, data locality, no per-start or plan limits, and custom images and kernels, not to undercut boat.dev.

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
- **Extra IPv4.** Hetzner charges €1.70/mo plus €4.90 setup per address (AX102 page). OVH US charges $2.00/mo with no setup fee ([OVH US blog](https://us.ovhcloud.com/resources/blog/internet-protocol-version-4-ipv4-pricing-adjustments)). The model needs none: VMs are reached over IPv6 and gateway URLs, as on boat.dev. If you give each VM a public IPv4, an AX102 full of VMs costs 13 × €1.70 = €22.10/mo more (about +$0.003 per VM-hour at 100% utilization).
- **Backup.** Hetzner charges €3.20/mo for a 1 TB Storage Box BX11 ([storage-box](https://www.hetzner.com/storage/storage-box/)). For the US servers I assumed $6/mo for 1 TB of S3-compatible storage *(unverified)*.

## 4. Model

**RAM capacity.** The model follows `docs/boat.md` (memory overcommit 1.0):
- Host reserve: 6 GB, the midpoint of 4–8.
- Each `default` VM: 8 GiB guest, 0.25 GiB VMM overhead and 0.5 GiB page-cache headroom, so 8.75 GB.
- VMs per server = ⌊(RAM − 6) / 8.75⌋.

**CPU capacity.** VMs per server = ⌊(threads − 2 reserved for the host) × overcommit / 4⌋.

| Server | VMs by RAM | VMs by CPU at 1× / 2× / 4× | **VMs at the default (4× CPU)** | Monthly cost¹ | + operator² |
|---|---|---|---|---|---|
| Hetzner AX42 | 6 | 3 / 7 / 14 | **6** (RAM-bound) | $119.04 | $269.04 |
| Hetzner AX102 | 13 | 7 / 15 / 30 | **13** (RAM) | $305.70 | $455.70 |
| Hetzner AX162 | 13 | 23 / 47 / 94 | **13** (RAM; 128 GB base) | $719.64 | $869.64 |
| OVH US ADVANCE-3 | 6 | 5 / 11 / 22 | **6** (RAM) | $282.25 | $432.25 |
| Latitude m4.metal.medium | 13 | 7 / 15 / 30 | **13** (RAM) | $462.00 | $612.00 |

¹ Monthly cost = price + setup/12 + backup.
² Operator time is assumed at **2 h/month × $75/h = $150/month** for patching, the weekly reboot window, abuse email, restore drills and disk alerts. This is a guess, so plug in your own number. A bad month (hardware swap, incident) is easily 10 hours.

At 1× CPU (no overcommit) the AX42, AX102, ADVANCE-3 and Latitude boxes become CPU-bound at 3, 7, 5 and 7 VMs, and their cost per VM-hour roughly doubles. The AX162 is the only box with CPU to spare, but at its 128 GB base RAM it holds the same 13 VMs as an AX102 for 2.4× the price. It only pays off with a RAM upgrade (512 GB would hold about 57 VMs; upgrade price not checked).

**Cost per VM-hour** = monthly cost / (VMs × 730 × utilization). The headline table shows the results. boat.dev charges $0.036 for every hour that actually runs, whatever the utilization.

**Break-even.** boat.dev costs max($20, $0.036 × VM-hours) per month. Self-hosting is cheaper once VM-hours exceed monthly cost / $0.036:

| Server | Break-even, no operator time | With operator time | Capacity (VMs × 730) |
|---|---|---|---|
| Hetzner AX42 | 3,307 h (75%) | 7,473 h (171%: never) | 4,380 h |
| Hetzner AX102 | 8,492 h (89%) | 12,658 h (133%: never) | 9,490 h |
| Hetzner AX162 | 19,990 h (211%: never) | 24,157 h (never) | 9,490 h |
| OVH US ADVANCE-3 | 7,840 h (179%: never) | 12,007 h (never) | 4,380 h |
| Latitude m4.metal.medium | 12,833 h (135%: never) | 17,000 h (never) | 9,490 h |

"Never" means the server cannot run enough `default` VM-hours in a month to undercut boat.dev. At the default overcommit, boat.dev's price per GB of RAM is below these servers' price per GB unless the box stays full.

**When self-hosting does win:**
- **Against E2B, Daytona, Modal or Vercel.** At about $0.33/h, a full AX102 breaks even at 924 VM-hours/month (10% utilization).
- **When RAM is effectively overcommitted.** Smaller guests, or agents that use less than their allocation, do not help today because the setting is 1.0. With memory overcommit 1.5, an AX102 holds about 20 VMs, and the cost at 100% utilization falls to about $0.021/h.
- **When you need something boat.dev doesn't sell.** That includes your own kernel or images, data on your own hardware, more than 12 starts per minute on the $20 plan, or no per-start and per-snapshot fees.

## 5. What self-hosting does not give you

- **Multi-region and redundancy.** One box is one failure domain. A disk, PSU or network fault takes every VM down until you fix it. Backups are nightly and crash-consistent, not live replicas.
- **Managed ops and an SLA.** You do the patching, kernel updates, reboots, monitoring and restores. Hetzner and OVH offer only hardware replacement and network SLAs, not uptime for your VMs.
- **Elastic burst.** Capacity is fixed at about 13 `default` VMs on an AX102. boat.dev's $20 plan allows 100 concurrent sandboxes. A second server means another month of rent and another setup fee, plus your own scheduling across boxes, which boat does not do.
- **Support.** Nobody answers for your VM layer but you.
- **Abuse handling.** You are the hosting provider for whatever tenants run. Complaints go to you, and the provider can null-route your IP (see `docs/boat.md`, *Abuse policy*).
- **Hourly exit.** You pay monthly plus a setup fee, and OVH's setup fee equals a full month. boat.dev is per second with a $20 floor.

## Reproduce

All derived numbers come from the inputs in `bench/boatbench/prices.json`:
- monthly cost = `usd_per_month + setup_usd/12 + backup_usd_per_month`
- VMs = `min(⌊(ram_gb − 6)/8.75⌋, ⌊(vcpu_threads − 2)·4/4⌋)`
- cost per VM-hour = cost / (VMs · 730 · utilization)
- break-even VM-hours = cost / 0.036

boatbench reads `pandaos.usd_per_hour` (AX102 at 100% utilization, no operator time: $0.0322) and `boatdev.usd_per_hour` ($0.036), and computes cost per run as `usd_per_hour × seconds / 3600`.
