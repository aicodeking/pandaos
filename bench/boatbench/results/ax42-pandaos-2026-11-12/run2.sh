#!/usr/bin/env bash
# Rerun of bcef and g after the shared-box interference (full-builder VM 05:26-05:45Z, nixos switch 05:39Z).
set -u
cd /tmp/bbr
G=/nix/store/fsd4vbp9zjd9iaicq2fb9lpc1h5j973y-glibc-2.42-100
L=/nix/store/dqj57l1prad38vjrad4bs9d977i4q5zm-gcc-15.3.0-lib
export PATH=/nix/store/jy00n3jp5xj8s40n1fbldmkcr5n7avxc-python3-3.13.15/bin:$PATH
export BB_NODE_LOADER="$G/lib/ld-linux-x86-64.so.2 --library-path $G/lib:$L/lib"
export BOAT_URL=http://127.0.0.1:8740
BOAT_KEY=$(cat ~/boat-owner-key); export BOAT_KEY
export BOATBENCH_PRESET=pandaos-install
export BOATBENCH_BOATD_OPTIONS='{"_installed":"/etc/boat/boatd.json sets no diskCache, memOvercommit, cpuOvercommit, warmPool or untrustedTenants: boatd defaults","defaultTeam":{"maxVMs":10,"maxVCPU":32,"maxMemMiB":65536}}'
export BOATBENCH_CONCURRENT_N=8
B=bench/boatbench/boatbench.py
CPUF=/sys/devices/system/cpu/cpufreq
export BOATBENCH_HOST_DESC="Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB; PandaOS 26.05 install (NixOS, kernel $(uname -r)); boat store = native btrfs (compress=zstd:1) on one NVMe, Samsung MZVL2512HCJQ; boatd defaults (trusted, memOvercommit 1.5), owner team quota 8 default VMs; governor $(cat $CPUF/policy0/scaling_governor), EPP $(cat $CPUF/policy0/energy_performance_preference); 16 CPUs online"
run() { echo "== $(date -u +%FT%TZ) $*"; python3 "$B" run "$@"; }
mkdir -p out/rerun
run --driver pandaos --repeats 3 --only bcef --out out/rerun
BOATBENCH_DENSITY_LEVELS=1,2,4,7,8,11 run --driver pandaos --repeats 2 --only g --out out/rerun
echo "ALL_DONE2 $(date -u +%FT%TZ)" | tee out/ALL_DONE2
