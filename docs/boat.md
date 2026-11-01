# boat: self-hosted agent VMs on a dedicated server

`boat` turns one dedicated server into a self-hosted alternative to boat.dev.
Each agent gets a persistent Firecracker microVM, and you get an API, a CLI,
SDKs, ssh, public port URLs, a browser desktop, forks and snapshots. The
server profile (`nixos/profiles/server.nix`) is headless PandaOS with
`services.boat.enable = true`.

| Part | Where | What it does |
|---|---|---|
| `boatd` | `components/boat/boatd` | Root VM manager: Firecracker + jailer, tap/bridge `boatbr0`, `10.200.0.0/16` NAT, nftables tenant isolation, snapshots. Writes `/run/boat/routes.json`. |
| `boat-api` | `components/boat/api` | Public REST API on 127.0.0.1:8740 (keys, teams, secrets, webhooks, metering). Runs as user `boat` and talks to boatd's socket. |
| `boat` | `components/boat/cli`, `sdk` | CLI (`--json` everywhere) and SDKs. |
| guest | `components/boat/guest` | Ubuntu 24.04 rootfs + kernel, with the `boat-agent` and a KasmVNC desktop on guest port 6080. |
| gateway | `components/boat/gateway` | Caddy (TLS), `boat-gateway` (HTTP routes, desktop links) and the ssh jump host. |
| module | `components/boat/module.nix` | NixOS options and units for all of the above. |

## Setup on a dedicated server (Hetzner, OVH)

1. **Hardware.** You need bare metal with VT-x or AMD-V. Run `ls /dev/kvm` in the rescue system to check. A cloud VM works only if the provider offers nested virtualization. Without `/dev/kvm`, boatd refuses to start and logs `boatd: /dev/kvm is missing ...`, with exit code 78 and no restart loop.
2. **Install.** Boot the PandaOS ISO (Hetzner: mount it as a virtual ISO in the Robot console or via KVM-over-IP; OVH: IPMI). Then run:
   ```sh
   panda-install --profile server --disk /dev/nvme0n1 [--data-disk /dev/nvme1n1] \
     --domain boat.example.com --acme-email ops@example.com --ssh-key "ssh-ed25519 ..."
   ```
   `--data-disk` formats the second disk as btrfs labelled `PANDADATA` and mounts it at `/var/lib/boat`. Without it, `/var/lib/boat` is a btrfs subvolume on the system disk. Either way, VM disks are reflink copies, so fork and snapshot cost no extra space. For a two-disk mirror, convert later with `btrfs device add` and `btrfs balance start -dconvert=raid1 -mconvert=raid1`. *(`--profile server`, `--data-disk`, `--domain` and `--acme-email` need installer support; see "Integration notes".)*
3. **Configure** in `/etc/pandaos/local.nix`, then `sudo nixos-rebuild switch`:
   ```nix
   services.boat = {
     domain = "boat.example.com";
     acmeEmail = "ops@example.com";
     ipv6Prefix = "2a01:4f8:xxx:yyy:b0a7::/80";   # optional, see IPv6 below
     images."ubuntu-24.04/vmlinux" = { url = "https://.../vmlinux"; sha256 = "..."; };
     images."ubuntu-24.04/rootfs.ext4.zst" = { url = "https://.../rootfs-full.ext4.zst"; sha256 = "..."; };
     gateway.caddyPackage = pkgs.caddy.withPlugins {
       plugins = [ "github.com/caddy-dns/cloudflare@v0.2.1" ]; hash = "sha256-...";
     };
     gateway.dnsProvider = "cloudflare {env.CF_API_TOKEN}";
     gateway.environmentFile = "/var/lib/secrets/caddy-dns.env";   # CF_API_TOKEN=...
   };
   ```
   Until a domain is set, the profile uses `boat.localhost`, which Caddy serves from its own local CA.
4. **First admin key:** `sudo -u boat boat-api admin init -data /var/lib/boat/api` prints an owner API key once. Then run `boat login`.

### DNS records

Use `boat.example.com` as the base domain and the server's addresses:

