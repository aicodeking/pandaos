# PandaOS VM tests

`default.nix` exposes these as `checks.x86_64-linux.*`. Shared helpers are in `lib.nix`, and the larger tests have their own files.

| test | what it proves | what it does not prove |
|---|---|---|
| `boot-headless` | boots; `panda-agentd` (enabled for this test; off by default) and `panda-mem.service` come up; with the relay unreachable, `pairing-code` is `0640 root:wheel`, unreadable by `nobody`, the code is not in `status.json`, `pairing` is true and `lastError`/`lastErrorAt` are set; idle RAM within budget | behaviour on real hardware |
| `boot-desktop` | `greetd` + `initial_session` autologin into labwc (pixman on virtio-gpu); labwc, its Wayland socket and waybar come up and labwc is still alive after 30 s; `idleRamMB` (budgeted) is the logged-in session; after logout tuigreet comes back and `greeterIdleRamMB` is reported; `panda-agents-status` and `panda-connect-status` (`test-connect-status.sh`) against fixtures, the real unsigned box shows *PandaCode: sign in*; a screenshot `session.png` is in `$out` | GPU rendering, real displays, input, audio |
| `agentd-roundtrip` | `box` pairs with `fake-relay` and runs `echo hi`; after pairing the code file is gone, `status.json` has no code and `lastError` is empty; a `sleep 600` job is cancelled; stopping the relay sets `lastError` and restarting it clears it (and re-pairs) | the real PandaConnect relay |
| `slow-network` | netem on **both** nodes (150 ms + 1% loss + 1 Mbit/s each way), with ping checking the RTT is ~300 ms (`rttMs`) before agentd starts; pairing + job round trip finish within 120 s (`roundtripSec`) | jitter, reordering, NAT, relay restarts |
| `mem` | write/search as `panda` over `/run/panda-mem/sock`, git commit, valid `/etc/panda/mcp.json`, btrbk skips without `/.snapshots` | |
| `mem-security` | `panda-agent` cannot write `.git` (no hook, no `core.fsmonitor`). Root then plants both, and plain `git` fires them (so the check is not vacuous). `panda-mem write` over the socket as `panda` and `panda-agent` then fires neither; refusing the tampered repo is accepted. Markers live in `.git`, the only place the sandboxed service could write them | other git vectors beyond panda-mem's config allow-list; an attacker who can replace the panda-mem binary or its unit |
| `update-rollback` | the real `nixos-upgrade` unit, `panda-update-run`, `panda-health` and rollback: a good generation is switched to and logged healthy; a generation whose agentd `ExecStart` is `false` is rolled back to the previous one, with `panda-agentd.service is …` and `rolling back to` in the journal and the system profile back on the good generation; `panda-update hold` makes the unit skip (`ConditionResult=no`), `panda-update run` refuse, and `unhold` removes the file | **`pandaos.updates.switchCommand` replaces `nixos-rebuild switch`** with a switch to a prebuilt specialisation. Not covered: evaluating `/etc/pandaos#pandaos`, `--update-input pandaos`, fetching `github:AICodeKing/pandaos/release`, binary-cache substitution, `flake.lock` restore, boot assessment (see `boot-fallback`) |
| `boot-fallback` | UEFI (OVMF) + systemd-boot, three boots: the installed generation passes `panda-boot-check` and becomes `LoaderEntryDefault`; a bad generation (panda-mem `ExecStart=false`) installed with `switch-to-configuration boot` becomes `LoaderEntryOneShot` while the default stays put; its trial boot fails the check and schedules `shutdown -r +1`; the guest's own reboot lands on the good entry, `LoaderEntryDefault` is still the good one and the one-shot is consumed | a kernel or initrd that panics (`panic=10` + `boot.panic_on_fail` should take the same fallback, untested); real firmware that drops EFI variable writes |
| `iso-boot` | the headless live ISO boots from a virtual CD-ROM (BIOS, squashfs root, `PANDAOS-LIVE` label) to multi-user, and `panda-install --help` runs; `isoBootSec` | the image is built from the same modules as `packages.iso-headless` **plus `test-instrumentation.nix` and zstd level 3**, so it is not the byte-identical release artifact; EFI boot of the ISO; USB boot |
| `pandacode-host` | the pinned PandaCode AppImage runs (`pandacode serve --help` under `xvfb-run` as `panda`); not signed in, `panda-connect status --json` says `signedIn:false`, the service is skipped with 0 restarts over 60 s and `panda-health` passes; `panda-connect full-access on/off` exit 0 and are saved; a stale sign-in marker makes `serve` exit "not signed in", the unit drops the marker and stops (≤ 2 restarts, no left-over processes) and `panda-health` still passes | **real sign-in** (`panda-connect sign-in` needs GitHub's device flow and PandaCode's Connect relay, not reachable from CI), a signed-in host serving, and real tasks from another device: test those by hand |
| `closures` | host-side, no VM: NAR closure of `nixosConfigurations.{headless,desktop}` (what panda-install puts on disk) vs `budgets.json`; `updateBytes`/`updatePaths` for a panda-mem-only update | compressed download size (NAR bytes are an upper bound); nixpkgs-bump updates |
| `security`, `browser`, `install` | see the test bodies in `default.nix` | |

