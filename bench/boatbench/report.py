"""Stats, cost and the markdown report for boatbench result files."""
import json
import os
import re
import statistics

HERE = os.path.dirname(os.path.abspath(__file__))
SIDES = ("pandaos", "boatdev")

# key, label, unit, higher_is_better
METRICS = [
    ("node_runs_s", "(a) Node.js build/test loop: PTS node-web-tooling", "runs/s", True),
    ("api_rtt_p50_ms", "(b) API round-trip baseline: authenticated GET of the VM, p50 of 25", "ms", False),
    ("create_ready_s", "(b) create -> exec-ready", "s", False),
    ("exec_rtt_p50_ms", "(b) exec round trip `true`, p50 of 50", "ms", False),
    ("exec_rtt_p95_ms", "(b) exec round trip `true`, p95 of 50", "ms", False),
    ("fork_ready_s", "(b) fork -> fork exec-ready", "s", False),
    ("stop_s", "(b) stop (until stopped)", "s", False),
    ("resume_ready_s", "(b) resume -> exec-ready", "s", False),
    ("snapshot_s", "(b) snapshot (until usable)", "s", False),
    ("from_snapshot_ready_s", "(b) new-from-snapshot -> exec-ready", "s", False),
    ("concurrent10_wall_s", "(c) 10 concurrent creates, wall to all exec-ready", "s", False),
    ("concurrent8_wall_s", "(c) 8 concurrent creates (BOATBENCH_CONCURRENT_N=8), wall to all exec-ready", "s", False),
    ("seq_write_mbps", "(d) disk seq write 1M", "MB/s", True),
    ("seq_read_mbps", "(d) disk seq read 1M", "MB/s", True),
    ("rand_write_iops", "(d) disk rand write 4k", "IOPS", True),
    ("rand_read_iops", "(d) disk rand read 4k", "IOPS", True),
    ("pts_rand_read_iops", "(d) PTS fio job: rand read 4k", "IOPS", True),
    ("pts_rand_write_iops", "(d) PTS fio job: rand write 4k", "IOPS", True),
    ("pts_seq_write_mibps", "(d) PTS fio job: seq write 1M", "MiB/s", True),
    ("fsync_rand_write_iops", "(d) rand write 4k, fsync after every write, iodepth 1", "IOPS", True),
    ("seq_write_endfsync_mbps", "(d) seq write 1M, fsync at the end", "MB/s", True),
    ("dl_go_tarball_googlecdn_mbit", "(e) download Go 1.25.0 tarball, 59.7 MB (dl.google.com)", "Mbit/s", True),
    ("dl_nodejs_tarball_mbit", "(e) download Node.js 22.22.3 tarball, ~30 MB (nodejs.org)", "Mbit/s", True),
    ("tcp_connect_cloudflare_1.1.1.1_ms", "(e) TCP connect 1.1.1.1, median of 10", "ms", False),
    ("tcp_connect_github.com_ms", "(e) TCP connect github.com, median of 10", "ms", False),
    ("npm_ci_s", "(f) cold `npm ci` of pinned project", "s", False),
]

# Lifecycle metrics also reported net of the API round trip (boatbench.add_net).
NET_KEYS = ("exec_rtt_p50_ms", "create_ready_s", "fork_ready_s", "stop_s", "resume_ready_s", "snapshot_s",
            "from_snapshot_ready_s", "concurrent10_wall_s", "concurrent8_wall_s")
_BASE = {k: (label, unit, hib) for k, label, unit, hib in METRICS}
METRICS = METRICS + [(k + "_net", _BASE[k][0] + ", NET of API round trips (raw - k x baseline p50)", _BASE[k][1],
                      _BASE[k][2]) for k in NET_KEYS]

RUNNER_CPU = "EPYC 7763"  # GitHub's ubuntu-latest runners; published.json cpu_scaling.runner_cpu

COMMON_CAVEATS = [
    "Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); "
    "boat.dev stop archives the disk and resume/fork restore it in the background after `ready` "
    "(docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.",
    "Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled "
    "to `ready`). New-from-snapshot uses each one's create-from-snapshot.",
    "Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.",
]
NET_CAVEAT = ("Raw lifecycle latencies include the API round trip from the harness host to the provider: {where}. "
              "The NET rows subtract k x that side's measured API round-trip p50, k = the fewest sequential API calls "
              "the timing contains (drivers.py ROUND_TRIPS: 2 for create/fork/resume/from-snapshot, stop and snapshot "
              "1 on PandaOS and 2 on boat.dev, which polls them).")
SAMPLE_CAVEAT = ("Small samples (default 5 fresh VMs per side). The table shows median and IQR; overlapping IQRs mean "
                 "no clear winner.")
