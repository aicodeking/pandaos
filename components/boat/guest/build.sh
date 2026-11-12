#!/usr/bin/env bash
# Build the boat guest: vmlinux + rootfs-{slim,full}.ext4.
#
#   ./build.sh kernel          # out/vmlinux (cached by input hash)
#   ./build.sh rootfs [slim|full]   # out/rootfs-<variant>.ext4(.zst) + manifest
#   ./build.sh all             # kernel + slim + full
#   ./build.sh fetch           # download a release instead (BOAT_GUEST_RELEASE_URL)
#
# Needs only docker (x86_64 host). Everything privileged happens in containers.
# Env: OUT (default ./out), JOBS, BUILDX_CACHE_ARGS (e.g. "--cache-from type=gha
# --cache-to type=gha,mode=max"), COMPRESS=0 to skip .zst.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-$HERE/out}
JOBS=${JOBS:-$(nproc 2>/dev/null || echo 4)}
BUILDER_IMAGE="ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852"

KERNEL_VERSION=6.1.191
KERNEL_SHA256=02f15b77d213e15607923832ef9382e2414ea90df6c007f15a910e139645c35d
FC_COMMIT=7614ab4d258ad66eee72a34f2fbf3712c3664c65   # firecracker main, guest_configs
FC_CONFIG_SHA256=153ca1b40f3312bfb40b7587b471ea548a915f98f480f0d0892a1537957a2147

mkdir -p "$OUT"
log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }

build_kernel() {
  local key cache
  key=$( (echo "$KERNEL_VERSION $KERNEL_SHA256 $FC_COMMIT $FC_CONFIG_SHA256"; cat "$HERE/kernel/docker.config" "$HERE/kernel/fix-mballoc.sh") | sha256sum | cut -c1-16)
  cache="$OUT/kernel-cache/vmlinux-$KERNEL_VERSION-$key"
  if [[ -f $cache ]]; then
    log "kernel: cached $cache"
  else
    log "kernel: building linux $KERNEL_VERSION (Firecracker 6.1 config + kernel/docker.config)"
    mkdir -p "$OUT/kernel-cache"
    docker run --rm -i -e KV="$KERNEL_VERSION" -e KSHA="$KERNEL_SHA256" \
      -e FC="$FC_COMMIT" -e FCSHA="$FC_CONFIG_SHA256" -e JOBS="$JOBS" \
      -v "$HERE/kernel:/frag:ro" -v "$OUT/kernel-cache:/out" "$BUILDER_IMAGE" bash -euo pipefail -s <<'EOS'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq && apt-get install -y -qq --no-install-recommends \
  build-essential flex bison bc libelf-dev libssl-dev curl ca-certificates xz-utils python3 >/dev/null
cd /tmp
curl -fsSLo linux.tar.xz "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KV.tar.xz"
echo "$KSHA  linux.tar.xz" | sha256sum -c -
curl -fsSLo fc.config "https://raw.githubusercontent.com/firecracker-microvm/firecracker/$FC/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config"
echo "$FCSHA  fc.config" | sha256sum -c -
tar -xJf linux.tar.xz && cd "linux-$KV"
sh /frag/fix-mballoc.sh
cp ../fc.config .config
scripts/kconfig/merge_config.sh -m .config /frag/docker.config >/dev/null
make olddefconfig >/dev/null
missing=0
for opt in $(grep -oE '^CONFIG_[A-Z0-9_]+' /frag/docker.config) CONFIG_VIRTIO_NET CONFIG_VIRTIO_BLK \
    CONFIG_VIRTIO_VSOCKETS CONFIG_OVERLAY_FS CONFIG_FUSE_FS CONFIG_BRIDGE CONFIG_BRIDGE_NETFILTER \
    CONFIG_VETH CONFIG_NF_NAT CONFIG_IP_NF_NAT CONFIG_NETFILTER_XT_MATCH_ADDRTYPE \
    CONFIG_NETFILTER_XT_TARGET_MASQUERADE CONFIG_NFT_COMPAT CONFIG_MEMCG CONFIG_CGROUP_PIDS \
    CONFIG_CGROUP_BPF CONFIG_USER_NS CONFIG_SECCOMP CONFIG_EXT4_FS CONFIG_IP_PNP CONFIG_DEVTMPFS_MOUNT; do
  grep -q "^$opt=y" .config || { echo "kernel config gap: $opt" >&2; missing=1; }
done
[ "$missing" = 0 ]
make -j"$JOBS" vmlinux >/dev/null
cp .config /out/config-$KV
cp vmlinux /out/vmlinux.tmp
EOS
    mv "$OUT/kernel-cache/vmlinux.tmp" "$cache"
  fi
  cp "$cache" "$OUT/vmlinux"
  cp "$OUT/kernel-cache/config-$KERNEL_VERSION" "$OUT/vmlinux.config" 2>/dev/null || true
}

