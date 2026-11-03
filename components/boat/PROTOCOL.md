# boat protocols

Two interfaces:

1. **boatd internal API**: HTTP+JSON on `/run/boat/boatd.sock`. boat-api uses it.
2. **Guest agent protocol**: JSON lines over Firecracker hybrid vsock. boatd uses it to talk to `guest/agent/boat-agent.py`.

boatd also writes `/run/boat/routes.json` for boat-gateway (see the last section).

---

## 1. boatd internal API

- Transport: HTTP/1.1 on the unix socket `/run/boat/boatd.sock`. The socket is mode 0660, owner `root:<socketGroup>` (`boat`). boatd does no other authentication, so the socket permissions are the access check. boat-api does tenant checks and must send `team`.
- Success: the status is 200, or 201 for creates, and the body is the object itself (no envelope).
- Error: the status is 4xx/5xx and the body is `{"error": "message"}`.
  - 400: bad input.
  - 404: unknown VM, snapshot or file.
  - 409: wrong state, for example exec on a stopped VM.
  - 429: team quota exceeded.
  - 502: guest agent error.
  - 503: no addresses or ports are free.
- Lifecycle calls are **synchronous**. They return after the operation completes. Create returns when the guest agent answers, or after `bootTimeoutS` (default 60 s).

### VM object

```json
{"id":"vm-1a2b3c4d5e6f","name":"web","team":"t1","image":"ubuntu-24.04","size":"default",
 "vcpu":4,"mem_mib":8192,"disk_gib":51,"labels":{"k":"v"},
 "state":"running","error":"","index":2,"ip":"10.200.0.2","mac":"06:00:0a:c8:00:02",
 "ipv6":"2001:db8:1::2","public_ip":"203.0.113.7","tap":"bt2","pid":1234,
 "has_snapshot":false,"agent_ready":true,
 "ports":[{"guest_port":8080,"host_port":20000,"hostname":"8080-vm-1a2b3c4d5e6f"}],
 "ttl_s":3600,"idle_stop_s":0,"expires_at":"…","parent_id":"","from_snapshot":"",
 "created_at":"…","started_at":"…","updated_at":"…"}
```

- `state` is one of `creating`, `running`, `stopping`, `stopped`, `starting`, `forking`, `error`.
- `has_snapshot`: a stop snapshot exists, so the next start restores it instead of cold booting.
- `agent_ready`: false if the guest agent did not answer, or if setting the identity or env failed. In that case `error` holds the reason. The VM can still run.
- `env` is never stored and never echoed back.

### Endpoints

| Method and path | Body | Result |
|---|---|---|
| `GET /v1/health` | | `{"ok":true}` |
| `POST /v1/vms` | `{name?, size?, image?, ttl_s?, idle_stop_s?, team, labels?, env?, snapshot?}` | 201, VM |
| `GET /v1/vms[?team=]` | | `{"vms":[VM…]}` |
| `GET /v1/vms/{id}` | | VM |
| `POST /v1/vms/{id}/start` | `{env?}` | VM (running) |
| `POST /v1/vms/{id}/stop` | | VM (stopped) |
| `POST /v1/vms/{id}/fork` | `{name?, team?}` | 201, child VM |
| `POST /v1/vms/{id}/delete` or `DELETE /v1/vms/{id}` | | VM with `"state":"deleted"` |
| `POST /v1/vms/{id}/exec` | `{argv` (or `cmd`)`, env?, cwd?, stdin_b64?, timeout_s?` (600)`, tty?, user?}` | NDJSON stream (see below) |
| `PUT /v1/vms/{id}/files?path=/abs&mode=644` | raw bytes | `{"ok":true,"path":…,"bytes":N}` |
| `GET /v1/vms/{id}/files?path=/abs` | | raw bytes (404 if missing) |
| `POST /v1/vms/{id}/env` | `{vars:{K:V}}` | agent reply (`/etc/boat/env`) |
| `POST /v1/vms/{id}/desktop` | `{action:"start\|stop\|status", token?}` | agent reply |
| `POST /v1/vms/{id}/agent` | `{op, ...}`, allowlisted guest ops (see section 2) | guest's final reply |
| `POST /v1/vms/{id}/resize` | `{size?, disk_gib?, discard_memory?}` | VM |
| `POST /v1/vms/{id}/ports` | `{port` (or `guest_port`)`, public?}` | 201, Port |
| `DELETE /v1/vms/{id}/ports?port=N` | | `{"ok":true}` |
| `GET /v1/vms/{id}/snapshots` | | `{"snapshots":[Snapshot…]}` |
| `POST /v1/vms/{id}/snapshots` | `{name?}` | 201, Snapshot |
| `DELETE /v1/vms/{id}/snapshots/{sid}` | | `{"ok":true}` |
| `GET /v1/vms/{id}/snapshots/{sid}/download` | | tar.zst stream |
| `GET /v1/snapshots[?team=]` | | `{"snapshots":[Snapshot…]}` |
| `POST /v1/snapshots/import?team=&name=[&image=&size=]` | tar.zst stream | 201, Snapshot |
| `POST /v1/snapshots` | `{vm_id, name?}` | 201, Snapshot |
| `DELETE /v1/snapshots/{sid}` | | `{"ok":true}` |
| `GET /v1/events[?team=&after=seq]` | | SSE stream (see below) |
| `GET /v1/usage[?since=RFC3339\|unix&team=]` | | usage report (see below) |