RUNNER_CAVEATS = [
    "The PandaOS numbers from CI come from a GitHub-hosted runner, NOT a dedicated server: shared 4 vCPU, "
    "KVM exposed to the runner VM (Firecracker runs nested inside Azure's hypervisor), US region. "
    "boat.dev runs in the EU (Germany/Finland/France). Comparisons are indicative until "
    "`run-on-server.sh` runs on the owner's dedicated server.",
    "On the runner, each PandaOS VM (4 vCPU / 8 GiB) gets the whole 4-vCPU host, and the 10-VM concurrency test "
    "oversubscribes it (boatd memOvercommit/cpuOvercommit raised just so it fits). boat.dev places VMs on its fleet.",
] + COMMON_CAVEATS + [
    NET_CAVEAT.format(where="the CI PandaOS job talks to localhost, the boat.dev job talks to boat.dev over the "
                            "internet from a US runner"),
    "Disk numbers on the runner come from a btrfs loop file on a shared cloud disk. They cannot speak for NVMe, "
    "including boat.dev's published numbers, even when the fio job is the same.",
    SAMPLE_CAVEAT,
]
SERVER_CAVEATS = [
    "The PandaOS numbers come from a dedicated server, with the harness on the same box talking to its local API. "
    "The server, its disks and its store are the harness-declared host in the table above. boat.dev runs in the EU "
    "(Germany/Finland/France).",
] + COMMON_CAVEATS + [
    NET_CAVEAT.format(where="the PandaOS harness talks to the API on the same box, the boat.dev side (if measured) "
                            "talks to boat.dev over the internet"),
    "Disk numbers come from this server's boat store as declared above. boat.dev's published disk numbers come "
    "from other NVMe drives with an unpublished fio target and cache mode, so even the same fio job is not a "
    "like-for-like disk comparison.",
    SAMPLE_CAVEAT,
]


def on_runner(results):
    """True when a result came from a GitHub-hosted runner (CI run URL or the runner CPU), not a dedicated server."""
    return any("/actions/runs/" in str(r.get("run_url") or "") or RUNNER_CPU in str(r.get("client", {}).get("cpu_model"))
               for r in results)


def caveats(results):
    return RUNNER_CAVEATS if on_runner(results) else SERVER_CAVEATS


WRITE_METRICS = ("seq_write_mbps", "rand_write_iops", "pts_rand_write_iops", "pts_seq_write_mibps",
                 "fsync_rand_write_iops", "seq_write_endfsync_mbps")

# Measured both on the runner itself and in a VM on it.
HOST_VM_METRICS = ("node_runs_s", "seq_write_mbps", "seq_read_mbps", "rand_write_iops", "rand_read_iops",
                   "pts_rand_read_iops", "pts_rand_write_iops", "pts_seq_write_mibps", "fsync_rand_write_iops",
                   "seq_write_endfsync_mbps")
WARM_POOL_CAVEAT = ("`warmPool` is ON in this run, and it is off by default. VMs from one template share boot-time "
                    "randomness, including the ASLR offsets of already-running processes, across tenants "
                    "(components/boat/PROTOCOL.md). Do not read warm numbers as the default configuration.")


def unsafe_cache(res):
    """The PandaOS side's Firecracker cache_type, if it ignores guest flushes."""
    c = str((res.get("pandaos") or {}).get("fc_cache_type", ""))
    return c if c.startswith("Unsafe") else None


def unsafe_note(cache):
    return (f"† PandaOS write numbers: Firecracker's drive `cache_type` was **{cache}**. In `Unsafe` mode the "
            "guest's flushes are not passed to the host, so fsync and O_DIRECT writes complete once they reach the "
            "host page cache, not the disk. They are not durable-write numbers, and boat.dev's need not be the same "
            "kind. `diskCache: Writeback` makes boatd honour guest flushes.")


def cpu_of(result):
    return next((rep["probe"].get("cpu_model") for rep in result.get("repeats", []) if rep.get("probe")), None) or "-"


def percentile(values, p):
    """Linear-interpolated percentile (p in 0..100), like numpy's default."""
    v = sorted(values)
    if not v:
        return None
    k = (len(v) - 1) * p / 100
    lo, hi = int(k), min(int(k) + 1, len(v) - 1)
    return v[lo] + (v[hi] - v[lo]) * (k - lo)


def summarize(values):
    v = [float(x) for x in values if x is not None]
    if not v:
        return None
    return {"n": len(v), "median": statistics.median(v), "q1": percentile(v, 25), "q3": percentile(v, 75),
            "min": min(v), "max": max(v)}


def samples(result, key):
    """Pool one metric across repeats; a repeat may hold a scalar or a list (passes)."""
    out = []
    for rep in result.get("repeats", []):
        v = rep.get("metrics", {}).get(key)
        out.extend(v if isinstance(v, list) else [v])
    return [x for x in out if x is not None]


