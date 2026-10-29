# bench

`collect.py` merges the VM-test metrics into `results/<date>.json` and prints a markdown table:

```sh
nix build .#checks.x86_64-linux.boot-headless -o result-boot-headless -L 2>&1 | tee boot-headless.log
python3 bench/collect.py result-* *.log
```

It accepts check output directories (it reads their `metrics.json`), `metrics.json` files, and
build logs (it parses their `METRIC {...}` lines). The nightly workflow (`.github/workflows/nightly.yml`)
runs every check, runs this script, and uploads the results as an artifact.

Run `python3 bench/test_collect.py` to self-test the script.

Budgets are looked up by the test's `profile`, or by a profile-prefixed key: the `closures` check
reports `headlessClosureMB`, which is compared with `budgets.headless.closureMB`. Closure sizes now
come from the real `nixosConfigurations`, not the instrumented test VMs. Desktop `idleRamMB` is
measured in a logged-in labwc session, and the greeter is reported separately as `greeterIdleRamMB`.
`updateBytes` is an upper bound (NAR bytes, not compressed downloads) for an agentd-only update.

## Comparison against other distros

`bench/compare/` boots PandaOS headless and stock cloud images in identical QEMU/KVM VMs and
measures them (`.github/workflows/compare.yml`, manual and weekly). Results: `docs/comparison.md`.
