"""Actual store use of stopped default VMs (for docs/cost.md's thin-provisioning model). Deletes what it makes."""
import json, os, subprocess, sys, time
sys.path.insert(0, "/tmp/bbr/bench/boatbench")
import boatbench, drivers, workloads as W
d = drivers.PandaOS()
def usage(vm):
    p = f"/var/lib/boat/vms/{vm}/"
    du = subprocess.run(["sudo", "du", "-s", "--block-size=1M", p], capture_output=True, text=True).stdout.split()[0]
    bt = subprocess.run(["sudo", "btrfs", "filesystem", "du", "-s", "--raw", p], capture_output=True, text=True).stdout
    ls = subprocess.run(["sudo", "ls", "-ls", "--block-size=1M", p], capture_output=True, text=True).stdout
    return {"du_mib": int(du), "btrfs_du": bt.strip().splitlines()[-1], "files": ls}
out = {}
for name, setup in (("fresh", None), ("nodesetup", W.NODE_SETUP)):
    vm, s = boatbench.ready(d, lambda: d.create(f"bb-thin-{name}"))
    try:
        if setup:
            boatbench.gexec(d, vm, setup)
        boatbench.gexec(d, vm, "sync")
        d.stop(vm)
        time.sleep(3)
        out[name] = usage(vm)
    finally:
        d.delete(vm)
print(json.dumps(out, indent=1))