def server_price(raw, key):
    """PandaOS price block for one server of prices.json["servers"], by its `key` (ax42, ax102, ...)."""
    srv = {x.get("key"): x for x in raw.get("servers", [])}
    if key not in srv:
        raise ValueError(f"unknown --server {key!r}; prices.json has {sorted(k for k in srv if k)}")
    s, a = srv[key], raw.get("assumptions", {})
    hours = a.get("hours_per_month", 730)
    monthly = s["usd_per_month"] + s["setup_usd"] / a.get("setup_amortization_months", 12) + s["backup_usd_per_month"]
    operator = a.get("operator_hours_per_month", 0) * a.get("operator_usd_per_hour", 0)
    return {"usd_per_hour": monthly / hours / s["vms_overcommit_1_0"], "cpu": s["cpu"], "server": s,
            "source": f"prices.json servers[{key}]: {s['source']}; ${monthly:.2f}/month incl. setup/12 + backup, "
                      f"/ {s['vms_overcommit_1_0']} VMs (memOvercommit 1.0) / {hours} h",
            "density_model": {"name": s["name"], "host_usd_per_hour": monthly / hours,
                              "host_usd_per_hour_operator": (monthly + operator) / hours,
                              "trusted": {"vms": s["vms_overcommit_1_5"], "cpus": s["vcpu_threads"]},
                              "untrusted": {"vms": s["vms_untrusted"], "cpus": s["cores"]}}}


def load_prices(path, server=None):
    if not path or not os.path.exists(path):
        return None
    with open(path) as f:
        raw = json.load(f)
    prices = {}
    for side in SIDES:
        p = raw.get(side)
        if side == "pandaos" and raw.get("servers") and isinstance(p, dict) and "usd_per_hour" not in p:
            prices[side] = server_price(raw, server or p.get("default_server", "ax102"))
            continue
        if isinstance(p, dict) and isinstance(p.get("usd_per_hour"), (int, float)) and p["usd_per_hour"] >= 0:
            prices[side] = {"usd_per_hour": float(p["usd_per_hour"]), "source": p.get("source", ""),
                            "cpu": p.get("cpu", "")}
            if isinstance(p.get("density_model"), dict):
                prices[side]["density_model"] = p["density_model"]
    return prices or None


def cost_rows(summaries, prices):
    """USD per 1000 Node loop runs and per cold npm ci, from price x measured time."""
    rows = []
    for side in SIDES:
        p = (prices or {}).get(side)
        s = summaries.get(side, {})
        if not p:
            continue
        node, npm = s.get("node_runs_s"), s.get("npm_ci_s")
        per_k = p["usd_per_hour"] / 3600 * 1000 / node["median"] if node and node["median"] > 0 else None
        per_npm = p["usd_per_hour"] / 3600 * npm["median"] if npm else None
        rows.append((side, p, per_k, per_npm))
    return rows


def cost_section(summ, prices, res, published):
    out = ["", "## Cost", ""]
    rows = cost_rows(summ, prices)
    if not rows:
        return out + ["No `prices.json` with `usd_per_hour` per side: cost not computed, runs/s only."]
    out += ["**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. "
            "Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS "
            "figure.", "",
            "Cost = side's price per hour (prices.json) x time. A per-run cost only means something when the "
            "price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost "
            "of any real machine.**", "",
            "| Side | USD/hour | The price buys | Speed measured on | USD per 1000 Node loop runs | USD per cold npm ci |",
            "|---|---|---|---|---|---|"]
    for side, p, per_k, per_npm in rows:
        measured = cpu_of(res[side]) if side in res else "-"
        mixed = side not in res or not speed_on_cpu(res[side], p.get("cpu"))
        if side in res and side == "pandaos":
            measured += f" (guest); harness host {res[side].get('client', {}).get('cpu_model', '-')}"
        tag = " (MIXED CPUs)" if mixed else ""
        out.append(f"| {side} | {p['usd_per_hour']:.4f} | {p.get('cpu') or '-'} | {measured} | "
                   f"{'-' if per_k is None else f'{per_k:.6f}{tag}'} | {'-' if per_npm is None else f'{per_npm:.6f}{tag}'} |")
    bd = (prices or {}).get("boatdev")
    if bd and published:
        v = published["metrics"]["node_runs_s"]["value"]
        out += ["", f"CPU-consistent figure: boat.dev's price with boat.dev's own published speed on the CPU it "
                f"sells ({v} runs/s, vendor-measured, {published['rows'][0]['cpu']}): "
                f"**{bd['usd_per_hour'] / 3600 * 1000 / v:.6f} USD per 1000 runs**. PandaOS's price here is "
                f"{(prices.get('pandaos') or {}).get('cpu') or '-'}: its per-run cost is CPU-consistent only where "
                "the speed above was measured on that CPU (the density section says which)."]
    return out


def speed_on_cpu(result, cpu):
    """Was this result's speed measured on `cpu`? Firecracker guests may report a generic model ("AMD EPYC"),
    so the harness host's CPU counts too: true when the harness runs on the PandaOS host (run-on-server.sh, CI)."""
    return bool(cpu) and (_same_cpu(cpu, cpu_of(result)) or _same_cpu(cpu, result.get("client", {}).get("cpu_model")))


