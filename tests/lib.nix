# Shared helpers for the VM tests in this directory.
{ lib }:
rec {
  # The base module pins hostName = "pandaos", which would give every node the
  # same name and break `relay` resolution, so each node forces its own.
  nodeName = name: { networking.hostName = lib.mkForce name; };

  # Multi-node tests: keep NetworkManager off the test VLAN so the driver's
  # static eth1 addresses stay put; agentd is what's under test, not NM.
  vlanNode = name: {
    imports = [ (nodeName name) ];
    networking.networkmanager.enable = lib.mkForce false;
  };

  # Tests that don't exercise first boot keep it off: it would grab tty8 and
  # switch VTs on every boot of a passwordless VM.
  noFirstboot = {
    pandaos.firstboot.enable = false;
  };

  # Driver-side Python shared by every test script.
  prelude = ''
    import json
    import re  # noqa: F401 (not every test uses it)
    import shlex  # noqa: F401
    import os
    import time


    def emit(metrics):
        print("METRIC " + json.dumps(metrics, sort_keys=True))
        out = os.environ.get("out")
        if out:
            with open(os.path.join(out, "metrics.json"), "w") as f:
                json.dump(metrics, f, indent=2, sort_keys=True)


    def idle_ram_mb(m):
        """Return (used, total_minus_available) in MB after 30s of idle.

        `used` is what `free` reports as used, plus tmpfs/shmem: anon memory,
        unreclaimable slab, kernel stacks, etc. MemTotal - MemAvailable is
        also returned for reference, but it counts the kernel's free-page
        watermark reserve (tens of MB, scaling with VM RAM) as used, so the
        budget is checked against `used`.
        """
        time.sleep(30)
        # Breakdown in the log so a budget miss can be diagnosed from CI alone.
        print(m.succeed("ps -eo rss=,user=,comm= --sort=-rss | head -n 20"))
        mi = {}
        for line in m.succeed("cat /proc/meminfo").splitlines():
            k, v = line.split(":", 1)
            mi[k] = int(v.split()[0])
        print({k: mi[k] for k in ("MemTotal", "MemFree", "MemAvailable", "Buffers", "Cached",
                                  "Shmem", "SReclaimable", "SUnreclaim", "AnonPages")})
        used = mi["MemTotal"] - mi["MemFree"] - mi["Buffers"] - (mi["Cached"] - mi["Shmem"]) - mi["SReclaimable"]
        return used // 1024, (mi["MemTotal"] - mi["MemAvailable"]) // 1024


    def status_json(m):
        return json.loads(m.succeed("cat /run/panda-agentd/status.json"))


    def wait_last_error(m, present, timeout=120):
        """Wait until status.json's lastError is non-empty (present) or empty."""
        m.wait_until_succeeds(
            "grep -q '\"lastError\": \"" + ("[^\"]" if present else "\"") + "' /run/panda-agentd/status.json",
            timeout=timeout,
        )
  '';
}