Firstboot is off in every test except `security`. Run one test with
`nix build .#checks.x86_64-linux.boot-headless -L`, or all with `nix flake check -L` (needs KVM).
`python3 tests/test_closures.py` self-tests `closures.py`.

## Metrics and budgets

Each test prints `METRIC {json}` (so failed runs still report) and writes `$out/metrics.json`.
`bench/collect.py` reads both.

- `idleRamMB` is `MemTotal - MemFree - Buffers - (Cached - Shmem) - SReclaimable`: the median of 3
  readings 10 s apart, after a 30 s settle (all three are printed in the log).
  This is budgeted. `memTotalMinusAvailableMB` is reported but not budgeted, because it also counts the kernel's free-page reserve.
- `closureMB` (`headlessClosureMB`, `desktopClosureMB` in `closures`) is the NAR size of the real
  installable system, the same number `nix path-info -S` gives. Earlier versions measured the test VM
  closure, which included test instrumentation.
- `updateBytes` is the NAR bytes of the paths that a machine on `nixosConfigurations.headless` lacks
  for the same system with a rebuilt panda-mem (agentd, used before, is off by default now).

`budgets.json`: the RAM budgets are deliberately loose. The closure budgets are the expected
real-system closures + 10%, so any growth past that fails CI. **Current values are estimates**
(headless ~1694 MB, desktop ~2136 MB) after dropping the pinned nixpkgs `source` (197 MB,
`nixpkgs.flake.setNixPath`/`setFlakeRegistry = false`) and NetworkManager's ModemManager link
(libqmi/libmbim/modemmanager, ~80 MB); replace them with the CI-measured numbers + 10%. The
`closures` log prints `why` chains (shortest referrer path) for python3, perl, libqmi, the nixpkgs
`source` and other heavy paths. python3 (127 MB) stays: NixOS's systemd-boot installer
(`systemd-boot-builder.py`) needs it. perl (55 MB) stays: the perl `/etc` and users activation
scripts (cutting it needs `system.etc.overlay` + `services.userborn`, which hide files the
installer writes to `/mnt/etc`). Previously measured: headless 1971 MB, desktop
2413 MB (with the curated firmware set; desktop GPU off), including panda-ckpt, panda-sys and
panda-agents. PandaCode Connect first added about 1.2 GB to headless and 0.8 GB to desktop (from
1637 / 2114 MB to 2834 / 2915 MB), mostly `appimageTools`' default FHS library set. The slim FHS env
in `components/pandacode-host/package.nix` (only the libraries the AppImage leaves to the host, no
`glibcLocales`; see `slim-notes.md`) brought that down by 863 MB (headless) and 502 MB (desktop).
