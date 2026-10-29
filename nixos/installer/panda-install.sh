# shellcheck shell=bash
# panda-install: put PandaOS on a whole disk, offline, from the live ISO.
# Flags and the offline design are in README.md next to this file.
#
# package.nix sets:
#   PANDA_SYSTEMS          directory with one prebuilt system per profile
#                          ($PANDA_SYSTEMS/headless -> /nix/store/...-nixos-system-...)
#   PANDA_DEFAULT_PROFILE  profile used when --profile is not given
#   PANDA_FLAKE_URL        pandaos flake the local /etc/pandaos flake tracks
#   PANDA_FLAKE_LOCK       flake.lock pinning that input to this ISO's source ("" if none)
#   PANDA_REVISION         git revision of this ISO's source
set -euo pipefail

readonly MNT=/mnt
readonly MIN_BYTES=$((8 * 1024 * 1024 * 1024))
readonly BTRFS_OPTS=compress=zstd:1,noatime
# subvolume:mountpoint (relative to $MNT). @ must stay first.
readonly SUBVOLS=("@:" "@nix:nix" "@home:home" "@mem:mem" "@log:var/log" "@snapshots:.snapshots")
readonly SW=/nix/var/nix/profiles/system/sw/bin
readonly UEFI_NOTE="PandaOS supports UEFI only (no legacy BIOS/CSM boot), and Secure Boot must be disabled."

readonly HOSTNAME_RE='^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
readonly USER_RE='^[a-z_][a-z0-9_-]{0,31}$'
# crypt(3) hashes ($y$..., $6$...). The charset also keeps them safe inside a Nix string.
readonly HASH_RE='^[$][A-Za-z0-9./$=]+$'
readonly KEY_RE='^(ssh-[a-z0-9-]+|ecdsa-sha2-[a-z0-9-]+|sk-[A-Za-z0-9@.-]+) [A-Za-z0-9+/=]+( [^"\\$]*)?$'
readonly TZ_RE='^[A-Za-z0-9_+/-]+$'
# No quotes, '$' or spaces: the URL also goes into a Nix string.
readonly RELAY_RE='^(https?)://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$'

die() {
  echo "panda-install: error: $*" >&2
  exit 1
}

# bad MSG: report an invalid answer and fail the check (interactive mode retries).
bad() {
  echo "panda-install: error: $*" >&2
  return 1
}

log() { echo "==> $*"; }

usage() {
  cat <<EOF
Usage: panda-install [options]

Installs PandaOS on a whole disk. ALL DATA ON THE DISK IS ERASED.
Works offline: the system is copied from the ISO, nothing is downloaded.
Without --yes, it asks for anything not given on the command line.
$UEFI_NOTE

  --disk DEV              target disk, e.g. /dev/sda, /dev/nvme0n1, /dev/vda
  --profile NAME          $(profiles)(default: $PANDA_DEFAULT_PROFILE)
  --hostname NAME         machine name (default: pandaos)
  --user NAME             login user, in group wheel (default: panda)
  --password-hash HASH    crypt(3) hash, e.g. from: mkpasswd -m yescrypt
  --ssh-key KEY           optional ssh public key for the user (one line)
  --timezone TZ           e.g. Europe/Berlin (default: UTC)
  --relay URL             enable the optional panda-agentd with this relay (https)
  --allow-insecure-relay  accept an http:// relay URL
  --yes                   no questions; --disk and --password-hash are required
  -h, --help              this text
EOF
}

# ask VAR PROMPT [DEFAULT]: read a line into VAR.
ask() {
  local reply
  read -r -p "$2${3:+ [$3]}: " reply || die "no answer for: $2"
  printf -v "$1" '%s' "${reply:-${3:-}}"
}

# ask_valid VAR CHECK PROMPT [DEFAULT]: ask until CHECK accepts the answer.
ask_valid() {
  while :; do
    ask "$1" "$3" "${4:-}"
    "$2" "${!1}" && return
  done
}