def _same_cpu(a, b):
    """Loose model match: the price's CPU string names the measured model."""
    m = re.search(r"(EPYC \w+|Ryzen \d (?:PRO )?\w+|Xeon[^,(]*)", b or "")
    return bool(m) and m.group(1).lower() in (a or "").lower()


def fmt(x):
    if x is None:
        return "-"
    a = abs(x)
    return f"{x:,.0f}" if a >= 1000 else f"{x:.1f}" if a >= 100 else f"{x:.2f}" if a >= 1 else f"{x:.3f}"


def cell(s):
    return "-" if not s else f"{fmt(s['median'])} [{fmt(s['q1'])}-{fmt(s['q3'])}] n={s['n']}"


def verdict(pan, bd, higher):
    if not pan or not bd:
        return "only one side measured"
    a, b = pan["median"], bd["median"]
    if a == b:
        return "tie"
    overlap = pan["q1"] <= bd["q3"] and bd["q1"] <= pan["q3"]
    win = a > b if higher else a < b
    word = "PandaOS wins" if win else "**PandaOS loses**"
    return word + (" (IQRs overlap)" if overlap else "")


def ratio(pan, bd):
    if not pan or not bd or not bd["median"]:
        return "-"
    return f"{pan['median'] / bd['median']:.2f}x"


def by_side(results):
    out = {}
    for r in results:
        out.setdefault(r["driver"], []).append(r)
    return out


def merged(results):
    """Several result files of one side are pooled as more repeats."""
    if len(results) == 1:
        return results[0]
    m = dict(results[0])
    m["repeats"] = [rep for r in results for rep in r.get("repeats", [])]
    return m


def auto_caveats(res):
    out = []
    if any('"warmPool":true' in str(r.get("boatd_options", "")).replace(" ", "") for r in res.values()):
        out.append(WARM_POOL_CAVEAT)
    probes = {s: [rep.get("probe") or {} for rep in r.get("repeats", [])] for s, r in res.items()}
    for side, ps in probes.items():
        for p in ps:
            if p and (p.get("nproc") != 4 or not p.get("mem_mib") or abs(p["mem_mib"] - 8192) > 0.1 * 8192):
                out.append(f"{side}: a VM reported {p.get('nproc')} vCPU / {p.get('mem_mib')} MiB, not 4 / ~8192.")
                break
    tools = {s: sorted({(rep.get("disk_tool") or "-") for rep in r.get("repeats", [])}) for s, r in res.items()}
    if len({t for ts in tools.values() for t in ts}) > 1:
        out.append(f"Disk tool differs between sides: {tools}. dd numbers are not comparable with fio.")
    for side, r in res.items():
        errs = [f"r{rep.get('index')}:{k}" for rep in r.get("repeats", []) for k in (rep.get("errors") or {})]
        if errs:
            out.append(f"{side}: failed steps (see raw JSON): {', '.join(errs[:20])}.")
    return out


def side_table(res):
    lines = ["| | " + " | ".join(res) + " |", "|---|" + "---|" * len(res)]

    def first_probe(r):
        return next((rep["probe"] for rep in r.get("repeats", []) if rep.get("probe")), {})

    rows = [
        ("Host / location (harness-declared)", lambda r: r.get("host", {}).get("desc", "-")),
        ("Harness client CPU", lambda r: r.get("client", {}).get("cpu_model", "-")),
        ("Harness client egress (Cloudflare colo)", lambda r: r.get("client", {}).get("egress", "-")),
        ("Requested size", lambda r: r.get("size", "-")),
        ("Preset (presets.json)", lambda r: r.get("preset", "-")),
        ("boatd options over the defaults", lambda r: r.get("boatd_options", "-")),
        ("Firecracker drive cache_type", lambda r: r.get("fc_cache_type", "-")),
        ("VM vCPU / RAM / disk (measured)", lambda r: "{} vCPU / {} MiB / {} GiB".format(
            first_probe(r).get("nproc"), first_probe(r).get("mem_mib"), first_probe(r).get("disk_gib"))),
        ("VM CPU model", lambda r: first_probe(r).get("cpu_model", "-")),
        ("VM kernel / OS / virt", lambda r: "{} / {} / {}".format(
            first_probe(r).get("kernel"), first_probe(r).get("os"), first_probe(r).get("virt"))),
        ("VM egress (Cloudflare colo, country)", lambda r: "{} {}".format(
            first_probe(r).get("egress_colo"), first_probe(r).get("egress_country"))),
        ("Node (benchmark / image default)", lambda r: "{} / {}".format(
            next((rep.get("node") for rep in r.get("repeats", []) if rep.get("node")), "-"),
            first_probe(r).get("system_node"))),
        ("Repeats (fresh VMs)", lambda r: str(len(r.get("repeats", [])))),
        ("Run started (UTC)", lambda r: r.get("started", "-")),
        ("Run URL", lambda r: r.get("run_url") or "-"),
        ("Source commit (boat / PandaOS)", lambda r: r.get("source_commit") or "not recorded"),
    ]
    for label, f in rows:
        lines.append(f"| {label} | " + " | ".join(str(f(r)) for r in res.values()) + " |")
    return lines


