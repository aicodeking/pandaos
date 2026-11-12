# boat: self-hosted agent VMs on a dedicated server

> **Status: pre-release.** The first release has not been tagged, so no release has shipped it. Do not offer it to untrusted tenants until
> you have done the [operator checklist](#operator-checklist) **and** had an external penetration test.
> It has had internal review rounds only, and those kept finding serious bugs: the latest was a cross-tenant
> desktop takeover through a same-site websocket. It is single-host. Its `/api/v1` layer serves boat.dev's
> official SDKs for the sandbox lifecycle, commands, files, prompts, snapshots and webhooks, but not the
> whole boat.dev API (see [boat.dev API compatibility](#boatdev-api-compatibility-apiv1)).

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
| module | `components/boat/module.nix`, `ops.nix` | NixOS options and units: the above plus reboot detection, the maintenance window (boatd restart or reboot), backups. |

## Operator checklist

Before you hand out API keys:

1. **Hardware with KVM.** `ls /dev/kvm` in the rescue system. Without it, boatd refuses to start (`boatd: /dev/kvm is missing ...`, exit 78, no restart loop).
2. **Separate domains for tenant content.** Pick a base domain (`boat.example.com`) and a **different registrable domain** for what tenants control: `vmDomain` (port routes) and `desktopDomain` (desktop pages), e.g. `boatvms.example.net` and `desktop.boatvms.example.net`, or names under a suffix on the [Public Suffix List](https://publicsuffix.org/). With the defaults (`vm.<domain>`, `desktop.<domain>`) a tenant's page is same-site with `api.<domain>` and can set cookies on the parent domain. A rebuild warns about this until you fix it. Tenants' origins are always same-site with each other unless the VM domain is on the PSL. The gateway's Origin checks cover requests, and it drops `Domain` from every `Set-Cookie` a tenant app sends, but it cannot see a page's own `document.cookie`, so a tenant's script can still set a cookie on `vmDomain` that reaches sibling tenants. **Only a `vmDomain` on the PSL stops that; adding it is your job, no code here can.**
3. **DNS** (see below), including both wildcards.
4. **TLS with DNS-01.** Set `gateway.dnsProvider`. Without it, a tenant creating routes can exhaust your Let's Encrypt limit, after which new certificates fail for everyone; the module refuses to evaluate unless you set `gateway.allowOnDemandTLS = true` (the installer does, since it has no DNS credentials, and every rebuild warns about it).
5. **Backups.** Set `services.boat.backup` to a restic repository you control, keep the repository password off the box, and keep an offline copy of both sealed keys (see "Secrets"). A backup alone does not restore secrets on a new host. Test a restore.
6. **Maintenance window.** Turn on `services.boat.maintenance.reboot` (default: Sunday 04:00, only when something is pending): with `action = "auto"` (the default) it restarts only boatd when nothing but boatd or Firecracker changed and reboots for anything else, such as a kernel; `action = "reboot"` always reboots. Or watch `boat_reboot_pending` and act by hand.
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
   `--acme-email` is optional: without it Caddy registers its ACME account with no e-mail.
   No ISO or console, only ssh into the provider's OS (Hetzner's default Ubuntu, a rescue system)?
   Install over ssh with kexec and `--build`; see [install.md](install.md#hosted-server-over-ssh-kexec).
   On hosts without DHCP (static addresses), use `--build` so the network settings apply from the first boot.
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
5. **First admin key:** run `sudo boat-admin init -user me -team ops`. boat-admin runs `boat-api admin init` as `boat` with the sealed master key, prints the new owner key record (JSON) once, writes the bare `boat_...` token to `/root/boat-owner-token` (mode 0600; `--token-file PATH` for elsewhere), then restarts boat-api. Then run `boat login --url https://api.boat.example.com --key "$(sudo cat /root/boat-owner-token)"`; `boat login` also accepts the whole JSON record. Delete the file once the token is stored where you keep secrets.

### Guest images before a release

No tagged release has published `boat-guest-*` yet, so the release URLs in step 4 do not exist until one does. Three ways to get images now; each ends with `/var/lib/boat/images/ubuntu-24.04/{vmlinux,rootfs.ext4}` (the image id is the directory name; boat-api offers `ubuntu-24.04` when `services.boat.images` is empty):

- **CI artifact (no build):** run the release workflow by hand, which is a dry run that builds every asset without publishing: `gh workflow run release.yml -R AICodeKing/pandaos`, then `gh run download <run-id> -R AICodeKing/pandaos -n boat-assets` (kept 7 days). It holds `boat-guest-vmlinux`, `boat-guest-rootfs-{slim,full}.ext4.zst` and `boat_guest_SHA256SUMS`. Check them with `sha256sum -c boat_guest_SHA256SUMS`.
- **Build:** on any x86_64 host with Docker, `components/boat/guest/build.sh all` writes `out/vmlinux` and `out/rootfs-{slim,full}.ext4.zst`.
- **Then install:** `sudo boat-admin images import boat-guest-vmlinux boat-guest-rootfs-full.ext4.zst` (a `.zst` is decompressed; a plain `.ext4` is copied with a reflink when it can be). It checks the rootfs is a clean ext4 image before replacing anything, installs both files 0644 as `ubuntu-24.04` (the default id while nothing is declared) and needs no restart: VMs created after it use the new files, running ones keep theirs. `--id NAME` installs another image; declare it with `services.boat.localImages = [ "NAME" ];` so boat-api offers it (boat-admin prints that line). `sudo boat-admin images list` shows what is installed and whether it is declared. `boat-images.service` logs the import command while the default image is missing. Or put the files on any HTTPS server you control and set `services.boat.images` to those URLs with the hashes from `boat_guest_SHA256SUMS`. `boat-images.service` fetches only `https://` URLs and checks every hash.

Use `full` for the desktop; on `slim`, `POST …/desktop` answers 422 `not_supported`.

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

The default sizes are `small` 2 vCPU/4 GiB, `default` 4/8, `large` 8/16 and `xlarge` 16/32, with sparse disks. The default team quota follows `services.boat.untrustedTenants`, which the server profile turns on by default: 10 VMs / 32 vCPU / 64 GiB per team; with `untrustedTenants = false` (a host whose tenants are all trusted) it is 0, no per-team cap, and host admission alone bounds a team.

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
- **Dashboard.** Static files from `components/boat/dashboard/web`, served from the read-only Nix store at `https://api.D/`, on the same origin as the API, with a strict CSP (`default-src 'self'`, no framing). `/v1/*`, `/api/v1/*` (boat.dev-compatible, for its official SDKs) and `/healthz` go to boat-api; `/internal/*` is 404. It holds the user's API key in the browser, which is why it is never on a tenant-controlled origin.
- **Desktop links.** `GET /v1/vms/<id>/desktop` returns `https://desktop.D/<id>?exp&sig` (sig = `hex(HMAC-SHA256(Kdesktop, "<id>.<exp>"))`, 5 min by default). The gateway 302s it to `https://<id>.desktop.D/?exp&sig`, which checks the signature, sets `__Host-boat_gw` and drops the signature from the URL. Toward the guest, the gateway removes the client's `boat_*`/`__Host-boat_*` cookies and sends `boat_desktop=hex(HMAC(Kguest, "guest-desktop.<id>"))`. boat-api starts the guest desktop with that same token.
- **Port routes are private by default.** A route needs a token: `token = <exp>.hex(HMAC(Kroute, "route.<port>-<id>.<exp>"))`, minted by `GET /v1/vms/<id>/ports/<port>/url` (`boat ports url VM PORT`). Programs send it as `X-Boat-Route-Token`; browsers open `…/?boat_exp&boat_sig` once, which sets the host-only `__Host-boat_route` (SameSite=Lax). A port is reachable without a token only if added with `--public`, which boatd lists in routes.json `"public"`. When the list is missing, routes stay private (fail-closed). Tokens and cookies never reach the VM.
- **One HMAC key per purpose.** The shared key file (`-desktop-key-file` / `-desktop-key`) is a master key that signs nothing itself: `Kdesktop`, `Kroute` and `Kguest` are `HMAC-SHA256(master, "boat-hmac-v2/desktop-link" | "…/port-route" | "…/guest-desktop")`, derived the same way by boat-api (`PurposeKey`) and boat-gateway, so a token of one kind never verifies as another, whatever its payload. **Migration:** before this, all three were signed with the master key, told apart by payload prefixes. After the upgrade boat-api mints only per-purpose tokens; the gateway still accepts a master-key token that expires within its legacy window (`-legacy-hmac-until`: unix time; default its start + 8 days, longer than any token boat-api mints; `-1` turns it off), and during that window also sends the guest desktop both cookies, so a desktop started before the upgrade keeps working until its next `GET …/desktop` restarts it with the new token. After the window, master-key tokens are refused. Nothing needs to be done by hand. CI: `TestPurposeKeysAndLegacyWindow` (gateway), `TestFilesSSHDesktop` (boat-api), and `gateway/e2e-edge.sh` (a desktop-link-key token is refused as a route token, a pre-split token still works inside the window).
- **Revoking port tokens.** A token lives up to 7 days. `POST /v1/vms/<id>/port-tokens/revoke` ends every token, link and cookie of that VM's private ports, and so does removing a port. Deleting an API key, or removing a member (whose keys go with them), does the same for every VM that key minted a private link of, and no link outlives the expiry of the key that minted it; a token minted afterwards signs `route.<port>-<id>.g<gen>.<exp>`. boat-gateway polls boat-api's loopback-only `/internal/route-gens` every 2 s, so it takes effect within about 2 s, and a token minted in that window can also get 401 until the next poll. If boat-api goes down the gateway keeps its last table. If the gateway starts while boat-api is down, private routes answer 503 until boat-api answers, because a revoked token cannot be told from a good one; public routes are not affected.
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
- **In-flight limits.** Per team, at most 64 open exec, SSE, log and download streams (`-team-streams`), 8 file uploads and imports (`-team-uploads`), and 128 requests of any kind (`-team-inflight`); the `/api/v1` file routes hold the same slots as their `/v1` twins. Signed-link downloads count separately from the team's. Per VM, at most 16 AI proxy requests (`-ai-vm-inflight`). Past a limit the API answers 429.
- **Request bodies.** A body sent to a VM route (`/v1/vms/{id}/…`, `/api/v1/sandboxes/{id}/…`) is read only after the VM's ownership check, so another team's VM costs boat-api no memory. On every route that is not a stream or an upload the body must arrive within `-body-timeout` (30 s). JSON bodies are capped at 1 MiB, boat.dev file writes at about 7 MiB (5 MiB base64-encoded).
- **Webhook targets.** Deliveries dial only public addresses, checked after DNS on the address actually dialed: never private or special-purpose networks, never this host's own addresses (its public ones included, which reach services bound to loopback through a NAT rule), never a VM's `ip`, `ipv6` or `public_ip` as boatd reports them (refreshed every 10 s; a webhook fails closed while boatd cannot be read), and never the `-webhook-deny` CIDRs (repeatable: set the VM pool's public prefix there). Uploads go to an unlinked temp file on the data disk, not memory. The AI proxy still shares boat-api's process, so a flood of allowed requests costs the API's memory too.
- **State caps.** Per team: 200 secrets and 2 MiB of sealed secret values, 100 live shares, 4 MiB of environment scripts and vars, 200 ssh keys. The command history keeps 8 KiB of each argv.
- **Secrets.** `state.json` is sealed as a whole under the master key, because it holds environment vars, setup scripts, command lines and prompts. A plaintext file from an older version is sealed when boat-api opens it, and an older boat-api cannot read the sealed file, so do not roll back past this change. Tenant secrets and webhook keys are also sealed individually with AES-256-GCM under boat-api's master key. That key and the desktop/route HMAC key exist on disk only as systemd credentials (`/var/lib/boat/secrets/*.cred`, directory 0700 root), encrypted with the TPM2 when present plus the host key `/var/lib/systemd/credential.secret`, and handed to the services with `LoadCredentialEncrypted`. Plaintext keys from older installs are sealed, verified and deleted on upgrade. Backups contain only the sealed files, so **a backup alone cannot decrypt anything on another host. Keep offline copies:** `sudo systemd-creds decrypt --name=boat-master-key /var/lib/boat/secrets/boat-master-key.cred - | base64` (same for `boat-desktop-key`). API keys are stored as salted HMACs. Caddy's DNS token belongs in `gateway.environmentFile`.
- **Host hardening** comes from PandaOS: nftables, key-only ssh, sshguard, sysctl hardening, daily updates with health-check rollback.
- **Not defended:** a malicious server admin; side channels between VMs on shared cores (no core pinning); one team saturating the disk (no IOPS limits) or, without `egressMbit`, the uplink; a full compromise of the host through a KVM or Firecracker bug (no second layer beyond the jailer); and abuse from your IPs beyond what your policy and quotas stop.

### Tenant model

- **Teams** own VMs, secrets, webhooks and keys. A key belongs to one user in one team and has scopes (`vms:read`, `vms:write`, `exec`, `admin`). Admins manage members and secrets.
- **Keys.** A key can list, rotate, delete or read usage of a key only if that key's scopes are within its own. Another user's key also needs the `admin` scope and a role no higher than the caller's, and only the operator can touch the operator's keys. Anything else is 404.
- **Deleting a team** also deletes its snapshots, imported ones included.
- A team sees and reaches only its own VMs: API objects are filtered by team, nftables blocks other teams' VMs, jump grants list only the team's VM IPs, and every desktop link and route token is bound to one VM.
- **Secrets reach VMs only when chosen.** See [Secrets](#secrets).
- **Quotas** (`services.boat.quotas`, per-team overrides with `boatdSettings.teams`) cap running and total VMs, vCPU, memory, disk, snapshots and routes, all finite by default under `untrustedTenants` (the server profile's default); with `untrustedTenants = false` the VM, vCPU and memory caps default to 0 (host admission only). Metering (`/v1/usage`) records VM-seconds per size and snapshot-days per team. There is no billing.

### Secrets

Writing a team secret needs `admin`, and a VM's env is readable by anyone with `exec` on it, so a VM gets a secret only if:

- the request that creates it names the secret (`"secrets": [...]`, `boat new --secrets A,B`), and either the key is `admin` or the secret is default-injected (a member key naming any other secret is refused with 403), or
- its environment selects it (environments are written by admins), or
- the request names nothing and an admin marked the secret **default-injected**: `PUT /v1/secrets/{name}` with `"inject": true`, `boat secrets set NAME VALUE` (the CLI marks it, as boat.dev's CLI secrets go to every sandbox; `--no-inject` does not), or boat.dev's `POST /api/v1/secrets`.

A new secret set through the API is not default-injected; a rotation keeps the mark. `GET /v1/secrets` shows `inject`.

**Migration.** Before this, a VM created without a selection got every team secret at each start. VMs created from now on record the marker `@default` (in `state.json` and the `boat.secrets` label, which forks copy) and get only the default-injected secrets, read at each start. VMs created before the upgrade carry no selection record and keep their old behaviour (every team secret) until deleted; to stop that, recreate or fork them with an explicit selection, or delete the secrets they must not see. Environments without a selection now give their VMs and builds the default-injected secrets, not all. To keep today's behaviour for a secret, mark it: `boat secrets set NAME VALUE` (or `PUT /v1/secrets/NAME {"value":…, "inject":true}`). Tests: `TestSecretsAPIAndInjection`, `TestPerVMSecretSelection`.

### AI credentials proxy: forwarded endpoints

The proxy forwards only inference endpoints; anything else is 403 before the team's key is read, so a VM token cannot reach a provider's files, batches, fine-tuning, assistants, admin or organization APIs. Paths are relative to the provider's upstream, `*` is one path segment; `.`/`..`/empty segments and encoded `/` or `.` are refused. Source: `components/boat/api/aiallow.go`; test: `TestAIProxyAllowlist`.

| Provider | Allowed |
|---|---|
| anthropic | `POST v1/messages`, `POST v1/messages/count_tokens`, `POST v1/complete`, `GET v1/models`, `GET v1/models/*` |
| openai | `POST v1/chat/completions`, `POST v1/completions`, `POST v1/embeddings`, `POST v1/responses`, `GET`/`DELETE v1/responses/*`, `POST v1/responses/*/cancel`, `GET v1/responses/*/input_items`, `POST v1/responses/input_tokens`, `POST v1/responses/compact`, `GET v1/models`, `GET v1/models/*` |
| openrouter | `POST api/v1/chat/completions`, `POST api/v1/completions`, `POST api/v1/responses`, `POST api/v1/messages`, `POST api/v1/embeddings`, `GET api/v1/models`, `GET api/v1/models/*/endpoints`, `GET api/v1/generation` |
| deepseek | `POST chat/completions`, `POST v1/chat/completions`, `POST beta/completions`, `POST completions`, `GET models`, `GET v1/models`, `POST anthropic/v1/messages`, `POST anthropic/v1/messages/count_tokens` |
| mistral | `POST v1/chat/completions`, `POST v1/fim/completions`, `POST v1/agents/completions`, `POST v1/embeddings`, `GET v1/models`, `GET v1/models/*` |
| moonshot | `POST v1/chat/completions`, `POST v1/embeddings`, `GET v1/models`, `GET v1/models/*`, `POST v1/tokenizers/estimate-token-count`, `POST anthropic/v1/messages`, `POST anthropic/v1/messages/count_tokens` |
| xai | `POST v1/chat/completions`, `POST v1/completions`, `POST v1/responses`, `GET v1/responses/*`, `POST v1/messages`, `POST v1/embeddings`, `GET v1/models`, `GET v1/models/*`, `GET v1/language-models`, `GET v1/language-models/*` |

These cover what the providers' SDKs and the harnesses `boat prompt` drives (Claude Code, Codex, OpenCode, kimi, vibe) call to run a model. A harness that needs another endpoint gets a 403 naming this list.

## Upgrades and reboots

- **Updates.** The daily unattended update (`updates.nix`) switches to the new system with a health check and rolls back on failure. boatd is never restarted by a switch (`restartIfChanged = false`), so running VMs are untouched. boat-api, boat-gateway and Caddy restart in seconds; the API's clients retry.
- **Pending reboot.** `boat-reboot-check` (hourly) compares the booted kernel, initrd and modules, and boatd plus its config and Firecracker paths, with the installed ones. It logs `boat: reboot pending (changed: ...)`, writes `/run/boat/reboot-pending` and exports `boat_reboot_pending` (0/1) with a `reason` label.
- **Maintenance window.** With `maintenance.reboot = true`, a timer (`onCalendar`, default Sunday 04:00, ±10 min) applies what `boat-reboot-check` reports pending (`onlyWhenPending = false` acts every time). With `maintenance.action = "auto"` (default) a pending change of boatd or Firecracker only restarts `boatd.service`, and the host stays up; any other pending change (a new kernel) reboots. `action = "reboot"` always reboots. Both drain on their own: stopping boatd pauses and memory-snapshots every running VM (`stopTimeoutSec`), and the new boatd resumes them, with processes, tmux sessions and open files intact. TCP connections into VMs drop. `boat_maintenance_window_enabled` and `boat_maintenance_last_run_timestamp_seconds{action}` are exported for node-exporter.
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

**Drop-in for the boat.dev SDK paths CI proves, not for its whole API.** The native API is `/v1/vms` and this project has its own SDKs. A compatibility layer under `/api/v1` maps 100 of boat.dev's 122 operations (the other 22 answer 501 `not_supported`) with boat.dev's ids, envelope, cursor pagination and `Idempotency-Key` (see [boat.dev API compatibility](#boatdev-api-compatibility-apiv1)). boat.dev's official Python SDK (`boat-sdk` on PyPI; this project's own is `pandaos-boat`) and TypeScript SDK (`@boatdev/sdk`) run, with only the base URL changed, through Caddy and TLS on real VMs in `boat-e2e`, over the calls listed under [What proves it](#boatdev-api-compatibility-apiv1). Mapped operations those drivers do not call are tested against a fake boatd only; code that uses the operations listed as not supported must be ported to `/v1`.

| boat.dev area | Feature | Here | Notes |
|---|---|---|---|
| Sandboxes | create, list, get, delete | yes | `boat new/list/rm` |
| | update (PATCH): rename, auto-stop | yes | the official SDKs' `update` and `PATCH /v1/vms/{id}` in `sdk/e2e-live.sh` |
| | recover, share | unit | `boat recover`, `boat share`; `recover` power-cycles a running VM (fake boatd only); `share` is a no-op, every sandbox is usable by its team (`share` also runs through the official SDKs in `boat-e2e`) |
| | unshare | n/a | there are no private sandboxes: `POST …/unshare` and `boat unshare` answer 501 `not_supported` |
| | stop, resume | yes | `boat stop`, `boat start` or `resume`; memory restored (same `boot_id`) |
| | force stop | unit | `boat stop --force`: no snapshot, the next start cold-boots from disk |
| | fork | yes | new hostname, IP, machine-id, host keys, entropy |
| | per-sandbox usage | unit | `boat vm-usage` |
| | `current` shortcut, `boat onboard`, sign up from an agent | no | keys come from the operator |
| | `boat self` / `host` inside the VM | unit | `BOAT_ID`, `BOAT_URL` and a key for that VM only, with `-vm-api-url` |
| Commands | exec with output | yes | |
| | detached commands, follow, signal, logs, history (also on a stopped VM) | yes | `boat cmd`; driven through the SDKs in `sdk/e2e-live.sh` |
| | boat.dev's attach stream (`commands/{id}/stream`, `stdoutOffset`/`nextOffset`) | yes | the official SDKs' `run_command`/`follow_command` (`runCommand`/`followCommand`) in `boat-e2e`; one combined log, served as stdout |
| | `ps` | yes | `boat ps`, through `sdk/e2e-live.sh` |
| Files | read, write | yes | `boat scp` |
| | list, search, move, delete, folder, info | yes | `boat files`, through `sdk/e2e-live.sh` |
| | permissions | unit | |
| | find in files, download URLs | yes | `/api/v1` only: `files/find` (ripgrep), `files/download-url` (signed, bound to team, VM, path and the minting key, ≤ 1 h); the official SDKs in `boat-e2e` |
| | replace, upload URLs, artifacts | yes | `/api/v1` only: `files/replace`, `files/upload-url` (signed, bound to team, VM, path and the minting key, ≤ 1 h), `artifacts`; the official SDKs in `boat-e2e` |
| Connect | ssh | yes | through the jump host; IP target; `boat ssh-config` writes the `ProxyJump` entry (in `sdk/e2e-live.sh`); boat.dev's `sshkey` through the official SDKs |
| | VS Code, Cursor | partial | `boat ssh-config` gives them a host entry; the editors themselves never ran in CI |
| | desktop in the browser | partial | boat.dev's `desktop` / `wait_for_desktop` through the official SDKs (vnc mode, signed link, no `publicAccess`). KasmVNC over a websocket (`full` image), no WebRTC. 1080p through the TLS edge in CI, software rendering: 34-40 fps shown, 47-55 decoded; 60 not reached. See [frame rate](#what-the-tests-prove) |
| | browser-only view, record to MP4, custom wallpaper | no | |
| | embed desktop in an iframe | no | refused on purpose, see Origin checks |
| Public URLs | HTTPS port URL, token protection | yes | private by default; boat.dev's `host` through the official SDKs, `hostedPorts` on create tested against a fake boatd |
| | raw TCP | yes | `--public` host port |
| | raw UDP, rename subdomain, stable names across forks | no | |
| | firewall rules per sandbox | no | SMTP blocked, `egressMbit` cap, operator-wide only |
| Long-running | TTL and idle auto-stop | yes | |
| | extend the deadline | yes | `boat extend --hours/--by/--no-auto-stop`, PATCH `ttlSeconds`, in `sdk/e2e-live.sh` |
| | prompts pushing it out | unit | an `/api/v1` prompt gives the VM a fresh full TTL |
| | systemd services across stop/resume | yes | memory restore keeps all processes; boat.dev restarts services instead |
| Environments | image, setup script, env vars, secret selection, prebuilt snapshot | yes | `boat env`; setup and build run in `sdk/e2e-live.sh` |
| | versions | unit | a counter; no upgrade of running VMs |
| | repo cloning, secret files, private registries, safe-for-third-parties, per-org access | no | |
| Snapshots | create, list, delete, new from snapshot | yes | memory + disk |
| | share code, import | yes | through `sdk/e2e-live.sh` |
| | download | yes | through `sdk/e2e-live.sh` |
| | boat.dev listings (UUID ids), latest, delete captures, delete one, named snapshots, share codes | yes | `/api/v1`, through the official SDKs in `boat-e2e` |
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
| | AI credentials, keep-keys-out proxy, usage | yes | proxy token in the VM, real key not, through `sdk/e2e-live.sh`; only inference endpoints are forwarded ([list](#ai-credentials-proxy-forwarded-endpoints)); one credential per provider and team, no profiles or backup accounts |
| | block a VM's AI access | unit | |
| | computer use, MCP server, skills, llms.txt | no | not integrated in boat (PandaOS has `panda-cu` separately) |
| API and SDKs | REST + OpenAPI, Python, TypeScript | yes | own schema, see above |
| | CLI with `--json` | partial | the common verbs plus `current`, `extend`, `ssh-config`, `push`, `host`, `run` (all in `sdk/e2e-live.sh`); no `api-key` presets |
| | boat.dev's `/api/v1` API and official SDKs | partial | 100 of 122 operations mapped, the other 22 answer 501 `not_supported`; see [boat.dev API compatibility](#boatdev-api-compatibility-apiv1). The official Python and TypeScript SDKs (pinned 1.13.0) run on real VMs through Caddy in `boat-e2e` over the calls listed there; prompts, events, `hostedPorts`, folder/move/permissions and the webhook/key/environment calls of the TypeScript SDK are tested against a fake boatd only |
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
3. `gateway/e2e-edge.sh`: real Caddy with its internal CA and the jump sshd on 2222 in front of a fresh stack, over verified TLS: private and public routes, per-VM desktop origins, **VM A's Origin refused on VM D's desktop including the websocket upgrade**, port token revocation, the desktop websocket's first frame, `ssh -J`, a **boatd stop (snapshots VMs) followed by a different boatd binary (resumes them): same `boot_id`, guest server and route still up**, the dashboard page and CSP through Caddy, `dashboard/e2e.sh` (its smoke test in real Chrome, served by its own test server with the same CSP against this API, not through Caddy), `sdk/e2e-live.sh` (this project's SDKs and CLI, then **boat.dev's official SDKs, hash- and lock-pinned, on `/api/v1`**), `api/e2e-auth.sh`.
4. `build iso-server`.

NixOS VM tests, with stubs for boatd and boat-api unless noted (`boat-install` and `boat-ops` are jobs in `boat-e2e`; run one with `nix build --impure -f tests/standalone.nix boat-ops`; `boat-module` is in `nix flake check`):

- `boat-install`: boots the `iso-server` modules under UEFI, runs `panda-install` with every server flag on two blank disks, boots the disk: `local.nix`, `disk.nix`, the data mount, sealed keys, `panda-health`, and **real** boat-api, gateway, Caddy and `boat-admin` over TLS.
- `boat-real guest (server profile in nspawn)` (job in `boat-e2e`, `tests/boat-real.nix`): **the server profile's boatd as a systemd-nspawn container on the runner boots real guests** from the real image and drives exec, stop/resume, fork, snapshot, delete and a boatd restart with VMs running. Not a booted kernel of ours (the runner's Ubuntu kernel), no uplink for guests, and no `switch`.
- `boat-real` (job in `boat-e2e`): **real boatd, jailer and Firecracker under the real NixOS module**: the process runs non-root, in its own mount namespace, in `boat-vms`, with `oom_score_adj` 500; a `switch` to a generation with a new boatd and boat-api leaves boatd and Firecracker running and moves boat-api, which still talks to the old boatd. It does **not** check the guest: three virtualisation levels down, the guest agent never answered, even in 25 minutes.
- `boat-ops`: `switch` between two prebuilt generations with a stub VM running: boatd untouched, boat-api moved, health passes, `boat_reboot_pending` turns on, a boatd restart snapshots and resumes; real restic backup, restore to a scratch dir, master key recovery.
- `boat-module`: units, sealed keys, gateway uid, data-disk guard, Caddy + gateway over TLS, Origin refusals, dashboard, jump host.

**Not tested by anything:** a running guest through a NixOS `switch` or a reboot of a NixOS host (guests boot and survive a boatd restart under the NixOS server profile only inside nspawn on the runner's kernel, in `boat-real guest`; a `switch` is shown only without a guest, in `boat-real`; so try it on your hardware before you rely on it), `local.nix` taking effect after the first rebuild, real ACME/DNS-01, a real TPM, public IPv4, the maintenance window (boatd restart or reboot) on a real host, restoring a backup into a running service, and two tenants' real browsers.

Desktop frame rate at 1080p through the whole edge (TLS, gateway), measured in real Chrome on CI runners with software rendering, encoding and decoding: 34-40 fps shown and 47-55 fps decoded, across runs. 60 fps is not reached, and there is no WebRTC path. Details in `components/boat/guest/README.md`. Expect a different number on your hardware and over an internet link.

## boat.dev API compatibility (`/api/v1`)

boat-api serves boat.dev's public API under `/api/v1`, on top of the native handlers and in the same process: same keys, scopes, team isolation, rate limits, secrets, AI and VM tokens. Point a boat.dev client's base URL at `https://<api host>/api/v1` and use a key from this server:

```python
from boat_sdk import ApiClient, BoatApi, Configuration
api = BoatApi(ApiClient(Configuration(host="https://api.boat.example/api/v1", access_token=KEY)))
```

```ts
const api = new BoatApi(new Configuration({ basePath: "https://api.boat.example/api/v1", accessToken: KEY }));
```

**Through the gateway.** Caddy sends `https://api.<domain>/api/v1/*` to boat-api like `/v1/*` (`gateway/module.nix`); before, it fell through to the dashboard's `index.html`.

**What proves it.** `openapi.json` documents every `/api/v1` operation with boat.dev's own schemas, copied from [boat-v1.yaml](https://docs.boat.dev/openapi/boat-v1.yaml) (read 2026-11-09) by `components/boat/api/tools/compat_openapi.py`, so the OpenAPI drift test checks each answer the Go tests get against boat.dev's spec: required fields, enums, id patterns. `components/boat/sdk/compat/` holds drivers for boat.dev's official SDKs, `boat-sdk` 1.13.0 from PyPI (hash-pinned with its dependencies) and `@boatdev/sdk` 1.13.0 from npm (lockfile integrity); `sdk/e2e-live.sh` installs them and, in `boat-e2e`, runs them on real VMs **through Caddy over TLS** (`gateway/e2e-edge.sh` sets `BOAT_COMPAT_URL=https://api.boat.test`; a skipped step fails the run): create, get, list, rename (PATCH), sync commands, long commands through the SDKs' own `run_command`/`follow_command` (`runCommand`/`followCommand`) on the attach stream, command status, history and stop, files, file info, `files/find`, `files/download-url` (then a GET of the URL with no key), `host`, `sshkey`, `wait_for_desktop`/`waitForDesktop` on a `full` VM, stop, the stop operation, resume, fork, saved snapshots, share codes (create, list, revoke), named snapshots, per-sandbox, latest and team snapshot listings, delete one snapshot, delete a sandbox's snapshots, `/secrets` (set, read back without values, restore), the AI proxy switch, webhooks, API keys (Python), environments and one-by-one environment variables (Python), `files/replace`, `artifacts`, `files/upload-url` (then a PUT of the URL with no key), `share`, AI usage and per-sandbox AI access, telemetry set and delete when the team has none (Python), limits and delete. `recover` is tested against the fake boatd only (it power-cycles the VM). The same drivers run against the fake boatd in `go test` (`TestLiveScripts`, `TestOfficialSDKDrivers`). Code: `components/boat/api/compat*.go`.

**Ids.** boat.dev's SDKs refuse a sandbox id that is not `bx_` + 8 characters of `23456789abcdefghjkmnpqrstuvwxyz`. Each VM gets one the first time `/api/v1` shows it: drawn at random, checked against every id in use, kept in `state.json` for the VM's life and dropped when it is deleted. Another team's id is 404. A native `vm-…` id also works in `/api/v1` paths.

**Envelope.** Success is `{"ok":true,"type":"…", …}`. Every error under `/api/v1` (auth, rate limits, validation, boatd) is `{"ok":false,"type":"sandbox.error","status","code","message","error":{"code","message","status","details?"},"requestId"}`, plus `retryAfterSeconds` on 503. Native codes are renamed where boat.dev has its own: `quota_exceeded` is `limit_reached`, `host_capacity` is `out_of_capacity`, upstream failures are `http_500`.

**States.** creating/starting/forking → `provisioning`; running → `ready`, or `running` while one of its prompts runs (boat.dev: an agent is working); stopping → `archiving`; stopped → `archived`; error → `error`. `idle`, `cloning` and `cancelled` never occur.

| boat.dev operation | Mapped to | Differences |
|---|---|---|
| `POST /sandboxes` | `POST /v1/vms` | 202 `sandbox.created`, `status` always `ready` (this server answers when the VM is up). `type`, `ttlSeconds` (absent = 3600, `null`/`"infinite"` = none), `labels`, `env`, `environment` (`base` = the team default), `noEnv`, `failFast`, `count` (1-16), `from` (saved snapshot name or id, or `bsh_` code). `snapshots`, `monitoring`, `visibility`, `org`, `teamId` are ignored; `hostedPorts` hosts each port as `POST …/host` does and answers `hostedPorts` (an entry with `error` for a port that failed; 400 `invalid_hosted_ports`, `count_with_hosted_ports`, `fail_fast_with_hosted_ports` as on boat.dev). `setupScript`, `files` are refused with 400 |
| `GET /sandboxes` | `GET /v1/vms` | `pageInfo {nextCursor, hasMore, limit}`; `limit` 1-200, `cursor`, `sort`, `state`, `name`, `namePrefix`, `label=k=v`; `org`, `include` ignored; anything else 400 `invalid_query` (`stoppedBefore`, `dataExpired` included) |
| `GET /sandboxes/{id}` | `GET /v1/vms/{id}` | `archiveAfter` is boatd's auto-stop deadline, `stop` the last stop operation, `stoppedAt` the last state change of a stopped VM. No `url`, `desktopUrl`, `cost`, `subdomain`, `hydrated`, `health` |
| `PATCH /sandboxes/{id}` | `PATCH /v1/vms/{id}` | `name`, `ttlSeconds` (sets the deadline to now + ttl; `null` = none). `subdomain` refused. Needs boatd's `PATCH /v1/vms/{id}`: an older boatd gives 501 `not_supported` |
| `DELETE /sandboxes/{id}` | `DELETE /v1/vms/{id}` | `X-Ascii-Confirm-Delete: <id>` required (409 `confirmation_required`). 202 `sandbox.deleting` with a `bdop_…` operation already `completed` |
| `GET /deletion-operations/{id}` | boat-api's store | kept across restarts (at most 10000) |
| `POST /sandboxes/{id}/stop` | `POST /v1/vms/{id}/stop` | `force` works. 202 `sandbox.stopping`, but the stop has finished: `status: archived`, a `completed` operation, `statusUrl` |
| `GET /sandboxes/{id}/stops/{stopId}` | boat-api's store | `completed`, `failing` (the native stop failed) or `superseded` (resumed); kept across restarts |
| `POST /sandboxes/{id}/resume` | `POST /v1/vms/{id}/start` | `failFast`; `ttlSeconds` ignored (the VM keeps its TTL, counted from each start); `type`, `env`, `environment`, `noEnv` refused with 400 (use `/v1/vms/{id}/resize`) |
| `POST /sandboxes/{id}/fork` | `POST /v1/vms/{id}/fork` | `name`, `failFast`, `Idempotency-Key`; `ttlSeconds`, `monitoring` ignored; `labels`, `env`, `environment`, `noEnv`, `type` refused with 400 |
| `POST /sandboxes/{id}/commands` | `POST /v1/vms/{id}/exec`, or `POST /v1/vms/{id}/commands` with `detached` | `command` runs as `bash -lc`, `cwd` relative to the guest home (`/home/user` maps to it), `timeoutSeconds` 1-600 (default 30). Sync: `command.finished`, stdout/stderr capped at 1 MiB each (`*Truncated`), `oomKilled` always false. `stream:true`: NDJSON frames `{type, …}`, `started`, then `stdout`/`stderr` with `data`, then `exit` or `error` (the frame fields `type` and `data` are the ones docs.boat.dev's examples read; `nextOffset` is not sent). `detached`: `command.started` with an integer `processId` per VM |
| `GET /sandboxes/{id}/commands/{processId}/stream` | guest `cmd_logs`, polled | boat.dev's `CommandAttachFrame` NDJSON: `attached`, `stdout` with `data` and `nextOffset`, `heartbeat` after 15 s of silence, then `exit` (`status` exited or lost, `exitCode`, `signal`), `reconnect` after 10 min, or `error`; every last line carries `stdoutOffset`/`stderrOffset` to reopen at with no gap or repeat. One combined log, served as stdout: stderr has no data and its offset is echoed |
| `GET /sandboxes/{id}/commands/{processId}` | guest `cmd_status` + logs | output is one combined log, returned as `stdout` (`stderr` empty), up to 1 MiB; offsets and `tailBytes` ignored |
| `DELETE /sandboxes/{id}/commands/{processId}` | `DELETE /v1/vms/{id}/commands/{cid}` | SIGTERM |
| `GET /sandboxes/{id}/command-history` | `GET /v1/vms/{id}/command-history` | detached commands, prompts and setup scripts this server started (not sync `exec`), newest first, no paging; `stdoutBytes`/`stderrBytes` are 0 |
| `GET /sandboxes/{id}/command-history/{commandId}[/logs]` | the record; guest `cmd_logs` | logs: `offset`, `limit`, `encoding`; `stream=stderr` is always empty (one combined log) |
| `GET /sandboxes/{id}/processes` | `ps` in the VM | `pid`, `parentPid`, `user`, `command` |
| `GET`/`PUT`/`DELETE /sandboxes/{id}/files` | boatd file read/write, guest delete | ≤ 5 MiB (413 `file_too_large`); paths are not limited to the home and `/tmp` as on boat.dev |
| `GET /sandboxes/{id}/files/list`, `/info`, `/search`; `POST …/files/folder`, `/move`, `/permissions` | the native file routes | `search`: `pattern` matches as a substring of the name (`*` stripped), at most 1000 |
| `POST /sandboxes/{id}/prompt` | `POST /v1/vms/{id}/prompts` | `provider` (boat.dev's names; `claude-code` = claude), `prompt`, `model`, `new`, `conversationId`; `reasoningEffort`, `fast` ignored. `GET /api/v1/provider-models` (boat.dev documents it for these choices) lists every harness with the models the operator gives `boat-api serve -provider-models 'claude=sonnet,opus;codex=gpt-5'`; no model lists reasoning efforts or fast mode. Gives the VM a fresh full TTL, as boat.dev does |
| `GET /sandboxes/{id}/prompts/{promptId}` | `GET /v1/vms/{id}/prompts/{pid}` | `status`: running, finished, failed, interrupted (a steered turn reads interrupted) |
| `GET /sandboxes/{id}/events` | prompt runs | one `prompt` event per turn and a `response` event when it ends, with up to 64 KiB of the agent's output as `data.text` while the VM runs. `limit`, `cursor` (the SDKs' `<timestamp>:<id>`), `sort`, `type`, `conversation`. No partial or tool-call events, no `ETag` |
| `GET /sandboxes/{id}/conversations` | `GET /v1/vms/{id}/conversations` | conversation ids are UUIDs made from the native `conv_` ids |
| `POST /sandboxes/{id}/steer`, `/interrupt` | `POST /v1/vms/{id}/prompts/{pid}/steer`, `/interrupt` | on the running turn of `conversation` (or the most recent one); steering interrupts and resumes the session (`native: false`, `mode: fallback`) |
| `GET /saved-snapshots`, `POST`, `GET`/`DELETE /saved-snapshots/{name}` | team snapshots | a saved snapshot is a team snapshot with a name boat.dev accepts (`^[a-z0-9][a-z0-9-]{0,62}$`). Saving is synchronous (`snapshot.status` is `ready`) and replaces an older snapshot of that name. No allowance |
| `GET`/`POST /named-snapshots`, `GET`/`DELETE /named-snapshots/{name}` | the saved-snapshot routes | the same snapshots with `snapshot.named.*` types |
| `GET`/`POST /saved-snapshots/{name}/shares`, `DELETE …/shares/{shareId}` | `/v1/vms/{id}/snapshots/{sid}/shares` | needs `admin` (boat.dev: `snapshot.write`). A code (`bsh_…`) is valid 30 days here; `shr_` + the native share id; `codePrefix` is `bsh_<share id>`; `lastUsedAt` is null. Team grants made through `/v1` are not listed. An imported snapshot cannot be shared (409) |
| `GET /snapshots`, `GET /sandboxes/{id}/snapshots`, `GET …/snapshots/latest` | `GET /v1/snapshots`, `GET /v1/vms/{id}/snapshots` | ids are UUIDs made from the native ids (stable, no state). Every snapshot is full: `kind`, `generation`, `chainId`, `fileCount` null, `status` completed, `origin` captured. `limit` (1-200), `sort`; one page (`nextCursor` null). A deleted VM's snapshots keep its native id as `sandboxId` |
| `DELETE /snapshots/{snapshotId}`, `DELETE /sandboxes/{id}/snapshots` | `DELETE /v1/snapshots/{sid}` | `X-Ascii-Confirm-Delete` required (409). The sandbox form deletes its unnamed snapshots and keeps its saved ones. 202 with operations already `completed` |
| `POST /sandboxes/{id}/host` | `POST /v1/vms/{id}/ports` + `GET …/ports/{port}/url` | `port`, `public`; `localhost` refused (400). A private URL carries `boat_exp`/`boat_sig` (not `_token`), lasts 7 days, and a new one is minted on each call; `isProtected`, `access` |
| `POST /sandboxes/{id}/desktop` | `GET /v1/vms/{id}/desktop` | `desktop.url`, `mode: vnc`, never `provisioning` (the call waits for the guest desktop); `publicAccess` refused (400); `theme` ignored. Needs a guest with a desktop (`full`) |
| `POST /sandboxes/{id}/sshkey` | `POST /v1/vms/{id}/ssh` | `machineIp` is the VM's IP (reach it through the jump host); `sshEndpoint` names the jump host when there is one |
| `GET /sandboxes/{id}/files/find` | `GET /v1/vms/{id}/files/search?content=1` | ripgrep in the guest: `pattern` (plain, or `regex=true`), `path` (default home); at most 1000 matches |
| `POST /sandboxes/{id}/files/download-url` | signed URL served by boat-api | `GET <api>/api/v1/file-downloads/<token>` with no key streams the file, any size. The token is HMAC-signed with a key derived only for this, bound to the team, VM, path and the minting API key (refused once that key is deleted or expired, or its user left the team), expires after `ttlSeconds` (default 900, at most 3600), and is redacted from the audit log. The file must exist when the URL is made |
| `GET`/`POST /secrets` | team secrets | the [default-injected](#secrets) secrets: GET lists `NAME=` lines, values are never returned; POST `envContents` sets each `NAME=value` and marks it, `NAME=` marks an existing one, a marked secret left out is unmarked (not deleted). `secretFiles` refused. Needs `admin`. `pushed` reports 0: secrets reach VMs at their next start |
| `GET`/`PUT /ai-proxy` | `GET`/`PUT /v1/ai/proxy` | `enabled`; `accountId` is the team id, `org: true`, `pricing` zero, `proxyUrl` the proxy VMs use. PUT needs `admin` |
| `GET`/`POST /webhooks`, `GET`/`PATCH`/`DELETE /webhooks/{id}`, `POST …/rotate` | webhooks of this server | boat.dev's six event names; deliveries carry boat.dev's payload (`{id, type, createdAt, data: {sandbox: {id, name}, previousState, state}}`) and `X-Ascii-Event`, `-Delivery`, `-Timestamp`, `-Signature: v1=hex(HMAC(secret, id.ts.body))`, `-Attempt`. Sent: `sandbox.ready`, `sandbox.archived`, `sandbox.error`; never `hydrated`, `degraded`, `recovered`. Rotation has no overlap. Webhooks made through `/v1` keep native events and are not listed here |
| `GET /api-keys`, `POST /api-keys/scoped`, `DELETE /api-keys/{id}`, `POST …/rotate`, `GET …/usage` | `/v1/keys` | boat.dev's actions are this server's scopes (`vms:read`, `vms:write`, `exec`, `admin`); presets `read-only`, `full-sandbox`, `ci`, `admin`. `ttl` (`90d`, `24h`, seconds, up to `365d`, or `never`; left out = never, where boat.dev defaults to 90d) sets `expiresAt`; an expired key gets 401, and the links it minted die with it. No `sandboxIds`/`environmentIds` (400). Rotation has no overlap. `keyLastFour` is `****` (the secret is not stored); usage counts requests since boat-api started |
| `GET`/`POST /environments`, `PUT`/`DELETE /environments/{id}` | `/v1/environments` | the id is a UUID made from the native id (the native id and the name work too). `envContents` is the variables as `KEY=value` lines; `passSecrets: false` selects no team secrets; `name`, `isDefault`. Image, size and setup script are `/v1` only; boat.dev's credential, GitHub, secret-file, repository and access settings are refused (400) |
| `GET /limits` | VM list | `activeSandboxes`, `canStart: true`, `billingStatus: self_hosted`, `maxActiveSandboxes: -1` (team quotas live in boatd's config; going over one is 429 `limit_reached`) |
| `GET /me` | the key's user | `id`, `login`, `displayName`, `accountStatus: active` |
| `GET /sandboxes/{id}/usage` | `GET /v1/vms/{id}/usage` | `seconds` since the VM was created, `dollars: 0` |
| `GET`/`PUT`/`DELETE /telemetry` | `/v1/telemetry` | needs `admin`. `headers` lists header names (values are sealed, never shown); `lastExportAt`, `lastError` null. PUT sends no test export: `test.ok` is false with a message saying so |
| `GET /ai-usage`, `GET /sandboxes/{id}/ai-usage` | `GET /v1/ai/usage`, `GET /v1/vms/{id}/ai-usage` | `days` 1-365 (default 30). Native rows are per VM, provider and model, dated by their last use: one row each, `day` = that date, `credential` = the provider. `errors`, cache tokens and `dollars` are 0; `profile`, `org` ignored |
| `PUT /sandboxes/{id}/ai-access` | `PUT /v1/vms/{id}/ai-access` | `blocked`; needs `admin` |
| `PUT`/`DELETE /environments/{id}/vars/{key}` | `PUT /v1/environments/{id}` | sets or removes one variable (DELETE of a missing one is 404); `versionId`/`versionNumber` from the native version. Two concurrent writes to one environment can lose one |
| `POST /sandboxes/{id}/files/replace` | boatd file read + write | plain text, every occurrence, per-file results; files ≤ 5 MiB |
| `POST /sandboxes/{id}/files/upload-url` | signed URL served by boat-api | `PUT <api>/api/v1/file-uploads/<token>` with no key writes the body (up to `-max-file-bytes`). Same token scheme as download URLs, marked for upload: neither kind works for the other |
| `GET /sandboxes/{id}/artifacts` | boatd file read | raw bytes, ≤ 50 MiB (413 above; use `files/download-url`) |
| `POST /sandboxes/{id}/share` | none | a no-op answering the sandbox: every sandbox here is usable by its whole team |
| `POST /sandboxes/{id}/unshare` | none | 501 `not_supported`: there are no private sandboxes here |
| `POST /sandboxes/{id}/recover` | `POST /v1/vms/{id}/stop` (`force`) then `start` | a running VM only (409 `no_machine` otherwise); memory is lost, the disk is kept; no `too_soon` limit and no `sandbox.recovered` event |
| `Idempotency-Key` | boat-api's store | create and fork, per team, 24 h, ≤ 255 chars; same body replays (`Idempotent-Replayed: true`), another body 409 `idempotency_key_reused`, running 409 `idempotency_in_progress`, a failure frees the key. Finished answers survive a restart; a request in flight when boat-api dies frees its key |

| `GET /credential-profiles`, `PUT`/`DELETE /credential-profiles/{profile}` | `/v1/ai/credentials` | one profile per team: `default` (its id is the team id; either works in paths), `isDefault: true`, `owner.kind: org`, `customApis` and `accounts` empty. PUT answers it unchanged for `isDefault`; renaming or `access` is 501 `not_supported`. DELETE is always 409 `is_default`. Needs `admin` (boat.dev: `account.admin`) |
| `PUT`/`DELETE /credential-profiles/{profile}/keys/{credential}` | `PUT`/`DELETE /v1/ai/credentials/{provider}` | `anthropic-`, `openai-`, `openrouter-`, `deepseek-`, `moonshot-`, `mistral-`, `xai-api-key`; `llmgateway-`, `opencode-api-key`, sign-ins (`*-subscription`) and Bedrock are 501 `not_supported`. `keepCurrent` over an existing key is 501 |
| `GET`/`POST /credential-profiles/{profile}/accounts/{credential}`, `PUT …/{account}/active`, `DELETE …/{account}` | the same | one account per credential, its id the credential id, `label` `…` + the key's last 4. POST sets the first key (a second account is 501); switching to the one account is a no-op; DELETE removes the key |
| `GET /snapshots/{snapshotId}/download` | `GET /v1/vms/{id}/snapshots/{sid}/download` | streams the archive itself (`tar.zst`, one file) instead of boat.dev's JSON of signed chunk URLs; an imported snapshot (no VM) is 501 |

**Not supported** (22 operations, 501 `not_supported` with a message saying what; use `/v1` where it exists): orgs (3), data retention (2), repos (2), CI (5), more credential profiles (`POST /credential-profiles`) and custom APIs (2), environment upgrades, secret files (2) and repositories (2), snapshot `tree` and `files` (2; download the snapshot instead). Also no `RateLimit` headers and no `X-Boat-Org`.

**Errors.** A refusal from the VM's own agent keeps its message and becomes a 4xx on `/v1` and `/api/v1` alike: an image without the feature (desktop on `slim`) is 422 `not_supported`, bad input 400, a session that exists 409; other guest failures are 502 with the guest's message. Under `/api/v1`, `code` is `http_<status>` for server-side failures, so it always agrees with `status`.

**CLI.** `boat` takes boat.dev's verbs next to its own: `boat extend <vm> --hours N | --ttl S | --no-auto-stop` (sets the time left, as boat.dev's) or `--by 2h` (pushes the deadline out), `boat limits`, `boat snapshot <vm> NAME` and `boat snapshot rm NAME`, `boat history <vm> [command-id] [-f]`, `boat webhook create URL --event ready … | list | rotate ID | remove ID`, `boat rename <vm> NAME`, `boat api-key create NAME [--preset read-only|full-sandbox|ci|admin | --actions a,b] | list | usage ID | rotate ID | revoke ID` (`--ttl 90d|24h|never`; `--sandbox` and `--env` are refused, as by the API), `boat status <vm>`, `boat events <vm>`, `boat recover <vm>`, `boat share <vm>`, `boat unshare <vm>` (501 here), `boat dashboard [--no-open]` (the API's origin), `boat deletion status <operation id>`, and the global `--api-url URL` (boat.dev's flag; this run only, `login --url` saves one). Native commands take `bx_` ids too (`boat ssh bx_…`). boat.dev's cloud-only verbs (`org`, `billing`, `repos`, `ci`, `credentials`, `retention`, `onboard`) answer "not supported on self-hosted boat", `boat login [KEY] | --key-stdin`, `boat logout`, `boat completions bash|zsh|fish|powershell` (subcommands and VM ids; no result cache) and `boat self-update` (= `boat upgrade`).

**Snapshot grants to another team** (`POST /v1/vms/{id}/snapshots/{sid}/shares` with `team_id`) answer the same 201 whether or not that team exists. The grant reaches the team only once it consents: its admin lists the sending team in `PUT /v1/teams/{id}/share-allowlist {"teams": ["team_…"]}`. Until then the grant is not listed in its `GET /v1/shared-snapshots` and `import-share` answers 404.

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
- **No guest is proven to survive a switch under NixOS.** Guests boot and survive a boatd restart under the NixOS server profile in an nspawn container on the runner (`boat-real guest`), not on a booted NixOS host; see "Not tested by anything" above.
- **No real agent run:** CI has no API keys, so agents are only exercised with a stub.
- **Windows CLI:** cross-compiled only, never run on Windows. `install.sh` and `boat upgrade` were not tested there.
- **First release not tagged:** `release.yml` has a `workflow_dispatch` dry run that builds every asset without publishing.
- **Only partly API-compatible with boat.dev:** `/api/v1` maps 100 of boat.dev's 122 operations (see [boat.dev API compatibility](#boatdev-api-compatibility-apiv1)). boat.dev's official SDKs run the calls listed there against real VMs in CI; the other mapped operations run only against a fake boatd. Orgs, repos, CI, data retention and billing are not offered (501 `not_supported`); credential profiles are one `default` profile per team.
- **SSH UX** is `-J jump@… dev@<ip>` (wrapped by `boat ssh`).
- **Egress controls are basic:** SMTP is blocked by default and egress can be capped per VM (`egressMbit`), but there are no IOPS limits, no per-team egress budget and no destination filtering.
- **The sealed keys** are bound to this host's TPM/host key. Keep the offline copies: without the master key a hardware change loses the whole API state (teams, keys, secrets, environments), and desktop and route links stop working.
- **`vmDomain` on the Public Suffix List** is an owner step, see checklist item 2. Until then a tenant page's script can set `Domain` cookies for sibling tenants.
- **The AI proxy** runs inside boat-api: per-VM limits bound it, but it has no process of its own.
- **The VM's own key** (`BOAT_API_KEY` in the guest, with `-vm-api-url`) reaches only that VM's routes, but anyone with a shell in the VM has it, and processes that read it before a rotation keep a dead one. See `components/boat/api/README.md`.
- **API latency:** create, fork, stop and start no longer wait for boat-api's `state.json` fsync, which cost 1.9 s of a 2.73 s new-from-snapshot in boatbench 41652606509, on a disk busy with a snapshot's writeback. `api/latency_test.go` holds each under 100 ms of API time with a 300 ms fsync. A fresh boatbench run has not measured the result yet.
- **Guest images** are not pinned by default: `services.boat.images` is empty until you set the URLs and hashes of a release (`boat-guest-*`, `boat_guest_SHA256SUMS`). No tagged release has published them yet; see [Guest images before a release](#guest-images-before-a-release).
- **Metrics:** host metrics and `boat_reboot_pending` only; per-VM numbers come from `/v1/usage`.

## Integration notes

- **Flake.** `services.boat` is in `nixosModules.default`, with `nixosModules.server`, `nixosConfigurations.server`, the `boat*` packages and `packages.iso-server` (headless live system that installs `headless` or `server`, built by `boat-e2e` and on release).
- **Health.** `panda-health` watches `boat-api`, and `boat-gateway` + `caddy` when the gateway is on, so a bad update rolls back. It leaves out `boatd`, which fails on purpose without KVM.