Notes:

- **create**
  - `name` matches `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` and defaults to the id. It is the guest hostname.
  - `team` matches `^[A-Za-z0-9_.-]{1,64}$` and defaults to `default`.
  - `size` is a key of the `sizes` config: small 2/4G/51G, default 4/8G/51G, large 8/16G/111G, xlarge 16/32G/251G.
  - `image` is a directory `stateDir/images/<image>/{vmlinux,rootfs.ext4,meta.json?}`. An optional `meta.json` `{"boot_args": "..."}` replaces the default kernel cmdline.
  - `snapshot` creates the VM from a saved snapshot of the same team. The VM inherits the snapshot's size, and restores its memory if the snapshot has memory.
  - `env` is pushed with `set_env`.
- **ttl_s** is counted from each start. When it expires, boatd **stops** the VM. **idle_stop_s** overrides the global `idleStopS`; -1 means never. Idle means no exec, file or desktop calls through boatd, and an agent `idle_s` (exec, ssh sessions, desktop input) above the limit. The reaper runs every 30 s. A VM whose agent cannot be reached is never idle-stopped.
- **stop**
  - boatd syncs the guest, pauses the VM, takes a snapshot (mem + vmstate, see "Memory snapshots") into the VM dir, then kills firecracker. Billing stops.
  - If the snapshot fails or the disk is below its free-space floor, boatd sends the agent `shutdown`, waits up to 30 s, then kills the VM. The next start is then a cold boot.
- **start**
  - With a snapshot: boatd restores it (load + resume), consumes `vmstate` (`mem` stays as the backing file), and resets the guest clock with `exec date -s @now` as root.
  - Without a snapshot: cold boot.
  - If the restore fails: cold boot.
- **fork**
  - Running parent: boatd pauses it, snapshots it, reflink-copies disk + mem + vmstate into the new VM, then resumes the parent. The pause lasts as long as writing the dirty pages and reflinking the disk.
  - Stopped parent: boatd copies the stop snapshot, or only the disk if there is none.
  - Then boatd restores the child on its own tap and sends `set_identity` with the child's IP, MAC and hostname. The agent also regenerates machine-id and ssh host keys.
  - `team`, if given, must equal the parent's team, else 404.
- **resize**: vCPU and RAM change only on a stopped VM (409 while running), and only through a cold boot. A memory snapshot cannot change shape, so a VM with a stop snapshot also needs `discard_memory:true` (409 otherwise). The disk grows at any time and never shrinks: `disk_gib` below the current size is 400, and a `size` with a smaller disk keeps the larger one. A running guest sees the new disk at once (`PATCH /drives`, `resize2fs`); a stopped one on its next start.
- **snapshot export/import**: `download` streams GNU sparse tar.zst, manifest first. Firecracker does not validate `vmstate`, so memory crosses only under an HMAC from this host's key (`stateDir/export.key`, covering the snapshot metadata and the SHA-256 of vmstate, mem and kernel). Anything else is treated as a tenant upload: the disk is kept (only the guest kernel parses it), vmstate, memory and kernel are dropped, and the snapshot cold-boots on the local image's kernel (`image`, `size` choose it). Import accepts only the expected regular files, each once and within a size cap; other members are 400. It stops below the free-space floor. Imported snapshots have an empty `vm_id` and outlive VM deletes. boatd trusts `team`: boat-api enforces it.
- **delete** kills the VM and removes its dir, its tap and its snapshots.
- **ports**: re-adding a port sets its visibility, so `public:false` closes a published port and flushes its flows. Limits: `maxPortsPerVM` (default 32), team `maxPorts` (default 512), both 429. Every port gets a gateway hostname route, `<guest_port>-<id>.vm.<domain>`; the `hostname` field holds the label before `.vm.<domain>`. With `public:true` the port also gets a raw TCP `host_port` from `portMin..portMax` (20000-40000), DNATed to `ip:guest_port`.
- **quotas**: see "Admission and storage limits" below.

### exec stream

- `Content-Type: application/x-ndjson`. boatd relays the agent's lines verbatim:
  - `{"stream":"stdout"|"stderr","data_b64":"…"}`, repeated
  - then one final `{"exit":N,"timed_out"?:true,"signal"?:"SIGKILL"}` or `{"error":"…"}`.
- A transport failure in the middle of the stream ends it with `{"error":"guest agent: …"}`.

### events (SSE)

```
id: 42
event: vm.state
data: {"seq":42,"time":"…","type":"vm.state","vm_id":"vm-…","team":"t1","size":"default","state":"stopped","prev":"stopping","reason":"idle"}
```

