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

## Plan: comparison against mainstream distros (not built yet)

Once we have a Hetzner box, a nightly job will provision **identical cloud VMs** (same size, region
and disk). It will run PandaOS headless and desktop, Ubuntu Server, Ubuntu Desktop, and Debian, and
record for each one:

- idle RAM after boot and settle (the same `MemTotal - MemAvailable` method)
- installed footprint on disk
- boot time to multi-user and to the greeter
- agent job round-trip latency, where it applies

The results will go into the same `results/<date>.json` format as a `baseline` section, so that
`collect.py` can print PandaOS next to each distro. Until that box exists we only track the
PandaOS budgets in `tests/budgets.json`.
