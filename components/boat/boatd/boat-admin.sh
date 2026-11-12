# boat-admin: operator commands on this host (root). Set by module.nix (runtimeEnv):
# BOAT_API DATA_DIR IMAGES_DIR MASTER_CRED IMAGE_IDS (declared image ids, comma-separated).
usage() {
  cat >&2 <<'USAGE'
usage: boat-admin init -user NAME -team TEAM [--token-file PATH] [boat-api admin init flags]
         creates the owner's API key; prints boat-api's JSON and writes the bare token
         to PATH (default /root/boat-owner-token, mode 0600)
       boat-admin images import [--id ID] VMLINUX ROOTFS[.zst]
         installs a guest image as IMAGES_DIR/ID/{vmlinux,rootfs.ext4} (default ID: the
         first declared image, ubuntu-24.04 unless services.boat.images/localImages say otherwise)
       boat-admin images list
USAGE
  exit 2
}

need_root() {
  [ "$(id -u)" -eq 0 ] || { echo "boat-admin: run as root (sudo boat-admin ...)" >&2; exit 1; }
}

init() {
  token_file=/root/boat-owner-token
  args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --token-file | -token-file) [ $# -ge 2 ] || usage; token_file=$2; shift ;;
      *) args+=("$1") ;;
    esac
    shift
  done
  out=$(systemd-run --quiet --pipe --wait --collect \
    -p User=boat -p Group=boat \
    -p LoadCredentialEncrypted="$MASTER_CRED" \
    -p RequiresMountsFor="$DATA_DIR" \
    "$BOAT_API" admin init -data "$DATA_DIR/api" "${args[@]}") || exit
  printf '%s\n' "$out"
  token=$(jq -r '.token // empty' <<<"$out")
  if [ -n "$token" ]; then
    (umask 077 && printf '%s\n' "$token" >"$token_file.tmp" && mv -f "$token_file.tmp" "$token_file") ||
      { echo "boat-admin: could not write $token_file" >&2; exit 1; }
    echo "boat-admin: owner token written to $token_file (mode 0600); e.g. BOAT_TOKEN=\$(sudo cat $token_file)" >&2
  fi
  # The running API reads its users at start.
  systemctl try-restart boat-api.service
}

images_import() {
  id=${IMAGE_IDS%%,*}
  id=${id:-ubuntu-24.04}
  while [ $# -gt 0 ]; do
    case "$1" in
      --id) [ $# -ge 2 ] || usage; id=$2; shift 2 ;;
      -*) usage ;;
      *) break ;;
    esac
  done
  [ $# -eq 2 ] || usage
  kernel=$1 rootfs=$2
  [[ $id =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]] || { echo "boat-admin: bad image id '$id'" >&2; exit 2; }
  for f in "$kernel" "$rootfs"; do
    [ -f "$f" ] || { echo "boat-admin: $f is not a file" >&2; exit 1; }
  done
  dest=$IMAGES_DIR/$id
  mkdir -p "$dest"
  tmp=$(mktemp -d "$IMAGES_DIR/.import-$id.XXXXXX")
  trap 'rm -rf "$tmp"' EXIT
  cp "$kernel" "$tmp/vmlinux"
  case "$rootfs" in
    *.zst) zstd -dq -o "$tmp/rootfs.ext4" "$rootfs" ;;
    *) cp --reflink=auto "$rootfs" "$tmp/rootfs.ext4" ;;
  esac
  # Not an ext4 image: refuse before it replaces a working one.
  e2fsck -fn "$tmp/rootfs.ext4" >/dev/null 2>&1 || { echo "boat-admin: $rootfs is not a clean ext4 image" >&2; exit 1; }
  chmod 0644 "$tmp/vmlinux" "$tmp/rootfs.ext4"
  # New inodes: VMs already reflinked from the old files keep theirs.
  mv -f "$tmp/vmlinux" "$dest/vmlinux"
  mv -f "$tmp/rootfs.ext4" "$dest/rootfs.ext4"
  rm -f "$dest/vmlinux.sha256" "$dest/rootfs.ext4.sha256" # not boat-images' download any more
  echo "boat-admin: installed image $id in $dest"
  case ",$IMAGE_IDS," in
    *",$id,"*) ;;
    *) if [ -n "$IMAGE_IDS" ] || [ "$id" != ubuntu-24.04 ]; then
         echo "boat-admin: declare it so boat-api offers it: services.boat.localImages = [ \"$id\" ]; then nixos-rebuild switch" >&2
       fi ;;
  esac
}

images_list() {
  for d in "$IMAGES_DIR"/*/; do
    [ -f "$d/rootfs.ext4" ] || continue
    id=$(basename "$d")
    case ",${IMAGE_IDS:-ubuntu-24.04}," in *",$id,"*) state=declared ;; *) state=undeclared ;; esac
    printf '%s\t%s\t%s\n' "$id" "$(du -h --apparent-size "$d/rootfs.ext4" | cut -f1)" "$state"
  done
}

cmd=${1-}
[ -n "$cmd" ] || usage
shift
case "$cmd" in
  init) need_root; init "$@" ;;
  images)
    sub=${1-}
    [ -n "$sub" ] || usage
    shift
    case "$sub" in
      import) need_root; images_import "$@" ;;
      list) images_list ;;
      *) usage ;;
    esac ;;
  # Older docs: `boat-admin -user ...` meant init.
  -*) need_root; init "$cmd" "$@" ;;
  *) usage ;;
esac
