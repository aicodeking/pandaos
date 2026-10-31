"""Scoring: pass rates with 95% Wilson intervals, medians, fail reasons, tables."""
import math
import statistics

REASONS = ("timeout", "wrong_result", "misclick", "crash")


def wilson(passes, n, z=1.96):
    """95% Wilson score interval (lo, hi) for passes/n."""
    if n == 0:
        return (0.0, 0.0)
    p = passes / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (max(0.0, c - h), min(1.0, c + h))


def fail_reason(rec, passed):
    """Why an attempt failed. ponytail: misclick is a heuristic (clicks that left the
    screen pixel-identical); a per-click target check would need UI ground truth."""
    if passed:
        return None
    if rec.get("error"):
        return "crash"
    clicks = rec.get("clicks", 0)
    if clicks >= 3 and 2 * rec.get("noop_clicks", 0) >= clicks:
        return "misclick"
    if rec.get("out_of_budget"):
        return "timeout"
    return "wrong_result"


def summarize(trials):
    """Group trials by (os, tools) -> summary rows."""
    groups = {}
    for t in trials:
        groups.setdefault((t["os"], t["tools"]), []).append(t)
    rows = []
    for (os_name, tools), ts in sorted(groups.items()):
        n, p = len(ts), sum(t["passed"] for t in ts)
        lo, hi = wilson(p, n)
        costs = [t["cost_usd"] for t in ts if t.get("cost_usd") is not None]
        rows.append({
            "os": os_name, "tools": tools, "n": n, "passes": p,
            "passRate": round(p / n, 4) if n else 0.0,
            "wilson95": [round(lo, 4), round(hi, 4)],
            "medianSteps": statistics.median(t["steps"] for t in ts) if ts else None,
            "medianSeconds": statistics.median(t["seconds"] for t in ts) if ts else None,
            "failReasons": {r: sum(t.get("reason") == r for t in ts) for r in REASONS},
            "costUSD": round(sum(costs), 2) if costs else None,
        })
    return rows


def markdown(rows, trials):
    out = ["| OS | Tools | Pass rate | 95% CI | Passed/N | Median steps | Median s | "
           "Timeout | Wrong | Misclick | Crash | Cost $ |",
           "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        fr = r["failReasons"]
        out.append(
            f"| {r['os']} | {r['tools']} | {100 * r['passRate']:.0f}% | "
            f"{100 * r['wilson95'][0]:.0f}-{100 * r['wilson95'][1]:.0f}% | {r['passes']}/{r['n']} | "
            f"{r['medianSteps']} | {r['medianSeconds']} | {fr['timeout']} | {fr['wrong_result']} | "
            f"{fr['misclick']} | {fr['crash']} | {r['costUSD'] if r['costUSD'] is not None else '?'} |")
    cols = [(r["os"], r["tools"]) for r in rows]
    tasks = sorted({t["task"] for t in trials})
    if cols and tasks:
        out += ["", "Per task (passes/attempts):", "",
                "| Task | " + " | ".join(f"{o} ({m})" for o, m in cols) + " |",
                "|---|" + "---|" * len(cols)]
        for task in tasks:
            cells = []
            for o, m in cols:
                ts = [t for t in trials if t["task"] == task and t["os"] == o and t["tools"] == m]
                cells.append(f"{sum(t['passed'] for t in ts)}/{len(ts)}" if ts else "-")
            out.append(f"| {task} | " + " | ".join(cells) + " |")
    return "\n".join(out) + "\n"
