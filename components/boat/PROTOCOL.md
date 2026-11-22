# boat protocols

Two interfaces:

1. **boatd internal API**: HTTP+JSON on `/run/boat/boatd.sock`. boat-api uses it.
2. **Guest agent protocol**: JSON lines over Firecracker hybrid vsock. boatd uses it to talk to `guest/agent/boat-agent.py`.

boatd also writes `/run/boat/routes.json` for boat-gateway (see the last section).

---

## 1. boatd internal API

- Transport: HTTP/1.1 on the unix socket `/run/boat/boatd.sock`. The socket is mode 0660, owner `root:<socketGroup>` (`boat`). boatd does no other authentication, so the socket permissions are the access check. boat-api does tenant checks and must send `team`.
- Success: the status is 200, or 201 for creates, and the body is the object itself (no envelope).
- Error: the status is 4xx/5xx and the body is `{"error": "message"}`, plus `"code"` where a caller has to tell refusals apart:
  - `host_capacity` (503): host admission refused the VM: committed RAM or vCPUs over the overcommit limit, too little really free memory, or no free VM address. boatd never queues such a request; it refuses at once.
  - `no_ready_machine` (503): the same refusal for a request with `fail_fast:true`, and a create with `fail_fast` while boatd is still converting its images at startup (a create without it waits for them). boat-api may wait and retry a `host_capacity`; it passes `no_ready_machine` through.
  - 400: bad input.
  - 404: unknown VM, snapshot or file.
  - 409: wrong state, for example exec on a stopped VM.
  - 429: team quota exceeded.
  - 502: guest agent error.
  - 503: no addresses or ports are free, or the host is full (`code`, above).
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
 "created_at":"…","started_at":"…","stopped_at":"…","updated_at":"…"}
