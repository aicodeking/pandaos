#!/usr/bin/env python3
"""Self-check for closures.py: python3 tests/test_closures.py"""
import contextlib
import io
import json
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import closures  # noqa: E402

H = "/nix/store/" + "a" * 32 + "-"


def closure_info(d, paths):
    """Write a fake closureInfo dir: paths is [(name, size, [ref names])]."""
    d.mkdir()
    lines = []
    for name, size, refs in paths:
        lines += [H + name, "sha256:x", str(size), "", str(len(refs))] + [H + r for r in refs]
    (d / "registration").write_text("\n".join(lines) + "\n")
    (d / "total-nar-size").write_text(str(sum(s for _, s, _ in paths)))
    return d


with tempfile.TemporaryDirectory() as tmp:
    t = Path(tmp)
    mb = closures.MB
    old = closure_info(t / "old", [("glibc", 30 * mb, ["glibc"]), ("agentd-1", 10 * mb, ["glibc"]), ("sys-1", mb, ["agentd-1"])])
    new = closure_info(t / "new", [("glibc", 30 * mb, ["glibc"]), ("agentd-2", 11 * mb, []), ("sys-2", 2 * mb, ["agentd-2", "glibc"])])
    assert closures.read_registration(old)[H + "agentd-1"] == 10 * mb
    added, nbytes = closures.update_size(closures.read_registration(old), closures.read_registration(new))
    assert set(added) == {H + "agentd-2", H + "sys-2"} and nbytes == 13 * mb, (added, nbytes)

    budgets = t / "budgets.json"
    budgets.write_text(json.dumps({"headless": {"closureMB": 50}, "desktop": {"closureMB": 10}}))
    out = t / "metrics.json"
    rc = closures.main(["--budgets", str(budgets), "--out", str(out),
                        "--system", f"headless={old}", "--update", f"headless={old}:{new}"])
    m = json.loads(out.read_text())
    assert rc == 0 and m["headlessClosureMB"] == 41 and m["updateBytes"] == 13 * mb, (rc, m)
    err = io.StringIO()
    with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
        rc = closures.main(["--budgets", str(budgets), "--out", str(out), "--system", f"desktop={new}"])
    assert rc == 1 and "FAIL: desktop closure" in err.getvalue(), "over-budget closure must fail"

    # why: shortest chain from the root (sys-*, referenced by nothing) to a path.
    sizes, refs = closures.read_graph(old)
    chains = closures.why(sizes, refs, ["glibc"])
    assert chains == {H + "glibc": [H + "sys-1", H + "agentd-1", H + "glibc"]}, chains
    sizes, refs = closures.read_graph(new)
    assert closures.why(sizes, refs, ["glib"])[H + "glibc"] == [H + "sys-2", H + "glibc"], "not the shortest chain"
    assert closures.why(sizes, refs, ["python3-"]) == {}
    rc = closures.main(["--budgets", str(budgets), "--out", str(out),
                        "--system", f"headless={new}", "--why", "glibc"])
    assert rc == 0

print("ok")
