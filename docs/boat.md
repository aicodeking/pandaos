# boat: self-hosted agent VMs on a dedicated server

`boat` turns one dedicated server into a self-hosted alternative to boat.dev.
Each agent gets a persistent Firecracker microVM, and you get an API, a CLI,
SDKs, ssh, port URLs, a browser desktop, forks and snapshots. The server
profile (`nixos/profiles/server.nix`) is headless PandaOS with
`services.boat.enable = true`.

| Part | Where | What it does |
|---|---|---|
| `boatd` | `components/boat/boatd` | Root VM manager: Firecracker + jailer, bridge `boatbr0`, `10.200.0.0/16` NAT, nftables tenant isolation, snapshots. Writes `/run/boat/routes.json`. |
| `boat-api` | `components/boat/api` | REST API on 127.0.0.1:8740 (keys, teams, secrets, webhooks, metering, link minting). User `boat`. |
| `boat` | `components/boat/cli`, `sdk` | CLI (`--json` everywhere) and the Python/TypeScript SDKs. |
| guest | `components/boat/guest` | Ubuntu 24.04 rootfs + kernel with `boat-agent`; the `full` image adds a KasmVNC desktop on guest port 6080. |
| gateway | `components/boat/gateway` | Caddy (TLS), `boat-gateway` (port routes, desktops) and the ssh jump host. |
| module | `components/boat/module.nix`, `ops.nix` | NixOS options and units: the above plus reboot detection, maintenance reboots, backups. |

## Operator checklist

Before you hand out API keys:

