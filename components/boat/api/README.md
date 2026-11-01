# boat-api

Public HTTP API for Boat (persistent Ubuntu VMs for AI agents). Go stdlib only.
It listens on `127.0.0.1:8740` and Caddy terminates TLS in front of it. It handles
auth, teams, secrets, webhooks, rate limits and audit, and forwards VM work to
`boatd` over `/run/boat/boatd.sock`.

```
boat-api admin init -data /var/lib/boat-api [-user alice] [-team acme]   # prints the first owner key once
boat-api serve -data /var/lib/boat-api -boatd /run/boat/boatd.sock \
  [-rate 10 -burst 40] [-desktop-url https://HOST/desktop -desktop-key-file F] \
  [-ssh-user ubuntu] [-ssh-jump jump@HOST:2222 -ssh-jump-keys-file F] [-images ubuntu-24.04,ubuntu-22.04]
```

Run `admin init` before `serve`. The server loads its state at startup.

## Endpoints

The full schema is in `openapi.json` (OpenAPI 3.1), which the server also serves at
`GET /v1/openapi.json`. Each operation's `x-boat-scope` field names the scope it needs.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /healthz`, `GET /v1/openapi.json` | none | no auth |
| `GET /v1/me` | any | user, team, role, effective scopes |
| `GET /v1/images` | any | from `-images`; the first one is the default |
| `GET /v1/usage[?from&to]` | vms:read | boatd `/v1/usage?team=`: VM-seconds by size, snapshot-days, starts |
| `GET/POST /v1/vms` | vms:read / vms:write | create body `{name,size,image,ttl_s,labels,env}`; team secrets are added to `env` |
| `GET/DELETE /v1/vms/{id}` | vms:read / vms:write | |
| `POST /v1/vms/{id}/start\|stop\|fork` | vms:write | start re-injects secrets `{env}`; fork `{name}` |
| `POST /v1/vms/{id}/exec` | exec | `{cmd[],env,cwd,stdin_b64,timeout_s}` → NDJSON `{stream,data_b64}` … `{exit}` |
| `POST /v1/vms/{id}/prompt` | exec | `{prompt,agent:claude\|codex\|opencode,model,cwd,timeout_s}`, streamed like exec; team secrets go in as env |
| `GET/PUT /v1/vms/{id}/files?path=` | exec | raw bytes; the path must be absolute; `-max-file-bytes` |
| `GET/POST /v1/vms/{id}/ports`, `DELETE …/ports/{port}` | vms:read / vms:write | `{port,public}` |
| `GET/POST /v1/vms/{id}/snapshots`, `DELETE …/snapshots/{sid}` | vms:read / vms:write | proxied to boatd |
| `POST /v1/vms/{id}/ssh` | exec | `{public_key}` is appended (deduped) to `/home/<ssh-user>/.ssh/authorized_keys`; returns `{host,port,user,jump?}` |
| `GET /v1/vms/{id}/desktop` | exec | `{url,expires_at}`, signed and short-lived (`-desktop-ttl`, default 5m) |
| `GET/POST /v1/teams`, `GET /v1/teams/{id}` | any / admin | the creator becomes owner |
| `POST /v1/teams/{id}/members`, `DELETE …/members/{user}` | admin + team owner/admin role | creating a member returns their first key; the last owner cannot be removed |
| `GET/POST /v1/keys`, `DELETE /v1/keys/{id}` | any | the token is shown only once |
| `GET /v1/secrets`, `PUT/DELETE /v1/secrets/{NAME}` | vms:read / admin | values are never returned |
| `GET/POST /v1/webhooks`, `DELETE /v1/webhooks/{id}` | admin | `{url,events[]}`; the secret is shown only once |
| `GET /v1/webhooks/{id}/deliveries`, `POST …/test` | admin | the last 100 attempts |

Errors use the shape `{"error":{"code","message","request_id"}}`. Every response carries an
`X-Request-Id` header. A client-supplied `X-Request-Id` (≤64 chars) is kept.

## Auth design

- **Token:** `boat_<16 hex id>_<64 hex secret>`, i.e. 256 random bits.
- **Hash:** `hex(HMAC-SHA256(pepper, salt ‖ 0x00 ‖ secret))` with a 16-byte salt per key.
  A slow KDF (PBKDF2/scrypt) only helps against low-entropy passwords. Brute-forcing
  256-bit secrets is already infeasible, and the HMAC keeps auth cheap on every request.
  The pepper is derived from the master key, so a leaked `state.json` alone cannot be
  used to test guesses. Comparison is constant-time.
- **Master key:** 32 bytes (raw or hex), taken from the first source that exists:
  `-master-key-file`, then `$CREDENTIALS_DIRECTORY/boat-master-key` (systemd
  `LoadCredential=`), then `<data>/master.key`. The last one is auto-created with mode 0600.
  The server refuses group- or world-readable key files. Subkeys are derived with
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
- **Rate limit:** a token bucket per key (`-rate`/`-burst`). A blocked request gets
  429 with `Retry-After`.
- **Audit:** JSON lines go to `<data>/audit.jsonl` (`-audit-log`, `-` = stderr). Each
  non-GET request and each 401/403 is logged with `ts, request_id, key_id, user_id,
  team_id, method, path, status, remote_ip, forwarded_for, ms`.

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

- **Retries:** after 10s, 1m, 5m, 30m and 2h, for 6 attempts in total. A 4xx response
  stops the retries, except 408 and 429.
- **SSRF protection:** targets must be https. After DNS resolution, the dialer refuses
  loopback, RFC 1918, link-local, CGNAT and multicast addresses. `-allow-private-webhooks`
  turns these checks off for dev.
- **Signature check:** receivers should compare `v1` in constant time and reject old
  timestamps. Both SDKs ship a `verify_webhook` / `verifyWebhook` helper that does this.

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

All state lives in a single `state.json`, guarded by one mutex:

- Each write deep-copies the state, applies the change, writes it atomically
  (temp file + fsync + rename + dir fsync), and then swaps it in.
- Readers get immutable snapshots.

This setup suits one server with a few thousand keys, webhooks and secrets. Webhook
delivery logs are the heaviest writer (≤100 per hook). Move to SQLite when the state
grows past roughly 10 MB or writes pass roughly 50/s. There are more limits:

- Webhook retries waiting in memory are lost when the server restarts.
- Rate-limit buckets live in each process, so running several replicas needs a shared limiter.
- `admin init` must run while the server is stopped (or restart the server afterwards).

## Assumptions about boatd

These were checked against `components/boat/boatd/api.go`: the `{"vms":[…]}` list, `exec` with `cmd`, `?port=` on port delete, the snapshot routes and the desktop op.

These details go beyond the published boatd contract:

- `GET /v1/vms` returns either `[...]` or `{"vms":[...]}`.
- VM objects carry `id` and `team`, and may carry `ip`, `ssh_host`, `ssh_port` and `ports`.
- `start` accepts `{env}`.
- `exec` accepts `{cmd,env,cwd,stdin_b64,timeout_s}`.
- Port removal sends `DELETE /v1/vms/{id}/ports?port=N` with the body `{"port":N}`.
- Snapshot routes live at `/v1/vms/{id}/snapshots[/{sid}]`.
- `GET /v1/usage?team=` returns that team's usage.
- `POST /v1/vms/{id}/desktop {action:"start",token}` starts the guest desktop proxy.
- `GET /v1/vms` without `?team` lists all VMs (used for the jump-host sync).

## Development

```
go vet ./... && go test -race ./...
```

The tests run every handler against an in-memory fake boatd (`fakeboatd_test.go`).
