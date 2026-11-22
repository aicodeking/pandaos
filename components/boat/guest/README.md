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
`console=ttyS0 quiet reboot=k panic=1 pci=off root=/dev/vda rw ip=<guest>::<gw>:<mask>::eth0:off:<dns>`.
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
| boat CLI (`/usr/local/bin/boat`, and `host` as a symlink to it) | yes | yes |
| pi 1.5.0, Kimi Code 2.4.1, Prime Agent 0.10.2, Mistral Vibe 3.0.4 (`/opt/boat-harness`) | no | yes |
| XFCE 4.18 + xfce4-terminal, Ghostty 1.3.1, KasmVNC 1.5.2, libav (H.264), Mesa llvmpipe, glxgears, xdotool | no | yes |
| Google Chrome stable, code-server 4.144.0 | no | yes |
| Languages boat.dev lists as pre-installed (below) | no | yes |
| `user` as a second name for uid 1000, `/home/user -> /home/dev` | no | yes |

Languages and tools in `full`, matching docs.boat.dev/machines/tools (measured there
2026-10-13). Ubuntu packages where noble has the documented version, otherwise a
release artifact pinned by sha256 in the Dockerfile:

| Tool | Version | Source |
|---|---|---|
| `deno` | 2.9.4 | GitHub release zip |
| `pnpm` | 10.34.5 | npm (global) |
| `nvm` + Node 22 | nvm 0.41.0, Node 22.23.4 in `/usr/local/nvm` | tarballs; `nvm use 22`, the default `node` stays 26.13.0 |
| `uv` / `uvx` | 0.12.0 | GitHub release tarball |
| `go` / `gofmt` | 1.23.4 | go.dev tarball, `/usr/local/go` |
| `java` / `javac` | OpenJDK 11 | Ubuntu `openjdk-11-jdk-headless` (OpenJDK 21 is kept out) |
| `mvn` | 3.8.7 | Ubuntu `maven` |
| `gradle` | 8.12 | services.gradle.org zip, `/opt/gradle-8.12` |
| `kotlin` / `kotlinc` | 2.1.0 | GitHub release zip, `/opt/kotlinc` |
| `scala` / `scalac` / `sbt` | Scala 3.3.8, sbt 1.13.0 | GitHub release tarballs, `/opt/scala3`, `/opt/sbt` |
| `ruby` / `gem` / `bundle` | 4.0.1 | built from the ruby-lang.org tarball in the `ruby-build` stage, `/opt/ruby` |
| `php` / `composer` | PHP 8.4.23, Composer 2.11.2 | static-php-cli `common` build; composer.phar |
| `erl` / `elixir` | Erlang/OTP 25, Elixir 1.14.0 | Ubuntu `elixir` |
| `dotnet` | .NET SDK 8.0.1xx | Ubuntu `dotnet-sdk-8.0` |
| `R` | 4.6.1 | CRAN's noble `r-base-core` .deb |
| `clang` / `cmake` / `ninja` | Clang 18, CMake 3.31.3, ninja 1.11.1 | Ubuntu; CMake is Kitware's tarball, `/opt/cmake` |
| `ffmpeg`, `wmctrl` | 6.1.1, 1.07 | Ubuntu |

Not matched: PHP is 8.4.23, not 8.4.24 (static-php-cli has no 8.4.24 build).
.NET and OpenJDK 11 follow Ubuntu's security updates, so their patch versions move.
boat.dev's default Node is 24 and it keeps apt's Node 20; ours stays Node 26 (slim
is unchanged), with Node 22 under nvm. The coding agents boat.dev lists beyond the
seven harnesses (hermes, openclaw, cursor-agent, lux) are not in the image.

Exact versions of everything land in `out/manifest-<variant>.txt`; sizes in
`out/size-<variant>.txt` (`content_mib`, `image_mib`, `zst_mib`).

Sizes (MiB, CI build):

| | content | raw ext4 | zstd -10 |
|---|---|---|---|
| slim, before trimming | 3027 | 4144 | 907 |
| full, before trimming | 4548 | 5823 | 1353 |
| slim, before the boat CLI | 2821 | 3065 | 842 |
| full, before the four extra harnesses | 4508 | 4803 | 1348 |
| slim, now (boat CLI) | 2828 | 3072 | 844 |
| full, before the boat.dev languages | 5189 | 5510 | 1489 |
| full, now (+ the boat.dev languages and tools above) | 7659 | 8048 | 2439 |