1. **Hardware with KVM.** `ls /dev/kvm` in the rescue system. Without it, boatd refuses to start (`boatd: /dev/kvm is missing ...`, exit 78, no restart loop).
2. **Two domains.** Pick a base domain (`boat.example.com`) and a **separate registrable domain for tenant port routes** (`boatvms.example.net`, or a name under a suffix on the [Public Suffix List](https://publicsuffix.org/)). Set it as `services.boat.vmDomain`. The default, `vm.<domain>`, works, but then a tenant's app is a sibling of `api.` and the desktops and can set cookies on the parent domain.
3. **DNS** (see below), including both wildcards.
4. **TLS.** A DNS-01 provider for the wildcards, or accept the HTTP-01 fallback's limits.
5. **Backups.** Set `services.boat.backup` to a restic repository you control, keep the repository password off the box, and keep an offline copy of the master key (see "Secrets").
6. **Reboot schedule.** Turn on `services.boat.maintenance.reboot` (default: Sunday 04:00, only when a reboot is pending), or watch `boat_reboot_pending` and reboot by hand.
7. **Abuse policy.** You are the hosting provider for everything your tenants run: public port URLs, outbound traffic from your IPs, mail, scanning and mining. Publish an acceptable-use policy, keep `abuse@` working for your provider (Hetzner and OVH forward complaints and can null-route the server), keep team quotas tight, and give keys only to people you trust. boat has no outbound rate limits or port 25 block yet; add an nftables rule if you need one (see "Honest gaps").
8. **Monitoring.** node-exporter is on :9100 (not opened in the firewall). It includes the textfile metric `boat_reboot_pending`. Usage per team is at `GET /v1/usage`.

## Install on a dedicated server (Hetzner, OVH)

1. Boot the PandaOS ISO (Hetzner: virtual ISO in the Robot console or KVM-over-IP; OVH: IPMI).
2. Run:
   ```sh
   panda-install --profile server --disk /dev/nvme0n1 --data-disk /dev/nvme1n1 \
     --domain boat.example.com --acme-email ops@example.com \
     [--ipv6-prefix 2a01:4f8:xxx:yyy:b0a7::/80] --ssh-key "ssh-ed25519 ..."
   ```
   `--data-disk` erases the second disk and formats it as btrfs (`PANDADATA`) for `/var/lib/boat`. It is pinned by UUID as a required mount in `/etc/pandaos/disk.nix` and recorded in `/etc/pandaos/data-disk`. If that disk is ever missing, boatd and boat-api refuse to start rather than write VM data to the system disk. Without `--data-disk`, `/var/lib/boat` is a btrfs subvolume on the system disk. VM disks are reflink copies either way, so fork and snapshot are near-free. To mirror later: `btrfs device add` + `btrfs balance start -dconvert=raid1 -mconvert=raid1`.
   *The ISOs need the server system on board for `--profile server`; see "Integration notes".*
3. Reboot, log in, and apply your settings now (the daily update would also do it):
   ```sh
   sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
   ```
4. Add more settings to `/etc/pandaos/local.nix`:
   ```nix
   services.boat = {
     vmDomain = "boatvms.example.net";
     images."ubuntu-24.04/vmlinux" = { url = "https://.../vmlinux"; sha256 = "..."; };
     images."ubuntu-24.04/rootfs.ext4.zst" = { url = "https://.../rootfs-full.ext4.zst"; sha256 = "..."; };
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
5. **First admin key:** run `sudo boat-admin init -user me -team ops`. It prints an owner API key once. boat-admin runs `boat-api admin init` as `boat` with the sealed master key. Then run `boat login --url https://api.boat.example.com --key ...`.

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
- **HTTP-01 fallback.** Leave `dnsProvider = null`. Caddy then gets one certificate per desktop and per port URL on the first request, after asking boat-gateway (127.0.0.1:8743) whether that host is live. The costs: a few seconds' wait on first use; Let's Encrypt's limit of 50 certificates per registered domain per week (a separate `vmDomain` gives routes their own budget); and every name is published in CT logs.

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

- **One origin per VM.** Each desktop is `<id>.desktop.D` and each port route is `<port>-<id>.<vmDomain>`. Session cookies are `__Host-` prefixed: host-only, `Secure`, `Path=/`, `HttpOnly`. Browsers refuse a `__Host-` cookie that carries a `Domain`, so a page on another VM's origin can neither read nor plant one. Desktop cookies are `SameSite=Strict`. The gateway sets them on a small same-origin page that then navigates on, because a Strict cookie set during a cross-site redirect chain is withheld. Every VM's credential is bound to its id by the HMAC: VM A's cookie or link gets 401/403 on VM B's origin. This is checked in the e2e.
- **Desktop links.** `GET /v1/vms/<id>/desktop` returns `https://desktop.D/<id>?exp&sig` (sig = `hex(HMAC-SHA256(key, "<id>.<exp>"))`, 5 min by default). The gateway 302s it to `https://<id>.desktop.D/?exp&sig`, which checks the signature, sets `__Host-boat_gw` and drops the signature from the URL. Toward the guest, the gateway removes the client's `boat_*`/`__Host-boat_*` cookies and sends `boat_desktop=hex(HMAC(key, "guest-desktop.<id>"))`. boat-api starts the guest desktop with that same token.
- **Port routes are private by default.** A route needs a token: `token = <exp>.hex(HMAC(key, "route.<port>-<id>.<exp>"))`, minted by `GET /v1/vms/<id>/ports/<port>/url` (`boat ports url VM PORT`). Programs send it as `X-Boat-Route-Token`; browsers open `…/?boat_exp&boat_sig` once, which sets the host-only `__Host-boat_route` (SameSite=Lax). A port is reachable without a token only if added with `--public`, which boatd lists in routes.json `"public"`. When the list is missing, routes stay private (fail-closed). Tokens and cookies never reach the VM.
- **Live routes.** boat-gateway (Go stdlib, websockets included) re-reads `/run/boat/routes.json` whenever its mtime or size changes, so new VMs and ports never reload Caddy.
- **SSH:** OpenSSH ProxyJump.
  ```sh
  boat ssh <vmid>                                    # ProxyCommand ssh -W via the jump host
  ssh -J jump@ssh.boat.example.com:2222 dev@10.200.x.y
  ```
  Port 2222 admits only the system user `jump`. jump has no password, shell, tty, agent, X11 or socket forwarding (`ForceCommand false`). It can open only `direct-tcpip` channels to the targets in its key's `permitopen="<ip>:22"`. boat-api writes those lines to `/var/lib/boat/ssh/jump_authorized_keys` and rebuilds them on grants, VM deletion and VM events. The AuthorizedKeysCommand `/etc/boat/jump-keys` passes only lines of exactly the form `restrict,port-forwarding,permitopen=...`. It also drops every target that is not a running VM in boatd's routes.json, so a grant dies the moment its VM stops, expires (TTL) or is deleted, even before boat-api prunes it. An IP reused by a new VM is never reachable with an old grant.

## Security model

- **VM isolation.** Each VM is a KVM guest run by Firecracker (a minimal Rust VMM) inside the jailer: its own uid/gid (from 900000), a chroot, a mount namespace, its own cgroup under `boatd.service`, and seccomp. A guest root exploit must escape KVM, then the jailed, unprivileged Firecracker process.
- **Tenant network isolation.** Every VM has its own tap on `boatbr0`. nftables drops tap-to-tap traffic, except within a team that has `allowIntraTeam`, and NAT is outbound only. VMs reach the host only on the firewall's open ports (80/443/22/2222), never 127.0.0.1 services.
- **Web isolation.** Every VM's desktop and every port route has its own origin and host-only `__Host-` cookies, and credentials are HMAC-bound to the VM (and port). Port routes are private by default. With a separate registrable `vmDomain`, tenant apps cannot touch `api.`/`desktop.` cookies either. `api.D` uses bearer tokens, not cookies.
- **Host services.** boatd is the only root daemon. boat-api and boat-gateway run as `boat` with no capabilities, `ProtectSystem=strict`, a syscall filter and MemoryDenyWriteExecute. `/internal/*` is never published. The boatd socket is 0660 `root:boat`. boatd and boat-api have `OOMScoreAdjust=-900`, so the host OOM killer takes a VM before them. Both carry `RequiresMountsFor=/var/lib/boat`.
- **Secrets.** Tenant secrets and webhook keys are AES-256-GCM sealed under boat-api's master key. That key exists on disk only as a systemd credential (`/var/lib/boat/secrets/boat-master-key.cred`), encrypted with the TPM2 when present plus the host key `/var/lib/systemd/credential.secret`, and handed to boat-api with `LoadCredentialEncrypted`. An older plaintext `master.key` is sealed, verified and deleted on upgrade. **Keep an offline copy:** `sudo systemd-creds decrypt --name=boat-master-key /var/lib/boat/secrets/boat-master-key.cred - | base64`. Without it, a new motherboard (TPM) or a lost credential.secret makes sealed secrets unreadable. API keys are stored as salted HMACs. The desktop/route HMAC key (`secrets/desktop.key`, 0600 `boat`) never leaves the host. Caddy's DNS token belongs in `gateway.environmentFile`.
- **Host hardening** comes from PandaOS: nftables, key-only ssh, sshguard, sysctl hardening, daily updates with health-check rollback.
- **Not defended:** a malicious server admin; side channels between VMs on shared cores (no core pinning); one team saturating the uplink or disk (no bandwidth/IOPS limits); and abuse from your IPs beyond what your policy and quotas stop.

### Tenant model

- **Teams** own VMs, secrets, webhooks and keys. A key belongs to one user in one team and has scopes (`vms:read`, `vms:write`, `exec`, `admin`). Admins manage members and secrets.
- A team sees and reaches only its own VMs: API objects are filtered by team, nftables blocks other teams' VMs, jump grants list only the team's VM IPs, and every desktop link and route token is bound to one VM.
- **Quotas** (`services.boat.quotas`, per-team overrides in boatd) cap VMs, vCPU and memory. Metering (`/v1/usage`) records VM-seconds per size and snapshot-days per team. There is no billing.

## Upgrades and reboots

- **Updates.** The daily unattended update (`updates.nix`) switches to the new system with a health check and rolls back on failure. boatd is never restarted by a switch (`restartIfChanged = false`), so running VMs are untouched. boat-api, boat-gateway and Caddy restart in seconds; the API's clients retry.
- **Pending reboot.** `boat-reboot-check` (hourly) compares the booted kernel, initrd and modules, and boatd plus its config and Firecracker paths, with the installed ones. It logs `boat: reboot pending (changed: ...)`, writes `/run/boat/reboot-pending` and exports `boat_reboot_pending` (0/1) with a `reason` label.
- **Maintenance reboot.** With `maintenance.reboot = true`, a timer (`onCalendar`, default Sunday 04:00, ±10 min) reboots when a reboot is pending (`onlyWhenPending = false` reboots every time). The reboot drains on its own: stopping boatd pauses and memory-snapshots every running VM (`stopTimeoutSec`), and boatd resumes them after boot, with processes, tmux sessions and open files intact. TCP connections into VMs drop.
- **Manual:** `sudo systemctl restart boatd` applies a new boatd without a reboot (VMs snapshot and resume); `sudo reboot` for a kernel.
- **Rollback:** pick the previous generation in the boot menu, or `sudo nixos-rebuild switch --rollback`. VM state on `/var/lib/boat` is independent of the system generation.

## Backups

`services.boat.backup` (restic, to a repository you provide):

1. Each run (default 03:00, `Persistent`) takes a **read-only btrfs snapshot** of `/var/lib/boat`, so VM disks, memory snapshots, the API database and the sealed master key are consistent to one instant (crash-consistent).
2. restic uploads the snapshot (images excluded; they are re-downloadable), the btrfs snapshot is deleted, and `pruneOpts` apply (7 daily, 4 weekly, 6 monthly).

Restore onto a fresh boat host:
1. Stop boatd and boat-api.
2. `restic restore latest --target /` (the paths are `/var/lib/boat/.backup/...`; move them into `/var/lib/boat`).
3. Restore the master key: `systemd-creds encrypt --name=boat-master-key <key> /var/lib/boat/secrets/boat-master-key.cred`.
4. Start boatd and boat-api again.

Test a restore before you need one.

## Feature parity with boat.dev

| Feature | Here | Notes |
|---|---|---|
| Create / delete VMs | yes | `boat new`, sizes and images from the module |
| Stop / start | yes | the disk persists |
| Pause / resume (memory) | yes | stop → start restores memory (same `boot_id`; e2e) |
| Fork | yes | reflink, near-instant; new hostname, IP, machine-id, ssh host keys |
| Snapshots | yes | create / list / delete, `new --snapshot` |
| ssh | yes | ProxyJump via `ssh.<domain>:2222` (`boat ssh`); IP target, not `<vmid>@host` |
| scp / files | yes | `boat scp` via the files API; `scp -J` works |
| Ports | yes | `https://<port>-<id>.<vmDomain>`, **private by default** (token/cookie), `--public` to open |
| Public IPs | partial | IPv6 per VM with `ipv6Prefix`; IPv4 only from `publicIPv4Pool`, otherwise NAT + host port forwards |
| Desktop (browser) | yes | KasmVNC (`full` image), one origin per VM, signed links |
| Prompt (run an agent) | yes | `boat prompt` |
| CLI with JSON | yes | `--json` |
| REST API | yes | `components/boat/api/openapi.json` |
| SDKs | yes | Python, TypeScript |
| Webhooks | yes | HMAC-signed, delivery log, test |
| API keys, scopes | yes | |
| Secrets | yes | sealed at rest, injected as env vars |
| Teams, quotas | yes | |
| Auto-stop on idle / TTL | yes | `idle_stop_s`, `ttl_s` |
| Metering | yes | `/v1/usage`: VM-seconds by size, snapshot-days |
| Backups | yes | restic of btrfs snapshots (operator-run) |
| Billing | no | metering only; no payments, invoices or plans |
| Regions | N/A | one server is one region |
| HA / live migration | no | single host |
| GPUs | no | Firecracker has no PCI passthrough |

### What the `boat-e2e` workflow proves

Both parts run on a GitHub KVM runner with real Firecracker (jailer on) and the real guest image.

1. `components/boat/e2e.sh` drives every feature through the `boat` CLI: new (slim/full), exec, ssh, scp, egress, secrets, ports, stop/start as memory restore, fork, snapshots, prompt, desktop, quota (429), TTL, signed webhooks, usage, rm.
2. `components/boat/gateway/e2e-edge.sh` puts **real Caddy with its internal CA (`tls internal`)** and the **jump sshd on 2222** (module settings and filter) in front of a fresh stack. Every check runs over verified TLS:
   - a private route gets 401 without a token and 200 with one (header, and link → host-only cookie); a token for another port gets 401; a public route gets 200;
   - the desktop link moves to `<id>.desktop.D`, its `__Host-` cookie is host-only, and VM D's cookie/link get 401/403 on VM A's origin;
   - `ssh -J jump@localhost:2222 dev@<vm>` and `boat ssh` through it work; the jump host gives no shell, refuses targets without a grant and other users on 2222, and the grant and routes are gone after `boat rm`;
   - it then calls `guest/desktop-check.sh` (desktop websocket + a frame), `sdk/e2e-live.sh` and `api/e2e-auth.sh` against that stack. Any of the three that is missing is reported as a workflow warning, not a pass.

`checks.boat-module` (NixOS VM, stubs for boatd and boat-api) proves the module itself: units, the sealed master key loaded into boat-api, the data-disk guard, the reboot metric, Caddy + gateway over TLS (private/public routes, per-VM desktop origins) and the jump host.

Not covered: idle auto-stop, IPv6 and public IPv4, cross-team network isolation between live VMs, boatd restart/reconcile under load, backup/restore, maintenance reboots and the installer's `--data-disk` path.

## Honest gaps

- **One host:** no HA, migration or cross-host scheduling.
- **No GPUs, no nested KVM** in guests (Docker works).
- **SSH UX** is `-J jump@… dev@<ip>` (wrapped by `boat ssh`).
- **No egress controls:** no bandwidth/IOPS limits, no port-25 block, no per-team egress rate limit. Add nftables rules for your abuse policy.
- **HTTP-01 fallback:** CT-log exposure, rate limits, first-request delay.
- **The master key** is bound to this host's TPM/host key. Keep the offline copy, or a hardware change loses sealed secrets.
- **Guest images** must be published (URL + sha256) for `boat-images`. Until then, build them with `components/boat/guest/build.sh` and copy them to `/var/lib/boat/images/<id>/`.
- **Metrics:** host metrics and `boat_reboot_pending` only; per-VM numbers come from `/v1/usage`.

## Integration notes

- **Flake.** `services.boat` is in `nixosModules.default`, with `nixosModules.server`, `nixosConfigurations.server` and the `boat*` packages. **Open:** the ISOs must carry the server system for `panda-install --profile server`. Add `"server"` to the `iso-desktop` targets (or a new `iso-server = mkIso system "headless" [ "headless" "server" ]`) and to `packages.panda-install`'s `systems` in `flake.nix`. Until then, install `headless` and change `profile = "server"` in `/etc/pandaos/flake.nix`.
- **Health.** Consider adding `boat-api`, `boat-gateway` and `caddy` to `panda-health`'s units (`nixos/modules/boot.nix`). Leave out `boatd`, which fails on purpose without KVM.
