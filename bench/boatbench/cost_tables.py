"""Price-only cost tables for docs/cost.md, from prices.json (boatbench.py costs).

Per VM-hour by utilization, break-even against boat.dev, and stopped-VM retention under two storage policies:
the shipped one (storageOvercommit 1: every VM, running or stopped, is charged its nominal disk + memory) and a
thin-provisioning MODEL (each VM charged what it actually uses). Nothing here is a benchmark result.
"""
import json

UTILS = (0.10, 0.25, 0.50, 1.00)
SESSIONS_H = (1, 8, 24, 168)
RETAIN_H = (0, 24, 336)  # delete on stop, 1 day, boat.dev's 14 days


def monthly(s, a):
    return s["usd_per_month"] + s["setup_usd"] / a["setup_amortization_months"] + s["backup_usd_per_month"]


def operator(a):
    return a["operator_hours_per_month"] * a["operator_usd_per_hour"]


def capacity(ram_vms, slots, h, r, hours):
    """Running VM-hours per month: RAM slots, or storage slots each held for a session plus its retention."""
    return min(ram_vms * hours, slots * hours * h / (h + r))


def vm_hour_table(p):
    a, bd = p["assumptions"], p["boatdev"]["usd_per_hour"]
    hours = a["hours_per_month"]
    out = ["| Server, mode (VMs) | " + " | ".join(f"{u:.0%} util." for u in UTILS) +
           f" | + operator time, 100% | Break-even vs boat.dev (${bd:.3f}/h) |", "|---|" + "---|" * (len(UTILS) + 2)]
    for s in p["servers"]:
        m, op = monthly(s, a), operator(a)
        for label, vms in (("conservative / untrusted", s["vms_overcommit_1_0"]), ("default 1.5", s["vms_overcommit_1_5"])):
            cap = vms * hours
            be, be_op = m / bd, (m + op) / bd
            pct = lambda x: f"{x / cap:.0%}" if x <= cap else f"never ({x / cap:.0%})"  # noqa: E731
            out.append(f"| {s['name']}, {label} ({vms}) | " + " | ".join(f"${m / (cap * u):.3f}" for u in UTILS) +
                       f" | ${(m + op) / cap:.3f} | {be:,.0f} VM-h/mo, {pct(be)}; with operator time {pct(be_op)} |")
    return out


def retention_table(p, s):
    """Cost per running VM-hour at 100% of capacity (no operator time), nominal vs thin storage charging."""
    a, ret = p["assumptions"], p["assumptions"]["retention"]
    hours, m = a["hours_per_month"], monthly(s, a)
    ram_vms = s["vms_untrusted"]
    policies = [(f"storageOvercommit 1 (shipped): {ret['nominal_gib_per_vm']} GiB per VM",
                 ret["nominal_gib_per_vm"])]
    thin = ret["thin_gib_per_vm"]
    policies += [(f"MODEL, thin: {g} GiB actual use per VM ({chr(97 + i)})", g) for i, (g, _) in enumerate(thin)]
    out = [f"{s['name']}, untrusted ({ram_vms} RAM slots), store {s['store_gib']:,} GiB ({s['store_source']}). "
           "Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.", "",
           "| Storage policy | Storage slots | Session H | " + " | ".join(
               "R = 0 (delete on stop)" if r == 0 else f"R = {r // 24} day{'s' if r > 24 else ''}" for r in RETAIN_H) + " |",
           "|---|---|---|" + "---|" * len(RETAIN_H)]
    for label, gib in policies:
        slots = int(s["store_gib"] // gib)
        for h in SESSIONS_H:
            cells = []
            for r in RETAIN_H:
                c = capacity(ram_vms, slots, h, r, hours)
                cells.append(f"{c:,.0f}{' (RAM-bound)' if c == ram_vms * hours else ''}, ${m / c:.3f}/h")
            out.append(f"| {label} | {slots} | {h} h | " + " | ".join(cells) + " |")
    return out + [""] + [f"({chr(97 + i)}) {why}." for i, (_, why) in enumerate(thin)]


def render(p):
    out = ["### Per VM-hour and break-even (from prices.json)", ""] + vm_hour_table(p)
    for s in p["servers"]:
        if s.get("store_gib"):
            out += ["", f"### Stopped-VM retention: {s['name']}", ""] + retention_table(p, s)
    return "\n".join(out) + "\n"


def load(path):
    with open(path) as f:
        return json.load(f)


if __name__ == "__main__":
    import os
    p = load(os.path.join(os.path.dirname(os.path.abspath(__file__)), "prices.json"))
    md = render(p)
    assert "$0.028" in md and "Hetzner AX102, conservative / untrusted (15)" in md  # the AX102 headline row
    print(md)
