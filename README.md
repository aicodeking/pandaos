# PandaOS

A lightweight, agent-first Linux built on NixOS. Install it on an old PC and it becomes an agent server that pairs with PandaCode.

**Goals (each one backed by CI numbers, not adjectives):**
- Lowest RAM and disk of any major OS for agent work
- Works on slow, lossy networks (tested at 1 Mbit / 300 ms / 2% loss)
- Shared, persistent agent memory at `/mem` (markdown + git, served over MCP)
- A proper repair kit, a very light desktop when you want one, and rollback on every update

## Layout

| Path | What |
|---|---|
| `flake.nix` | ISOs, VM, NixOS configs, checks |
| `nixos/` | base module, `headless` and `desktop` (labwc) profiles |
| `components/agentd` | `panda-agentd`: pairs with the relay (dials out) and runs jobs in limited systemd units |
| `components/mem` | `panda-mem`: the `/mem` store and MCP server |
| `tests/` | NixOS VM tests and RAM/disk budgets |
| `bench/` | metric collection |

## Build

```sh
nix build .#iso-headless      # or .#iso-desktop
nix build .#vm-headless && ./result/bin/run-pandaos-vm
nix flake check -L            # VM tests (needs KVM)
```

Status: early. The pairing protocol is a placeholder until PandaConnect is defined.