Trimming: tight ext4 packing (above); dpkg excludes for docs/man/info/locales;
opencode's unused `x64-baseline` and musl binaries (npm installs both CPU variants,
214 MB each; postinstall hardlinks the chosen one into `bin/`); rustup's download
and tmp dirs and root's caches. Rust stays on the `minimal` profile. There is one
Node toolchain (code-server's bundled Node excepted). The codex binary is already
stripped. The build log prints the 40 largest directories for the next round.

Choices:
- **Harnesses:** `full` has all seven `boat.dev` prompt harnesses (`claude`, `codex`,
  `opencode`, `pi`, `prime`, `kimi`, `mistral`, plus aliases `claude-code`,
  `prime-agent`, `kimi-code`). `slim` has `claude`, `codex` and `opencode`, and stays
  small. `pi` and Kimi Code install with `npm ci` from the lockfiles in
  `rootfs-full/opt/boat-harness`, and Mistral Vibe (`vibe`) with `pip --require-hashes`
  from its lock there, so every dependency is pinned by checksum. Prime Agent is its
  sha256-pinned release tarball; it may fetch `uv` and Python on first use.
  `test-in-vm.sh` checks `--version` only, because CI has no model keys: none of
  `pi`, `prime`, `kimi` or `mistral` has run a real prompt here, though `boat prompt`
  (sync `/v1/vms/{id}/prompt`, async `/v1/vms/{id}/prompts`, and
  `/api/v1/sandboxes/{id}/prompt`) and `boat exec` accept all seven. Other coding-agent
  CLIs (`gemini`, `droid`, `amp`, `goose`, `qwen`, `kilocode`, `factory`, `crush`,
  `hermes`, `openclaw`, `cursor-agent`, `lux`) are not pre-installed in the image or
  wired into `boat prompt` (install inside the VM and run via `boat exec`).
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
Managed keys are authorized online for each new authentication by the root-owned
`/usr/local/libexec/boat-ssh-authorize` Python stdlib helper, executed with `python3 -I`
as dedicated nologin user `boat-ssh`. Only an explicitly enrolled tenant key file,
`/home/dev/.ssh/authorized_keys.tenant`, provides independent tenant access. Enroll
that file through an authenticated guest command with mode 0600 and owner dev;
never write managed grants there. Tenant/root authority is independent of managed
revocation. Legacy `authorized_keys` and `authorized_keys2` are ignored. The installer
preserves their unknown bytes in root-only `/etc/boat/ssh-legacy-archive` and never
promotes them to tenant keys. CA keys, authorized-principals commands, password,
keyboard-interactive and hostbased authentication are disabled.

The API provisions `/etc/boat/ssh-auth.json` (root:boat-ssh, 0640, protected root-owned
parents) independently of tenant environment or `BOAT_API_KEY`. Its exact v1 fields
are `version`, `url` (full HTTPS `/v1/vms/{vm_id}/ssh/authorize` endpoint), `token`
(lookup-only bearer), `vm_id`, `team_id`, `generation`, `boot_epoch`, and `users`
(explicit OS usernames). The helper accepts only installed dev/user accounts; the
API may authorize a restrictive subset. Both aliases are verified in effective sshd
policy when installed. No config, no verified HTTPS, malformed/partial/oversized
response, non-200 status, or a deadline failure means empty stdout and denied managed
authorization. Requests have a 500ms socket timeout and a hard two-second total
budget including DNS, TLS and complete body buffering (64 KiB maximum). The helper
uses OpenSSL's hashed `/etc/ssl/certs` directory (`capath`) first for `O(1)` issuer
lookup and falls back once to `/etc/ssl/certs/ca-certificates.crt` (`cafile`) only if
`/etc/ssl/certs` is absent or the handshake raises `SSLCertVerificationError` (such as
an unhashed custom root appended directly to `ca-certificates.crt`), with direct HTTPS
and without environment `SSL_CERT_*` variables, proxies, redirects, retries, caches,
shell evaluation, or static-key fallback.

The host installs exact helper bytes as root:root 0555, then runs
`/usr/bin/python3 -I /usr/local/libexec/boat-ssh-authorize install` through existing
root guest exec, with the config JSON on stdin. Before any policy file upload or
root exec, boatd must require the health capability `trusted_root_exec:true` (literal
boolean); older agents silently ignore unknown request fields and must stay
quarantined. Install, verify and `rotate-token` exec requests carry explicit
`user:"root"` and `trusted_root_exec:true`. This reserved host-only mode builds
exactly fixed `PATH=/usr/sbin:/usr/bin:/sbin:/bin`, `HOME=/root`, `USER=root`,
`LOGNAME=root`, `LANG=C`, `SHELL=/bin/sh` before spawning the
executable, ignores inherited/tenant environment, clears supplementary groups,
uses `/root` as cwd, and rejects request environment, tty or cwd overrides. Public
API routes must never forward this selector or elevate the requested user. `verify`
and `rotate-token` accept the same stdin. All three emit
`{version:1,config_digest,boot_epoch,readiness:true}` only on successful
checks; `config_digest` is SHA256 of ASCII JSON with sorted keys, compact separators,
no newline, and array order preserved. All config strings are ASCII. The installer
closes `ssh.socket`, stops old sshd and its preauth/session processes (pidfd-based
signals avoid numeric-PID reuse; absent kernel/Python support fails closed), archives the
old config, installs an authoritative sshd config without legacy Includes, checks
`sshd -t` and `sshd -T -C` for every installed alias, and verifies the restarted
service. Errors do not restore the previous static policy. Installation can end
existing SSH sessions; the host must keep ingress quarantined until attestation. When
only the lookup token/generation in `/etc/boat/ssh-auth.json` changes on an already
provisioned VM, `rotate-token` verifies `boat-ssh`, `sshd_config`, the `ssh.service`
drop-in, helper permissions and active `ssh.service` state (failing closed on any
divergence) and atomically updates `/etc/boat/ssh-auth.json` (`0640` `root:boat-ssh`)
without stopping `ssh.service`, killing active `sshd` sessions, or invoking `sshd -T`.

For existing guests, use a maintenance window: close guest/direct/jump ingress,
suspend access, install and independently verify every authority slice, and reopen
only after host identity/digest/epoch fences pass. Rollback keeps access suspended;
it must never restore legacy static authorization. This guest slice alone does not
establish system readiness. No deployment or VM execution is implied by these
source changes.

Run local helper tests with `python3 -B test_ssh_authorize.py`. They include real
TLS failure/slow-stream probes and local `sshd -t/-T` with temporary host keys when
available. Full root/systemd install and actual login/legacy/tenant enforcement
must also be exercised on Linux; the macOS parser harness does not establish them.

`user` (full only): boat.dev's guest user is `user` in `/home/user`. In `full`,
`user` is a second passwd entry for uid 1000 (`useradd -o`, group `dev`, same groups)
whose home is `/home/user`, a symlink to `/home/dev`. Everything stays dev's: files,
`whoami` and `id -un` (uid lookups find `dev` first), the agent's default exec user,
and every `/home/dev` path. `ssh user@...` works with explicitly enrolled tenant keys (and managed
keys only when `user` is listed in the trusted online config)
(`rootfs-full/etc/ssh/sshd_config.d/boat-user.conf` adds `AllowUsers user`),
`exec` with `"user": "user"` runs with `HOME=/home/user`, and both have sudo.

## Desktop: KasmVNC

KasmVNC over Selkies-GStreamer: WebRTC media needs UDP or a TURN server, which a
host HTTP reverse proxy and a signed URL cannot carry. KasmVNC is one HTTP + WebSocket
port, so the host proxies it like any other URL.

**1080p60 is not what we measure on CI.** Numbers from `desktop-browser-fps.py`:
headless Chrome through the real link and web client at 1920x1080, on a 4-core GitHub
runner that also hosts the VM, with software rendering (llvmpipe), software encoding
and software decoding. "Shown" counts paints where the canvas pixels changed, which is
what a viewer sees. "Decoded" counts WebCodecs frames, and is 0 in image mode. Ranges
over five CI runs (41326922291, 41328850520, 41332202705, 41333827937, 41336650348),
which vary by runner; glxgears was the source and was not the limit.

| 1080p desktop, 2 vCPU VM, direct to the guest | shown fps | decoded fps | Mbps |
|---|---|---|---|
| Fullscreen motion, stock KasmVNC client settings | 18.4-23.8 | - | 6.5-8.3 |
| Fullscreen motion, preset 3, JPEG/WebP | 20.8-25.6 | - | 7.2-8.9 |
| **Fullscreen motion, preset 3, H.264 (default)** | **26.6-30.0** | 34.4-36.4 | 19.9-20.9 |
| 400x400 window moving, preset 3, JPEG/WebP | 51.2-59.2 | - | 3.5-3.8 |
| **400x400 window moving, preset 3, H.264 (default)** | **40.3-47.0** | 54.5-58.3 | 8.5-9.0 |

Through the TLS edge (Caddy + gateway, default-size VM), default H.264, fullscreen:
**34.0-41.9 shown, 47.2-53.9 decoded, 25.4-28.4 Mbps.**

Earlier versions of this file claimed 44-62 fps. That was wrong: it counted raw 8bpp
RFB updates at the server, not what a browser shows, and the range was too generous
even for that. `desktop-check.sh` still prints that rate as `desktop_raw_8bpp_fps`
(16-19 in fullscreen).

Why these settings:
- The stock client asks for 24 fps at 960x540 in video mode: its `video_quality` 2
  preset overrides the server's `-FrameRate 60`. `boat-defaults.js` seeds preset 3
  (60 fps, 1080p) before the client starts. A choice saved in the browser wins.
- **Encoder: software H.264** (`-videoCodec h264`, client `stream_mode` -1027, x264
  `ultrafast`), decoded by WebCodecs. KasmVNC dlopens `libavcodec.so` and has no H.264
  without it, which costs about 105 MiB of the full image. H.264 beats JPEG/WebP on
  fullscreen motion, and a user's machine usually decodes it in hardware, which CI
  cannot show. The costs: 2.5x the bandwidth and softer text than image mode, and fewer
  shown frames in the small-window case above. The client's settings panel can switch
  to image mode, and it falls back by itself if the browser cannot decode H.264.
- Lowering the source's priority, the client's stream-quality setting and a tuned
  JPEG profile moved fullscreen by no more than a few fps, so none are used.
- The limit looks like CPU. Decoded fps was 34-36 on 2 vCPU and 47-54 on a
  default-size VM, on a runner with no spare cores. More cores or a VA-API GPU should
  help; neither is measured here.
- Not WebRTC: Selkies would use the same x264 on the same cores, and needs TURN and
  gateway support that the host does not have. It was not built or measured. KasmVNC's
  `enable_webrtc` UDP path needs a UDP route to the guest, which is also absent.

The gates: `desktop-check.sh` fails below 20 shown fps, below 25 decoded fps or under
1920 wide (`MIN_BROWSER_FPS`, `MIN_DECODED_FPS`). Those catch a real regression: stock
settings decode nothing, and losing H.264 does the same. They are not quality targets.

Auth: no RFB or HTTP auth on Xvnc (`-SecurityTypes None -disableBasicAuth`). Its ports
(5901, 6081) listen on loopback only, and anything that can reach those already runs
inside the VM, where `dev` has sudo. The way in is the agent's proxy, which requires
the cookie on every connection including the websocket (a query token only buys the
cookie redirect) and rejects any `Origin` that does not match `Host`.

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
| `exec` | `argv, env?, cwd?, stdin_b64?, timeout_s? (600), tty?, user? ("dev"), session?` | `{stream, data_b64}`... then `{exit, signal?, timed_out?}` |
| `exec_attach` | `session, seen, exec?` | `{attached, started?}`, then the session's lines from `seen` on (PROTOCOL.md) |
| `put_file` | `path, data_b64, mode? ("0644"), offset?, last? (true), owner?` | `{ok, size, complete}` |
| `get_file` | `path` | `{offset, data_b64}` (1 MiB each)... then `{eof, size, mode}` |
| `set_identity` | `hostname, ipv4_cidr, gw, dns, mac?, ipv6_cidr?, ipv6_gw?, defer_ssh_restart?` | `{ok, ms: {network, secrets}}` |
| `set_env` | `vars` | `{ok, count}` |
| `desktop` | `action: start\|stop\|status, token?` | `{ok, port, token, path}` / `{ok}` / `{ok, running, port}` |
| `shutdown` | | `{ok}`, then the VM reboots and Firecracker exits |
| `reseed` | `entropy_b64` (32-4096 bytes), `generation?` | `{ok, bytes}` |
| `sync` | | `{ok}` after `sync(2)`, i.e. every filesystem flushed (boatd calls it before pause) |
| `cmd_start` | `argv, env?, cwd?, user? ("dev")` | `{ok, cmd_id}` |
| `cmd_list` | | `{ok, cmds: [status...]}` |
| `cmd_status` | `cmd_id` | `{ok, cmd_id, argv, state, running, pid, started, ended?, exit?, signal?}` |
| `cmd_logs` | `cmd_id, offset? (0), limit? (1 MiB, max 4 MiB)` | `{ok, data_b64, offset, next_offset, size, truncated, running, exit, signal}` (`exit` and `signal` are `null` while the command runs or when not applicable; a guest from before they were added omits them, so their presence is the capability signal, as in PROTOCOL.md) |
| `cmd_signal` | `cmd_id, signal` ("TERM", "SIGTERM" or a number) | `{ok}` |
| `file_list` | `path` | `{ok, entries: [{name, type, size, mode, mtime}], truncated}` |
| `file_delete` | `path, recursive? (false)` | `{ok}` |
| `file_move` | `from, to, overwrite? (false)` | `{ok}` |
| `file_search` | `path, query, limit? (100, max 1000), content? (true), regex? (false)` | `{ok, matches: [{path, line?, text?}], truncated}` |

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
- `set_identity` restarts `ssh.service` synchronously when it is active (or failed in
  standalone SSH mode) before replying so regenerated host keys take effect without
  racing a subsequent `boat-ssh-authorize install`; passing `defer_ssh_restart: true`
  skips that restart when the host is about to run `boat-ssh-authorize install` before
  releasing network quarantine.
- Host keys are ed25519 and ecdsa only. RSA keygen took a random 59-555 ms per key on
  an M-series Mac (the other two: ~25 ms), which showed up as `identity=349-1208ms`
  on forks in CI. With RSA gone and sshd no longer awaited, one CI run measured
  `identity` at 0-326 ms (mostly under 100 ms) and a fork at 0.90 s. No RSA host key
  means a client that only accepts RSA can't connect.
- `cmd_*`: a detached command runs under its own double-forked supervisor, so an agent
  restart doesn't touch it, and a snapshot keeps it like any process. Everything lives
  in `/var/lib/boat/cmds/<cmd_id>` (root only). The start spec, which holds the
  environment, is deleted once started. The env is the same as `exec`: `/etc/boat/env`,
  then the request's `env`. `state` is `running`, `exited` or `lost` (its supervisor is
  gone, for example after a reboot). Logs merge stdout and stderr and keep the newest
  32-64 MiB; offsets are absolute, and an offset that rotated away moves forward with
  `truncated: true`. The newest 100 finished commands are kept. A running command holds
  `idle_s` at 0. `cmd_signal` signals the process group and refuses once the command
  has exited, so background children it left behind can't be signalled this way.
