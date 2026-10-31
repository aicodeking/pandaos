"""Scoring: like-for-like tables, task-level bootstrap intervals, paired
per-task differences between OSes, fail reasons, validation and versions.

Tool modes fall into two classes and rows are only ever compared inside one:
  generic  pixels + keys from outside the VM (the headline);
  a11y     generic + accessibility tools: `atspi` (the same AT-SPI tools on
           every OS) and `panda` (PandaOS's panda-cu, more tools still). A panda
           row is listed with the a11y class, never against generic rows.
"""
import math
import random
import statistics

REASONS = ("timeout", "wrong_result", "misclick", "crash")
CLASS = {"generic": "generic", "atspi": "a11y", "panda": "a11y"}
CLASS_TITLE = {"generic": "Generic tools (screenshots, mouse, keyboard): the headline comparison",
               "a11y": "With accessibility tools (generic + tree/find/act): atspi on every OS, "
                       "plus PandaOS's panda-cu column (more tools than atspi)"}
BOOT = 2000


def wilson(passes, n, z=1.96):
    """95% Wilson score interval (lo, hi) for passes/n (attempt level)."""
    if n == 0:
        return (0.0, 0.0)
    p = passes / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (max(0.0, c - h), min(1.0, c + h))


def bootstrap(values, stat=statistics.fmean, n=BOOT, seed=0):
    """95% percentile interval of stat over values resampled with replacement."""
    if not values:
        return (0.0, 0.0)
    rnd = random.Random(seed)
    xs = sorted(stat([rnd.choice(values) for _ in values]) for _ in range(n))
    return (xs[int(0.025 * n)], xs[int(0.975 * n) - 1])


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


def task_rates(ts):
    """{task: pass rate over its repeats}: repeats aggregate per task first."""
    by = {}
    for t in ts:
        by.setdefault(t["task"], []).append(1.0 if t["passed"] else 0.0)
    return {k: statistics.fmean(v) for k, v in by.items()}


def _median(ts, key):
    xs = [t[key] for t in ts if t.get(key) is not None]
    return round(statistics.median(xs), 1) if xs else None


def summarize(trials):
    """Group trials by (os, tools) -> summary rows, sorted by class then OS."""
    groups = {}
    for t in trials:
        groups.setdefault((t["os"], t["tools"]), []).append(t)
    rows = []
    for (os_name, tools), ts in sorted(groups.items(), key=lambda kv: (CLASS.get(kv[0][1], kv[0][1]), kv[0])):
        n, p = len(ts), sum(t["passed"] for t in ts)
        lo, hi = wilson(p, n)
        rates = list(task_rates(ts).values())
        blo, bhi = bootstrap(rates)
        costs = [t["cost_usd"] for t in ts if t.get("cost_usd") is not None]
        rows.append({
            "os": os_name, "tools": tools, "class": CLASS.get(tools, tools), "n": n, "passes": p,
            "tasks": len(rates), "passRate": round(p / n, 4) if n else 0.0,
            "wilson95": [round(lo, 4), round(hi, 4)],
            "taskRate": round(statistics.fmean(rates), 4) if rates else 0.0,
            "taskBootstrap95": [round(blo, 4), round(bhi, 4)],
            "medianSteps": _median(ts, "steps"), "medianSeconds": _median(ts, "seconds"),
            "medianApiSeconds": _median(ts, "api_seconds"), "medianEnvSeconds": _median(ts, "env_seconds"),
            "failReasons": {r: sum(t.get("reason") == r for t in ts) for r in REASONS},
            "costUSD": round(sum(costs), 2) if costs else None,
        })
    return rows


def paired(trials):
    """Per-task differences between every two columns of the same class only."""
    cols = {}
    for t in trials:
        cols.setdefault((t["os"], t["tools"]), []).append(t)
    rates = {k: task_rates(v) for k, v in cols.items()}
    keys = sorted(rates, key=lambda k: (CLASS.get(k[1], k[1]), k))
    out = []
    for i, a in enumerate(keys):
        for b in keys[i + 1:]:
            if CLASS.get(a[1]) != CLASS.get(b[1]) or a[0] == b[0]:
                continue  # never across classes; same-OS mode deltas are not OS comparisons
            common = sorted(set(rates[a]) & set(rates[b]))
            if not common:
                continue
            d = [rates[a][t] - rates[b][t] for t in common]
            lo, hi = bootstrap(d)
            out.append({"class": CLASS.get(a[1]), "a": f"{a[0]} ({a[1]})", "b": f"{b[0]} ({b[1]})",
                        "tasks": len(common), "meanDiff": round(statistics.fmean(d), 4),
                        "bootstrap95": [round(lo, 4), round(hi, 4)],
                        "aBetter": sum(x > 0 for x in d), "ties": sum(x == 0 for x in d),
                        "bBetter": sum(x < 0 for x in d),
                        "likeForLike": a[1] == b[1]})
    return out