- `type` is one of `vm.state`, `vm.deleted`, `snapshot.created`, `snapshot.deleted`.
- `reason` is one of `created`, `api`, `ttl`, `idle`, `exited` (guest powered off or crashed), `failed`, `deleted`, `boatd shutdown`, `boatd restart`.
- To resume after a disconnect, send `Last-Event-ID` or `?after=`. boatd keeps the last 1024 events in memory. Sequence numbers restart when boatd restarts, so after a gap, re-list the VMs.
- A `: keepalive` comment is sent every 25 s.

### usage

```json
{"since":"…","until":"…",
 "vms":[{"id":"vm-…","team":"t1","size":"default","vm_seconds":3600}],
 "snapshots":[{"id":"snap-…","team":"t1","snapshot_days":0.5}],
 "teams":[{"team":"t1","vm_seconds":3600,"vm_seconds_by_size":{"default":3600},"snapshot_days":0.5}]}
```

- VM-seconds count time in `running`. Closed intervals are stored in `stateDir/usage.jsonl`, and they survive VM deletion. Intervals that are still open are counted up to now.
- Snapshot-days count named snapshots from creation until deletion.

---

## 2. Guest agent protocol (vsock)

The host side uses the Firecracker hybrid vsock:

1. Connect to the VM's unix socket (`<vm root>/v.sock`).
2. Send `CONNECT 1024\n`.
3. Read a line `OK <host port>\n`.
4. The connection is now a byte stream to guest vsock port 1024.

The guest agent listens on `AF_VSOCK`, CID any, port 1024.

Framing:

- One JSON object per line, UTF-8, `\n`-terminated, in both directions.
- A connection may carry many requests, one after another. A request is answered completely before the next request is read.
- Every reply sequence ends with one **final** line: one that contains `"ok":true`, `"exit"`, `"eof":true` or `"error"`.
- An error is `{"error":"message"}`. A missing file gives a message that contains "No such file" (boatd maps it to 404).
- Lines are at most 64 MiB guest-side. boatd rejects any line from the guest longer than 8 MiB, because it treats the guest as untrusted.

| op | request | reply |
|---|---|---|
| `health` | `{"op":"health"}` | `{"ok":true,"uptime":s,"idle_s":s,"version":"1"}` |
| `exec` | `{"op":"exec","argv":[…],"env"?:{},"cwd"?:"/","stdin_b64"?:"…","timeout_s"?:600,"tty"?:false,"user"?:"dev"}` | `{"stream":"stdout\|stderr","data_b64":"…"}`… then `{"exit":N,"timed_out"?:true,"signal"?:"…"}` |
| `put_file` | `{"op":"put_file","path":"/abs","mode":"644","data_b64":"…","offset":N,"last":bool,"owner"?:"user[:group]"}`, one line per chunk | `{"ok":true,"size":N,"complete":bool}` for each chunk |
| `get_file` | `{"op":"get_file","path":"/abs"}` | `{"offset":N,"data_b64":"…"}`… then `{"eof":true,"size":N,"mode":"644"}` |
| `set_identity` | `{"op":"set_identity","hostname":"web","ipv4_cidr":"10.200.0.5/16","gw":"10.200.0.1","mac":"06:00:0a:c8:00:05","dns":["1.1.1.1"],"ipv6_cidr"?:"2001:db8::5/64","ipv6_gw"?:"2001:db8::1"}` | `{"ok":true,"ms":{"network":N,"secrets":N}}` |
| `set_env` | `{"op":"set_env","vars":{"K":"V"}}` | `{"ok":true,"count":N}` |
| `desktop` | `{"op":"desktop","action":"start\|stop\|status","token"?:"…"}` | `{"ok":true,"port":6080,…}` |
| `shutdown` | `{"op":"shutdown"}` | `{"ok":true}`, then the guest syncs and reboots (= firecracker exits) |
| `cmd_start` | `{"op":"cmd_start","argv":[…],"env"?:{},"cwd"?:"/","user"?:"dev"}` | `{"ok":true,"cmd_id":"…"}` |
| `cmd_list` | `{"op":"cmd_list"}` | `{"ok":true,"cmds":[status…]}` |
| `cmd_status` | `{"op":"cmd_status","cmd_id":"…"}` | `{"ok":true,"cmd_id","argv","state":"running\|exited\|lost","pid","exit"?,"signal"?,"started","ended"?}` |
| `cmd_logs` | `{"op":"cmd_logs","cmd_id":"…","offset"?:0,"limit"?:1048576}` | `{"ok":true,"data_b64","offset","next_offset","size","truncated","running"}` |
| `cmd_signal` | `{"op":"cmd_signal","cmd_id":"…","signal":"TERM"}` | `{"ok":true}` |
| `file_list` | `{"op":"file_list","path":"/abs"}` | `{"ok":true,"entries":[{name,type,size,mode,mtime}],"truncated"}` |
| `file_delete` | `{"op":"file_delete","path":"/abs","recursive"?:false}` | `{"ok":true}` |
| `file_move` | `{"op":"file_move","from":"/a","to":"/b","overwrite"?:false}` | `{"ok":true}` |
| `file_search` | `{"op":"file_search","path":"/abs","query":"…","limit"?:100,"regex"?:false}` | `{"ok":true,"matches":[{path,line,text}],"truncated"}` |
| `reseed` | `{"op":"reseed","entropy_b64":"<32..4096 bytes>","generation"?:N}` | `{"ok":true,"bytes":n}` |
| `sync` | `{"op":"sync","drop_caches"?:false}` | `{"ok":true}` after sync(2) (and dropping clean page cache) |
| `set_clock` | `{"op":"set_clock","unix_ns":N}` | `{"ok":true}` |