def host_vs_vm(host, vm, runner=True):
    """Markdown table and the node VM / host ratio (None if not measured on both)."""
    labels = {k: (label, unit) for k, label, unit, _ in METRICS}
    lines = [f"| Metric | Unit | {'Runner host' if runner else 'Host'}, no VM (measured) | PandaOS VM (measured) | VM / host |",
             "|---|---|---|---|---|"]
    node = None
    for key in HOST_VM_METRICS:
        h, v = summarize(samples(host, key)), summarize(samples(vm, key))
        if not h or not v:
            continue
        r = v["median"] / h["median"] if h["median"] else None
        node = r if key == "node_runs_s" else node
        lines.append(f"| {labels[key][0]} | {labels[key][1]} | {cell(h)} | {cell(v)} | {fmt(r)} |")
    return lines, node


def projections(host_node, overhead, runner_cpu, published):
    """(method, runs/s) estimates of PandaOS on the vendor's CPU. Each one is an ESTIMATE."""
    sc = published["cpu_scaling"]
    ob, pm = sc["same_benchmark"], sc["passmark_single_thread"]
    vendor = published["metrics"]["node_runs_s"]["value"]
    rows = [(f"vendor's VM score x our VM/host: {vendor} x {overhead:.3f}. A floor if the vendor's VM also "
             "has overhead, because its host then scores above 27.67", vendor * overhead),
            (f"public host average x our VM/host: {ob['reference']} x {overhead:.3f}", ob["reference"] * overhead)]
    if sc["runner_cpu"] in (runner_cpu or ""):  # the CPU ratios only hold for this runner CPU
        for name, s in (("same benchmark, public averages", ob), ("PassMark single-thread", pm)):
            ratio_ = s["reference"] / s["runner"]
            rows.append((f"our host x CPU ratio ({name}) x our VM/host: {host_node:.2f} x {ratio_:.2f} x {overhead:.3f}",
                         host_node * ratio_ * overhead))
    return rows


def normalization(host_results, vm, published, runner=True):
    host = merged(host_results)
    desc = host.get("host", {}).get("desc", "-")
    table, overhead = host_vs_vm(host, vm, runner)
    out = ["", "## Runner host vs PandaOS VM (MEASURED, same runner)" if runner else
           "## Host vs PandaOS VM (MEASURED, same host)", "",
           f"Identical scripts. Host: {desc}. Disk: both sides write to the same btrfs store "
           f"({'a loop file' if runner else 'as declared'}); the host side's files are NOCOW. "
           "The VM adds its guest ext4, virtio and Firecracker's I/O engine, and its O_DIRECT stops at the "
           "host page cache, so a ratio above 1 does not mean a VM beats the host.", ""] + table
    host_node = summarize(samples(host, "node_runs_s"))
    if not published or not overhead or not host_node:
        return out
    sc = published["cpu_scaling"]
    rows = projections(host_node["median"], overhead, host.get("client", {}).get("cpu_model"), published)
    vals = [v for _, v in rows]
    out += ["", f"## PROJECTION of PandaOS on {sc['reference_cpu']} (NOT A MEASUREMENT)", "",
            "**Nobody ran PandaOS on this CPU. These numbers are arithmetic, not benchmark results.** "
            f"Estimated Node loop range: **{min(vals):.1f}-{max(vals):.1f} runs/s**.", "",
            "| Method | Projected runs/s (not measured) |", "|---|---|"]
    out += [f"| {m} | {v:.1f} |" for m, v in rows]
    where = ("a shared, nested Azure VM (a GitHub runner), and it may not transfer to a dedicated host" if runner
             else "this host (see the Host line above)")
    lim = (sc["cpu_ratio_limits"] + " " if len(rows) > 2 else "") + sc["limits"]
    out += ["", f"Limits: the VM/host ratio ({overhead:.3f}) was measured on {where}. {lim}", "",
            f"Same-benchmark source: {sc['same_benchmark']['source']}. "
            f"PassMark source: {sc['passmark_single_thread']['source']}."]
    return out


PRESET_METRICS = ("node_runs_s", "api_rtt_p50_ms", "create_ready_s", "exec_rtt_p50_ms", "fork_ready_s", "stop_s", "resume_ready_s",
                  "snapshot_s", "from_snapshot_ready_s", "concurrent10_wall_s", "concurrent8_wall_s", "seq_write_mbps", "seq_read_mbps",
                  "rand_write_iops", "rand_read_iops", "pts_rand_write_iops", "pts_rand_read_iops",
                  "fsync_rand_write_iops", "seq_write_endfsync_mbps",
                  "dl_go_tarball_googlecdn_mbit", "npm_ci_s")