ask_password() {
  local p1 p2
  while :; do
    read -r -s -p "Password for $user: " p1 || die "no password given"
    echo
    read -r -s -p "Repeat it: " p2 || die "no password given"
    echo
    [[ -n $p1 && $p1 == "$p2" ]] && break
    echo "Empty or not matching, try again."
  done
  password_hash=$(printf '%s' "$p1" | mkpasswd -m yescrypt --stdin)
}

profiles() {
  local p
  for p in "$PANDA_SYSTEMS"/*; do printf '%s ' "${p##*/}"; done
}

check_profile() {
  [[ $1 =~ ^[a-z]+$ && -e $PANDA_SYSTEMS/$1 ]] ||
    bad "profile '$1' is not on this ISO (available: $(profiles))"
}

check_hostname() {
  [[ $1 =~ $HOSTNAME_RE ]] ||
    bad "bad hostname '$1': use a-z, 0-9 and '-', at most 63 characters"
}

check_user() {
  [[ $1 =~ $USER_RE && $1 != root ]] ||
    bad "bad user name '$1': start with a-z or _, then a-z, 0-9, _ or -"
}

check_hash() {
  [[ $1 =~ $HASH_RE ]] ||
    bad "--password-hash must be a crypt(3) hash such as \$y\$... (make one with: mkpasswd -m yescrypt)"
}

check_ssh_key() {
  [[ -z $1 ]] && return
  [[ $1 != *$'\n'* && $1 =~ $KEY_RE ]] ||
    bad "bad ssh key: give one public key line, like 'ssh-ed25519 AAAA... me@laptop'"
}

check_timezone() {
  [[ $1 =~ $TZ_RE && -e /etc/zoneinfo/$1 ]] ||
    bad "unknown time zone '$1' (see /etc/zoneinfo)"
}

check_relay() {
  [[ -z $1 ]] && return
  [[ $1 =~ $RELAY_RE ]] ||
    bad "bad relay URL '$1': expected https://host[:port][/path]" || return
  [[ ${BASH_REMATCH[1]} == https ]] || ((allow_insecure_relay)) ||
    bad "relay '$1' is not https; use https, or pass --allow-insecure-relay"
}

check_disk() {
  local dev size mounts
  [[ -n $1 ]] || bad "no disk given" || return
  dev=$(readlink -f -- "$1")
  [[ -b $dev ]] || bad "$1 is not a block device" || return
  [[ $(lsblk -dno TYPE -- "$dev") == disk ]] ||
    bad "$dev is not a whole disk; pick one like /dev/sda, not a partition" || return
  size=$(blockdev --getsize64 "$dev")
  ((size >= MIN_BYTES)) ||
    bad "$dev is only $((size / 1024 / 1024 / 1024)) GiB; PandaOS needs at least 8 GiB" || return
  # No pipe into grep -q here: with pipefail a SIGPIPE would read as "not mounted".
  mounts=$(lsblk -nro MOUNTPOINTS -- "$dev")
  [[ -z ${mounts//[[:space:]]/} ]] ||
    bad "$dev is in use (mounted or swap: ${mounts//$'\n'/ }); unmount it first"
}

parse_args() {
  while (($#)); do
    case $1 in
      -h | --help)
        usage
        exit 0
        ;;
      -y | --yes)
        yes=1
        shift
        ;;
      --allow-insecure-relay)
        allow_insecure_relay=1
        shift
        ;;
      --disk | --profile | --hostname | --user | --password-hash | --ssh-key | --timezone | --relay)
        (($# >= 2)) || die "$1 needs a value"
        case $1 in
          --disk) disk=$2 ;;
          --profile) profile=$2 ;;
          --hostname) hostname=$2 ;;
          --user) user=$2 ;;
          --password-hash) password_hash=$2 ;;
          --ssh-key)
            ssh_key=$2
            ssh_key_set=1
            ;;
          --timezone) timezone=$2 ;;
          --relay)
            relay=$2
            relay_set=1
            ;;
        esac
        shift 2
        ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
  done
}

