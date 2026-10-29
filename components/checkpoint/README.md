# panda-ckpt

Instant copy-on-write checkpoints and forks of agent workspaces on btrfs.
It runs unprivileged as the workspace owner, and it has no root helper.

## Layout

- Workspace: the subvolume `~/work/<name>`. `~/work` is itself a subvolume (a plain directory off btrfs), made at boot by `panda-ckpt-init-<user>.service` (tmpfiles `v` would make a plain directory unless `/` itself is a btrfs subvolume).
- Checkpoint: the read-only snapshot `~/.checkpoints/<ws>/<id>`, plus a `<id>.json` file with
  `{id, created, label, reason, parent, sizeEstimate}`.
  - `reason` is `manual`, `auto` or `pre-restore`.
  - `parent` is the checkpoint that the workspace derived from. For a fork it is `<ws>/<id>`.
  - `sizeEstimate` is the apparent size of the regular files. It is not exclusive extent usage, because that needs qgroups and root.
- `~/.checkpoints/<ws>/.head` records the current parent. `~/.checkpoints/.lock` serialises the CLI, the MCP server and the timer (flock).
- Names match `[A-Za-z0-9._-]{1,64}` and must not start with `.`. Names that start with `.` in `~/work` are reserved for temporary items: `.<ws>.restore` and `.<ws>.adopt`.
- Every path component is checked with lstat. Trees are walked without following symlinks, and files are opened with `O_NOFOLLOW`.

## Commands (all accept `--json`)

```
panda-ckpt init <name>
panda-ckpt adopt ~/work/<name>          # plain dir -> subvolume: create, cp --reflink, swap, drop old
panda-ckpt create <ws> [-m label]
panda-ckpt list [<ws>]
panda-ckpt diff <ws> <id> [<id2>|current]
panda-ckpt restore <ws> <id>            # safety checkpoint, then renameat2(RENAME_EXCHANGE)
panda-ckpt fork <ws> <id> <newname>
panda-ckpt prune <ws> [--keep N] [--older-than 7d]
panda-ckpt gc                           # leftovers of interrupted restore/adopt/create
panda-ckpt auto [--keep-auto 24]        # what the timer runs
panda-ckpt mcp                          # ckpt_create, ckpt_list, ckpt_diff, ckpt_restore (confirm:true), ckpt_fork
```

- `diff` compares trees by type, mode, size, mtime and symlink target, like the rsync quick check. It shows unified diffs (`diff -u`) for text files up to 64 KiB, for at most 100 files.
- `auto` uses the same comparison against the head checkpoint to find changes.
  `btrfs subvolume find-new` is not used, because it needs `CAP_SYS_ADMIN` (`TREE_SEARCH`) and it does not report deletions.
- `prune` deletes checkpoints that are beyond the newest N and older than D. It needs at least one of N or D.
- Automatic retention keeps the newest `keepAuto` auto checkpoints. It never deletes manual or pre-restore checkpoints.

## Privileges and mount options

| Operation | Unprivileged? |
|---|---|
| `subvolume create` | Yes: needs write access to the parent directory. |
| `subvolume snapshot -r` | Yes: the caller must own the source subvolume. |
| Clear `ro` (`property set -ts … ro false`) | Yes: the caller must own the subvolume. |
| `subvolume delete` | Only with the **`user_subvol_rm_allowed`** mount option. |

**The integration must add `user_subvol_rm_allowed` to the btrfs mount that holds `/home`** (the `@home` subvolume).
btrfs mount options apply to the whole filesystem, so also add it to the other btrfs mounts of the same fs, to stay consistent.

Without this option, `panda-ckpt` falls back as follows: it clears `ro`, empties the snapshot, then runs `rmdir` on it. Kernels from 4.18 let the owner `rmdir` an empty subvolume. The fallback works, but it is slower than a delete.
No other mount option is needed, and the module needs no polkit or sudo rule.

## NixOS

```nix
services.panda-ckpt = { enable = true; users = [ "panda" ]; interval = "10min"; keepAuto = 24; };
```

The module provides:

- `panda-ckpt-init-<user>.service` (root, oneshot) for `~/work`, and a tmpfiles rule for `~/.checkpoints` (mode 0700).
- `panda-ckpt-auto-<user>.{service,timer}`, run as the user, with write access only to their home.
- The package.
- `/etc/panda/mcp.d/panda-ckpt.json`.

`vmtest.nix` is the NixOS VM test. It uses a real btrfs disk.