Semantics:

- **exec**
  - Runs as `user` (default `dev`) in a new session.
  - If `timeout_s` passes, the agent sends SIGTERM, then SIGKILL after 2 s, and `timed_out` is set.
  - With `tty`, there is a single `stdout` stream, and stdin gets ^D after `stdin_b64`.
- **put_file**
  - Chunks go to `<path>.boat-part`. `offset` must equal the bytes received so far, and `offset:0` truncates.
  - The chunk with `last:true` sets the mode (an octal string or an int), sets the owner (default: like the parent dir), and renames the file into place.
  - boatd sends 1 MiB chunks.
- **cmd_\***, **file_\***: the ops `POST /v1/vms/{id}/agent` passes through; nothing else is allowed there. Commands outlive the connection. Log offsets are absolute in the merged stdout+stderr stream, with the newest ~64 MiB kept; `cmd_logs` `limit` is at most 4 MiB. `state:"lost"` means the supervisor died, for example across a reboot. `file_delete` refuses `/`, and a non-empty directory without `recursive`. An unknown `cmd_id` or missing path gives an error containing "No such" (404).
- **reseed**: the agent credits the bytes to the kernel pool with `RNDADDENTROPY`, then forces a CRNG reseed with `RNDRESEEDCRNG`. boatd sends 64 bytes from its own `crypto/rand` right before every `set_identity`, that is, for every new VM (create, fork, from-snapshot). `set_identity` refuses to run unless a reseed came after the previous `set_identity`, and each reseed is good for one `set_identity`. A snapshot holds the parent's used-up state, so siblings forked from one snapshot each derive their machine-id and host keys from fresh entropy. In addition, Firecracker ≥ 1.10 (x86_64) bumps VMGenID on every snapshot load, and the 6.1 `vmgenid` driver reseeds the kernel CRNG at that point; that event is invisible to userspace. Old agents reply `unknown op`, which boatd logs and ignores.
- **sync**: flushes every guest filesystem. boatd calls it before each pause/snapshot (stop, fork, named snapshot). For agents without this op, it falls back to `exec ["sync"]` as root. Only template builds set `drop_caches`.
- **set_clock**: sets `CLOCK_REALTIME` in the agent process. boatd calls it after every restore. For agents without this op, it falls back to `exec date` (150 ms of cold pages per restore).
- **set_identity**
  - The agent sets the MAC (link down, set address, up), flushes and adds IPv4 (and IPv6), sets the default route(s), `/etc/resolv.conf`, hostname and `/etc/hosts`.
  - It **always** regenerates `/etc/machine-id` and the ed25519 and ecdsa ssh host keys (no RSA host key), and they are on disk before the reply. sshd restarts with `--no-block`, so it may refuse connections for a moment after the reply. `ms` holds the guest-side step timings.
  - It saves the identity to `/etc/boat/identity.json` and re-applies it at every boot, which overrides the kernel `ip=`.
  - boatd sends it once for each new VM (create, fork, from-snapshot), never on start.
- **set_env** writes `/etc/boat/env` (mode 0600, owner the exec user) as `export K='V'` lines. It is meant for team secrets.
- **Activity**: the agent's `idle_s` is the time since the last exec/file activity. It is 0 while an exec runs or an ssh session is logged in. When a desktop runs, it is the lower value of that and the desktop input idle time.

---

## 3. Host side: design and security model

### Layout

```
stateDir/images/<image>/{vmlinux,rootfs.ext4,meta.json}
stateDir/vms/<id>/vm.json                       state, atomic write (tmp+fsync+rename)
stateDir/vms/<id>/firecracker.log
stateDir/vms/<id>/<root>/{rootfs.ext4,vmlinux,fc.sock,v.sock,vmstate?,mem?}
    <root> = firecracker/<id>/root (jailer chroot) or root (no jailer)
stateDir/snapshots/<sid>/{snap.json,rootfs.ext4,vmlinux,vmstate?,mem?}
stateDir/warm/<image>@<size>/{warm.json,rootfs.ext4,vmlinux,vmstate,mem}   warmPool templates
stateDir/usage.jsonl
runDir/routes.json
```

- The disk is a `cp --reflink=always` copy of the image rootfs (instant on btrfs, see "Performance and density" for NOCOW). A cold-booted disk is then grown with `truncate` to the size's `disk_gib`; the guest grows its filesystem on first boot.
- boatd gives Firecracker only **relative** paths (`rootfs.ext4`, `v.sock`, `vmstate`, `mem`), and Firecracker runs with that dir as its cwd or chroot. Because of this, a snapshot never names a host path, and a copy of the dir restores anywhere. That is how forks and snapshots work.
- Every VM's tap is named `tap0` inside its own netns, so restored VMs and forks all find theirs. boatd still passes `network_overrides` on restore (Firecracker 1.12 or later).

