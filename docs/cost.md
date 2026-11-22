# Cost: self-hosted boat vs boat.dev

All prices were read from public list-price pages on **2026-11-02**; boat.dev's AI proxy fee and its vendor benchmark figure (28.06 index units) were re-read on **2026-11-17**. They exclude VAT and are in USD unless marked. EUR prices are converted at **1.12 USD/EUR**, an approximate October 2026 spot rate ([30rates](https://30rates.com/eur-usd-forecast-tomorrow-this-week)) that was not checked for today. Unverified numbers are marked *(unverified)*. The machine-readable copy is [`bench/boatbench/prices.json`](../bench/boatbench/prices.json).

**Benchmark units.** The Node.js web-tooling **geometric-mean index** is the geometric mean of subtest rates (subtest runs/s). It does not count completed Node jobs or full suites per second. The host index is a sum of per-VM indices. All Node cost figures below are USD per 1000 index units, an index-normalized price; the ratios describe that benchmark index only. They do not establish the cost of completing 1000 jobs. Historical JSON keys ending in `_runs_s` retain their spelling. Completed-suite throughput is shown separately from saved pass counts and wall times; no compatible vendor completed-suite rate is available.

## Headline

One `default` VM (4 vCPU / 8 GiB), cost per VM-hour. Self-hosted figures include amortized setup and backup.
- **Conservative / untrusted** rows use `memOvercommit` 1.0. That is the setting `services.boat.untrustedTenants` forces, so **a public boat.dev alternative, which hosts strangers, runs these rows.**
- **Default** rows use boatd's default of 1.5, which is a bet that guests stay idle (see [Load](#what-happens-under-load)). They are only for one trust domain (your own agents).

