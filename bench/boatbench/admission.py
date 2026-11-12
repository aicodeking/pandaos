"""boatd's host admission, with its constants read from boatd's Go source.

The cost model (prices.json, docs/cost.md) and the tests use this so the VM
counts cannot drift from components/boat/boatd again:

  config.go defaultConfig()  hostReserveMiB, vmOverheadMiB, memOvercommit, cpuOvercommit, Sizes
  manager.go quotaLocked     sum(MemMiB + VMOverheadMiB) <= (MemTotal - hostReserveMiB) * memOvercommit
                             sum(VCPU) <= host CPUs * cpuOvercommit
  perf.go realMemCheck       above 1.0: MemAvailable >= hostReserveMiB + (MemMiB + VMOverheadMiB) / memOvercommit

The module's untrustedTenants settings come from components/boat/module.nix.
"""
import os
import json
import re

HERE = os.path.dirname(os.path.abspath(__file__))
BOAT = os.path.normpath(os.path.join(HERE, "..", "..", "components", "boat"))
# MemTotal is lower than the RAM sold: the kernel's struct page array (64 bytes per
# 4 KiB page, 1/64 of RAM) and firmware-reserved ranges are never in it. The firmware
# part varies by board and was not measured on these servers: an ESTIMATE.
MEMMAP_FRACTION = 1 / 64
FIRMWARE_RESERVED_MIB = 256


def _read(rel):
    with open(os.path.join(BOAT, rel)) as f:
        return f.read()


def boatd_defaults():
    """The admission constants from boatd's defaultConfig() and the default size."""
    src = _read("boatd/config.go")
    body = src[src.index("func defaultConfig() Config {"):]
    body = body[:body.index("\n}\n")]

    def num(field):
        m = re.search(rf"^\s*{field}:\s*([0-9.]+),", body, re.M)
        if not m:
            raise ValueError(f"boatd/config.go defaultConfig(): no {field}")
        return float(m.group(1))

    size = re.search(r'"default":\s*\{(\d+),\s*(\d+),\s*(\d+)\}', body)
    if not size:
        raise ValueError('boatd/config.go defaultConfig(): no "default" size')
    return {"host_reserve_mib": int(num("HostReserveMiB")), "vm_overhead_mib": int(num("VMOverheadMiB")),
            "vm_page_cache_mib": int(num("VMPageCacheMiB")), "mem_overcommit": num("MemOvercommit"),
            "cpu_overcommit": num("CPUOvercommit"), "vm_vcpu": int(size.group(1)),
            "vm_mem_mib": int(size.group(2)), "vm_disk_gib": int(size.group(3))}


def admission_counts_overhead():
    """True while manager.go counts VMOverheadMiB per active VM in the host RAM check."""
    src = _read("boatd/manager.go")
    return "hostMem+v.MemMiB+m.cfg.VMOverheadMiB" in src and "hostMem+s.MemMiB+m.cfg.VMOverheadMiB" in src


def untrusted_settings():
    """What services.boat.untrustedTenants sets: boatd/untrusted.json, which module.nix imports."""
    return json.loads(_read("boatd/untrusted.json"))


def est_memtotal_mib(ram_gb):
    """ESTIMATED MemTotal of a box sold with ram_gb GiB (see MEMMAP_FRACTION)."""
    sold = ram_gb * 1024
    return int(sold * (1 - MEMMAP_FRACTION) - FIRMWARE_RESERVED_MIB)


def vms_by_ram(memtotal_mib, overcommit, d=None):
    d = d or boatd_defaults()
    per_vm = d["vm_mem_mib"] + d["vm_overhead_mib"]
    return int((memtotal_mib - d["host_reserve_mib"]) * overcommit // per_vm)


def vms_by_cpu(threads, cpu_overcommit, d=None):
    d = d or boatd_defaults()
    return int(threads * cpu_overcommit // d["vm_vcpu"])


def memtotal_needed(n, overcommit, d=None):
    """Smallest MemTotal (MiB) at which boatd admits n default VMs."""
    d = d or boatd_defaults()
    return d["host_reserve_mib"] + int(-(-n * (d["vm_mem_mib"] + d["vm_overhead_mib"]) // overcommit))


def vms(server, overcommit, d=None, smt_off=False, memtotal=None):
    """default VMs a server admits: RAM (MemTotal estimate unless given) and CPU at cpuOvercommit."""
    d = d or boatd_defaults()
    threads = server["cores"] if smt_off else server["vcpu_threads"]
    mt = est_memtotal_mib(server["ram_gb"]) if memtotal is None else memtotal
    return min(vms_by_ram(mt, overcommit, d), vms_by_cpu(threads, d["cpu_overcommit"], d))