# A failed earlier run can leave the new disk mounted on /mnt.
release_stale_mnt() {
  local reply
  mountpoint -q "$MNT" || return 0
  echo "$MNT is still mounted, probably from an earlier failed run:"
  findmnt -R "$MNT" || true
  if ((!yes)); then
    read -r -p "Unmount everything under $MNT? [y/N] " reply || reply=
    [[ $reply == [yY]* ]] || die "$MNT is in use; unmount it and run panda-install again"
  fi
  log "Unmounting $MNT"
  umount -R "$MNT"
}

# Values given as flags are checked as-is; interactive answers are checked
# right after each prompt and asked again until valid. Password comes last.
gather() {
  if ((yes)); then
    [[ -n $disk ]] || die "--disk is required with --yes"
    [[ -n $password_hash ]] || die "--password-hash is required with --yes"
    profile=${profile:-$PANDA_DEFAULT_PROFILE}
    hostname=${hostname:-pandaos}
    user=${user:-panda}
    timezone=${timezone:-UTC}
  fi

  if [[ -n $disk ]]; then
    check_disk "$disk" || exit 1
  else
    echo "Disks:"
    lsblk -dp -e 7,11 -o NAME,SIZE,TYPE,MODEL
    ask_valid disk check_disk "Target disk (will be erased)"
  fi
  if [[ -n $profile ]]; then check_profile "$profile" || exit 1; else
    ask_valid profile check_profile "Profile ($(profiles))" "$PANDA_DEFAULT_PROFILE"
  fi
  if [[ -n $hostname ]]; then check_hostname "$hostname" || exit 1; else
    ask_valid hostname check_hostname "Hostname" pandaos
  fi
  if [[ -n $user ]]; then check_user "$user" || exit 1; else
    ask_valid user check_user "User" panda
  fi
  if [[ -n $timezone ]]; then check_timezone "$timezone" || exit 1; else
    ask_valid timezone check_timezone "Time zone" UTC
  fi
  if ((ssh_key_set || yes)); then check_ssh_key "$ssh_key" || exit 1; else
    ask_valid ssh_key check_ssh_key "ssh public key (optional, Enter to skip)"
  fi
  if ((relay_set || yes)); then check_relay "$relay" || exit 1; else
    ask_valid relay check_relay "panda-agentd relay URL (optional, most boxes skip it; Enter to skip)"
  fi
  if [[ -n $password_hash ]]; then check_hash "$password_hash" || exit 1; else
    ask_password
  fi
  disk=$(readlink -f -- "$disk")
}

confirm() {
  local reply
  ((yes)) && return
  echo
  lsblk -p -o NAME,SIZE,FSTYPE,LABEL,MODEL -- "$disk"
  echo
  echo "Install PandaOS ($profile) as '$hostname' for user '$user'."
  echo "$UEFI_NOTE"
  echo "ALL DATA ON $disk WILL BE ERASED."
  read -r -p "Type the disk name (${disk##*/}) to confirm: " reply || reply=
  [[ $reply == "${disk##*/}" || $reply == "$disk" ]] || die "confirmation did not match; nothing was changed"
}

# part N: partition N of $disk. Names ending in a digit (nvme0n1, mmcblk0) get a 'p'.
part() {
  if [[ $disk =~ [0-9]$ ]]; then echo "${disk}p$1"; else echo "${disk}$1"; fi
}

