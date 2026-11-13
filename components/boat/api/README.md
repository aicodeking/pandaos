# boat-api

Public HTTP API for Boat (persistent Ubuntu VMs for AI agents). Go stdlib only.
It listens on `127.0.0.1:8740` and Caddy terminates TLS in front of it. It handles
auth, teams, secrets, webhooks, rate limits and audit, and forwards VM work to
`boatd` over `/run/boat/boatd.sock`.

```
boat-api admin init -data /var/lib/boat-api [-user alice] [-team acme] [-json]   # prints the operator key once (bare token on a terminal, JSON otherwise)
boat-api admin gen-master-key                                            # 64 hex chars for systemd-creds
boat-api serve -data /var/lib/boat-api -boatd /run/boat/boatd.sock \
  [-rate 10 -burst 40] [-team-rate 30 -team-burst 120] \
  [-max-keys-per-team 100] [-max-webhooks-per-team 20] \
  [-desktop-url https://HOST/desktop -desktop-key-file F] [-vm-domain vm.HOST] [-port-url-ttl 1h] \
  [-ssh-user dev] [-ssh-jump jump@HOST:2222 -ssh-jump-keys-file F] [-images ubuntu-24.04,ubuntu-22.04] \
  [-audit-log F -audit-max-bytes 104857600 -audit-keep 20 -audit-max-age 2160h] \
  [-ai-proxy-listen 10.200.0.1:8744] [-ai-proxy-url URL] [-ai-rate 20] [-public-url https://HOST] \
  [-otlp-endpoint URL -otlp-header k=v] [-webhook-team-inflight 8 -webhook-team-pending 1000]
```

Run `admin init` before `serve`. The server loads its state at startup.

## Tenant model

- **Operator:** the user created by `admin init` is the instance operator. State
  written before this role existed promotes its oldest user at load. Only the
  operator creates and deletes teams, and only with a key that has the `admin` scope:
  - `POST /v1/teams {name, owner?}`: with `owner`, a new user owns the team and their
    first key is returned once. Without it, the operator owns the team.
  - `DELETE /v1/teams/{id}`: refused while the team has VMs, or when it is the team of
    the key in use. It removes the team's keys, secrets, webhooks, ssh grants and
    orphaned users.
  Team owners and admins manage members, keys, secrets and webhooks inside their
  team, but cannot create teams.
- **Per-team limits:**
  - `-max-keys-per-team`, default 100, counts every key including member keys.
  - `-max-webhooks-per-team`, default 20.
  Past a limit the server answers 403 `limit_reached`. boatd enforces the VM, vCPU
  and memory quotas and answers 429.
- **Rate limits:** two token buckets, both must pass:
  - per key: `-rate`/`-burst`;
  - per team, shared by all its keys: `-team-rate`/`-team-burst`.
  The answer is 429 with `Retry-After`.

## Endpoints