| Name | Type | Value |
|---|---|---|
| `api.boat.example.com` | A / AAAA | server |
| `desktop.boat.example.com` | A / AAAA | server |
| `ssh.boat.example.com` | A / AAAA | server |
| `*.vm.boat.example.com` | A / AAAA | server |

Port URLs look like `https://<port>-<vmid>.vm.boat.example.com`.

### TLS: wildcard (DNS-01) or per-host (HTTP-01)

- **Wildcard (recommended).** Set `gateway.dnsProvider` and a Caddy build that has the matching [caddy-dns](https://github.com/caddy-dns) plugin (`pkgs.caddy.withPlugins`). Caddy gets one `*.vm.<domain>` certificate through DNS-01. New port URLs then work instantly and no hostnames show up in Certificate Transparency logs.
- **Fallback, no DNS API.** Leave `dnsProvider = null`. Caddy then gets a certificate per port URL on demand (HTTP-01/TLS-ALPN) on the first request. It asks boat-gateway's `ask` endpoint (127.0.0.1:8743) first, so certificates are issued only for live `<port>-<vmid>` routes. The costs: the first request to a new URL waits a few seconds; Let's Encrypt allows 50 certificates per registered domain per week, so churny port use can hit that limit; and every port URL is published in CT logs.
- `api.`, `desktop.` and `ssh.` are fixed names and always use normal certificates (ssh needs none).

### IPv6 subnet routing

- **Hetzner** routes a `/64` to the server. Use a sub-prefix for VMs, e.g. `ipv6Prefix = "2a01:4f8:a:b:b0a7::/80"`, and keep `::2` on the uplink. Because the /64 is routed (not on-link), no NDP proxy is needed.
- **OVH** puts the `/64` on-link. Either ask OVH to route it, or enable NDP proxying (`net.ipv6.conf.<uplink>.proxy_ndp=1` plus `ip -6 neigh add proxy ...`, or `ndppd`). The module sets `accept_ra=2` so the uplink still takes router advertisements while forwarding is on.
- **Extra IPv4.** Route additional IPs or a subnet to the server, list them in `publicIPv4Pool`, and boatd maps them 1:1 to VMs.

## Sizing

The defaults are sizes `small` 2 vCPU/4 GiB, `default` 4/8, `large` 8/16 and `xlarge` 16/32, all with sparse disks, plus a team quota of 10 VMs / 32 vCPU / 64 GiB.

- **RAM** is the limit. Firecracker does not overcommit guest memory the way balloons do. Budget about the sum of running VMs' `mem_mib` plus 2 GiB for the host. Stopped VMs cost only disk.
- **CPU** may be overcommitted about 2-4x for agent workloads, which are mostly idle and waiting on LLMs.
- **Disk**: about 3 GB per image, plus each VM's written blocks (reflink, copy-on-write). Snapshots of running VMs also store guest memory.
- Example: an AX102 (16 cores, 128 GB RAM, 2x1.92 TB NVMe) fits about 14 `default` VMs running at once and hundreds stopped.

## Gateway design

```
Internet --443--> Caddy --api.D---------------------> boat-api 127.0.0.1:8740   (/internal/* -> 404)
                        --desktop.D/<vmid>/---------> boat-gateway :8742 --> <vm ip>:6080 (guest token proxy)
                        --<port>-<vmid>.vm.D--------> boat-gateway :8742 --> <vm ip>:<port>
         --2222--> sshd (user jump only) --direct-tcpip, permitopen--> <vm ip>:22
```

- **HTTP routes.** boat-gateway is a small Go stdlib reverse proxy (websockets included). It re-reads `/run/boat/routes.json` whenever the file's mtime or size changes, so adding a port or a VM never reloads Caddy. Only ports a VM has published (`boat ports add`) are routed. Everything else returns 404. Port URLs are public; a VM service that needs auth must do it itself.
- **Desktop links.** `GET /v1/vms/<id>/desktop` returns `https://desktop.D/<id>?exp=<unix>&sig=<hex HMAC-SHA256(key, "<id>.<exp>")>`, valid for 5 minutes by default. The gateway checks it, trades it for an HttpOnly, Secure cookie `boat_gw` scoped to `Path=/<id>/`, and redirects to a URL without the signature. The link is useless for any other VM. Toward the guest, the gateway strips the client's `boat_*` cookies and sends the guest proxy's `boat_desktop=<hex HMAC(key, "guest-desktop.<id>")>`. boat-api starts the guest desktop with that same token. `key` is `/var/lib/boat/secrets/desktop.key` (32 random bytes, 0600 `boat`), created once by `boat-secrets.service`.
- **SSH** uses stock OpenSSH ProxyJump, chosen over sshpiper or a custom proxy. The reasons: no extra daemon or dependency, the Go side stays stdlib, and the bastion is plain sshd, already hardened and watched by sshguard. Usage:
  ```sh
  boat ssh <vmid>                                  # same, via ProxyCommand ssh -W
  ssh -J jump@ssh.boat.example.com:2222 dev@10.200.x.y
  boat scp ./a.txt <vmid>:/tmp/a.txt               # via the files API
  ```
  Port 2222 accepts only the system user `jump`. jump has no password, shell, tty, agent, X11 or socket forwarding (`ForceCommand false`). It can open only `direct-tcpip` channels to the targets its key's `permitopen="<ip>:22"` lists. boat-api writes those lines to `/var/lib/boat/ssh/jump_authorized_keys`. `/etc/boat/jump-keys`, the AuthorizedKeysCommand, passes only lines that start with `restrict,port-forwarding,permitopen="<ip>:22"`, so a bad write cannot grant more. The guest's `dev` user trusts the same public key; boat-api installs it. Port 22 is for admins as usual.
  The trade-off: the user sees an IP rather than `ssh <vmid>@ssh.D`. An `~/.ssh/config` `Host *.boat` block with `ProxyJump` hides that. If `<vmid>@host` UX matters, swap in sshpiper (workingdir plugin) on 2222 later; the key file stays the same.

## Security model

- **VM isolation.** Each VM is a KVM guest run by Firecracker, a small Rust VMM with a minimal device model, inside the jailer: its own uid/gid per VM (from 900000), a chroot, a mount namespace and its own cgroup under `boatd.service` (`Delegate=yes`), plus Firecracker's seccomp filters. A guest root exploit has to escape KVM first, then the jailed, unprivileged Firecracker process.
- **Tenant network isolation.** Each VM gets a tap on `boatbr0`. boatd's nftables rules drop tap-to-tap traffic, except within one team when `allowIntraTeam` is set, and NAT only outbound traffic. VMs reach the host only on the ports open in the firewall (80/443/22/2222), never on 127.0.0.1 services (boat-api, the gateway, node-exporter).
- **Host services.** boatd is the only root daemon (it needs KVM, taps, nftables and the jailer). boat-api and boat-gateway run as `boat` with a strict systemd sandbox: no capabilities, `ProtectSystem=strict`, a syscall filter and MemoryDenyWriteExecute. `/internal/*` API paths are never published. The boatd socket is 0660 `root:boat`.
- **Secrets.** User secrets and webhook signing keys are AES-256-GCM sealed by boat-api's master key in `/var/lib/boat/api` (0700 `boat`); systemd `LoadCredential` is supported. API keys are stored only as salted HMACs. The desktop key and the jump key file never leave the host. Caddy's DNS token belongs in `gateway.environmentFile`, not in the Nix store.
- **Host hardening** comes from PandaOS: nftables firewall, key-only ssh, sshguard, kernel sysctl hardening, daily unattended updates with health-check rollback. boatd is never restarted by an update (`restartIfChanged = false`), so VMs survive; it picks up a new version on reboot or `systemctl restart boatd`.
- **Not defended:** a malicious server admin, side channels between VMs on shared cores (no core pinning yet), and DoS by one team against the host's uplink (no bandwidth shaping yet).

## Feature parity with boat.dev

| Feature | Here | Notes |
|---|---|---|
| Create / delete VMs | yes | `POST /v1/vms`, `boat new`; sizes and images from the module |
| Stop / start | yes | the disk persists across stops |
| Pause / resume (memory) | yes | Firecracker pause/resume and memory snapshots (boatd) |
| Fork | yes | `POST /v1/vms/{id}/fork`; btrfs reflink, near-instant |
| Snapshots | yes | create/list/delete; memory + disk (boatd restores from them) |
| ssh | yes | ProxyJump via `ssh.<domain>:2222` |
| scp / files | yes | `boat scp` via the files API; real `scp -J` works too |
| Ports (public URLs) | yes | `https://<port>-<vmid>.vm.<domain>` |
| Public IPs | partial | IPv6 per VM from `ipv6Prefix`; IPv4 only from `publicIPv4Pool`, otherwise NAT |
| Desktop (browser) | yes | KasmVNC in the `full` image, signed links |
| Prompt (run an agent) | yes | `POST /v1/vms/{id}/prompt` |
| CLI with JSON | yes | `--json` |
| REST API | yes | `components/boat/api/openapi.json` |
| SDKs | yes | `components/boat/sdk` |
| Webhooks | yes | signed (`X-Boat-Signature`), delivery log, test |
| API keys, scopes | yes | |
| Secrets | yes | sealed at rest, injected into VMs as env vars |
| Teams, quotas | yes | default quota from `services.boat.quotas` |
| Auto-stop on idle / TTL | yes | `idle_stop_s`, `ttl_s` per VM |
| Metering | yes | `GET /v1/usage`: VM-seconds (per size), snapshot-days, per team |
| Billing | no | metering only; no payments, invoices or plans |
| Regions | N/A | one server is one region; run several servers for more |

## Honest gaps

- **One host.** No live migration, no cross-host scheduling and no HA. If the box dies, its VMs go down with it. Back up `/var/lib/boat` with `btrfs send` of snapshots.
- **No nested KVM inside guests.** Docker works in VMs; KVM in VMs does not.
- **No GPUs.** Firecracker has no PCI passthrough.
- **SSH UX** is `-J jump@... dev@<ip>` (wrapped by `boat ssh`), not `ssh <vmid>@host`.
- **HTTP-01 fallback** limits: CT-log exposure, Let's Encrypt rate limits, and a first-request delay.
- **Port URLs are public.** There is no per-port auth option yet.
- **No bandwidth or IOPS limits per VM** yet; CPU and memory are limited by size.
- **Guest images** must be published (URL + sha256) before `boat-images` has anything to fetch. Until then, build them with `components/boat/guest/build.sh` and copy them to `/var/lib/boat/images/<id>/`.
- **Metrics:** node-exporter (:9100, not opened in the firewall) covers the host. There is no per-VM Prometheus endpoint; `GET /v1/usage` has the per-VM numbers.

## Integration notes

- **Flake.** Wired: `services.boat` is in `nixosModules.default` (off unless enabled), `nixosModules.server`, `nixosConfigurations.server`, `packages.{boatd,boat-api,boat,boat-gateway}`. The ISOs do not ship the server profile yet: install `headless`, then switch to `server` (import `nixosModules.server` in `/etc/pandaos/flake.nix`).
- **Tests.** `checks.boat-module` (`components/boat/vmtest.nix`: stub boatd/boat-api, real gateway, Caddy and sshd) and `checks.boat-packages` (Nix builds, so `go test`, of the four Go packages). The `boat-e2e` workflow runs `components/boat/e2e.sh` on a KVM runner: real guest image, boatd + boat-api + boat-gateway as processes, every feature through the `boat` CLI.
- **Health.** Consider adding `boat-api.service`, `boat-gateway.service` and `caddy.service` to `panda-health`'s units in `nixos/modules/boot.nix`. Leave out `boatd`: on a box without KVM it fails on purpose.
- **Installer** (`panda-install --profile server`): write `services.boat.domain` and `acmeEmail` into `local.nix` from `--domain`/`--acme-email`. For optional `--data-disk DEV`: `mkfs.btrfs -L PANDADATA DEV`; no fstab edit is needed, because the profile mounts the label with `nofail`. The installer's `disk.nix` can pin it by UUID like the other mounts.
