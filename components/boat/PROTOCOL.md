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
| `POST /v1/vms/{id}/ports` | `{port` (or `guest_port`)`, public?}` | 201, Port |
| `DELETE /v1/vms/{id}/ports?port=N` | | `{"ok":true}` |
| `GET /v1/vms/{id}/snapshots` | | `{"snapshots":[Snapshot…]}` |
| `POST /v1/vms/{id}/snapshots` | `{name?}` | 201, Snapshot |
| `DELETE /v1/vms/{id}/snapshots/{sid}` | | `{"ok":true}` |
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
  - boatd pauses the VM, takes a full Firecracker snapshot (mem + vmstate) into the VM dir, then kills firecracker. Billing stops.
  - If the snapshot fails, boatd sends the agent `shutdown`, waits up to 30 s, then kills the VM. The next start is then a cold boot.
- **start**
  - With a snapshot: boatd restores it (load + resume), deletes the snapshot files (they are stale once the guest runs), and resets the guest clock with `exec date -s @now` as root.
  - Without a snapshot: cold boot.
  - If the restore fails: cold boot.
- **fork**
  - Running parent: boatd pauses it, snapshots it, reflink-copies disk + mem + vmstate into the new VM, then resumes the parent. The pause lasts as long as the memory dump.
  - Stopped parent: boatd copies the stop snapshot, or only the disk if there is none.
  - Then boatd restores the child on its own tap and sends `set_identity` with the child's IP, MAC and hostname. The agent also regenerates machine-id and ssh host keys.
  - `team`, if given, must equal the parent's team, else 404.
- **delete** kills the VM and removes its dir, its tap and its snapshots.
- **ports**: every port gets a gateway hostname route, `<guest_port>-<id>.vm.<domain>`; the `hostname` field holds the label before `.vm.<domain>`. With `public:true` the port also gets a raw TCP `host_port` from `portMin..portMax` (20000-40000), DNATed to `ip:guest_port`.
- **quotas** (`teams.<team>` or `defaultTeam`): `maxVMs`, `maxVCPU`, `maxMemMiB` count VMs in any state except stopped/error. They are enforced on create, fork and start.

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
| `set_identity` | `{"op":"set_identity","hostname":"web","ipv4_cidr":"10.200.0.5/16","gw":"10.200.0.1","mac":"06:00:0a:c8:00:05","dns":["1.1.1.1"],"ipv6_cidr"?:"2001:db8::5/64","ipv6_gw"?:"2001:db8::1"}` | `{"ok":true}` |
| `set_env` | `{"op":"set_env","vars":{"K":"V"}}` | `{"ok":true,"count":N}` |
| `desktop` | `{"op":"desktop","action":"start\|stop\|status","token"?:"…"}` | `{"ok":true,"port":6080,…}` |
| `shutdown` | `{"op":"shutdown"}` | `{"ok":true}`, then the guest syncs and reboots (= firecracker exits) |

Semantics:

- **exec**
  - Runs as `user` (default `dev`) in a new session.
  - If `timeout_s` passes, the agent sends SIGTERM, then SIGKILL after 2 s, and `timed_out` is set.
  - With `tty`, there is a single `stdout` stream, and stdin gets ^D after `stdin_b64`.
- **put_file**
  - Chunks go to `<path>.boat-part`. `offset` must equal the bytes received so far, and `offset:0` truncates.
  - The chunk with `last:true` sets the mode (an octal string or an int), sets the owner (default: like the parent dir), and renames the file into place.
  - boatd sends 1 MiB chunks.
