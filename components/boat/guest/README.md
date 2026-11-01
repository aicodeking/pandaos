# boat guest: image, agent, desktop

The VM side of boat: a Firecracker `vmlinux`, an Ubuntu 24.04 ext4 root disk
(`full` and `slim`), the vsock guest agent, and a browser desktop.

```
build.sh          kernel + rootfs build (docker only; privileged steps run in containers)
Dockerfile        rootfs contents; targets `slim` and `full`
kernel/docker.config   options merged on top of Firecracker's 6.1 guest config
agent/boat-agent.py    the guest agent (python3 stdlib), + test_boat_agent.py
rootfs/, rootfs-full/  files copied into the image (units, sshd, sudoers)
test-in-vm.sh     boots the image in Firecracker and checks the whole contract
```

## Build

```sh
./build.sh all                 # out/vmlinux, out/rootfs-{slim,full}.ext4[.zst], manifests, SHA256SUMS
./build.sh kernel              # kernel only (cached in out/kernel-cache by input hash)
./build.sh rootfs slim         # one variant
BOAT_GUEST_RELEASE_URL=https://github.com/<org>/<repo>/releases/download/<tag> ./build.sh fetch
```

Requirements: an x86_64 host with docker + buildx. No sudo: the ext4 is made with
`mkfs.ext4 -d` inside a container, so file ownership survives.

### GitHub Actions (for the integration worker)

```yaml
- uses: docker/setup-buildx-action@v3
- uses: actions/cache@v4            # kernel: ~10 min cold, seconds warm
  with: { path: pandaos/components/boat/guest/out/kernel-cache, key: boat-kernel-${{ hashFiles('pandaos/components/boat/guest/build.sh', 'pandaos/components/boat/guest/kernel/*') }} }
- run: ./build.sh all
  working-directory: pandaos/components/boat/guest
  env: { BUILDX_CACHE_ARGS: "--cache-from type=gha --cache-to type=gha,mode=max" }
- run: python3 -m unittest -v test_boat_agent
  working-directory: pandaos/components/boat/guest/agent
- run: sudo chmod 666 /dev/kvm && ./test-in-vm.sh out/vmlinux out/rootfs-slim.ext4 && ./test-in-vm.sh out/vmlinux out/rootfs-full.ext4
  working-directory: pandaos/components/boat/guest
- uses: actions/upload-artifact@v4  # or attach to a release; `build.sh fetch` consumes it
  with: { name: boat-guest, path: "pandaos/components/boat/guest/out/{vmlinux,vmlinux.config,rootfs-*.ext4.zst,manifest-*.txt,size-*.txt,SHA256SUMS}" }
```

Docker layer caching (`type=gha`) makes a rebuild with no Dockerfile change a
few minutes long. If the full build is too slow for every PR, build on
`main`/tags only, publish a release, and use `./build.sh fetch` elsewhere.

## Kernel

Linux **6.1.191** (sha256-pinned tarball from kernel.org) built from Firecracker's
`resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config` at commit
`7614ab4d` (sha256-pinned), with `kernel/docker.config` merged on top. No modules
(`CONFIG_MODULES` is off in that config), so everything is `=y`. The build fails if
any required option is missing from the final `.config` (shipped as `out/vmlinux.config`).

Already in the Firecracker config: virtio-net/blk/vsock, overlayfs, fuse, cgroup v2
controllers (memory, pids, cpuset, bpf, devices, io, cfs bandwidth), all namespaces,
seccomp, bridge + br_netfilter, veth, vxlan, tun, iptables and nftables NAT,
xt addrtype/conntrack/MASQUERADE, `IP_PNP` (for `ip=`).

Gaps in the Firecracker config that `docker.config` fills: `NFT_MASQ`, `NFT_REDIR`,
`NFT_FIB_IPV4`, `IP_VS` (+rr, nfct, xt ipvs; swarm/overlay service VIPs),
`MACVLAN`, `IPVLAN`, `DUMMY`, `CGROUP_MISC`, `CRYPTO_USER_API_HASH`.