def render_presets(results):
    """PandaOS medians side by side for each boatd preset (one runner per preset)."""
    groups = {}
    for r in results:
        if r["driver"] == "pandaos":
            groups.setdefault(r.get("preset", "-"), []).append(r)
    res = {k: merged(v) for k, v in sorted(groups.items())}
    labels = {k: (label, unit, hib) for k, label, unit, hib in METRICS}
    where = ("Each column ran on its own GitHub runner (shared, nested KVM, not a dedicated server), so runner-to-"
             "runner variance is in the differences too." if on_runner(results) else
             "Each column is a separate run on the harness-declared host(s) below, so run-to-run variance is in the "
             "differences too.")
    out = ["# boatbench: PandaOS by boatd preset (MEASURED)", "", where + " median [IQR] n=samples.", "",
           "| Metric | Unit | Better | " + " | ".join(res) + " |", "|---|---|---|" + "---|" * len(res)]
    for key in PRESET_METRICS:
        cells = [cell(summarize(samples(r, key))) for r in res.values()]
        if all(c == "-" for c in cells):
            continue
        label, unit, hib = labels[key]
        out.append(f"| {label} | {unit} | {'higher' if hib else 'lower'} | " + " | ".join(cells) + " |")
    out += ["", "| | " + " | ".join(res) + " |", "|---|" + "---|" * len(res)]
    for name, f in (("boatd options", lambda r: r.get("boatd_options", "-")),
                    ("Firecracker cache_type", lambda r: r.get("fc_cache_type", "-")),
                    ("VM vCPU seen", lambda r: next((str(rep["probe"].get("nproc")) for rep in r.get("repeats", [])
                                                     if rep.get("probe")), "-")),
                    ("Failed steps", lambda r: ", ".join(f"r{rep.get('index')}:{k}" for rep in r.get("repeats", [])
                                                         for k in (rep.get("errors") or {}))[:300] or "none"),
                    ("Host (harness-declared)", lambda r: r.get("host", {}).get("desc", "-")),
                    ("Run URL", lambda r: r.get("run_url") or "-")):
        out.append(f"| {name} | " + " | ".join(str(f(r)).replace("|", "/") for r in res.values()) + " |")
    return "\n".join(out) + "\n"


def render(results, prices=None, published=None):
    host_results = [r for r in results if r["driver"] == "host"]
    results = [r for r in results if r["driver"] != "host"]
    res = {s: merged(rs) for s, rs in by_side(results).items()}
    summ = {s: {k: summarize(samples(r, k)) for k, *_ in METRICS} for s, r in res.items()}
    out = ["# boatbench: PandaOS boat vs boat.dev", "",
           "Measured numbers below come from this harness (`bench/boatbench`). Published numbers are "
           "boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.", "",
           "## Sides: hardware and location", ""]
    out += side_table(res)
    out += ["", "## Measured results", "",
            "median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.", ""]
    if "boatdev" not in res:
        out += ["**boat.dev was not measured by this harness (no `BOATDEV_API_KEY`), so no row has a measured-vs-"
                "measured verdict.** The only boat.dev numbers are the vendor's published claims (next section), and "
                "its 27.67 runs/s is one solo VM, not a fully packed server.", ""]
    out += [
            "| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |",
            "|---|---|---|---|---|---|---|"]
    cache = unsafe_cache(res)
    for key, label, unit, hib in METRICS:
        pan, bd = summ.get("pandaos", {}).get(key), summ.get("boatdev", {}).get(key)
        if not pan and not bd:
            continue
        label = label + (" †" if cache and key in WRITE_METRICS and pan else "")
        out.append(f"| {label} | {unit} | {'higher' if hib else 'lower'} | {cell(pan)} | {cell(bd)} | "
                   f"{ratio(pan, bd)} | {verdict(pan, bd, hib)} |")
    if cache:
        out += ["", unsafe_note(cache)]
    if published:
        out += ["", "## Published (boat.dev's claims) vs measured (this harness)", "",
                f"Source: {published['source']} (measured by the vendor on {published['measured_on']}). "
                f"{published.get('note', '')}", "",
                "| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |",
                "|---|---|---|---|---|"]
        units = {k: u for k, _, u, _ in METRICS}
        for key, p in published["metrics"].items():
            pan, bd = summ.get("pandaos", {}).get(key), summ.get("boatdev", {}).get(key)
            out.append(f"| {key} ({units.get(key, '')}) | {fmt(p['value'])} | {cell(pan)} | {cell(bd)} | "
                       f"{p.get('note', '')} |")
        if published.get("rows"):
            out += ["", "The vendor's whole table (VENDOR-MEASURED, not by this harness). The CPU is what each "
                    "guest reported in the vendor's run.json, so these rows differ in hardware as well as software.", "",
                    "| Provider | runs/s | CPU the guest reported |", "|---|---|---|"]
            out += [f"| {r['provider']} | {r['runs_s']} | {r['cpu']} |" for r in published["rows"]]
    if host_results and "pandaos" in res:
        out += normalization(host_results, res["pandaos"], published, on_runner(host_results + results))
    out += density_section(res, prices, published)
    out += host_density_lines(host_results, res.get("pandaos"))
    out += cost_section(summ, prices, res, published)
    out += ["", "## Caveats", ""] + [f"- {c}" for c in caveats(host_results + results) + auto_caveats(res)]
    return "\n".join(out) + "\n"


