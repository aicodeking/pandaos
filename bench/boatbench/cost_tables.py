"""Price-only cost tables for docs/cost.md, from prices.json (boatbench.py costs).

Per VM-hour by utilization, break-even against boat.dev (both list PAYG $0.0360/h and max 25% top-up bonus
$0.0288/h), single-node vs N+1 HA + off-host snapshot backup sensitivity, and stopped-VM retention under usable NVMe
storage (after OS/images/btrfs metadata + boatd minFreePct=10% reserve) across two storage policies: the shipped one
(storageOvercommit 1: every VM, running or stopped, is charged its nominal disk + memory) and a thin-provisioning
MODEL (each VM charged what it actually uses). Nothing here is a benchmark result.
"""
import json

UTILS = (0.10, 0.25, 0.50, 1.00)
PROXY_REQ_PER_DAY = (0, 2_000, 20_000, 100_000)  # fleet-wide proxied requests per UTC day (0 = proxy on, idle)
PROXY_FLEETS = (1, 7, 15, 50, 100)  # VMs
PROXY_SERVERS = ("ax42", "ax102")
SESSIONS_H = (1, 8, 24, 168)
RETAIN_H = (0, 24, 336)  # delete on stop, 1 day, boat.dev's 14 days


def hardware_monthly(s, a):
    return s["usd_per_month"] + s["setup_usd"] / a["setup_amortization_months"]


def monthly(s, a):
    return hardware_monthly(s, a) + s["backup_usd_per_month"]


def operator(a):
    return a["operator_hours_per_month"] * a["operator_usd_per_hour"]


def usable_store_gib(s, a):
    """Usable tenant GiB after deducting OS/base images/btrfs metadata and boatd minFreePct (10%)."""
    if "usable_store_gib" in s and s["usable_store_gib"] is not None:
        return int(s["usable_store_gib"])
    factor = (a.get("storage_reserve") or {}).get("usable_disk_factor", 0.85)
    return int(s["store_gib"] * factor)


