# Benchmarks

Every number PandaOS publishes comes from the NixOS VM tests and host-side checks in `tests/`, run by
CI with `nix flake check` on nixos-26.05. All of them are measured in QEMU VMs on GitHub runners;
**nothing has been measured on real hardware yet**. Each test prints a `METRIC {json}` line in its build log and writes
the same object to `$out/metrics.json`; `bench/collect.py` gathers them into a table.

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

## Planned: Ubuntu and Debian comparison

**Not built yet.** We make no claims against other systems until it runs.

The plan (also in `bench/README.md`): a nightly job on dedicated hardware provisions **identical VMs**
(same size, region and disk) running PandaOS headless and desktop, Ubuntu Server, Ubuntu Desktop and
Debian, and records for each:

- idle RAM after boot and settle, with both methods above
- installed footprint on disk
- boot time to multi-user and to the greeter
- agent job round trip, where it applies

The results go into the same `bench/results/<date>.json` format as a `baseline` section, so
`collect.py` can print PandaOS next to each distro.
