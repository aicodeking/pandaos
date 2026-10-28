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
| `boot-headless` | boots, `panda-agentd` and `panda-mem` come up, idle RAM and closure within budget |
| `boot-desktop` | boots, `greetd` is active, `panda-agents-status` gives the right output for fixture status files, waybar/labwc config is in place, idle RAM and closure within budget (2 GB VM) |
| `agentd-roundtrip` | `box` pairs with a `fake-relay` node (`status.json` says paired), runs `echo hi`, then a job cancelled through the relay is stopped and reported as cancelled |
| `slow-network` | the same round trip at 300 ms, 2% loss, 1 Mbit/s, within 120 s |
| `mem` | `panda-mem write`/`search` work, writes commit to git, `/etc/panda/mcp.json` is valid; btrbk skips cleanly without a `/.snapshots` mount |
| `security` | `panda` has no usable password and `panda-firstboot` prompts; sshd refuses passwords (also tried from a second node); only port 22 is reachable from the network; the hardening sysctls are set |
| `browser` | `panda-browser` serves CDP on 127.0.0.1 only, not reachable from a second node, fresh profile after a restart |
| `install` | `panda-install --yes` onto an empty 10 GiB disk (subvolumes, labels, `/etc/pandaos`), then boots that disk with OVMF: hostname, password hash, time zone, btrfs mounts, services, and a btrbk snapshot run |
| `closures` (being added) | host-side, no VM: closure sizes of the real `nixosConfigurations`, and `updateBytes` for an agentd-only update |
| `mem-security` (being added) | the job user tries to plant git hooks or config in `/mem`; later `panda-mem` writes by other users must not run them |

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
| `release.yml` | tag `v*` | `nix flake check`, builds both ISOs (all with `--no-update-lock-file`), writes `SHA256SUMS`, attests build provenance, publishes a GitHub release, then force-moves the `release` branch to the tag |

KVM on GitHub runners is enabled with a udev rule that makes `/dev/kvm` usable by everyone; Nix is
installed with `cachix/install-nix-action` with the `kvm` and `nixos-test` system features, and
`magic-nix-cache` caches builds between CI runs (CI only, not for boxes).

Every third-party action is pinned to a full commit SHA, with its version in a comment. To bump one,
resolve the new tag and replace the SHA:

```sh
gh api repos/actions/checkout/commits/v4.4.0 --jq .sha
```

Workflows set `permissions: {}` at the top and grant each job only what it needs: `contents: read`
everywhere, plus `id-token`/`attestations: write` for the release build and `contents: write` only
for the release `publish` job.

## Binary cache (Cachix)

**Not set up yet. This is an owner action.** Without a binary cache, installed boxes compile
panda-agentd, panda-mem and the other PandaOS packages themselves whenever an update changes them
(see [updates-and-rollback.md](updates-and-rollback.md#what-an-update-downloads-and-builds)).

`ci.yml` (on `main` only) and `release.yml` already have an optional `cachix/cachix-action` step. It
is skipped until the secret exists, and it pushes every store path the job builds (the ISOs contain
the installed systems, so this covers what boxes need). To turn it on:

1. Create a cache on [cachix.org](https://app.cachix.org), for example `pandaos`.
2. Create an auth token with write access to it.
3. In the GitHub repo settings, add the **secret** `CACHIX_AUTH_TOKEN`. If the cache is not called
   `pandaos`, also add the **variable** `CACHIX_CACHE` with its name.
4. Point boxes at it: set `pandaos.binaryCache.url` (e.g. `https://pandaos.cachix.org`) and
   `pandaos.binaryCache.publicKey` (shown on the cache's Cachix page), either as a default in this
   repo, shipped in a release, or per box in `/etc/pandaos/local.nix`.

Only `main` and release tags push. Pull requests, other branches and forks only read.

## Releasing

There is no release yet. Installed boxes update from the `release` branch, which only exists after
the first release.

1. Move the Unreleased entries in `CHANGELOG.md` under the new version.
2. Tag a commit on `main` and push the tag: `git tag v0.2.0 && git push origin v0.2.0`.
3. `release.yml` builds and checks everything, publishes `pandaos-<tag>-headless-x86_64-linux.iso`,
   `pandaos-<tag>-desktop-x86_64-linux.iso` and `SHA256SUMS` with provenance, and only then moves
   `release` to the tag (`git push origin <sha>:refs/heads/release --force`). It refuses tags that are
   not on `main`. If `release` is a protected branch, allow GitHub Actions to force-push to it.
