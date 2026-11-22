"""Stats, cost and the markdown report for boatbench result files."""
import json
import math
import os
import re
import statistics

import density

HERE = os.path.dirname(os.path.abspath(__file__))
SIDES = ("pandaos", "boatdev")

# key, label, unit, higher_is_better
METRICS = [
    ("node_runs_s", "(a) Node.js web-tooling geometric-mean index", "index units", True),
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
    ("rand_read_iops", "(d) disk rand read 4k, fio unpinned", "IOPS", True),
    ("rand_read_irq_cpu_iops", "(d) disk rand read 4k, fio pinned to the vCPU taking the virtio-blk IRQ", "IOPS", True),
    ("rand_read_other_cpu_iops", "(d) disk rand read 4k, fio pinned off that vCPU", "IOPS", True),
    ("pts_rand_read_iops", "(d) PTS fio job: rand read 4k, fio unpinned", "IOPS", True),
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
NET_KEYS = ("exec_rtt_p50_ms", "exec_rtt_p95_ms", "create_ready_s", "fork_ready_s", "stop_s", "resume_ready_s",
            "snapshot_s", "from_snapshot_ready_s", "concurrent10_wall_s", "concurrent8_wall_s")
DEFAULT_ROUND_TRIPS = {"create_ready_s": 2, "fork_ready_s": 2, "resume_ready_s": 2, "from_snapshot_ready_s": 2,
                       "stop_s": 1, "snapshot_s": 1, "concurrent10_wall_s": 2, "concurrent8_wall_s": 2,
                       "exec_rtt_p50_ms": 1, "exec_rtt_p95_ms": 1}
DRIVER_ROUND_TRIPS = {
    "pandaos": DEFAULT_ROUND_TRIPS,
    "boatdev": {**DEFAULT_ROUND_TRIPS, "stop_s": 2, "snapshot_s": 2},
}
_BASE = {k: (label, unit, hib) for k, label, unit, hib in METRICS}
METRICS = METRICS + [(k + "_net", _BASE[k][0] + ", NET of API round trips (raw - k x baseline p50)", _BASE[k][1],
                      _BASE[k][2]) for k in NET_KEYS]
NET_KEY_ALIASES = {
    "exec_rtt_p50_net_ms": "exec_rtt_p50_ms_net",
    "exec_rtt_p95_net_ms": "exec_rtt_p95_ms_net",
}

RUNNER_CPU = "EPYC 7763"  # GitHub's ubuntu-latest runners; published.json cpu_scaling.runner_cpu

COMMON_CAVEATS = [
    "The Node.js web-tooling geometric-mean index is the geometric mean of subtest rates (subtest runs/s), not completed Node jobs or full suites per second. host_runs_s sums VM indices; node_runs_s and *_runs_s are legacy JSON keys. Cost per 1000 index units is an index-normalized price, not a bill for 1000 completed jobs. Completed-suite rates, where available, use successful pass counts divided by the density level wall time, including synchronization and harness overhead; no vendor completed-suite rate is available.",
    "Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); "
    "boat.dev stop archives the disk and resume/fork restore it in the background after `ready` "
    "(docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.",
    "Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled "
    "to `ready`). New-from-snapshot uses each one's create-from-snapshot.",
    "Exec round-trip (`run_true`) asymmetry: native PandaOS (`/v1/vms/{id}/exec`) runs `[\"true\"]` directly via "
    "`execve`, whereas boat.dev and compat (`/api/v1/sandboxes/{id}/commands`) run `bash -lc true` and record a "
    "command. Use `--driver pandaos --compat` for the like-for-like mode.",
    "Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.",
]
NET_CAVEAT = ("Raw lifecycle latencies include the API round trip from the harness host to the provider: {where}. "
              "The NET rows subtract k x that side's measured API round-trip p50 (clamped at >= 0), where k is the "
              "recorded serial critical-path HTTP call count (`api_calls`, excluding intermediate async polls that "
              "overlap server-side work) per operation when present, falling back to `drivers.py` `ROUND_TRIPS` "
              "for older artifacts (2 for create/fork/resume/from-snapshot, stop and snapshot 1 on PandaOS and 2 on "
              "boat.dev).")
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
    pan = res.get("pandaos")
    if not isinstance(pan, dict):
        return None
    c = str(pan.get("fc_cache_type") or "").strip()
    cl = c.lower()
    host = pan.get("host") if isinstance(pan.get("host"), dict) else {}
    preset = str(pan.get("preset") or "").strip()
    pl = preset.lower()
    if cl.startswith("writeback") or host.get("unsafe_cache") is False:
        return None
    raw_opts = pan.get("boatd_options")
    if isinstance(raw_opts, str) and raw_opts.strip() not in ("", "-"):
        try:
            parsed_opts = json.loads(raw_opts)
            if isinstance(parsed_opts, dict):
                if str(parsed_opts.get("diskCache") or "").strip().lower() == "writeback":
                    return None
                if parsed_opts.get("unsafeCache") is False:
                    return None
        except ValueError:
            pass
    if cl.startswith("unsafe"):
        return c
    if host.get("unsafe_cache") is True:
        return c if c and c != "-" else "Unsafe (host metadata)"
    if pl in ("writeback", "trusted-writeback"):
        return None
    if cl.startswith("unknown"):
        label = preset if preset and preset != "-" else "boatd"
        return f"Unsafe (default for {label}; raw artifact recorded {c})"
    if preset and preset != "-":
        return f"Unsafe (default for {preset}; raw artifact recorded {c or 'unknown'})"
    return None


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


def _target_rtt_ms(result):
    rtts = [rep.get("metrics", {}).get("api_rtt_p50_ms") for rep in result.get("repeats", [])]
    valid = [float(x) for x in rtts if x is not None]
    return statistics.median(valid) if valid else None


def samples(result, key):
    """Pool one metric across repeats; intra-VM pass lists are aggregated per-VM via median first."""
    orig_key = key
    key = NET_KEY_ALIASES.get(key, key)
    out = []
    if key.endswith("_net") and key[:-4] in NET_KEYS:
        base = key[:-4]
        shared_rtt = _target_rtt_ms(result)
        default_rt = DRIVER_ROUND_TRIPS.get(result.get("driver"), DEFAULT_ROUND_TRIPS).get(base, 1)
        for rep in result.get("repeats", []):
            m = rep.get("metrics", {})
            calls_map = rep.get("api_calls") if isinstance(rep.get("api_calls"), dict) else {}
            sample_calls = calls_map.get(base)
            if sample_calls is None:
                sample_calls = m.get(f"{base}_calls", rep.get(f"{base}_calls"))
            rtt = m.get("api_rtt_p50_ms")
            if rtt is None:
                rtt = shared_rtt
            val = None
            if sample_calls is not None and m.get(base) is not None and rtt is not None:
                val = m[base] - sample_calls * (rtt if base.endswith("_ms") else rtt / 1000)
            elif m.get(key) is not None:
                val = m[key]
            elif m.get(orig_key) is not None:
                val = m[orig_key]
            elif m.get(base) is not None and rtt is not None and default_rt is not None:
                val = m[base] - default_rt * (rtt if base.endswith("_ms") else rtt / 1000)
            if val is not None:
                out.append(max(0.0, float(val)))
        return out

    for rep in result.get("repeats", []):
        v = rep.get("metrics", {}).get(key)
        if isinstance(v, list):
            vals = [x for x in v if x is not None]
            if vals:
                out.append(statistics.median(vals))
        elif v is not None:
            out.append(v)
    return out


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
            if isinstance(p.get("sizes"), dict):
                prices[side]["sizes"] = p["sizes"]
            if isinstance(p.get("density_model"), dict):
                prices[side]["density_model"] = p["density_model"]
    return prices or None


def shape_price(side, price, result):
    """Use the recorded shape. PandaOS admission counts model only `default`."""
    if not price or not result:
        return None
    size = result.get("size")
    if not isinstance(size, str) or not size:
        return None
    if side == "pandaos" and size != "default":
        return None
    selected = dict(price, size=size)
    if side == "boatdev":
        rates = price.get("sizes", {})
        rate = rates.get(size, price.get("usd_per_hour") if size == "default" else None)
        if (not isinstance(rate, (int, float)) or isinstance(rate, bool)
                or not math.isfinite(rate) or rate < 0):
            return None
        selected["usd_per_hour"] = rate
    return selected


def cost_rows(summaries, prices):
    """USD per 1000 index units and per cold npm ci, from price x measured time."""
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
    selected = {side: shape_price(side, (prices or {}).get(side), r) for side, r in res.items()}
    for side, r in res.items():
        if (prices or {}).get(side) and not selected[side]:
            out += [f"{side}: cost suppressed for recorded size `{r.get('size')}`: no supported shape price/admission model.", ""]
    rows = cost_rows(summ, selected)
    if not rows:
        return out + ["No supported shape price: cost not computed, index units only."]
    out += ["**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. "
            "Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS "
            "figure.", "",
            "Index-normalized USD per 1000 index units = hourly price / 3.6 / index; npm cost = hourly price × seconds / 3600. An index-normalized cost only means something when the "
            "price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost "
            "of any real machine.**", "",
            "| Side | USD/hour | The price buys | Speed measured on | USD per 1000 index units | USD per cold npm ci |",
            "|---|---|---|---|---|---|"]
    for side, p, per_k, per_npm in rows:
        measured = cpu_of(res[side]) if side in res else "-"
        mixed = side not in res or not speed_on_cpu(res[side], p.get("cpu"))
        if side in res and side == "pandaos":
            measured += f" (guest); harness host {res[side].get('client', {}).get('cpu_model', '-')}"
        tag = " (MIXED CPUs)" if mixed else ""
        out.append(f"| {side} | {p['usd_per_hour']:.4f} | {p.get('cpu') or '-'}; `{p['size']}` VM | {measured} | "
                   f"{'-' if per_k is None else f'{per_k:.6f}{tag}'} | {'-' if per_npm is None else f'{per_npm:.6f}{tag}'} |")
    bd = (prices or {}).get("boatdev")
    if bd and published:
        v = published["metrics"]["node_runs_s"]["value"]
        out += ["", f"CPU-consistent figure: boat.dev's `default` VM price with boat.dev's own published speed on the CPU it "
                f"sells ({v} index units, vendor-measured, {published['rows'][0]['cpu']}): "
                f"**{bd['usd_per_hour'] / 3600 * 1000 / v:.6f} USD per 1000 index units**. PandaOS's price here is "
                f"{(prices.get('pandaos') or {}).get('cpu') or '-'}: its index-normalized cost is CPU-consistent only where "
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


POOL_IGNORED_KEYS = frozenset({"repeats", "run_id", "started", "run_url", "parts"})
POOL_CHECKED_KEYS = (
    "fc_cache_type",
    "driver",
    "size",
    "preset",
    "boatd_options",
    "host",
    "client",
    "source_commit",
)


def _canonical_json(value):
    try:
        return ("json", json.dumps(value, sort_keys=True, separators=(",", ":")))
    except TypeError:
        return ("repr", type(value).__qualname__, repr(value))


def _norm_fc_cache_type(res):
    raw = res.get("fc_cache_type", "-")
    if raw is None:
        return ("none", None, False)
    if not isinstance(raw, str):
        return ("other", _canonical_json(raw), False)
    s = raw.strip()
    mode = s.split(" (", 1)[0].strip() if " (" in s and s.endswith(")") else s
    return ("str", mode, bool(unsafe_cache({"pandaos": {"fc_cache_type": raw}})))


def _norm_boatd_options(res):
    raw = res.get("boatd_options", "-")
    if raw is None:
        return ("none", None)
    if isinstance(raw, str):
        s = raw.strip()
        if not s or s == "-":
            return ("empty", "-")
        try:
            parsed = json.loads(s)
        except ValueError:
            return ("raw", s)
        return ("json", _canonical_json(parsed))
    return ("json", _canonical_json(raw))


def _norm_pool_field(res, key):
    if key == "fc_cache_type":
        return _norm_fc_cache_type(res)
    if key == "boatd_options":
        return _norm_boatd_options(res)
    if key in ("size", "preset"):
        return _canonical_json(res.get(key, "-"))
    if key in ("driver", "host", "client", "source_commit"):
        return _canonical_json(res.get(key))
    return ("present", _canonical_json(res[key])) if key in res else ("missing",)


WARM_CREATE_THRESHOLD_S = 0.65


def _warm_pool_on(res):
    raw = res.get("boatd_options", "")
    explicit = None
    if isinstance(raw, dict):
        if "warmPool" in raw:
            wp = raw["warmPool"]
            explicit = wp is True or (isinstance(wp, (int, float)) and not isinstance(wp, bool) and wp > 0)
    elif isinstance(raw, str):
        try:
            parsed = json.loads(raw)
        except ValueError:
            compact = re.sub(r"\s+", "", raw)
            if '"warmPool":true' in compact:
                explicit = True
            elif '"warmPool":false' in compact or '"warmPool":0' in compact:
                explicit = False
        else:
            if isinstance(parsed, dict) and "warmPool" in parsed:
                wp = parsed["warmPool"]
                explicit = wp is True or (isinstance(wp, (int, float)) and not isinstance(wp, bool) and wp > 0)
    if explicit is not None:
        return explicit

    host = res.get("host") if isinstance(res.get("host"), dict) else {}
    if "warm_pool" in host and host["warm_pool"] is not None:
        wp = host["warm_pool"]
        if isinstance(wp, bool):
            return wp
        if isinstance(wp, (int, float)):
            return wp > 0

    preset = str(res.get("preset") or "").strip()
    if preset in ("trusted", "trusted-warm", "pandaos-install"):
        creates = samples(res, "create_ready_s")
        if creates and statistics.median(creates) < WARM_CREATE_THRESHOLD_S:
            return True
    return False


_WORKLOAD_METRICS = {
    "a": ("node_runs_s",),
    "b": ("exec_rtt_p50_ms", "fork_ready_s", "stop_s", "resume_ready_s", "snapshot_s", "from_snapshot_ready_s"),
    "d": ("seq_write_mbps", "seq_read_mbps", "rand_write_iops", "rand_read_iops", "pts_rand_read_iops",
          "fsync_rand_write_iops"),
    "e": ("dl_go_tarball_googlecdn_mbit", "dl_nodejs_tarball_mbit",
          "tcp_connect_cloudflare_1.1.1.1_ms", "tcp_connect_github.com_ms"),
    "f": ("npm_ci_s",),
}


def repeat_count_str(result):
    """Per-workload repeat count across workloads (a..g) so disjoint split runs do not sum repeats."""
    reps = result.get("repeats", [])
    counts = {}
    for rep in reps:
        m = rep.get("metrics") or {}
        for part, keys in _WORKLOAD_METRICS.items():
            for k in keys:
                v = m.get(k)
                if isinstance(v, list):
                    if any(x is not None for x in v):
                        counts[part] = counts.get(part, 0) + 1
                        break
                elif v is not None:
                    counts[part] = counts.get(part, 0) + 1
                    break
        if any(k.startswith("concurrent") and k.endswith("_wall_s") and m.get(k) is not None for k in m):
            counts["c"] = counts.get("c", 0) + 1
        if rep.get("density"):
            counts["g"] = counts.get("g", 0) + 1
    if not counts:
        return str(len(reps))
    lo, hi = min(counts.values()), max(counts.values())
    return str(lo) if lo == hi else f"{lo}-{hi}"


def by_side(results):
    out = {}
    for r in results:
        out.setdefault(r["driver"], []).append(r)
    return out


def merged(results):
    """Several result files of one side are pooled as more repeats."""
    if not results:
        raise ValueError("cannot merge empty results")
    first = results[0]
    for other in results[1:]:
        extra = sorted((set(first) | set(other)) - POOL_IGNORED_KEYS - set(POOL_CHECKED_KEYS))
        diffs = [k for k in (*POOL_CHECKED_KEYS, *extra)
                 if _norm_pool_field(first, k) != _norm_pool_field(other, k)]
        if diffs:
            detail = ", ".join(f"{k}: {first.get(k)!r} vs {other.get(k)!r}" for k in diffs)
            msg = f"cannot pool incompatible benchmark results ({detail})".replace("\n", "\\n").replace("\r", "\\r")
            raise ValueError(msg)
    if len(results) == 1:
        return first
    m = dict(first)
    m["repeats"] = [rep for r in results for rep in r.get("repeats", [])]
    return m


def auto_caveats(res):
    out = []
    cache = unsafe_cache(res)
    if cache:
        out.append(f"Firecracker drive `cache_type` was `{cache}`: guest flushes (`fsync` / `O_DIRECT`) stop at the "
                   "host page cache and are not durable-write numbers (`diskCache: Writeback` enables durable flushes).")
    if any(_warm_pool_on(r) for r in res.values()):
        out.append(WARM_POOL_CAVEAT)
    probes = {s: [rep.get("probe") or {} for rep in r.get("repeats", [])] for s, r in res.items()}
    for side, ps in probes.items():
        for p in ps:
            if p and (p.get("nproc") != 4 or not p.get("mem_mib") or abs(p["mem_mib"] - 8192) > 0.1 * 8192):
                out.append(f"{side}: a VM reported {p.get('nproc')} vCPU / {p.get('mem_mib')} MiB, not 4 / ~8192.")
                break
    tools = {
        s: sorted({t for rep in r.get("repeats", []) for t in [rep.get("disk_tool")] if t and t != "-"})
        for s, r in res.items()
    }
    if len({t for ts in tools.values() for t in ts}) > 1:
        out.append(f"Disk tool differs between sides: {tools}. dd numbers are not comparable with fio.")
    for side, r in res.items():
        for rep in r.get("repeats", []):
            for kind in ("concurrent_cleanup", "density_cleanup"):
                cleanup = rep.get(kind) or {}
                if cleanup.get("errors"):
                    out.append(f"{side}: r{rep.get('index')}: {kind}: unresolved VM cleanup: {cleanup['errors']}.")
        errs = [f"r{rep.get('index')}:{k}" for rep in r.get("repeats", []) for k in (rep.get("errors") or {})]
        if errs:
            out.append(f"{side}: failed steps (see raw JSON): {', '.join(errs[:20])}.")
        skipped = [f"r{rep.get('index')}:{k} ({reason})"
                   for rep in r.get("repeats", [])
                   for k, reason in (rep.get("skipped") or {}).items()]
        if skipped:
            out.append(f"{side}: skipped steps (not passed): {', '.join(skipped[:20])}.")
    return out + density_caveats(res)


def density_caveats(res):
    out = []
    for side, r in res.items():
        for rep in r.get("repeats", []):
            trial = rep.get("density") or {}
            cleanup = trial.get("cleanup_errors") or {}
            if cleanup:
                out.append(f"{side}: r{rep.get('index')}: unresolved VM cleanup: {cleanup}.")
            rejected = [str(lv.get("n")) for lv in trial.get("levels", []) if not density_trial_complete(lv, trial)]
            if rejected:
                out.append(f"{side}: r{rep.get('index')}: incomplete density excluded at n={', n='.join(rejected)} "
                           "from throughput, efficiency, cost and projections.")
            if trial.get("errors"):
                evidence = "; ".join(str(e).replace("\n", " ") for e in trial["errors"])
                out.append(f"{side}: r{rep.get('index')}: density failures (see raw JSON): {evidence}.")
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
        ("Repeats (fresh VMs)", repeat_count_str),
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
    """(method, index units) estimates of PandaOS on the vendor's CPU. Each one is an ESTIMATE."""
    sc = published["cpu_scaling"]
    ob, pm = sc["same_benchmark"], sc["passmark_single_thread"]
    vendor = published["metrics"]["node_runs_s"]["value"]
    rows = [(f"vendor's VM score x our VM/host: {vendor} x {overhead:.3f}. A floor if the vendor's VM also "
             f"has overhead, because its host then scores above {vendor}", vendor * overhead),
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
            f"Estimated Node loop range: **{min(vals):.1f}-{max(vals):.1f} index units**.", "",
            "| Method | Projected index units (not measured) |", "|---|---|"]
    out += [f"| {m} | {v:.1f} |" for m, v in rows]
    where = ("a shared, nested Azure VM (a GitHub runner), and it may not transfer to a dedicated host" if runner
             else "this host (see the Host line above)")
    lim = (sc["cpu_ratio_limits"] + " " if len(rows) > 2 else "") + sc["limits"]
    out += ["", f"Limits: the VM/host ratio ({overhead:.3f}) was measured on {where}. {lim}", "",
            f"Same-benchmark source: {sc['same_benchmark']['source']}. "
            f"PassMark source: {sc['passmark_single_thread']['source']}."]
    return out


PRESET_METRICS = (
    "node_runs_s", "api_rtt_p50_ms", "create_ready_s", "exec_rtt_p50_ms", "exec_rtt_p95_ms",
    "fork_ready_s", "stop_s", "resume_ready_s", "snapshot_s", "from_snapshot_ready_s",
    "concurrent10_wall_s", "concurrent8_wall_s", "seq_write_mbps", "seq_read_mbps",
    "rand_write_iops", "rand_read_iops", "rand_read_irq_cpu_iops", "rand_read_other_cpu_iops",
    "pts_rand_write_iops", "pts_seq_write_mibps", "pts_rand_read_iops",
    "fsync_rand_write_iops", "seq_write_endfsync_mbps",
    "dl_go_tarball_googlecdn_mbit", "npm_ci_s",
)


def _metrics_for(res):
    """METRICS plus any dynamic concurrent{N}_wall_s / _net keys present in res."""
    known = {k for k, *_ in METRICS}
    extra_base = []
    extra_net = []
    seen = set()
    for r in res.values():
        for rep in r.get("repeats", []):
            for k in (rep.get("metrics") or {}):
                m = re.fullmatch(r"concurrent(\d+)_wall_s", k)
                if m and k not in known and k not in seen:
                    seen.add(k)
                    n = int(m.group(1))
                    label = f"(c) {n} concurrent creates, wall to all exec-ready"
                    extra_base.append((k, label, "s", False))
                    extra_net.append((k + "_net", label + ", NET of API round trips (raw - k x baseline p50)", "s", False))
    if not extra_base:
        return METRICS
    return list(METRICS) + extra_base + extra_net


def render_presets(results):
    """PandaOS medians side by side for each boatd preset (one runner per preset)."""
    groups = {}
    for r in results:
        if r["driver"] == "pandaos":
            groups.setdefault(r.get("preset", "-"), []).append(r)
    res = {k: merged(v) for k, v in sorted(groups.items())}
    preset_caches = {k: unsafe_cache({"pandaos": r}) for k, r in res.items()}
    all_metrics = _metrics_for(res)
    labels = {k: (label, unit, hib) for k, label, unit, hib in all_metrics}
    extra_preset_keys = [k for k, *_ in all_metrics if k not in PRESET_METRICS and k.startswith("concurrent") and k.endswith("_wall_s")]
    where = ("Each column ran on its own GitHub runner (shared, nested KVM, not a dedicated server), so runner-to-"
             "runner variance is in the differences too." if on_runner(results) else
             "Each column is a separate run on the harness-declared host(s) below, so run-to-run variance is in the "
             "differences too.")
    out = ["# boatbench: PandaOS by boatd preset (MEASURED)", "", where + " median [IQR] n=samples.", "",
           "| Metric | Unit | Better | " + " | ".join(res) + " |", "|---|---|---|" + "---|" * len(res)]
    marked_unsafe = False
    for key in (*PRESET_METRICS, *extra_preset_keys):
        raw_cells = [(p, cell(summarize(samples(r, key)))) for p, r in res.items()]
        if all(c == "-" for _, c in raw_cells):
            continue
        label, unit, hib = labels[key]
        if key in WRITE_METRICS:
            cells = []
            row_marked = False
            for p, c in raw_cells:
                if c != "-" and preset_caches.get(p):
                    cells.append(c + " †")
                    row_marked = True
                else:
                    cells.append(c)
            if row_marked:
                label = label + " †"
                marked_unsafe = True
        else:
            cells = [c for _, c in raw_cells]
        out.append(f"| {label} | {unit} | {'higher' if hib else 'lower'} | " + " | ".join(cells) + " |")
    unsafe_presets = [(p, c) for p, c in preset_caches.items() if c]
    if unsafe_presets and (marked_unsafe or len(unsafe_presets) > 0):
        unique_modes = list(dict.fromkeys(c for _, c in unsafe_presets))
        names = ", ".join(p for p, _ in unsafe_presets if p != "-")
        cache_desc = unique_modes[0] if len(unique_modes) == 1 else "; ".join(f"{p}: {c}" for p, c in unsafe_presets)
        if names and len(unsafe_presets) < len(res):
            cache_desc = f"{cache_desc} (presets: {names})"
        out += ["", unsafe_note(cache_desc)]
    out += ["", "| | " + " | ".join(res) + " |", "|---|" + "---|" * len(res)]
    for name, f in (("boatd options", lambda _p, r: r.get("boatd_options", "-")),
                    ("Firecracker cache_type", lambda p, r: (
                        preset_caches[p]
                        if preset_caches.get(p) and str(r.get("fc_cache_type") or "-").strip().lower() in ("-", "unknown")
                        or (preset_caches.get(p) and str(r.get("fc_cache_type") or "").strip().lower().startswith("unknown"))
                        else r.get("fc_cache_type", "-")
                    )),
                    ("VM vCPU seen", lambda _p, r: next((str(rep["probe"].get("nproc")) for rep in r.get("repeats", [])
                                                         if rep.get("probe")), "-")),
                    ("Failed steps", lambda _p, r: ", ".join(f"r{rep.get('index')}:{k}" for rep in r.get("repeats", [])
                                                             for k in (rep.get("errors") or {}))[:300] or "none"),
                    ("Host (harness-declared)", lambda _p, r: r.get("host", {}).get("desc", "-")),
                    ("Run URL", lambda _p, r: r.get("run_url") or "-")):
        out.append(f"| {name} | " + " | ".join(str(f(p, r)).replace("|", "/") for p, r in res.items()) + " |")
    return "\n".join(out) + "\n"


def render(results, prices=None, published=None):
    host_results = [r for r in results if r["driver"] == "host"]
    results = [r for r in results if r["driver"] != "host"]
    if host_results:
        merged(host_results)
    res = {s: merged(rs) for s, rs in by_side(results).items()}
    all_metrics = _metrics_for(res)
    summ = {s: {k: summarize(samples(r, k)) for k, *_ in all_metrics} for s, r in res.items()}
    out = ["# boatbench: PandaOS boat vs boat.dev", "",
           "Measured numbers below come from this harness (`bench/boatbench`). Published numbers are "
           "boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.", "",
           "## Sides: hardware and location", ""]
    out += side_table(res)
    out += ["", "## Measured results", "",
            "median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.", ""]
    if "boatdev" not in res:
        vendor_speed = f"{published['metrics']['node_runs_s']['value']} index units" if published else "published index units"
        out += ["**boat.dev was not measured by this harness (no `BOATDEV_API_KEY`), so no row has a measured-vs-"
                "measured verdict.** The only boat.dev numbers are the vendor's published claims (next section), and "
                f"its {vendor_speed} is one solo VM, not a fully packed server.", ""]
    out += [
            "| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |",
            "|---|---|---|---|---|---|---|"]
    cache = unsafe_cache(res)
    for key, label, unit, hib in all_metrics:
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
            out += ["", "The vendor's whole table (VENDOR-MEASURED, not by this harness). The CPU column is what the "
                    "vendor's page or run reported, so these rows differ in hardware as well as software.", "",
                    "| Provider | index units | CPU |", "|---|---|---|"]
            out += [f"| {r['provider']} | {r['runs_s']} | {r['cpu']} |" for r in published["rows"]]
    if host_results and "pandaos" in res:
        out += normalization(host_results, res["pandaos"], published, on_runner(host_results + results))
    out += density_section(res, prices, published)
    out += host_density_lines(host_results, res.get("pandaos"))
    out += cost_section(summ, prices, res, published)
    out += ["", "## Caveats", ""] + [f"- {c}" for c in caveats(host_results + results) + auto_caveats(res) + density_caveats({"host": merged(host_results)} if host_results else {})]
    return "\n".join(out) + "\n"


def load_published(path=os.path.join(HERE, "published.json")):
    with open(path) as f:
        return json.load(f)


# ---- workload (g): density under load -------------------------------------------------------

GUEST_VCPUS = 4
DENSITY_FIELDS = ("per_vm_runs_s", "host_runs_s", "efficiency", "cpu_per_loop", "loops_per_cpu", "host_cpus")


def density_trial_complete(lv, trial):
    # Older collectors kept planned N even after a worker failed. Setup refusals
    # ("vm i:") do not invalidate smaller, fully measured levels.
    return density.level_complete(lv) and not any(
        str(e).startswith(f"n={lv.get('n')}:") for e in trial.get("errors", []))


def density_levels(result):
    """{n: {field: summary}} pooled over the repeats of one result."""
    by_n = {}
    for rep in result.get("repeats", []):
        trial = rep.get("density") or {}
        levels = trial.get("levels", [])
        solo_ok = any(lv.get("n") == 1 and density_trial_complete(lv, trial) for lv in levels)
        for lv in levels:
            if not density_trial_complete(lv, trial):
                continue
            counts = lv.get("pass_counts")
            required = lv.get("passes_required", trial.get("passes"))
            wall = lv.get("wall_s")
            if (isinstance(required, int) and not isinstance(required, bool) and required > 0
                    and (counts == [required] * lv["n"] or counts is None and trial.get("passes") == required)
                    and density.positive_finite(wall)):
                by_n.setdefault(lv["n"], {}).setdefault("completed_suites_s", []).append(lv["n"] * required / wall)
            for f in DENSITY_FIELDS:
                if f in ("efficiency", "loops_per_cpu") and not solo_ok:
                    continue
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
    # Interpolate aggregate host index per CPU, not per-VM efficiency: a straight line through
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
                "Efficiency = per-VM index units at N / at N=1. CPU per loop = (user+sys)/real of node in the guest. "
                "median [IQR] n=samples. The index is not completed-job throughput. Completed suites/s is total successful full-suite passes divided by level wall time, including harness overhead; no vendor counterpart is published.", "",
                "| N | per-VM index units | host index units (sum) | efficiency | CPU per loop | loops per host CPU | host CPUs | completed suites/s (measured) |",
                "|---|---|---|---|---|---|---|---|"]
        for n, f in lv.items():
            host = cell(f.get("host_runs_s")) if pan else "n/a (fleet)"
            out.append(f"| {n} | {cell(f.get('per_vm_runs_s'))} | {host} | {cell(f.get('efficiency'))} | "
                       f"{cell(f.get('cpu_per_loop'))} | {cell(f.get('loops_per_cpu'))} | "
                       f"{fmt((f.get('host_cpus') or {}).get('median'))} | {cell(f.get('completed_suites_s'))} |")
        p = shape_price(side, (prices or {}).get(side), r)
        if p and not pan:
            v = (lv[max(lv)].get("per_vm_runs_s") or {}).get("median")
            if v:
                out += ["", f"Cost per 1000 index units at N={max(lv)} concurrent sandboxes (what each one delivers "
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
    """boat.dev's USD per 1000 index units at its price and its published solo speed (the CPU-consistent figure)."""
    bd = (prices or {}).get("boatdev")
    return bd["usd_per_hour"] / 3.6 / published["metrics"]["node_runs_s"]["value"] if bd and published else None


def measured_cost_lines(lv, model, mode, prices, published):
    """Cost per index unit from the measured aggregate host index at each N, when the price is this CPU's."""
    host, op, ref = model["host_usd_per_hour"], model.get("host_usd_per_hour_operator"), boatdev_ref(prices, published)
    out = ["", f"### Cost per index unit on {model['name']}, MEASURED (price and speed from the same CPU)", "",
           f"USD per 1000 index units = ${host:.4f}/h / 3.6 / measured host index units, at 100% busy, no retention"
           + (f"; + operator time: ${op:.4f}/h" if op else "") + ". "
           f"boatd admits {model[mode]['vms']} `default` VMs on it in {mode} mode (prices.json). "
           + (f"boat.dev reference: ${ref:.6f} (its price at its PUBLISHED solo speed; its speed under load is "
              "unmeasured)." if ref else ""), "",
           "| N VMs | host index units (measured) | USD per 1000 index units | + operator time | vs boat.dev reference |",
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
    """Compare density curves only when recorded host and CPU layouts agree."""
    host_result = merged(host_results) if host_results else None
    hl = density_levels(host_result) if host_result else {}
    if not hl:
        return []
    vl = density_levels(vm_result) if vm_result else {}
    def known(value):
        return value not in (None, "", "-", "unknown")
    reasons = []
    for key, field in (("host", "desc"), ("client", "cpu_model"), ("client", "os")):
        h = (host_result.get(key) or {}).get(field)
        v = ((vm_result or {}).get(key) or {}).get(field)
        if not known(h) or not known(v) or h != v:
            reasons.append(f"{key}.{field} differs or is missing")
    def cpu_counts(result):
        return [lv.get("host_cpus") for rep in (result or {}).get("repeats", [])
                for lv in (rep.get("density") or {}).get("levels", []) if density.level_complete(lv)]
    hc, vc = cpu_counts(host_result), cpu_counts(vm_result)
    if (not hc or not vc or not all(density.positive_finite(x) for x in hc + vc)
            or len(set(hc + vc)) != 1):
        reasons.append("recorded density host CPU counts differ or are missing")
    matched = not reasons
    label = "MEASURED, same host" if matched else "UNMATCHED host/layout evidence"
    note = ("Recorded host descriptions, client CPUs/OS and density CPU counts match. The ratio includes guest, "
            "VMM and policy effects; these runs do not isolate an individual source of overhead." if matched else
            "VM / host overhead is not attributable: " + "; ".join(reasons) + ". Ratios are suppressed.")
    out = ["", f"## (g) Host-native density baseline vs VMs ({label})", "",
           "N copies of the Node loop directly on the host (no VM), against N VMs each running one. " + note, "",
           "| N | host loops: per-loop / total index units | efficiency | VMs: per-VM / total index units | efficiency | "
           "VM / host at this N |", "|---|---|---|---|---|---|"]
    med = lambda lv, n, k: ((lv.get(n) or {}).get(k) or {}).get("median")  # noqa: E731
    for n in sorted(set(hl) | set(vl)):
        h, v = med(hl, n, "per_vm_runs_s"), med(vl, n, "per_vm_runs_s")
        out.append(f"| {n} | {fmt(h)} / {fmt(med(hl, n, 'host_runs_s'))} | {fmt(med(hl, n, 'efficiency'))} | "
                   f"{fmt(v)} / {fmt(med(vl, n, 'host_runs_s'))} | {fmt(med(vl, n, 'efficiency'))} | "
                   f"{fmt(v / h) if matched and h and v else '-'} |")
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
           "ABOVE the measured range: aggregate host index per CPU is held flat at the highest measured load's, "
           "so efficiency = that / this load" if load > top else
           "BELOW the measured range: the lowest measured load's efficiency is used")
    short = lv[1].get("host_cpus", {}).get("median", GUEST_VCPUS) < GUEST_VCPUS
    warn = (["", f"**This efficiency is relative to N=1 on {lv[1]['host_cpus']['median']:.0f} host CPUs, fewer than the VM's "
             f"{GUEST_VCPUS} vCPUs, so N=1 is itself short of CPU and the efficiency may be flattered.**"] if short else [])
    per_k = host_usd / 3600 * 1000 / (vms * solo * eff)
    return ["", f"### PROJECTION: {model['name']}, {mode} ({vms} VMs on {cpus} CPUs), NOT A MEASUREMENT", "",
            f"Load = {vms} VMs x {lv[1]['cpu_per_loop']['median']:.2f} CPU per loop / {cpus} CPUs = "
            f"**{load:.2f} loops per CPU** ({rng}). {'Measured' if inside else 'Projected'} efficiency there: **{eff:.3f}**, so a packed host "
            f"delivers {vms} x {eff:.3f} = {vms * eff:.1f} solo-VM equivalents, and cost per index unit is "
            f"**{1 / eff:.2f}x** the uncontended figure.",
            f"With the measured solo speed ({solo:.2f} index units, a different CPU) as the per-VM speed: "
            f"${host_usd:.4f}/h / ({vms} x {solo:.2f} x {eff:.3f}) = **{per_k:.6f} USD per 1000 index units (MIXED CPUs)**. "
            "Nobody ran PandaOS on that server; the efficiency curve comes from the measured host, whose CPU, SMT "
            "layout and memory bandwidth differ."] + warn