### Memory snapshots (stop, fork, named snapshots)

- `root/mem` is the VM's memory as of its last snapshot point. A restored VM is mmapped from it (MAP_PRIVATE, never written). After a cold boot it is absent, which stands for all zeros.
- With `diffSnapshots` (default on) VMs run with `track_dirty_pages`. A snapshot is a Firecracker `Diff`: only pages dirtied since the last snapshot point go into a sparse file. boatd merges that file's data extents (`SEEK_DATA`/`SEEK_HOLE`) onto a `cp --reflink` copy of `root/mem` and renames the result over `root/mem`. The cost follows dirty memory, not VM size.
- `root/mem` is replaced, never modified in place, so a running VM's backing file and its forks' copies stay intact. That makes it safe to share: memory files are sealed (root-owned, mode 0444) and hard-linked between a VM, its forks and its snapshots, so they cost no flush and no space. They stay clones across team boundaries, and across btrfsQgroups subvolumes.
- Order of a fork or named snapshot: `sync`, pause, Firecracker writes the diff with `sync_snapshot_files:false`, disk reflink, resume, merge, copies. The guest is paused for the dump and the reflink only. Writeback of the dump runs behind the call, so the next merge is likely to find its base clean: a reflink of a dirty file waits for that file's writeback. A stop keeps Firecracker's fsync, since its file is the next start's base.
- The dump's page cache is charged to the VM's cgroup, usually close to `memory.high`, which throttled it to disk speed. `memory.high` and `memory.max` are lifted for the dump and restored once the file is on disk. A paused guest allocates nothing and a running one cannot pass its RAM, so this window adds only host page cache. A restarted boatd re-applies them to the VMs it adopts.
- A saved snapshot is usable when the API returns, and durable shortly after: `snap.json` carries `unsynced:true` until its memory is fsynced. After a host crash an unsynced snapshot keeps its disk and loses its memory (it restores cold), and a `snap.json` torn by the crash removes the snapshot.
- If a snapshot fails after Firecracker may have cleared its dirty bitmap, `diff_ok` goes false and the next snapshot is `Full`.
- boatd asks the guest to `sync` before each pause. Phase timings are logged: `snapshot (diff) sync= pause= write= written=`, `snapshot install merge=`, `freeze disk= rest=`.
- `vmstate` is consumed by a restore; `mem` stays until the next snapshot or cold boot.

### Admission and storage limits

- **Team quotas** (`teams.<team>` or `defaultTeam`, 429): `maxVMs`, `maxVCPU`, `maxMemMiB` count active VMs. `maxTotalVMs` counts VMs in any state. `maxSnapshots` counts snapshots. `maxDiskGiB` limits committed storage: each VM's disk + RAM size, and each snapshot's disk (+ RAM size if it has memory).
- **Host admission** (503): active VM RAM across all teams may not exceed (`hostMemMiB`, default MemTotal, minus `hostReserveMiB`, default 1024) × `memOvercommit` (default 1.5). Above 1.0, a new VM also needs real memory: `MemAvailable` ≥ `hostReserveMiB` + its RAM / `memOvercommit`. Active vCPUs may not exceed `hostCPUs` × `cpuOvercommit` (default 4.0). Enforced on create, fork and start.
- **Free-space floor** (507): create is refused when stateDir's free space is below the larger of `minFreeGiB` (5) and `minFreePct` (10%); fork and snapshot also require room for the VM's RAM. A stop below that level skips the snapshot and shuts the guest down cleanly (`has_snapshot:false`, the next start is a cold boot).
- **Defaults are finite**: `defaultTeam` starts at `maxTotalVMs` 100, `maxSnapshots` 200, `maxDiskGiB` 4096, `maxPorts` 512. Keys under `teams.<team>` replace these: 0 there means unlimited.
- **btrfs qgroups** (`btrfsQgroups`, default off because they slow reflinks: fork 1.31 s without, 3.25 s with, run 41305928387): each VM dir is a subvolume with an exclusive-bytes limit of disk + 2 × RAM + 1 GiB. Past it, the guest's disk writes fail instead of filling the host. Reflinked data shared with the image or parent does not count.
- **Per-VM limits**: `diskMBps` and `diskIOPS` (default 0, none: the old 50000 IOPS cap sat just above what the CI runner measured, and below any NVMe) are Firecracker drive limiters, `ingressMbit` its rx limiter (these apply from the next cold boot; a restored VM keeps those in its snapshot). `ioMaxMBps`/`ioMaxIOPS` set cgroup `io.max` on stateDir's disk (default off). `ctPerVM` (default 32768) refuses new forwarded connections past that count; counts restart whenever the rules reload, so it is a limit on bursts between VM changes, not an absolute one.
- **Logs**: guest serial and VMM output go to `firecracker.log` (O_APPEND, so no reader is needed and a boatd restart does not disturb it). boatd checks it every second and past 16 MiB moves the last 1 MiB to `firecracker.log.1` and truncates. Bytes written while boatd is down are bounded only by the disk until it returns. Both files count in `/v1/usage`, and `vmCommit` reserves room for them.
- `GET /v1/health` reports `conntrack_flush_errors`: flushes that failed, so stale flows may survive.
- `GET /v1/usage` adds `storage[]` per team (`vms`, `snapshots`, `committed_bytes`, `vm_apparent_bytes`, `vm_allocated_bytes`, `snapshot_apparent_bytes`, `snapshot_allocated_bytes`) and, when not filtered by team, `disk` (`free_bytes`, `total_bytes`, `floor_bytes`). Allocated bytes count reflinked extents once per file, so they are an upper bound.