- `file_*` run as root, like `put_file`. `file_delete` does not follow symlinks and
  refuses `/` and top-level system directories. `file_move` treats `to` as the exact
  destination. `file_search` uses ripgrep (so `.gitignore` applies): a fixed string by
  default; `content: false` matches a case-insensitive substring of file names.
- `set_env` atomically replaces `/run/boat/env` (`export K='v'` lines), 0600, owned by `dev` so
  that dev's login shells (`/etc/profile.d/boat-env.sh`) and bash (`/etc/bash.bashrc`)
  can source it through the compatibility symlink `/etc/boat/env`. Exec, detached
  commands, `get_env`, and existing self-API smoke scripts use the same values.
- `idle_s` is 0 while an exec runs or anyone is logged in (`who`, i.e. utmp: ssh
  sessions), else the time since the last exec/put_file/desktop start, or since the
  last desktop input (`xprintidle` on `:1`) if that is more recent.

### Managed environment across boots

New slim/full images keep the managed environment on the guest's per-boot `/run`
tmpfs. A cold boot starts without that file; the host must push the current
environment before releasing access. Restarting the agent or preparation service
within a boot preserves a freshly pushed environment. Persistent `identity.json`,
VM identity restoration and reseed/adoption operations retain their existing behavior.

The root-owned helper `/usr/local/lib/boat/prepare-managed-env` runs as
`/usr/bin/python3 -I /usr/local/lib/boat/prepare-managed-env`. It discards the old
`/etc/boat/env` regular file and `env.boat-tmp` without reading or executing them,
installs the compatibility link, verifies `/run` is tmpfs, and prepares root-owned
0755 `/run/boat`. `boat-managed-env.service` is required before `sysinit.target`
and by `boat-agent.service`, so preparation failure blocks normal service startup.
The Docker build calls the same helper with `--image-setup` to discard a legacy
managed file and install the link without requiring a mounted runtime tmpfs.