The full schema is in `openapi.json` (OpenAPI 3.1), which the server also serves at
`GET /v1/openapi.json`. Each operation's `x-boat-scope` field names the scope it needs.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /healthz`, `GET /v1/openapi.json` | none | no auth |
| `GET /v1/me` | any | user, team, role, effective scopes |
| `GET /v1/images` | any | from `-images`; the first one is the default |
| `GET /v1/usage[?from&to]` | vms:read | boatd `/v1/usage?team=`: VM-seconds by size, snapshot-days, starts |
| `GET/POST /v1/vms` | vms:read / vms:write | create body `{name,size,image,ttl_s,idle_stop_s,snapshot,labels,env,secrets,environment,no_env,from}`; `secrets:[names]` picks the injected team secrets (absent = the ones an admin marked default-injected, `[]` = none); label keys starting with `boat.` are reserved (boat-api stores the selection in `boat.secrets`) |
| `GET/DELETE /v1/vms/{id}` | vms:read / vms:write | |
| `POST /v1/vms/{id}/start\|stop\|fork` | vms:write | start re-injects secrets `{env}`; fork `{name, env, labels}` (labels replace the parent's; its `boat.*` ones carry over) |
| `POST /v1/vms/{id}/exec` | exec | `{cmd[],env,cwd,stdin_b64,timeout_s}` → NDJSON `{stream,data_b64}` … `{exit}` |
| `POST /v1/vms/{id}/prompt` | exec | `{prompt,agent:claude\|codex\|opencode,model,cwd,timeout_s}`, streamed like exec; team secrets go in as env |
| `GET/PUT /v1/vms/{id}/files?path=` | exec | raw bytes; the path must be absolute; `-max-file-bytes` |
| `GET/POST /v1/vms/{id}/ports`, `DELETE …/ports/{port}` | vms:read / vms:write | `{port,auth:private\|public}`, default private; `public:true` is shorthand for `auth:public` |
| `GET /v1/vms/{id}/ports/{port}/url[?ttl_s=]` | exec | `{url,auth,token?,expires_at?}`; private routes get a signed token (see Port routes) |
| `POST /v1/vms/{id}/port-tokens/revoke` | exec | `{ok,generation}`; ends every token, link and cookie of the VM's private ports within about 2 s. Removing a port does this too |
| `GET/POST /v1/vms/{id}/snapshots`, `DELETE …/snapshots/{sid}` | vms:read / vms:write | proxied to boatd |
| `POST /v1/vms/{id}/ssh` | exec | `{public_key}` is appended (deduped) to `/home/<ssh-user>/.ssh/authorized_keys`; returns `{host,port,user,jump?}` |
| `GET /v1/vms/{id}/desktop` | exec | `{url,expires_at}`, signed and short-lived (`-desktop-ttl`, default 5m) |
| `GET/POST /v1/teams`, `GET/DELETE /v1/teams/{id}` | any / admin + operator | create and delete are operator-only (see Tenant model) |
| `POST /v1/teams/{id}/members`, `DELETE …/members/{user}` | admin + team owner/admin role | creating a member returns their first key; the last owner cannot be removed |
| `GET/POST /v1/keys`, `DELETE /v1/keys/{id}` | any | the token is shown only once; `expires_in_s` (≤ 365 days) makes the key expire (then 401). Deleting a key revokes the private port links it minted and its signed file URLs |
| `GET /v1/secrets`, `PUT/DELETE /v1/secrets/{NAME}` | vms:read / admin | values are never returned |
| `POST/GET /v1/vms/{id}/commands`, `GET/DELETE …/commands/{cid}` | exec | detached commands: `{cmd[],env,cwd}` → `{id,cmd,state}`; DELETE sends TERM |
| `GET …/commands/{cid}/logs?offset=&limit=&follow=1` | exec | one chunk, or with `follow=1` NDJSON until the command ends |
| `POST …/commands/{cid}/signal` | exec | `{signal: TERM\|INT\|KILL\|HUP}` |
| `GET /v1/vms/{id}/command-history`, `GET …/processes` | exec | history is boat-api's own record, so it answers for a stopped VM |
| `GET …/files/list`, `DELETE …/files`, `POST …/files/move`, `GET …/files/search`, `GET …/files/info`, `POST …/files/folder`, `POST …/files/permissions` | exec | `search`: names, or contents with `content=1`; `regex=1` |
| `POST/GET /v1/vms/{id}/prompts`, `GET …/prompts/{pid}`, `GET …/prompts/{pid}/logs` | exec | background turns; 202 `{id,conversation_id,cmd_id,state}`; 409 while its conversation runs a turn |
| `POST …/prompts/{pid}/steer`, `POST …/prompts/{pid}/interrupt`, `GET …/conversations` | exec | see Agents |
| `GET/POST /v1/environments`, `GET/PUT/DELETE …/{id}`, `POST …/{id}/build` | vms:read / admin | `{name,image,size,setup_script,env,secrets,is_default}`; by id or name |
| `POST /v1/vms/{id}/resize` | vms:write | `{size,disk_gib,discard_memory}`; size needs a stopped VM, and `discard_memory` if it has a stop snapshot |
| `GET /v1/vms/{id}/usage`, `GET /v1/events` | vms:read | one VM's seconds; SSE lifecycle events for the team (`after=`, `Last-Event-ID`) |
| `GET /v1/snapshots`, `DELETE /v1/snapshots/{sid}`, `POST /v1/snapshots/import?name=` | vms:read / vms:write | the team's snapshots, imports included; import takes the raw archive |
| `GET /v1/vms/{id}/snapshots/{sid}/download` | exec | streams the archive (tar.zst) |
| `POST/GET …/snapshots/{sid}/shares`, `DELETE …/shares/{share}` | admin | `{team_id}` grants a team; otherwise a signed link whose `code` is shown once |
| `GET /v1/shared-snapshots`, `POST /v1/snapshots/import-share` | vms:read / vms:write | grants to this team; `{share_id\|code,name}` copies one in |
| `GET /v1/snapshot-links/{code}/download` | none | the code is the credential; audited with the code redacted |
| `GET /v1/ai/credentials`, `PUT/DELETE …/{provider}` | admin | see AI credentials proxy |
| `GET/PUT /v1/ai/proxy`, `GET /v1/ai/usage`, `GET /v1/vms/{id}/ai-usage`, `PUT /v1/vms/{id}/ai-access` | vms:read / admin | switch, metering, per-VM block |
| `GET/PUT/DELETE /v1/telemetry` | admin | the team's OTLP endpoint for its VM lifecycle events |
| `POST /v1/keys/{id}/rotate`, `GET /v1/keys/{id}/usage` | any | `{overlap_s}`, default 86400; usage counts since this process started |
| `GET/POST /v1/webhooks`, `DELETE /v1/webhooks/{id}` | admin | `{url,events[]}`; the secret is shown only once |
| `GET/PATCH /v1/webhooks/{id}`, `POST …/rotate` | admin | rotate takes `{overlap_s}`, default 86400 |
| `GET /v1/webhooks/{id}/deliveries`, `POST …/test` | admin | the last 100 attempts |

Errors use the shape `{"error":{"code","message","request_id"}}`. Every response carries an
`X-Request-Id` header. A client-supplied `X-Request-Id` (≤64 chars) is kept.

## Auth design

### The VM's own key (`BOAT_ID`, `BOAT_URL`, `BOAT_API_KEY`)

Every VM gets `BOAT_ID` in its env at create, fork and start. With `-vm-api-url`
(default `-public-url`; the URL VMs use to reach this API) it also gets `BOAT_URL` and
`BOAT_API_KEY`, a token for that VM alone, so `boat self …` and `host …` work in the guest
without a team key. A team that stores its own `BOAT_API_KEY` secret keeps it.

- **Token:** `boatself_<gen>_<hex(HMAC-SHA256(vmKey, "self|<vmid>|<gen>"))>_<vmid>`, `vmKey`
  derived from the master key. Valid while the VM exists and its generation is `<gen>`.
- **Reach:** only routes under `/v1/vms/<that id>/` with `vms:read`, `vms:write`, `exec`
  (read, exec, files, ports, stop/start, fork, snapshots, delete). Every other route, and
  every other VM, answers 404. No admin scope.
- **Revocation:** the generation moves on at create, fork and start, and on the source VM
  whenever its disk is copied (fork, snapshot, share), because the copy holds the token.
  A running source VM gets the new token pushed to `/etc/boat/env`; a process that read
  the old one holds a dead token. Delete ends it.
- **Crash:** the generation is written without waiting for the disk (see Store), so a crash
  can make an older token of the same VM valid again, never one of another VM.

- **Token:** `boat_<16 hex id>_<64 hex secret>`, i.e. 256 random bits.
- **Hash:** `hex(HMAC-SHA256(pepper, salt ‖ 0x00 ‖ secret))` with a 16-byte salt per key.
  A slow KDF (PBKDF2/scrypt) only helps against low-entropy passwords. Brute-forcing
  256-bit secrets is already infeasible, and the HMAC keeps auth cheap on every request.
  The pepper is derived from the master key, and `state.json` is sealed under it too. Comparison is constant-time.
- **Master key:** 32 bytes (raw or hex), taken from the first source that exists:
  1. `-master-key-file`;
  2. the systemd credential `$CREDENTIALS_DIRECTORY/boat-master-key`. This is the
     recommended source: `LoadCredentialEncrypted=boat-master-key:/etc/credstore.encrypted/boat-master-key`,
     with the file created by
     `boat-api admin gen-master-key | systemd-creds encrypt --with-key=auto --name=boat-master-key - <file>`.
     `--with-key=auto` seals the key to the TPM2 (plus the host key) when the machine
     has one, so a copied disk or backup can't decrypt it;
  3. a fallback file `<data>/master.key`, auto-created with mode 0600. The server logs a
     WARNING at every start when it uses it.

  To move from the fallback file to a credential, encrypt the existing
  `master.key`, not a new key, or every stored secret becomes unreadable. The server
  refuses group- or world-readable key files. Subkeys are derived with
  HMAC-SHA256 under different labels:
  - the AES-256-GCM key for secrets and webhook secrets,
  - the key-hash pepper.
- **Roles and scopes:** owner/admin get `vms:read vms:write exec admin`. Member gets
  everything except `admin`. A key is bound to one team. Its effective scopes are
  (key scopes, or all scopes if none are listed) ∩ (scopes of the user's role in that
  team). A new key can never get more scopes than the key that created it.
- **Team isolation:** every `/v1/vms/{id}*` call first runs `GET` against boatd and checks
  `vm.team`. A VM that belongs to another team returns 404.
- **Secrets:** stored as AES-256-GCM with AAD `team/NAME`, so ciphertexts can't be
  moved between teams or names. Names are `UPPER_SNAKE` env var names, and each value
  is at most 64 KiB. Secrets are injected via `env` on create, start and prompt. boat-api
  strips the `env` field from any VM object it returns.
  - **Per VM:** `secrets:[names]` at create limits what a VM gets; `[]` gives it none.
    Forks inherit their parent's selection, because they inherit its disk anyway.
    The default (no field) is every team secret, except on a disk the team did not
    build (a share, an import, or a fork or snapshot of such a VM): that gets none
    unless named, which needs the `exec` scope, and it starts blocked at the AI proxy.
  - **What encryption does not cover:** anyone with the `exec` scope on a VM can read
    every secret injected into it. They sit in `/etc/boat/env`, in the environment of
    every process, and in whatever the agent writes to disk. Encryption at rest
    protects `state.json` and backups, not running VMs. Give a VM only the secrets
    it needs, and give untrusted users keys without `exec`.
  - **Rotation** (`PUT` again, `boat secrets rotate`): this doesn't change existing
    VMs. Only VMs created or started afterwards get the new value. Running VMs keep
    the old one until restarted, and their disks, forks and snapshots keep a copy of
    it for good. Rotating also does not scrub those disks or snapshots. To retire a
    leaked value, revoke it at the issuer, rotate the secret here, and restart or
    delete the VMs that had it. The PUT response reports `rotated:true` and a note.
- **Rate limit:** per key and per team, see Tenant model.
- **Audit:** JSON lines go to `<data>/audit.jsonl` (`-audit-log`, `-` = stderr). Each
  line has `ts, request_id, key_id, user_id, team_id, method, path, query, status,
  remote_ip, forwarded_for, ms`; `query` records, for example, which file was read. Logged requests:
  - every non-GET request;
  - every 401/403/429;
  - the GETs that disclose data or grant access: file reads, desktop links, port URLs,
    the secrets and keys lists, and webhook deliveries. ssh info is a POST, so it is
    always logged.

  Rotation and retention:
  - the file rotates at `-audit-max-bytes` (100 MiB) into `audit.jsonl.<UTC timestamp>`;
  - at most `-audit-keep` (20) rotated files are kept;
  - files older than `-audit-max-age` (90 days) are deleted at startup and at every
    rotation.

## AI credentials proxy

`PUT /v1/ai/credentials/{provider}` stores a team's provider key on the host, sealed
like a secret. Providers: anthropic, openai, openrouter, deepseek, mistral, moonshot, xai.

- **What a VM gets:** `ANTHROPIC_BASE_URL=<proxy>/anthropic` and
  `ANTHROPIC_API_KEY=boatvm_<vmid>_<hmac>`, likewise for each provider the team has.
  It is pushed to `/etc/boat/env` at create, start and fork, and passed to prompts.
  So `exec` in the VM can read a token, and never the key. This replaces a team
  secret of the same name.
- **The token:** valid for one run. It changes at every start and stop (TTL and idle
  stops included), and a fork gets its own. A copied token dies with its run.
- **The proxy:** a separate listener (`-ai-proxy-listen`, e.g. `10.200.0.1:8744`,
  needs boatd's `aiProxyPort`) that serves nothing else. It checks the token and the
  per-VM block, swaps in the key, and sends the request only to that provider's
  fixed host. It forwards no cookies or `X-Forwarded-*`, and refuses redirects.
- **Metering:** requests and tokens per VM, provider and model, read from a copy of
  the response (JSON or SSE). Flushed to state every 30 s, so a crash can lose up to
  that much.
- **Limits:** the proxy stops a VM from reading the key. It does not stop a VM from
  *using* it while the run lasts: an agent in the VM can spend your quota. Use
  `PUT /v1/vms/{id}/ai-access` to cut one off (`-ai-rate` caps requests per VM). VM
  traffic on the bridge is plain HTTP; tap isolation keeps other VMs from seeing it.
- **Off switch:** `PUT /v1/ai/proxy {enabled:false}` injects the raw keys at the next
  start. Without `-ai-proxy-url` and with the switch on, no key is injected at all.

## Agents

`POST /v1/vms/{id}/prompts` runs the agent CLI as a detached command, with the team's
secrets and proxy tokens. A conversation maps to the CLI's own session:
`claude --session-id/--resume`; `codex exec resume <thread_id>` and
`opencode run --session <sessionID>`, whose ids are read from the first turn's JSON
output. A fork copies the conversations along with the disk that holds their sessions.

**Steer is interrupt + resume**, not in-turn injection. These CLIs take no input during
a headless turn, so steer sends SIGINT (SIGKILL after 5 s), then starts a new turn in
the same session. Files and memory stay; a tool call in flight is cut off.

## Environments

A named base image + setup script + vars + secret selection. `POST /v1/vms {environment}`
(or the team's default environment; `no_env` opts out) applies it and runs the setup as
a detached command, returned as `setup_cmd_id`. `POST …/build` runs the setup once and
snapshots the result, so later VMs start from it with `from_build:true` and no setup.
Changing the image, size, script, vars or secrets makes a build stale, and new VMs run
the setup again. The builder VM stays stopped because deleting a VM deletes its
snapshots; it counts against `maxTotalVMs` and `maxDiskGiB`. **Secrets given to a
build end up on the snapshot's disk**, so select only what the setup needs.

## Snapshot sharing

A snapshot holds the VM's disk, so anything on it goes with a share, including
`/etc/boat/env` (raw secrets; the proxy token, which creating a share rotates so the
copy on the snapshot is dead). Taking a share
copies the snapshot into the taker's team; revoking stops further copies, not ones
already taken. A copy from another host is disk-only: it cold boots.

## OpenTelemetry

OTLP/HTTP JSON, stdlib only. `-otlp-endpoint URL -otlp-header k=v` exports request
spans (W3C `traceparent` is continued) to `/v1/traces` and VM lifecycle logs to
`/v1/logs`; the queue drops rather than blocks. `PUT /v1/telemetry` gives a team its own
endpoint for its lifecycle logs only. No metrics are exported yet (boat.dev exports
metrics and logs, and no traces). Prompts and command output are never exported.

## Key and webhook rotation

`POST /v1/keys/{id}/rotate {overlap_s}` keeps the id, scopes and team; the old secret
works until `prev_until`. `POST /v1/webhooks/{id}/rotate {overlap_s}` does the same for a
signing secret: during the overlap, `X-Boat-Signature` carries a `v1=` for each secret.
Accept any match. Both SDK helpers do.

## Webhooks

The server follows boatd's `GET /v1/events` SSE stream and reconnects with backoff. It
maps each event to a team using `team`, then `vm.team`, then a lookup of `vm_id`. It
then POSTs the event to every team webhook whose filter matches. A filter can be
empty (all events), `*`, an exact type, or a `prefix.*` pattern.

```
POST <url>
X-Boat-Event: vm.stopped
X-Boat-Delivery: evt_…
X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, "<t>." + body))
{"id","type","created","team_id","data":{…}}
```

- **Delivery limits:** only an HTTP attempt holds a slot; a retry waiting out its backoff
  does not. Per team: `-webhook-team-inflight` (8) attempts at once, and
  `-webhook-team-pending` (1000) deliveries pending, past which they are dropped and
  logged as `dropped:` in the delivery log.
- **Retries:** after 10s, 1m, 5m, 30m and 2h, for 6 attempts in total. A 4xx response
  stops the retries, except 408 and 429.
- **SSRF protection:** targets must be https. After DNS resolution, the dialer refuses
  loopback, RFC 1918, link-local, CGNAT and multicast addresses. `-allow-private-webhooks`
  turns these checks off for dev.
- **Signature check:** receivers should compare `v1` in constant time and reject old
  timestamps. Both SDKs ship a `verify_webhook` / `verifyWebhook` helper that does this.

## Port routes

The gateway serves `<port>-<vmid>.<vm-domain>` (`-vm-domain`; a bare domain means
https, and `scheme://host:port` is used as is for dev).

- **Private (default):** the gateway requires a token from
  `GET /v1/vms/{id}/ports/{port}/url`:
  - format: `<exp>.<sig>`, with `sig = hex(HMAC-SHA256(desktopKey, "route.<port>-<vmid>.<exp>"))`;
  - lifetime: `ttl_s` 60..604800, default `-port-url-ttl` = 1h, never past the minting key's expiry;
  - deleting the minting key, or removing its member, bumps the VM's route generation;
  - the URL carries `?boat_exp=&boat_sig=`. The gateway trades them for the host-only
    cookie `__Host-boat_route` and redirects without them;
  - scripts send the header `X-Boat-Route-Token: <exp>.<sig>` instead.
  No raw TCP port is opened, because a raw port can't check a token.
- **Public** (`auth:"public"`, CLI `--public`): boatd gets `public:true`. The route
  has no token check, and it also gets a raw TCP `host_port`. boatd lists it under
  `public` in routes.json, which is how the gateway knows.

## Desktop URLs

`<desktop-url>/<vm_id>?exp=<unix>&sig=hex(HMAC-SHA256(key, "<vm_id>.<exp>"))`. The
desktop gateway reads the same `-desktop-key-file` and must reject `exp < now`.

Before boat-api returns the link, it starts the guest desktop through boatd with
`POST /v1/vms/{id}/desktop {"action":"start","token": T}`, where `T = hex(HMAC-SHA256(key, "guest-desktop.<vm_id>"))`.
The gateway sends this token to the guest proxy as `Cookie: boat_desktop=T`.

## SSH jump host

With `-ssh-jump-keys-file /var/lib/boat/ssh/jump_authorized_keys`, every `POST /ssh`
records a grant (team, key, VM) and then rewrites the file atomically with mode 0640.
The file has one line per key:

```
restrict,port-forwarding,permitopen="10.200.x.y:22",… ssh-ed25519 AAAA…
```

boat-api rebuilds the file from the live boatd VM list. It does this after each `/ssh`
call, after `DELETE /v1/vms/{id}`, on every `vm.*` event and at startup. When a VM is
deleted or moves to another team, its IP drops out of the file, so a reused IP is never
reachable through an old grant. Grants for VMs that no longer exist are pruned.

## Store and scaling limits

All state lives in a single `state.json`, sealed under the master key:

- Each write takes one mutex, deep-copies the state, applies the change, writes it
  atomically (temp file + fsync + rename + dir fsync), and then swaps it in.
- The VM lifecycle paths (create, fork, start) do not wait for that fsync: they use
  `Store.Apply`, which swaps the change in and leaves the write to a background flusher.
  An fsync can take seconds when the disk is busy writing a snapshot (boatbench
  41652606509: 1.9 s of a 2.73 s new-from-snapshot). What must survive a crash goes
  somewhere durable instead: the secret selection is the VM's `boat.secrets` label in
  boatd. Losing the rest (environment name, AI and VM token generations, copied
  conversations) is harmless. `latency_test.go` fails if a lifecycle call waits for an
  fsync again. Each create, fork, stop and start logs one
  `<vm>: api <op> step=…ms … total=…ms` line.
- Readers, authentication included, get immutable snapshots and never wait for a write.
- Per team: 200 secrets and 2 MiB of sealed values, 100 live shares, 4 MiB of environment
  scripts and vars, 200 ssh keys. See `limits.go`.

This setup suits one server with a few thousand keys, webhooks and secrets. Webhook
delivery logs are the heaviest writer (≤100 per hook). Move to SQLite when the state
grows past roughly 10 MB or writes pass roughly 50/s. There are more limits:

- Webhook retries waiting in memory are lost when the server restarts.
- Rate-limit buckets live in each process, so running several replicas needs a shared limiter.
- `admin init` must run while the server is stopped (or restart the server afterwards).
- Removing a member revokes their API keys, but not ssh public keys they already
  registered. Those stay in the VMs' `authorized_keys` and in the jump-host grants,
  because grants are per team and key, not per user. To cut off a departing member,
  delete or re-key the VMs they could reach.

## Assumptions about boatd

These were checked against `components/boat/boatd/api.go`: the `{"vms":[…]}` list, `exec` with `cmd`, `?port=` on port delete, the snapshot routes and the desktop op.

These details go beyond the published boatd contract:

- `GET /v1/vms` returns either `[...]` or `{"vms":[...]}`.
- VM objects carry `id` and `team`, and may carry `ip`, `ssh_host`, `ssh_port` and `ports`.
- `start` accepts `{env}`.
- `exec` accepts `{cmd,env,cwd,stdin_b64,timeout_s}`.
- Port removal sends `DELETE /v1/vms/{id}/ports?port=N`. A port with `host_port` is public.
- Snapshot routes live at `/v1/vms/{id}/snapshots[/{sid}]`.
- `GET /v1/usage?team=` returns that team's usage.
- `POST /v1/vms/{id}/desktop {action:"start",token}` starts the guest desktop proxy.
- `GET /v1/vms` without `?team` lists all VMs (used for the jump-host sync).

## boat.dev compatibility (`/api/v1`)

`compat.go`, `compat_exec.go`, `compat_idem.go`: boat.dev's sandbox create/list/get/delete,
stop/resume/fork, commands, files and `/limits`, with its `{ok, type}` envelope and error
shape (every error under `/api/v1` is reshaped in `ServeHTTP`), cursor pagination and
`Idempotency-Key` (in memory, per team, 24 h). The handlers call the native ones in-process
(`callNative`), so auth, scopes, secrets and tokens are the same code. What is mapped and
what is not: docs/boat.md, "boat.dev API compatibility".

## Parity with docs.boat.dev

Checked against docs.boat.dev on 2026-11-01. Our paths are `/v1/vms`, theirs `/sandboxes`.
Not built, and why:

| boat.dev | Status here |
|---|---|
| Billing, plans, wallet, top-ups, free trial, seats | Not applicable when self-hosted; boatd's quotas stand in for limits |
| Agent self-signup + human claim (`/agent/identity`) | Not built: keys come from the operator or a team admin |
| Browser/GitHub sign-in, sessions | Not built: API keys only |
| Credential profiles and several accounts per credential | One set per team; no account switching |
| Claude/ChatGPT/Grok **subscription sign-ins** behind the proxy | Not built: they need OAuth flows and token renewal with each vendor. API keys only |
| AWS Bedrock credentials | Not built, and not proxied there either |
| Custom APIs through the proxy (`<NAME>_BASE_URL`) | Not built: the proxy has a fixed provider list |
| Harnesses pi, prime, kimi, mistral | Not built: claude, codex, opencode only |
| Steer **inside** a running turn | Interrupt + resume instead (see Agents) |
| Prompts queued while a turn runs | 409 instead; use `new` for a parallel conversation |
| Agent event stream (`/events`, tool calls) | Raw turn output only, through the logs route |
| Environment repositories (`env add-repo`, GitHub App, repo setup scripts), secret files, registries, versions and `upgrade` | Not built: one setup script, env vars and secrets |
| `boat extend`, `PATCH /sandboxes/{id}` (TTL, name), `recover` | Not built: boatd has no update route |
| Sandbox sharing inside an org; org roles beyond owner/admin/member | Teams only |
| Signed file upload/download URLs, `files/replace`, artifacts | Not built; files go through the API |
| Snapshot tree and single-file read without starting the VM | Not built: download the archive |
| Browser-only streaming, desktop recording | Not built here; see the desktop gateway |
| Data retention policies, deletion operations, zero data retention | Not built: deletes are immediate |
| OTLP metrics | Not built: traces + lifecycle logs instead |
| SDKs generated from the spec, `boat_sdk` names | Hand-written, with our own names |

## Development

```
go vet ./... && go test -race ./...
```

The tests run every handler against an in-memory fake boatd (`fakeboatd_test.go`).

- **Spec drift:** `TestOpenAPIMatchesRoutes` diffs the registered route table against
  `openapi.json` in both directions.
- **Schema drift:** every request the suite makes passes through a recorder
  (`openapidrift_test.go`) that validates request and response bodies against the
  spec's strict schemas, compares each handler's request struct with the spec, and,
  in a full run, fails for any operation no test drove to a 2xx.
- **Live scripts:** `TestLiveScripts` runs both live scripts against that stack. It
  skips when bash, curl, jq, python3 or node is missing.
- **Against a real stack** (boat-e2e.yml does this):
  - `BOAT_URL=… BOAT_KEY=<operator key> ./e2e-auth.sh` checks that a read-only key
    gets 403 on create, that escalation is refused, that members and team admins
    can't create teams, and that a second team gets 404 on another team's VM. It
    creates one VM and deletes everything it made.
  - `../sdk/e2e-live.sh` runs both SDKs through the whole surface, and the CLI when it
    is on PATH.
  - `../sdk/e2e-agent-from-env.sh` runs a real agent through the real proxy when CI has
    a provider key. With none it prints `SKIPPED:` and exits 0, so only
    `e2e-agent: PASS` in the log means an agent ran.