partition() {
  local sv _
  boot_dev=$(part 1)
  root_dev=$(part 2)
  log "Partitioning $disk"
  wipefs -af "$disk" >/dev/null
  sgdisk --zap-all "$disk" >/dev/null
  sgdisk -n 1:1M:+512M -t 1:EF00 -c 1:PANDABOOT \
    -n 2:0:0 -t 2:8300 -c 2:PANDAOS "$disk" >/dev/null
  udevadm settle
  for _ in $(seq 50); do
    [[ -b $boot_dev && -b $root_dev ]] && break
    sleep 0.2
  done
  [[ -b $boot_dev && -b $root_dev ]] || die "partitions $boot_dev and $root_dev did not appear"

  log "Formatting"
  mkfs.fat -F 32 -n PANDABOOT "$boot_dev" >/dev/null
  mkfs.btrfs -f -L PANDAOS "$root_dev" >/dev/null
  mkdir -p "$MNT" # not on every system (e.g. the bare iso-image.nix ISO)
  mount "$root_dev" "$MNT"
  for sv in "${SUBVOLS[@]}"; do btrfs subvolume create "$MNT/${sv%%:*}" >/dev/null; done
  umount "$MNT"

  log "Mounting on $MNT"
  mount -o "subvol=@,$BTRFS_OPTS" "$root_dev" "$MNT"
  for sv in "${SUBVOLS[@]:1}"; do
    mkdir -p "$MNT/${sv#*:}"
    mount -o "subvol=${sv%%:*},$BTRFS_OPTS" "$root_dev" "$MNT/${sv#*:}"
  done
  mkdir -p "$MNT/boot"
  mount -o fmask=0077,dmask=0077 "$boot_dev" "$MNT/boot"
}

# disk.nix: this machine's filesystems by UUID, so a second disk carrying the
# PANDAOS/PANDABOOT labels can never be mounted instead.
write_disk_nix() {
  local root_uuid boot_uuid sv
  root_uuid=$(blkid -s UUID -o value "$root_dev")
  boot_uuid=$(blkid -s UUID -o value "$boot_dev")
  [[ -n $root_uuid && -n $boot_uuid ]] || die "could not read filesystem UUIDs of $disk"
  {
    echo "# Written by panda-install: mount this disk by UUID, not by label."
    echo "{ lib, ... }:"
    echo "{"
    for sv in "${SUBVOLS[@]}"; do
      echo "  fileSystems.\"/${sv#*:}\".device = lib.mkForce \"/dev/disk/by-uuid/$root_uuid\";"
    done
    echo "  fileSystems.\"/boot\".device = lib.mkForce \"/dev/disk/by-uuid/$boot_uuid\";"
    echo "}"
  } >"$1/disk.nix"
}