def load_published(path=os.path.join(HERE, "published.json")):
    with open(path) as f:
        return json.load(f)


# ---- workload (g): density under load -------------------------------------------------------

GUEST_VCPUS = 4
DENSITY_FIELDS = ("per_vm_runs_s", "host_runs_s", "efficiency", "cpu_per_loop", "loops_per_cpu", "host_cpus")


def density_levels(result):
    """{n: {field: summary}} pooled over the repeats of one result."""
    by_n = {}
    for rep in result.get("repeats", []):
        for lv in (rep.get("density") or {}).get("levels", []):
            for f in DENSITY_FIELDS:
                if lv.get(f) is not None:
                    by_n.setdefault(lv["n"], {}).setdefault(f, []).append(lv[f])
    return {n: {f: summarize(v) for f, v in fs.items()} for n, fs in sorted(by_n.items())}


def interpolate(points, x):
    """Linear in (x, y) points sorted by x; (y, inside_measured_range). Clamps outside the range."""
    pts = sorted(points)
    if not pts:
        return None, False
    if x <= pts[0][0]:
        return pts[0][1], x >= pts[0][0]
    for (x0, y0), (x1, y1) in zip(pts, pts[1:]):
        if x0 <= x <= x1:
            return (y0 + (y1 - y0) * (x - x0) / (x1 - x0) if x1 > x0 else y1), True
    return pts[-1][1], False


def density_projection(levels, model):
    """(VMs, CPUs, load, efficiency, inside_range) for a server from measured efficiency vs loops/CPU."""
    solo = levels.get(1, {})
    cpu = (solo.get("cpu_per_loop") or {}).get("median")
    pts = [(lv["loops_per_cpu"]["median"], lv["efficiency"]["median"]) for lv in levels.values()
           if lv.get("loops_per_cpu") and lv.get("efficiency")]
    if not cpu or not pts:
        return None
    load = model["vms"] * cpu / model["cpus"]
    # Interpolate host throughput per CPU, not per-VM efficiency: a straight line through
    # efficiency overshoots the measured plateau. Past the last point the plateau holds.
    per_cpu, inside = interpolate([(x, x * e) for x, e in pts], load)
    eff = min(pts)[1] if load <= min(pts)[0] else per_cpu / load
    return model["vms"], model["cpus"], load, eff, inside


def density_section(res, prices, published=None):
    out = []
    for side, r in res.items():
        lv = density_levels(r)
        if not lv:
            continue
        pan = side == "pandaos"
        out += ["", f"## (g) Density under load: {side} (MEASURED)", "",
                "N VMs run the Node loop at the same time from one barrier, "
                f"{'all on this one host' if pan else 'placed by the provider (per-VM numbers only)'}. "
                "Efficiency = per-VM runs/s at N / at N=1. CPU per loop = (user+sys)/real of node in the guest. "
                "median [IQR] n=samples.", "",
                "| N | per-VM runs/s | host runs/s (sum) | efficiency | CPU per loop | loops per host CPU | host CPUs |",
                "|---|---|---|---|---|---|---|"]
        for n, f in lv.items():
            host = cell(f.get("host_runs_s")) if pan else "n/a (fleet)"
            out.append(f"| {n} | {cell(f.get('per_vm_runs_s'))} | {host} | {cell(f.get('efficiency'))} | "
                       f"{cell(f.get('cpu_per_loop'))} | {cell(f.get('loops_per_cpu'))} | "
                       f"{fmt((f.get('host_cpus') or {}).get('median'))} |")
        p = (prices or {}).get(side)
        if p and not pan:
            v = (lv[max(lv)].get("per_vm_runs_s") or {}).get("median")
            if v:
                out += ["", f"Cost per 1000 Node runs at N={max(lv)} concurrent sandboxes (what each one delivers "
                        f"under load, price ${p['usd_per_hour']}/h per sandbox): "
                        f"**{p['usd_per_hour'] / 3600 * 1000 / v:.6f} USD** (MIXED CPUs unless measured on "
                        f"{p.get('cpu') or '-'})."]
        model = (p or {}).get("density_model") if pan else None
        if model:
            mode = "untrusted" if "untrusted" in str(r.get("preset", "")) else "trusted"
            same = speed_on_cpu(r, p.get("cpu"))
            out += (measured_cost_lines(lv, model, mode, prices, published) if same
                    else density_projection_lines(lv, model, mode))
    return out


def boatdev_ref(prices, published):
    """boat.dev's USD per 1000 runs at its price and its published solo speed (the CPU-consistent figure)."""
    bd = (prices or {}).get("boatdev")
    return bd["usd_per_hour"] / 3.6 / published["metrics"]["node_runs_s"]["value"] if bd and published else None


