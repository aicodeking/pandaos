#!/bin/sh
# Regenerate every committed AX42 report from the raw results next to it. `git diff --exit-code` after this is the check.
set -eu
cd "$(dirname "$0")"
B=../boatbench.py
(cd ax42-ubuntu-2026-11-11 && for d in trusted untrusted trusted-warm trusted-writeback; do
  python3 ../$B report $d/results-*.json --server ax42 --out $d/report.md >/dev/null; done
 python3 ../$B presets */results-pandaos-*.json --out presets.md >/dev/null)
(cd ax42-pandaos-2026-11-12 && for d in powersave performance; do
  python3 ../$B report $d/results-*.json --server ax42 --out report-$d.md >/dev/null; done)
(cd ax42-r11-2026-11-13 && python3 ../$B report lifecycle-b/results-*.json disk-d/results-*.json --server ax42 --out report.md >/dev/null
 python3 ../$B report lifecycle-b-cold/results-*.json --server ax42 --out report-cold.md >/dev/null
 python3 ../$B report disk-d-writeback/results-*.json --server ax42 --out report-writeback.md >/dev/null)
(cd ax42-r12-2026-11-13 && python3 ../$B report disk-d-pinned/results-*.json --server ax42 --out report-disk-pinned.md >/dev/null)