Required for fork safety and checked by the build: `ACPI` + `VMGENID`. Firecracker
(>= 1.10, x86_64) exposes a VMGenID ACPI device; 1.17 fixed its HID so the 6.1
driver binds (`test-in-vm.sh` checks `/sys/bus/acpi/drivers/vmgenid/`). On restore
the driver mixes the new generation ID into the CRNG. On 6.1 it gives userspace no
signal (no uevent, no sysfs value; those came in later kernels), so the agent
cannot detect a restore by itself. That is why `reseed` exists (see below).

Gaps left on purpose: `KVM` (no nested VMs), AppArmor/SELinux (docker runs
without an LSM profile; the VM is the boundary), modules (`modprobe` warnings from
docker/iptables are harmless).

Host kernel cmdline:
`console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw ip=<guest>::<gw>:<mask>::eth0:off:<dns>`.
`reboot=k` matters: Firecracker has no ACPI poweroff, so the `shutdown` op reboots
and Firecracker exits.

## Image

Ubuntu 24.04 (digest-pinned base). systemd is init; cloud-init is not installed.
Interface naming is off (`99-default.link -> /dev/null`) so the NIC is `eth0`.
`/etc/resolv.conf -> /proc/net/pnp` until `set_identity` writes a real one.
No ssh host keys or machine-id are baked in; they are made at first boot.
`boat-growfs.service` runs `resize2fs /dev/vda` every boot (idempotent; forks may
get bigger disks).

Packing: the exported tree goes into an ext4 sized to content + 3% + 160 MiB
(64 MiB journal, no reserved blocks, inodes = files x 1.2 + 16k), growing 5% per
retry if mkfs runs out of room. Then `zerofree` and `e2fsck -E discard` keep free
blocks zero and sparse. `resize2fs -M` was dropped: its minimum estimate equals the
full size (`resize2fs -P`), so it never shrank anything. That was ~1.1 GB of the
old slim image.

| | slim | full |
|---|---|---|
| docker.io + compose v2, git, gh, build-essential, python3, tmux, ripgrep, jq, openssh-server | yes | yes |
| Node.js 26.13.0 LTS + npm, Bun 1.4.4, rustup 1.29.2 (stable, minimal, for `dev`) | yes | yes |
| claude-code 2.1.316, codex 0.166.0, opencode 1.19.5 (npm, global) | yes | yes |
| XFCE 4.18 + xfce4-terminal, Ghostty 1.3.1, KasmVNC 1.5.2, Mesa llvmpipe, glxgears, xdotool | no | yes |
| Google Chrome stable, code-server 4.144.0 | no | yes |

Exact versions of everything land in `out/manifest-<variant>.txt`; sizes in
`out/size-<variant>.txt` (`content_mib`, `image_mib`, `zst_mib`).

Sizes (MiB, CI build):

| | content | raw ext4 | zstd -10 |
|---|---|---|---|
| slim, before trimming | 3027 | 4144 | 907 |
| full, before trimming | 4548 | 5823 | 1353 |
| slim, now | 2821 | 3065 | 842 |
| full, now (with Ghostty + Mesa added) | 4406 | 4698 | 1313 |