```

- `state` is one of `creating`, `running`, `stopping`, `stopped`, `starting`, `forking`, `error`.
- `has_snapshot`: a stop snapshot exists, so the next start restores it instead of cold booting.
- `agent_ready`: false if the guest agent did not answer, or if setting the identity or env failed. In that case `error` holds the reason. The VM can still run.
- `env` is never stored and never echoed back.

### Endpoints

| Method and path | Body | Result |
|---|---|---|
| `GET /v1/health` | | `{"ok":true}` |
| `POST /v1/vms` | `{name?, size?, image?, ttl_s?, idle_stop_s?, team, labels?, env?, snapshot?, fresh_boot?, fail_fast?, count?}` | 201, VM; with `count` > 1: 201, `{"vms":[VM…]}` |
| `GET /v1/vms[?team=&include_pending=true&view=ssh]` | | `{"vms":[VM…]}` (`view=ssh`: lightweight SSH-authority projection) |
| `GET /v1/vms/{id}` | | VM |
| `PATCH /v1/vms/{id}` | `{name?, ttl_s?, extend_s?, labels?, idle_stop_s?, resume_on_boot?}` | VM, as GET (see below) |
| `POST /v1/vms/{id}/start` | `{env?, fail_fast?, ttl_s?, fresh_boot?}` | VM (running) |
| `POST /v1/vms/{id}/stop` | `{force?}` | VM (stopped) |
| `POST /v1/vms/{id}/fork` | `{name?, team?, fail_fast?, labels?, ttl_s?}` | 201, child VM |
| `POST /v1/vms/{id}/fork/prepare` | `{name?, team?, fail_fast?, labels?, ttl_s?, reprovision_ssh?}` | 201, pending unbooted child (boat-api only) |
| `POST /v1/vms/{id}/release` | `{env?, fresh_boot?}` | 200, authorized child; idempotent (boat-api only) |
| `POST /v1/vms/{id}/ssh-policy` | `{version:1, boot_epoch, config, helper_b64}` | 200, `{version, config_digest, boot_epoch, readiness}` (boat-api only) |
| `POST /v1/vms/ssh-policy-batch` | `{items:[{id, version:1, boot_epoch, config, helper_b64}]}` | 200, `{"results":{id:…},"errors":{id:…}}` (boat-api only) |
| `POST /v1/vms/{id}/delete` or `DELETE /v1/vms/{id}` | | VM with `"state":"deleted"` |
| `POST /v1/vms/{id}/exec` | `{argv` (or `cmd`)`, env?, cwd?, stdin_b64?, timeout_s?` (600)`, tty?, user?}` | NDJSON stream (see below) |
| `PUT /v1/vms/{id}/files?path=/abs&mode=644` | raw bytes | `{"ok":true,"path":…,"bytes":N}` |
| `GET /v1/vms/{id}/files?path=/abs` | | raw bytes (404 if missing) |
| `GET /v1/vms/{id}/env` | | `{"ok":true,"vars":{K:V}}`, authoritative guest env (internal only) |
| `POST /v1/vms/{id}/env` | `{vars:{K:V}}` | agent reply (`/etc/boat/env`) |
| `POST /v1/vms/{id}/desktop` | `{action:"start\|stop\|status", token?}` | agent reply |
| `POST /v1/vms/{id}/agent` | `{op, ...}`, allowlisted guest ops (see section 2) | guest's final reply |
| `POST /v1/vms/{id}/resize` | `{size?, disk_gib?, discard_memory?}` | VM |
| `POST /v1/vms/{id}/ports` | `{port` (or `guest_port`)`, public?}` | 201, Port |
| `DELETE /v1/vms/{id}/ports?port=N` | | `{"ok":true}` |
| `GET /v1/vms/{id}/snapshots` | | `{"snapshots":[Snapshot…]}` |
| `POST /v1/vms/{id}/snapshots` | `{name?, reprovision_ssh?}` | 201, Snapshot |
| `POST /v1/vms/{id}/snapshots/prepare` | `{name?, reprovision_ssh?}` | 201, pending snapshot (boat-api only) |
| `POST /v1/vms/{id}/snapshots/{sid}/release` | | 200, authorized snapshot; idempotent (boat-api only) |
| `DELETE /v1/vms/{id}/snapshots/{sid}` | | `{"ok":true}` |
| `GET /v1/vms/{id}/snapshots/{sid}/download[?team=&protected=1]` | | tar.zst stream |
| `GET /v1/snapshots/{sid}/export?team=[&protected=1]` | | tar.zst stream |
| `GET /v1/snapshots[?team=]` | | `{"snapshots":[Snapshot…]}` |
| `POST /v1/snapshots/import?team=&name=[&image=&size=]` | tar.zst stream | 201, Snapshot |
| `POST /v1/snapshots` | `{vm_id, name?}` | 201, Snapshot |
| `DELETE /v1/snapshots/{sid}` | | `{"ok":true}` |
| `GET /v1/events[?team=&after=seq]` | | SSE stream (see below) |
| `GET /v1/usage[?since=RFC3339\|unix&team=&vm_id=&until=]` | | usage report (see below) |

Notes:

- **create**
  - `name` matches `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` and defaults to the id. It is the guest hostname.
  - `team` matches `^[A-Za-z0-9_.-]{1,64}$` and defaults to `default`.
  - `size` is a key of the `sizes` config: small 2/4G/51G, default 4/8G/51G, large 8/16G/111G, xlarge 16/32G/251G.
  - `image` is a directory `stateDir/images/<image>/{vmlinux,rootfs.ext4,meta.json?}`. An optional `meta.json` `{"boot_args": "..."}` replaces the default kernel cmdline.
  - `snapshot` creates the VM from a saved snapshot of the same team. The VM inherits the snapshot's size, and restores its memory if the snapshot has memory.
  - `fresh_boot:true` (with `snapshot`) restores only the snapshot's disk (reflink) and cold boots: the snapshot's memory is never loaded and its `vmstate`/`mem` are not copied into the new VM, so the new VM has no `has_snapshot`. boat-api sends it when the new VM's secret selection is narrower than the snapshot source's. Without `snapshot` it has no effect.
  - `env` is pushed with `set_env`.
  - `fail_fast`: see `no_ready_machine` above. It changes only what a refusal says and that a create does not wait for the startup image conversion; it never makes a request succeed or fail otherwise. The same holds on start and fork.
  - `count` (1-16, default 1): create that many VMs from one request, **all or none**. All of them pass team quota and host admission together, under one hold of boatd's lock, before any boots; if one then fails to boot, all are deleted and the call returns the first error (no VM of the batch survives). A `name` `x` becomes `x-1` … `x-N` (400 if that is too long). count 0 or 1 replies with the VM object as before; more replies `{"vms":[…]}` in order.
- **ttl_s** is counted from each start. When it expires, boatd **stops** the VM. **idle_stop_s** overrides the global `idleStopS`; -1 means never. Idle means no exec, file or desktop calls through boatd, and an agent `idle_s` (exec, ssh sessions, desktop input) above the limit. The reaper runs every 30 s. A VM whose agent cannot be reached is never idle-stopped.
- **PATCH** updates VM metadata and auto-stop/recovery policy. At least one of `name`, `ttl_s`, `extend_s`, `labels`, `idle_stop_s`, `resume_on_boot` must be non-null (`null` is the same as absent). All fields are validated up front and applied atomically (`name`, `ttl_s`, `extend_s`, `labels`, `idle_stop_s`, `resume_on_boot`) — nothing changes unless every part applies and `vm.json` persists cleanly. It runs under boatd's state lock, not the VM's lifecycle lock, so it answers at once even while a stop is dumping memory.
  - `name`: the create rules (lowercase letters, digits, `-`, max 63). 409 if another VM **of the same team** already has that name; a name used in another team is fine, renaming to the current name is a no-op. Create itself does not refuse duplicate names (ids are the key), so a rename can not make a name unique where creates already duplicated it. The guest's hostname was set from the name at its first boot and is not changed.
  - `ttl_s` (0 to 315360000): sets the VM's `ttl_s`. While running, `expires_at` becomes now + `ttl_s`; 0 clears it (no auto-stop). On a stopped VM it applies from the next start.
  - `extend_s` (1 to 315360000): `expires_at` = max(`expires_at`, now) + `extend_s`; `ttl_s` stays as it was, so the next start counts the full `ttl_s` again. 409 if the VM is not running, 400 if it has no auto-stop (`ttl_s` 0).
  - `labels`: replaces the VM's `labels` map (at most 33 keys: up to 32 user labels plus internal `boat.secrets`; keys match `^[A-Za-z0-9_./-]{1,63}$`, values at most 256 bytes; `{}` clears all labels).
  - `idle_stop_s` (-1 to 315360000): sets per-VM `idle_stop_s` (-1 disables idle auto-stop, 0 uses global `idleStopS`).
  - `resume_on_boot` (bool): sets `resume_on_boot`.
  - 400 for an empty body, bad JSON or a value out of range; 404 for an unknown or `pending_copy` VM. The change is persisted in `vm.json` before the reply. No event is published for it (`vm.state` is only for state changes).
- **stop**
  - boatd syncs the guest, pauses the VM, takes a snapshot (mem + vmstate, see "Memory snapshots") into the VM dir, then kills firecracker. Billing stops.
  - If the snapshot fails or the disk is below its free-space floor, boatd sends the agent `shutdown`, waits up to 30 s, then kills the VM. The next start is then a cold boot.
  - `force:true` kills firecracker at once: no guest sync, no snapshot, no `shutdown`. Any stop snapshot from before is removed, so the next start is a cold boot from the disk, which keeps only what the guest had written out (the page cache is lost, as on a power cut). The event's reason is `force`. Use it for a guest that hangs a normal stop.
  - boatd logs each stop's phases: `stop (<reason>|force) snapshot= kill= flush= state= net=` (ms), after the `snapshot (diff|full) sync= pause= write=` line of the snapshot itself.
- **start** (`startVM`)
  - Optional `ttl_s` (0 to 315360000) atomically updates the VM's `ttl_s` and computes `expires_at` (`now + ttl_s`, or `""` when 0) as the VM enters `running` (including in-place when already `running`).
  - With a snapshot: boatd restores it (load + resume), consumes `vmstate` (`mem` stays as the backing file), and resets the guest clock with `set_clock` (falling back to `exec date -s @now` as root on older agents).
  - Without a snapshot: cold boot.
  - `fresh_boot:true` on a stopped VM discards its stop snapshot (`vmstate` and `mem` are deleted, `has_snapshot` becomes false) and cold boots from the disk, never restoring the old memory. boat-api sends it on a start without a secret environment after a run that had secrets, so the old guest RAM cannot reach the new run. On a VM that is not `stopped` or in `error` (for example `running`) it is refused with 409 and the VM is left unchanged.
  - If the restore fails: cold boot.
- **fork**
  - Running parent: boatd pauses it, snapshots it, reflink-copies disk + mem + vmstate into the new VM, then resumes the parent. The pause lasts as long as writing the dirty pages and reflinking the disk.
  - Forks of one running parent serialize through the freeze (dump + reflink + resume): the parent's snapshot files (`mem`, `vmstate`) are single-slot, so only one freeze can write them at a time. A child's disk allocation and boot overlap the next fork's freeze, but the freezes themselves run one after another, so a fan-out of N forks from one running parent pays the freeze N times in sequence. Fork from a stopped parent, or from a saved snapshot, to avoid the freeze.
  - Stopped parent: boatd copies the stop snapshot, or only the disk if there is none.
  - Then boatd restores the child on its own tap and sends `set_identity` with the child's IP, MAC and hostname. The agent also regenerates machine-id and ssh host keys.
  - `team`, if given, must equal the parent's team, else 404.
  - `labels`: when omitted, the child copies only the parent's reserved labels (keys starting `boat.`, such as `boat.secrets`); user labels are never inherited. When given, the child gets exactly that map (`{}` for none).
  - Optional `ttl_s` (0 to 315360000) overrides the child's `ttl_s` (0 clears auto-stop). When omitted, the child gets the default 3600 s, never the parent's `ttl_s`, so a fork of a parent with auto-stop off still stops after an hour. The effective value sets the child's initial `expires_at` atomically on both `fork` and `fork/prepare` (applied when `release` boots the child), without mutating the parent's `ttl_s`.
  - `release` with `fresh_boot:true` discards the child's copy of the parent's memory (`vmstate` and `mem` are deleted from the child, `has_snapshot` becomes false) and cold boots it from its reflinked disk instead of restoring the parent's RAM. boat-api sends it when it releases a child with no secret environment (`no_env`) of a run that had team secrets, so the parent's guest RAM cannot reach the child. The discard happens before the copy is authorized: if the files cannot be removed the release fails and the copy stays pending. The parent's own files are untouched (a hard link is dropped, not the shared inode). The parent is still paused and snapshotted by `fork/prepare` (the disk must be consistent); only the release decides whether the child uses the memory. A replayed release of an already released child does nothing.
  - Optional `reprovision_ssh: true` on `fork/prepare` and `snapshots/prepare` (and `snapshots`): `freeze` still quarantines the running source VM (`ssh_policy_ready: false`) before pausing/resuming its VMM, and skips `restoreSSHAfterPause` after resuming so the source stays quarantined until `boat-api` finishes rotating credentials and calls `POST /v1/vms/{id}/ssh-policy` (avoiding a duplicate guest `verify` + `nft` reload). When `reprovision_ssh` is `false` or omitted, `restoreSSHAfterPause` re-verifies and restores the source VM as before.
- **Managed-credential copy boundary:** boat-api uses the distinct prepare routes above,
  waits for immutable capture, durably records required child policy and revokes both
  source self and AI generations, then explicitly releases the copy. Prepare never boots
  a child; pending snapshots cannot be exported or used by single/batch create. Default
  VM/snapshot lists and VM GET hide pending copies, while `include_pending=true` on list
  routes retains operator cleanup visibility. Pending copies reserve quota and storage;
  deleting a parent also removes its pending snapshots. `pending_copy` is persisted
  separately from lifecycle state; `copy_ready` records completed child materialization.
  Reconcile never adopts or automatically resumes pending copies, and explicit release
  rejects incomplete capture. Unsynced memory retains the normal cold-boot fallback.
  Background snapshot sync preserves the current authorization metadata.
  A registry/revocation failure leaves the copy pending even if deletion fails. A lost
  prepare response leaves an inaccessible copy. After explicit release, a lost response
  can leave an authorized running child or published snapshot; captured parent tokens are
  already durably revoked. The original `Fork`/`CreateSnapshot` methods and immediate
  routes remain for trusted host callers that own credential handling; they do not revoke
  boat-api credentials. The new API never falls back to them when prepare is unavailable.
  Do not downgrade either component after migration: older binaries do not enforce
  pending markers, signing epochs, or credential retirement.
  Before listeners start, boat-api durably upgrades managed tokens to the `copy-v1|`
  HMAC signing domain, versions generation-bound link minters, and bumps known generations
  once; missing historical first-generation records cannot revive legacy tokens. Every
  startup reconfirms durable state and reconciles missed outside-API deletions. Running
  guests receive refreshed env files; processes holding old tokens must reload them.
  Refresh obtains `vars` through the guest `get_env` operation, not VM metadata; it
  preserves custom/user-supplied values and only replaces managed token values. Missing,
  unreadable, malformed or unsupported getters fail without an env write. Older guest
  agents need upgrading for automatic refresh; captured-token revocation still holds.
  The getter is on boatd's private socket and is not a public boat-api route. Both first
  managed-token issuance and later revocations are durable before publication.
  This boundary covers managed synthetic tokens; raw/user-supplied secrets deliberately
  present on disk or in memory are still copied.

- **resize**: vCPU and RAM change only on a stopped VM (409 while running), and only through a cold boot. A memory snapshot cannot change shape, so a VM with a stop snapshot also needs `discard_memory:true` (409 otherwise). The disk grows at any time and never shrinks: `disk_gib` below the current size is 400, and a `size` with a smaller disk keeps the larger one. A running guest sees the new disk at once (`PATCH /drives`, `resize2fs`); a stopped one on its next start. If the guest's online `resize2fs` does not finish within 50 s the answer is 504 `resize_timeout` (boat-api passes it on as such): the disk is already grown and recorded on the host, and `disk_gib` at the current size retries the guest side. Known issue (2026-11-12, cause unproven): in 1 of 14 boat-e2e runs (43313462575, attempt 2) `resize2fs` hung after `PATCH /drives` and ignored SIGKILL, so it was stuck in uninterruptible I/O on `/dev/vda`. boatd now logs the guest's blocked tasks (`sysrq w` + `dmesg`) and the end of `firecracker.log` as `resize diag:` lines, and the agent answers a process that survives SIGKILL instead of waiting on it.
- **snapshot export/import**: `download` (`GET /v1/vms/{id}/snapshots/{sid}/download[?team=&protected=1]`) and `export` (`GET /v1/snapshots/{sid}/export?team=[&protected=1]`) stream GNU sparse tar.zst, manifest (`export.json`) first. Firecracker does not validate `vmstate`, so memory crosses only under an HMAC from this host's key (`stateDir/export.key`, covering the snapshot metadata, `no_env`, `protected`, and the SHA-256 of `rootfs.ext4`, `vmstate`, `mem` and `vmlinux`). Passing `protected=1` (honored only for a snapshot owned by that team) records `"protected": true` inside the HMAC-signed manifest. Anything without a verified manifest is treated as an untrusted tenant upload: the disk is kept (only the guest kernel parses it), `vmstate`, `mem` and `vmlinux` are dropped, and the snapshot cold-boots on the local image's kernel (`image`, `size` choose it). Import returns top-level `"protected": true` on the response `Snapshot` JSON ONLY when `verifyManifest` succeeded with `"protected": true` or when this host's own export registry matches the extracted disk for the same team (never for unknown/untrusted archives and never from labels). To prevent stripping `export.json` from dropping sticky `no_env` policy, boatd persists an export registry at `stateDir/exports.json` (mode `0600`, atomic write, bounded to the newest 2048 entries) mapping `disk_sha256 -> {team, no_env, protected, at}` on every export. At import, the trusted path merges manifest values OR registry restrictions; the untrusted path hashes the extracted `rootfs.ext4` and, if present in `exports.json`, restores `no_env` (and same-team `protected`) while still dropping `vmstate`/`mem`/`vmlinux`. Import accepts only the expected regular files, each once and within a size cap; other members are 400. It stops below the free-space floor. Imported snapshots have an empty `vm_id` and outlive VM deletes. boatd trusts `team`: boat-api enforces it.
- **delete** kills the VM and removes its dir, its tap and its snapshots.
  - A tombstone is made durable before any teardown, in `stateDir/tombstones/<id>`, **outside the team's quota'd subvolume** (`vms/<id>` is a symlink into it): a team at its quota cannot be stopped from deleting by its own full volume. It carries a copy of the VM record. A crash after it makes the next boatd finish the delete; the VM is never resumed. The older marker, a file `deleting` inside the VM dir, is still honored and migrated on start. A tombstone for a dir that is already gone is just cleared.
  - If the dir cannot be removed (EIO, a busy mount), the tombstone stays and the VM goes to the terminal `error` state (`error` says `delete failed: …`): `start` is refused with 409 `vm is being deleted`, and it stays in the VM table, so its resources and storage keep counting against the team while the dir exists. boatd retries the removal in the background (30 s, doubling to 10 min), also after a restart (reconcile loads the VM from the dir, or from the tombstone's record if the dir lost its `vm.json`). Delete again is accepted and retries at once. The first call returns the failure.
- **ports**: re-adding a port sets its visibility, so `public:false` closes a published port and flushes its flows. Limits: `maxPortsPerVM` (default 32), team `maxPorts` (default 512), both 429. Every port gets a gateway hostname route, `<guest_port>-<id>.vm.<domain>`; the `hostname` field holds the label before `.vm.<domain>`. With `public:true` the port also gets a raw TCP `host_port` from `portMin..portMax` (20000-40000), DNATed to `ip:guest_port`.
- **quotas**: see "Admission and storage limits" below.

### exec stream

- `Content-Type: application/x-ndjson`. boatd relays the agent's lines verbatim:
  - `{"stream":"stdout"|"stderr","data_b64":"…"}`, repeated
  - then one final `{"exit":N,"timed_out"?:true,"signal"?:"SIGKILL"}` or `{"error":"…"}`.
- A transport failure in the middle of the stream ends it with `{"error":"guest agent: …"}`.
- The stream survives forks, snapshots and warm captures of its running VM. Firecracker resets the guest's vsock transport on every snapshot, which drops the agent connection inside the guest while the host side stays open. boatd gives each exec a session id and, after each such snapshot, reconnects with `exec_attach`, so the stream carries every line once and ends with the command's own exit. An agent without sessions answers `exec_attach` with `unknown op`, which ends the stream as an error.
- boatd waits at most 15 s for the vsock handshake, so a guest that is not running (a VM left paused) fails an exec at once, not at `timeout_s`+30.

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

- **Aggregate cache.** Without `vm_id`, the existing fleet/team report rounds `since` down to the minute and caches reports for up to five seconds, invalidated by VM/snapshot/ledger changes. `until` requires `vm_id` and `team`; an exact window does not use this cache.
- **Exact per-VM windows.** `GET /v1/usage?team=T&vm_id=ID&since=...&until=...` requires the VM to belong to `T` (otherwise 404), accepts RFC 3339 with fractional seconds or unix seconds, and clips retained running intervals to the exact bounds. Omitted `since` starts at VM creation; omitted/future `until` stops at the snapshot's clock. The answer has `exact: true`, the measured `until`, and `intervals` with historical `size`, `start`, `end`, `id`, `team` and `kind`; its VM/team totals cover only those intervals. One snapshot pins both the closed ledger and the active run. This path bypasses the aggregate cache and storage walk. It scans the retained ledger once, filters by team and VM before retaining records, and deduplicates before clipping (an earlier real stop can precede `since`). The configured retention cutoff or an overlapping compacted team/day makes an older exact query return 501; aggregate totals cannot reconstruct VM identity. Missing legacy records, manually rotated ledgers, and history pruned under an earlier retention policy cannot be reconstructed; do not use this endpoint to certify completeness of such history.
- **Native API accounting.** boat-api `GET /v1/vms/{id}/usage` uses that exact snapshot and returns fractional `vm_seconds` plus `vm_seconds_by_size`, excluding env-error spans from each historical interval. Env-error history has its own 400-day retention. An older boatd without `exact: true` returns 501 through this endpoint. Compatibility `/api/v1/sandboxes/{id}/usage` returns 501 `not_supported` after ownership checks: self-hosted runtime seconds do not establish boat.dev's weighted billing or an account dollar/seconds-per-dollar basis. Use the native endpoint for measured runtime; no dollar amount is fabricated.
- **Stop timestamps.** VM `stopped_at` records the transition to `stopped` and is persisted in `vm.json`. Rename, TTL, labels, resize and other edits while stopped preserve it; a new transition to `running` clears it and refreshes `started_at`. Legacy stopped records without this field use their prior `updated_at` as a fallback, frozen on their next stopped edit; their original stop time cannot be recovered. A dead running VM recovered after a daemon/host restart is stamped at recovery because its actual exit time is unknown.
- VM-seconds count time in `running`. Closed intervals are stored in `stateDir/usage.jsonl`, and they survive VM deletion. Intervals that are still open are counted up to now.
- boatd keeps a sparse in-memory index of `usage.jsonl` (byte offset and latest end time every 1024 records, built at start in the background, or by one scan at the first query if that has not finished), so a query with `since` reads only the records from the last 1024-record block that ends before `since`. A query without `since` reads every record still in the file (see compaction below). A file shortened behind boatd (rotated) is re-indexed.
- **Compaction (bounded growth).** At start and then daily, boatd folds intervals that ended before the retention cutoff into aggregates. The cutoff is `now - retention` rounded down to a UTC midnight. The retention is `usageRetentionDays` in the config (default 400; negative = never compact) or the `-usage-retention-days` flag, which overrides it. An aggregate is one line `{"agg":true,"kind":"vm|snapshot","team":…,"size":…,"day":"<UTC midnight>","nanos":N}`: the time of that kind, team and size inside that UTC day. Intervals that cross midnights are split across days, so totals are exact.
  - Team totals (`teams`) always include the aggregates. They are exact for any `since` on a UTC midnight at or after the aggregate prune cutoff (5 years back by default, see `ledgerAggRetention`; daily aggregates older than that are dropped), which covers billing periods. A `since` of 0 is therefore an all-time total only back to that cutoff. For a `since` inside a compacted day, that day counts pro rata to the part after `since`. Per-VM and per-snapshot rows (`vms`, `snapshots`) cover only the records still in the file, that is, the retention window. Duplicate `(id, start)` VM records are collapsed during compaction, as `summarize` already does.
  - A query without `since` reads the aggregates, which are loaded once and held in memory, plus the retained records. It no longer reads the whole history.
  - Compaction is crash safe. boatd writes `usage.jsonl.compact` (records after the cutoff, then the aggregates), fsyncs it, copies what was appended in the meantime, fsyncs again, renames it over `usage.jsonl` and fsyncs the directory. The ledger lock is held only to pin the length at the start and to copy the small tail and rename at the end, not across the rewrite. A crash before the rename leaves the old ledger untouched, and a stale `.compact` file is ignored and overwritten by the next run. The index of the new file is built during the rewrite and installed under the lock at the rename, so the first read after compaction scans nothing. At start, boatd also builds the index in the background without holding the ledger lock: it pins the size, scans, then installs under the lock after scanning the small tail appended meanwhile (skipped if a read or compaction built it first), so appends never wait for the scan.
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
- An error is `{"error":"message","code":"..."}` (`code` optional for older guest images). When the guest supplies a known `code`, boatd treats it as authoritative and exposes `{"error":"guest agent: <message>","code":"<code>"}`; message heuristics run only when `code` is empty or unknown. For compatibility with older guest images that return `code:"not_found"` with a message starting `"no such user"`, boatd responds with HTTP 400 and `"code":"invalid_user"`.
  - Guest error `code` → boatd HTTP status:
    | Guest `code` | HTTP status |
    |---|---|
    | `invalid_user`, `spawn_failed`, `invalid_argument`, `invalid_request`, `bad_request`, `invalid_path`, `refused`, `forbidden_path`, `protected_path`, `not_a_directory`, `enotdir`, `einval` | `400` |
    | `permission_denied`, `eacces`, `eperm` | `403` |
    | `not_found`, `no_such_file`, `no_such_command`, `enoent` | `404` |
    | `conflict`, `already_exists`, `exists`, `is_a_directory`, `eisdir`, `not_empty`, `directory_not_empty`, `enotempty`, `not_running`, `command_not_running`, `eexist` | `409` |
    | `unsupported`, `not_supported` | `422` |
    | unknown / unmapped `code` (with no matching legacy heuristic) | `502` |
- Lines are at most 64 MiB guest-side. boatd rejects any line from the guest longer than 8 MiB, because it treats the guest as untrusted.

| op | request | reply |
|---|---|---|
| `health` | `{"op":"health"}` | `{"ok":true,"uptime":s,"idle_s":s,"version":"1"}` |
| `exec` | `{"op":"exec","argv":[…],"env"?:{},"cwd"?:"/","stdin_b64"?:"…","timeout_s"?:600,"tty"?:false,"user"?:"dev"}` | `{"stream":"stdout\|stderr","data_b64":"…"}`… then `{"exit":N,"timed_out"?:true,"signal"?:"…"}` |
| `exec_attach` | `{"op":"exec_attach","session":"x-…","seen":N,"exec"?:{exec request}}` | `{"attached":"x-…","started"?:true}`, then the session's lines from line `seen` on, as `exec` |
| `put_file` | `{"op":"put_file","path":"/abs","mode":"644","data_b64":"…","offset":N,"last":bool,"owner"?:"user[:group]"}`, one line per chunk | `{"ok":true,"size":N,"complete":bool}` for each chunk |
| `get_file` | `{"op":"get_file","path":"/abs"}` | `{"offset":N,"data_b64":"…"}`… then `{"eof":true,"size":N,"mode":"644"}` |
| `set_identity` | `{"op":"set_identity","hostname":"web","ipv4_cidr":"10.200.0.5/16","gw":"10.200.0.1","mac":"06:00:0a:c8:00:05","dns":["1.1.1.1"],"ipv6_cidr"?:"2001:db8::5/64","ipv6_gw"?:"2001:db8::1"}` | `{"ok":true,"ms":{"network":N,"secrets":N}}` |
| `set_env` | `{"op":"set_env","vars":{"K":"V"}}` | `{"ok":true,"count":N}` |
| `desktop` | `{"op":"desktop","action":"start\|stop\|status","token"?:"…"}` | `{"ok":true,"port":6080,…}` |
| `shutdown` | `{"op":"shutdown"}` | `{"ok":true}`, then the guest syncs and reboots (= firecracker exits) |
| `cmd_start` | `{"op":"cmd_start","argv":[…],"env"?:{},"cwd"?:"/","user"?:"dev"}` | `{"ok":true,"cmd_id":"…"}` |
| `cmd_list` | `{"op":"cmd_list"}` | `{"ok":true,"cmds":[status…]}` |
| `cmd_status` | `{"op":"cmd_status","cmd_id":"…"}` | `{"ok":true,"cmd_id","argv","state":"running\|exited\|lost","pid","exit"?,"signal"?,"started","ended"?}` |
| `cmd_logs` | `{"op":"cmd_logs","cmd_id":"…","offset"?:0,"limit"?:1048576}` | `{"ok":true,"data_b64","offset","next_offset","size","truncated","running","exit","signal"}` (`exit` and `signal` are `null` while the command runs or when not applicable; a guest from before they were added omits them, so their presence is the capability signal) |
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
  - If `timeout_s` passes, the agent sends SIGTERM, then SIGKILL after 2 s, and `timed_out` is set. A process still alive 2 s after the SIGKILL (uninterruptible sleep) is not waited for: the reply is `{"exit":137,"signal":"SIGKILL","timed_out":true,"error":"pid N did not exit after SIGKILL …"}`.
  - With `tty`, there is a single `stdout` stream, and stdin gets ^D after `stdin_b64`.
  - With `session` (1-64 of `A-Za-z0-9_-`, single use: a reused id is `session exists`), the agent keeps the reply lines (the newest 16 MiB) and keeps the command running if the connection drops. The session stays attachable for 300 s after its final line.
- **exec_attach** takes a session over. It replies `{"attached":id}`, replays the lines from index `seen` (0-based, counting every line the session sent), then streams the rest to this connection. A later attach takes the stream over again. `seen` below the kept lines is `exec session output lost`. With `seen:0` and `exec`, a session the agent never got (the request was lost in the reset) is started there instead, with `started:true`. An unknown session is `no such exec session`.
- **put_file**
  - Chunks go to `<path>.boat-part`. `offset` must equal the bytes received so far, and `offset:0` truncates.
  - The chunk with `last:true` sets the mode (an octal string or an int), sets the owner (default: like the parent dir), and renames the file into place.
  - boatd sends 1 MiB chunks. It sends `last:true` only once the HTTP body ended cleanly at its `Content-Length`: a body that breaks off or comes up short (the client went away) is 400 `short body`, and boatd removes `<path>.boat-part` with `file_delete`, so a truncated upload never replaces the file.
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
- Order of a fork or named snapshot: `sync`, pause, Firecracker writes the diff with `sync_snapshot_files:false`, disk reflink, resume, merge, copies. The guest is paused for the dump and the reflink only. Writeback of the dump runs behind the call, so the next merge is likely to find its base clean: a reflink of a dirty file waits for that file's writeback. boatd starts that writeback while Firecracker is still writing (`sync_file_range(SYNC_FILE_RANGE_WRITE)` on the dump every 250 ms, without waiting): a create right after a 3.8 GiB dump spent 6.2 s in `boot=` (quotas on 7.39 s, off 1.05 s, boat-e2e 43328227157), most likely binding its API socket, an inode create that a btrfs commit behind the dirty pages stalls (not measured apart then; the phase log now splits `boot vmm+sock=` from `load=`). Not caused by the uncompressed dumps: the e2e store is not mounted `compress`, and the dump wrote 3.80-3.85 GiB in every run before and after that change. A stop keeps Firecracker's fsync, since its file is the next start's base.
- Each memory dump owns a generation for its lifted memory limits and writeback cap. Background cleanup takes the VM lifecycle lock and restores limits only if that generation is still current; an older dump cannot lower the limits or remove the cap of a newer dump. Startup reconciliation restores limits only when no dump owns them.
- **A dump is capped while another VM boots.** From a dump's start (`liftMemLimits`) until its file is on disk (`setMemLimits`, or a stop's flush), the VM is a dumping one; while any VM is between its VMM start and its guest's init (`bootNew`, start/resume), every dumping VM's cgroup gets `io.max` `wbps` 64 MiB/s (`dumpWbpsWhileBooting = 64 << 20`; root-cgroup fallback extent copies also pace to 64 MiB/s and periodic `sync_file_range` kicks pause while a boot is active) on stateDir's disk (lower configured `ioMaxMBps` values stay), lifted when the last boot ends. Without a cap, a cold create 1 s into another VM's 2 GiB dump waited up to 12.4 s for its guest's boot reads behind the dump's writeback on the CI runner: boat-e2e on main 3.01-12.94 s (4 of 5 runs slow), with the initial r12 cap 3.66-5.92 s (5 runs; the e2e-perf gate went from 15 to 9 s, and `dumpWbpsWhileBooting` is now 64 MiB/s alongside loop direct-I/O). A dump nobody boots next to runs at full speed. cgroup `io.latency` was tried first: the runner's kernel lacks it, and on the AX42's NVMe an 8 GiB writeback moved another cgroup's 4k reads from 56 to 62 µs, so it never tripped (`bench/boatbench/results/ax42-r12-2026-11-13/`).
- A merge renames the new memory file over the old one with a descriptor to the old one still open and closes it in the background: the unlink of an unshared 8 GiB file frees its extents in the caller, 347-630 ms of a fork's freeze on the CI runner when the fork before it was gone (26-33 ms after).
- The dump's page cache is charged to the VM's cgroup, usually close to `memory.high`, which throttled it to disk speed. `memory.high` and `memory.max` move up by the VM's RAM for the dump (never to `max`: a resumed guest can write to its disk meanwhile, and a tenant can keep the window open by snapshotting in a loop) and come back once the file is on disk. A restarted boatd re-applies them to the VMs it adopts.
- **Jail dirs**: a VM's root belongs to its jail uid, so a compromised VMM can plant a symlink, FIFO or device there between two of boatd's calls. boatd opens files in it only with `O_NOFOLLOW`, regular files only, and acts on the descriptor: cloning (`FICLONE`, not `cp`), truncating, sealing, hard-linking (`linkat` on `/proc/self/fd`), fsync. Its sockets are dialled through an `O_PATH` descriptor. A memory file is built in the root-owned VM dir (`mem.stage`) and renamed in.
- A saved snapshot is usable when the API returns, and durable shortly after: `snap.json` carries `unsynced:true` until its memory is fsynced. After a host crash an unsynced snapshot keeps its disk and loses its memory (it restores cold), and a `snap.json` torn by the crash removes the snapshot.
- If a snapshot fails after Firecracker may have cleared its dirty bitmap, `diff_ok` goes false and the next snapshot is `Full`.
- boatd asks the guest to `sync` before each pause. Phase timings are logged: `snapshot (diff) sync= pause= write= written=`, `snapshot install merge=`, `freeze disk= rest=`.
- `vmstate` is consumed by a restore; `mem` stays until the next snapshot or cold boot.

### Admission and storage limits

- **Team quotas** (`teams.<team>` or `defaultTeam`, 429): `maxVMs`, `maxVCPU`, `maxMemMiB` count active VMs. `maxTotalVMs` counts VMs in any state. `maxSnapshots` counts snapshots. `maxDiskGiB` limits committed storage: each VM's disk + RAM size, and each snapshot's disk (+ RAM size if it has memory).
- **Host admission** (503): active VM RAM plus `vmOverheadMiB` per VM (what its cgroup allows it), across all teams, may not exceed (`hostMemMiB`, default MemTotal, minus `hostReserveMiB`, default 1024) × `memOvercommit` (default 1.5). Above 1.0, a new VM also needs real memory: `MemAvailable` ≥ `hostReserveMiB` + (its RAM + overhead) / `memOvercommit`. Active vCPUs may not exceed `hostCPUs` × `cpuOvercommit` (default 4.0). Enforced on create, fork and start.
- **Free-space floor** (507): create is refused when stateDir's free space is below the larger of `minFreeGiB` (5) and `minFreePct` (10%); fork and snapshot also require room for the VM's RAM. A stop below that level skips the snapshot and shuts the guest down cleanly (`has_snapshot:false`, the next start is a cold boot).
- **Host storage** (507): nominal committed storage across all teams (`hostStorageCheck`) may not exceed `stateDir`'s total filesystem capacity × `storageOvercommit` (default 10, because disks are sparse; 1 with `untrustedTenants`, where nominal committed bytes cannot exceed the total filesystem size; 0 = no limit), while `checkFloor` separately enforces the live `minFreeGiB` / `minFreePct` reserve on actual unallocated free space (`statfs.Bavail`). Quota for a snapshot or import being written is reserved until it lands, so concurrent operations of a team on different VMs cannot all pass one check, and an import is charged each member's declared size before a byte of it is written. Only growth is refused.
- **Defaults are finite**: `defaultTeam` starts at `maxTotalVMs` 100, `maxSnapshots` 200, `maxDiskGiB` 4096, `maxPorts` 512. Keys under `teams.<team>` replace these: 0 there means unlimited.
- **btrfs simple quotas** (`btrfsQgroups`, default on; without btrfs and Linux 6.7 boatd logs it and goes on without them, except with `untrustedTenants`): a team's VMs and snapshots live in the subvolume `teams/t_<team>`, with `vms/<id>` and `snapshots/<id>` symlinks into it, and its exclusive bytes are limited to 125% of `maxDiskGiB`. Past that, the team's own guests get disk errors, and nobody else notices. Data shared with an image by reflink does not count. Simple quotas, because full qgroups walk every extent's references at each commit (fork 1.31 s without, 3.25 s with, run 41305928387). Enabling them sets a filesystem feature older kernels cannot mount; a filesystem with full qgroups keeps them until `btrfs quota disable`. One subvolume per team, not per VM, because a memory file can only be hard-linked inside one subvolume, and its reflink waits for writeback. `boatd/e2e-perf.sh` measures the cost (`noQuotas` in its METRIC). VM dirs from before this stay where they are, without a limit, until deleted.
- **Per-VM limits**: `diskMBps` and `diskIOPS` (default 0, none: the old 50000 IOPS cap sat just above what the CI runner measured, and below any NVMe) are Firecracker drive limiters, `ingressMbit` its rx limiter (these apply from the next cold boot; a restored VM keeps those in its snapshot). `ioMaxMBps`/`ioMaxIOPS` set cgroup `io.max` on stateDir's disk (default off). `ctPerVM` (default 4096) refuses new forwarded connections, and new ones to the gateway, past that count per VM. The count lives in table `inet boat_ct`, which reloads leave alone. The module sets `nf_conntrack_max` to 1048576, room for 256 VMs at the cap; the kernel's default on 16 GiB is about 64.
- **Logs**: guest serial and VMM output go to `firecracker.log` (O_APPEND, so no reader is needed and a boatd restart does not disturb it). boatd checks it every second and past 16 MiB moves the last 1 MiB to `firecracker.log.1` and truncates. Bytes written while boatd is down are bounded only by the disk until it returns. Both files count in `/v1/usage`, and `vmCommit` reserves room for them.
- **Cgroup limits**: if the controllers cannot be delegated to `cgroupParent`, boatd refuses to start, since a VM without them can take the host's memory. `cgroupsOptional` allows it.
- `GET /v1/health` reports `conntrack_flush_errors`: flushes that failed, so stale flows may survive.
- `GET /v1/usage` adds `storage[]` per team (`vms`, `snapshots`, `committed_bytes`, `vm_apparent_bytes`, `vm_allocated_bytes`, `snapshot_apparent_bytes`, `snapshot_allocated_bytes`) and, when not filtered by team, `disk` (`free_bytes`, `total_bytes`, `floor_bytes`). Allocated bytes count reflinked extents once per file, so they are an upper bound.

### Performance and density

**Disk**

- `hugePages` (default off; NixOS `services.boat.hugePages`): guest memory on 2 MiB huge pages (Firecracker `huge_pages: "2M"`), from the kernel's surplus pool (`vm.nr_overcommit_hugepages`). Firecracker supports neither the balloon nor dirty-page tracking with them, and restores huge-page memory only through UFFD, not from boatd's memory file: boatd refuses the option unless `balloon`, `diffSnapshots` and `warmPool` are off, refuses snapshots and forks (409), and a stop shuts the guest down instead of saving its memory, so the next start cold-boots. Huge-page memory is outside the VM's cgroup memory limit, so `untrustedTenants` refuses it. The server profile also sets `powerManagement.cpuFreqGovernor = "performance"` (with amd-pstate/intel_pstate active that also sets EPP to performance); `"powersave"` trades latency for watts. Both are levers for boatbench to A/B on real hardware.
- `diskEngine` (default `auto`): Firecracker's `Async` engine (io_uring), and `Sync` when Firecracker refuses it (host kernel < 5.10.51, io_uring disabled). `Async` is a Firecracker developer preview. It is saved in snapshots, so a restore on a host without io_uring fails and a start falls back to a cold boot.
- `diskNoCow` (default on): at startup boatd rewrites each image's rootfs as a btrfs NOCOW file, once. `cp --reflink` carries the flag, so VM disks and their forks are NOCOW too. On other filesystems it does nothing. CoW turned each random guest write into an extent, and a reflink is O(extents), with the source VM paused: 4.5 s after boatbench's fio. The rewrite is dense (holes written as zeros): a hole at every zero block, from zstd's or `cp --sparse=always`'s output (boatd used the latter until 2026-11-12), made 22,567 extents in a 3 GiB image and a 151–170 ms reflink on every create and disk snapshot (AX42); dense, 4 extents and 0.1 ms, for 0.2 GiB of zeros. An image left fragmented by an older boatd (more than one extent per MiB) is rewritten once more.
- Memory files are not compressed. Every `vms/<id>` and `snapshots/<id>` dir gets btrfs `FS_NOCOMP_FL` (`chattr +m`, the `compression=none` property) when it is created, and a VM's jail root before each memory dump (VMs from older boatd builds), so files created there are written uncompressed even on a store mounted `compress=zstd`. Measured on the AX42 install (8 GiB VM): the dumped `mem` had 109 extents instead of 16,814 (128 KiB compressed extents), the dump took 2.96 s instead of 3.19 s. Not NOCOW: memory files keep btrfs checksums and can reflink to and from any file, whereas a NOCOW file can only reflink another NOCOW file. btrfs refuses NOCOW on a file that has the no-compression flag, so a NOCOW disk created in such a dir has the flag removed first. On other filesystems it does nothing.
  - The trade-off: NOCOW data has **no btrfs checksums** and no compression, so bitrot on a VM disk is not detected by btrfs. Run a guest filesystem or the host's RAID scrub if that matters. Reflink sharing still works: the first write to a shared extent copies it once, then writes go in place. A guest writing randomly into never-written parts of its sparse disk still allocates one small extent per write: 337k extents and a 2.2 s reflink in the worst case measured (run 41584046201).
- `hostDirtyRatio` (default 50, 0 leaves it alone): boatd sets the host's `vm.dirty_ratio` at startup, so that a memory dump goes to the page cache instead of being throttled to the disk's speed. A host crash can lose more, and VM disks in writeback were never durable before a guest flush anyway (Firecracker's default `Unsafe` cache mode).
- `diskCache` (default `Unsafe`): Firecracker's `cache_type`. `Unsafe` does not offer the guest a write cache flush (`write_cache` reads "write through"), so a guest `fsync` returns once the data is in the host's page cache. `Writeback` offers the flush and turns each guest flush into an `fsync` of the VM's disk file on the host. See "Disk: measured, defaults and limits" below for why `Unsafe` stays the default.
- `ioCost` (default off) and `ioCostModel`: blk-iocost on stateDir's disk, so that busy VMs share it by `io.weight` (equal) and an idle disk is free to take. **Never without `ioCostModel`**: the kernel's default model for an SSD costs about 8.5k rand IOPS and 430-490 MB/s, and caps the whole disk there, the host included (see below). `ioCostModel` is the disk's real capacity, `rbps= rseqiops= rrandiops= wbps= wseqiops= wrandiops=` (from `iocost_coef_gen.py` in the kernel tree, or fio on the disk); Unset, boatd reads `stateDir/iocost-model` (`boatd -measure-disk`, see "Untrusted tenants"), and with neither it leaves iocost off. boatd writes the model to `io.cost.model` with `ctrl=user` before it enables iocost, and `ioCostQoS` (p95 latency targets and the virtual rate's range) to `io.cost.qos`. If a write fails boatd logs it and runs without iocost.
- Guest: scheduler `none` on `vda` (udev rule). No `discard` mount option: Firecracker's block device does not offer discard (`discard_max_bytes` = 0 in the guest), so it would do nothing. Space freed inside a guest is not returned to the host while the VM runs.

**Async or Sync?** On the CI runner, in the same run, reads the host has cached favour `Sync` (46-145k against 27-98k rand read IOPS; each read costs one more wakeup through io_uring). Reads that reach the disk favour `Async` (5.0-7.4k against 4.3-5.8k IOPS, host cache dropped), and so do sequential writes (770-973 against 640-712 MB/s, runs 41646323823, 41652556991, 41660971823). Rand writes differ by less than the runs vary. The host's own rand read moved from 75k to 143k IOPS between those runs, so one run's VM/host ratio says little. This runner cannot say which wins on NVMe; `diskEngine: "Sync"` is the switch. `Async` stays the default because a queue depth is the only way past one I/O at a time on a real disk. `untrustedTenants` used to take `Sync` and disable io_uring in the kernel; it now opens io_uring to the VMMs' gid only ("Untrusted tenants" below).

**Disk: measured, defaults and limits.** `boatd/disk-matrix.sh` runs the host and a 4 vCPU / 8 GiB VM under several boatd configs on one runner in one run (boatbench's fio set: direct, libaio, iodepth 32, 15 s; plus a 4k random write with an `fsync` each), and a noisy-neighbour pair: VM b's jobs alone, then while VM a writes 1 MiB direct at iodepth 32. Run 42769967285 (GitHub runner, EPYC, btrfs on a loop file on the runner's disk) is the cleanest; runs 42677179392, 42749629578 and 42759114633 agree within the ranges below. MB/s or IOPS:

| | host (3 passes) | VM default (`Unsafe`, `Async`) | VM `Writeback` | VM `Sync` | untrusted before: `Sync`, io.max 500 MB/s / 20k IOPS | untrusted now: `Sync`, iocost (model + QoS), io.max 4000 / 200k |
|---|---|---|---|---|---|---|
| seq write | 411-752 | 919-1006 | 575 | 871 | 564 | 858 |
| seq read | 5283-5889 | 2539-3497 | 3450 | 3670 | 3192 | 2142 |
| rand write | 18.2-18.5k | 19.0-20.3k | 20.5k | 20.2k | 20.7k | 19.6k |
| rand read | 166-171k | 49.6-56.7k | 56.2k | 60.9k | 79.8k | 82.9k |
| 4k write + fsync | (594-1013 in earlier runs) | 2278-2520 | 369 | 2547 | 2386 | 2126 |

- **The last column is not what an untrusted host gets.** Those runs had SMT on (4 CPUs) and no `untrustedTenants`. With SMT off, as `untrustedTenants` requires (boatbench's `untrusted` preset: 2 CPUs online, a 4 vCPU VM), the VM's sequential write fell to 204-349 MB/s (runs 42800408080 and 42818758694, 10 VMs) while its random writes stayed near the host's: the bound was CPU for Firecracker's one VMM thread, which was one of five equal threads on 2 CPUs, not the I/O caps. With the vCPU threads in their own cgroup (`vcpuCpuWeight`, below) the same preset measured 482-767 MB/s (run 42819701658), with the VMM thread at 34-68% of a CPU and the VM's cgroup waiting for CPU (PSI) 14-47% of the time.
- The VM out-writes the host because its writes stop in the host's page cache (see below), and reads the host has cached run at memory speed on both sides; rand read in the VM moved between 8.5k and 74k IOPS across runs with the host's cache, more than any setting moves it. Compare configs within one run only.
- **`diskCache`: `Unsafe` stays the default.** `Writeback` makes guest `fsync` durable at 5-10x fewer fsync'd writes (297-380 against 1680-3625 IOPS over four runs) and 20-43% less sequential write, and next to a noisy neighbour its flushes stalled to 0 IOPS in every run. Choose it for VMs whose data must survive a host crash; a stop, snapshot or fork does not need it (boatd syncs the guest first).
- **`diskEngine`: `auto` (`Async`) stays.** With `Unsafe`, neither engine wins across runs (seq write 687-1006 against 700-871 MB/s, rand read 36-50k against 8.5-74k); with `Writeback`, `Sync`'s one write at a time halved sequential writes (359 against 658 MB/s, run 42749629578). `untrustedTenants` now gets `Async` too, with io_uring limited to the VMMs' gid.
- **`ioCost`: off by default**, because it needs the disk's model. With the kernel's default model it capped the runner's disk for everyone: the host's own rand read fell from 82k to 7k IOPS and seq read from 9.0 to 1.5 GB/s (run 42749629578). With a measured model it costs nothing measurable: host rand read 138k against 146k (run 42759114633), and the VM numbers in the last column.
- **Untrusted tenants: why `io.max` 500 MB/s / 20k IOPS went.** On this runner the cap already took 41% of a VM's sequential write (564 against 919-1006 MB/s). On an NVMe disk (3-7 GB/s, 500k-1M IOPS) the same cap is 6-14x below the device on bandwidth and 25-50x on IOPS, for every VM, idle disk or not. Its replacement keeps a single VM at the default's speed (last column) and shares a busy disk instead. Noisy neighbour, victim alone → while flooded (run 42769967285, `Unsafe`): fsync'd 4k writes 2122-2391 → 235-414 IOPS unprotected (11-17% kept) and 2226-2235 → 527-573 with iocost + QoS (24-26%); sequential write 46-61% kept unprotected, 40-53% with iocost. So iocost roughly doubles what a flushing victim keeps, and it does not protect a `Writeback` victim's flushes at all (0 IOPS with and without, runs 42759114633 and 42769967285): every byte of both VMs passes through the host's page cache and its one writeback queue, which the cgroup I/O controllers see only in part. A fast, honest-flush disk shared with hostile neighbours is beyond what this architecture can promise; give such tenants their own disk.

**What Firecracker's block device cannot reach.** These are properties of Firecracker 1.17's virtio-blk, not boatd settings; no `diskEngine`, `diskCache` or cgroup knob changes them (run 42677179392 recorded the guest queue and Firecracker's per-thread CPU for every fio job):

- **One request = one segment.** The device advertises no `SEG_MAX`, so the guest sees `max_segments` = 1: every request is one physically contiguous run of guest memory. A 1 MiB direct write reaches `vda` as 20-38 KiB requests (about 40 per MiB); reads merge better (64-95 KiB). Each request is a VM exit and a trip through the VMM.
- **One device thread.** All block (and net) emulation runs on Firecracker's single VMM thread: it sat at 87-92% of a core during sequential writes, which is the ceiling there, not the disk. In the VM's cgroup that thread was one of vCPUs+1 equal threads, so on a host with fewer CPUs than busy threads it got a fraction of a CPU. boatd moves the vCPU threads (`fc_vcpu N`) into a threaded child cgroup, `<vm>/vcpu`, with `cpu.weight` `vcpuCpuWeight` (default 100; 0 keeps one cgroup); the VMM, API and other threads stay in the VM's cgroup, each weighing about as much as all vCPUs together. `cpu.max`, memory, io and pids still apply to the whole VM. It is redone at each boot and restore. `cpuTraceS` (default 0, off) logs each running VM's per-thread CPU share and its cgroup's PSI (cpu, memory, io) every N seconds, to see which one a slow VM waits for. `Async` adds queue depth on the host side (io_uring) but completions still come back through that thread; `Sync` does one blocking read or write at a time on it.
- **One queue.** No multi-queue virtio-blk, so guest vCPUs do not get their own queues; a VM's I/O does not scale with its vCPUs.
- **Always the host page cache.** Firecracker opens the disk file without `O_DIRECT`. Every VM byte goes through the host's page cache (charged to the VM's cgroup: about 940 MiB `memory.current` per idle VM), reads are cached twice (guest and host), and a host-side measurement of "the disk" is mostly memory: the VM's seq write (690-990 MB/s) beats the host's own direct fio (370-550 MB/s) because the VM's never waits for the device.
- **Flush is all or nothing.** `Unsafe` advertises no write cache flush, so guest `fsync` is free and not durable. `Writeback` maps every guest flush to an `fsync` of the whole disk file: 297-305 fsync'd 4k writes/s against the host's own 594-1013 and `Unsafe`'s 1680-3625 (runs 42677179392, 42749629578, 42759114633).
- **No discard, no write-zeroes**: space freed in the guest stays allocated on the host until the VM is deleted.
- **Rate limiting is per drive, in the VMM** (`diskMBps`/`diskIOPS`, a token bucket): it has no notion of other VMs, so it caps a VM whether or not the disk is busy. Sharing a busy disk fairly is the host's job (`ioCost`).
- **vhost-user-blk**, which would move the data path out of the VMM onto a multi-queue backend, cannot be snapshotted in Firecracker, and boatd's stop, fork and snapshots are memory snapshots.
- **Snapshots write every dirty page.** `dump_dirty` (diff snapshots) writes each page dirtied since the last dump, also pages the balloon has reported free since (zeros on the host, still dirty in KVM's log). That is not what makes a first stop slow: in boat-e2e's first stop of an 8 GiB VM holding 2 GiB of random data in `/dev/shm` (15.47 s, run 42820379276) the dump was 2518 MiB, of which 64 MiB were all-zero 2 MiB blocks; the time is the `fsync` of real guest memory at the runner's disk speed (write 2.2 s, `fsync` 12.7 s, about 200 MB/s). Punching zero blocks out of a first dump before its `fsync` was tried in that run and cost (a 384 ms scan) about what it saved, so it was dropped. Leaving the `fsync` to the background instead would not remove the write either: the next diff snapshot reflinks this file and waits for its writeback (the stop after the next start, gated at 6 s, would pay the 12 s).

**Restore** is lazy already: Firecracker's `File` memory backend mmaps `mem` MAP_PRIVATE and faults pages on demand, which is why a UFFD handler would add a process and no speed here. Every restore gets a new VMGenID from Firecracker, and a create, fork or new-from-snapshot gets fresh host entropy before its new identity (see `reseed`).

**`warmPool`** (boatd's default off; the NixOS module turns it on unless `untrustedTenants`, which refuses it): a create restores a template instead of booting. boatd boots one VM of each image + size in use (default image + size at startup, others after their first cold create), under team `_warm`, waits for it to settle, drops its page cache and saves a memory snapshot in `stateDir/warm/`. Templates are not VMs in RAM, so an idle pool costs disk only. A template is rebuilt when the image, kernel or Firecracker binary changes, and one that fails to restore is deleted.

- Template VMs share boot-time randomness: kernel and process ASLR, `boot_id`, early userspace state. Keys and machine identity are regenerated from fresh entropy, but ASLR offsets of already-running processes are the same in every VM from one template, across tenants. Keep it off if your threat model counts on them differing.
- Memory files stay per-VM clones here, so tenants share no page cache.

**Density**

- `balloon` (default on): each cold-booted VM gets a virtio-balloon with free page reporting and `deflate_on_oom`, at size 0. The guest reports free memory, Firecracker `MADV_DONTNEED`s it. The kernel reports in about 2 s batches, so it is for idle VMs, not for squeezing a snapshot. Firecracker's free page *hinting* is not used: its documented race can corrupt guest memory.
- Host pressure: when `MemAvailable` falls below `hostReserveMiB`, boatd inflates every running VM's balloon by 10% of its RAM per 5 s (up to 50%), which makes guests give up page cache, and deflates them at twice the reserve. VMs still run at `oom_score_adj` 500, so if that is not enough the host OOM killer takes a VM, not the host. A compromised guest driver can ignore the balloon; the real bound on a VM stays its cgroup.
- `memOvercommit` above 1.0 is a bet that guests stay idle. If they all use their RAM at once, the host OOM killer will kill some.
- `ksmTeam` (default off, refused by `untrustedTenants`): page merging for one team's VMs, or `"*"` for all. **KSM is a cross-VM side channel**: a guest can time a write to learn whether another guest holds the same page contents, so it must not span a trust boundary. Linux has no KSM domains, so the only scope that is safe between tenants is one team: only VMs of `ksmTeam` are mergeable, others are not. Use `"*"` only when every team on the host trusts the others. It needs host kernel ≥ 6.4 (`PR_SET_MEMORY_MERGE`, set by re-executing boatd before the jailer) and costs a kernel thread scanning 200 MB/s.

**Measured** on GitHub's runner (EPYC 7763, 4 shared vCPU, 16 GiB, nested KVM, btrfs on a loop file; the host itself does 346-449 MB/s and 13k/75-83k rand write/read IOPS with fio direct). `boatd/e2e-perf.sh` prints `METRIC {"test":"boat-perf",...}` and holds the gates, whose margins are documented there. Single-VM create, new-from-snapshot, fork and create-during-dump timings now run through the first successful guest exec; snapshot timing remains the snapshot API call. The measurements below and the gate calibration reported in the script predate this timing correction; thresholds are unchanged.

| 4 vCPU / 8 GiB | before (boatbench 41573512017) | boatbench 41605624552 | e2e-perf 41609413499 |
|---|---|---|---|
| seq write | 195 MB/s | 710 MB/s | 837 MB/s |
| seq read | 745 MB/s | 3311 MB/s | 5846 MB/s |
| rand write | 29.8k IOPS | 43.2k IOPS | 45.6k IOPS |
| rand read | 44.1k IOPS | 43.9k IOPS | 53.5k IOPS |
| fork | 6.93 s | 1.77 s | 1.50 s |
| snapshot | 17.1 s | 4.68 s | 8.12 s (3.8 GiB written) |
| new-from-snapshot | 7.39 s | 4.57 s (5.65 s in 41614646999) | 1.08 s |
| 10 creates, cold | 28.4 s | 25.1 s | 23.2 s |
| 10 creates, `warmPool` | | | 8.12 s |

boatbench's new-from-snapshot went from 5.65 s to 2.73 s (run 41652606509) when `routes.json` stopped being fsynced: that fsync sat behind the dump's writeback, up to 5.3 s (`net=` in the phase log, run 41628497611). Of the 2.73 s, boatd accounts for 0.76 s and the first exec for 0.08 s; the rest is outside boatd, in boat-api. boatbench's guest also dirties less than e2e-perf's: 2.8 GiB against 3.8 GiB, which is why its snapshot is quicker.

Simple quotas, on (the default) against off, same run (41660971823, 41652556991, 41646323823): seq write 973/1101, 939/969, 770/804 MB/s; rand write 47.0k/46.2k, 44.6k/50.4k, 43.8k/45.3k IOPS; snapshot 7.48/7.21, 8.02/7.92, 8.89/7.77 s; new-from-snapshot 1.22/0.82, 1.41/0.84, 1.77/1.31 s; fork 1.11/0.96, 1.14/1.12, 2.23/1.64 s. So up to about 10% on sequential writes, and about 0.4 s on a new-from-snapshot, which writes to a new subvolume's metadata.

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

### Untrusted tenants

The NixOS server profile sets `services.boat.untrustedTenants = true` as a default (`lib.mkDefault`); a host whose tenants are all trusted opts out with `services.boat.untrustedTenants = false`. Left at that default, the domain checks (`untrustedTenantsAcceptRisks`: a VM or desktop domain sharing the API's registrable domain, on-demand TLS) are warnings, so an existing host config still evaluates; set explicitly to `true`, they are errors. Per-team quotas (`services.boat.quotas`, the default for every team): trusted, 0 = no per-team cap, only host admission bounds a team; untrusted, 10 VMs, 32 vCPUs and 64 GiB of RAM per team.

`services.boat.untrustedTenants` (NixOS) merges `boatd/untrusted.json` into boatd's config: `untrustedTenants`, `standaloneSSH` false, `btrfsQgroups`, `storageOvercommit` 1, `memOvercommit` 1, `uplinkSharePct` 25, `uplinkFairQueue`, `ioCost` with `ioCostQoS`, `ioMaxMBps`/`ioMaxIOPS`, `ctPerVM`, `hostDirtyRatio` 20 (at 50 a tenant can dirty half of the host's RAM and stall everybody's writes), `allowSMTP` false, no `warmPool`, no `ksmTeam`. boatd checks every key of that file against one rule each (`untrustedRules` in config.go; `TestUntrustedEveryKeyChecked` applies a weaker value to each key and expects a refusal naming it): each limit can be raised, none weakened or removed. It also refuses to start without the jailer or cgroup limits (including the `+io` cgroup controller and a resolvable physical whole-disk `major:minor` for `stateDir`, unwrapped across partitions, `/dev/loopN` backing files, and single-disk `dm-*`/LVM slaves), if an explicit `ioCostModel` fails to apply, with `hugePages`, if the running kernel has SMT on or KSM running, or if io_uring is open beyond the VMMs (below). At VM launch, if writing per-VM cgroup `io.max` fails under `untrustedTenants`, boatd immediately kills the spawned Firecracker process rather than letting it run unthrottled. The option also sets the kernel command line (`nosmt=force`, mitigations, L1D flush) and no swap. A reboot is required: until then boatd refuses, by design.

io_uring: `kernel.io_uring_disabled = 1` and `kernel.io_uring_group = 899999`, the `jailGid` every jailed Firecracker then runs with (uids stay per VM; each jail is a 0700 chroot owned by its uid, so the shared gid opens no file of another VM). Only CAP_SYS_ADMIN and that gid may create an io_uring, so VMMs get the `Async` disk engine and tenants' guests, which only reach the host through their VMM, cannot reach io_uring otherwise. The jailer (Rust's `Command::uid/gid`) drops supplementary groups, which is why the group has to be the primary gid. boatd refuses to start in untrusted mode unless `io_uring_disabled` is 2, or 1 with `io_uring_group` = `jailGid` (or none); Linux < 6.6 has neither knob and is refused. Until 2026-11-12 the option took `Sync` with io_uring off for everyone: all disk I/O then ran on the one VMM thread (351 against 1,863 MB/s sequential write trusted, on the AX42). `boatd/e2e-perf.sh` measures both on the runner (`diskSync` against `diskAsyncIoUringGroup` in its METRIC), and checks that a process outside the group is refused io_uring and one in it is not.

Bandwidth: `uplinkSharePct` turns an unset `egressMbit`/`ingressMbit` into that share of the uplink's speed (the IPv4 default route's NIC, `/sys/class/net/<dev>/speed`; 1000 Mbit when the NIC reports none): 250 Mbit per VM on a 1 Gbit port, where the old fixed 1000 let one VM take the whole port. `uplinkFairQueue` replaces the uplink's root qdisc with `cake unlimited besteffort dual-srchost nat`: cake looks up the conntrack entry behind the masquerade, so the port is shared per VM (and the host), then per flow inside each, whatever number of flows a VM opens; when cake is missing, `fq_codel` (per flow). Explicit `egressMbit`/`ingressMbit` win over the share.

Disk: the option sets `ioCost` with `ioCostQoS` (p95 read 5 ms / write 10 ms, virtual rate 25-400%) and `io.max` 4000 MB/s / 200k IOPS per VM as a ceiling (until 2026-11-10: `io.max` 500 MB/s / 20k IOPS; "Disk: measured, defaults and limits" has both). iocost needs the state disk's model: `ioCostModel` (NixOS `services.boat.ioCostModel`), else `stateDir/iocost-model`, which `boatd -measure-disk -config <file>` writes (about 25 s of direct I/O on a 1 GiB file: 1 MiB sequential at queue depth 4, 4 KiB random at 32; sequential 4 KiB IOPS are set to the random ones). The NixOS module runs it once before boatd (`boat-iocost-model.service`, skipped while the file exists; delete it to measure a new disk). Without a model boatd still starts, in untrusted mode too: iocost stays off, the disk is shared under the `io.max` ceilings only, and the log says so with the command that measures this disk (``no ioCostModel, disk shared under io.max ceilings only; run `boatd -measure-disk -config <file>` ``; until 2026-11-12 untrusted mode refused to start, exit status 78, so a fresh host whose measurement failed did not boot its VMs).

Backups: restic cannot forget one tenant, so data a tenant deletes stays in the repository until the snapshots holding it are pruned. `backup.pruneOpts` defaults to 4 weeks for that reason. `export.key` is a sealed credential on NixOS (`boat-export-key`, migrated from the plain file), so a backup of `dataDir` holds no usable key.

### Reconcile and restart

- On startup, boatd reads every `vm.json`:
  - a VM in `running` whose firecracker PID is alive (checked through `/proc/<pid>/cmdline`, which contains the VM id) is re-adopted;
  - a VM in any other active state is killed and marked `stopped`. Half-written snapshots are deleted. `has_snapshot` is set only when both files exist. A VM whose disk is missing goes to `error`.
- VM cgroups live under `cgroupParent` (default `boat-vms`, at the cgroup root), outside boatd's unit, so a boatd crash under `KillMode=mixed` touches neither the VMs nor their memory, and the next boatd re-adopts them.
- A VM that was running but whose process is gone (a host crash, or a `cgroupParent` inside the unit) gets `resume_on_boot` and cold-boots from its disk (`resumeCrashed`, default on). A VM the user stopped stays stopped.
- A clean unit stop is different: the unit uses `KillMode=mixed`, so on SIGTERM boatd snapshots every running VM in parallel. It sets `resume_on_boot` on them and restarts them on its next start, including after a host reboot. `TimeoutStopSec` must cover writing the dirty RAM of all VMs to disk.

### Networking

- `boatbr0` has `accept_ra=0`, `accept_redirects=0`, `autoconf=0`, and tenant RAs and redirects are dropped at L2 and L3, so a VM cannot configure the host.
- Neighbour spoofing: the host's ARP and NDP entries for a VM are `permanent`, an NA may only name the VM's own assigned IPv6 address or its exact per-tap EUI-64 link-local address (`fe80::…` derived from the VM's MAC) as its target, and ARP's sender MAC must be the VM's. With nothing resolving, a learned MAC could age out and flood the VM's traffic to every port: each port has a static FDB entry, with `learning off` and `flood off`. ARP does not cross between VMs, except inside a team with `allowIntraTeam`.
- Bridge `boatbr0` has `10.200.0.1/16`. Each VM gets slot n (2..65534):
  - IP `10.200.n>>8.n&255`
  - MAC `06:00:0a:c8:hi:lo`
  - netns `boat<n>` holding `tap0` (owned by the VM's uid), bridged to `veth0`, whose peer `bt<n>` is the VM's port on `boatbr0`
- Optional `ipv6Prefix` (routed to the host): the bridge gets `prefix::1`, and VM n gets `prefix::n` (on-link prefix length). Inbound IPv6 is limited to established/related traffic and ports published with `public:true`.
- Optional `publicIPv4` pool: outbound SNAT uses the VM's assigned `public_ip`, while inbound DNAT on `public_ip` in `inet boat prerouting` is restricted strictly to published TCP ports (`HostPort > 0`: `public_ip:guest_port` → `ip:guest_port`, matching IPv6 inbound port policy). The address stays with the VM until the VM is deleted. The upstream network must route these addresses to the host.
- nftables: boatd regenerates the tables `inet boat` and `bridge boat` from the VM list and loads them atomically (`nft -f -`) on every change.
  - **Host input from VMs**: only DNS to the gateway, ICMP `echo-request`, restricted ICMPv6 (`{ echo-request, destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert }` after dropping `{ nd-router-advert, nd-redirect }`), established traffic, and `aiProxyPort` / `hostHttpsPort` when set. Everything else is dropped.
  - **Forward into boatbr0**: established traffic, DNAT (port forwards, public IPv4) and published IPv6 ports. Everything else is dropped.
  - **Egress from VMs**: dropped to RFC 1918, `100.64.0.0/10`, loopback, `169.254.0.0/16` (metadata), `192.0.0.0/24`, `198.18.0.0/15`, `0.0.0.0/8`, `224.0.0.0/3`, and IPv6 `::/128`, `::1/128`, `::ffff:0:0/96`, `64:ff9b::/96` (NAT64), `64:ff9b:1::/48`, `100::/64` (discard), `2001::/32` (Teredo), `2001:db8::/32` (documentation), `2002::/16` (6to4), `fc00::/7` (ULA, including `fd00:ec2::254`), `fe80::/10`, `fec0::/10`, `ff00::/8`: IPv4-carrying/tunnel prefixes would otherwise reach the blocked IPv4 ranges through a translator. TCP 25/465/587 is dropped unless `allowSMTP`. `egressMbit` adds a per-VM cap (default none), policed in the bridge table on the VM's port. nft's token bucket holds one second's worth, so a burst can briefly exceed it; the steady rate is the cap (measured in e2e).
  - **Jail uids**: output from uids `jailUidBase`..`jailUidBase+65534` in the host netns is dropped.
  - **NAT**: masquerade `10.200.0.0/16` out of non-bridge interfaces. A hairpin masquerade lets a VM reach a sibling's public port.
  - **Bridge anti-spoofing** per tap: the source MAC, IPv4, ARP sender IP and IPv6 (assigned address, exact per-tap EUI-64 link-local `fe80::…` derived from `v.MAC` rather than `fe80::/10`, `::`) must match. Because of this, a fork is silent until `set_identity` gives it its own MAC and IP.
  - **Bridge isolation**: tap → tap traffic is dropped (ARP is allowed). The exception is taps in the same team when that team has `allowIntraTeam`.
- Rules load before a VM boots, including from a snapshot or a fork. A `bt*` port with no rules passes nothing.
- `aiProxyPort` (default 0, closed) opens one TCP port on the gateway to VMs, for boat-api's AI proxy; when non-zero it must lie outside `portMin`-`portMax`.
- `reservedPorts` (optional array of integers, each `1-65535` and strictly outside `portMin`-`portMax`) lists host service ports (such as `boat-api`, SSH jump host, AI proxy, and gateway ports) that must never collide with the published-port range or be allocated to VMs. When allocating a published `host_port`, boatd skips `reservedPorts` (and `hostHttpsPort`/`aiProxyPort`) and probes the candidate port with a local TCP listen+close so no locally bound host port is ever handed out. Both `inet boat prerouting` and `inet boat output_nat` exclude loopback destinations (`ip daddr != 127.0.0.0/8`) so host-loopback traffic is never DNAT'ed into a guest.
- `hostHttpsPort` (default 0, closed; the NixOS module sets 443 whenever `vmApiUrl` is set) opens one TCP port on host-local addresses to VMs. `hostHttpsAddrs` (canonical unicast IPv4 or IPv6 addresses or CIDR prefixes; rejecting hostnames, ports, whitespace, unspecified `0.0.0.0`/`::`, loopback `127.0.0.0/8`/`::1`, multicast and IPv4-mapped IPv6, while allowing `10.200.0.1` and explicit unicast addresses/prefixes) is honored **literally** (`10.200.0.1` is accepted only when explicitly listed; `boatd` never silently injects the bridge gateway IP): when non-empty, `input` emits `ip daddr { … } tcp dport <hostHttpsPort> accept` (when IPv4 entries exist) and `ip6 daddr { … } tcp dport <hostHttpsPort> accept` (when IPv6 entries exist), and does not emit `fib daddr type local`. When `hostHttpsAddrs` is empty, `input` drops BOTH all `privateV4` (RFC 1918, `100.64.0.0/10` CGNAT/Tailscale, loopback, `169.254.0.0/16` link-local, `192.0.0.0/24`, `198.18.0.0/15`, `0.0.0.0/8`, `224.0.0.0/3`) AND all `privateV6` (`::/128`, `::1/128`, `::ffff:0:0/96`, `64:ff9b::/96`, `64:ff9b:1::/48`, `100::/64`, `2001::/32`, `2001:db8::/32`, `2002::/16`, `fc00::/7` ULA/Tailscale, `fe80::/10` link-local, `fec0::/10`, `ff00::/8`) on `hostHttpsPort` before `fib daddr type local tcp dport <hostHttpsPort> accept`, blocking Tailscale, WireGuard/management RFC 1918 & ULA, link-local, documentation and loopback `:443` listeners while keeping public host IPs reachable (single-host/CI RFC 1918 setups pass explicit `hostHttpsAddrs`, e.g. `boatd/e2e-selfapi.sh` and `bench/boatbench/stack.sh`). `hostHttpsPort` must lie outside `portMin`-`portMax`.
- **Pre-SSH-policy quarantine & `standaloneSSH`**: by default (`standaloneSSH: false`), VMs enter `running` with `ssh_policy_ready: false` until `POST /v1/vms/{id}/ssh-policy` (or coalesced `POST /v1/vms/ssh-policy-batch`) installs and verifies guest SSH authorization offline over Firecracker vsock (`v.sock`). Quarantined VMs have zero network exceptions: 100% of IPv4 and IPv6 traffic is dropped in `inet boat` (`input`, `forward`, `output`) and `bridge boat` (`forward`, `output`), DNAT and intra-team sets omit them, and their IPv4 addresses are exported in `routes.json`'s `"quarantined"` array. Standalone `boatd` deployments without `boat-api` may set `standaloneSSH: true` (rejected when `untrustedTenants` is `true`) so VMs entering `running` start with `ssh_policy_ready: true`.
- nftables failures fail closed: boatd refuses to start, and a create, fork or start fails (the VM is killed) if its rules cannot be loaded; port changes report the error.
- Releasing or reusing an address flushes its conntrack entries (`conntrack -D`).
- `indexQuarantineS` (default 600, 0 = off): a deleted VM's slot (IP, MAC, tap, jail uid) is not handed out again for 10 minutes while another slot is free. Other readers of boatd's VM list may follow deletes asynchronously; quarantine reduces accidental address reuse for those readers. Managed SSH separately requires a current online authorization decision bound to VM identity and readiness; this allocation delay is not its authorization boundary. It never costs capacity: when every free slot is quarantined, the one freed longest ago is taken. The quarantine survives a boatd restart (`stateDir/freed.json`, not fsynced: a host crash restarts every reader too).
- Requirements: `br_netfilter` must not be loaded (`bridge-nf-call-iptables=1` would send bridged frames through the inet hooks). `ip_forward` is set by the module and by boatd.

### Isolation of the hypervisor

- **Jailer mode (default when `jailer` is set)**: each VM runs under the Firecracker jailer as its own uid/gid `jailUidBase + n` (default 900000+n), chrooted to its VM root, with its own netns (`--netns`, so firecracker sees only `tap0`) and pid namespace (`--new-pid-ns`). The jailer creates the device nodes. Its cgroup is a child of `cgroupParent`, or of boatd's own delegated cgroup; in that case boatd moves itself into a `boatd` leaf so it can enable the controllers. Limits: `memory.high` = RAM + `vmOverheadMiB` (256), `memory.max` = that + `vmPageCacheMiB` (512; the host page cache of the VM's disk writes is charged to its cgroup, and without this headroom a fast writer was OOM-killed), `cpu.max` = vCPUs + 1, `pids.max` = 256. If the controllers cannot be enabled, boatd logs it and VMs run without limits. Firecracker's built-in seccomp filters are on, as they are by default. The VM's files and tap are owned by that uid, so a compromised VMM cannot read other VMs' disks or memory.
- Firecracker runs at `oom_score_adj` 500 (set on launch and on adoption), while the unit keeps boatd at -900, so under memory pressure VMs die before boatd or the host.
- **No-jailer mode** (`jailer: ""`, used by the unit tests and for development): firecracker runs through `nsenter` in the VM's netns as `fcUid`/`fcGid` with cwd = VM root, plus its built-in seccomp and no cgroup limits. All VMs then share one uid, which is weaker isolation. Do not use it in production.
- **Snapshot imports**: the decompressor (`zstd -dc`) parses a tenant's archive. With `unpackSandbox` (NixOS: on) boatd runs it through `systemd-run --pipe --wait` as a transient unit: `User=nobody`, `PrivateNetwork`, `IPAddressDeny=any`, `SystemCallFilter=@system-service ~@privileged @resources @mount @network-io` (systemd 259 refuses `RestrictAddressFamilies` on a transient unit, so the socket calls are filtered instead), no capabilities, read-only system, private /tmp and devices, `MemoryDenyWriteExecute`, `MemoryMax=1G`. Without it, as nobody in a fresh network namespace. boat's output chain drops every packet from uid nobody either way.
- boatd itself runs as root (taps, nftables, jailer). It treats the guest agent as untrusted: reply lines are capped at 8 MiB, the JSON is decoded strictly, and guest output only reaches the caller of exec/files.
- Input validated at the API boundary: names, team, image (no path traversal: `^[a-z0-9][a-z0-9._-]{0,63}$`), labels, env names, absolute file paths, octal modes, ports 1-65535, and JSON bodies of at most 1 MiB (48 MiB for `POST /v1/vms/ssh-policy-batch`).

---

### boat-api routes that build on these

Documented here because they change what a route token or a boatd call means; the public API itself is in `docs/boat.md`.

- `POST /v1/vms/{id}/port-tokens/revoke` (boat-api, scope `exec`, no body) bumps the VM's route generation to revoke its existing private-port and desktop tokens, links and cookies, and replies `{"ok":true,"generation":N}`. New port tokens are HMACs over `route.<label>.g<N>.<exp>` (generation 0 keeps the old `route.<label>.<exp>`). boatd is not involved.
  With `-api` configured, boat-gateway fetches boat-api's `GET /internal/route-gens` (`{"gens":{"vm-…":N},"desktops":["vm-…"]}`, loopback only, refused with `X-Forwarded-For`) with a 1 s timeout, waiting 2 s between background fetches. Older generations are rejected once a newer snapshot arrives; this is asynchronous, and a newly minted token can get 401 until its generation is fetched. A validated snapshot expires 5 s after the successful fetch's request start. Failed or invalid responses, including HTTP 404, neither replace the last good snapshot nor renew its freshness.
  A missing or stale snapshot makes private-port and desktop requests retry a fetch once, then fail closed with 503 if no fresh snapshot is available. Desktop-host TLS `ask` checks likewise retry and deny approval (404) until a fresh snapshot confirms a minted desktop link. Public port routes do not depend on this snapshot. Without `-api`, gateway revocation tracking is disabled and no snapshot is required.
- **Managed SSH authorization v1.** Guest `POST /v1/vms/{id}/ssh/authorize` accepts at most 32 KiB JSON `{user,key_type,key_blob}` with a distinct lookup-only bearer credential (`ssh-auth-v1`, bound to team, VM, current token generation, unpredictable host boot epoch and `copy-v1`). Ordinary API/self bearer tokens cannot use this lookup; lookup credentials cannot use ordinary API routes. `200 {"authorized_key":"TYPE BLOB"}` permits exactly the offered key; `204` denies, `401`/`404` rejects identity, `503` indicates unready authority. Responses are `no-store`. This credential is independently refreshed, never read from tenant environment overrides.
  Host jump `POST /internal/ssh/authorize` is numeric loopback-only and rejects forwarded requests. It accepts the same key/user JSON and returns `200 {"authorized_keys":["restrict,port-forwarding,permitopen=\"10.200.0.2:22\" TYPE BLOB"]}` or `204`. Caddy blocks `/internal` and `/internal/*`; this route must never be publicly proxied. sshd calls `/etc/boat/jump-keys http://127.0.0.1:<apiPort>/internal/ssh/authorize %t %k %u` as `boat-ssh`. The helper validates every complete JSON line, exact offered key and canonical safe literal IPv4 port-22 targets before any stdout, with a 2 s total deadline, 500 ms connect timeout and 64 KiB response limit. No proxy, redirect, cache, static key file or routes-file fallback is permitted.
  Guest policy deployment uses boatd's private UDS `POST /v1/vms/{id}/ssh-policy` with `{version:1,boot_epoch,config,helper_b64}` (performing a single `trustedRootAgent` health check before `put_file`, `install` and `verify`) or `POST /v1/vms/ssh-policy-batch` with `{items:[{id,version:1,boot_epoch,config,helper_b64}]}` (running per-VM guest installation concurrently under sorted per-VM locks and coalescing publication into a single `nft -f -` reload, rolling back all batch items fail-closed on firewall error). For already-attested running VMs whose `boot_epoch`, helper and `users` are unchanged and only the lookup `token`/`generation` rotates, `boatd` invokes the helper's `rotate-token` subcommand (`user:"root"`, `trusted_root_exec:true`) in place without fencing `ssh_policy_ready` or reloading `nft -f -`: `rotate-token` verifies `boat-ssh`, `sshd_config`, the `ssh.service` drop-in, helper `0555` permissions and active `ssh.service` state (failing closed on any divergence) and atomically updates `/etc/boat/ssh-auth.json` (`0640` `root:boat-ssh`) without stopping `ssh.service`, terminating active `sshd` sessions, or running `sshd -T`. The canonical helper artifact is `guest/ssh-authorize.py`, supplied to boat-api by `-ssh-policy-helper-file`, installed root-owned at `/usr/local/libexec/boat-ssh-authorize`. Config is `{version:1,url,token,vm_id,team_id,generation,boot_epoch,users}`, root:boat-ssh mode 0640 at `/etc/boat/ssh-auth.json`. `url` is the full HTTPS guest authorization endpoint. `users` contains only explicitly configured and verified OS accounts. `install`, `verify` and `rotate-token` each receive config JSON on stdin and return `{version:1,config_digest,boot_epoch,readiness:true}` after verification; digest is SHA-256 of UTF-8 JSON with sorted object keys, compact separators, preserved array order and no trailing newline. Inventory (`GET /v1/vms`, including lightweight `GET /v1/vms?view=ssh` returning `{id,team,state,ip,pending_copy,agent_ready,ssh_boot_epoch,ssh_policy_ready,ssh_policy_digest}` for `boat-api` reconcile/SSH authority without full port/label structs) exposes `ssh_boot_epoch`, `ssh_policy_ready`, `ssh_policy_digest`; readiness is never persisted across host/VM incarnation adoption.
  New grants are durably committed before authorizing publication. API final decisions fence current grant, credential, VM identity and readiness against invalidation; guest IO never runs under that fence. Historical unowned grants deny managed access. Tenant authority is only explicitly enrolled `authorized_keys.tenant`; unknown legacy key files are protected archives requiring review/re-enrollment. Managed-key removal is cleanup, never the revocation boundary. The guarantee ends at the API's allow decision, not later sshd receipt or termination of existing authenticated sessions/transports.
  Migration requires maintenance ingress closure, access suspension, verified guest policy and old listener/socket/pre-authentication teardown before release. Failed policy/firewall/route updates remain quarantined; rollback never restores static authorization. See `docs/boat.md` for the operator migration procedure. All slices require integrated verification before any deployment safety claim.
- `stop {force}`, `fail_fast` and `count` above are what boat-api forwards for the CLI's force stop, fail-fast and batch create requests.

## 4. routes.json (for boat-gateway)

```json
{"vms":{"vm-…":{"ip":"10.200.0.2","ports":[3000,8080],"public":[8080],"desktopPort":6080,"team":"t1","state":"running"}},"quarantined":["10.200.0.3"]}
```

- `public` lists the guest ports published with `public:true`; the gateway requires a route token for every other port.
- `vms` lists only running VMs whose `ssh_policy_ready` state has been committed to nftables. `quarantined` lists sorted IPv4 addresses of running VMs that are not yet network-ready (so `boat-gateway` can immediately reject requests originating from quarantined guest IPs with 403). During `syncNet()` reloads, already-published ready VMs remain live in `vms` (never blanked out to `{}`), while newly-promoted VMs stay withheld until `nft -f -` succeeds.
- Mode 0640, group `routesGroup` (NixOS: `boat-routes`, whose members are `boat-gateway` and `boat`; the jump lookup command runs separately as `boat-ssh` without this group), set before the rename, so a reader never sees it unreadable. Without `routesGroup` only root reads it.
- The gateway routes `<port>-<id>.vm.<domain>` to `ip:port` for the listed ports, and routes the desktop to `ip:desktopPort`.

---

## 5. Measured deviations

- **Firecracker block**: no discard support, see "Performance and density".

- **Nested KVM**: measured in e2e (`METRIC` fields `nestedHost`, `nestedGuestCpus`, `nestedGuestKvm`). On a host with `nested=1`, Firecracker 1.17 passes VMX/SVM to the guest with no CPU template (run 41315786226: 4 of 4 vCPUs). `/dev/kvm` in the guest then depends only on the guest kernel's config, and on the host enabling nested virtualization. Firecracker does not save KVM nested state, so do not expect a stop snapshot or fork taken while a nested VM runs to restore that nested VM.
