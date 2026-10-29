# Checkpoints: panda-ckpt

> **New in Unreleased.** On by default (`services.panda-ckpt.enable`).

`panda-ckpt` keeps instant copy-on-write checkpoints of agent workspaces on btrfs. You can diff,
restore and fork them. It runs as the workspace owner and has no root helper. Agents call the same
operations through its MCP server.

## Workspaces

A workspace is a btrfs subvolume at `~/work/<name>`. Boot creates `~/work` (a subvolume) and
`~/.checkpoints` (mode 0700) for each user in `services.panda-ckpt.users`. The default is
`[ "agent" ]`, the user that PandaCode's agents run as, so `~` here is `/var/lib/agent`. Add `"panda"`
to the list to checkpoint your own workspaces too.

```sh
panda-ckpt init myproj              # new empty workspace ~/work/myproj
panda-ckpt adopt ~/work/oldproj     # convert an existing plain directory in place
panda-ckpt list                     # workspaces
```

Names match `[A-Za-z0-9._-]{1,64}` and must not start with `.`, and `_ext` is reserved. `adopt` refuses
while a process of the same user has its working directory or an open file inside the directory,
because those processes would keep using the old tree. `--confirm` overrides this.

## External repos

PandaCode, or you, often check out repos outside `~/work`. Moving them would break the tools that
use them, so panda-ckpt checkpoints them in place:

```sh
panda-ckpt discover                       # git repos up to 4 levels below ~, outside ~/work
panda-ckpt create ~/src/myrepo -m before  # any command accepts an absolute or ~/ path for <ws>
panda-ckpt list                           # {workspaces, external}
```

A checkpoint of an external directory is a subvolume `~/.checkpoints/_ext/<slug>/<id>`, filled with
`cp -a --reflink=always` (`.git` included) and then made read-only. It shares extents with the
original, so it costs almost no space at first, but it takes longer than a snapshot because it copies
every file. `restore` first takes a safety copy, then swaps a reflinked copy into place with
`renameat2(RENAME_EXCHANGE)`. The path never changes. The auto timer also checkpoints changed
discovered repos (`services.panda-ckpt.external`, on by default). `discover` skips caches and
toolchains (`.cache`, `node_modules`, `.cargo`, ...) and never follows symlinks.

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
Tools: `ckpt_create`, `ckpt_list` (`limit`, default 20), `ckpt_diff`, `ckpt_fork`, `ckpt_init`,
`ckpt_adopt`, `ckpt_discover`, and `ckpt_restore`, which refuses to run without `"confirm": true`.
Every tool carries MCP `annotations` (read-only, destructive, idempotent hints).

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

On a non-btrfs filesystem, `~/work` is a plain directory, and `init`/`create` fail with an error.

## Threat model: same-uid checkpoints vs. root snapshots

Checkpoints protect against **mistakes**: a bad refactor, a deleted file, a script that went wrong.
They are **not tamper-resistant**. A checkpoint belongs to the same uid as the agent, so a rogue
agent can clear `ro` and delete its own checkpoints, as it can delete any other file it owns. The
`confirm: true` on `ckpt_restore` guards against accidents. It is not a security boundary.

The tamper-resistant layer is btrbk: hourly snapshots in `/.snapshots`, made and owned by root
(24 hourly, 7 daily). An agent cannot delete them. Because btrfs snapshots are not recursive, each
subvolume needs its own entry:

| Snapshot | Covers |
|---|---|
| `home.<time>` | `/home`, your files |
| `mem.<time>` | `/mem` |
| `agent.<time>` (`btrbk-agent`) | `/var/lib/agent`: CLI logins, PandaCode data, external repos, `~/.checkpoints` metadata |
| `agent-work.<time>` | `~/work` itself, for example plain directories that are not workspaces |
| `agent-workspaces/<ws>.<time>` | each writable workspace `~/work/<ws>`. The glob skips read-only subvolumes, so checkpoints are not copied again |

`/var/lib/agent` is a subvolume because tmpfiles creates it with `v` on installed boxes, where `/` is
the `@` subvolume. If it is a plain directory instead, `btrbk-agent` is skipped and logs that the agent
home is **not** snapshotted. Remaining gaps:

- Workspaces of other `services.panda-ckpt.users` under `/home` (for example `panda`'s) appear in
  `home.<time>` only as empty directories.
- A workspace that is created and deleted within the same hour is never snapshotted.
- Snapshots are on the same disk. They do not protect against disk failure.

## Checkpoints vs. snapshots

| | panda-ckpt | btrbk snapshots |
|---|---|---|
| What | one workspace in `~/work`, or one external repo | `/home`, `/mem`, the agent home and its workspaces |
| When | on demand, plus every 10 min if changed | hourly |
| Who | the user or agent, unprivileged | root (agents cannot delete them) |
| Restore | `panda-ckpt restore` | `panda-rollback-mem` (for `/mem`), or by hand |

See [updates-and-rollback.md](updates-and-rollback.md) for system rollback.
