"""Stats, cost and the markdown report for boatbench result files."""
import json
import os
import statistics

HERE = os.path.dirname(os.path.abspath(__file__))
SIDES = ("pandaos", "boatdev")

# key, label, unit, higher_is_better
METRICS = [
    ("node_runs_s", "(a) Node.js build/test loop: PTS node-web-tooling", "runs/s", True),
    ("create_ready_s", "(b) create -> exec-ready", "s", False),
    ("exec_rtt_p50_ms", "(b) exec round trip `true`, p50 of 50", "ms", False),
    ("exec_rtt_p95_ms", "(b) exec round trip `true`, p95 of 50", "ms", False),
    ("fork_ready_s", "(b) fork -> fork exec-ready", "s", False),
    ("stop_s", "(b) stop (until stopped)", "s", False),
    ("resume_ready_s", "(b) resume -> exec-ready", "s", False),
    ("snapshot_s", "(b) snapshot (until usable)", "s", False),
    ("from_snapshot_ready_s", "(b) new-from-snapshot -> exec-ready", "s", False),
    ("concurrent10_wall_s", "(c) 10 concurrent creates, wall to all exec-ready", "s", False),
    ("seq_write_mbps", "(d) disk seq write 1M", "MB/s", True),
    ("seq_read_mbps", "(d) disk seq read 1M", "MB/s", True),
    ("rand_write_iops", "(d) disk rand write 4k", "IOPS", True),
    ("rand_read_iops", "(d) disk rand read 4k", "IOPS", True),
    ("dl_go_tarball_googlecdn_mbit", "(e) download Go 1.25.0 tarball, 59.7 MB (dl.google.com)", "Mbit/s", True),
    ("dl_nodejs_tarball_mbit", "(e) download Node.js 22.22.3 tarball, ~30 MB (nodejs.org)", "Mbit/s", True),
    ("tcp_connect_cloudflare_1.1.1.1_ms", "(e) TCP connect 1.1.1.1, median of 10", "ms", False),
    ("tcp_connect_github.com_ms", "(e) TCP connect github.com, median of 10", "ms", False),
    ("npm_ci_s", "(f) cold `npm ci` of pinned project", "s", False),
]

CAVEATS = [
    "The PandaOS numbers from CI come from a GitHub-hosted runner, NOT a dedicated server: shared 4 vCPU, "
    "KVM exposed to the runner VM (Firecracker runs nested inside Azure's hypervisor), US region. "
    "boat.dev runs in the EU (Germany/Finland/France). Comparisons are indicative until "
    "`run-on-server.sh` runs on the owner's dedicated server.",
    "On the runner, each PandaOS VM (4 vCPU / 8 GiB) gets the whole 4-vCPU host, and the 10-VM concurrency test "
    "oversubscribes it (boatd memOvercommit/cpuOvercommit raised just so it fits). boat.dev places VMs on its fleet.",
    "Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); "
    "boat.dev stop archives the disk and resume/fork restore it in the background after `ready` "
    "(docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.",
    "Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled "
    "to `ready`). New-from-snapshot uses each one's create-from-snapshot.",
    "Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.",
    "Lifecycle latencies include the API round trip from the harness host to the provider: the CI PandaOS job "
    "talks to localhost, the boat.dev job talks to boat.dev over the internet from a US runner.",
    "Small samples (default 5 fresh VMs per side). The table shows median and IQR; overlapping IQRs mean "
    "no clear winner.",
]


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


def load_prices(path):
    if not path or not os.path.exists(path):
        return None
    with open(path) as f:
        raw = json.load(f)
    prices = {}
    for side in SIDES:
        p = raw.get(side)
        if isinstance(p, dict) and isinstance(p.get("usd_per_hour"), (int, float)) and p["usd_per_hour"] >= 0:
            prices[side] = {"usd_per_hour": float(p["usd_per_hour"]), "source": p.get("source", "")}
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
    probes = {s: [rep.get("probe") or {} for rep in r.get("repeats", [])] for s, r in res.items()}
    for side, ps in probes.items():
        for p in ps:
            if p and (p.get("nproc") != 4 or not p.get("mem_mib") or abs(p["mem_mib"] - 8192) > 0.1 * 8192):
                out.append(f"{side}: a VM reported {p.get('nproc')} vCPU / {p.get('mem_mib')} MiB, not 4 / ~8192.")
                break
    tools = {s: {(rep.get("disk_tool") or "-") for rep in r.get("repeats", [])} for s, r in res.items()}
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
    ]
    for label, f in rows:
        lines.append(f"| {label} | " + " | ".join(str(f(r)) for r in res.values()) + " |")
    return lines


def render(results, prices=None, published=None):
    res = {s: merged(rs) for s, rs in by_side(results).items()}
    summ = {s: {k: summarize(samples(r, k)) for k, *_ in METRICS} for s, r in res.items()}
    out = ["# boatbench: PandaOS boat vs boat.dev", "",
           "Measured numbers below come from this harness (`bench/boatbench`). Published numbers are "
           "boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.", "",
           "## Sides: hardware and location", ""]
    out += side_table(res)
    out += ["", "## Measured results", "",
            "median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.", "",
            "| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |",
            "|---|---|---|---|---|---|---|"]
    for key, label, unit, hib in METRICS:
        pan, bd = summ.get("pandaos", {}).get(key), summ.get("boatdev", {}).get(key)
        if not pan and not bd:
            continue
        out.append(f"| {label} | {unit} | {'higher' if hib else 'lower'} | {cell(pan)} | {cell(bd)} | "
                   f"{ratio(pan, bd)} | {verdict(pan, bd, hib)} |")
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
        if published.get("others"):
            out += ["", "Other providers on the same published leaderboard (runs/s, vendor-measured): " +
                    ", ".join(f"{k} {v}" for k, v in published["others"].items()) + "."]
    out += ["", "## Cost", ""]
    rows = cost_rows(summ, prices)
    if not rows:
        out.append("No `prices.json` with `usd_per_hour` per side: cost not computed, runs/s only.")
    else:
        out += ["Cost = side's price per hour (prices.json) x measured time. Per 1000 Node loop runs uses "
                "1000 / runs_s seconds of one 4 vCPU / 8 GiB VM.", "",
                "| Side | USD/hour | Price source | USD per 1000 Node loop runs | USD per cold npm ci |",
                "|---|---|---|---|---|"]
        for side, p, per_k, per_npm in rows:
            out.append(f"| {side} | {p['usd_per_hour']:.4f} | {p['source']} | "
                       f"{'-' if per_k is None else f'{per_k:.6f}'} | {'-' if per_npm is None else f'{per_npm:.6f}'} |")
    out += ["", "## Caveats", ""] + [f"- {c}" for c in CAVEATS + auto_caveats(res)]
    return "\n".join(out) + "\n"


def load_published(path=os.path.join(HERE, "published.json")):
    with open(path) as f:
        return json.load(f)