- **set_identity**
  - The agent sets the MAC (link down, set address, up), flushes and adds IPv4 (and IPv6), sets the default route(s), `/etc/resolv.conf`, hostname and `/etc/hosts`.
  - It **always** regenerates `/etc/machine-id` and the ssh host keys, then restarts sshd (which also starts one that had failed).
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
stateDir/usage.jsonl
runDir/routes.json
```

- The disk is a `cp --reflink=always` copy of the image rootfs (instant on btrfs). A cold-booted disk is then grown with `truncate` to the size's `disk_gib`; the guest grows its filesystem on first boot.
- boatd gives Firecracker only **relative** paths (`rootfs.ext4`, `v.sock`, `vmstate`, `mem`), and Firecracker runs with that dir as its cwd or chroot. Because of this, a snapshot never names a host path, and a copy of the dir restores anywhere. That is how forks and snapshots work.
- The tap is swapped on restore with `network_overrides`, which needs Firecracker 1.12 or later.

### Reconcile and restart

- On startup, boatd reads every `vm.json`:
  - a VM in `running` whose firecracker PID is alive (checked through `/proc/<pid>/cmdline`, which contains the VM id) is re-adopted;
  - a VM in any other active state is killed and marked `stopped`. Half-written snapshots are deleted. `has_snapshot` is set only when both files exist. A VM whose disk is missing goes to `error`.
- The unit uses `KillMode=mixed`, so on SIGTERM boatd snapshots every running VM in parallel. It sets `resume_on_boot` on them and restarts them on its next start, including after a host reboot. `TimeoutStopSec` must cover writing all guest RAM to disk.

### Networking

- Bridge `boatbr0` has `10.200.0.1/16`. Each VM gets slot n (2..65534):
  - IP `10.200.n>>8.n&255`
  - MAC `06:00:0a:c8:hi:lo`
  - tap `bt<n>`, owned by the VM's uid
- Optional `ipv6Prefix` (routed to the host): the bridge gets `prefix::1`, and VM n gets `prefix::n` (on-link prefix length). Inbound traffic to that address is forwarded.
- Optional `publicIPv4` pool: 1:1 DNAT/SNAT. The address stays with the VM until the VM is deleted. The upstream network must route these addresses to the host.
- nftables: boatd regenerates the tables `inet boat` and `bridge boat` from the VM list and loads them atomically (`nft -f -`) on every change.
  - **Host input from VMs**: only DNS to the gateway, ICMP/ICMPv6, and established traffic. Everything else is dropped.
  - **Forward into boatbr0**: established traffic, DNAT (port forwards, public IPv4) and the public IPv6 prefix. Everything else is dropped. VM → `169.254.0.0/16` (metadata) is dropped.
  - **NAT**: masquerade `10.200.0.0/16` out of non-bridge interfaces. A hairpin masquerade lets a VM reach a sibling's public port.
  - **Bridge anti-spoofing** per tap: the source MAC, IPv4, ARP sender IP and IPv6 (assigned address, link-local, `::`) must match. Because of this, a fork is silent until `set_identity` gives it its own MAC and IP.
  - **Bridge isolation**: tap → tap traffic is dropped (ARP is allowed). The exception is taps in the same team when that team has `allowIntraTeam`.
- Requirements: `br_netfilter` must not be loaded (`bridge-nf-call-iptables=1` would send bridged frames through the inet hooks). `ip_forward` is set by the module and by boatd.

### Isolation of the hypervisor

- **Jailer mode (default when `jailer` is set)**: each VM runs under the Firecracker jailer as its own uid/gid `jailUidBase + n` (default 900000+n), chrooted to its VM root. The jailer creates the device nodes. Its cgroup is a child of boatd's own (delegated) cgroup, through `--parent-cgroup` read from `/proc/self/cgroup`. Firecracker's built-in seccomp filters are on, as they are by default. The VM's files and tap are owned by that uid, so a compromised VMM cannot read other VMs' disks or memory.
- **No-jailer mode** (`jailer: ""`, used by the unit tests and for development): firecracker runs as `fcUid`/`fcGid` with cwd = VM root, plus its built-in seccomp. All VMs then share one uid, which is weaker isolation. Do not use it in production.
- boatd itself runs as root (taps, nftables, jailer). It treats the guest agent as untrusted: reply lines are capped at 8 MiB, the JSON is decoded strictly, and guest output only reaches the caller of exec/files.
- Input validated at the API boundary: names, team, image (no path traversal: `^[a-z0-9][a-z0-9._-]{0,63}$`), labels, env names, absolute file paths, octal modes, ports 1-65535, and JSON bodies of at most 1 MiB.

---

## 4. routes.json (for boat-gateway)

```json
{"vms":{"vm-…":{"ip":"10.200.0.2","ports":[8080,3000],"desktopPort":6080,"team":"t1","state":"running"}}}
```

- Lists only running VMs. Rewritten atomically on every VM, port or state change.
- The gateway routes `<port>-<id>.vm.<domain>` to `ip:port` for the listed ports, and routes the desktop to `ip:desktopPort`.
