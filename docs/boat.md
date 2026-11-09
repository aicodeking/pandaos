# boat: self-hosted agent VMs on a dedicated server

> **Status: pre-release.** The first release has not been tagged, so no release has shipped it. Do not offer it to untrusted tenants until
> you have done the [operator checklist](#operator-checklist) **and** had an external penetration test.
> It has had internal review rounds only, and those kept finding serious bugs: the latest was a cross-tenant
> desktop takeover through a same-site websocket. It is single-host, and it is not API-compatible with
> boat.dev (see [parity](#feature-parity-with-boatdev)).

`boat` turns one dedicated server into a self-hosted alternative to boat.dev.
Each agent gets a persistent Firecracker microVM, and you get an API, a CLI,
SDKs, a web dashboard, ssh, port URLs, a browser desktop, forks and snapshots. The server
profile (`nixos/profiles/server.nix`) is headless PandaOS with
`services.boat.enable = true`.

| Part | Where | What it does |
|---|---|---|
| `boatd` | `components/boat/boatd` | Root VM manager: Firecracker + jailer, bridge `boatbr0`, `10.200.0.0/16` NAT, nftables tenant isolation, snapshots. Writes `/run/boat/routes.json`. |
| `boat-api` | `components/boat/api` | REST API on 127.0.0.1:8740 (keys, teams, secrets, webhooks, metering, link minting). User `boat`. |
| `boat` | `components/boat/cli`, `sdk` | CLI (`--json` everywhere) and the Python/TypeScript SDKs. |
| guest | `components/boat/guest` | Ubuntu 24.04 rootfs + kernel with `boat-agent`; the `full` image adds a KasmVNC desktop on guest port 6080. |
| gateway | `components/boat/gateway` | Caddy (TLS), `boat-gateway` (port routes, desktops; its own uid, no access to boatd), the ssh jump host, and the static dashboard at `https://api.<domain>/`. |
| module | `components/boat/module.nix`, `ops.nix` | NixOS options and units: the above plus reboot detection, maintenance reboots, backups. |

## Operator checklist

Before you hand out API keys:

1. **Hardware with KVM.** `ls /dev/kvm` in the rescue system. Without it, boatd refuses to start (`boatd: /dev/kvm is missing ...`, exit 78, no restart loop).
2. **Separate domains for tenant content.** Pick a base domain (`boat.example.com`) and a **different registrable domain** for what tenants control: `vmDomain` (port routes) and `desktopDomain` (desktop pages), e.g. `boatvms.example.net` and `desktop.boatvms.example.net`, or names under a suffix on the [Public Suffix List](https://publicsuffix.org/). With the defaults (`vm.<domain>`, `desktop.<domain>`) a tenant's page is same-site with `api.<domain>` and can set cookies on the parent domain. A rebuild warns about this until you fix it. Tenants' origins are always same-site with each other unless the VM domain is on the PSL. The gateway's Origin checks cover requests, and it drops `Domain` from every `Set-Cookie` a tenant app sends, but it cannot see a page's own `document.cookie`, so a tenant's script can still set a cookie on `vmDomain` that reaches sibling tenants. **Only a `vmDomain` on the PSL stops that; adding it is your job, no code here can.**
3. **DNS** (see below), including both wildcards.
4. **TLS with DNS-01.** Set `gateway.dnsProvider`. Without it, a tenant creating routes can exhaust your Let's Encrypt limit, after which new certificates fail for everyone; the module refuses to evaluate unless you set `gateway.allowOnDemandTLS = true` (the installer does, since it has no DNS credentials, and every rebuild warns about it).
5. **Backups.** Set `services.boat.backup` to a restic repository you control, keep the repository password off the box, and keep an offline copy of both sealed keys (see "Secrets"). A backup alone does not restore secrets on a new host. Test a restore.
6. **Reboot schedule.** Turn on `services.boat.maintenance.reboot` (default: Sunday 04:00, only when a reboot is pending), or watch `boat_reboot_pending` and reboot by hand.
7. **Abuse policy.** You are the hosting provider for everything your tenants run: public port URLs, outbound traffic from your IPs, mail, scanning and mining. Publish an acceptable-use policy, keep `abuse@` working for your provider (Hetzner and OVH forward complaints and can null-route the server), keep team quotas tight (defaults: 10 running VMs, 50 total, 32 vCPU, 64 GiB RAM, 1000 GiB disk, 100 snapshots, 50 routes), and give keys only to people you trust. boatd drops outbound SMTP (25/465/587) unless `boatdSettings.allowSMTP = true`, and can cap each VM's egress (`boatdSettings.egressMbit`).
8. **Monitoring.** node-exporter is on :9100 (not opened in the firewall). It includes the textfile metric `boat_reboot_pending`. Usage per team is at `GET /v1/usage`.
9. **External penetration test** before untrusted tenants. Nobody outside this project has reviewed it.

## Install on a dedicated server (Hetzner, OVH)

1. Boot the PandaOS ISO (Hetzner: virtual ISO in the Robot console or KVM-over-IP; OVH: IPMI).
2. Run:
   ```sh
   panda-install --profile server --disk /dev/nvme0n1 --data-disk /dev/nvme1n1 \
     --domain boat.example.com --acme-email ops@example.com \
     [--ipv6-prefix 2a01:4f8:xxx:yyy:b0a7::/80] --ssh-key "ssh-ed25519 ..."
   ```
   `--data-disk` erases the second disk and formats it as btrfs (`PANDADATA`) for `/var/lib/boat`. It is pinned by UUID as a required mount in `/etc/pandaos/disk.nix` and recorded in `/etc/pandaos/data-disk`. If that disk is ever missing, boatd and boat-api refuse to start rather than write VM data to the system disk. Without `--data-disk`, `/var/lib/boat` is a btrfs subvolume on the system disk. VM disks are reflink copies either way, so fork and snapshot are near-free. To mirror later: `btrfs device add` + `btrfs balance start -dconvert=raid1 -mconvert=raid1`.
   Use the `iso-server` ISO: the other ISOs do not carry the server system.
3. Reboot, log in, and apply your settings now (the daily update would also do it):
   ```sh
   sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
   ```
4. Add more settings to `/etc/pandaos/local.nix`:
   ```nix
   services.boat = {
     vmDomain = "boatvms.example.net";
     # Hashes: boat_guest_SHA256SUMS on the release page. "slim" also exists.
     images."ubuntu-24.04/vmlinux" = {
       url = "https://github.com/AICodeKing/pandaos/releases/download/vX.Y.Z/boat-guest-vmlinux"; sha256 = "...";
     };
     images."ubuntu-24.04/rootfs.ext4.zst" = {
       url = "https://github.com/AICodeKing/pandaos/releases/download/vX.Y.Z/boat-guest-rootfs-full.ext4.zst"; sha256 = "...";
     };
     gateway.caddyPackage = pkgs.caddy.withPlugins {
       plugins = [ "github.com/caddy-dns/cloudflare@v0.2.1" ]; hash = "sha256-...";
     };
     gateway.dnsProvider = "cloudflare {env.CF_API_TOKEN}";
     gateway.environmentFile = "/var/lib/secrets/caddy-dns.env";   # CF_API_TOKEN=...
     backup = {
       enable = true;
       repository = "sftp:u123@u123.your-storagebox.de:/boat";
       passwordFile = "/var/lib/secrets/restic-boat.pass";
     };
     maintenance.reboot = true;
   };
   ```
5. **First admin key:** run `sudo boat-admin init -user me -team ops`. It prints an owner API key once. boat-admin runs `boat-api admin init` as `boat` with the sealed master key, then restarts boat-api. Then run `boat login --url https://api.boat.example.com --key ...`.

### DNS records

| Name | Type | Value |
|---|---|---|
| `api.boat.example.com` | A / AAAA | server |
| `desktop.boat.example.com` | A / AAAA | server |
| `*.desktop.boat.example.com` | A / AAAA | server (one origin per VM desktop) |
| `ssh.boat.example.com` | A / AAAA | server |
| `*.boatvms.example.net` (= `vmDomain`) | A / AAAA | server (port routes `<port>-<vmid>.<vmDomain>`) |

### TLS

- **Wildcards via DNS-01 (recommended).** Set `gateway.dnsProvider` and use a Caddy build with the matching [caddy-dns](https://github.com/caddy-dns) plugin. Caddy then gets `*.<desktopDomain>` and `*.<vmDomain>` certificates, so the provider must manage both zones. New VMs and ports work instantly, and their names stay out of Certificate Transparency logs.
- **HTTP-01 fallback, not for production.** Set `gateway.allowOnDemandTLS = true`. Caddy then gets one certificate per desktop and per port URL on the first request, after asking boat-gateway (127.0.0.1:8743) whether that host is live. The costs: a few seconds' wait on first use; every name is published in CT logs; and Let's Encrypt's limit of 50 certificates per registered domain per week is shared, so **one tenant creating routes can stop new certificates for everyone**. `quotas.maxPorts` limits this but does not remove it.

### IPv6 subnet routing

- **Hetzner** routes a `/64` to the server. Give VMs a sub-prefix such as `2a01:4f8:a:b:b0a7::/80` and keep `::2` on the uplink; no NDP proxy is needed.
- **OVH** puts the `/64` on-link. Either have it routed, or use NDP proxying (`proxy_ndp=1` + `ip -6 neigh add proxy`, or `ndppd`). The module sets `accept_ra=2` so the uplink keeps taking RAs while forwarding.
- **Extra IPv4:** route the addresses to the server and list them in `publicIPv4Pool`; boatd maps them 1:1 to VMs.

## Sizing

The default sizes are `small` 2 vCPU/4 GiB, `default` 4/8, `large` 8/16 and `xlarge` 16/32, with sparse disks. The default team quota is 10 VMs / 32 vCPU / 64 GiB.

- **RAM** is the limit, because guest memory is not overcommitted. Budget the sum of running VMs' memory plus 2 GiB for the host.
- **Stop time.** On stop or reboot, boatd writes every running VM's memory to disk. `services.boat.stopTimeoutSec` (default 900 s) must cover the total RAM of running VMs divided by the disk's write speed. That is about 128 GiB at 1 GB/s ≈ 2-3 min, so 900 s leaves room.
- **CPU** can be overcommitted about 2-4x for agent workloads, which mostly wait on LLMs.
- **Disk** needs about 3 GB per image, plus the blocks each VM writes, plus memory snapshots.
- **Example:** an AX102 (16 cores, 128 GB, 2x1.92 TB NVMe) runs about 14 `default` VMs at once and holds hundreds stopped.

## Gateway design

```
Internet --443--> Caddy --api.D-----------------------> boat-api :8740 (/internal/* -> 404)
                        --desktop.D/<id>?sig----------> boat-gateway: 302 -> <id>.desktop.D (sets nothing)
                        --<id>.desktop.D--------------> boat-gateway --> <vm ip>:6080 (guest token proxy)
                        --<port>-<id>.<vmDomain>------> boat-gateway --> <vm ip>:<port>
         --2222--> sshd (user jump only) --direct-tcpip, permitopen--> <vm ip>:22
```

- **One origin per VM.** Each desktop is `<id>.desktop.D` and each port route is `<port>-<id>.<vmDomain>`. Session cookies are `__Host-` prefixed: host-only, `Secure`, `Path=/`, `HttpOnly`. Browsers refuse a `__Host-` cookie that carries a `Domain`, so a page on another VM's origin can neither read nor plant one. Desktop cookies are `SameSite=Strict`. A browser withholds a Strict cookie on the redirects of a navigation that began on another site, so for `Sec-Fetch-Site: cross-site` the gateway answers with a small same-origin page that starts a fresh navigation; everything else gets a 302. Every VM's credential is bound to its id by the HMAC: VM A's cookie or link gets 401/403 on VM B's origin. This is checked in the e2e.
- **Origin checks.** All VM origins are same-site with each other, so `SameSite` does not stop a page on VM A from sending B's cookie along, for example on `wss://<B>.desktop.D/`. The gateway therefore refuses (403) a desktop request whose `Origin` is not exactly its own origin, any desktop request marked `Sec-Fetch-Site: same-site` or `cross-site` unless it carries a signed link, and framing (`frame-ancestors 'self'`). A private port route accepts its cookie only from its own origin or another port of the same VM, and from top-level navigations; a program sending `X-Boat-Route-Token` is not limited, since it holds the token itself. Consequence: `<img>` or `<script>` from one private port of a VM to another is refused, so use relative paths or a public port. Desktops cannot be embedded in an iframe on another origin.
- **Dashboard.** Static files from `components/boat/dashboard/web`, served from the read-only Nix store at `https://api.D/`, on the same origin as the API, with a strict CSP (`default-src 'self'`, no framing). `/v1/*` and `/healthz` go to boat-api; `/internal/*` is 404. It holds the user's API key in the browser, which is why it is never on a tenant-controlled origin.
- **Desktop links.** `GET /v1/vms/<id>/desktop` returns `https://desktop.D/<id>?exp&sig` (sig = `hex(HMAC-SHA256(key, "<id>.<exp>"))`, 5 min by default). The gateway 302s it to `https://<id>.desktop.D/?exp&sig`, which checks the signature, sets `__Host-boat_gw` and drops the signature from the URL. Toward the guest, the gateway removes the client's `boat_*`/`__Host-boat_*` cookies and sends `boat_desktop=hex(HMAC(key, "guest-desktop.<id>"))`. boat-api starts the guest desktop with that same token.
- **Port routes are private by default.** A route needs a token: `token = <exp>.hex(HMAC(key, "route.<port>-<id>.<exp>"))`, minted by `GET /v1/vms/<id>/ports/<port>/url` (`boat ports url VM PORT`). Programs send it as `X-Boat-Route-Token`; browsers open `…/?boat_exp&boat_sig` once, which sets the host-only `__Host-boat_route` (SameSite=Lax). A port is reachable without a token only if added with `--public`, which boatd lists in routes.json `"public"`. When the list is missing, routes stay private (fail-closed). Tokens and cookies never reach the VM.
- **Revoking port tokens.** A token lives up to 7 days. `POST /v1/vms/<id>/port-tokens/revoke` ends every token, link and cookie of that VM's private ports, and so does removing a port; a token minted afterwards signs `route.<port>-<id>.g<gen>.<exp>`. boat-gateway polls boat-api's loopback-only `/internal/route-gens` every 2 s, so it takes effect within about 2 s, and a token minted in that window can also get 401 until the next poll. If boat-api goes down the gateway keeps its last table. If the gateway starts while boat-api is down, private routes answer 503 until boat-api answers, because a revoked token cannot be told from a good one; public routes are not affected.
- **Tenant cookies.** The gateway removes the `Domain` attribute from each `Set-Cookie` in a guest's response, so the cookie stays on that VM's own host. See checklist item 2 for what it cannot remove.
- **Live routes.** boat-gateway (Go stdlib, websockets included) re-reads `/run/boat/routes.json` whenever its mtime or size changes, so new VMs and ports never reload Caddy.
- **SSH:** OpenSSH ProxyJump.
  ```sh
  boat ssh <vmid>                                    # ProxyCommand ssh -W via the jump host
  ssh -J jump@ssh.boat.example.com:2222 dev@10.200.x.y
  ```
  Port 2222 admits only the system user `jump`. jump has no password, shell, tty, agent, X11 or socket forwarding (`ForceCommand false`). It can open only `direct-tcpip` channels to the targets in its key's `permitopen="<ip>:22"`. boat-api writes those lines to `/var/lib/boat/ssh/jump_authorized_keys` and rebuilds them on grants, VM deletion and VM events. The AuthorizedKeysCommand `/etc/boat/jump-keys` passes only lines of exactly the form `restrict,port-forwarding,permitopen=...`. It also drops every target that is not a running VM in boatd's routes.json, so a grant dies the moment its VM stops, expires (TTL) or is deleted, even before boat-api prunes it. An IP reused by a new VM is never reachable with an old grant.

## Security model

- **VM isolation.** Each VM is a KVM guest run by Firecracker (a minimal Rust VMM) inside the jailer: its own uid/gid (from 900000), a chroot, a mount namespace, its own cgroup under `boat-vms`, outside `boatd.service`, so a boatd crash spares the VMs, and seccomp. A guest root exploit must escape KVM, then the jailed, unprivileged Firecracker process.
- **Tenant network isolation.** Every VM has its own tap on `boatbr0`. nftables drops tap-to-tap traffic, except within a team that has `allowIntraTeam`, and NAT is outbound only. From a VM, the host answers only DNS on the bridge address, the AI proxy port and ICMP (`boatd/netw.go`); everything else addressed to the host from `boatbr0` is dropped, including 80/443/22/2222.
- **Web isolation.** Every VM's desktop and every port route has its own origin and host-only `__Host-` cookies, and credentials are HMAC-bound to the VM (and port). Port routes are private by default. With a separate registrable `vmDomain`, tenant apps cannot touch `api.`/`desktop.` cookies either. `api.D` uses bearer tokens, not cookies.
- **Host services.** boatd is the only root daemon. boat-api runs as `boat`; boat-gateway, which faces the internet, runs as its own `boat-gateway` with no access to boatd's socket (0660 `root:boat`, in a 0751 directory) or to boat-api's data. Both have no capabilities, `ProtectSystem=strict`, a syscall filter and MemoryDenyWriteExecute. `/internal/*` is never published. boatd and boat-api have `OOMScoreAdjust=-900` and each Firecracker 500, so the host OOM killer takes a VM before them. boatbr0 never accepts IPv6 router advertisements, so a VM cannot reconfigure the host. Firecracker is pinned to the release e2e tests (v1.17.0).
- **Disks a team did not build.** A VM from an imported or shared snapshot, or from a fork or snapshot of such a VM, runs that disk's init before anyone can look at it. So it gets **no team secrets** unless the create request names them in `secrets`, which needs the `exec` scope (such a key could read them from any VM anyway); an environment's secret selection does not count. It also starts blocked at the AI proxy, and with the proxy off it gets no raw keys; `PUT /v1/vms/<id>/ai-access` opens it. The mark is recorded when the VM is created, so VMs made before this rule are not marked.
- **What a share gives away.** A snapshot holds the VM's disk, including `/etc/boat/env`. Creating a share moves the source VM to a new AI proxy token, so the token on the snapshot is dead; processes already running in that VM keep the dead token until they restart. Team secrets on the disk and raw AI keys (proxy off) go with the share and cannot be taken back: revoke them at their issuer.
- **In-flight limits.** Per team, at most 64 open exec, SSE, log and download streams (`-team-streams`) and 8 file uploads and imports (`-team-uploads`); signed-link downloads count separately from the team's. Per VM, at most 16 AI proxy requests (`-ai-vm-inflight`). Past a limit the API answers 429. Uploads go to an unlinked temp file on the data disk, not memory. The AI proxy still shares boat-api's process, so a flood of allowed requests costs the API's memory too.
- **State caps.** Per team: 200 secrets and 2 MiB of sealed secret values, 100 live shares, 4 MiB of environment scripts and vars, 200 ssh keys. The command history keeps 8 KiB of each argv.
- **Secrets.** `state.json` is sealed as a whole under the master key, because it holds environment vars, setup scripts, command lines and prompts. A plaintext file from an older version is sealed when boat-api opens it, and an older boat-api cannot read the sealed file, so do not roll back past this change. Tenant secrets and webhook keys are also sealed individually with AES-256-GCM under boat-api's master key. That key and the desktop/route HMAC key exist on disk only as systemd credentials (`/var/lib/boat/secrets/*.cred`, directory 0700 root), encrypted with the TPM2 when present plus the host key `/var/lib/systemd/credential.secret`, and handed to the services with `LoadCredentialEncrypted`. Plaintext keys from older installs are sealed, verified and deleted on upgrade. Backups contain only the sealed files, so **a backup alone cannot decrypt anything on another host. Keep offline copies:** `sudo systemd-creds decrypt --name=boat-master-key /var/lib/boat/secrets/boat-master-key.cred - | base64` (same for `boat-desktop-key`). API keys are stored as salted HMACs. Caddy's DNS token belongs in `gateway.environmentFile`.
- **Host hardening** comes from PandaOS: nftables, key-only ssh, sshguard, sysctl hardening, daily updates with health-check rollback.
- **Not defended:** a malicious server admin; side channels between VMs on shared cores (no core pinning); one team saturating the disk (no IOPS limits) or, without `egressMbit`, the uplink; a full compromise of the host through a KVM or Firecracker bug (no second layer beyond the jailer); and abuse from your IPs beyond what your policy and quotas stop.

### Tenant model

- **Teams** own VMs, secrets, webhooks and keys. A key belongs to one user in one team and has scopes (`vms:read`, `vms:write`, `exec`, `admin`). Admins manage members and secrets.
- **Keys.** A key can list, rotate, delete or read usage of a key only if that key's scopes are within its own. Another user's key also needs the `admin` scope and a role no higher than the caller's, and only the operator can touch the operator's keys. Anything else is 404.
- **Deleting a team** also deletes its snapshots, imported ones included.
- A team sees and reaches only its own VMs: API objects are filtered by team, nftables blocks other teams' VMs, jump grants list only the team's VM IPs, and every desktop link and route token is bound to one VM.
- **Quotas** (`services.boat.quotas`, per-team overrides with `boatdSettings.teams`) cap running and total VMs, vCPU, memory, disk, snapshots and routes, all finite by default. Metering (`/v1/usage`) records VM-seconds per size and snapshot-days per team. There is no billing.

## Upgrades and reboots

- **Updates.** The daily unattended update (`updates.nix`) switches to the new system with a health check and rolls back on failure. boatd is never restarted by a switch (`restartIfChanged = false`), so running VMs are untouched. boat-api, boat-gateway and Caddy restart in seconds; the API's clients retry.
- **Pending reboot.** `boat-reboot-check` (hourly) compares the booted kernel, initrd and modules, and boatd plus its config and Firecracker paths, with the installed ones. It logs `boat: reboot pending (changed: ...)`, writes `/run/boat/reboot-pending` and exports `boat_reboot_pending` (0/1) with a `reason` label.
- **Maintenance reboot.** With `maintenance.reboot = true`, a timer (`onCalendar`, default Sunday 04:00, ±10 min) reboots when a reboot is pending (`onlyWhenPending = false` reboots every time). The reboot drains on its own: stopping boatd pauses and memory-snapshots every running VM (`stopTimeoutSec`), and boatd resumes them after boot, with processes, tmux sessions and open files intact. TCP connections into VMs drop.
- **Manual:** `sudo systemctl restart boatd` applies a new boatd without a reboot (VMs snapshot and resume); `sudo reboot` for a kernel.
- **Rollback:** pick the previous generation in the boot menu, or `sudo nixos-rebuild switch --rollback`. VM state on `/var/lib/boat` is independent of the system generation.

## Backups

`services.boat.backup` (restic, to a repository you provide):

1. Each run (default 03:00, `Persistent`) takes **read-only btrfs snapshots**: `/var/lib/boat` as `.backup/top`, and every nested subvolume (each VM's directory, with boatd's `btrfsQgroups`) as `.backup/sub/<path>`. A btrfs snapshot does not include nested subvolumes, so each one is snapshotted on its own. Each VM's disk and memory snapshot is crash-consistent; the API database and sealed master key come with `top`.
2. restic uploads the snapshots (images excluded; they are re-downloadable), the btrfs snapshots are deleted, and `pruneOpts` apply (7 daily, 4 weekly, 6 monthly).

Restore onto a fresh boat host (`checks.boat-ops` runs the restic restore into a scratch directory and the key recovery; it does not restart real VMs from a restore):
1. Stop boatd and boat-api.
2. `restic restore latest --target /tmp/r`, then copy `/tmp/r/var/lib/boat/.backup/top/.` and each `.backup/sub/<path>` back to `/var/lib/boat/<path>` (`cp -a --reflink=auto`).
3. Reseal both keys from your offline copies, because the `.cred` files in the backup open only on the old host: `base64 -d copy | sudo systemd-creds encrypt --with-key=auto --name=boat-master-key - /var/lib/boat/secrets/boat-master-key.cred`, and the same for `boat-desktop-key`.
4. Start boatd and boat-api again.

Test a restore before you need one.

## Feature parity with boat.dev

Compared with the full [docs.boat.dev](https://docs.boat.dev/llms.txt) page list, read on 2026-11-01. **yes** = works and is exercised by the `boat-e2e` workflow on real VMs. **unit** = implemented, tested only against a fake boatd or a stub. **partial** = a smaller version. **no** = missing. **n/a** = does not apply to self-hosting. Features that boat.dev documents and this table omits should be read as **no**.

**Partly drop-in.** The native API is `/v1/vms` and the SDKs are this project's own. A compatibility layer under `/api/v1` speaks boat.dev's sandbox API for its most used calls, envelope, cursor pagination and `Idempotency-Key` included (see [boat.dev API compatibility](#boatdev-api-compatibility-apiv1)), so code that only creates, lists, runs commands in, moves files to, stops, resumes, forks and deletes sandboxes can point its base URL at `https://<api host>/api/v1`. Everything else has to be ported to `/v1`.

| boat.dev area | Feature | Here | Notes |
|---|---|---|---|
| Sandboxes | create, list, get, delete | yes | `boat new/list/rm` |
| | update (PATCH), recover, share/unshare | no | |
| | stop, resume | yes | `boat stop`, `boat start` or `resume`; memory restored (same `boot_id`) |
| | force stop | unit | `boat stop --force`: no snapshot, the next start cold-boots from disk |
| | fork | yes | new hostname, IP, machine-id, host keys, entropy |
| | per-sandbox usage | unit | `boat vm-usage` |
| | `current` shortcut, `boat onboard`, sign up from an agent | no | keys come from the operator |
| | `boat self` / `host` inside the VM | unit | `BOAT_ID`, `BOAT_URL` and a key for that VM only, with `-vm-api-url` |
| Commands | exec with output | yes | |
| | detached commands, follow, signal, logs, history (also on a stopped VM) | yes | `boat cmd`; driven through the SDKs in `sdk/e2e-live.sh` |
| | `ps` | yes | `boat ps`, through `sdk/e2e-live.sh` |
| Files | read, write | yes | `boat scp` |
| | list, search, move, delete, folder, info | yes | `boat files`, through `sdk/e2e-live.sh` |
| | permissions | unit | |
| | find in files, replace, upload/download URLs, artifacts | no | |
| Connect | ssh | yes | through the jump host; IP target; no `boat ssh-config` for editors |
| | VS Code, Cursor | partial | works with a hand-written `ProxyJump`; not automated |
| | desktop in the browser | partial | KasmVNC over a websocket (`full` image), no WebRTC. 1080p through the TLS edge in CI, software rendering: 34-40 fps shown, 47-55 decoded; 60 not reached. See [frame rate](#what-the-tests-prove) |
| | browser-only view, record to MP4, custom wallpaper | no | |
| | embed desktop in an iframe | no | refused on purpose, see Origin checks |
| Public URLs | HTTPS port URL, token protection | yes | private by default |
| | raw TCP | yes | `--public` host port |
| | raw UDP, rename subdomain, stable names across forks | no | |
| | firewall rules per sandbox | no | SMTP blocked, `egressMbit` cap, operator-wide only |
| Long-running | TTL and idle auto-stop | yes | |
| | extend the deadline, prompts pushing it out | no | |
| | systemd services across stop/resume | yes | memory restore keeps all processes; boat.dev restarts services instead |
| Environments | image, setup script, env vars, secret selection, prebuilt snapshot | yes | `boat env`; setup and build run in `sdk/e2e-live.sh` |
| | versions | unit | a counter; no upgrade of running VMs |
| | repo cloning, secret files, private registries, safe-for-third-parties, per-org access | no | |
| Snapshots | create, list, delete, new from snapshot | yes | memory + disk |
| | share code, import | yes | through `sdk/e2e-live.sh` |
| | download | yes | through `sdk/e2e-live.sh` |
| | browse file tree, retention rules | no | |
| Machines | sizes | yes | operator-defined |
| | resize | unit | `boat resize` |
| | fail-fast | unit | `--fail-fast` on `new`, `fork`, `start`/`resume`: 503 `no_ready_machine` at once |
| | batch create | unit | `boat new --count N` (1-16): one request, all or none in boatd |
| | nested virtualization | no | |
| | Docker in the sandbox | yes | `full` image |
| Agents | `boat prompt` | partial | with a stub agent. No real agent has run: CI has no keys, so `e2e-agent-from-env.sh` reports SKIPPED |
| | harnesses | partial | `boat prompt` drives Claude Code, Codex, OpenCode, pi, prime, kimi and mistral (vibe); the last four were written from their `--help` and never run, see [Agents](#agents) |
| | async prompts, steer, conversations | yes | with a stub agent, through `sdk/e2e-live.sh` |
| | interrupt | yes | with a stub agent, through `sdk/e2e-live.sh` |
| | AI credentials, keep-keys-out proxy, usage | yes | proxy token in the VM, real key not, through `sdk/e2e-live.sh`; one credential per provider and team, no profiles or backup accounts |
| | block a VM's AI access | unit | |
| | computer use, MCP server, skills, llms.txt | no | not integrated in boat (PandaOS has `panda-cu` separately) |
| API and SDKs | REST + OpenAPI, Python, TypeScript | yes | own schema, see above |
| | CLI with `--json` | partial | the common verbs; no `current`, `extend`, `ssh-config` or `api-key` presets |
| | boat.dev's `/api/v1` sandbox API | partial | the endpoints in [boat.dev API compatibility](#boatdev-api-compatibility-apiv1), envelope, cursor pagination, `Idempotency-Key`; tested against a fake boatd only |
| Keys | create, list, revoke, scopes | yes | |
| | rotate | yes | through `sdk/e2e-live.sh` |
| | per-key usage | unit | |
| Webhooks | signed, delivery log, test | yes | |
| | rotate | yes | through `sdk/e2e-live.sh` |
| | update | unit | |
| Monitoring | OpenTelemetry | unit | `boat telemetry`; exporting to a real collector is untested |
| Teams | teams, members, quotas | yes | no invites, seats or shared wallets |
| Dashboard | web UI | partial | new; sign-in and VM list covered by a browser smoke test; less than boat.dev's |
| Security | isolation, tokenized URLs | yes | see Security model |
| | data retention, zero data retention, GDPR erasure tooling | no | you are the data controller |
| | SOC 2, status page, regions | n/a | your host |
| Billing | metering | yes | `/v1/usage`; no prices |
| | plans, wallets, invoices, trial | no | |
| Integrations | Harbor, Eve adapters, Windows apps recipes | no | |
| Guests | KVM inside a guest | no | off by design: a deliberate deviation, see [Honest gaps](#honest-gaps) |
| Platforms | CLI on Windows | partial | `boat_windows_{amd64,arm64}.exe` are cross-compiled in `release.yml`; never run on Windows |
| Hosted service | reliability, support, capacity | n/a | you run it: one host, no HA |

### What the tests prove

Workflow `boat-e2e`, on a GitHub KVM runner with real Firecracker (jailer on) and the real guest image:

1. `components/boat/e2e.sh` + `boatd/e2e-host.sh`: every CLI feature above marked yes, plus host phases (jail, network isolation, IPv6 with a ULA prefix, idle stop, boatd restart re-adopting VMs, free-space floor, Docker, qgroups).
2. `guest/test-in-vm.sh`: guest checks.
3. `gateway/e2e-edge.sh`: real Caddy with its internal CA and the jump sshd on 2222 in front of a fresh stack, over verified TLS: private and public routes, per-VM desktop origins, **VM A's Origin refused on VM D's desktop including the websocket upgrade**, port token revocation, the desktop websocket's first frame, `ssh -J`, a **boatd stop (snapshots VMs) followed by a different boatd binary (resumes them): same `boot_id`, guest server and route still up**, the dashboard page and CSP through Caddy, `dashboard/e2e.sh` (its smoke test in real Chrome, served by its own test server with the same CSP against this API, not through Caddy), `sdk/e2e-live.sh`, `api/e2e-auth.sh`.
4. `build iso-server`.

NixOS VM tests, with stubs for boatd and boat-api unless noted (`boat-install` and `boat-ops` are jobs in `boat-e2e`; run one with `nix build --impure -f tests/standalone.nix boat-ops`; `boat-module` is in `nix flake check`):

- `boat-install`: boots the `iso-server` modules under UEFI, runs `panda-install` with every server flag on two blank disks, boots the disk: `local.nix`, `disk.nix`, the data mount, sealed keys, `panda-health`, and **real** boat-api, gateway, Caddy and `boat-admin` over TLS.
- `boat-real` (job in `boat-e2e`): **real boatd, jailer and Firecracker under the real NixOS module**: the process runs non-root, in its own mount namespace, in `boat-vms`, with `oom_score_adj` 500; a `switch` to a generation with a new boatd and boat-api leaves boatd and Firecracker running and moves boat-api, which still talks to the old boatd. It does **not** check the guest: three virtualisation levels down, the guest agent never answered, even in 25 minutes.
- `boat-ops`: `switch` between two prebuilt generations with a stub VM running: boatd untouched, boat-api moved, health passes, `boat_reboot_pending` turns on, a boatd restart snapshots and resumes; real restic backup, restore to a scratch dir, master key recovery.
- `boat-module`: units, sealed keys, gateway uid, data-disk guard, Caddy + gateway over TLS, Origin refusals, dashboard, jump host.

**Not tested by anything:** a running guest through a NixOS `switch`, boatd restart or reboot on NixOS (guest survival is only shown by `e2e-edge.sh` on Ubuntu, process survival only by `boat-real`, and no CI here boots a guest under NixOS, so try it on your hardware before you rely on it), `local.nix` taking effect after the first rebuild, real ACME/DNS-01, a real TPM, public IPv4, maintenance reboots, restoring a backup into a running service, and two tenants' real browsers.

Desktop frame rate at 1080p through the whole edge (TLS, gateway), measured in real Chrome on CI runners with software rendering, encoding and decoding: 34-40 fps shown and 47-55 fps decoded, across runs. 60 fps is not reached, and there is no WebRTC path. Details in `components/boat/guest/README.md`. Expect a different number on your hardware and over an internet link.

## boat.dev API compatibility (`/api/v1`)

boat-api serves boat.dev's public API shapes under `/api/v1`, read from docs.boat.dev (`api/v1.md` and its reference pages) on 2026-11-09, on top of the native handlers: same keys, scopes, team isolation, secrets, AI and VM tokens. Point a boat.dev client's base URL at `https://<api host>/api/v1` and use a key from this server. Code: `components/boat/api/compat*.go`; tests: `compat_test.go` (fake boatd; no boat.dev SDK has been run against it).

**Envelope.** Success is `{"ok":true,"type":"…", …}`. Every error under `/api/v1` (auth, rate limits, validation, boatd) is `{"ok":false,"type":"sandbox.error","status","code","message","error":{"code","message","status","details?"},"requestId"}`, plus `retryAfterSeconds` on 503. Native codes are renamed where boat.dev has its own: `quota_exceeded` is `limit_reached`, `host_capacity` is `out_of_capacity`, upstream failures are `http_500`.

| boat.dev endpoint | Mapped to | Differences |
|---|---|---|
| `POST /sandboxes` | `POST /v1/vms` | 202 `sandbox.created`, `status` always `ready` (this server answers when the VM is up). `type`, `ttlSeconds` (absent = 3600, `null`/`"infinite"` = none), `labels`, `env`, `environment` (`base` = the team default), `noEnv`, `failFast`, `count` (1-16, not 1000, and no `from` needed), `from` (saved snapshot name or id, or `bsh_` code). `snapshots`, `monitoring`, `visibility`, `org`, `teamId` are ignored; `setupScript`, `hostedPorts`, `files` are refused with 400. No `secrets` field (boat.dev has none): all team secrets, or the environment's selection |
| `GET /sandboxes` | `GET /v1/vms` | `sandbox.list` with `pageInfo {nextCursor, hasMore, limit}`; `limit` 1-200, `cursor`, `sort`, `state`, `name`, `namePrefix`, `label=k=v`; `org`, `include` ignored; anything else is 400 `invalid_query` (`stoppedBefore`, `dataExpired` included). The cursor is opaque and stable under deletes |
| `GET /sandboxes/{id}` | `GET /v1/vms/{id}` | `sandbox.info`. States map: creating/starting/forking → `provisioning`, running → `running`, stopping → `archiving`, stopped → `archived`, error → `error`. Ids are this server's (`vm-…`), not `bx_…` |
| `DELETE /sandboxes/{id}` | `DELETE /v1/vms/{id}` | `X-Ascii-Confirm-Delete: <id>` required (409 `confirmation_required` otherwise). 202 `sandbox.deleting` with an `operation` (`bdop_…`) that is already `completed` |
| `GET /deletion-operations/{id}` | in memory | the operations of this process (lost on restart, at most 10000) |
| `POST /sandboxes/{id}/stop` | `POST /v1/vms/{id}/stop` | `force` works. Synchronous: 200 `sandbox.stopped`, `status: archived`, a `completed` operation, never 202 |
| `POST /sandboxes/{id}/resume` | `POST /v1/vms/{id}/start` | `failFast` works; `ttlSeconds` accepted and ignored (the VM keeps its create-time TTL, counted from each start); `type`, `env`, `environment`, `noEnv` refused with 400 (use `/v1/vms/{id}/resize`). 202 `sandbox.resuming` once running |
| `POST /sandboxes/{id}/fork` | `POST /v1/vms/{id}/fork` | `name`, `failFast`, `Idempotency-Key`; `ttlSeconds`, `monitoring` ignored; `labels`, `env`, `environment`, `noEnv`, `type` refused with 400. 202 `sandbox.forking` once running |
| `POST /sandboxes/{id}/commands` | `POST /v1/vms/{id}/exec` | `command` runs as `bash -lc`. `cwd` relative to the guest home (`/home/user` maps to it), `timeoutSeconds` 1-600 (default 30). Sync answer `command.finished` with stdout/stderr capped at 1 MiB each (`*Truncated`); `oomKilled` always false. `stream:true` sends NDJSON `{type: started|stdout|stderr|exit|error}`: the frame field name `type` is a guess, docs.boat.dev does not name it. `detached` refused with 400 (use `/v1/vms/{id}/commands`) |
| `PUT /sandboxes/{id}/files` | boatd file write | `path`, `content`, `encoding` (utf8, base64), ≤ 5 MiB (413 `file_too_large`). Paths are not limited to the home and `/tmp` as on boat.dev |
| `GET /sandboxes/{id}/files` | boatd file read | `path`, `encoding`; `file.read` with `content`, ≤ 5 MiB |
| `GET /limits` | VM list | `activeSandboxes`, `canStart: true`, `billingStatus: self_hosted`, `maxActiveSandboxes: -1`: team quotas live in boatd's config, which boat-api cannot read; going over one is 429 `limit_reached` |
| `Idempotency-Key` | in memory | on create and fork, per team, 24 h, ≤ 255 chars; same body replays (header `Idempotent-Replayed: true`), other body 409 `idempotency_key_reused`, running 409 `idempotency_in_progress`, a failed request frees the key. Lost on restart |

**Not mapped** (use `/v1`, or not available): prompts, events, conversations, `POST /sandboxes/{id}/host` and hosted ports, desktop and browser URLs, SSH endpoints, detached commands and their logs, `files/list`, `files/info`, upload and download URLs, saved snapshots (`/saved-snapshots`), share/unshare, `PATCH` sandbox, recover, stop operations (`/stops/{id}`), environments, secrets, AI credentials, keys, webhooks, orgs, wallets and billing, `RateLimit`/`RateLimit-Policy` headers, `X-Boat-Org`.

## Agents

`boat prompt --agent` and `POST /v1/vms/{id}/prompt(s)` run these CLIs headless in the VM (aliases: `claude-code` = `claude`, `prime-agent` = `prime`):

| Agent | Command | Session |
|---|---|---|
| claude | `claude -p --dangerously-skip-permissions --output-format stream-json --verbose --session-id U` / `--resume U` | boat-api picks it |
| codex | `codex exec [resume S] --json --full-auto --skip-git-repo-check` | read from output (`thread_id`) |
| opencode | `opencode run --format json [--session S]` | read from output (`sessionID`) |
| pi | `pi -p --mode json --session-id U [--model M] -- PROMPT` | boat-api picks it |
| prime | `prime-agent -p --mode json [--model M] PROMPT` | none: each turn starts fresh, steer is refused |
| kimi | `kimi -p PROMPT --yolo --output-format stream-json [-m M] [-S S]` | read from output (`session_id`/`sessionId`) |
| mistral | `vibe -p PROMPT --auto-approve --output streaming [--model ALIAS] [--resume S]` | read from output |

**pi, prime, kimi and mistral are untested.** Their flags were read from each CLI's `--help`; none of them has run here, in CI or by hand, and their output formats are unknown. boat-api passes their output through as is and looks for a session id with the same loose patterns as for codex and opencode; a turn whose output has none still runs, and the next turn in that conversation starts a new session. A `prime` prompt that starts with `-` gets a leading space, because its prompt is positional and no `--` is documented.

## Honest gaps

- **Unreviewed:** no external penetration test. See the status note at the top.
- **One host:** no HA, migration or cross-host scheduling.
- **No GPUs, and guest KVM is off by design** (Docker works). This deviates from boat.dev on purpose: guests get no `/dev/kvm`.
- **No guest is proven to survive a switch under NixOS.** Guest survival across a boatd restart is only shown on Ubuntu runners; see "Not tested by anything" above.
- **No real agent run:** CI has no API keys, so agents are only exercised with a stub.
- **Windows CLI:** cross-compiled only, never run on Windows. `install.sh` and `boat upgrade` were not tested there.
- **First release not tagged:** `release.yml` has a `workflow_dispatch` dry run that builds every asset without publishing.
- **Only partly API-compatible with boat.dev:** `/api/v1` maps the sandbox basics (list below), tested against a fake boatd only; much of the surface above is **unit** or **no**.
- **SSH UX** is `-J jump@… dev@<ip>` (wrapped by `boat ssh`).
- **Egress controls are basic:** SMTP is blocked by default and egress can be capped per VM (`egressMbit`), but there are no IOPS limits, no per-team egress budget and no destination filtering.
- **The sealed keys** are bound to this host's TPM/host key. Keep the offline copies: without the master key a hardware change loses the whole API state (teams, keys, secrets, environments), and desktop and route links stop working.
- **`vmDomain` on the Public Suffix List** is an owner step, see checklist item 2. Until then a tenant page's script can set `Domain` cookies for sibling tenants.
- **The AI proxy** runs inside boat-api: per-VM limits bound it, but it has no process of its own.
- **The VM's own key** (`BOAT_API_KEY` in the guest, with `-vm-api-url`) reaches only that VM's routes, but anyone with a shell in the VM has it, and processes that read it before a rotation keep a dead one. See `components/boat/api/README.md`.
- **API latency:** create, fork, stop and start no longer wait for boat-api's `state.json` fsync, which cost 1.9 s of a 2.73 s new-from-snapshot in boatbench 41652606509, on a disk busy with a snapshot's writeback. `api/latency_test.go` holds each under 100 ms of API time with a 300 ms fsync. A fresh boatbench run has not measured the result yet.
- **Guest images** are not pinned by default: `services.boat.images` is empty until you set the URLs and hashes of a release (`boat-guest-*`, `boat_guest_SHA256SUMS`). No tagged release has published them yet; until one does, build them with `components/boat/guest/build.sh` and copy them to `/var/lib/boat/images/<id>/`.
- **Metrics:** host metrics and `boat_reboot_pending` only; per-VM numbers come from `/v1/usage`.

## Integration notes

- **Flake.** `services.boat` is in `nixosModules.default`, with `nixosModules.server`, `nixosConfigurations.server`, the `boat*` packages and `packages.iso-server` (headless live system that installs `headless` or `server`, built by `boat-e2e` and on release).
- **Health.** `panda-health` watches `boat-api`, and `boat-gateway` + `caddy` when the gateway is on, so a bad update rolls back. It leaves out `boatd`, which fails on purpose without KVM.
