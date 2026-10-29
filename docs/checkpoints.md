# Checkpoints: panda-ckpt

> **New in Unreleased.** On by default (`services.panda-ckpt.enable`).

`panda-ckpt` keeps instant copy-on-write checkpoints of agent workspaces on btrfs. You can diff,
restore and fork them. It runs as the workspace owner and has no root helper. Agents call the same
operations through its MCP server.

## Workspaces

A workspace is a btrfs subvolume at `~/work/<name>`. Boot creates `~/work` (a subvolume) and
`~/.checkpoints` (mode 0700) for each user in `services.panda-ckpt.users` (default `[ "panda" ]`).

```sh
panda-ckpt init myproj              # new empty workspace ~/work/myproj
panda-ckpt adopt ~/work/oldproj     # convert an existing plain directory in place
panda-ckpt list                     # workspaces
```

Names match `[A-Za-z0-9._-]{1,64}` and must not start with `.`.

## Commands

All commands accept `--json`.

| Command | What it does |
|---|---|
| `create <ws> [-m label]` | read-only snapshot `~/.checkpoints/<ws>/<id>` plus `<id>.json` metadata |
| `list <ws>` | checkpoints: id, created, label, reason (`manual`, `auto`, `pre-restore`), parent |
| `diff <ws> <id> [<id2>\|current]` | changed files, with unified diffs for small text files |
| `restore <ws> <id>` | takes a `pre-restore` safety checkpoint first, then atomically swaps the workspace back to `<id>` |
| `fork <ws> <id> <new>` | a new writable workspace `~/work/<new>` from `<id>` |
| `prune <ws> [--keep N] [--older-than 7d]` | delete old checkpoints |
| `gc` | remove leftovers of interrupted operations |

A restore never loses the current state: you can undo it by restoring the safety checkpoint.

## Auto-checkpoints

`panda-ckpt-auto-<user>.timer` runs every `interval` (default `10min`). It checkpoints each
workspace that changed since its last checkpoint and keeps the newest `keepAuto` (default 24) auto
checkpoints per workspace. It never prunes manual or pre-restore checkpoints. The service runs as the
user, with no network and write access to their home only.

```nix
services.panda-ckpt = { interval = "5min"; keepAuto = 48; users = [ "panda" "alice" ]; };
```

## MCP

Registered as `/etc/panda/mcp.d/panda-ckpt.json` (see [agents.md](agents.md#mcp-registry)).
Tools: `ckpt_create`, `ckpt_list`, `ckpt_diff`, `ckpt_fork`, and `ckpt_restore`, which refuses to run
without `"confirm": true`.

## Desktop

`panda-menu` → **Checkpoints** offers List, Create, Restore (asks first) and Fork.

## Disk and mount options

Checkpoints share extents with the workspace, so a new one costs almost nothing. Space grows as files
change afterwards. `sizeEstimate` is the apparent size of the files, not their exclusive usage.
`panda-sys`'s `sys_ckpt_space` reports what is free on the filesystem.

Deleting a subvolume as a normal user needs the btrfs mount option `user_subvol_rm_allowed`. The
PandaOS disk layout (`generic-disk.nix`) sets it on every btrfs mount, and the installer's
`/etc/pandaos/disk.nix` overrides only the devices, so installed boxes keep it. Without the option,
`panda-ckpt` empties each snapshot and removes it with `rmdir`. That is slower, but it works.

On a non-btrfs `/home`, `~/work` is a plain directory, and `init`/`create` fail with an error.

## Checkpoints vs. snapshots

| | panda-ckpt | btrbk snapshots |
|---|---|---|
| What | one workspace in `~/work` | all of `/home` and `/mem` |
| When | on demand, plus every 10 min if changed | hourly |
| Who | the user or agent, unprivileged | root |
| Restore | `panda-ckpt restore` | `panda-rollback-mem` (for `/mem`), or by hand |

See [updates-and-rollback.md](updates-and-rollback.md) for system rollback.