### Performance and density

**Disk**

- `diskEngine` (default `auto`): Firecracker's `Async` engine (io_uring), and `Sync` when Firecracker refuses it (host kernel < 5.10.51, io_uring disabled). `Async` is a Firecracker developer preview. It is saved in snapshots, so a restore on a host without io_uring fails and a start falls back to a cold boot.
- `diskNoCow` (default on): at startup boatd rewrites each image's rootfs as a btrfs NOCOW file, once. `cp --reflink` carries the flag, so VM disks and their forks are NOCOW too. On other filesystems it does nothing. CoW turned each random guest write into an extent, and a reflink is O(extents), with the source VM paused: 4.5 s after boatbench's fio.
  - The trade-off: NOCOW data has **no btrfs checksums** and no compression, so bitrot on a VM disk is not detected by btrfs. Run a guest filesystem or the host's RAID scrub if that matters. Reflink sharing still works: the first write to a shared extent copies it once, then writes go in place. A guest writing randomly into never-written parts of its sparse disk still allocates one small extent per write: 337k extents and a 2.2 s reflink in the worst case measured (run 41584046201).
- `hostDirtyRatio` (default 50, 0 leaves it alone): boatd sets the host's `vm.dirty_ratio` at startup, so that a memory dump goes to the page cache instead of being throttled to the disk's speed. A host crash can lose more, and VM disks in writeback were never durable before a guest flush anyway (Firecracker's default `Unsafe` cache mode).
- Guest: scheduler `none` on `vda` (udev rule). No `discard` mount option: Firecracker's block device does not offer discard (`discard_max_bytes` = 0 in the guest), so it would do nothing. Space freed inside a guest is not returned to the host while the VM runs.

**Async or Sync?** On the CI runner, in the same run, reads the host has cached favour `Sync` (46-78k against 27-53k rand read IOPS; each read costs one more wakeup through io_uring). Reads that reach the disk favour `Async` (7.2-7.4k against 5.8k IOPS, host cache dropped). Sequential I/O and rand writes differ by less than the runs vary. This runner cannot say which wins on NVMe; `diskEngine: "Sync"` is the switch. `Async` stays the default because a queue depth is the only way past one I/O at a time on a real disk.

**Restore** is lazy already: Firecracker's `File` memory backend mmaps `mem` MAP_PRIVATE and faults pages on demand, which is why a UFFD handler would add a process and no speed here. Every restore gets a new VMGenID from Firecracker, and a create, fork or new-from-snapshot gets fresh host entropy before its new identity (see `reseed`).

**`warmPool`** (default off): a create restores a template instead of booting. boatd boots one VM of each image + size in use (default image + size at startup, others after their first cold create), under team `_warm`, waits for it to settle, drops its page cache and saves a memory snapshot in `stateDir/warm/`. Templates are not VMs in RAM, so an idle pool costs disk only. A template is rebuilt when the image, kernel or Firecracker binary changes, and one that fails to restore is deleted.

- Template VMs share boot-time randomness: kernel and process ASLR, `boot_id`, early userspace state. Keys and machine identity are regenerated from fresh entropy, but ASLR offsets of already-running processes are the same in every VM from one template, across tenants. Keep it off if your threat model counts on them differing.
- Memory files stay per-VM clones here, so tenants share no page cache.

**Density**

- `balloon` (default on): each cold-booted VM gets a virtio-balloon with free page reporting and `deflate_on_oom`, at size 0. The guest reports free memory, Firecracker `MADV_DONTNEED`s it. The kernel reports in about 2 s batches, so it is for idle VMs, not for squeezing a snapshot. Firecracker's free page *hinting* is not used: its documented race can corrupt guest memory.
- Host pressure: when `MemAvailable` falls below `hostReserveMiB`, boatd inflates every running VM's balloon by 10% of its RAM per 5 s (up to 50%), which makes guests give up page cache, and deflates them at twice the reserve. VMs still run at `oom_score_adj` 500, so if that is not enough the host OOM killer takes a VM, not the host. A compromised guest driver can ignore the balloon; the real bound on a VM stays its cgroup.
- `memOvercommit` above 1.0 is a bet that guests stay idle. If they all use their RAM at once, the host OOM killer will kill some.
- `ksmTeam` (default off): page merging for one team's VMs, or `"*"` for all. **KSM is a cross-VM side channel**: a guest can time a write to learn whether another guest holds the same page contents, so it must not span a trust boundary. Linux has no KSM domains, so the only scope that is safe between tenants is one team: only VMs of `ksmTeam` are mergeable, others are not. Use `"*"` only when every team on the host trusts the others. It needs host kernel ≥ 6.4 (`PR_SET_MEMORY_MERGE`, set by re-executing boatd before the jailer) and costs a kernel thread scanning 200 MB/s.

**Measured** on GitHub's runner (EPYC 7763, 4 shared vCPU, 16 GiB, nested KVM, btrfs on a loop file; the host itself does 346-449 MB/s and 13k/75-83k rand write/read IOPS with fio direct). `boatd/e2e-perf.sh` prints `METRIC {"test":"boat-perf",...}` and holds the gates, whose margins are documented there.

| 4 vCPU / 8 GiB | before (boatbench 41573512017) | boatbench 41605624552 | e2e-perf 41609413499 |
|---|---|---|---|
| seq write | 195 MB/s | 710 MB/s | 837 MB/s |
| seq read | 745 MB/s | 3311 MB/s | 5846 MB/s |
| rand write | 29.8k IOPS | 43.2k IOPS | 45.6k IOPS |
| rand read | 44.1k IOPS | 43.9k IOPS | 53.5k IOPS |
| fork | 6.93 s | 1.77 s | 1.50 s |
| snapshot | 17.1 s | 4.68 s | 8.12 s (3.8 GiB written) |
| new-from-snapshot | 7.39 s | 4.57 s | 1.08 s |
| 10 creates, cold | 28.4 s | 25.1 s | 23.2 s |
| 10 creates, `warmPool` | | | 8.12 s |

A snapshot's time follows what the guest dirtied since boot: 3.8 GiB at about 480 MB/s here, which is page cache on this runner's btrfs-on-loop. Cold creates are CPU: 10 boots of 4 vCPUs on 4 shared host CPUs.

Density, idle `default` VMs on the same runner:

| | RSS per VM | fit in an 8 GiB budget, by RSS |
|---|---|---|
| cold boot, balloon or not | 528 MiB | 15 |
| from a `warmPool` template | 264 MiB | 31 |
| cold boot, `ksmTeam` | 528 MiB, minus 337 MiB merged | 42 |

- A guest that used 3 GiB and freed it: RSS 3605 MiB, 20 s later 531 MiB (free page reporting). Without the balloon that memory stays with the VM. This, and not the idle figure, is what the balloon buys.
- These are idle VMs. Nothing above says what a host holds under load, which is why `memOvercommit` caps at 1.5 by default: VM count on a host goes from RAM / 8 GiB to 1.5 x that.
- Cgroup `memory.current` reads about 940 MiB per idle VM (it includes the reclaimable host page cache of the disk).

### Reconcile and restart

- On startup, boatd reads every `vm.json`:
  - a VM in `running` whose firecracker PID is alive (checked through `/proc/<pid>/cmdline`, which contains the VM id) is re-adopted;
  - a VM in any other active state is killed and marked `stopped`. Half-written snapshots are deleted. `has_snapshot` is set only when both files exist. A VM whose disk is missing goes to `error`.
- VM cgroups live under `cgroupParent` (default `boat-vms`, at the cgroup root), outside boatd's unit, so a boatd crash under `KillMode=mixed` touches neither the VMs nor their memory, and the next boatd re-adopts them.
- A VM that was running but whose process is gone (a host crash, or a `cgroupParent` inside the unit) gets `resume_on_boot` and cold-boots from its disk (`resumeCrashed`, default on). A VM the user stopped stays stopped.
- A clean unit stop is different: the unit uses `KillMode=mixed`, so on SIGTERM boatd snapshots every running VM in parallel. It sets `resume_on_boot` on them and restarts them on its next start, including after a host reboot. `TimeoutStopSec` must cover writing the dirty RAM of all VMs to disk.

### Networking

- `boatbr0` has `accept_ra=0`, `accept_redirects=0`, `autoconf=0`, and tenant RAs and redirects are dropped at L2 and L3, so a VM cannot configure the host.
- Bridge `boatbr0` has `10.200.0.1/16`. Each VM gets slot n (2..65534):
  - IP `10.200.n>>8.n&255`
  - MAC `06:00:0a:c8:hi:lo`
  - netns `boat<n>` holding `tap0` (owned by the VM's uid), bridged to `veth0`, whose peer `bt<n>` is the VM's port on `boatbr0`
- Optional `ipv6Prefix` (routed to the host): the bridge gets `prefix::1`, and VM n gets `prefix::n` (on-link prefix length). Inbound IPv6 is limited to established/related traffic and ports published with `public:true`.
- Optional `publicIPv4` pool: 1:1 DNAT/SNAT, so every port of such a VM is reachable (unlike IPv6, where only published ports are). The address stays with the VM until the VM is deleted. The upstream network must route these addresses to the host.
- nftables: boatd regenerates the tables `inet boat` and `bridge boat` from the VM list and loads them atomically (`nft -f -`) on every change.
  - **Host input from VMs**: only DNS to the gateway, ICMP/ICMPv6, and established traffic. Everything else is dropped.
  - **Forward into boatbr0**: established traffic, DNAT (port forwards, public IPv4) and published IPv6 ports. Everything else is dropped.
  - **Egress from VMs**: dropped to RFC 1918, `100.64.0.0/10`, loopback, `169.254.0.0/16` (metadata), `192.0.0.0/24`, `198.18.0.0/15`, `0.0.0.0/8`, `224.0.0.0/3`, and IPv6 `::1`, `fc00::/7` (ULA, including `fd00:ec2::254`), `fe80::/10`, `ff00::/8`, `64:ff9b:1::/48`. TCP 25/465/587 is dropped unless `allowSMTP`. `egressMbit` adds a per-VM cap (default none), policed in the bridge table on the VM's port. nft's token bucket holds one second's worth, so a burst can briefly exceed it; the steady rate is the cap (measured in e2e).
  - **Jail uids**: output from uids `jailUidBase`..`jailUidBase+65534` in the host netns is dropped.
  - **NAT**: masquerade `10.200.0.0/16` out of non-bridge interfaces. A hairpin masquerade lets a VM reach a sibling's public port.
  - **Bridge anti-spoofing** per tap: the source MAC, IPv4, ARP sender IP and IPv6 (assigned address, link-local, `::`) must match. Because of this, a fork is silent until `set_identity` gives it its own MAC and IP.
  - **Bridge isolation**: tap → tap traffic is dropped (ARP is allowed). The exception is taps in the same team when that team has `allowIntraTeam`.
- Rules load before a VM boots, including from a snapshot or a fork. A `bt*` port with no rules passes nothing.
- `aiProxyPort` (default 0, closed) opens one TCP port on the gateway to VMs, for boat-api's AI proxy.
- nftables failures fail closed: boatd refuses to start, and a create, fork or start fails (the VM is killed) if its rules cannot be loaded; port changes report the error.
- Releasing or reusing an address flushes its conntrack entries (`conntrack -D`).
- Requirements: `br_netfilter` must not be loaded (`bridge-nf-call-iptables=1` would send bridged frames through the inet hooks). `ip_forward` is set by the module and by boatd.

### Isolation of the hypervisor

- **Jailer mode (default when `jailer` is set)**: each VM runs under the Firecracker jailer as its own uid/gid `jailUidBase + n` (default 900000+n), chrooted to its VM root, with its own netns (`--netns`, so firecracker sees only `tap0`) and pid namespace (`--new-pid-ns`). The jailer creates the device nodes. Its cgroup is a child of `cgroupParent`, or of boatd's own delegated cgroup; in that case boatd moves itself into a `boatd` leaf so it can enable the controllers. Limits: `memory.high` = RAM + `vmOverheadMiB` (256), `memory.max` = that + `vmPageCacheMiB` (512; the host page cache of the VM's disk writes is charged to its cgroup, and without this headroom a fast writer was OOM-killed), `cpu.max` = vCPUs + 1, `pids.max` = 256. If the controllers cannot be enabled, boatd logs it and VMs run without limits. Firecracker's built-in seccomp filters are on, as they are by default. The VM's files and tap are owned by that uid, so a compromised VMM cannot read other VMs' disks or memory.
- Firecracker runs at `oom_score_adj` 500 (set on launch and on adoption), while the unit keeps boatd at -900, so under memory pressure VMs die before boatd or the host.
- **No-jailer mode** (`jailer: ""`, used by the unit tests and for development): firecracker runs through `nsenter` in the VM's netns as `fcUid`/`fcGid` with cwd = VM root, plus its built-in seccomp and no cgroup limits. All VMs then share one uid, which is weaker isolation. Do not use it in production.
- boatd itself runs as root (taps, nftables, jailer). It treats the guest agent as untrusted: reply lines are capped at 8 MiB, the JSON is decoded strictly, and guest output only reaches the caller of exec/files.
- Input validated at the API boundary: names, team, image (no path traversal: `^[a-z0-9][a-z0-9._-]{0,63}$`), labels, env names, absolute file paths, octal modes, ports 1-65535, and JSON bodies of at most 1 MiB.

---

## 4. routes.json (for boat-gateway)

```json
{"vms":{"vm-…":{"ip":"10.200.0.2","ports":[3000,8080],"public":[8080],"desktopPort":6080,"team":"t1","state":"running"}}}
```

- `public` lists the guest ports published with `public:true`; the gateway requires a route token for every other port.
- Lists only running VMs. Rewritten atomically on every VM, port or state change.
- The gateway routes `<port>-<id>.vm.<domain>` to `ip:port` for the listed ports, and routes the desktop to `ip:desktopPort`.

---

## 5. Measured deviations

- **Firecracker block**: no discard support, see "Performance and density".

- **Nested KVM**: measured in e2e (`METRIC` fields `nestedHost`, `nestedGuestCpus`, `nestedGuestKvm`). On a host with `nested=1`, Firecracker 1.17 passes VMX/SVM to the guest with no CPU template (run 41315786226: 4 of 4 vCPUs). `/dev/kvm` in the guest then depends only on the guest kernel's config, and on the host enabling nested virtualization. Firecracker does not save KVM nested state, so do not expect a stop snapshot or fork taken while a nested VM runs to restore that nested VM.
