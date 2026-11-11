#!/usr/bin/env bash
# Bring up boatd + boat-api from source on a Linux x86_64 host with /dev/kvm
# (a GitHub ubuntu runner) for boatbench, with boatd's PRODUCTION defaults
# (sizes default = 4 vCPU / 8 GiB / 51 GiB, diff snapshots, jailer, disk
# limits, no egress cap) except the overcommit needed to fit 10 x 8 GiB VMs on
# a 16 GiB runner.
#
#   stack.sh up GUEST_DIR   # GUEST_DIR has vmlinux, rootfs-slim.ext4; writes $WORK/env (BOAT_URL, BOAT_KEY)
#   BOATBENCH_PRESET=untrusted stack.sh up ...   # presets.json: trusted (default), trusted-warm, trusted-writeback, untrusted
#   BOATBENCH_BOATD_OPTIONS='{"warmPool":true}' stack.sh up ...   # JSON merged over boatd.json and the preset
#   BENCH_WORK=/opt/boatbench/work BENCH_STATE=/opt/boatbench/state stack.sh up ...   # a real server: all under one dir
#   stack.sh down
#
# ponytail: components/boat/e2e.sh has no "stack only" mode and is outside this
# benchmark's ownership, so this mirrors its setup steps (the Firecracker
# version and checksum are READ from e2e.sh, not copied). If e2e.sh grows an
# E2E_STACK_ONLY switch, call it here instead.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
BOAT=$(cd "$HERE/../../components/boat" && pwd)
WORK=${BENCH_WORK:-/mnt/boatbench}
STATE=${BENCH_STATE:-/var/lib/boatbench}   # a real server: keep it under $WORK (BENCH_STATE=$WORK/state)
SOCK=$WORK/run/boatd.sock
API=http://127.0.0.1:8740
# shellcheck disable=SC2016 # literal ${...} in the sed pattern
FC_VERSION=$(sed -n 's/^FC_VERSION=\${FC_VERSION:-\(v[0-9.]*\)}.*/\1/p' "$BOAT/e2e.sh")
FC_SHA256=$(sed -n 's/^FC_SHA256=\([0-9a-f]\{64\}\).*/\1/p' "$BOAT/e2e.sh")
log() { printf '\n\033[1m==> %s\033[0m\n' "$*" >&2; }

down() {
  set +e
  sudo pkill -9 -x firecracker
  if command -v iptables >/dev/null; then
    while sudo iptables -C FORWARD -i boatbr0 -j ACCEPT 2>/dev/null; do sudo iptables -D FORWARD -i boatbr0 -j ACCEPT; done
    while sudo iptables -C FORWARD -o boatbr0 -j ACCEPT 2>/dev/null; do sudo iptables -D FORWARD -o boatbr0 -j ACCEPT; done
  fi
  [ -f "$WORK/boatd.pid" ] && sudo kill "$(cat "$WORK/boatd.pid")" 2>/dev/null
  [ -f "$WORK/api.pid" ] && kill "$(cat "$WORK/api.pid")" 2>/dev/null
  sleep 1
  for t in $(ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -E '^bt[0-9]+$'); do sudo ip link del "$t"; done
  sudo ip link del boatbr0 2>/dev/null
  sudo nft delete table inet boat 2>/dev/null
  sudo nft delete table bridge boat 2>/dev/null
  mountpoint -q "$STATE" && sudo umount -l "$STATE"
  sudo rmdir /sys/fs/cgroup/boat-bench 2>/dev/null
  true
}