For an existing supported guest, install the updated agent, helper and both units
as root during an access-suspended maintenance window, enable
`boat-managed-env.service`, reload systemd and run the helper before restarting the
agent or allowing any environment consumer. Verify the symlink, tmpfs and empty
runtime environment before pushing fresh values. Enabling the unit alone does not
retroactively order already-running services. This is a guest installation
interface; no automatic host migration is supplied here.

This guarantee requires the updated guest code and enabled early unit. Preexisting
snapshots/images without them remain legacy. Memory snapshots can retain tmpfs and
process environments; tenant copies, command data/logs and prior disk blocks or
snapshots are not erased by unlinking the old file. This change removes the managed
file from future persistent writes; it does not sanitize historical storage.

## Tests

- `cd agent && python3 -B -m unittest -v test_boat_agent`: 78 tests including socketpair protocol checks
  (exec streaming/exit/stdin/env/cwd/tty/3 MB output/timeout + group kill,
  set_env, chunked put/get, validation, reseed gating set_identity, sync, the
  desktop token check and proxy, and `desktop-check.sh` end to end through the
  real token proxy to a fake KasmVNC), plus managed-env migration, tmpfs validation,
  atomic publication and failed-write cleanup.
- `./desktop-check.sh LINK [SECONDS]`: follows the link's redirects and meta
  refreshes with curl and a cookie jar (`$CURL_OPTS` is added; `--resolve`, `--connect-to`, `--cacert`
  and `-k` also apply to the websocket, as does `$SSL_CA`). It then opens
  `<page>/websockify`, does the RFB handshake (security None, raw 8bpp), and passes
  on the first FramebufferUpdate. It counts non-empty updates for SECONDS and prints
  `METRIC desktop_first_frame_ms` and `METRIC desktop_raw_8bpp_fps`, then runs
  `desktop-browser-fps.py` if Chrome is present. `$ANIMATE_CMD` loops in the
  background, e.g. `boat exec VM -- boat-desktop-animate 10`
  (glxgears on llvmpipe, with an xdotool-moved window as fallback).
