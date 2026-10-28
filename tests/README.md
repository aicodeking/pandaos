# PandaOS VM tests

`default.nix` holds the NixOS VM tests, which the flake exposes as `checks.x86_64-linux.*`:

| test | what it checks |
|---|---|
| `boot-headless` | boots, `panda-agentd` and `panda-mem-init` come up, idle RAM and closure size stay within budget |
| `boot-desktop` | boots, `greetd` is active, `panda-agents-status` against fixture `status.json` files, waybar/labwc config, idle RAM and closure size within budget (2 GB VM) |
| `agentd-roundtrip` | `box` pairs with a `fake-relay` node and runs `echo hi`, `status.json` says paired, then a `sleep 600` job is cancelled through the relay |
| `slow-network` | same round trip with 300 ms delay, 2% loss and 1 Mbit/s, must finish in 120 s |
| `mem` | `panda-mem write`/`search` work, the write makes a git commit, `/etc/panda/mcp.json` is valid JSON, btrbk skips cleanly without `/.snapshots` |
| `security` | no usable `panda` password and `panda-firstboot` prompts, sshd refuses passwords (from a second node too), only port 22 is reachable, hardening sysctls applied |
| `browser` | `panda-browser` CDP on 127.0.0.1:9222 only, unreachable from a second node, fresh profile per start |
| `install` | `panda-install --yes` to an empty 10 GiB disk, checks labels, subvolumes and `/etc/pandaos`, then boots the installed disk via OVMF and checks hostname, password, time zone, mounts, services and a btrbk run |

Firstboot is off in every test except `security`. The `install` test installs the real prebuilt
headless system plus the NixOS test backdoor (`test-instrumentation.nix`), so the driver can talk to
the installed system.

Run one test with `nix build .#checks.x86_64-linux.boot-headless -L`. Run all of them with `nix flake check -L`. You need KVM.

## Budgets

`budgets.json` sets the limits the boot tests fail on. The numbers are **deliberately loose** for now.
CI prints the real values as `METRIC {...}` lines. Once we have a few runs of data we will tighten
the budgets to the measured value plus some headroom.

- `idleRamMB` is what `free` reports as used, plus shmem/tmpfs (`MemTotal - MemFree - Buffers - (Cached - Shmem) - SReclaimable`), read after a 30 s settle.
  `memTotalMinusAvailableMB` is also reported. It is not budgeted, because it also counts the kernel's free-page watermark reserve (about 80 MB on a 1 GB VM).
- `closureMB` is the NAR size of the test VM's system closure, the same number `nix path-info -S` gives.
  It is computed on the host, because the VM does not register its own closure in its Nix database.
  This closure includes the test-driver instrumentation. CI also prints the real ISO sizes (`isoMB`).

Each test writes its metrics to `$out/metrics.json` and prints a `METRIC` line, so the line is still
there when a test fails. `bench/collect.py` reads both.