def _pct(x):
    return f"{100 * x:.0f}%"


def markdown(rows, trials, pairs=()):
    out = []
    for cls in ("generic", "a11y"):
        crow = [r for r in rows if r["class"] == cls]
        if not crow:
            continue
        out += [f"## {CLASS_TITLE[cls]}", "",
                "| OS | Tools | Task pass rate | 95% CI (task bootstrap) | Attempts passed | 95% CI (Wilson) | "
                "Median steps | Median env s | Median API s | Timeout | Wrong | Misclick | Crash | Cost $ |",
                "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for r in crow:
            fr = r["failReasons"]
            out.append(
                f"| {r['os']} | {r['tools']} | {_pct(r['taskRate'])} | {_pct(r['taskBootstrap95'][0])}-"
                f"{_pct(r['taskBootstrap95'][1])} | {r['passes']}/{r['n']} | {_pct(r['wilson95'][0])}-"
                f"{_pct(r['wilson95'][1])} | {r['medianSteps']} | {r['medianEnvSeconds']} | {r['medianApiSeconds']} | "
                f"{fr['timeout']} | {fr['wrong_result']} | {fr['misclick']} | {fr['crash']} | "
                f"{r['costUSD'] if r['costUSD'] is not None else '?'} |")
        cp = [p for p in pairs if p["class"] == cls]
        if cp:
            out += ["", "Paired per-task differences (A minus B, task pass rates; 95% bootstrap over tasks):", "",
                    "| A | B | Tasks | Mean diff | 95% CI | A better / tie / B better | Note |",
                    "|---|---|---|---|---|---|---|"]
            for p in cp:
                note = "like for like" if p["likeForLike"] else "panda-cu vs the AT-SPI baseline (panda has more tools)"
                out.append(f"| {p['a']} | {p['b']} | {p['tasks']} | {100 * p['meanDiff']:+.0f} pts | "
                           f"{100 * p['bootstrap95'][0]:+.0f} to {100 * p['bootstrap95'][1]:+.0f} | "
                           f"{p['aBetter']} / {p['ties']} / {p['bBetter']} | {note} |")
        out.append("")
    cols = [(r["os"], r["tools"]) for r in rows]
    tasks = sorted({t["task"] for t in trials})
    if cols and tasks:
        out += ["Per task (passes/attempts):", "",
                "| Task | " + " | ".join(f"{o} ({m})" for o, m in cols) + " |",
                "|---|" + "---|" * len(cols)]
        for task in tasks:
            cells = []
            for o, m in cols:
                ts = [t for t in trials if t["task"] == task and t["os"] == o and t["tools"] == m]
                cells.append(f"{sum(t['passed'] for t in ts)}/{len(ts)}" if ts else "-")
            out.append(f"| {task} | " + " | ".join(cells) + " |")
    return "\n".join(out) + "\n"


def validation_markdown(validations):
    """validations: [(meta, rows)] from `cubench.py validate`."""
    if not validations:
        return ""
    out = ["", "## Checker validation (null agent must fail, scripted oracle must pass)", "",
           "| OS | Tasks | Null fail-rate | Oracle pass-rate | AT-SPI tools | Mismatches |", "|---|---|---|---|---|---|"]
    for meta, rows in validations:
        n = len(rows)
        bad = [r["task"] for r in rows if not r["ok"]]
        at = meta.get("atspi") or {}
        out.append(f"| {meta['os']} | {n} | {sum(bool(r['nullFailed']) for r in rows)}/{n} | "
                   f"{sum(bool(r['oraclePassed']) for r in rows)}/{n} | "
                   f"{'ok' if at.get('ok') else at.get('error') or '?'} | {', '.join(bad) or 'none'} |")
        if meta.get("error"):
            out.append(f"| {meta['os']} | run failed: {meta['error']} | | | | |")
    return "\n".join(out) + "\n"


def environment_markdown(metas):
    seen, out = set(), []
    for m in metas:
        v = m.get("versions")
        if not v or m["os"] in seen:
            continue
        seen.add(m["os"])
        out.append(f"| {m['os']} | {v.get('os', '?')} | {v.get('kernel', '?')} | {m.get('sessionType', '?')} | "
                   f"{v.get('desktop', '?')} | {v.get('editor', '?')} | {v.get('files', '?')} | "
                   f"{v.get('terminal', '?')} | {v.get('browser', '?')} |")
    if not out:
        return ""
    return "\n".join(["", "## Environment", "",
                      "| Profile | OS | Kernel | Session | Desktop | Editor | Files | Terminal | Browser |",
                      "|---|---|---|---|---|---|---|---|---|"] + out) + "\n"