- `./desktop-browser-fps.py LINK [SECONDS]`: the browser measurement described under
  Desktop. Needs Chrome or Chromium; `$CURL_OPTS` becomes Chrome's resolver rules.
- `./test-in-vm.sh [vmlinux] [rootfs.ext4]`: needs `/dev/kvm` and sudo (tap + NAT).
  Downloads Firecracker v1.17.0 (sha-pinned) unless `FIRECRACKER` is set, boots the
  image with an 8 GB disk (or the image size + 3 GB, if more; `DISK_GB` overrides), and checks health, exec (user, exit code, timeout),
  disk growth, a 3 MiB put/get, ssh key login + sudo and sshd's password/root
  settings, set_env in exec and login shells, vmgenid bound, set_identity refused
  without a reseed and with a used-up one, then hostname, machine-id, every host key
  type, DNS and ssh afterwards, sync, `docker run hello-world`, the agent CLIs and
  tool versions (full: every language and tool in the table above, OpenJDK 11 as
  `java`, Node 22 under nvm with Node 26 still the default, and the `user` alias over
  exec and ssh), idle_s, the desktop (403 without the token, 302 + cookie, page
  served, 6081 not reachable from the host, `desktop-check.sh` fps with glxgears,
  the Ghostty llvmpipe check, code-server on 127.0.0.1:8080; slim: refused), and
  shutdown and a second cold boot of the same disk: managed and legacy environment
  files absent, identity preserved, login environment empty, and fresh `set_env` usable.
