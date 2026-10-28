# Contributing to PandaOS

Thanks for helping. PandaOS is small on purpose, so every change has to justify its RAM, disk and
complexity.

## Ground rules

- **Numbers, not adjectives.** A change that affects idle RAM or closure size should show the
  `METRIC` lines from CI before and after. The budgets live in `tests/budgets.json`.
- **Keep the base lean.** New packages go into a profile or behind an option, not into `base.nix`,
  unless every box needs them.
- **Tests come with features.** New services get a NixOS VM test in `tests/default.nix`; Go code gets
  `go test` coverage in its component.
- **Security issues go through [SECURITY.md](SECURITY.md)**, not public issues.

## Workflow

1. Open an issue first for anything non-trivial (bug or feature template).
2. Branch from `main`. Use conventional commits: `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, `perf:`, `ci:`.
3. Run the checks locally (see [docs/development.md](docs/development.md)):
   ```sh
   nix fmt                                   # nixfmt
   (cd components/agentd && go vet ./... && go test ./...)
   (cd components/mem && go vet ./... && go test ./...)
   python3 bench/test_collect.py
   nix flake check -L                        # VM tests, needs Linux + KVM
   ```
4. Add a line under **Unreleased** in [CHANGELOG.md](CHANGELOG.md).
5. Open a pull request. CI must be green.

## Releases

Maintainers tag `vX.Y.Z` on `main`. `.github/workflows/release.yml` runs `nix flake check`, builds both
ISOs, writes `SHA256SUMS`, attests build provenance and publishes the GitHub release. Move the
Unreleased changelog entries under the new version before tagging.

By contributing you agree that your work is licensed under the [MIT license](LICENSE).