The AX42 is the only server that has been benchmarked (see [Measured on a real dedicated server](#measured-on-a-real-dedicated-server-hetzner-ax42)). Its measured `MemTotal` (63,381 MiB) gives the 7 and 11 VMs below. Every other row's VM count is boatd's admission rule applied to an *estimated* `MemTotal` ([§4](#4-model)). **These are prices per VM-hour, not per benchmark index unit**, and the two lead to opposite answers. Per VM-hour, a full AX42 is cheaper than boat.dev. Per geometric-mean index unit, a full AX42 costs 1.46× (trusted) to 1.67× (untrusted) boat.dev's cost per index unit, because a VM on a packed host runs at 38–55% of its solo speed ([cost per index unit](#cost-per-index-unit-measured-on-the-ax42)). The [verdict](#verdict) separates the two by workload type.

**What is not measured, and provenance of checked-in AX42 artifacts (as of 2026-11-18).**
- **AX42 `trusted` lifecycle baselines (`0d160dd`):** the checked-in PandaOS AX42 lifecycle baselines (`0.15s` resume, `0.30s` fork, `0.32s` warm create, `0.89s` cold create in `ax42-r11-2026-11-13`, plus `ax42-pandaos-2026-11-12` and `ax42-r12-2026-11-13`) ran in **`trusted` mode** (`fc_cache_type=Unsafe` except the dedicated `Writeback` disk run, `warmPool` enabled in the default r11/r12 runs, and SMT on) and **predate `-ssh-policy-helper-file`** / online SSH quarantine across all presets. Round 23–24 batch SSH policy (`POST /v1/vms/ssh-policy-batch`) and the non-disruptive `rotate-token` guest helper subcommand are unit-tested to avoid `sshd` restarts and `nft -f -` reloads on steady-state token refreshes, while live AX42 hardware timings for both initial cold/restore SSH policy bootstrap and `rotate-token` await the next AX42 capture.
- **PandaOS rebooted `untrusted` pending hardware re-run:** production `untrusted` mode (`services.boat.untrustedTenants = true`: `nosmt=force`, `memOvercommit` 1.0, `storageOvercommit` 1, `warmPool` off, `blk-iocost` + `io.max`, `jailGid` `io_uring` `Async`, and online vsock/HTTPS SSH quarantine) is **pending an owner-approved rebooted AX42 hardware re-run**; the earlier Ubuntu loop-file `untrusted` run (`ax42-ubuntu-2026-11-11`) predated `stack.sh`'s `memOvercommit: 1` enforcement, `jailGid` `io_uring` (`Async`) setup, `losetup --direct-io=on` loop pass-through, and online SSH quarantine (`-ssh-policy-helper-file`). `bench/boatbench/presets.json` and `bench/boatbench/stack.sh` now enforce `memOvercommit: 1` for `untrusted` (matching `untrusted.json`) and are validated offline against `boatd`'s actual `loadConfig()` checks (`test_stack_config.py`), alongside `components/boat/boatd/e2e-perf.sh`, ready for the next hardware capture.
- **Live `boat.dev` unmeasured (`BOATDEV_API_KEY` not provided):** there is **no measured `boat.dev` column** anywhere on this page because `BOATDEV_API_KEY` was not provided; every `boat.dev` figure is its published list price or its own vendor-published solo-VM benchmark. **AX102 numbers are projections** from the AX42's measured curve; nobody has run PandaOS on an AX102.

**The VM counts are boatd's RAM admission, not the team quota.** Every team also has a quota (check `services.boat` team quota, boatd's `defaultTeam`). On the AX42 install it was 32 vCPU / 10 VMs / 64 GiB, which admits 8 `default` VMs, and the install run hit that cap. That is the default with `untrustedTenants` (the server profile's default); with `untrustedTenants = false` the default is 0, no per-team cap. To run the 11, 15 or 22 VMs priced below, raise that quota for the team that runs them.

Generated by `python3 bench/boatbench/boatbench.py costs` from `prices.json`. Self-hosted figures include amortized setup and backup. Utilization is the share of VM slots that run around the clock. Stopped-VM retention is not included here; it has [its own tables](#stopped-vm-retention-what-boatdevs-free-14-days-cost-on-own-hardware). boat.dev charges $0.036 per running `default` VM-hour subject to its **$20/mo minimum plan floor** (555 included `default` VM-hours/mo); at low-utilization cells below 555 VM-h/mo (e.g., 10% utilization on a 7-VM or 6-VM host is 511 or 438 VM-h/mo), boat.dev's effective rate rises to **$0.039–$0.046/h** ($20 ÷ VM-hours) rather than $0.036/h — still far below self-hosted $0.233–$0.272/h at 10% utilization.

| Server, mode (VMs) | 10% util. | 25% util. | 50% util. | 100% util. | + operator time, 100% | Break-even vs boat.dev ($0.036/h) |
|---|---|---|---|---|---|---|
| Hetzner AX42, conservative / untrusted (7) | $0.233 | $0.093 | $0.047 | $0.023 | $0.053 | 3,307 VM-h/mo, 65%; with operator time never (146%) |
| Hetzner AX42, untrusted full-disk worst-case, 100%-full 59 GiB disks (6) | $0.272 | $0.109 | $0.054 | $0.027 | $0.061 | 3,307 VM-h/mo, 75%; with operator time never (171%) |
| Hetzner AX42, default 1.5 (11) | $0.148 | $0.059 | $0.030 | $0.015 | $0.034 | 3,307 VM-h/mo, 41%; with operator time 93% |
| Hetzner AX102, conservative / untrusted (15) | $0.279 | $0.112 | $0.056 | $0.028 | $0.042 | 8,492 VM-h/mo, 78%; with operator time never (116%) |
| Hetzner AX102, default 1.5 (22) | $0.190 | $0.076 | $0.038 | $0.019 | $0.028 | 8,492 VM-h/mo, 53%; with operator time 79% |
| Hetzner AX162, conservative / untrusted (15) | $0.657 | $0.263 | $0.131 | $0.066 | $0.079 | 19,990 VM-h/mo, never (183%); with operator time never (221%) |
| Hetzner AX162, default 1.5 (22) | $0.448 | $0.179 | $0.090 | $0.045 | $0.054 | 19,990 VM-h/mo, never (124%); with operator time never (150%) |
| OVH US ADVANCE-3 (2024), conservative / untrusted (7) | $0.552 | $0.221 | $0.110 | $0.055 | $0.085 | 7,840 VM-h/mo, never (153%); with operator time never (235%) |
| OVH US ADVANCE-3 (2024), default 1.5 (11) | $0.351 | $0.141 | $0.070 | $0.035 | $0.054 | 7,840 VM-h/mo, 98%; with operator time never (150%) |
| Latitude.sh m4.metal.medium, conservative / untrusted (15) | $0.422 | $0.169 | $0.084 | $0.042 | $0.056 | 12,833 VM-h/mo, never (117%); with operator time never (155%) |
| Latitude.sh m4.metal.medium, default 1.5 (22) | $0.288 | $0.115 | $0.058 | $0.029 | $0.038 | 12,833 VM-h/mo, 80%; with operator time never (106%) |

E2B, Daytona, Modal and Vercel cost $0.22–$0.68/h ([§2](#2-other-sandbox-providers-about-4-vcpu--8-gb)).

**AX42 `7` RAM-bound sparse VMs vs `6` full-disk worst-case VMs, and stopped-VM retention.** `untrustedTenants` forces `storageOvercommit` 1, where [`snapshot.go`](../components/boat/boatd/snapshot.go) enforces two distinct checks: `hostStorageCheck` compares nominal committed bytes (`51 + 8 GiB ≈ 59 GiB` per running or stopped `default` VM) against raw `total * storageOvercommit`, while `checkFloor` enforces a live `minFreePct = 10` free-space floor on actual allocated blocks. On the AX42's single 512 GB NVMe (`476 GiB` raw partition, `404 GiB` usable after `minFreePct = 10` + ~5% OS/base images/btrfs metadata), `hostStorageCheck` admits up to `⌊476 / 59⌋ = 8` nominal VMs, so all **7** RAM-bound `default` VMs (`5,110 VM-h/mo, $0.023/h`) boot and run as long as disks remain sparse; however, if every tenant fills 100% of its 59 GiB disk, `checkFloor` (`minFreePct = 10%` on `404 GiB` usable) caps worst-case full-disk capacity at **6** VMs (`4,380 VM-h/mo, $0.027/h`), shown as its own worst-case row above. Once stopped sandboxes are kept (`R > 0`), nominal `storageOvercommit` 1 binds sharply. Raw-capacity sparse admission gives 8 AX42 / 30 AX102 storage slots; the full-disk reserve projection below uses 6 / 25: boat.dev keeps a stopped sandbox's disk **at least 14 days at no charge** (the tables price exactly 14), whereas holding each sandbox's nominal 59 GiB for its run time plus 336 h on an AX102 (**25 usable storage slots** on `1,519 GiB` usable of `1,788 GiB` raw partition) leaves 424 running VM-hours a month for 8 h sessions, or **$0.720** per running VM-hour ($1.169/h on an AX42). A thin-provisioning setting that charged actual use would lift most of that limit (*model*, see [retention](#stopped-vm-retention-what-boatdevs-free-14-days-cost-on-own-hardware)). boatd does not ship such a setting for untrusted mode, because a tenant could then fill the disk.

**What the table shows.** At $0.036/h list ($0.0288/h with boat.dev's published +25% top-up credit bonus) for 4 vCPU / 8 GB, boat.dev costs about as much as running the hardware yourself. In untrusted mode (no RAM overcommit), a single Hetzner host is cheaper per VM-hour than boat.dev's list rate only above 65–78% around-the-clock utilization (`75%` on AX42 full-disk worst-case; **81–97%** against boat.dev's +25% bonus rate of $0.0288/h, `94%` on AX42 full-disk worst-case), only if stopped VMs are deleted quickly, only without N+1 HA spare capacity ([TCO sensitivity](#credit-bonus-plan-fee--habackup-tco-sensitivity)), and only if your own time is free. At the default overcommit, which is for trusted guests only, the AX102 and AX42 beat boat.dev's list hourly rate even with operator time, at 79% and 93% full. Those VM counts hold only while guests stay mostly idle. Per geometric-mean index unit, the measured AX42 loses at every fill level. Against the other sandbox providers (about $0.33–$0.48 per hour), an AX102 is about 6–9× cheaper at 50% utilization with no overcommit. All of this assumes boat.dev's AI credentials proxy is off: with it on, boat.dev adds $1/day plus $1 per 1,000 requests, which for AI-heavy fleets can flip the result ([sensitivity](#boatdev-ai-proxy-fee-sensitivity-from-pricesjson)).

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
  - The AI credentials proxy costs **$1 for each UTC day it is on, plus $1 per 1,000 proxied requests**, on top of VM time ([pricing.md](https://docs.boat.dev/pricing.md), [ai-credentials.md](https://docs.boat.dev/ai-credentials.md), both re-read 2026-11-17). It also charges while sandboxes are stopped. The day fee is per account switch (one for you, one per organization), not per VM; the request charge goes to the wallet of the sandbox that made the request. Neither page defines what counts as a request (errors, blocked placeholders) or whether idle days are waived, and boat.dev has no automatic AI spend cap. A sandbox that holds real keys (proxy off) pays nothing and gets no usage tracking. **PandaOS's proxy has no fee.** The fee is modeled in the [AI-proxy sensitivity](#boatdev-ai-proxy-fee-sensitivity-from-pricesjson) below.
- **Included at no charge:**
  - Automatic snapshots, kept at least 14 days after a stop (the live page says "at least 14 days"; cleanup is currently report-only, so a disk can outlive that).
  - Egress.
  - Public IPv6.
  - Public URLs.
- **Not stated:** a per-GB snapshot storage price. A stopped sandbox keeps its disk for at least 14 days (the page's wording; cleanup is currently report-only). The retention tables below price exactly 14 days, the stated minimum.
- **Other terms:** No GPUs. 7-day trial (card required). Wallet bonus of 2–25% on payments of $200 or more.

## 2. Other sandbox providers, about 4 vCPU / 8 GB

**API compatibility vs cloud pricing reference:** PandaOS `boat` implements its native `/v1` API plus drop-in **`boat.dev` `/api/v1`** compatibility (98 of 122 operations mapped, tested against official `boat-sdk` and `@boatdev/sdk`; 24 SaaS-only routes return `501 not_supported` — see [`boat.md`](boat.md#boatdev-api-compatibility-apiv1)). E2B (`e2b.dev`), Daytona, Modal Sandbox, Vercel Sandbox, and Sprites are included strictly as **cloud per-VM-hour pricing references** — `boat` does **not** implement `e2b.dev`'s SDK/ConnectRPC protocol, Jupyter code-interpreter kernels, or Daytona/Modal/Vercel/Sprites SDKs.

| Provider | List rates | 4 vCPU / 8 GB per hour | Plan fee, caveats |
|---|---|---|---|
| [E2B](https://e2b.dev/pricing) | $0.000014/vCPU-s, $0.0000045/GiB-s, disk free | **$0.331** | Pro costs $150/mo and is needed for sessions over 1 h (Hobby max 1 h, 20 concurrent). |
| [Daytona](https://www.daytona.io/pricing) | $0.0504/vCPU-h, $0.0162/GiB-h, $0.000108/GiB-h disk after 5 GiB | **$0.336** with 51 GB disk ($0.331 without) | $200 free compute. Limits are not published. |
| [Modal Sandbox](https://modal.com/pricing) | $0.00003942/core-s (1 core = 2 vCPU), $0.00000667/GiB-s | **$0.476** (2 cores + 8 GiB) | Starter $0 with $30/mo credit, Team $250/mo. Sandbox rates are 3× function rates. |
| [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox/pricing) (Pro, iad1) | $0.128/active-CPU-h, $0.0212/GB-h provisioned memory, $0.60/1M creations | **$0.221** at 10% CPU, **$0.682** at 100% | **Not like-for-like:** CPU is billed only while busy, so cost depends on the workload. Pro $20/mo credit. 8 vCPU / 16 GB max on Pro. 24 h session max. |

E2B and Daytona bill by the resource you size, so they are roughly like-for-like. Modal "cores" are physical cores. Vercel (and active-CPU-billed sandboxes like Fly.io Sprites) bills CPU only while active; Vercel ties memory to vCPUs (2 GB each). None of the provisioned-hour providers above bills idle time differently from boat.dev.

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

These counts are RAM admission only. The team quota is checked separately, so check `services.boat` team quota (boatd's `defaultTeam`) and raise it for the team that should run 11, 15 or 22 VMs. On the AX42 install it admitted 8.

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

**Cost per VM-hour** = monthly cost / (VMs × 730 × utilization). The headline table shows the results. boat.dev charges $0.036 per running `default` VM-hour subject to its $20/mo minimum plan floor (555 included VM-hours/mo, so low-utilization workloads below 555 VM-h/mo — such as the 10% utilization cells on 6- and 7-VM hosts — pay the $20/mo floor, i.e. $0.039–$0.046/h effective).

**Break-even.** At list rates boat.dev costs max($20, $0.036 × VM-hours) per month across its published **$20, $100, $500 and $2,000** monthly plans (whose fee comes back as `555 / 2,777 / 13,888 / 55,555` included `default` VM-hours at `$0.0360/h`, with higher concurrency and start-rate limits — `$100/mo` gives `30/210/840` starts per min/h/day, needed for a 5-repeat `boatbench` run). Separately, wallet top-up payments of **$200+** earn a **+2% to +25%** credit bonus, bringing effective compute down from `$0.0353/h` (`+2%`) to `$0.036 / 1.25 =` **$0.0288/h** (`+25%`). Self-hosting is cheaper once VM-hours exceed monthly cost / boat.dev's effective hourly rate. The last column of the [headline table](#headline) gives that point against the $0.036/h list rate as a share of capacity (VMs × 730); the sensitivity table below adds the +25% bonus rate ($0.0288/h), operator time, and N+1 HA / off-host snapshot backup topologies. Above 100% means never. Both columns ignore stopped-VM retention.

### Credit-bonus, plan-fee & HA/backup TCO sensitivity

Generated by `python3 bench/boatbench/boatbench.py costs` from `prices.json`.

Published `boat.dev` monthly plans (`$20`, `$100`, `$500`, `$2,000`) and separate wallet top-up credit bonus tiers (`+2%` to `+25%` on `$200+` payments, `default` 4 vCPU / 8 GB):

| boat.dev tier | Monthly fee | Top-up bonus | Effective $/VM-hour | Notes |
|---|---|---|---|---|
| Plan $20/mo (minimum) | $20/mo | +0% | $0.0360/h | 555 included default VM-hours; 100 concurrent; 12/60/200 starts per min/h/day; 10 saved snapshots |
| Plan $100/mo | $100/mo | +0% | $0.0360/h | 2,777 included default VM-hours; 300 concurrent; 30/210/840 starts per min/h/day (needed for 5-repeat boatbench); 50 saved snapshots |
| Plan $500/mo | $500/mo | +0% | $0.0360/h | 13,888 included default VM-hours; 1,000 concurrent; 65/420/1,680 starts per min/h/day; 250 saved snapshots |
| Plan $2,000/mo | $2,000/mo | +0% | $0.0360/h | 55,555 included default VM-hours; 2,000 concurrent; 90/600/2,400 starts per min/h/day; 1,000 saved snapshots |
| Wallet top-up ($200+ min bonus, +2%) | $0/mo | +2% | $0.0353/h | Wallet bonus starts at +2% on top-up payments of $200 or more ($0.036 / 1.02, separate from monthly plan fee) |
| Wallet top-up (max published bonus, +25%) | $0/mo | +25% | $0.0288/h | $0.036 / 1.25 = $0.0288/VM-hour at maximum published +25% wallet top-up bonus |

**Architecture & TCO formula note:** **`boatd` and `boat-api` are single-host daemons (no built-in multi-node scheduler, shared control plane, or automatic failover)**, so the `4+1` and `1+1` rows below model independent hosts behind external routing plus periodic off-host snapshot export, not built-in live HA. Let `H = usd_per_month + setup_usd / 12`. **4+1 total cluster TCO = `5 × H + 14.784`; per serving host = `(5 × H + 14.784) / 4`**. The table allocates this across four serving hosts; the fifth is spare. Single host = `H + backup_usd_per_month`; 1+1 pair = `2 × H + 14.784` per one serving host. Multi-host topologies replace the single-host archive with **one shared `$14.78/mo` (`€13.20 × 1.12`) 5 TB Hetzner BX21 off-host archive**. The archive is counted once per cluster. Archive capacity and restore time are assumptions, not measured availability. The `+ op ($150/mo)` column adds `$150/mo` per serving host (a conservative operator allowance).

Self-hosted `untrusted` (`memOvercommit 1.0`, SMT off) TCO sensitivity across single-node vs N+1 HA + off-host snapshot backup storage, compared against both `boat.dev` list PAYG ($0.0360/h) and max +25% credit bonus ($0.0288/h):

| Server (untrusted VMs) | Self-hosted topology | Monthly TCO / serving host | 50% util. $/VM-h | 100% util. $/VM-h | + op ($150/mo) 100% $/VM-h | Break-even vs list ($0.0360/h) | Break-even vs +25% bonus ($0.0288/h) |
|---|---|---|---|---|---|---|---|
| Hetzner AX42 (7) | Single host (1.00x, basic backup) | $119.04 | $0.047 | $0.023 | $0.053 | 3,307 VM-h/mo (65%) | 4,133 VM-h/mo (81%) |
| Hetzner AX42 (7) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $148.01 | $0.058 | $0.029 | $0.058 | 4,111 VM-h/mo (80%) | 5,139 VM-h/mo (never (101%)) |
| Hetzner AX42 (7) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $245.69 | $0.096 | $0.048 | $0.077 | 6,825 VM-h/mo (never (134%)) | 8,531 VM-h/mo (never (167%)) |
| Hetzner AX42 full-disk worst-case (6) | Single host (1.00x, basic backup) | $119.04 | $0.054 | $0.027 | $0.061 | 3,307 VM-h/mo (75%) | 4,133 VM-h/mo (94%) |
| Hetzner AX42 full-disk worst-case (6) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $148.01 | $0.068 | $0.034 | $0.068 | 4,111 VM-h/mo (94%) | 5,139 VM-h/mo (never (117%)) |
| Hetzner AX42 full-disk worst-case (6) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $245.69 | $0.112 | $0.056 | $0.090 | 6,825 VM-h/mo (never (156%)) | 8,531 VM-h/mo (never (195%)) |
| Hetzner AX102 (15) | Single host (1.00x, basic backup) | $305.70 | $0.056 | $0.028 | $0.042 | 8,492 VM-h/mo (78%) | 10,615 VM-h/mo (97%) |
| Hetzner AX102 (15) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $381.35 | $0.070 | $0.035 | $0.049 | 10,593 VM-h/mo (97%) | 13,241 VM-h/mo (never (121%)) |
| Hetzner AX102 (15) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $619.02 | $0.113 | $0.057 | $0.070 | 17,195 VM-h/mo (never (157%)) | 21,494 VM-h/mo (never (196%)) |
| Hetzner AX162 (15) | Single host (1.00x, basic backup) | $719.64 | $0.131 | $0.066 | $0.079 | 19,990 VM-h/mo (never (183%)) | 24,987 VM-h/mo (never (228%)) |
| Hetzner AX162 (15) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $898.76 | $0.164 | $0.082 | $0.096 | 24,966 VM-h/mo (never (228%)) | 31,207 VM-h/mo (never (285%)) |
| Hetzner AX162 (15) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $1,446.89 | $0.264 | $0.132 | $0.146 | 40,191 VM-h/mo (never (367%)) | 50,239 VM-h/mo (never (459%)) |
| OVH US ADVANCE-3 (2024) (7) | Single host (1.00x, basic backup) | $282.25 | $0.110 | $0.055 | $0.085 | 7,840 VM-h/mo (never (153%)) | 9,800 VM-h/mo (never (192%)) |
| OVH US ADVANCE-3 (2024) (7) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $349.01 | $0.137 | $0.068 | $0.098 | 9,695 VM-h/mo (never (190%)) | 12,118 VM-h/mo (never (237%)) |
| OVH US ADVANCE-3 (2024) (7) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $567.28 | $0.222 | $0.111 | $0.140 | 15,758 VM-h/mo (never (308%)) | 19,697 VM-h/mo (never (385%)) |
| Latitude.sh m4.metal.medium (15) | Single host (1.00x, basic backup) | $462.00 | $0.084 | $0.042 | $0.056 | 12,833 VM-h/mo (never (117%)) | 16,042 VM-h/mo (never (146%)) |
| Latitude.sh m4.metal.medium (15) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $573.70 | $0.105 | $0.052 | $0.066 | 15,936 VM-h/mo (never (146%)) | 19,920 VM-h/mo (never (182%)) |
| Latitude.sh m4.metal.medium (15) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $926.78 | $0.169 | $0.085 | $0.098 | 25,744 VM-h/mo (never (235%)) | 32,180 VM-h/mo (never (294%)) |

### boat.dev AI-proxy fee sensitivity (from prices.json)

boat.dev's AI credentials proxy is billed apart from VM time: **$1 per UTC day it is on plus $1 per 1,000 proxied requests**, from `prices.json` `boatdev.ai_proxy`. It matters only for agents that use boat.dev's proxy instead of holding real keys. Assumptions: the requests are the fleet's total per UTC day; the fleet runs 24/7 for the effective rate (the day fee also accrues while sandboxes are stopped); list rate $0.036/h, no wallet bonus (a bonus would shrink the fee too); 730 h = 30.42 days a month; self-hosted cost is whole hosts at the `untrusted` VM count (`ceil(fleet / VMs per host)` × monthly cost, no operator time or HA); **break-even duty** is the share of each VM's hours that must run before boat.dev costs as much as those hosts, where `boat.dev = fleet × 730 × duty × $0.036 + proxy fee` (the fee does not shrink with duty) and `any` means the fee alone exceeds the hosts. `0` requests/day is proxy-on-but-idle ($1/day).

| Proxied requests/day (fleet) | Fleet (VMs) | Proxy fee $/mo | boat.dev effective $/VM-h, 24/7 | Break-even duty vs Hetzner AX42 (7 VMs/host, $119.04/mo/host) | Break-even duty vs Hetzner AX102 (15 VMs/host, $305.70/mo/host) |
|---|---|---|---|---|---|
| 0 | 1 | $30.42 | $0.078 | never (337%) (1 host) | never (1048%) (1 host) |
| 0 | 7 | $30.42 | $0.042 | 48% (1 host) | never (150%) (1 host) |
| 0 | 15 | $30.42 | $0.039 | 83% (3 hosts) | 70% (1 host) |
| 0 | 50 | $30.42 | $0.037 | 70% (8 hosts) | 91% (4 hosts) |
| 0 | 100 | $30.42 | $0.036 | 67% (15 hosts) | 80% (7 hosts) |
| 2,000 | 1 | $91.25 | $0.161 | never (106%) (1 host) | never (816%) (1 host) |
| 2,000 | 7 | $91.25 | $0.054 | 15% (1 host) | never (117%) (1 host) |
| 2,000 | 15 | $91.25 | $0.044 | 67% (3 hosts) | 54% (1 host) |
| 2,000 | 50 | $91.25 | $0.038 | 66% (8 hosts) | 86% (4 hosts) |
| 2,000 | 100 | $91.25 | $0.037 | 64% (15 hosts) | 78% (7 hosts) |
| 20,000 | 1 | $638.75 | $0.911 | any (fee alone > hosts) (1 host) | any (fee alone > hosts) (1 host) |
| 20,000 | 7 | $638.75 | $0.161 | any (fee alone > hosts) (1 host) | any (fee alone > hosts) (1 host) |
| 20,000 | 15 | $638.75 | $0.094 | any (fee alone > hosts) (3 hosts) | any (fee alone > hosts) (1 host) |
| 20,000 | 50 | $638.75 | $0.053 | 24% (8 hosts) | 44% (4 hosts) |
| 20,000 | 100 | $638.75 | $0.045 | 44% (15 hosts) | 57% (7 hosts) |
| 100,000 | 1 | $3,072.08 | $4.244 | any (fee alone > hosts) (1 host) | any (fee alone > hosts) (1 host) |
| 100,000 | 7 | $3,072.08 | $0.637 | any (fee alone > hosts) (1 host) | any (fee alone > hosts) (1 host) |
| 100,000 | 15 | $3,072.08 | $0.317 | any (fee alone > hosts) (3 hosts) | any (fee alone > hosts) (1 host) |
| 100,000 | 50 | $3,072.08 | $0.120 | any (fee alone > hosts) (8 hosts) | any (fee alone > hosts) (4 hosts) |
| 100,000 | 100 | $3,072.08 | $0.078 | any (fee alone > hosts) (15 hosts) | any (fee alone > hosts) (7 hosts) |

Reading it: with the proxy off (real keys in the VM) the fee is $0 and the earlier tables apply unchanged. At 2,000 requests/day the fee is $91/mo, which moves the AX42 break-even for a 7-VM fleet from 65% to 15% of hours. At 20,000 requests/day ($639/mo) a fleet of up to 15 VMs is cheaper on one AX102 (or three AX42s) at any utilization, and at 100,000 requests/day ($3,072/mo) the fee alone exceeds 7 AX102s. **For AI-heavy fleets the proxy fee, not the VM rate, decides the comparison.** The fee matches a whole AX102 ($305.70/mo) at about 9,050 requests/day. The fee is moot when real keys are held in the VM, but boat.dev then has no usage tracking or blocking for that sandbox.

### Stopped-VM retention: what boat.dev's free 14 days cost on own hardware

**Where the cost comes from.** The retention cost comes from our own `storageOvercommit` 1 policy, which `untrustedTenants` forces, combined with usable NVMe headroom: `boatd` holds back `minFreePct = 10` (10% of the filesystem) and OS/base guest images/btrfs metadata reserve another ~5%, leaving **85% usable tenant storage** (`usable_disk_factor = 0.85`: **404 GiB** usable of `476 GiB` raw on the AX42, **1,519 GiB** of `1,788 GiB` on the AX102 and AX162, **759 GiB** of `894 GiB` on OVH Advance-3, and **1,503 GiB** of `1,769 GiB` on Latitude m4.metal.medium). Every VM, running or stopped, is charged its nominal disk plus its memory snapshot (about 59 GiB for a `default` VM) against raw capacity, whatever it actually uses. A stopped VM therefore takes one nominal storage slot and no RAM slot. Raw-capacity sparse admission permits 8 AX42 / 30 AX102 storage slots as long as actual blocks remain below the live floor. With fully occupied quotas and reserve headroom, the projection uses 6 / 25 slots. In that full-disk case the marginal cost of keeping one stopped VM is monthly cost / usable storage slots: on an AX102 that is $305.70 / 25 = **$12.23 a month** ($5.63 for boat.dev's 14 days); on an AX42 it is $119.04 / 6 = **$19.84 a month** — where `boatd`'s `hostStorageCheck` (`⌊476 / 59⌋ = 8`) lets all **7** RAM-bound VMs (`5,110 VM-h/mo, $0.023/h`) boot while disks stay sparse, whereas worst-case 100%-full 59 GiB disks hit `checkFloor` (`minFreePct = 10%` on `404 GiB` usable) at **6** VMs (`4,380 VM-h/mo, $0.027/h`) even at `R = 0`.

Running VM-hours per month ≤ min(RAM slots × 730, case-specific storage slots × 730 × H / (H + R)), for sessions of H hours kept R hours after stopping. Cost per running VM-hour is at 100% of that capacity, with no operator time. The tables below come from `python3 bench/boatbench/boatbench.py costs`.

- **PROJECTION, shipped raw-capacity sparse rows:** raw nominal admission under the shipped policy, conditional on sparse allocation.
- **PROJECTION, full-disk reserve rows:** fully occupied quotas after the free-space and metadata reserves. Both are projections, not measured retained fleets.
- **MODEL rows:** a thin-provisioning setting that charges each VM its actual use. **They are a model, not a measurement, and boatd has no such setting for untrusted mode today.** Charging actual use lets a tenant grow into space the admission check did not reserve, so it needs a quota per VM (btrfs qgroups exist) and a policy for a full disk. **Not built in r12, because boatd is not half-way there:** each *team* already has a qgroup limit (its subvolume, 1.25 × its `maxDiskGiB`) and the free-space floor refuses creates and snapshots, but a per-VM limit needs a subvolume per VM, which boatd gave up because a memory file hard-linked across subvolumes becomes a reflink that waits for its writeback; and returning a VM's free blocks is not possible: Firecracker's virtio-blk offers no discard (`discard_max_bytes` 0 in the guest, boat-e2e 43379945048), and punching them from the host (`e2fsck -E discard` on a stopped VM's disk) would replay the guest's journal under a memory snapshot that still holds that filesystem, as root, on tenant-controlled ext4. The cost inputs stay as measured (3.35 GiB per stopped VM, `thin.json`). The per-VM use figures and their sources are in `prices.json` (`assumptions.retention`).

#### Stopped-VM retention: Hetzner AX42

Hetzner AX42, untrusted (7 RAM slots), usable tenant store **404 GiB** of 476 GiB raw partition (deducts OS/images/btrfs metadata + `boatd` `minFreePct=10%`; measured: PandaOS install, boat store = btrfs on one 512 GB NVMe, df 477G (2026-11-12); 404 GiB usable after 15% reserve (10% boatd minFreePct + 5% base images & btrfs metadata)). Note: `boatd` `hostStorageCheck` checks raw partition capacity (`⌊476 / 59⌋ = 8` nominal VMs), so all **7** RAM-bound VMs (`5,110 VM-h/mo, $0.023/h`) boot while disks remain sparse; if every tenant fills 100% of its 59 GiB quota, live `checkFloor` (`minFreePct=10%`) caps worst-case full-disk capacity at **6** VMs (`4,380 VM-h/mo, $0.027/h`). All retention cells are projections, not measured retained fleets. Raw-capacity sparse uses nominal admission against the raw partition and assumes allocated blocks stay below the live floor; full-disk reserve budgets fully occupied quotas against usable storage. Thin rows model a policy that is not shipped. Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.

| Storage policy | Storage slots (case) | Session H | R = 0 (delete on stop) | R = 1 day | R = 14 days |
|---|---|---|---|---|---|
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 8 | 1 h | 5,110 (RAM-bound), $0.023/h | 234, $0.510/h | 17, $6.869/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 8 | 8 h | 5,110 (RAM-bound), $0.023/h | 1,460, $0.082/h | 136, $0.876/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 8 | 24 h | 5,110 (RAM-bound), $0.023/h | 2,920, $0.041/h | 389, $0.306/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 8 | 168 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 1,947, $0.061/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 6 | 1 h | 4,380, $0.027/h | 175, $0.679/h | 13, $9.159/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 6 | 8 h | 4,380, $0.027/h | 1,095, $0.109/h | 102, $1.169/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 6 | 24 h | 4,380, $0.027/h | 2,190, $0.054/h | 292, $0.408/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 6 | 168 h | 4,380, $0.027/h | 3,832, $0.031/h | 1,460, $0.082/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 118 | 1 h | 5,110 (RAM-bound), $0.023/h | 3,446, $0.035/h | 256, $0.466/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 118 | 8 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 2,003, $0.059/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 118 | 24 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 118 | 168 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 40 | 1 h | 5,110 (RAM-bound), $0.023/h | 1,168, $0.102/h | 87, $1.374/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 40 | 8 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 679, $0.175/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 40 | 24 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 1,947, $0.061/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 40 | 168 h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h | 5,110 (RAM-bound), $0.023/h |

(a) measured: a stopped default VM after the Node loop setup, btrfs exclusive bytes incl. memory snapshot, 3.35 GiB; PandaOS AX42 2026-11-12, results/ax42-pandaos-2026-11-12/thin.json.
(b) illustrative: a heavier session.

#### Stopped-VM retention: Hetzner AX102

Hetzner AX102, untrusted (15 RAM slots), usable tenant store **1,519 GiB** of 1,788 GiB raw partition (deducts OS/images/btrfs metadata + `boatd` `minFreePct=10%`; estimate: one 1.92 TB NVMe = 1,788 GiB raw; 1,519 GiB usable after 15% reserve (10% boatd minFreePct + 5% OS/base images & btrfs metadata)). All retention cells are projections, not measured retained fleets. Raw-capacity sparse uses nominal admission against the raw partition and assumes allocated blocks stay below the live floor; full-disk reserve budgets fully occupied quotas against usable storage. Thin rows model a policy that is not shipped. Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.

| Storage policy | Storage slots (case) | Session H | R = 0 (delete on stop) | R = 1 day | R = 14 days |
|---|---|---|---|---|---|
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 1 h | 10,950 (RAM-bound), $0.028/h | 876, $0.349/h | 65, $4.704/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 8 h | 10,950 (RAM-bound), $0.028/h | 5,475, $0.056/h | 509, $0.600/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 24 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 1,460, $0.209/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 168 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 7,300, $0.042/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 1 h | 10,950 (RAM-bound), $0.028/h | 730, $0.419/h | 54, $5.645/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 8 h | 10,950 (RAM-bound), $0.028/h | 4,562, $0.067/h | 424, $0.720/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 24 h | 10,950 (RAM-bound), $0.028/h | 9,125, $0.034/h | 1,217, $0.251/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 168 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 6,083, $0.050/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 1 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 966, $0.316/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 8 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 7,572, $0.040/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 24 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 168 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 1 h | 10,950 (RAM-bound), $0.028/h | 4,409, $0.069/h | 327, $0.935/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 8 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 2,563, $0.119/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 24 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 7,349, $0.042/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 168 h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h | 10,950 (RAM-bound), $0.028/h |

(a) measured: a stopped default VM after the Node loop setup, btrfs exclusive bytes incl. memory snapshot, 3.35 GiB; PandaOS AX42 2026-11-12, results/ax42-pandaos-2026-11-12/thin.json.
(b) illustrative: a heavier session.

#### Stopped-VM retention: Hetzner AX162

Hetzner AX162, untrusted (15 RAM slots), usable tenant store **1,519 GiB** of 1,788 GiB raw partition (deducts OS/images/btrfs metadata + `boatd` `minFreePct=10%`; estimate: one 1.92 TB NVMe = 1,788 GiB raw; 1,519 GiB usable after 15% reserve (10% boatd minFreePct + 5% OS/base images & btrfs metadata)). All retention cells are projections, not measured retained fleets. Raw-capacity sparse uses nominal admission against the raw partition and assumes allocated blocks stay below the live floor; full-disk reserve budgets fully occupied quotas against usable storage. Thin rows model a policy that is not shipped. Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.

| Storage policy | Storage slots (case) | Session H | R = 0 (delete on stop) | R = 1 day | R = 14 days |
|---|---|---|---|---|---|
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 1 h | 10,950 (RAM-bound), $0.066/h | 876, $0.822/h | 65, $11.074/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 8 h | 10,950 (RAM-bound), $0.066/h | 5,475, $0.131/h | 509, $1.413/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 24 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 1,460, $0.493/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 30 | 168 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 7,300, $0.099/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 1 h | 10,950 (RAM-bound), $0.066/h | 730, $0.986/h | 54, $13.289/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 8 h | 10,950 (RAM-bound), $0.066/h | 4,562, $0.158/h | 424, $1.696/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 24 h | 10,950 (RAM-bound), $0.066/h | 9,125, $0.079/h | 1,217, $0.591/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 168 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 6,083, $0.118/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 1 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 966, $0.745/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 8 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 7,572, $0.095/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 24 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 446 | 168 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 1 h | 10,950 (RAM-bound), $0.066/h | 4,409, $0.163/h | 327, $2.200/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 8 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 2,563, $0.281/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 24 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 7,349, $0.098/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 151 | 168 h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h | 10,950 (RAM-bound), $0.066/h |

(a) measured: a stopped default VM after the Node loop setup, btrfs exclusive bytes incl. memory snapshot, 3.35 GiB; PandaOS AX42 2026-11-12, results/ax42-pandaos-2026-11-12/thin.json.
(b) illustrative: a heavier session.

#### Stopped-VM retention: OVH US ADVANCE-3 (2024)

OVH US ADVANCE-3 (2024), untrusted (7 RAM slots), usable tenant store **759 GiB** of 894 GiB raw partition (deducts OS/images/btrfs metadata + `boatd` `minFreePct=10%`; estimate: one 960 GB NVMe = 894 GiB raw; 759 GiB usable after 15% reserve (10% boatd minFreePct + 5% OS/base images & btrfs metadata)). All retention cells are projections, not measured retained fleets. Raw-capacity sparse uses nominal admission against the raw partition and assumes allocated blocks stay below the live floor; full-disk reserve budgets fully occupied quotas against usable storage. Thin rows model a policy that is not shipped. Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.

| Storage policy | Storage slots (case) | Session H | R = 0 (delete on stop) | R = 1 day | R = 14 days |
|---|---|---|---|---|---|
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 15 | 1 h | 5,110 (RAM-bound), $0.055/h | 438, $0.644/h | 32, $8.687/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 15 | 8 h | 5,110 (RAM-bound), $0.055/h | 2,738, $0.103/h | 255, $1.108/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 15 | 24 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 730, $0.387/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 15 | 168 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 3,650, $0.077/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 12 | 1 h | 5,110 (RAM-bound), $0.055/h | 350, $0.806/h | 26, $10.858/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 12 | 8 h | 5,110 (RAM-bound), $0.055/h | 2,190, $0.129/h | 204, $1.385/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 12 | 24 h | 5,110 (RAM-bound), $0.055/h | 4,380, $0.064/h | 584, $0.483/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 12 | 168 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 2,920, $0.097/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 223 | 1 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 483, $0.584/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 223 | 8 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 3,786, $0.075/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 223 | 24 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 223 | 168 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 75 | 1 h | 5,110 (RAM-bound), $0.055/h | 2,190, $0.129/h | 162, $1.737/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 75 | 8 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 1,273, $0.222/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 75 | 24 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 3,650, $0.077/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 75 | 168 h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h | 5,110 (RAM-bound), $0.055/h |

(a) measured: a stopped default VM after the Node loop setup, btrfs exclusive bytes incl. memory snapshot, 3.35 GiB; PandaOS AX42 2026-11-12, results/ax42-pandaos-2026-11-12/thin.json.
(b) illustrative: a heavier session.

#### Stopped-VM retention: Latitude.sh m4.metal.medium

Latitude.sh m4.metal.medium, untrusted (15 RAM slots), usable tenant store **1,503 GiB** of 1,769 GiB raw partition (deducts OS/images/btrfs metadata + `boatd` `minFreePct=10%`; estimate: one 1.9 TB data NVMe = 1,769 GiB raw; 1,503 GiB usable after 15% reserve (10% boatd minFreePct + 5% OS/base images & btrfs metadata)). All retention cells are projections, not measured retained fleets. Raw-capacity sparse uses nominal admission against the raw partition and assumes allocated blocks stay below the live floor; full-disk reserve budgets fully occupied quotas against usable storage. Thin rows model a policy that is not shipped. Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.

| Storage policy | Storage slots (case) | Session H | R = 0 (delete on stop) | R = 1 day | R = 14 days |
|---|---|---|---|---|---|
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 29 | 1 h | 10,950 (RAM-bound), $0.042/h | 847, $0.546/h | 63, $7.354/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 29 | 8 h | 10,950 (RAM-bound), $0.042/h | 5,292, $0.087/h | 492, $0.938/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 29 | 24 h | 10,950 (RAM-bound), $0.042/h | 10,585, $0.044/h | 1,411, $0.327/h |
| PROJECTION, shipped raw-capacity sparse: 59 GiB nominal per VM | 29 | 168 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 7,057, $0.065/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 1 h | 10,950 (RAM-bound), $0.042/h | 730, $0.633/h | 54, $8.531/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 8 h | 10,950 (RAM-bound), $0.042/h | 4,562, $0.101/h | 424, $1.089/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 24 h | 10,950 (RAM-bound), $0.042/h | 9,125, $0.051/h | 1,217, $0.380/h |
| PROJECTION, full-disk reserve: 59 GiB per VM | 25 | 168 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 6,083, $0.076/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 442 | 1 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 957, $0.483/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 442 | 8 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 7,504, $0.062/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 442 | 24 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h |
| MODEL, thin: 3.4 GiB actual use per VM (a) | 442 | 168 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 150 | 1 h | 10,950 (RAM-bound), $0.042/h | 4,380, $0.105/h | 325, $1.422/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 150 | 8 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 2,547, $0.181/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 150 | 24 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 7,300, $0.063/h |
| MODEL, thin: 10 GiB actual use per VM (b) | 150 | 168 h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h | 10,950 (RAM-bound), $0.042/h |

(a) measured: a stopped default VM after the Node loop setup, btrfs exclusive bytes incl. memory snapshot, 3.35 GiB; PandaOS AX42 2026-11-12, results/ax42-pandaos-2026-11-12/thin.json.
(b) illustrative: a heavier session.

Every table assumes each sandbox runs one session and is then kept the full R hours. A sandbox resumed within R reuses its slot and does better. Other ways out, none built into boatd today: a bigger store (more NVMe; price not checked), or archiving stopped VMs off the host (a Storage Box costs €3.20/TB-month, but boatd has no archive tier). Trusted mode (`storageOvercommit` 10) does not bind here.

## 4b. Untrusted tenants (a public boat.dev alternative)

`services.boat.untrustedTenants` ([`module.nix`](../components/boat/module.nix)) is the mode a service that hosts strangers must run. **This shipped default (SMT off, iocost, no warm pool, `memOvercommit` 1.0, `storageOvercommit` 1) has not been benchmarked on the AX42.** It needs `nosmt=force` on the kernel command line, which means a reboot of the owner's box; that is waiting for the owner's approval, so every AX42 number on this page from the PandaOS install is trusted mode. The PandaOS install run below ran in trusted mode, and the Ubuntu run's untrusted column used an older untrusted configuration (`Sync` engine, `io.max` 500 MB/s, see the table below), not today's. The server profile turns it on by default; a host with only trusted tenants opts out with `untrustedTenants = false` (left at the profile default, the domain checks warn; set to `true` explicitly, they are errors). What it changes for cost:

| | Trusted (defaults) | Untrusted |
|---|---|---|
| `memOvercommit` | 1.5 | **1.0**: the conservative rows. AX102 **15** VMs, not 22 |
| SMT | on | **off** (`nosmt=force`): an AX102 has 16 threads, not 32 |
| VMs by CPU at `cpuOvercommit` 4 | 32 on an AX102 | **16**: RAM (15) still binds first, but 15 VMs are 60 vCPUs on 16 cores, 3.75 per core instead of 1.9 per thread |
| VMs at 1× CPU (no CPU overcommit) | 8 | **4** |
| Disk | `auto` engine (io_uring), no I/O cap | `Async` (`io_uring`) scoped strictly to the jailed VMM group (`kernel.io_uring_disabled = 1` + `kernel.io_uring_group = jailGid` in both `nixos/profiles/server.nix` and `bench/boatbench/stack.sh`; historical `ax42-ubuntu-2026-11-11` and CI runs before R22 fell back to `Sync`); busy VMs share the disk by weight (blk-iocost with the disk's measured model and p95 latency targets), `io.max` 4,000 MB/s and 200,000 IOPS per VM as a ceiling. Until 2026-11-10: `io.max` 500 MB/s and 20,000 IOPS (the run below) |
| Network | no cap | 25% uplink share per VM with fair queueing, 4,096 tracked connections, no SMTP (until `46507a8`: 1,000 Mbit/s egress and ingress per VM, which the AX42 Ubuntu run used) |
| Storage | `storageOvercommit` 10 | **1**: nominal committed storage (51 GiB disk + 8 GiB memory + logs, about 59 GiB per VM, running or stopped, plus snapshots) is checked against `total * storageOvercommit` in `hostStorageCheck`, while `checkFloor` enforces a live `minFreePct = 10` free-space floor (~85% usable tenant storage after `minFreePct = 10` + ~5% OS/base images/btrfs metadata). **25 usable storage slots** (`1,519 GiB` usable of `1,788 GiB` raw) on an AX102's 1.92 TB mirror *(estimate: raw size before RAID 1 and btrfs)*. On the AX42 (**476 GiB** raw btrfs directly on **one** 512 GB NVMe, `df` 477G, 2026-11-12, `404 GiB` usable), `hostStorageCheck` admits `⌊476 / 59⌋ = 8` nominal VMs so all **7** RAM-bound VMs (`$0.023/h`) boot while disks stay sparse, whereas `checkFloor` (`minFreePct = 10%`) caps worst-case 100%-full 59 GiB disks at **6** usable storage slots (`404 / 59 = 6`, `$0.027/h`). That single-disk install store has **no redundancy** (the Ubuntu run used mdadm RAID 1 across both; a mirrored install has about the same usable size and survives a disk failure). **Storage binds before RAM as soon as stopped VMs are kept — and on the AX42 (6 full-disk slots vs 7 RAM slots) whenever tenants fill their full 59 GiB quota** ([retention](#stopped-vm-retention-what-boatdevs-free-14-days-cost-on-own-hardware)) |
| `warmPool`, `ksmTeam` | `warmPool` on by default (NixOS module), `ksmTeam` off by default | both refused |
| Team quota (`services.boat.quotas`) | 0: no per-team cap, host admission only | 10 VMs, 32 vCPU, 64 GiB per team |
| Cost per VM-hour, AX102, 100% | $0.019 (22 VMs) | **$0.028** (15 VMs), 78% full to break even with boat.dev list ($0.036/h), 97% vs +25% credit bonus ($0.0288/h), never with operator time or 1+1 HA |

So against boat.dev, a public PandaOS service is priced by the conservative rows: **$0.028 per VM-hour on a single AX102 at 100% utilization versus boat.dev's $0.036 list ($0.0288 with +25% credit bonus), and more expensive than boat.dev below 78% utilization (97% vs bonus) or with operator time; a 4+1 spare-host projection requires 97% utilization versus list and never breaks even versus the bonus rate.** SMT off does not lower the VM count at `cpuOvercommit` 4, but it halves the CPU behind each busy vCPU. [Measured on an AX42](#measured-on-a-real-dedicated-server-hetzner-ax42) below: SMT off cost 6% of aggregate host index at 7 VMs. The runner measurements follow it.

### Untrusted disk: the old caps and what replaced them

Same-runner disk matrix ([42769967285](https://github.com/aicodeking/pandaos/actions/runs/42769967285), `boatd/disk-matrix.sh`, 4 vCPU / 8 GiB VM, `Unsafe` cache; full table in [PROTOCOL.md "Disk: measured"](../components/boat/PROTOCOL.md)):

| | VM, trusted defaults | untrusted, old (`Sync`, `io.max` 500 MB/s / 20k IOPS) | untrusted, now (`Sync`, iocost model + QoS, `io.max` 4,000 / 200k) |
|---|---|---|---|
| seq write, MB/s | 919-1006 | **564** (-41%) | 858 |
| seq read, MB/s | 2539-3497 | 3192 | 2142 |
| rand write, IOPS | 19.0-20.3k | 20.7k | 19.6k |
| rand read, IOPS | 49.6-56.7k | 79.8k | 82.9k |

- **The old caps were per VM and always on**: 41% of sequential write on a runner whose disk does 400-750 MB/s, and on an NVMe server (3-7 GB/s, 500k-1M IOPS) a ceiling 6-14x below the disk on bandwidth and 25-50x on IOPS, busy disk or idle.
- **What replaced them protects only modestly.** A victim VM next to one flooding 1 MiB writes kept 24-26% of its fsync'd 4k writes with iocost + QoS, against 11-17% unprotected, and 40-53% of its sequential write against 46-61% (same run). In `untrustedTenants`, `boatd` fails closed at startup unless cgroup v2 `+io` delegation, a resolvable whole block `stateDisk`, `ioCost` + `ioCostQoS`, and per-VM **`ioMax` (`io.max` hard BPS/IOPS ceiling)** are enforced; `ioCostModel` (`blk-iocost` proportional weight sharing) is enabled automatically once hardware model parameters are measured or configured (`boat-iocost-model.service` runs `boatd -measure-disk` once into `/var/lib/boat/iocost-model`, or set `services.boat.ioCostModel`) and logs a startup warning while sharing under `io.max` ceilings rather than blocking `boatd` startup if unset.
- **`Writeback` cannot protect fsync.** With honest flushes, a flooding neighbour stalled the victim's fsync loop to 0 IOPS with or without iocost (runs 42759114633, 42769967285): both VMs' I/O passes through the host's page cache and one writeback queue. Tenants who need durable writes next to hostile neighbours need their own disk; price that in.

## Measured on a real dedicated server (Hetzner AX42)

**2026-11-11. These are the first boatbench numbers not from a CI runner.** The repo owner lent a Hetzner AX42: AMD Ryzen 7 PRO 8700GE (Zen 4, 8 cores / 16 threads, a 35 W desktop APU), 64 GB, 2 × 512 GB NVMe in mdadm RAID1, stock Ubuntu 24.04 (kernel 6.8), `/dev/kvm`. boatd and boat-api were built on the box from main at `bcf0cda`, with this commit's `stack.sh` changes (`BENCH_STATE`; the stale-socket fix from the restart onwards). The slim guest image was built on the box with `components/boat/guest/build.sh`, using a temporary static `dockerd` that was removed afterwards. The harness ran on the same box, against boat-api on 127.0.0.1 (nothing was exposed publicly). **boat.dev was not measured** (no API key), so every boat.dev number here is still its own published figure.

The raw `results-*.json` files, logs and a README with hardware, OS, commit and commands are in [`bench/boatbench/results/ax42-ubuntu-2026-11-11/`](../bench/boatbench/results/ax42-ubuntu-2026-11-11/README.md). The per-preset table below is `boatbench.py presets */results-pandaos-*.json` there. The box was later reinstalled with PandaOS and measured again ([PandaOS install](#measured-on-the-pandaos-install-ax42-native-btrfs)).

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

- **Density levels follow the box.** boatd admits **7** `default` VMs on it at `memOvercommit` 1.0 (conservative / untrusted) and **11** at the default 1.5. Measured `MemTotal` is **63,381 MiB**, 875 MiB under this page's estimate of 64,256, which still gives 7 and 11 (11 needs 62,976). Untrusted stops at 7, its admission count. In that historical 2026-11-11 run, `memOvercommit` stayed at the bench's 8 for admission (workload c's 10 VMs); `presets.json` and `stack.sh` now enforce `memOvercommit: 1` for `untrusted` and are validated against `boatd`'s `loadConfig()`.
- **Untrusted ran with SMT off** at runtime (`/sys/devices/system/cpu/smt/control`, 8 CPUs online), as `nosmt=force` would give. SMT was switched back on afterwards.
- trusted-warm ran only b and c (the warm pool changes lifecycle, not steady-state speed) and trusted-writeback only d (the cache mode changes disk only). The other presets ran a–g.
- Two of trusted's three host / VM pairs came from a first session. The stack was restarted before pair 3 (same build, same config). A first pass of trusted-warm and trusted-writeback was thrown away because the previous preset's options had leaked into it (see `unset` above).
- `Unsafe` cache (†) on every column except trusted-writeback: the guest's flushes are dropped, as everywhere on this page.

### Per preset, AX42

Medians across independent VMs (`n = 3` VMs for (a), per-VM median of 3 intra-VM passes; `n = 3` for the rest, and `n = 8` for `trusted` and `untrusted` creates, which every repeat does).

> **Provenance & scope caveats for this table:** (1) All four columns in this `2026-11-11` Ubuntu loop-file run (as well as the later `0d160dd` `trusted` baselines: `0.15s` resume, `0.30s` fork, `0.32s` warm create) **predate `-ssh-policy-helper-file`** across presets; Round 23–24 batch SSH policy (`ssh-policy-batch`) and non-disruptive `rotate-token` are unit-tested to avoid `sshd` restarts and `nft` reloads on token refreshes, while live AX42 hardware timing for initial cold/restore SSH policy bootstrap and `rotate-token` awaits a fresh AX42 capture. (2) The `untrusted, SMT off` column predated `stack.sh`'s `memOvercommit: 1` enforcement (now fixed and validated offline against `boatd`'s `loadConfig()`), `jailGid` `io_uring` (`Async`) setup (falling back to `Sync`), `losetup --direct-io=on` loop pass-through, and online SSH quarantine; **rebooted PandaOS `untrusted` (`nosmt=force`, `memOvercommit: 1`, `blk-iocost`, `jailGid` `io_uring`, online SSH quarantine) remains pending hardware re-run**. (3) **Live `boat.dev` remains unmeasured** (`BOATDEV_API_KEY` not provided).

| Metric | trusted | trusted-warm | trusted-writeback | untrusted, SMT off |
|---|---|---|---|---|
| (a) Node loop, VM, index units | **21.15** [21.14–21.17] | | | **21.25** [21.17–21.45] |
| (a) Node loop, same box without a VM, index units | 23.06 | | | 23.04 |
| (a) VM / host | **0.917** | | | **0.922** |
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

| N | trusted, 16 threads: per-VM / host index units | efficiency | CPU per loop | untrusted, SMT off, 8 cores: per-VM / host index units | efficiency | CPU per loop |
|---|---|---|---|---|---|---|
| 1 | 20.79 / 20.79 | 1.00 | 1.49 | 20.96 / 20.96 | 1.00 | 1.48 |
| 2 | 18.94 / 37.89 | 0.911 | 1.50 | 19.38 / 38.76 | 0.925 | 1.49 |
| 4 | 15.48 / 62.01 | 0.744 | 1.48 | 15.98 / 63.96 | 0.763 | 1.38 |
| 7 | 11.51 / 80.47 | 0.553 | 1.47 | 10.82 / **75.91** | 0.516 | 1.08 |
| 11 | 7.92 / **87.06** | 0.381 | 1.27 | | | |

- **A real server scales far better than the runners did.** Host throughput still rises at 11 VMs on 16 threads (87.1 index units, 4.2× one VM), where every 4-vCPU runner had peaked by N = 4. The runners' curves were 2–4 CPU artefacts, and this page no longer uses them for any server projection.
- **SMT off costs little at 7 VMs:** 75.9 against 80.5 index units (−6%) on half the threads. The single-VM speed is the same (20.96 vs 20.79).
- **Per-VM speed still falls under load:** a packed VM runs at 52–55% of its solo speed at 7 VMs and 38% at 11. A VM-hour on a full host buys about half the work of one on an idle host.

### Cost per index unit, measured on the AX42

The first CPU-consistent PandaOS figure: price and speed come from the same machine. AX42 = **$119.04 / month** (`prices.json`: €99 + €49 setup / 12 + €3.20 backup, at 1.12 USD/EUR) = $0.1631 / h. With 2 h of operator time at $75 it is $269.04 / month = $0.3685 / h. Both tables are generated from the committed raw results:
- trusted: `bench/boatbench/results/ax42-ubuntu-2026-11-11/trusted/`, density run `699e4a`, started 2026-11-11T09:17:30Z;
- untrusted (SMT off): `.../untrusted/`, density run `207854`, started 2026-11-11T10:38:37Z.

Command: `boatbench.py report <dir>/results-*.json --server ax42`.

**Trusted, 16 threads**

USD per 1000 index units = $0.1631/h / 3.6 / measured host index units, at 100% busy, no retention; + operator time: $0.3685/h. boatd admits 11 `default` VMs on it in trusted mode (prices.json). boat.dev reference: $0.000356 (its price at its PUBLISHED solo speed; its speed under load is unmeasured).

| N VMs | host index units (measured) | USD per 1000 index units | + operator time | vs boat.dev reference |
|---|---|---|---|---|
| 1 | 20.79 | $0.002178 | $0.004924 | 6.11x |
| 2 | 37.89 | $0.001196 | $0.002702 | 3.35x |
| 4 | 62.01 | $0.000730 | $0.001651 | 2.05x |
| 7 | 80.47 | $0.000563 | $0.001272 | 1.58x |
| 11 | 87.06 | $0.000520 | $0.001176 | 1.46x |

**Untrusted, SMT off, 8 cores**

USD per 1000 index units = $0.1631/h / 3.6 / measured host index units, at 100% busy, no retention; + operator time: $0.3685/h. boatd admits 7 `default` VMs on it in untrusted mode (prices.json). boat.dev reference: $0.000356 (its price at its PUBLISHED solo speed; its speed under load is unmeasured).

| N VMs | host index units (measured) | USD per 1000 index units | + operator time | vs boat.dev reference |
|---|---|---|---|---|
| 1 | 20.96 | $0.002161 | $0.004885 | 6.06x |
| 2 | 38.76 | $0.001169 | $0.002641 | 3.28x |
| 4 | 63.96 | $0.000708 | $0.001601 | 1.99x |
| 7 | 75.91 | $0.000597 | $0.001349 | 1.67x |

- **Per index unit, a full AX42 costs 1.46–1.67× boat.dev's published price**, before operator time (3.3–3.8× with it). To match boat.dev, the box would need **127 host index units** (287 with operator time). It reached 87.
- **Per VM-hour, the AX42 is cheaper** ($0.023 untrusted at 7 VMs, 100% utilization, against $0.036). The two statements do not conflict. A packed AX42 VM runs at 10.8–11.5 index units, and boat.dev's figure is a solo VM at 28.06. boat.dev's speed under load has never been measured, so this comparison flatters boat.dev by an unknown amount, just as "100% busy" flatters the AX42.

### Against boat.dev's published figures (different CPU)

boat.dev's 28.06 index units (StarSling run 37073680030, 2026-10-03; it replaces the 27.67 of the 2026-08-24 AnicetNgrt run, the same 9950X, within both runs' intervals) is a 4 vCPU guest on an **AMD Ryzen 9 9950X (Zen 5**, 16 cores, 170 W; per boat.dev's page the fleet also has Ryzen 7 9700X hosts that run did not measure). The AX42 is a **Ryzen 7 PRO 8700GE (Zen 4**, 8 cores, 35 W). The CPUs differ by a generation and in power budget, so a gap in single-VM speed is mostly hardware.

| | boat.dev PUBLISHED (9950X guest) | PandaOS MEASURED (AX42 guest) |
|---|---|---|
| Node loop, one 4 vCPU VM, index units | 28.06 | 21.15 (trusted), 21.25 (untrusted) |
| Same CPU without a VM, index units | not published (OpenBenchmarking's 9950X average: 28.8) | 23.06 |
| VM / host | about 0.97 *if* their host scores the public average (inference) | **0.917–0.922** (measured) |
| PTS rand read 4k, IOPS | 433,500 | 155,795–202,589 |
| PTS rand write 4k, IOPS | 412,500 | 159,704–171,442 † |
| PTS seq write 1M, MiB/s | 5,838 | 2,476–2,740 †; 4,056 with `Writeback` |

- **CPU:** the AX42 VM is 24–25% slower than boat.dev's published VM, and the AX42 host is 20% below the 9950X's public average. Almost all of the gap is the CPU. *Projection, not a measurement:* a PandaOS VM on a 9950X would score about 28.8 × 0.92 ≈ 26.4 index units.
- **Disk: boat.dev's published numbers are 2–3× higher.** Part of that is our storage stack (below). Part may be boat.dev's, whose fio target and cache mode are not published.

### What the disk numbers are, and how a real btrfs partition differs

boat's store here is **a sparse btrfs loop file on the box's ext4 root, on md RAID1** (what `stack.sh` does, so the disks were never touched). A real PandaOS install puts btrfs directly on the disks. Compared with that:
- **Every write passes two filesystems and the loop driver.** btrfs writes into a file that ext4 journals, and the page cache can hold the same data twice. That inflates cached reads and buffered writes and costs CPU on direct I/O.
- **An fsync goes btrfs → loop → ext4 fsync of the image → md → a flush on both NVMe drives.** The host managed **82 fsync'd 4k writes per second**, and a `Writeback` VM 38. Those are the honest durable-write rates of *this* stack. They say little about the drives, which are not enterprise drives with power-loss protection (model not recorded), and they do not measure a native btrfs.
- RAID1 doubles every write. A native btrfs `raid1` profile does the same, so that part carries over.
- **Native btrfs was measured later** on the same box after the PandaOS install: [below](#measured-on-the-pandaos-install-ax42-native-btrfs). It is not uniformly faster.

### What is measured now, and what is still a projection

- **Measured on real hardware (this AX42, on Ubuntu and on the PandaOS install):** single-VM speed and the VM / host ratio (0.92); lifecycle latencies; density under CPU load up to 11 VMs (8 on the install, its quota), with a host-native baseline on the install; aggregate host index; cost per index unit at the AX42's list price; `MemTotal` and admission counts on a 64 GB box; the cost of SMT off (−6% at 7 VMs); the CPU governor (no effect); a stopped VM's actual disk use; disk on a loop file and on native btrfs, with a host-native fio baseline on the install.
- **Still projections or unmeasured:** everything about the AX102 and the other servers (no run there; the AX102 is also Zen 4, so the AX42 curve is the best proxy, see [below](#cost-per-index-unit-from-aggregate-host-index)); PandaOS on a 9950X; boat.dev under load, boat.dev's lifecycle times, and boat.dev's VM / host ratio (no key); density under *memory* load (the Node loop uses little RAM); utilization below 100%; thin provisioning (a model).

## Measured on the PandaOS install (AX42, native btrfs)

**2026-11-12. These numbers come from the product as shipped:** PandaOS 26.05 installed on the same AX42, with boat's store as native btrfs (`compress=zstd:1`) directly on one NVMe, boatd as configured by the install (**trusted mode**: `untrustedTenants` was off, so SMT on, no iocost, overcommit 1.5; the shipped untrusted default has not been benchmarked here), and the CPU governor as the box runs it (`powersave`, EPP `balance_performance`). The harness ran on the box against boat-api on 127.0.0.1. The raw results, exact scripts, store paths and run IDs are in [`bench/boatbench/results/ax42-pandaos-2026-11-12/`](../bench/boatbench/results/ax42-pandaos-2026-11-12/README.md). **boat.dev was not measured.**

- **Admission limit.** boatd's defaults admit 11 `default` VMs by RAM. The owner team's quota (32 vCPU, 64 GiB) admits **8**, so workload c created 8 VMs at once, not 10, and density stopped at 8. The API refused the 9th to 11th VMs with HTTP 429.
- **Shared box.** Another worker's 16-vCPU VM and a `nixos-rebuild switch` overlapped the first lifecycle and density runs (05:26–05:45Z). Those two runs are kept under `contaminated/` and are not used here. Lifecycle and density were rerun from 06:28Z on a quiet box: run `595790` (b, c, e, f) and run `849e85` (g).
- `Unsafe` drive cache (†), as on every column of the Ubuntu run except trusted-writeback.

### PandaOS install vs the Ubuntu loop-file run, AX42

Medians across independent VMs (`n = 3` VMs for (a), per-VM median of 3 intra-VM passes: runs dc7c96, cb2137, ca2db2, with host runs 25e7d9, f6a03c, 1056a7; d `n = 3`, everything else `n = 3` repeats, run 595790). Ubuntu: the trusted column [above](#per-preset-ax42). The two columns differ in more than the filesystem: kernel (6.18 vs 6.8), boatd build (the install vs `bcf0cda`), guest image (the install's `ubuntu-24.04` vs a locally built `slim`), RAID (one NVMe vs mdadm RAID1) and overcommit (defaults vs the bench's 8). A difference is therefore not proof of a btrfs effect. **Caveats:** both columns ran in **`trusted` mode** (`Unsafe` cache) and **predate `-ssh-policy-helper-file`** across presets (Round 23–24 batch SSH policy + non-disruptive `rotate-token` are unit-tested with live AX42 timing awaiting the next capture); `stack.sh` `untrusted` config (including `memOvercommit: 1`) is fixed and validated offline against `boatd`'s `loadConfig()`, while **rebooted PandaOS `untrusted`** (`nosmt=force`, `memOvercommit: 1`, `blk-iocost`, `jailGid` `io_uring`, online SSH quarantine) remains pending hardware re-run; and **live `boat.dev` remains unmeasured** (`BOATDEV_API_KEY` not provided).

| Metric | PandaOS install, native btrfs | Ubuntu, btrfs loop file on ext4 / RAID1 |
|---|---|---|
| (a) Node loop, VM, index units | **21.25** [21.20–21.29] | 21.15 |
| (a) Node loop, same box without a VM, index units | 23.08 | 23.06 |
| (a) VM / host | **0.921** | 0.917 |
| (b) API round trip p50, ms | 0.311 | 0.283 |
| (b) exec `true` p50 / p95, ms | 2.51 / 4.99 | 2.29 / 3.06 |
| (b) create → exec-ready, s | 1.58 | 1.40 |
| (b) fork → exec-ready, s | 0.824 | 1.24 |
| (b) stop, s | 0.728 | 0.617 |
| (b) resume → exec-ready, s | **0.198** | 0.256 |
| (b) snapshot until usable, s | 2.67 | 1.79 |
| (b) new-from-snapshot → exec-ready, s | 0.512 | 0.220 |
| (c) concurrent creates, s | 3.54 (8 VMs, quota) | 2.99 (10 VMs) |
| (d) seq write 1M, MB/s † | 1,514 | 1,863 |
| (d) seq read 1M, MB/s | 7,546 | 5,903 |
| (d) rand write 4k, IOPS † | 227,075 | 147,933 |
| (d) rand read 4k, IOPS | 218,156 [138,340–219,680] | 82,523 |
| (d) PTS rand read 4k, IOPS | **56,693** [IQR 56,424–139,164; samples 56,154 / 56,693 / 221,635] | 155,795 |
| (d) PTS rand write 4k, IOPS † | 260,411 | 171,442 |
| (d) PTS seq write 1M, MiB/s † | 1,651 | 2,740 |
| (d) 4k rand write, fsync after each, IOPS † | 26,008 (flushes dropped) | 23,942 (flushes dropped) |
| (d) seq write 1M, fsync at the end, MB/s † | 1,589 | 2,640 |
| (e) Go tarball download, Mbit/s | 818.8 | 812.4 |
| (f) cold `npm ci`, s | 2.46 | 2.56 |

- **CPU-bound speed is the same as on Ubuntu:** 21.25 vs 21.15 index units in a VM, 0.921 vs 0.917 of the host.
- **Disk is mixed, not better across the board.** Random 4k writes and reads (our job, iodepth 32) are 1.5–2.6× the loop-file numbers. Sequential writes and the PTS random-read job are lower: two of three PTS random-read repeats gave about 56k IOPS (56,154 and 56,693) and one 221,635. The bracket in the table is the interpolated IQR, not a sample: 139,164 is the midpoint of 56,693 and 221,635. The split is examined in [Host disk baseline](#host-disk-baseline-and-the-56k--222k-random-read-split-ax42-pandaos-install). All of it stays below boat.dev's published PTS numbers (433k / 413k IOPS, 5,838 MiB/s). A durable-write (`Writeback`) run was made later: [84 fsyncs/s](#re-measured-after-the-fixes-ax42-main-0d160dd-2026-11-13).
- **Lifecycle:** resume (0.20 s) and fork (0.82 s) are faster than on the Ubuntu box; create, snapshot and new-from-snapshot are slower ([why](#why-create-snapshot-and-new-from-snapshot-are-slower-on-the-install)). No boat.dev lifecycle number exists to compare with.

### Why create, snapshot and new-from-snapshot are slower on the install

Raw data: the committed results and boatd log tails of both runs, the install's boatd journal for the 06:28Z reruns (read on the box on 2026-11-12), and a diagnostic run at 13:41–14:16Z ([`ax42-diag-2026-11-12`](../bench/boatbench/results/ax42-diag-2026-11-12/README.md): `out-life/`, `lifecycle-b-performance/`).

**Measured.**
- **Same guest.** Both runs' VMs report guest kernel 6.1.191, Ubuntu 24.04.5 and the same Node (probe fields in the raw JSON), and both ran b, c, e, f in one repeat (`parts` = `bcef`). The guest image file differs (the install's `ubuntu-24.04` vs a locally built `slim`), not its kernel.
- **Create (+0.18 s) is boatd's `materialize` step.** The install's journal splits a create into alloc 21–25 ms, materialize **149–171 ms**, Firecracker boot 87–125 ms and guest agent init 1.18–1.45 s. The Ubuntu log tails show materialize **0–1 ms** for cold creates, single and concurrent, boot 105–125 ms and agent init 1.20–1.25 s. Only materialize moved, by about the whole gap.
- **New-from-snapshot (+0.29 s) has the same step:** materialize took 225–251 ms on the install (209–212 ms in the diagnostic run), plus 85–119 ms of guest identity setup. The Ubuntu tails have no new-from-snapshot line to compare with.
- **It is the image reflink** (corrected 2026-11-12 evening; this line used to say the opposite). The install's `ubuntu-24.04` rootfs was NOCOW, uncompressed, **22,567 extents** (archived since: boatd's journal line `extents 22567 -> 4` when the fixed boatd converted it, in [`ax42-r11-2026-11-13/run.log`](../bench/boatbench/results/ax42-r11-2026-11-13/run.log)). *Observed, not archived:* FICLONE of it, timed directly (3 repeats in the team subvolume, 3 at the top level), took 151–170 ms, about all of materialize; the kernel copy took 1 ms; an earlier `cp --reflink=always` timing of 10 ms was not reproduced; a `--sparse=never` (dense) copy had 4 extents (3.0 GiB allocated instead of 2.8) and cloned in 0.1 ms, and `--sparse=auto` gave 15. The extents are holes at zero blocks, from the image's sparse writes (zstd -d, `cp --sparse=always` in boatd's NOCOW conversion). See [Fixes](#fixes-after-the-install-run) for what was measured after.
- **Snapshot memory goes through compression on the install.** The store is mounted `compress=zstd:1`. A snapshot's `mem` file is CoW and zstd-compressed (401 of 440 MiB in zstd extents, `compsize`), and so is the guest kernel copy. On the same store, the host wrote a CoW (compressed) file at **746 MiB/s** and a NOCOW file at **4,842 MiB/s** (PTS seq write job). The install's snapshots wrote 2.50–2.62 GiB of guest memory in 2.23–2.29 s (about 1.1 GiB/s), plus a 0.10–0.65 s sync and a 0.15 s disk freeze.
- **exec p95 depends on the run's conditions.** Under the `performance` governor (as found at 14:16Z), b alone gave exec p95 **3.81–4.53 ms** (median 3.87) and p50 2.17 ms, against 4.99 ms (p50 2.51) in the `powersave` / `balance_performance` run and 3.06 ms (p50 2.29) on Ubuntu, whose governor was not recorded. This was b only, not bcef, so the guests were less loaded.

**Inferred, not measured.**
- Snapshot (+0.9 s): the Ubuntu store was a loop-mounted btrfs without a `compress` option (`stack.sh`), so its memory files were not compressed. zstd on the write path is the likely cost. The Ubuntu run logged no full-snapshot write time to confirm it, and its three snapshot samples spread 1.23–2.55 s.
- New-from-snapshot: restoring from a compressed `mem` file costs decompression on every page fault, on top of the slower materialize. Not separated.
- What inside materialize costs 150–250 ms: measured since (above), the image reflink.
- exec p95: part of the gap follows the governor (3.87 ms under `performance`), and the host kernel (6.18 vs 6.8) and guest image may account for the rest. Neither was A/B tested.
- **RAID1 vs one disk does not explain any of these.** Ubuntu's RAID1 doubled writes and was the *faster* side for create and snapshot. The install's single NVMe sits under the compression and quota differences above.

### Fixes after the install run

2026-11-12, after the run above, in boatd and the guest image (not in the build the run used). The "before" figures were observed on this AX42 but **their raw output was not archived**, except where marked. r12 re-ran what can be re-run without reconfiguring the server ([`ax42-r12-2026-11-13/before/`](../bench/boatbench/results/ax42-r12-2026-11-13/README.md)): the interrupt and `rq_affinity` figures are archived now ([below](#host-disk-baseline-and-the-56k--222k-random-read-split-ax42-pandaos-install)); the image layout could not be: the 22,567-extent image was replaced, and a `cp --sparse=always` copy of today's image has 5 extents and clones in 2.8–3.2 ms, so the FICLONE "before" stays observed. The others need the old boatd, image or kernel command line. The "after" figures were measured on 2026-11-13 on the same box at main `0d160dd` with the guest image rebuilt at that commit; raw logs in [`ax42-r11-2026-11-13/`](../bench/boatbench/results/ax42-r11-2026-11-13/README.md) (`fixes/log.txt`, `run.log`).

| What | Before (2026-11-12) | After (measured 2026-11-13) |
|---|---|---|
| Image NOCOW conversion writes a dense copy (no holes) instead of `cp --sparse=always`; a fragmented image is converted again once | 22,567 extents (**archived**: boatd's `extents 22567 -> 4` journal line when the fix converted it); FICLONE 151–170 ms, materialize 149–236 ms (observed, not archived) | `ubuntu-24.04` 5 extents (`ubuntu-full` 1); `cp --reflink=always` 3.2–4.1 ms wall time including the process start, 3 × each image; a fresh import of 18,932 extents converted to 5 in 3.5 s at boatd's start. Cold create → exec-ready 0.87–0.90 s against 1.56–1.58 s ([below](#re-measured-after-the-fixes-ax42-main-0d160dd-2026-11-13)) |
| VM and snapshot dirs get `FS_NOCOMP_FL` (inherited by new files), on a `compress` store | an 8 GiB VM's `mem` after a snapshot: 16,814 extents; snapshot write 3,189 ms (observed, not archived; one VM's dir flagged by hand gave 109 extents and 2,961 ms) | same dirty memory (1 GiB random + `/usr` page cache): `mem` 98 extents, attribute `m` (no compression); 1,961 MiB written in 1,580 ms |
| `ldconfig.service` and the journal catalog no longer run on each VM's first boot (`/etc/.updated`, `/var/.updated` in the image) | 114–127 ms on the agent's critical chain; agent started at 0.90–0.92 s (`systemd-analyze critical-chain`; observed, not archived) | `ldconfig.service` ConditionResult `no` (skipped); `boat-agent.service` @332 ms in the critical chain of the first attempt's VM (`aborted/fixes-unconverted-image/log.txt`, same image) |
| Kernel cmdline `quiet` | kernel 420 ms, 96 ms of it the console replaying the log (dmesg gaps; observed, not archived) | `quiet` on the cmdline; `Run /sbin/init` at 0.247 s and 0.251 s in the guest's dmesg (two VMs) |

The disk-interrupt split above is not fixed (see the last bullet).

### Re-measured after the fixes (AX42, main 0d160dd, 2026-11-13)

The install updated in place to main `0d160dd` (no reboot, same kernel), **still trusted mode**: `services.boat.untrustedTenants = false` is now set explicitly, so SMT stays on. Guest image rebuilt at `0d160dd`. Governor `performance` as found. Raw results, scripts and every step: [`ax42-r11-2026-11-13/`](../bench/boatbench/results/ax42-r11-2026-11-13/README.md); reports from `results/regen.sh`. Medians [min–max], 3 repeats each, all measured.

> **Important caveats alongside these AX42 lifecycle baselines:**
> - **Pre-`-ssh-policy-helper-file` `trusted` baselines:** the `0d160dd` lifecycle medians below (`0.15s` resume, `0.30s` fork, `0.32s` warm create, `0.89s` cold create) **predate enabling `-ssh-policy-helper-file`** across presets. Round 23–24 batch SSH policy (`POST /v1/vms/ssh-policy-batch`) and the non-disruptive `rotate-token` guest helper subcommand are unit-tested to avoid `sshd` restarts and `nft -f -` reloads on steady-state token refreshes, while live AX42 timing for both initial per-VM cold/restore SSH policy bootstrap and `rotate-token` awaits a fresh AX42 capture.
> - **Rebooted PandaOS `untrusted` pending hardware re-run:** production `untrusted` (`nosmt=force`, `memOvercommit: 1`, `blk-iocost`, `jailGid` `io_uring`, online SSH quarantine, `warmPool` off) was **not** benchmarked here because SMT off requires an owner-approved reboot (`stack.sh`'s `untrusted` config is fixed and validated offline against `boatd`'s `loadConfig()`).
> - **Live `boat.dev` unmeasured:** `BOATDEV_API_KEY` was not provided, so there are no live `boat.dev` lifecycle or disk measurements.

With trusted tenants the module now turns boatd's warm pool on, which the install run did not have, so creates are restores of a booted template. The like-for-like column is the one with the warm pool off.

| (b) metric | install, `powersave` (595790) | install, `performance` (diag 141558Z) | 0d160dd, `performance`, warm pool off | 0d160dd, warm pool on (as installed) |
|---|---|---|---|---|
| create → exec-ready, s | 1.56 [1.56–1.60] | 1.57 [1.56–1.58] | **0.89 [0.87–0.90]** | 0.32 [0.31–0.32] |
| fork → exec-ready, s | 0.82 [0.79–0.85] | 0.57 [0.56–0.57] | **0.30 [0.29–0.31]** | 0.36 [0.35–0.37] |
| new-from-snapshot → exec-ready, s | 0.51 [0.48–0.54] | 0.40 [0.36–0.42] | **0.19 [0.19–0.20]** | 0.22 [0.22–0.23] |
| `fresh_boot` penalty (derived, not measured): cold boot instead of snapshot resume, s | +1.05 (1.56 − 0.51) | +1.17 (1.57 − 0.40) | **+0.70 (0.89 − 0.19)** | n/a (a fresh boot is a cold boot, not a warm-pool restore) |
| snapshot until usable, s | 2.67 [2.55–3.07] | 0.66 [0.63–0.67] | **0.48 [0.48–0.55]** | 0.38 [0.38–0.40] |
| stop, s | 0.73 [0.73–0.74] | 0.37 [0.29–0.43] | **0.33 [0.30–0.35]** | 0.40 [0.39–0.40] |
| resume → exec-ready, s | 0.20 [0.19–0.24] | 0.17 [0.16–0.17] | **0.15 [0.15–0.16]** | 0.16 [0.16–0.17] |
| exec `true` p50, ms | 2.51 [2.42–2.55] | 2.17 [2.12–2.24] | **2.06 [2.04–2.08]** | 2.24 [2.21–2.33] |
| exec `true` p95, ms | 4.99 [4.57–5.21] | 3.87 [3.81–4.53] | **3.21 [2.48–3.41]** | 4.37 [2.86–4.48] |

- **`fresh_boot` costs about 0.7 s (like-for-like column).** boatd's `fresh_boot` makes a start, create-from-snapshot or fork ignore the memory snapshot and boot the disk cold, so it costs the cold-create time (0.89 s) instead of the from-snapshot time (0.19 s): about **+0.70 s** per VM, and the source's running processes are not carried over. The row above is derived from the two measured rows; no `fresh_boot` run was timed on its own. It applies where the memory snapshot holds secrets the new run must not have: a `no_env` start, fork or create (and a create from a snapshot) whose secret selection is **narrower than the source's** (a `no_env` run after a run that had secrets; a `no_env` fork of a normal run; a create from a snapshot with fewer secrets). A `no_env` start after a `no_env` run, a normal start and a fork of a `no_env` run resume as usual and pay nothing. A fresh boot that cannot apply its env fails closed (the VM is force-stopped), so the cost is paid once per attempt.
- **Cold create −0.67 s (1.57 → 0.89 s), same governor.** Inferred split: materialize was 149–236 ms of it (now 1 ms in boatd's journal), the rest the guest boot (`quiet`, no `ldconfig`). The per-step journal of the cold runs was not captured, so the split is not measured.
- **Snapshot, fork and new-from-snapshot** are 0.18–0.27 s faster against the same governor; the uncompressed memory files and the dense image reflink are the likely causes (not separated).

| (d) metric | install, `Unsafe`, `powersave` (dc7c96, cb2137, ca2db2) | 0d160dd, `Unsafe`, `performance`, warm pool on | 0d160dd, **`Writeback`**, `performance`, warm pool off |
|---|---|---|---|
| seq write 1M, MB/s | 1,514 [1,460–1,523] | 1,634 [1,579–1,661] | 1,652 [1,599–1,691] |
| seq write 1M, fsync at the end, MB/s | 1,589 [1,574–1,848] | 1,866 [1,682–1,949] | 1,612 [1,552–1,752] |
| rand write 4k, IOPS | 227,075 [225,788–229,193] | 226,283 [225,762–227,053] | 211,011 [209,273–213,512] |
| rand read 4k, IOPS | 218,156 [58,524–221,203] | 216,751 [58,261–216,905] | 213,865 [58,976–216,583] |
| **4k rand write, fsync after each, IOPS** | 26,008 [25,686–26,040] (flushes dropped) | 25,630 [25,371–25,902] (flushes dropped) | **84 [83–84]** (honest) |
| PTS seq write 1M, MiB/s | 1,651 [1,605–1,691] | 1,777 [1,725–1,854] | 1,723 [1,722–1,791] |
| PTS rand read 4k, IOPS | 56,693 [56,154–221,635] | 216,255 [57,951–216,285] | 215,627 [212,359–216,170] |
| PTS rand write 4k, IOPS | 260,411 [260,009–260,659] | 260,018 [256,978–261,468] | 247,347 [244,571–247,476] |
| seq read 1M, MB/s | 7,546 [6,569–7,619] | 12,592 [9,429–13,478] | 12,976 [7,753–13,354] |

- **Durable writes on native btrfs: 84 fsyncs/s** in a `Writeback` VM. The guest's flushes reach the drive: nvme0n1 counted 3,361 flushes during a 20 s fsync-per-write job, against 1 under `Unsafe` (`flush/log.txt`). This NVMe (Samsung PM9A1 class, volatile write cache, no power-loss protection) is the limit; drives with power-loss protection would raise it. The Ubuntu loop-file stack gave 38 ([above](#per-preset-ax42)). Everything that does not flush is within about 7% of `Unsafe`.
- **A boatd bug this run found:** a warm template kept the drive it was booted with, so turning on `diskCache: Writeback` on a host with a warm pool went on creating `Unsafe` VMs (26.5k fsync IOPS and 1 flush in the first attempt, kept in `aborted/warm-template/`). Fixed after `0d160dd` (the template's stamp now includes the drive and boot config); the numbers above ran with the warm pool off.
- **The disk is otherwise unchanged by the fixes.** The PTS random-read median moved from 56.7k to 216k only because 2 of 3 repeats landed off the interrupt vCPU this time (1 of 3 before); the bimodal split is the same.

**The sequential-write gap, inside Firecracker's limits** (20 s PTS-style 1M writes, iodepth 64, 3 × each, MiB/s; `seqw/`, `pre-update/knobs-pre/`). Nothing measurably helps, so nothing shipped:

| Variant | MiB/s |
|---|---|
| as booted (scheduler `none`, `nr_requests` 128, `max_sectors_kb` 1280, read-ahead 128 KiB) | 1,689 / 1,718 / 1,710 |
| scheduler `none` set again / `mq-deadline` | 1,722 / 1,724 / 1,703 · 1,681 / 1,673 / 1,619 |
| `max_sectors_kb` = hardware maximum / `nr_requests` 1024 | 1,713 / 1,611 / 1,696 · 1,784 / 1,697 / 1,654 |
| host: `Unsafe` + Async (io_uring) / `Writeback` + Async / `Unsafe` + Sync, cold-booted VMs | 1,792 / 1,778 / 1,698 · 1,629 / 1,648 / 1,676 · 1,681 / 1,722 / 1,742 |

The guest's queue reports **`max_segments` 1**: Firecracker's virtio-blk does not offer `VIRTIO_BLK_F_SEG_MAX`, so the guest sends one segment per request. **Profiled in r12** ([`ax42-r12-2026-11-13/seqw/`](../bench/boatbench/results/ax42-r12-2026-11-13/README.md)): a 1 MiB O_DIRECT write leaves the guest as requests of 17–27 KiB on average, 62–98k of them per second with the guest's queue full (121 of 128), and Firecracker's one VMM thread runs at 88–95% of a CPU serving them. That thread is the ceiling. The host's NVMe saw 1.1–3.3k requests/s of ~127 KiB at a queue depth below 7 (`Unsafe` writes land in the page cache first). With fio's buffers in a 2 MiB huge page (physically contiguous) the guest sends 1 MiB requests, the VMM thread drops to 2%, throughput rises 15–17% (1,904–1,914 MiB/s), and the single io_uring worker that copies into the host page cache becomes the next ceiling (99.5%). `max_sectors_kb` and `mq-deadline` changed nothing again, and huge-page buffers are a property of the guest program, not a setting boatd or the image can give, so **nothing shipped**; it needs Firecracker to offer `SEG_MAX` or more VMM I/O threads. Read-ahead does not apply to writes.

**The disk interrupt.** Steering virtio-blk's interrupt off vCPU 0 (`smp_affinity_list` 1-3) does not help the common case, unpinned fio: 4 of 16 slow (~59k) random reads with it moved against 5 of 16 as booted, over two sessions (`pre-update/irq-pre/`, `irq/`). No boot step was added.

### Host disk baseline and the 56k / 222k random-read split (AX42, PandaOS install)

[`ax42-diag-2026-11-12`](../bench/boatbench/results/ax42-diag-2026-11-12/README.md): fio on the host in a scratch directory on the same btrfs as boatd's store (deleted afterwards), and the same jobs inside `bb-fio-*` VMs (deleted). The governor was `performance` (as found, not changed); the benchmark run used `powersave`. All numbers are measured.

| PTS fio job (iodepth 64, 1 GiB, 60 s) | Host, NOCOW file | Host, CoW file (zstd) | VM, `Unsafe` + io_uring | boat.dev published |
|---|---|---|---|---|
| seq write 1M, MiB/s | **4,842** | 746 | 1,722 (1,693–1,771; install run 1,605–1,691) | 5,838 |
| rand read 4k, IOPS | **198,036** (197,548–198,247) | 174,485 | **~219k** or **~59k** (bimodal, below); install run 56,154 / 56,693 / 221,635 | 433,500 |
| rand read 4k, iodepth 1, IOPS (20 s) | 17,808 | | 33,021 | |
| rand read 4k, io_uring, iodepth 64, IOPS (20 s) | 222,505 | | 245,245 | |

- **The drive and filesystem account for part of the gap to boat.dev's numbers.** On the host, with no VM, this NVMe gives 198k random-read IOPS and 4,842 MiB/s sequential write on a NOCOW file: 2.2× and 1.2× below boat.dev's published figures. A CoW file, which the store's `compress=zstd:1` compresses, writes at only 746 MiB/s. VM disks are NOCOW (boatd's `diskNoCow`), so they avoid that.
- **The VM's sequential write is the VM path.** 1,722 MiB/s is 0.36× the host's NOCOW write on the same filesystem, so about 2.8–2.9× of the 3.5× sequential-write gap comes from Firecracker's virtio-blk: one VMM thread at 88–95% CPU on ~25 KiB requests (measured in r12, [above](#re-measured-after-the-fixes-ax42-main-0d160dd-2026-11-13); the host NOCOW write re-measured at 4,815–4,853 MiB/s, the guest 1,566–1,667, 0.34×). The drive accounts for 1.2×.
- **The VM's fast random reads do not touch the disk.** With `Unsafe` caching, the guest's O_DIRECT reads become buffered reads of the host page cache: nvme0n1 served **0** read I/Os during the guest's 60 s reads (`out-wb2`). That is why the VM's 219k and its iodepth-1 33k beat the host's direct NVMe numbers.
- **The 56k / 222k split is the guest's fio sharing a vCPU with the disk interrupt.** Firecracker's virtio-blk has one queue, whose interrupt went to one guest vCPU (vCPU 3, `/proc/interrupts`). With fio pinned to that vCPU, three of three reads gave **59.5–59.9k** IOPS; pinned to another vCPU, three of three gave **219.1–219.8k** (`out-irq`). Unpinned, the guest scheduler picks the vCPU: 5 of 16 reads right after a write were slow. In slow runs only one guest vCPU thread was busy and the VMM thread used 0.32 CPU; in fast runs two were busy and the VMM used 0.81 (`out-wb3`).
- **Ruled out:** host page-cache misses (0 NVMe reads in slow runs too), cgroup reclaim (the VM's cgroup stayed at 2.6–5.7 GiB of its 8.25 GiB `memory.high`), btrfs CoW (the VM disk is NOCOW, and the host's CoW file is slower but not bimodal), first touch of sparse blocks (the file is written first and is fully resident), the io engine (io_uring and libaio both reach ~220–245k in fast mode), and queue depth (the same iodepth 64 gives both modes). A host `sync` before the read gave 6 of 6 fast reads against 11 of 16 without it, but pinning shows the cause directly. With 16 and 6 trials, that difference is not significant (one-sided Fisher p ≈ 0.17).
- **So of the 7.6× random-read loss in the verdict** (56,693 against 433,500), about 2× is what this host's storage reaches on this job (2.2× for the host's direct NVMe reads, 2.0× for the VM's page-cache reads), and about 3.9× (219k / 56.7k) is the guest putting fio on the interrupt vCPU, a scheduling accident the benchmark did not control. With fio on another vCPU, the VM reads at about 219k, half of boat.dev's published figure. A guest with more virtio-blk queues would remove the split. *Observed on 2026-11-12, archived since in r12* ([`ax42-r12-2026-11-13/before/`](../bench/boatbench/results/ax42-r12-2026-11-13/README.md), same box, fio pinned per vCPU, 8 s each, 3 rounds; 58.4k / 58.8k / 57.7k on the new interrupt vCPU, 213–218k elsewhere; `rq_affinity` 2: 58.7k and 171.8–174.7k): moving the interrupt to another vCPU moved the slow vCPU with it (58.9k on the new one, 214–218k elsewhere), and `rq_affinity` 2 made it worse (60k on the interrupt vCPU, 172–175k elsewhere, against 215–220k at 1). One edge-triggered IO-APIC interrupt is delivered to one vCPU at a time, so irqbalance could only move it, not spread it. *Measured and archived since* ([re-measured](#re-measured-after-the-fixes-ax42-main-0d160dd-2026-11-13)): with fio unpinned, steering the interrupt off vCPU 0 does not help the common case. Not fixed: the guest image keeps `rq_affinity` 1 and the interrupt where the kernel puts it. **boatbench now runs the 4k rand read three times** (unpinned, pinned to the interrupt vCPU, pinned off it), so the number no longer depends on where fio lands: on this AX42 (r12, [`report-disk-pinned.md`](../bench/boatbench/results/ax42-r12-2026-11-13/report-disk-pinned.md), 3 repeats) 58,891–59,341 on the interrupt vCPU, 216,919–217,216 off it, 215,100–219,617 unpinned, while the unpinned PTS job gave 59,143 / 59,484 / 218,278. Reports from before this change hold only the unpinned row and are labelled so. boat.dev's fio target and cache mode are not published, so the remaining 2× is not attributed.

### Density at the admission limit, and the host-native baseline

Run `849e85` (VMs, 2 repeats) and run `28a141` (the same N Node loops directly on the host, no VMs, 2 repeats). Generated by `boatbench.py report powersave/results-*.json --server ax42`.

**VMs**

| N | per-VM index units | host index units (sum) | efficiency | CPU per loop | loops per host CPU | host CPUs |
|---|---|---|---|---|---|---|
| 1 | 20.97 [20.97-20.98] n=2 | 20.97 [20.97-20.98] n=2 | 1.00 [1.00-1.00] n=2 | 1.46 [1.46-1.47] n=2 | 0.092 [0.091-0.092] n=2 | 16.00 |
| 2 | 18.97 [18.97-18.98] n=2 | 37.95 [37.93-37.96] n=2 | 0.905 [0.904-0.905] n=2 | 1.49 [1.49-1.49] n=2 | 0.183 [0.183-0.183] n=2 | 16.00 |
| 4 | 15.60 [15.53-15.67] n=2 | 62.42 [62.19-62.66] n=2 | 0.744 [0.740-0.747] n=2 | 1.48 [1.48-1.49] n=2 | 0.366 [0.366-0.366] n=2 | 16.00 |
| 7 | 11.60 [11.59-11.61] n=2 | 81.14 [81.14-81.15] n=2 | 0.553 [0.552-0.554] n=2 | 1.46 [1.46-1.46] n=2 | 0.641 [0.640-0.641] n=2 | 16.00 |
| 8 | 10.41 [10.39-10.43] n=2 | 83.45 [83.29-83.61] n=2 | 0.496 [0.495-0.498] n=2 | 1.44 [1.43-1.44] n=2 | 0.732 [0.732-0.733] n=2 | 16.00 |

**Host-native baseline vs VMs**

| N | host loops: per-loop / total index units | efficiency | VMs: per-VM / total index units | efficiency | VM / host at this N |
|---|---|---|---|---|---|
| 1 | 21.96 / 21.96 | 1.00 | 20.97 / 20.97 | 1.00 | 0.955 |
| 2 | 21.11 / 42.23 | 0.962 | 18.97 / 37.95 | 0.905 | 0.899 |
| 4 | 17.40 / 69.53 | 0.793 | 15.60 / 62.42 | 0.744 | 0.896 |
| 7 | 12.99 / 90.95 | 0.592 | 11.60 / 81.14 | 0.553 | 0.893 |
| 8 | 11.72 / 93.66 | 0.534 | 10.41 / 83.45 | 0.496 | 0.888 |
| 11 | 9.02 / 99.10 | 0.411 | - / - | - | - |

- **Most of the per-VM slowdown under load is CPU saturation, not virtualization.** Eight bare Node loops on the host fall to 0.53 of their solo speed. Eight VMs fall to 0.50. At every N from 2 to 8, the VMs deliver 0.89–0.90 of what the same number of bare loops does (0.955 at N = 1 in this mode).
- **The VMs' share of aggregate host index is what virtualization costs:** 83.45 against 93.66 index units at N = 8, so about 11% of throughput, in line with the 0.92 single-VM ratio.
- **Even with no virtualization at all, this CPU loses per index unit.** The bare host's best measured throughput is 99.10 index units (11 loops), which is $0.000457 per 1000 index units at the AX42's price: 1.28× boat.dev's published cost per index unit. Matching boat.dev's published figure needs 127 index units.

### Cost per index unit on the PandaOS install

Price and speed from the same machine (AX42 list price, $0.1631 / h; $0.3685 / h with 2 h of operator time), at 100% busy, with no retention.

| N VMs | host index units (measured) | USD per 1000 index units | + operator time | vs boat.dev reference |
|---|---|---|---|---|
| 1 | 20.97 | $0.002160 | $0.004881 | 6.06x |
| 2 | 37.95 | $0.001194 | $0.002698 | 3.35x |
| 4 | 62.42 | $0.000726 | $0.001640 | 2.04x |
| 7 | 81.14 | $0.000558 | $0.001262 | 1.57x |
| 8 | 83.45 | $0.000543 | $0.001227 | 1.52x |

### CPU governor: performance vs powersave / balance_performance

The box runs amd-pstate in active mode with `powersave` and EPP `balance_performance`. For the A/B test the governor was set to `performance` (EPP `performance`) for runs 2af7f9, 16dd93, 945166 and 555bc1. It was then set back to `powersave` / `balance_performance` on all 16 CPUs, and that was checked.

| | powersave / balance_performance | performance | Change |
|---|---|---|---|
| (a) Node loop, VM, index units | 21.25 (n = 3 VMs, 3 passes each) | 21.43 (n = 1 VM, 3 passes) | +0.8% |
| (a) Node loop, host, index units | 23.08 (n = 3 runs, 3 passes each) | 22.93 (n = 1 run, 3 passes) | −0.6% |
| (g) 8 VMs, host index units | 83.45 (n = 2) | 84.06 (n = 1) | +0.7% |
| (g) 8 host loops, index units | 93.66 (n = 2) | 93.08 (n = 1) | −0.6% |
| (g) 11 host loops, index units | 99.10 (n = 2) | 98.64 (n = 1) | −0.5% |

**The governor makes no measurable difference** on this CPU-bound workload. Both directions stay within 1%, about the run-to-run spread. EPP `balance_performance` already lets the cores boost. The cost per index unit does not change: $0.000539 vs $0.000543 per 1000 index units at 8 VMs.

### Stopped-VM disk use (input to the thin-provisioning model)

`thin.json`, measured on the install: a stopped `default` VM holds **0.47 GiB** of exclusive btrfs data when fresh, and **3.35 GiB** after the Node loop setup (about 600 MB of downloads plus an `npm install`). Both figures include the memory snapshot. The 2.76 GiB guest image is shared by reflinks and counted once per host. boatd's untrusted policy charges such a VM 59 GiB. The [retention tables](#stopped-vm-retention-what-boatdevs-free-14-days-cost-on-own-hardware) use 3.4 GiB as the measured thin-model case.

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
| (a) Node loop, VM, index units | 10.49 [10.41–10.54] | 13.49 [13.28–13.74] | 16.45 [16.30–16.63] | 13.41 [13.08–13.81] |
| (a) Node loop, same runner without a VM, index units | 11.88 | 15.49 | 19.98 | 15.11 |
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
² `stack.sh` turned SMT off as `nosmt=force` would, so the runner had 2 CPUs online, and the VM's 4 vCPUs and the host-side runs shared those 2. In that historical 2026-11-10 run, `memOvercommit` stayed at the bench's 8; `presets.json` and `stack.sh` now enforce `memOvercommit: 1` for `untrusted` to match `boatd`'s `loadConfig()` validation (see the [bench README](../bench/boatbench/README.md#boatd-presets-options-and-the-warm-pool)).
³ Fixed: earlier runs failed here with `503 host_capacity` at `cpuOvercommit` 12. `stack.sh` now derives it from the CPUs online (24 on 2 CPUs).

**API baseline.** `net` = max(0, raw − k × that side's API round trip), where k is the recorded serial critical-path HTTP call count (`api_calls` in new runs with adaptive `0.05s → 0.10s → 0.20s` state polling, excluding intermediate async polls that overlap server-side work, falling back to `drivers.py` `ROUND_TRIPS` for older JSON artifacts). On localhost the baseline is under 1 ms, so net and raw agree to within a few ms on every lifecycle row. It matters only for boat.dev, which is reached over the WAN, and **boat.dev's side has not been measured**, so there is still no equal-footing lifecycle comparison. The harness will produce one when a key exists.

### Density under load, run 42860774575

N VMs ran the Node loop at the same time, on one runner, from one barrier. Fresh VMs, all N measured, no errors in any level. Two repeats per preset agreed within 5%; the table shows their median.

| N | trusted, EPYC 9V74, 4 CPUs: per-VM / host index units | efficiency | trusted-warm, Xeon 8573C, 4 CPUs | efficiency | trusted-writeback, EPYC 7763, 4 CPUs | efficiency | untrusted, Xeon 8370C, **2 CPUs** | efficiency |
|---|---|---|---|---|---|---|---|---|
| 1 | 13.62 / 13.62 | 1.00 | 9.42 / 9.42 | 1.00 | 10.89 / 10.89 | 1.00 | 6.91 / 6.91 | 1.00 |
| 2 | 10.31 / 20.62 | 0.757 | 4.72 / 9.44 ⁴ | 0.501 | 8.44 / 16.88 | 0.775 | 6.85 / 13.70 | 0.992 |
| 4 | 5.55 / **22.24** | 0.408 | 5.26 / **21.00** | 0.558 | 4.37 / **17.46** | 0.401 | 2.62 / 10.47 | 0.379 |
| 6 | 3.49 / 20.94 | 0.256 | 2.40 / 14.39 | 0.255 | 2.82 / 16.97 | 0.259 | 1.42 / 8.49 | 0.205 |

Efficiency = per-VM index units at N / at N = 1. One loop uses 1.49–1.53 CPUs when it has them (1.23 on 2 CPUs): V8's GC and compiler threads run beside the main thread.

⁴ Reproduced in both repeats and not explained.

What was measured:
- **Host throughput plateaus early and then falls.** On a 4-thread runner (2 cores with SMT) the peak is 1.6–2.2× one VM, at N = 4, and at N = 6 it is no higher. On 2 cores with SMT off the peak is at N = 2, and N = 6 gives 38% less than that peak.
- **Per-VM speed collapses under oversubscription:** at N = 6 every VM runs at 21–26% of its solo speed.
- The workload is one Node process per VM, mostly one thread. A workload that keeps all 4 vCPUs busy would contend sooner.

### Cost per index unit from aggregate host index

The fair figure is the host's price over the host's aggregate index: **USD per 1000 index units = host $/h ÷ 3.6 ÷ host index units**, at 100% busy, no operator time, no retention. **The AX42 rows are measured** ([above](#cost-per-index-unit-measured-on-the-ax42)): $0.000520 trusted and $0.000597 untrusted, against boat.dev's published $0.000356.

**The AX102 figures below are projections.** Nobody has run PandaOS on an AX102. They use the AX42's measured curve, because the AX42 is a dedicated Zen 4 box like the AX102 (Ryzen 9 7950X3D). Generated with `boatbench.py report <dir>/results-*.json --server ax102` from the same AX42 raw results.

#### PROJECTION: Hetzner AX102, trusted (22 VMs on 32 CPUs), NOT A MEASUREMENT

Load = 22 VMs x 1.49 CPU per loop / 32 CPUs = **1.02 loops per CPU** (ABOVE the measured range: aggregate host index per CPU is held flat at the highest measured load's, so efficiency = that / this load). Projected efficiency there: **0.381**, so a packed host delivers 22 x 0.381 = 8.4 solo-VM equivalents, and cost per index unit is **2.63x** the uncontended figure.
With the measured solo speed (20.79 index units, a different CPU) as the per-VM speed: $0.4188/h / (22 x 20.79 x 0.381) = **0.000668 USD per 1000 index units (MIXED CPUs)**. Nobody ran PandaOS on that server; the efficiency curve comes from the measured host, whose CPU, SMT layout and memory bandwidth differ.

#### PROJECTION: Hetzner AX102, untrusted (15 VMs on 16 CPUs), NOT A MEASUREMENT

Load = 15 VMs x 1.48 CPU per loop / 16 CPUs = **1.39 loops per CPU** (ABOVE the measured range: aggregate host index per CPU is held flat at the highest measured load's, so efficiency = that / this load). Projected efficiency there: **0.482**, so a packed host delivers 15 x 0.482 = 7.2 solo-VM equivalents, and cost per index unit is **2.07x** the uncontended figure.
With the measured solo speed (20.96 index units, a different CPU) as the per-VM speed: $0.4188/h / (15 x 20.96 x 0.482) = **0.000768 USD per 1000 index units (MIXED CPUs)**. Nobody ran PandaOS on that server; the efficiency curve comes from the measured host, whose CPU, SMT layout and memory bandwidth differ.

The 7950X3D clocks higher than the 35 W 8700GE and has more L3, so these projections are likely low. The speed-independent requirement stands: **to match boat.dev's published cost per index unit, an AX102 must sustain 326 host index units, about 20 per core** (487 and 30 with operator time). The AX42 measured **10.9 index units per core** with SMT (87.06 on 8 cores) and **9.5** without it, so an AX102 would need roughly twice the AX42's per-core throughput.

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

Run [42937179139](https://github.com/aicodeking/pandaos/actions/runs/42937179139), main at `bcf0cda` (the boatd-r2 merge: vCPU threads get their own cgroup, apart from the VMM thread). Untrusted preset, **EPYC 9V45 runner, SMT off: 2 CPUs online**, `Unsafe` cache. Medians, IQR in brackets. (Note: like the other `2026-11-10`–`2026-11-11` runner tables above, this run predates `stack.sh`'s `jailGid` `io_uring`, `losetup --direct-io=on`, and `-ssh-policy-helper-file` online SSH quarantine; rebooted PandaOS `untrusted` on AX42 hardware is pending re-run, and live `boat.dev` is unmeasured without `BOATDEV_API_KEY`.)

| Metric | Runner, no VM | Untrusted VM | VM / host | Old untrusted (run 42818758694, EPYC 9V74, before the fix) |
|---|---|---|---|---|
| (a) Node loop, index units (n = 9) | 18.26 | 16.54 [16.27–16.76] | **0.906** | 13.41 (VM / host 0.887) |
| (d) seq write 1M, MB/s † | 8,514 | 297.1 [284.1–317.2] | 0.035 | 304.1 |
| (d) PTS seq write 1M, MiB/s † | 4,884 | 757.2 | 0.155 | n/a |
| (d) rand write 4k, IOPS † | 71,382 | 52,798 | 0.740 | 41,578 |
| (d) PTS rand read 4k, IOPS | 180,802 | 370,331 | 2.05 | 116,897 |
| (d) 4k rand write, fsync each, IOPS † | 1,446 | 13,661 | (Unsafe drops flushes) | n/a |
| (b) create → exec-ready, s | | 2.34 | | 2.75 |
| (b) fork / resume / new-from-snapshot → exec-ready, s | | 1.24 / 0.581 / 0.811 | | 1.25 / 0.507 / n/a |
| (c) 10 concurrent creates, s | | 22.02 | | 26.69 |
| (e) Go tarball download, Mbit/s | | 1,167 (above the 1,000 Mbit/s ingress cap: Firecracker's rx token bucket holds 1 s of the cap and starts full, so the 59.7 MB download fits in the burst; the steady state is capped, which `boatd/e2e-limits.sh` now measures back to back) | | 1,037 |
| (f) cold `npm ci`, s | | 3.82 | | 4.91 |

Density (g) on those 2 CPUs: per-VM / host index units 16.17 / 16.17 at N = 1, 10.96 / 21.92 at N = 2 (efficiency 0.678), 4.62 / 18.52 at N = 4 (0.286), 3.06 / 18.33 at N = 6 (0.189).

What this run settles and what it does not:
- **The 1M sequential write did not recover on main.** The branch run's 626.6 MB/s (n = 2, EPYC 9V74) is not reproduced: 297.1 MB/s here (n = 3, EPYC 9V45), the same as before the fix. The PTS job (iodepth 64) reaches 757 MiB/s. Different runner CPUs, so this does not prove the fix does nothing; it does mean the branch number should not be quoted.
- **The Node loop keeps 0.906 of the host** in untrusted mode, in line with the trusted presets (0.82–0.88).
- **The untrusted density curve no longer looks flat to N = 2.** The earlier untrusted curve (Xeon 8370C, run 42860774575) gave efficiency 0.992 at N = 2; this one gives 0.678. Runner curves on 2 CPUs do not settle untrusted efficiency. The [measured AX42 curve](#density-under-load-ax42) replaces both for a real server, and the AX102 projection now uses it.

**Caveats for every number above:**
- **† Unsafe cache** on every column except trusted-writeback, as above. The write rows are also not the same kind of number as boat.dev's published fio results.
- **None of these CPUs is the one priced.** boat.dev's published 28.06 index units (earlier: 27.67, 2026-08-24) is a VM on an **AMD Ryzen 9 9950X** (Zen 5). The self-hosted price is a Hetzner AX102, a **Ryzen 9 7950X3D** (Zen 4). The speeds come from EPYC and Xeon runners. A cost per index unit that multiplies one CPU's price by another's speed is no machine's cost, and is marked **MIXED CPUs**. The CPU-consistent figures are boat.dev's price at boat.dev's published speed (**$0.000356 per 1000 index units**) and the [AX42's measured cost](#cost-per-index-unit-measured-on-the-ax42) ($0.000520–$0.000597).
- **Runner, not server.** These are shared 4-vCPU cloud VMs with nested KVM and a loop-file disk. Each preset ran on its own runner, so runner-to-runner variance is in the column differences too.

## 5. What self-hosting does not give you

- **Multi-region and redundancy.** One box is one failure domain. A disk, PSU or network fault takes every VM down until you fix it. Backups are nightly and crash-consistent, not live replicas.
- **Managed ops and an SLA.** You do the patching, kernel updates, reboots, monitoring and restores. Hetzner and OVH offer only hardware replacement and network SLAs, not uptime for your VMs.
- **Measured density and speed on your server.** One dedicated box has been measured (the AX42, one Node workload, CPU load only; a loop-file store on Ubuntu, then native btrfs under PandaOS). The other servers' counts come from an admission formula, and nothing measured memory pressure, so test your own workload first.
- **Elastic burst.** Capacity is fixed at 15 `default` VMs on an AX102 (22 if you accept the OOM risk above). boat.dev's $20 plan allows 100 concurrent sandboxes. A second server means another month of rent and another setup fee, plus your own scheduling across boxes, which boat does not do.
- **Support.** Nobody answers for your VM layer but you.
- **Abuse handling.** You are the hosting provider for whatever tenants run. Complaints go to you, and the provider can null-route your IP (see `docs/boat.md`, *Abuse policy*).
- **Fast creates without a trade-off.** `warmPool` makes creates faster and halves idle RSS, but VMs from one template share ASLR offsets across tenants. `ksmTeam` saves more memory and is a side channel across whatever it spans. `ksmTeam` is off by default; `warmPool` is on by default only on hosts with `untrustedTenants = false`, and `untrustedTenants` refuses both, for those reasons.
- **Hourly exit.** You pay monthly plus a setup fee, and OVH's setup fee equals a full month. boat.dev is per second with a $20 floor.

## Verdict

As of the AX42 runs (Ubuntu, 2026-11-11; PandaOS install, 2026-11-12; lifecycle and disk re-measured on the install at main `0d160dd` after its fixes, 2026-11-13, trusted mode), CI runs 42818758694, 42860774575 and 42937179139, and the 2026-11-02 prices. **No boat.dev number on this page was measured by us: there is no measured boat.dev column, because no run had a `BOATDEV_API_KEY`.** boat.dev's 28.06 index units is one solo VM, not a loaded host. Every boat.dev figure is its published price or its vendor-published benchmark, so each comparison below is PandaOS **measured** against boat.dev **published**, or it is a **projection** (arithmetic, not a run). Where boat.dev publishes nothing, there is no verdict. The Basis column says which kind each row is.

| Workload type | Basis | PandaOS (what we know) | boat.dev (what we know) | Verdict |
|---|---|---|---|---|
| **CPU benchmark index, cost per index unit** | Measured vs published (AX102: projection vs published) | Measured, AX42 at list price, box full: $0.000543 per 1000 index units on the PandaOS install (8 VMs, quota); $0.000520 trusted (11 VMs) and $0.000597 untrusted (7 VMs) on Ubuntu. AX102: *projection* $0.000668–$0.000768 | Published price × published solo speed: $0.000356 | **Loses: 1.46–1.67× boat.dev** (1.52× on the install), 3.3–3.8× with operator time; AX102 *projected* 1.9–2.2×. boat.dev's figure is a solo VM. Its speed under load is unpublished, which flatters boat.dev by an unknown amount |
| **Idle / agent-wait work, cost per VM-hour** (sandboxes mostly waiting, CPU speed irrelevant) | Price arithmetic vs published price | Price arithmetic on measured admission counts: AX42 $0.023 (untrusted, 7 RAM-bound VMs when sparse; $0.027 worst-case at 6 100%-full 59 GiB disks), AX102 $0.028 (15 VMs) at 100% utilization; $0.015 / $0.019 at the trusted 1.5 overcommit | Published $0.036 list ($0.0288 with +25% wallet top-up bonus on $200+ payments; $20/$100/$500/$2,000 monthly plans) | **Wins only when a single box stays nearly full.** Break-even in untrusted mode is 65% on AX42 (75% at 6-VM full-disk worst-case) to 78% on AX102 vs list ($0.036/h), **81% (94% full-disk) to 97%** vs +25% bonus ($0.0288/h), 80–97% (list; 94% on AX42 6-VM full-disk) / never (bonus) on a `4+1` external N+1 cluster + backup, and never with `1+1` HA or operator time (146% / 171% / 116%). Trusted overcommit breaks even at 41–53% full vs list (79–93% with operator time). It loses below that, and loses badly if stopped VMs are kept (next rows). **These break-evens exclude boat.dev's AI proxy fee ($1/day + $1 per 1,000 requests, only with the proxy on):** it can flip the verdict for AI-heavy fleets (above ~9,000 requests/day the fee alone exceeds an AX102; see the [AI-proxy sensitivity](#boatdev-ai-proxy-fee-sensitivity-from-pricesjson)) |
| **Lifecycle latency** | Measured vs not measured | Measured on the AX42 at `0d160dd` (**trusted mode**, predating `-ssh-policy-helper-file` across presets; Round 23–24 batch SSH policy + non-disruptive `rotate-token` keep steady-state token refreshes lightweight): create → exec-ready 0.87–0.90 s cold (1.40–1.58 s before the fixes), 0.31–0.32 s from the warm pool (on by default only with `untrustedTenants = false`); resume 0.15–0.17 s; fork 0.29–0.37 s; new-from-snapshot 0.19–0.23 s; exec round trip 2.0–2.3 ms | **Not measured** (`BOATDEV_API_KEY` not provided), no comparable published figures | **No verdict.** These are PandaOS's own `trusted`-mode numbers predating `-ssh-policy-helper-file`, not a win; rebooted PandaOS `untrusted` (`nosmt=force`, `blk-iocost`, `jailGid` `io_uring`, online SSH quarantine, `warmPool` off) is pending hardware re-run |
| **Disk** | Measured vs published | Measured, `Unsafe` cache (guest flushes dropped): PTS job 57–216k random-read IOPS (medians; bimodal on the install, single repeats 56k–222k, median 216k at `0d160dd` and 56.7k before), 160–260k random-write IOPS, 1,651–2,740 MiB/s seq write (loop file and native btrfs). Durable: a `Writeback` VM on the install's native btrfs fsyncs 84 4k writes/s (measured, the drive's flush counter confirms the flushes); the Ubuntu loop stack 38 (host 82) | Published PTS job: 433k / 413k IOPS, 5,838 MiB/s (fio target and cache mode not published) | **Loses** on every published disk number, by 1.6–7.6×. The two are not necessarily the same kind of number. [Host baseline](#host-disk-baseline-and-the-56k--222k-random-read-split-ax42-pandaos-install): the bare NVMe is already 2.2× (random read) and 1.2× (seq write) below; the rest is the VM path, and the 56k reads come from fio sharing the disk-interrupt vCPU |
| **Single-VM CPU speed** | Measured vs published (boat.dev: one solo VM) | Measured: 21.15–21.25 index units (AX42, Zen 4, 35 W) | Published: 28.06 (Ryzen 9 9950X, Zen 5; StarSling run, 2026-10-03) | **Loses (−24 to −25%)**, mostly on CPU. Both are one solo VM. *Projection:* PandaOS on a 9950X ≈ 26.4–26.6 |
| **Virtualization overhead** | Measured vs inferred | Measured: a VM keeps 0.92 of the host's Node index; VMs keep 0.89–0.90 of bare-host index at 2–8 at once | **Not measured.** 0.97 is only an inference from a public 9950X average; boat.dev's host was never measured | **No verdict.** Measured 0.92 is not compared with an inferred 0.97 |
| **Stopped-VM retention** (boat.dev keeps 14 days free) | Policy arithmetic and a model vs published terms | Full-disk reserve projection under `storageOvercommit` 1 (25 AX102 / 6 AX42 storage slots; raw-capacity sparse projection: 30 / 8): AX102 full-disk **$0.25–$0.72/h**, sparse **$0.21–$0.60/h**, for 24 h / 8 h sessions kept 14 days; AX42 full-disk **$0.41–$1.17/h**, sparse **$0.31–$0.88/h**. *Model:* charging actual use (3.4 GiB measured) brings the AX102 to $0.028–$0.040 and the AX42 to $0.023–$0.059 for the same sessions | Included in the price | **Loses in both nominal-retention projections.** It ties or wins only in the thin-provisioning *model*, which boatd does not ship for untrusted tenants |
| **Against E2B / Daytona / Modal / Vercel** | Price arithmetic vs published list prices | Price arithmetic | $0.22–$0.68 per VM-hour (their list prices) | **Wins per VM-hour**, 6–9× cheaper at 50% utilization (AX102, no overcommit, retention aside) |

Also measured, without a boat.dev counterpart: SMT off costs untrusted mode 6% of aggregate host index at 7 VMs, and the CPU governor (`performance` vs `powersave` / `balance_performance`) changes nothing measurable.

**Where this page is generous to PandaOS.** The headline per-VM-hour and cost-per-index-unit tables price **one host with no spare capacity**, filling it to boatd's admission limit and comparing that packed box with boat.dev's per-sandbox list price. A service that offers what boat.dev's price includes cannot run that way: `boatd` and `boat-api` are single-host daemons with no built-in multi-node scheduler or automatic failover, so surviving a kernel-reboot window or hardware fault ([§5](#5-what-self-hosting-does-not-give-you)) requires independent spare hosts behind external routing plus off-host snapshot backups and headroom for bursts, while buyers spending $200+ on boat.dev receive up to a +25% wallet credit bonus ($0.0288/h). The [sensitivity table](#credit-bonus-plan-fee--habackup-tco-sensitivity) prices both effects explicitly (without double-counting single-host backup on top of the shared 5 TB BX21 archive): a `1+1` HA pair + 5 TB off-host backup roughly doubles cost (`134–157%` break-even vs list, `167–196%` vs +25% bonus — never breaking even), and even a `4+1` N+1 cluster + backup pushes AX42/AX102 untrusted break-even to 80–97% vs list (`94%` on AX42 6-VM full-disk worst-case) and above 100% (never) vs +25% bonus.

**In one line:** per geometric-mean index unit, self-hosted PandaOS on the AX42 costs about 1.5× boat.dev's published list price (and ~1.9× its +25% bonus rate). Per VM-hour it is cheaper only on a single box kept more than 65–78% full (`75%` on AX42 full-disk worst-case; 81–97% vs +25% bonus), with free operator time, no HA spare node, and prompt deletion of stopped VMs. You self-host for control (kernels, images, data locality, no start or snapshot fees), not to undercut boat.dev.

**What would change this verdict:**
- A `BOATDEV_API_KEY` run (ideally on the $100/mo plan for 30 starts/min), for a measured boat.dev column: API baseline, lifecycle, disk and a load curve. That would also turn the "no verdict" rows into verdicts.
- A post-reboot AX42 `untrusted` run (`services.boat.untrustedTenants = true`, `nosmt=force`, `io_uring` via `jailGid`, `blk-iocost`, online SSH quarantine) on the PandaOS install.
- An AX102 (or 9950X) run with `BOATBENCH_DENSITY_LEVELS=1,4,8,15,22`, to replace the AX102 projection.
- Drives with power-loss protection for durable writes (the native-btrfs `Writeback` run gave 84 fsyncs/s, bound by the NVMe's flush).
- A thin-provisioning storage policy with per-VM quotas, or an archive tier for stopped VMs, which would lift the retention limit.

### Completed-suite throughput from saved density trials (MEASURED)

One pass invokes the complete pinned web-tooling CLI once. Rate = `N × successful passes per VM / wall_s`, pooled as a median over complete trials. Wall time includes the barrier, execution, and harness overhead. These historical runs predate current production untrusted boot validation. These figures are not compared with the vendor geometric-mean index; the vendor has no compatible completed-suite measurement.

| Existing artifact | Concurrent suites (N) | Completed suites/s | Complete trials |
|---|---|---|---|
| [Ubuntu trusted](../bench/boatbench/results/ax42-ubuntu-2026-11-11/trusted/results-pandaos-2026-11-11T091730Z.json) | 1 | 0.01929 | 2 |
| [Ubuntu trusted](../bench/boatbench/results/ax42-ubuntu-2026-11-11/trusted/results-pandaos-2026-11-11T091730Z.json) | 11 | 0.10319 | 2 |
| [Ubuntu untrusted (historical)](../bench/boatbench/results/ax42-ubuntu-2026-11-11/untrusted/results-pandaos-2026-11-11T103837Z.json) | 1 | 0.01975 | 2 |
| [Ubuntu untrusted (historical)](../bench/boatbench/results/ax42-ubuntu-2026-11-11/untrusted/results-pandaos-2026-11-11T103837Z.json) | 7 | 0.08828 | 2 |
| [PandaOS install performance (trusted)](../bench/boatbench/results/ax42-pandaos-2026-11-12/performance/results-pandaos-2026-11-12T061212Z.json) | 1 | 0.01938 | 1 |
| [PandaOS install performance (trusted)](../bench/boatbench/results/ax42-pandaos-2026-11-12/performance/results-pandaos-2026-11-12T061212Z.json) | 8 | 0.09884 | 1 |
| [Bare host performance](../bench/boatbench/results/ax42-pandaos-2026-11-12/performance/results-host-2026-11-12T061932Z.json) | 1 | 0.02043 | 1 |
| [Bare host performance](../bench/boatbench/results/ax42-pandaos-2026-11-12/performance/results-host-2026-11-12T061932Z.json) | 11 | 0.11652 | 1 |

## Reproduce

All derived numbers come from the inputs in `bench/boatbench/prices.json` and boatd's source:
- monthly cost = `usd_per_month + setup_usd/12 + backup_usd_per_month` (4+1 total cluster: `5·(usd_per_month + setup_usd/12) + 14.784`, divided across 4 serving hosts; 1+1: `2·(usd_per_month + setup_usd/12) + 14.784`; each replaces single-host backup with one shared 5 TB archive)
- VMs = `⌊(MemTotal − hostReserveMiB)·memOvercommit / (8192 + vmOverheadMiB)⌋`, capped by `⌊threads·cpuOvercommit/4⌋`, with MemTotal ≈ RAM·63/64 − 256 MiB and the constants read from `components/boat/boatd/config.go` by `bench/boatbench/admission.py`. Untrusted: overcommit 1.0, threads = cores. `bench/boatbench/test_boatbench.py` recomputes every count in `prices.json` and fails if boatd's constants or its admission formula change.
- cost per VM-hour = cost / (VMs · 730 · utilization)
- break-even VM-hours = cost / 0.036 (list PAYG) or cost / 0.0288 (max +25% wallet bonus)
- usable tenant store GiB = `usable_store_gib` (`⌊store_gib · 0.85⌋`, deducting 10% `boatd` `minFreePct` + 5% OS/base images/btrfs metadata: 404 GiB on AX42, 1,519 GiB on AX102 and AX162, 759 GiB on OVH Advance-3, 1,503 GiB on Latitude m4.metal.medium); untrusted full-disk storage slots = `⌊usable_store_gib / 59⌋` (6 on AX42, 25 on AX102/AX162/Latitude, 12 on OVH Advance-3; on AX42 `hostStorageCheck` checks raw `⌊476 / 59⌋ = 8` nominal VMs so all 7 RAM-bound sparse VMs boot); running VM-hours ≤ min(VMs·730, slots·730·H/(H+R)); a kept stopped VM costs cost / slots per month

Every cost table on this page is generated by boatbench from committed inputs:
- `boatbench.py costs`: per VM-hour, credit-bonus / plan-fee / HA-backup sensitivity, break-even and retention, from `prices.json`.
- `boatbench.py report <results> --server ax42|ax102|...`: cost per index unit, from the raw results under `bench/boatbench/results/`.

The PandaOS price is derived from the chosen server in `prices.json` (`report.load_prices`): month + setup/12 + backup, per host hour, and per VM at the `memOvercommit` 1.0 count. Where the speed was measured on the priced CPU, the report prints a **measured** cost per index unit: host $/h ÷ 3.6 ÷ measured host index units. Elsewhere it prints a **projection** from the measured efficiency curve, with every index-normalized figure marked **MIXED CPUs**.

Benchmark report prices follow the recorded VM size. boat.dev measurements use the saved `sizes` rate card;
the published-speed reference is a `default` VM. PandaOS admission counts, per-VM prices and density
projections model `default` only; costs for unsupported or missing shapes are suppressed. Host-native
density ratios require matching recorded host descriptions, client CPU/OS and host CPU counts; other
evidence is marked **UNMATCHED**, with ratios suppressed rather than attributed to VM overhead.
