# Development

You need Nix with flakes. Building ISOs and running the VM tests needs Linux on x86_64, and the VM
tests need KVM. On macOS, use a Linux builder or let CI run them.

## Flake outputs

| Output | What |
|---|---|
| `packages.x86_64-linux.iso-headless` | Headless installer ISO |
| `packages.x86_64-linux.iso-desktop` | Desktop installer ISO |
| `packages.x86_64-linux.vm-headless` | QEMU VM of the headless system |
| `packages.x86_64-linux.panda-install` | The installer, with both prebuilt systems |
| `nixosConfigurations.headless`, `.desktop` | The prebuilt systems the ISOs install, on the btrfs layout |
| `lib.mkPandaSystem { profile; modules; }` | Builds a PandaOS system (used by the installed `/etc/pandaos` flake) |
| `nixosModules.default` | Base module (security, updates, first boot, snapshots) plus `panda-agentd`, `panda-mem` and `panda-browser` (off) |
| `nixosModules.headless`, `.desktop` | The two profiles |
| `checks.x86_64-linux.*` | NixOS VM tests, see below |
| `formatter.x86_64-linux` | `nixfmt` (run `nix fmt`) |

Installed boxes use a different flake: the local `/etc/pandaos` flake written by `panda-install`
(new in Unreleased), whose `nixosConfigurations.pandaos` builds on this repo.

```sh
nix build .#iso-headless                      # result/iso/*.iso
nix build .#vm-headless && ./result/bin/run-pandaos-vm
```

## VM tests

| Check | What it checks |
|---|---|
| `boot-headless` | boots, `panda-agentd` and `panda-mem-init` come up, idle RAM and closure within budget |
| `boot-desktop` | boots, `greetd` is active, `panda-agents-status` gives the right output for fixture status files, waybar/labwc config is in place, idle RAM and closure within budget (2 GB VM) |
| `agentd-roundtrip` | `box` pairs with a `fake-relay` node (`status.json` says paired), runs `echo hi`, then a job cancelled through the relay is stopped and reported as cancelled |
| `slow-network` | the same round trip at 300 ms, 2% loss, 1 Mbit/s, within 120 s |
| `mem` | `panda-mem write`/`search` work, writes commit to git, `/etc/panda/mcp.json` is valid; btrbk skips cleanly without a `/.snapshots` mount |
| `security` | `panda` has no usable password and `panda-firstboot` prompts; sshd refuses passwords (also tried from a second node); only port 22 is reachable from the network; the hardening sysctls are set |
| `browser` | `panda-browser` serves CDP on 127.0.0.1 only, not reachable from a second node, fresh profile after a restart |
| `install` | `panda-install --yes` onto an empty 10 GiB disk (subvolumes, labels, `/etc/pandaos`), then boots that disk with OVMF: hostname, password hash, time zone, btrfs mounts, services, and a btrbk snapshot run |

```sh
nix flake check -L --keep-going                     # all of them
nix build .#checks.x86_64-linux.boot-headless -L    # one
```

Budgets are in `tests/budgets.json`; how metrics are measured is in [benchmarks.md](benchmarks.md).

## Go components

```sh
cd components/agentd && go vet ./... && go test ./...
cd components/mem && go vet ./... && go test ./...
```

`components/agentd/cmd/fake-relay` is a small relay used by the round-trip tests. You can also run it
by hand (`fake-relay -listen :8787`) and point a local `panda-agentd -relay http://localhost:8787` at it.

## CI

| Workflow | When | What |
|---|---|---|
| `ci.yml` | every push and PR | fails without `flake.lock`; `nix flake check` with KVM, `go vet`/`go test` for both components, `bench/test_collect.py`; on `main` also builds both ISOs and reports their size |
| `lock.yml` | by hand | runs `nix flake lock` (or `nix flake update`) and uploads `flake.lock` as the `flake-lock` artifact, for contributors without Nix |
| `nightly.yml` | daily, or by hand | runs each check separately and uploads metrics as an artifact |
| `release.yml` | tag `v*` | `nix flake check`, builds both ISOs, writes `SHA256SUMS`, attests build provenance, publishes a GitHub release |

KVM on GitHub runners is enabled with a udev rule that makes `/dev/kvm` usable by everyone; Nix is
installed with `cachix/install-nix-action` with the `kvm` and `nixos-test` system features, and
`magic-nix-cache` caches builds.

## Releasing

1. Move the Unreleased entries in `CHANGELOG.md` under the new version.
2. Tag and push: `git tag v0.2.0 && git push origin v0.2.0`.
3. `release.yml` publishes `pandaos-<tag>-headless-x86_64-linux.iso`,
   `pandaos-<tag>-desktop-x86_64-linux.iso` and `SHA256SUMS`.