up() {
  local guest
  guest=$(cd "${1:?usage: stack.sh up GUEST_DIR}" && pwd)
  [ -e /dev/kvm ] || { echo "stack: /dev/kvm missing" >&2; exit 1; }
  [ -n "$FC_VERSION" ] && [ -n "$FC_SHA256" ] || { echo "stack: cannot read FC_VERSION/FC_SHA256 from e2e.sh" >&2; exit 1; }
  sudo mkdir -p "$WORK" && sudo chown "$(id -u):$(id -g)" "$WORK"
  mkdir -p "$WORK/bin" "$WORK/dl" "$WORK/run" "$WORK/api"

  log "tools + runner network (same fixes as e2e.sh: br_netfilter off, FORWARD accept)"
  local missing=()
  for c in jq:jq nft:nftables mkfs.btrfs:btrfs-progs curl:curl; do command -v "${c%%:*}" >/dev/null || missing+=("${c#*:}"); done
  [ ${#missing[@]} = 0 ] || { sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null; }
  if lsmod | grep -q '^br_netfilter'; then
    sudo modprobe -r br_netfilter 2>/dev/null ||
      sudo sysctl -qw net.bridge.bridge-nf-call-iptables=0 net.bridge.bridge-nf-call-ip6tables=0 net.bridge.bridge-nf-call-arptables=0
  fi
  if command -v iptables >/dev/null; then
    sudo iptables -I FORWARD -i boatbr0 -j ACCEPT
    sudo iptables -I FORWARD -o boatbr0 -j ACCEPT
  fi

  log "firecracker $FC_VERSION (checksum from e2e.sh)"
  if [ ! -x "$WORK/dl/firecracker" ]; then
    curl -fsSLo "$WORK/dl/fc.tgz" "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz"
    echo "$FC_SHA256  $WORK/dl/fc.tgz" | sha256sum -c -
    tar -xzf "$WORK/dl/fc.tgz" -C "$WORK/dl"
    mv "$WORK/dl/release-$FC_VERSION-x86_64/firecracker-$FC_VERSION-x86_64" "$WORK/dl/firecracker"
    mv "$WORK/dl/release-$FC_VERSION-x86_64/jailer-$FC_VERSION-x86_64" "$WORK/dl/jailer"
  fi

  log "go build boatd, boat-api"
  (cd "$BOAT/boatd" && CGO_ENABLED=0 go build -o "$WORK/bin/boatd" .)
  (cd "$BOAT/api" && CGO_ENABLED=0 go build -o "$WORK/bin/boat-api" .)

  log "boatd options: preset ${BOATBENCH_PRESET:-trusted}, then BOATBENCH_BOATD_OPTIONS"
  local preset=${BOATBENCH_PRESET:-trusted} options=${BOATBENCH_BOATD_OPTIONS:-'{}'} merged
  jq -e --arg p "$preset" 'has($p) and ($p | startswith("_") | not)' "$HERE/presets.json" >/dev/null ||
    { echo "stack: unknown BOATBENCH_PRESET $preset (see presets.json)" >&2; exit 2; }
  jq -e 'type == "object"' <<<"$options" >/dev/null || { echo "stack: BOATBENCH_BOATD_OPTIONS must be a JSON object: $options" >&2; exit 2; }
  merged=$(jq -c --arg p "$preset" --argjson o "$options" '.[$p] * $o' "$HERE/presets.json")
  local untrusted state_size=100G
  untrusted=$(jq -r '.untrustedTenants // false' <<<"$merged")
  if [ "$untrusted" = true ]; then
    log "untrustedTenants host settings (module.nix kernel params, as far as a runner allows)"
    # module.nix boots with nosmt=force; boatd refuses to start while SMT control reads "on".
    local smt=/sys/devices/system/cpu/smt/control
    if [ "$(cat "$smt" 2>/dev/null)" = on ]; then echo off | sudo tee "$smt" >/dev/null || true; fi
    echo "SMT control: $(cat "$smt" 2>/dev/null || echo absent), CPUs online: $(nproc)" | tee "$WORK/smt.txt" >&2
    sudo sysctl -qw kernel.io_uring_disabled=2 || echo "stack: kernel.io_uring_disabled not settable" >&2
    [ ! -w /sys/kernel/mm/ksm/run ] || echo 0 | sudo tee /sys/kernel/mm/ksm/run >/dev/null
    # storageOvercommit 1: committed storage (51 GiB disk + 8 GiB per VM) must fit the store.
    state_size=1T
  fi

  log "state: btrfs loop (reflinks like production, $state_size sparse) + slim image"
  [ -f "$WORK/state.img" ] || { truncate -s "$state_size" "$WORK/state.img"; mkfs.btrfs -q "$WORK/state.img"; }
  sudo mkdir -p "$STATE"
  mountpoint -q "$STATE" || sudo mount -o loop "$WORK/state.img" "$STATE"
  sudo mkdir -p "$STATE/images/slim"
  sudo cp --sparse=always "$guest/vmlinux" "$STATE/images/slim/vmlinux"
  sudo cp --sparse=always "$guest/rootfs-slim.ext4" "$STATE/images/slim/rootfs.ext4"
  sudo sync
  sudo mkdir -p /sys/fs/cgroup/boat-bench

  # cpuOvercommit: room for BENCH_VMS (12) x 4 vCPU on the CPUs online NOW, i.e. after SMT
  # went off (untrusted leaves a 4-vCPU runner 2 CPUs: 12 x 2 = 24 vCPUs refused workload c's 10 VMs).
  local cpus cpu_oc
  cpus=$(nproc)
  cpu_oc=${BENCH_CPU_OVERCOMMIT:-$(( (${BENCH_VMS:-12} * 4 + cpus - 1) / cpus ))}
  [ "${BENCH_CPU_OVERCOMMIT:-}" ] || [ "$cpu_oc" -ge 12 ] || cpu_oc=12
  echo "cpuOvercommit $cpu_oc on $cpus online CPUs" | tee -a "$WORK/smt.txt" >&2
  log "boatd (production defaults; overcommit raised for 10 x 8 GiB on the runner): $merged"
  jq --argjson options "$merged" '. * $options' >"$WORK/boatd.json" <<JSON
{"stateDir":"$STATE","runDir":"$WORK/run","socket":"$SOCK","socketGroup":"$(id -gn)",
 "firecracker":"$WORK/dl/firecracker","jailer":"$WORK/dl/jailer","cgroupParent":"boat-bench","reflink":"always",
 "bootTimeoutS":120,"defaultImage":"slim","memOvercommit":${BENCH_MEM_OVERCOMMIT:-8},"cpuOvercommit":$cpu_oc,
 "defaultTeam":{"maxVMs":16}}
JSON
  # ioCost needs the state disk's model (untrustedTenants refuses to start without one):
  # measure it once, as the NixOS module's boat-iocost-model.service does.
  if jq -e '.untrustedTenants == true or (.ioCost == true and (.ioCostModel // "") == "")' "$WORK/boatd.json" >/dev/null &&
    ! sudo test -s "$STATE/iocost-model"; then
    log "iocost model of the state disk (boatd -measure-disk, about 25 s)"
    sudo "$WORK/bin/boatd" -measure-disk -config "$WORK/boatd.json" | tee "$WORK/iocost-model.txt"
  fi
  sudo rm -f "$SOCK"   # a stale socket from a killed boatd would pass the wait below at once
  sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >"$4" 2>&1' _ \
    "$WORK/boatd.pid" "$WORK/bin/boatd" "$WORK/boatd.json" "$WORK/boatd.log" &
  for _ in $(seq 1200); do [ -S "$SOCK" ] && break; sleep 0.1; done
  curl -fsS --unix-socket "$SOCK" http://boatd/v1/health >/dev/null || { tail -50 "$WORK/boatd.log"; exit 1; }
  # A create before the template exists boots cold and would be reported as warm.
  if [ "$(jq -r .warmPool "$WORK/boatd.json")" = true ]; then
    log "warm template of slim@default"
    for _ in $(seq 300); do sudo test -f "$STATE/warm/slim@default/warm.json" && break; sleep 1; done
    sudo test -f "$STATE/warm/slim@default/warm.json" || { tail -50 "$WORK/boatd.log"; exit 1; }
  fi

  log "boat-api"
  local key
  key=$("$WORK/bin/boat-api" admin init -data "$WORK/api" -user bench -team bench | jq -r .token)
  [[ $key == boat_* ]] || { echo "stack: admin init: $key" >&2; exit 1; }
  "$WORK/bin/boat-api" serve -data "$WORK/api" -listen 127.0.0.1:8740 -boatd "$SOCK" -rate 0 -images slim >"$WORK/api.log" 2>&1 &
  echo $! >"$WORK/api.pid"
  for _ in $(seq 100); do curl -fs "$API/healthz" >/dev/null && break; sleep 0.1; done
  curl -fs "$API/healthz" >/dev/null || { tail -50 "$WORK/api.log"; exit 1; }
  printf 'BOAT_URL=%s\nBOAT_KEY=%s\nBOATBENCH_BOATD_OPTIONS=%q\n' "$API" "$key" "$(jq -c 'del(.stateDir, .runDir, .socket, .socketGroup, .firecracker, .jailer)' "$WORK/boatd.json")" >"$WORK/env"
  chmod 600 "$WORK/env"
  log "stack up: $WORK/env"
}

case "${1:-}" in
  up) shift; up "$@" ;;
  down) down ;;
  *) echo "usage: stack.sh up GUEST_DIR | down" >&2; exit 2 ;;
esac
