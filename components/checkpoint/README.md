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
- Names match `[A-Za-z0-9._-]{1,64}`, must not start with `.`, and `_ext` is reserved. Names that start with `.` in `~/work` are reserved for temporary items: `.<ws>.restore` and `.<ws>.adopt`.
- Every path component is checked with lstat. Trees are walked without following symlinks, and files are opened with `O_NOFOLLOW`.

## Commands (all accept `--json`)

`<ws>` is a workspace name in `~/work`, or the absolute or `~/` path of a directory elsewhere in the home (see "External directories").

```
panda-ckpt init <name>
panda-ckpt adopt ~/work/<name> [--confirm]   # plain dir -> subvolume: create, cp --reflink, swap, drop old
panda-ckpt create <ws> [-m label]
panda-ckpt list [<ws>] [--limit N]      # no <ws>: {workspaces, external}
panda-ckpt discover                     # git repos outside ~/work
panda-ckpt diff <ws> <id> [<id2>|current]
panda-ckpt restore <ws> <id>            # safety checkpoint, then renameat2(RENAME_EXCHANGE)
panda-ckpt fork <ws> <id> <newname>
panda-ckpt prune <ws> [--keep N] [--older-than 7d]
panda-ckpt gc                           # leftovers of interrupted restore/adopt/create
panda-ckpt auto [--keep-auto 24] [--no-external]   # what the timer runs
panda-ckpt mcp
```

MCP tools are `ckpt_create`, `ckpt_list` (`limit`, default 20), `ckpt_diff`, `ckpt_restore` (needs `confirm:true`), `ckpt_fork`, `ckpt_init`, `ckpt_adopt` and `ckpt_discover`.
Every tool carries MCP `annotations` (`readOnlyHint`, `destructiveHint`, `idempotentHint`).

- `adopt` refuses while any process of the same user has its cwd or an open file inside the directory (read from `/proc/*/cwd` and `/proc/*/fd`). Those processes would keep using the old, deleted tree. `--confirm` / `confirm:true` overrides the refusal.

- `diff` compares trees by type, mode, size, mtime and symlink target, like the rsync quick check. It shows unified diffs (`diff -u`) for text files up to 64 KiB, for at most 100 files.
- `auto` uses the same comparison against the head checkpoint to find changes.
  `btrfs subvolume find-new` is not used, because it needs `CAP_SYS_ADMIN` (`TREE_SEARCH`) and it does not report deletions.
- `prune` deletes checkpoints that are beyond the newest N and older than D. It needs at least one of N or D.
- Automatic retention keeps the newest `keepAuto` auto checkpoints. It never deletes manual or pre-restore checkpoints.

## External directories (git checkouts anywhere in the home)

Checkouts made by PandaCode (or by hand) often live outside `~/work`, and moving them would break the tools that use them.
`panda-ckpt` checkpoints them in place:

- `discover` finds git repos up to 4 levels below the home, outside `~/work`. A repo is a directory with a `.git` directory or file.
  It does not follow symlinks or enter repos, and it skips caches and toolchains (`.cache`, `.npm`, `.cargo`, `.rustup`, `node_modules`, …).
- A checkpoint of such a directory is a new subvolume `~/.checkpoints/_ext/<slug>/<id>`, filled with `cp -a --reflink=always` and then made read-only.
  - The `<slug>` is a readable name plus a hash of the home-relative path.
  - The copy includes `.git`.
  - It shares extents with the original, so it is fast and initially costs almost no space. It is not instant like a snapshot, because it scales with the number of files.
- `restore` first takes a safety copy. It then reflink-copies the checkpoint into a sibling directory `.<name>.restore`, swaps it with the original using `renameat2(RENAME_EXCHANGE)`, and removes the old tree.
  The path never changes.
- `auto` (the timer) checkpoints changed discovered repos too (`services.panda-ckpt.external`, default on), with the same `keepAuto` retention.
- Paths must be existing directories inside the home, reached without symlinks, and outside `~/work` and `~/.checkpoints`.

## What checkpoints do and do not protect against

Checkpoints protect against **mistakes**: a bad refactor, a deleted file, a script that went wrong.
They are **not tamper-resistant**. Checkpoints belong to the same uid as the agent, so a rogue agent can clear `ro` and delete its own checkpoints, like any other file it owns.

The tamper-resistant layer is the hourly, root-owned btrbk snapshots in `/.snapshots` (`nixos/modules/snapshots.nix`: 24 hourly and 7 daily).
They cover `/home`, and the agent's home through a second `btrbk-agent` instance when that home lives outside `/home` and is itself a subvolume.
btrfs snapshots are not recursive, so those snapshots contain the plain files of the homes, including external repos.
Nested subvolumes appear there as empty directories, so `btrbk-agent` lists them as extra entries for the agent user (`services.pandacode-host.user`, when panda-ckpt manages it):
`~/work` itself (`/.snapshots/agent-work.<time>`) and each writable `~/work/<ws>` through a glob (`/.snapshots/agent-workspaces/<ws>.<time>`).
The glob skips read-only subvolumes, so panda-ckpt's own checkpoints are not snapshotted again.
Gaps: workspaces of other `services.panda-ckpt.users` under `/home` (for example `panda`'s) are only in the `home` snapshot as empty directories, and a workspace created and deleted within the same hour is never snapshotted.

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
services.panda-ckpt = { enable = true; users = [ "agent" "panda" ]; interval = "10min"; keepAuto = 24; external = true; };
```

The module provides:

- `panda-ckpt-init-<user>.service` (root, oneshot) for `~/work`, and a tmpfiles rule for `~/.checkpoints` (mode 0700).
- `panda-ckpt-auto-<user>.{service,timer}`, run as the user, with write access only to their home.
- The package.
- `/etc/panda/mcp.d/panda-ckpt.json`.

`vmtest.nix` is the NixOS VM test. It uses a real btrfs disk.
