#!/usr/bin/env bash
# Run boatbench against the owner's real PandaOS boat server (and boat.dev if
# BOATDEV_API_KEY is set), then write the report. Runs from any machine that can
# reach the API; run it ON the server for lifecycle numbers without WAN latency.
#
#   BOAT_URL=https://api.boat.example BOAT_KEY=boat_... \
#   BOATBENCH_HOST_DESC="Hetzner AX52, Ryzen 7 7700, 64 GiB, NVMe, Falkenstein DE" \
#   [BOATDEV_API_KEY=boat_...] bench/boatbench/run-on-server.sh [REPEATS] [WORKLOADS]
#
# The key needs vms:write + exec. boat-api's default rate limit (-rate 10/s per key)
# throttles the exec round-trip loop; throttled calls are retried and not counted,
# but a key on an instance started with -rate 0 gives cleaner numbers.
# The server must offer size "default" = 4 vCPU / 8 GiB (boatd's default) and fit
# 10 of them at once for workload c, or run with WORKLOADS=abdefg.
#
# Workload g (density under load) runs BOATBENCH_DENSITY_LEVELS VMs (default 1,2,4,6)
# through the Node loop at once and takes the host's CPU count from the machine it
# runs on: run it ON the server (or set BOATBENCH_HOST_CPUS). On an AX102 in untrusted
# mode, BOATBENCH_DENSITY_LEVELS=1,4,8,15 measures the packed host the price assumes.
#
# Both drivers first record an API round-trip baseline (25 authenticated GETs of the VM)
# and report lifecycle times raw and net of it; the medians are printed at the end.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPEATS=${1:-5}
ONLY=${2:-abcdefg}
OUTDIR=${BOATBENCH_OUT:-boatbench-results/$(date -u +%Y%m%dT%H%M%SZ)}
: "${BOAT_URL:?set BOAT_URL}" "${BOAT_KEY:?set BOAT_KEY}"
if [ -z "${BOATBENCH_HOST_DESC:-}" ]; then
  BOATBENCH_HOST_DESC="$(uname -n 2>/dev/null): $(lscpu 2>/dev/null | sed -n 's/^Model name: *//p' | head -1), $(nproc 2>/dev/null) CPU, $(free -g 2>/dev/null | awk '/Mem:/{print $2}') GiB (harness host; set BOATBENCH_HOST_DESC to describe the server)"
  export BOATBENCH_HOST_DESC
fi
mkdir -p "$OUTDIR"
python3 "$HERE/boatbench.py" run --driver pandaos --repeats "$REPEATS" --only "$ONLY" --out "$OUTDIR"
if [ -n "${BOATDEV_API_KEY:-}" ]; then
  BOATBENCH_HOST_DESC="boat.dev managed fleet, EU (Germany/Finland/France per boat.dev)" \
    python3 "$HERE/boatbench.py" run --driver boatdev --repeats "$REPEATS" --only "$ONLY" --out "$OUTDIR"
else
  echo "boatdev: SKIPPED (BOATDEV_API_KEY not set)" >&2
fi
python3 "$HERE/boatbench.py" report "$OUTDIR"/results-*.json --out "$OUTDIR/report.md" >/dev/null
echo "API round-trip baseline (p50 of 25 GETs, median over repeats):"
python3 - "$OUTDIR"/results-*.json <<'PY'
import json, statistics, sys
for p in sys.argv[1:]:
    r = json.load(open(p))
    v = [rep["metrics"]["api_rtt_p50_ms"] for rep in r["repeats"] if "api_rtt_p50_ms" in rep.get("metrics", {})]
    print(f"  {r['driver']}: {statistics.median(v):.1f} ms over {len(v)} repeats" if v else f"  {r['driver']}: not measured")
PY
echo "report: $OUTDIR/report.md"
