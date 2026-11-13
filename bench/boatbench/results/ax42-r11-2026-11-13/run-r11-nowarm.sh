#!/usr/bin/env bash
# r11 on the AX42 (PandaOS install at SRC), from /tmp/bbr11 (bench/boatbench at SRC + ab.py). Log: OUT/run.log.
# Trusted mode as configured. Two temporary boatd configs (Writeback cache, Sync engine) are runtime-only
# systemd drop-ins under /run, removed again at the end; no VM runs while boatd restarts.
set -euo pipefail
SRC=${SRC:?source commit}
OUT=${OUT:-/tmp/r11out}
cd /tmp/bbr11
export BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key) BB_DIR=/tmp/bbr11 BOATBENCH_SOURCE_COMMIT=$SRC BOATBENCH_PRESET=pandaos-install
exec > >(tee -a "$OUT/run.log") 2>&1
log() { echo "$(date -u +%H:%M:%SZ) $*"; }
vms() { curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"; }
boatd_with() { # JQ_FILTER over /etc/boat/boatd.json, or "" for the installed config
  sudo rm -rf /run/systemd/system/boatd.service.d
  if [ -n "$1" ]; then
    sudo jq "$1" /etc/boat/boatd.json | sudo tee /run/r11-boatd.json >/dev/null
    sudo mkdir -p /run/systemd/system/boatd.service.d
    printf '[Service]\nExecStart=\nExecStart=%s -config /run/r11-boatd.json\n' "$(systemctl show -p ExecStart --value boatd | grep -o '/nix/store/[^ ;]*/bin/boatd' | head -1)" |
      sudo tee /run/systemd/system/boatd.service.d/r11.conf >/dev/null
  fi
  sudo systemctl daemon-reload; sudo systemctl restart boatd; sleep 3
  log "boatd: $(systemctl is-active boatd) config: ${1:-installed} $(sudo journalctl -u boatd -n 1 -o cat)"
}
log "boatd image conversion: $(sudo journalctl -u boatd -o cat --no-pager | grep "converted to NOCOW" | tail -n 3)"
# boatd at 0d160dd reuses a warm template booted under the installed drive config (fixed after
# 0d160dd: warmStamp now includes it), so every changed-config run here has the warm pool off.
log "start (no warm pool): vms $(vms)"
boatd_with '.warmPool = false'
python3 ab.py flush "$OUT/flush" unsafe-cold
python3 ab.py seqw "$OUT/seqw" unsafe-async-cold
boatd_with '.warmPool = false | .diskCache = "Writeback"'
python3 ab.py flush "$OUT/flush" writeback-cold
BOATBENCH_PRESET=trusted-writeback BOATBENCH_BOATD_OPTIONS='{"diskCache":"Writeback","warmPool":false}' \
  python3 boatbench.py run --driver pandaos --repeats 3 --only d --out "$OUT/disk-d-writeback"
python3 ab.py seqw "$OUT/seqw" writeback-async-cold
boatd_with '.warmPool = false | .diskEngine = "Sync"'
python3 ab.py seqw "$OUT/seqw" unsafe-sync-cold
boatd_with ''
log "end: vms $(vms) snapshots $(curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/snapshots") drop-ins $(ls /run/systemd/system/boatd.service.d 2>&1)"