Trimming: tight ext4 packing (above); dpkg excludes for docs/man/info/locales;
opencode's unused `x64-baseline` and musl binaries (npm installs both CPU variants,
214 MB each; postinstall hardlinks the chosen one into `bin/`); rustup's download
and tmp dirs and root's caches. Rust stays on the `minimal` profile. There is one
Node toolchain (code-server's bundled Node excepted). The codex binary is already
stripped. The build log prints the 40 largest directories for the next round.

Choices:
- **Ghostty** has no official build for Ubuntu 24.04: Ubuntu packages it from
  26.04, and the snap needs snapd. The full image installs the community noble
  .deb that the Ghostty docs list (mkasberg/ghostty-ubuntu 1.3.1, sha256-pinned).
  It renders with Mesa llvmpipe (measured in CI: GL 4.5, Mesa 25.2.8, LLVM 20.1.2)
  under Xvnc. `test-in-vm.sh` checks that it starts a shell and stays up
  (`METRIC ghostty_llvmpipe_ok`, currently 1). That proves it starts, not how fast
  it renders.
  xfce4-terminal stays the XFCE default terminal.
- **VS Code: code-server only.** The `code-server@.service` unit from the deb is
  installed, not enabled: `sudo systemctl enable --now code-server@dev` (it listens
  on 127.0.0.1:8080 with password auth from `~/.config/code-server/config.yaml`).
  The desktop `code` app (~400 MB more) is not shipped.
- **Chrome** comes from Google's apt repo, so its version is not pinned by the
  Dockerfile; the manifest records it.

User `dev` (uid 1000): passwordless sudo, `docker` group, no password (`*`), ssh
keys only (`PasswordAuthentication no`, `PermitRootLogin no`, `AllowUsers dev`).
Keys go in with `put_file /home/dev/.ssh/authorized_keys`.

## Desktop: KasmVNC

KasmVNC over Selkies-GStreamer: Selkies is smoother on a good link, but WebRTC
media needs UDP or a TURN server, which a host HTTP reverse proxy and a signed URL
cannot carry. KasmVNC is one HTTP + WebSocket port, so the host proxies it like any
other URL. Xvnc runs with `-FrameRate 60` and still does up to 60 fps with WebP/JPEG and dynamic quality, much
smoother than noVNC + x11vnc.

`desktop start` starts `boat-desktop.service` (Xvnc `:1` with XFCE, as `dev`,
web client + websocket on **127.0.0.1:6081 only**) and the agent's token proxy on
**0.0.0.0:6080**. The reply is `{ok, port: 6080, token, path: "/?token=..."}`. The
host can pass its own `token` (16-128 url-safe chars) in the request.

Auth flow: `GET <prefix>/?token=T` gets a 302 to a *relative* location with
`Set-Cookie: boat_desktop=T; HttpOnly; SameSite=Lax`; later requests and the
websocket carry the cookie. A missing or wrong token gets 403. `desktop stop` clears
the token. A host-side path prefix works because the redirect is relative.

## Agent protocol

JSON lines on vsock port 1024 (host: Firecracker UDS, `CONNECT 1024\n`). A
connection can carry many requests in order. An error reply is `{"error": "..."}`
and the connection stays usable.

| op | request | reply |
|---|---|---|
| `health` | | `{ok, uptime, idle_s, version}` |
| `exec` | `argv, env?, cwd?, stdin_b64?, timeout_s? (600), tty?, user? ("dev")` | `{stream, data_b64}`... then `{exit, signal?, timed_out?}` |
| `put_file` | `path, data_b64, mode? ("0644"), offset?, last? (true), owner?` | `{ok, size, complete}` |
| `get_file` | `path` | `{offset, data_b64}` (1 MiB each)... then `{eof, size, mode}` |
| `set_identity` | `hostname, ipv4_cidr, gw, dns, mac?, ipv6_cidr?, ipv6_gw?` | `{ok}` |
| `set_env` | `vars` | `{ok, count}` |
| `desktop` | `action: start\|stop\|status, token?` | `{ok, port, token, path}` / `{ok}` / `{ok, running, port}` |
| `shutdown` | | `{ok}`, then the VM reboots and Firecracker exits |
| `reseed` | `entropy_b64` (32-4096 bytes), `generation?` | `{ok, bytes}` |
| `sync` | | `{ok}` after `sync(2)`, i.e. every filesystem flushed (boatd calls it before pause) |

Additions to the PROTOCOL.md contract (all optional):
- `exec.user` (default `dev`; `"root"` for root). The command runs in its own
  session/process group with `HOME`, `USER`, `PATH` (incl. cargo/bun) set; then
  `/etc/boat/env`, then `env`. On timeout the whole group gets SIGTERM, then
  SIGKILL 2 s later, and the reply has `timed_out: true` and the signal. A killed
  command reports `exit` = 128 + signal. With `tty: true`, output is one `stdout`
  stream; stdin (if any) is followed by ^D.
  After the main process exits, the agent reads for 1 s more and then reports
  `exit`, even if a backgrounded child still holds the pipe. Later output from that
  child is discarded, so `server &` keeps running and doesn't die of SIGPIPE.
- `put_file`: chunks append at `offset` (must equal the bytes received so far) into
  `<path>.boat-part`, which is renamed onto `path` on the `last` chunk. Missing
  parent dirs are created. Owner is the parent directory's owner unless `owner`
  (`user[:group]`) is given, so files under `/home/dev` belong to `dev`.
- `set_identity` sets the MAC (link down/up), the IPv4/IPv6 address and default
  route, `/etc/resolv.conf`, hostname and `/etc/hosts`, a new `/etc/machine-id` and
  new ssh host keys (sshd is restarted). It is saved to `/etc/boat/identity.json`
  and re-applied when the agent starts, so it wins over the cmdline `ip=` on reboot.
- `reseed` credits the bytes to the kernel pool (`RNDADDENTROPY`, entropy = 8 bits
  per byte), then forces a CRNG reseed (`RNDRESEEDCRNG`). Each reseed **arms exactly
  one `set_identity`**, which refuses (`reseed required ...`) unless armed. A
  snapshot captures the parent's disarmed state, so every created, forked or
  restored VM needs a fresh host reseed before its ssh host keys and machine-id are
  regenerated. If both requests carry `generation` (boatd's restore counter), they
  must match. A request that fails validation does not use up the reseed; a
  failure after validation does, so the host reseeds again before a retry.
- `set_env` writes `/etc/boat/env` (`export K='v'` lines), 0600, owned by `dev` so
  that dev's login shells (`/etc/profile.d/boat-env.sh`) and bash (`/etc/bash.bashrc`)
  can source it.
- `idle_s` is 0 while an exec runs or anyone is logged in (`who`, i.e. utmp: ssh
  sessions), else the time since the last exec/put_file/desktop start, or since the
  last desktop input (`xprintidle` on `:1`) if that is more recent.

## Tests

- `cd agent && python3 -m unittest -v test_boat_agent`: 19 tests over a socketpair
  (exec streaming/exit/stdin/env/cwd/tty/3 MB output/timeout + group kill,
  set_env, chunked put/get, validation, reseed gating set_identity, sync, the
  desktop token check and proxy, and `desktop-check.sh` end to end through the
  real token proxy to a fake KasmVNC).
- `./desktop-check.sh LINK [SECONDS]`: follows the link's redirects and meta
  refreshes with curl and a cookie jar (`$CURL_OPTS` is added; `--resolve`, `--connect-to`, `--cacert`
  and `-k` also apply to the websocket, as does `$SSL_CA`). It then opens
  `<page>/websockify`, does the RFB handshake (security None, raw 8bpp), and passes
  on the first FramebufferUpdate. It counts non-empty updates for SECONDS and prints
  `METRIC desktop_first_frame_ms` and `METRIC desktop_fps`. `$ANIMATE_CMD` runs
  first in the background, e.g. `boat exec VM -- boat-desktop-animate 10`
  (glxgears on llvmpipe, with an xdotool-moved window as fallback). Measured in CI
  directly against the guest's token proxy, 2 vCPU, 1920x1080, glxgears at 400x400:
  46.4 fps, first frame 158 ms. Those are raw 8bpp updates, not the browser's encoded
  stream, so they show how fast the server produces frames, not what a user sees.
- `./test-in-vm.sh [vmlinux] [rootfs.ext4]`: needs `/dev/kvm` and sudo (tap + NAT).
  Downloads Firecracker v1.17.0 (sha-pinned) unless `FIRECRACKER` is set, boots the
  image with an 8 GB disk, and checks health, exec (user, exit code, timeout),
  disk growth, a 3 MiB put/get, ssh key login + sudo and sshd's password/root
  settings, set_env in exec and login shells, vmgenid bound, set_identity refused
  without a reseed and with a used-up one, then hostname, machine-id, every host key
  type, DNS and ssh afterwards, sync, `docker run hello-world`, the agent CLIs and
  tool versions, idle_s, the desktop (403 without the token, 302 + cookie, page
  served, 6081 not reachable from the host, `desktop-check.sh` fps with glxgears,
  the Ghostty llvmpipe check, code-server on 127.0.0.1:8080; slim: refused), and
  shutdown.