write_local_nix() {
  local keys="" extra="" agent=""
  [[ -z $ssh_key ]] || keys=" \"$ssh_key\" "
  if [[ $user != panda ]]; then
    extra='    isNormalUser = true;
    extraGroups = [ "wheel" "networkmanager" "panda-mem" ];
'
  fi
  if [[ -n $relay ]]; then
    agent="  services.panda-agentd.enable = true;
  services.panda-agentd.relayUrl = \"$relay\";
"
    [[ $relay == https://* ]] || agent+="  services.panda-agentd.allowInsecure = true;
"
  fi
  {
    cat <<EOF
# Machine settings written by panda-install.
{ lib, ... }:
{
  # agents.nix: written by panda-agents-install (agent CLIs), when present.
  imports = [ ./disk.nix ] ++ lib.optional (builtins.pathExists ./agents.nix) ./agents.nix;

  networking.hostName = "$hostname";
  time.timeZone = "$timezone";
$agent
  users.users.$user = {
$extra    hashedPassword = "$password_hash";
    openssh.authorizedKeys.keys = [$keys];
  };
  users.users.panda.initialPassword = lib.mkForce null;
EOF
    [[ $user == panda ]] || echo '  users.users.panda.hashedPassword = "!";'
    echo '}'
  } >"$1/local.nix"
}

# The local flake /etc/pandaos#pandaos, used by every later nixos-rebuild.
write_config() {
  local dir=$MNT/etc/pandaos
  log "Writing /etc/pandaos"
  mkdir -p "$dir"
  nixos-generate-config --root "$MNT" --no-filesystems --show-hardware-config \
    >"$dir/hardware-configuration.nix"

  cat >"$dir/flake.nix" <<EOF
# This machine's PandaOS configuration, written by panda-install.
# Apply changes with: sudo nixos-rebuild switch --flake /etc/pandaos#pandaos
{
  inputs.pandaos.url = "$PANDA_FLAKE_URL";

  outputs =
    { pandaos, ... }:
    {
      nixosConfigurations.pandaos = pandaos.lib.mkPandaSystem {
        profile = "$profile";
        modules = [
          ./hardware-configuration.nix
          ./local.nix
        ];
      };
    };
}
EOF
  # Pins pandaos (and its nixpkgs) to exactly the source this ISO was built from.
  if [[ -n $PANDA_FLAKE_LOCK ]]; then
    install -m 0644 "$PANDA_FLAKE_LOCK" "$dir/flake.lock"
  fi
  printf '%s\n' "$PANDA_REVISION" >"$dir/VERSION"
  write_disk_nix "$dir"
  write_local_nix "$dir"
}

# Copy the prebuilt system from the ISO store. --system skips evaluation and
# building; with substituters cleared, nothing is fetched from the network.
install_system() {
  local system
  system=$(readlink -f -- "$PANDA_SYSTEMS/$profile")
  log "Installing $system"
  nixos-install --root "$MNT" --system "$system" \
    --no-root-passwd --no-channel-copy --option substituters ""
}

# The prebuilt system has no local.nix yet, so apply the same settings to its
# mutable state (users.mutableUsers keeps them across activations).
personalise() {
  log "Setting hostname, time zone, user"
  echo "$hostname" >"$MNT/etc/hostname"
  ln -sfn "/etc/zoneinfo/$timezone" "$MNT/etc/localtime"
  if [[ -n $relay ]]; then
    # Read by panda-agentd -relay-file; overrides the built-in relay.
    install -d -m 0711 "$MNT/var/lib/panda-agentd"
    install -m 0600 /dev/null "$MNT/var/lib/panda-agentd/relay"
    printf '%s\n' "$relay" >"$MNT/var/lib/panda-agentd/relay"
  fi
  # shellcheck disable=SC2016 # expanded inside the target, not here
  nixos-enter --root "$MNT" --silent -- "$SW/bash" -euc '
    export PATH=$1
    user=$2 hash=$3 key=$4
    id "$user" >/dev/null 2>&1 || useradd -m -g users -G wheel,networkmanager,panda-mem "$user"
    usermod -p "$hash" "$user"
    if [ "$user" != panda ] && id panda >/dev/null 2>&1; then usermod -p "!" panda; fi
    if [ -n "$key" ]; then
      group=$(id -gn "$user")
      install -d -m 700 -o "$user" -g "$group" "/home/$user/.ssh"
      printf "%s\n" "$key" >"/home/$user/.ssh/authorized_keys"
      chown "$user:$group" "/home/$user/.ssh/authorized_keys"
      chmod 600 "/home/$user/.ssh/authorized_keys"
    fi
  ' personalise "$SW" "$user" "$password_hash" "$ssh_key"
}

main() {
  disk="" profile="" hostname="" user="" password_hash="" ssh_key="" timezone="" relay=""
  yes=0 ssh_key_set=0 relay_set=0 allow_insecure_relay=0 boot_dev="" root_dev=""
  [[ -d ${PANDA_SYSTEMS:-} && -n ${PANDA_FLAKE_URL:-} && -n ${PANDA_DEFAULT_PROFILE:-} ]] ||
    die "PANDA_SYSTEMS/PANDA_FLAKE_URL/PANDA_DEFAULT_PROFILE unset; run the packaged panda-install"
  PANDA_FLAKE_LOCK=${PANDA_FLAKE_LOCK:-}
  PANDA_REVISION=${PANDA_REVISION:-unknown}
  parse_args "$@"
  [[ $EUID -eq 0 ]] || die "run as root: sudo panda-install"
  [[ -d /sys/firmware/efi ]] ||
    die "this machine booted in legacy BIOS mode. $UEFI_NOTE Reboot the installer in UEFI mode."
  ((yes)) || echo "Note: $UEFI_NOTE"
  release_stale_mnt
  gather
  confirm
  # From here on a failure must not leave the new disk mounted.
  trap 'umount -R "$MNT" 2>/dev/null || true' EXIT
  partition
  write_config
  install_system
  personalise
  sync
  umount -R "$MNT"
  trap - EXIT
  log "Done. Remove the install media and reboot."
}

main "$@"
