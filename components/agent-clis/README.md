# Agent CLIs

The PandaCode host starts agent CLIs by name from PATH. None ship by default
(`pandaos.agents.clis = [ ]`); pick them in config or at runtime.

| `pandaos.agents.clis` | nixpkgs 26.05 attr | binary | license | closure (NAR / download) |
| --- | --- | --- | --- | --- |
| `claude-code` | `claude-code` | `claude` | unfree | not on cache.nixos.org; fetches the ~277 MiB native binary, then small deps from the cache |
| `codex` | `codex` (0.146.0) | `codex` | Apache-2.0 | 518 MiB / 161 MiB |
| `gemini-cli` | `gemini-cli` (0.42.0) | `gemini` | Apache-2.0 | 304 MiB / 86 MiB |
| `opencode` | `opencode` (1.15.10) | `opencode` | MIT | 200 MiB / 64 MiB |
| `kimi` | none: not in nixpkgs 26.05 | | | selecting it fails evaluation |

Sizes are from cache.nixos.org narinfo (x86_64-linux, 2026-10-29) and
include paths the base system may already have. claude-code is allowed via
`nixpkgs.config.allowUnfreePredicate` for exactly the selected unfree
names; global `allowUnfree` stays off. (In VM tests with a read-only
`nixpkgs.pkgs`, selecting claude-code needs `node.pkgsReadOnly = false`.)

## Declarative

```nix
pandaos.agents.clis = [ "codex" "claude-code" ];
```

## Runtime: `panda-agents-install`

```sh
sudo panda-agents-install codex opencode   # add, then apply
panda-agents-install --check codex          # validate names only
```

It merges the names into `/etc/pandaos/agents.nix`:

```nix
{ pandaos.agents.clis = [ "codex" "opencode" ]; }
```

then runs `panda-update run` (or `nixos-rebuild switch --flake
/etc/pandaos#pandaos` when `pandaos.updates.enable = false`). Rerunning with
CLIs that are already applied does nothing. If the apply fails, the previous
`agents.nix` is restored. Note `panda-update run` also updates the
`pandaos` input, like the daily update does.

**`local.nix` contract.** The installer's `local.nix` must import the file
when it exists:

```nix
imports = [ ./disk.nix ] ++ lib.optional (builtins.pathExists ./agents.nix) ./agents.nix;
```

Older boxes without that line: `panda-agents-install` refuses and prints it.

**Offline installs.** The ISO's prebuilt systems contain no agent CLIs, so
an offline install cannot add them. Choosing them needs a network and one
rebuild (`sudo panda-agents-install ...`), at first boot or later.

## Tests

- `test-agents-install.sh [script]`: unit test of validation, merge,
  idempotence and restore on failure, with a fake updater. Needs bash 4+.
- `vmtest.nix`: `clis = [ "codex" ]`; codex on the `agent` user's PATH,
  `codex --version` offline; runs the unit test inside the VM.