def full_disk_slots(s, a):
    if "store_gib" not in s and "usable_store_gib" not in s:
        return None
    nom = int((a.get("retention") or {}).get("nominal_gib_per_vm", 59))
    return int(usable_store_gib(s, a) // nom)


def topology_monthly_tco(s, a, topo, ha_cfg=None):
    """Monthly TCO without double-counting single-host backup on multi-host topologies.

    Formula per serving host: hardware_monthly * cluster_factor + backup_usd / serving_hosts,
    Archive cost is shared across serving_hosts (default 1). For 4+1, this returns
    (5 * hardware + one archive) / 4, the allocation per serving host.
    where backup_usd is s['backup_usd_per_month'] for single-host and one shared 5 TB BX21 ($14.784/mo)
    replacing s['backup_usd_per_month'] when replace_backup_usd_per_month is set.
    """
    hw = hardware_monthly(s, a)
    factor = float(topo["cluster_factor"])
    repl = topo.get("replace_backup_usd_per_month")
    if repl is not None:
        return hw * factor + float(repl) / int(topo.get("serving_hosts", 1))
    if topo.get("extra_backup_usd_per_month"):
        # Legacy fallback: add net delta once without multiplying single-host backup by cluster_factor.
        return hw * factor + float(s["backup_usd_per_month"]) + float(topo["extra_backup_usd_per_month"])
    return hw * factor + float(s["backup_usd_per_month"])


def capacity(ram_vms, slots, h, r, hours):
    """Running VM-hours per month: RAM slots, or storage slots each held for a session plus its retention."""
    return min(ram_vms * hours, slots * hours * h / (h + r))


def _fmt_be(be_hours, cap):
    pct = f"{be_hours / cap:.0%}" if be_hours <= cap else f"never ({be_hours / cap:.0%})"
    return f"{be_hours:,.0f} VM-h/mo ({pct})"


def vm_hour_table(p):
    a, bd = p["assumptions"], p["boatdev"]["usd_per_hour"]
    hours = a["hours_per_month"]
    out = ["| Server, mode (VMs) | " + " | ".join(f"{u:.0%} util." for u in UTILS) +
           f" | + operator time, 100% | Break-even vs boat.dev (${bd:.3f}/h) |", "|---|" + "---|" * (len(UTILS) + 2)]
    for s in p["servers"]:
        m, op = monthly(s, a), operator(a)
        rows = [("conservative / untrusted", s["vms_overcommit_1_0"])]
        fslots = full_disk_slots(s, a)
        if fslots is not None and fslots < s["vms_overcommit_1_0"]:
            rows.append(("untrusted full-disk worst-case, 100%-full 59 GiB disks", fslots))
        rows.append(("default 1.5", s["vms_overcommit_1_5"]))
        for label, vms in rows:
            cap = vms * hours
            be, be_op = m / bd, (m + op) / bd
            pct = lambda x: f"{x / cap:.0%}" if x <= cap else f"never ({x / cap:.0%})"  # noqa: E731
            out.append(f"| {s['name']}, {label} ({vms}) | " + " | ".join(f"${m / (cap * u):.3f}" for u in UTILS) +
                       f" | ${(m + op) / cap:.3f} | {be:,.0f} VM-h/mo, {pct(be)}; with operator time {pct(be_op)} |")
    return out


def sensitivity_tables(p):
    """boat.dev credit-bonus / plan tiers vs self-hosted single-node and N+1 HA + off-host snapshot backups."""
    a, bd_cfg = p["assumptions"], p["boatdev"]
    hours = a["hours_per_month"]
    op = operator(a)
    bd_list = float(bd_cfg["usd_per_hour"])
    bonus_pct = bd_cfg.get("max_topup_bonus_pct", 25)
    bd_bonus = float(bd_cfg.get("usd_per_hour_max_bonus", bd_list / (1.0 + bonus_pct / 100.0)))
    plans = bd_cfg.get("plans", [])
    ha_cfg = a.get("ha_and_backup") or {}
    offhost_b = float(ha_cfg.get("offhost_backup_usd_per_month", 14.784))
    topologies = ha_cfg.get("topologies") or [
        {"label": "Single host (1.00x, basic backup)", "cluster_factor": 1.0, "replace_backup_usd_per_month": None},
        {"label": "4+1 N+1 cluster + 5 TB off-host backup (1.25x)", "cluster_factor": 1.25, "serving_hosts": 4, "replace_backup_usd_per_month": offhost_b},
        {"label": "1+1 HA pair + 5 TB off-host backup (2.00x)", "cluster_factor": 2.0, "replace_backup_usd_per_month": offhost_b},
    ]

    out = []
    if plans:
        out += [
            "Published `boat.dev` monthly plans (`$20`, `$100`, `$500`, `$2,000`) and separate wallet top-up credit bonus tiers (`+2%` to `+25%` on `$200+` payments, `default` 4 vCPU / 8 GB):",
            "",
            "| boat.dev tier | Monthly fee | Top-up bonus | Effective $/VM-hour | Notes |",
            "|---|---|---|---|---|",
        ]
        for pl in plans:
            out.append(
                f"| {pl['name']} | ${pl['monthly_fee_usd']:,.0f}/mo | +{pl['topup_bonus_pct']}% | "
                f"${pl['effective_usd_per_hour']:.4f}/h | {pl.get('note', '-')} |"
            )
        out.append("")

    out += [
        f"**Architecture & TCO formula note:** **`boatd` and `boat-api` are single-host daemons (no built-in multi-node scheduler, shared control plane, or automatic failover)**, so the `4+1` and `1+1` rows below model independent hosts behind external routing plus periodic off-host snapshot export, not built-in live HA. "
        f"Let `H = usd_per_month + setup_usd / 12`. **4+1 total cluster TCO = `5 × H + {offhost_b}`; per serving host = `(5 × H + {offhost_b}) / 4`**. The table allocates this across four serving hosts; the fifth is spare. Single host = `H + backup_usd_per_month`; 1+1 pair = `2 × H + {offhost_b}` per one serving host. Multi-host topologies replace the single-host archive with **one shared `${offhost_b:.2f}/mo` (`€13.20 × 1.12`) 5 TB Hetzner BX21 off-host archive**. The archive is counted once per cluster. Archive capacity and restore time are assumptions, not measured availability. The `+ op (${op:,.0f}/mo)` column adds `${op:,.0f}/mo` per serving host (a conservative operator allowance).",
        "",
        f"Self-hosted `untrusted` (`memOvercommit 1.0`, SMT off) TCO sensitivity across single-node vs N+1 HA + "
        f"off-host snapshot backup storage, compared against both `boat.dev` list PAYG (${bd_list:.4f}/h) and "
        f"max +{bonus_pct}% credit bonus (${bd_bonus:.4f}/h):",
        "",
        f"| Server (untrusted VMs) | Self-hosted topology | Monthly TCO / serving host | 50% util. $/VM-h | 100% util. $/VM-h | "
        f"+ op (${op:,.0f}/mo) 100% $/VM-h | Break-even vs list (${bd_list:.4f}/h) | Break-even vs +{bonus_pct}% bonus (${bd_bonus:.4f}/h) |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for s in p["servers"]:
        variants = [(f"{s['name']} ({s['vms_untrusted']})", s["vms_untrusted"])]
        fslots = full_disk_slots(s, a)
        if fslots is not None and fslots < s["vms_untrusted"]:
            variants.append((f"{s['name']} full-disk worst-case ({fslots})", fslots))
        for srv_label, vms in variants:
            cap = vms * hours
            for topo in topologies:
                tco = topology_monthly_tco(s, a, topo, ha_cfg)
                out.append(
                    f"| {srv_label} | {topo['label']} | ${tco:,.2f} | ${tco / (cap * 0.5):.3f} | "
                    f"${tco / cap:.3f} | ${(tco + op) / cap:.3f} | {_fmt_be(tco / bd_list, cap)} | {_fmt_be(tco / bd_bonus, cap)} |"
                )
    return out


def ai_proxy_monthly(cfg, req_per_day, hours_per_month):
    """boat.dev AI-credentials-proxy fee per month: ($/UTC day on + $/1000 requests x req/1000) x days."""
    days = hours_per_month / 24
    return days * (float(cfg["usd_per_utc_day_on"]) + req_per_day / 1000 * float(cfg["usd_per_1000_requests"]))


def effective_vm_hour(bd_rate, fee_month, fleet, hours_per_month):
    """boat.dev $/VM-h with the proxy fee spread over a fleet that runs 24/7 (100% duty cycle)."""
    return bd_rate + fee_month / (fleet * hours_per_month)


def hosts_for(fleet, vms_per_host):
    return -(-fleet // vms_per_host)


def breakeven_duty(host_usd, fee_month, fleet, bd_rate, hours_per_month):
    """Duty cycle (share of each VM's 730 h running) at which boat.dev costs the same as the self-hosted hosts.

    boat.dev = fleet x hours x duty x rate + proxy fee (the fee does not shrink with duty); self-hosted = host_usd.
    <= 0: the fee alone exceeds the hosts, so self-hosting wins at any duty cycle. > 1: boat.dev wins even at 100%.
    """
    return (host_usd - fee_month) / (fleet * hours_per_month * bd_rate)


def _fmt_duty(u):
    if u <= 0:
        return "any (fee alone > hosts)"
    return f"{u:.0%}" if u <= 1 else f"never ({u:.0%})"


def ai_proxy_table(p):
    """boat.dev AI-proxy fee sensitivity: requests/day x fleet size -> effective $/VM-h and shifted break-even."""
    a, bd_cfg = p["assumptions"], p["boatdev"]
    cfg = bd_cfg.get("ai_proxy")
    if not cfg:
        return []
    hours, bd = a["hours_per_month"], float(bd_cfg["usd_per_hour"])
    servers = [s for s in p["servers"] if s["key"] in PROXY_SERVERS]
    head = ["| Proxied requests/day (fleet) | Fleet (VMs) | Proxy fee $/mo | boat.dev effective $/VM-h, 24/7 |"]
    head += [f" Break-even duty vs {s['name']} ({s['vms_untrusted']} VMs/host, ${monthly(s, a):,.2f}/mo/host) |" for s in servers]
    out = ["".join(head), "|---|---|---|---|" + "---|" * len(servers)]
    for req in PROXY_REQ_PER_DAY:
        fee = ai_proxy_monthly(cfg, req, hours)
        for fleet in PROXY_FLEETS:
            cells = []
            for s in servers:
                host_usd = hosts_for(fleet, s["vms_untrusted"]) * monthly(s, a)
                cells.append(f"{_fmt_duty(breakeven_duty(host_usd, fee, fleet, bd, hours))} ({hosts_for(fleet, s['vms_untrusted'])} host"
                             f"{'s' if hosts_for(fleet, s['vms_untrusted']) > 1 else ''})")
            out.append(f"| {req:,} | {fleet} | ${fee:,.2f} | ${effective_vm_hour(bd, fee, fleet, hours):.3f} | " + " | ".join(cells) + " |")
    return out


def retention_table(p, s):
    """Cost per running VM-hour at 100% of capacity (no operator time), nominal vs thin storage charging."""
    a, ret = p["assumptions"], p["assumptions"]["retention"]
    reserve = a.get("storage_reserve") or {}
    min_free_pct = reserve.get("min_free_pct", 10)
    usable_gib = usable_store_gib(s, a)
    raw_gib = s["store_gib"]
    nom_gib = ret["nominal_gib_per_vm"]
    hours, m = a["hours_per_month"], monthly(s, a)
    ram_vms = s["vms_untrusted"]
    nom_usable_slots = int(usable_gib // nom_gib)
    nom_raw_slots = int(raw_gib // nom_gib)
    policies = [(f"PROJECTION, shipped raw-capacity sparse: {nom_gib} GiB nominal per VM", nom_gib, raw_gib),
                (f"PROJECTION, full-disk reserve: {nom_gib} GiB per VM", nom_gib, usable_gib)]
    thin = ret["thin_gib_per_vm"]
    policies += [(f"MODEL, thin: {g} GiB actual use per VM ({chr(97 + i)})", g, usable_gib) for i, (g, _) in enumerate(thin)]
    extra_note = ""
    if nom_usable_slots < ram_vms <= nom_raw_slots:
        extra_note = (
            f" Note: `boatd` `hostStorageCheck` checks raw partition capacity (`⌊{raw_gib:,} / {nom_gib}⌋ = {nom_raw_slots}` nominal VMs), "
            f"so all **{ram_vms}** RAM-bound VMs (`{ram_vms * hours:,} VM-h/mo, ${m / (ram_vms * hours):.3f}/h`) boot while disks remain sparse; "
            f"if every tenant fills 100% of its {nom_gib} GiB quota, live `checkFloor` (`minFreePct={min_free_pct}%`) caps worst-case full-disk capacity at **{nom_usable_slots}** VMs (`{nom_usable_slots * hours:,} VM-h/mo, ${m / (nom_usable_slots * hours):.3f}/h`)."
        )
    out = [f"{s['name']}, untrusted ({ram_vms} RAM slots), usable tenant store **{usable_gib:,} GiB** of "
           f"{raw_gib:,} GiB raw partition (deducts OS/images/btrfs metadata + `boatd` `minFreePct={min_free_pct}%`; "
           f"{s['store_source']}).{extra_note} All retention cells are projections, not measured retained fleets. Raw-capacity sparse uses nominal admission against the raw partition and assumes allocated blocks stay below the live floor; full-disk reserve budgets fully occupied quotas against usable storage. Thin rows model a policy that is not shipped. Cells: running VM-hours per month, and $ per running VM-hour at 100% of that capacity.", "",
           "| Storage policy | Storage slots (case) | Session H | " + " | ".join(
               "R = 0 (delete on stop)" if r == 0 else f"R = {r // 24} day{'s' if r > 24 else ''}" for r in RETAIN_H) + " |",
           "|---|---|---|" + "---|" * len(RETAIN_H)]
    for label, gib, store in policies:
        slots = int(store // gib)
        for h in SESSIONS_H:
            cells = []
            for r in RETAIN_H:
                c = capacity(ram_vms, slots, h, r, hours)
                cells.append(f"{c:,.0f}{' (RAM-bound)' if c == ram_vms * hours else ''}, ${m / c:.3f}/h")
            out.append(f"| {label} | {slots} | {h} h | " + " | ".join(cells) + " |")
    return out + [""] + [f"({chr(97 + i)}) {why}." for i, (_, why) in enumerate(thin)]


def render(p):
    out = ["### Per VM-hour and break-even (from prices.json)", ""] + vm_hour_table(p)
    out += ["", "### Credit-bonus, plan-fee & HA/backup TCO sensitivity (from prices.json)", ""] + sensitivity_tables(p)
    if p["boatdev"].get("ai_proxy"):
        out += ["", "### boat.dev AI-proxy fee sensitivity (from prices.json)", ""] + ai_proxy_table(p)
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