def measured_cost_lines(lv, model, mode, prices, published):
    """Cost per run from the measured host throughput at each N, when the price is this CPU's."""
    host, op, ref = model["host_usd_per_hour"], model.get("host_usd_per_hour_operator"), boatdev_ref(prices, published)
    out = ["", f"### Cost per run on {model['name']}, MEASURED (price and speed from the same CPU)", "",
           f"USD per 1000 runs = ${host:.4f}/h / 3.6 / measured host runs/s, at 100% busy, no retention"
           + (f"; + operator time: ${op:.4f}/h" if op else "") + ". "
           f"boatd admits {model[mode]['vms']} `default` VMs on it in {mode} mode (prices.json). "
           + (f"boat.dev reference: ${ref:.6f} (its price at its PUBLISHED solo speed; its speed under load is "
              "unmeasured)." if ref else ""), "",
           "| N VMs | host runs/s (measured) | USD per 1000 runs | + operator time | vs boat.dev reference |",
           "|---|---|---|---|---|"]
    for n, f in lv.items():
        h = (f.get("host_runs_s") or {}).get("median")
        if not h:
            continue
        k = host / 3.6 / h
        out.append(f"| {n} | {h:.2f} | ${k:.6f} | {f'${op / 3.6 / h:.6f}' if op else '-'} | "
                   f"{f'{k / ref:.2f}x' if ref else '-'} |")
    return out


def host_density_lines(host_results, vm_result):
    """N Node loops directly on the host vs N VMs: separates CPU saturation from virtualization overhead."""
    hl = density_levels(merged(host_results)) if host_results else {}
    if not hl:
        return []
    vl = density_levels(vm_result) if vm_result else {}
    out = ["", "## (g) Host-native density baseline vs VMs (MEASURED, same host)", "",
           "N copies of the Node loop directly on the host (no VM), against N VMs each running one. "
           "Both lose speed as the CPUs fill; the VM / host ratio at each N is what virtualization adds.", "",
           "| N | host loops: per-loop / total runs/s | efficiency | VMs: per-VM / total runs/s | efficiency | "
           "VM / host at this N |", "|---|---|---|---|---|---|"]
    med = lambda lv, n, k: ((lv.get(n) or {}).get(k) or {}).get("median")  # noqa: E731
    for n in sorted(set(hl) | set(vl)):
        h, v = med(hl, n, "per_vm_runs_s"), med(vl, n, "per_vm_runs_s")
        out.append(f"| {n} | {fmt(h)} / {fmt(med(hl, n, 'host_runs_s'))} | {fmt(med(hl, n, 'efficiency'))} | "
                   f"{fmt(v)} / {fmt(med(vl, n, 'host_runs_s'))} | {fmt(med(vl, n, 'efficiency'))} | "
                   f"{fmt(v / h) if h and v else '-'} |")
    return out


def density_projection_lines(lv, model, mode):
    m = model[mode]
    proj = density_projection(lv, m)
    if not proj:
        return []
    vms, cpus, load, eff, inside = proj
    solo = lv[1]["per_vm_runs_s"]["median"]
    host_usd = model["host_usd_per_hour"]
    top = max(f["loops_per_cpu"]["median"] for f in lv.values() if f.get("loops_per_cpu"))
    rng = ("inside the measured range" if inside else
           "ABOVE the measured range: host throughput per CPU is held flat at the highest measured load's, "
           "so efficiency = that / this load" if load > top else
           "BELOW the measured range: the lowest measured load's efficiency is used")
    short = lv[1].get("host_cpus", {}).get("median", GUEST_VCPUS) < GUEST_VCPUS
    warn = (["", f"**This efficiency is relative to N=1 on {lv[1]['host_cpus']['median']:.0f} host CPUs, fewer than the VM's "
             f"{GUEST_VCPUS} vCPUs, so N=1 is itself short of CPU and the efficiency may be flattered.**"] if short else [])
    per_k = host_usd / 3600 * 1000 / (vms * solo * eff)
    return ["", f"### PROJECTION: {model['name']}, {mode} ({vms} VMs on {cpus} CPUs), NOT A MEASUREMENT", "",
            f"Load = {vms} VMs x {lv[1]['cpu_per_loop']['median']:.2f} CPU per loop / {cpus} CPUs = "
            f"**{load:.2f} loops per CPU** ({rng}). {'Measured' if inside else 'Projected'} efficiency there: **{eff:.3f}**, so a packed host "
            f"delivers {vms} x {eff:.3f} = {vms * eff:.1f} solo-VM equivalents, and cost per run is "
            f"**{1 / eff:.2f}x** the uncontended figure.",
            f"With the measured solo speed ({solo:.2f} runs/s, a different CPU) as the per-VM speed: "
            f"${host_usd:.4f}/h / ({vms} x {solo:.2f} x {eff:.3f}) = **{per_k:.6f} USD per 1000 runs (MIXED CPUs)**. "
            "Nobody ran PandaOS on that server; the efficiency curve comes from the measured host, whose CPU, SMT "
            "layout and memory bandwidth differ."] + warn