build_rootfs() {
  local variant=$1 tag="boat-guest:$1" cid img="$OUT/rootfs-$1.ext4"
  [[ $variant == slim || $variant == full ]] || { echo "variant must be slim|full" >&2; exit 2; }
  log "rootfs: docker build --target $variant"
  # shellcheck disable=SC2086
  docker buildx build --load --platform linux/amd64 --target "$variant" -t "$tag" \
    --build-context cli="$HERE/../cli" ${BUILDX_CACHE_ARGS:-} "$HERE"

  log "rootfs: manifest"
  docker run --rm --platform linux/amd64 --entrypoint bash "$tag" -c '
    v() { printf "%-14s %s\n" "$1" "$("${@:2}" 2>&1 | head -1)"; }
    grep PRETTY_NAME /etc/os-release
    v docker   docker --version; v compose docker compose version; v git git --version
    v gh       gh --version; v gcc gcc --version; v python3 python3 --version
    v node     node --version; v npm npm --version; v bun bun --version
    v rustc    su dev -c "~/.cargo/bin/rustc --version"
    v claude   claude --version; v codex codex --version; v opencode opencode --version
    v boat     boat version
    v tmux     tmux -V; v rg rg --version; v jq jq --version; v sshd dpkg-query -W -f=\${Version} openssh-server
    if command -v Xvnc >/dev/null; then
      v kasmvnc dpkg-query -W -f=\${Version} kasmvncserver; v chrome google-chrome --version
      v code-server code-server --version; v xfce4 dpkg-query -W -f=\${Version} xfce4-session
      export HOME=/tmp
      v pi pi --version; v kimi kimi --version; v prime-agent prime-agent --version; v vibe vibe --version
    fi' > "$OUT/manifest-$variant.txt"
  cat "$OUT/manifest-$variant.txt" >&2

  log "rootfs: export -> ext4"
  cid=$(docker create --platform linux/amd64 "$tag" /sbin/init)
  rm -f "$img"
  docker export "$cid" | docker run --rm -i -v "$OUT:/out" -e IMG="/out/rootfs-$variant.ext4" \
    "$BUILDER_IMAGE" bash -euo pipefail -c '
    apt-get update -qq && apt-get install -y -qq --no-install-recommends e2fsprogs zerofree >/dev/null
    mkdir /r && tar -x -C /r --numeric-owner
    # Files docker bind-mounts at runtime come out empty; set them for a VM.
    rm -f /r/.dockerenv /r/etc/resolv.conf
    ln -s /proc/net/pnp /r/etc/resolv.conf      # DNS from the kernel ip= until set_identity
    echo boat > /r/etc/hostname
    printf "127.0.0.1 localhost\n127.0.1.1 boat\n::1 localhost ip6-localhost ip6-loopback\n" > /r/etc/hosts
    : > /r/etc/machine-id
    rm -f /r/etc/ssh/ssh_host_*
    printf "/dev/vda / ext4 defaults,noatime 0 1\n" > /r/etc/fstab
    # /usr is not newer than /etc and /var (ConditionNeedsUpdate): without the stamps
    # every VM ran ldconfig.service on its first boot, 114-127 ms on the agent path
    # (AX42), plus the journal catalog. The format systemd-update-done writes.
    for s in /r/etc/.updated /r/var/.updated; do
      printf "TIMESTAMP_NSEC=%s000000000\n" "$(stat -c %Y /r/usr)" > "$s"; touch -r /r/usr "$s"
    done
    echo "--- largest dirs (MiB) ---" >&2
    du -xm -d3 /r | sort -n | tail -40 >&2
    used=$(du -sm /r | cut -f1)
    files=$(find /r -xdev | wc -l)
    # Tight fit: content + 3% + journal/metadata; grow 5% per retry if mkfs runs
    # out of room. No reserved blocks; first boot grows the fs to the disk.
    # (resize2fs -M overestimates its minimum and never shrank this image.)
    size=$(( used * 103 / 100 + 160 ))
    for try in 1 2 3 4 5 6 7 8 9 10; do
      rm -f "$IMG"; truncate -s "${size}M" "$IMG"
      if mkfs.ext4 -q -F -m 0 -J size=64 -N $(( files * 6 / 5 + 16384 )) -L boatroot \
           -E root_owner=0:0 -d /r "$IMG" 2>/tmp/mkfs.err; then break; fi
      [ "$try" = 10 ] && { cat /tmp/mkfs.err >&2; exit 1; }
      size=$(( size * 105 / 100 ))
    done
    zerofree "$IMG"                              # no stale data in free blocks
    e2fsck -fy -E discard "$IMG" >/dev/null || [ $? -le 1 ]   # punch free blocks: sparse file
    echo "files=$files"
    echo "content_mib=$used"
  ' | tee "$OUT/size-$variant.txt"
  docker rm -f "$cid" >/dev/null
  {
    echo "image_mib=$(( $(stat -c %s "$img" 2>/dev/null || stat -f %z "$img") / 1048576 ))"
  } >> "$OUT/size-$variant.txt"
  if [[ ${COMPRESS:-1} != 0 ]]; then
    log "rootfs: zstd"
    docker run --rm -v "$OUT:/out" "$BUILDER_IMAGE" bash -euo pipefail -c "
      apt-get update -qq && apt-get install -y -qq zstd >/dev/null
      zstd -q -f -T0 -10 /out/rootfs-$variant.ext4 -o /out/rootfs-$variant.ext4.zst"
    echo "zst_mib=$(( $(stat -c %s "$img.zst" 2>/dev/null || stat -f %z "$img.zst") / 1048576 ))" >> "$OUT/size-$variant.txt"
  fi
  cat "$OUT/size-$variant.txt" >&2
}

checksums() {
  (cd "$OUT" && sha256sum vmlinux rootfs-*.ext4.zst 2>/dev/null > SHA256SUMS || true)
}

fetch() {
  local url=${BOAT_GUEST_RELEASE_URL:?set BOAT_GUEST_RELEASE_URL to a release download base URL}
  log "fetch: $url"
  curl -fsSLo "$OUT/SHA256SUMS" "$url/SHA256SUMS"
  for f in $(awk '{print $2}' "$OUT/SHA256SUMS"); do
    curl -fsSLo "$OUT/$f" "$url/$f"
  done
  (cd "$OUT" && sha256sum -c SHA256SUMS)
  for z in "$OUT"/rootfs-*.ext4.zst; do zstd -q -d -f "$z"; done
}

case ${1:-all} in
  kernel) build_kernel ;;
  rootfs) build_rootfs "${2:-full}"; checksums ;;
  all) build_kernel; build_rootfs slim; build_rootfs full; checksums ;;
  fetch) fetch ;;
  *) sed -n '2,12p' "$0"; exit 2 ;;
esac
