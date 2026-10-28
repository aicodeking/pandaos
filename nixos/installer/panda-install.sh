# shellcheck shell=bash
# panda-install: put PandaOS on a whole disk, offline, from the live ISO.
# Flags and the offline design are in README.md next to this file.
#
# package.nix sets:
#   PANDA_SYSTEMS    directory with one prebuilt system per profile
#                    ($PANDA_SYSTEMS/headless -> /nix/store/...-nixos-system-...)
#   PANDA_FLAKE_URL  pandaos flake the local /etc/pandaos flake tracks
set -euo pipefail

readonly MNT=/mnt
readonly MIN_BYTES=$((8 * 1024 * 1024 * 1024))
readonly BTRFS_OPTS=compress=zstd:1,noatime
# subvolume:mountpoint (relative to $MNT). @ must stay first.
readonly SUBVOLS=("@:" "@nix:nix" "@home:home" "@mem:mem" "@log:var/log" "@snapshots:.snapshots")
readonly SW=/nix/var/nix/profiles/system/sw/bin

readonly HOSTNAME_RE='^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
readonly USER_RE='^[a-z_][a-z0-9_-]{0,31}$'
# crypt(3) hashes ($y$..., $6$...). The charset also keeps them safe inside a Nix string.
readonly HASH_RE='^[$][A-Za-z0-9./$=]+$'
readonly KEY_RE='^(ssh-[a-z0-9-]+|ecdsa-sha2-[a-z0-9-]+|sk-[A-Za-z0-9@.-]+) [A-Za-z0-9+/=]+( [^"\\$]*)?$'
readonly TZ_RE='^[A-Za-z0-9_+/-]+$'

die() {
  echo "panda-install: error: $*" >&2
  exit 1
}

log() { echo "==> $*"; }

usage() {
  cat <<'EOF'
Usage: panda-install [options]

Installs PandaOS on a whole disk. ALL DATA ON THE DISK IS ERASED.
Works offline: the system is copied from the ISO, nothing is downloaded.
Without --yes, it asks for anything not given on the command line.

  --disk DEV            target disk, e.g. /dev/sda, /dev/nvme0n1, /dev/vda
  --profile NAME        headless (default) or desktop (desktop ISO only)
  --hostname NAME       machine name (default: pandaos)
  --user NAME           login user, in group wheel (default: panda)
  --password-hash HASH  crypt(3) hash, e.g. from: mkpasswd -m yescrypt
  --ssh-key KEY         optional ssh public key for the user (one line)
  --timezone TZ         e.g. Europe/Berlin (default: UTC)
  --yes                 no questions; --disk and --password-hash are required
  -h, --help            this text
EOF
}

# ask VAR PROMPT [DEFAULT]: read a line into VAR.
ask() {
  local reply
  read -r -p "$2${3:+ [$3]}: " reply || die "no answer for: $2"
  printf -v "$1" '%s' "${reply:-${3:-}}"
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
      --disk | --profile | --hostname | --user | --password-hash | --ssh-key | --timezone)
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
        esac
        shift 2
        ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
  done
}

gather() {
  if ((yes)); then
    [[ -n $disk ]] || die "--disk is required with --yes"
    [[ -n $password_hash ]] || die "--password-hash is required with --yes"
  else
    if [[ -z $disk ]]; then
      echo "Disks:"
      lsblk -dp -e 7,11 -o NAME,SIZE,TYPE,MODEL
      ask disk "Target disk (will be erased)"
    fi
    [[ -n $profile ]] || ask profile "Profile ($(profiles))" headless
    [[ -n $hostname ]] || ask hostname "Hostname" pandaos
    [[ -n $user ]] || ask user "User" panda
    [[ -n $password_hash ]] || ask_password
    ((ssh_key_set)) || ask ssh_key "ssh public key (optional, Enter to skip)"
    [[ -n $timezone ]] || ask timezone "Time zone" UTC
  fi
  profile=${profile:-headless}
  hostname=${hostname:-pandaos}
  user=${user:-panda}
  timezone=${timezone:-UTC}
}

validate() {
  [[ $profile =~ ^[a-z]+$ && -e $PANDA_SYSTEMS/$profile ]] ||
    die "profile '$profile' is not on this ISO (available: $(profiles))"
  [[ $hostname =~ $HOSTNAME_RE ]] ||
    die "bad hostname '$hostname': use a-z, 0-9 and '-', at most 63 characters"
  [[ $user =~ $USER_RE && $user != root ]] ||
    die "bad user name '$user': start with a-z or _, then a-z, 0-9, _ or -"
  [[ $password_hash =~ $HASH_RE ]] ||
    die "--password-hash must be a crypt(3) hash such as \$y\$... (make one with: mkpasswd -m yescrypt)"
  if [[ -n $ssh_key ]]; then
    [[ $ssh_key != *$'\n'* && $ssh_key =~ $KEY_RE ]] ||
      die "bad --ssh-key: give one public key line, like 'ssh-ed25519 AAAA... me@laptop'"
  fi
  [[ $timezone =~ $TZ_RE && -e /etc/zoneinfo/$timezone ]] ||
    die "unknown time zone '$timezone' (see /etc/zoneinfo)"
}

check_disk() {
  local size mounts
  disk=$(readlink -f -- "$disk")
  [[ -b $disk ]] || die "$disk is not a block device"
  [[ $(lsblk -dno TYPE -- "$disk") == disk ]] ||
    die "$disk is not a whole disk; pick one like /dev/sda, not a partition"
  size=$(blockdev --getsize64 "$disk")
  ((size >= MIN_BYTES)) ||
    die "$disk is only $((size / 1024 / 1024 / 1024)) GiB; PandaOS needs at least 8 GiB"
  # No pipe into grep -q here: with pipefail a SIGPIPE would read as "not mounted".
  mounts=$(lsblk -nro MOUNTPOINTS -- "$disk")
  [[ -z ${mounts//[[:space:]]/} ]] ||
    die "$disk is in use (mounted or swap: ${mounts//$'\n'/ }); unmount it first"
  ! mountpoint -q "$MNT" || die "$MNT is already mounted; unmount it first"
}

confirm() {
  local reply
  ((yes)) && return
  echo
  lsblk -p -o NAME,SIZE,FSTYPE,LABEL,MODEL -- "$disk"
  echo
  echo "Install PandaOS ($profile) as '$hostname' for user '$user'."
  echo "ALL DATA ON $disk WILL BE ERASED."
  read -r -p "Type the disk name (${disk##*/}) to confirm: " reply || reply=
  [[ $reply == "${disk##*/}" || $reply == "$disk" ]] || die "confirmation did not match; nothing was changed"
}

# part N: partition N of $disk. Names ending in a digit (nvme0n1, mmcblk0) get a 'p'.
part() {
  if [[ $disk =~ [0-9]$ ]]; then echo "${disk}p$1"; else echo "${disk}$1"; fi
}

partition() {
  local boot root sv _
  boot=$(part 1)
  root=$(part 2)
  log "Partitioning $disk"
  wipefs -af "$disk" >/dev/null
  sgdisk --zap-all "$disk" >/dev/null
  sgdisk -n 1:1M:+512M -t 1:EF00 -c 1:PANDABOOT \
    -n 2:0:0 -t 2:8300 -c 2:PANDAOS "$disk" >/dev/null
  udevadm settle
  for _ in $(seq 50); do
    [[ -b $boot && -b $root ]] && break
    sleep 0.2
  done
  [[ -b $boot && -b $root ]] || die "partitions $boot and $root did not appear"

  log "Formatting"
  mkfs.fat -F 32 -n PANDABOOT "$boot" >/dev/null
  mkfs.btrfs -f -L PANDAOS "$root" >/dev/null
  mkdir -p "$MNT" # not on every system (e.g. the bare iso-image.nix ISO)
  mount "$root" "$MNT"
  for sv in "${SUBVOLS[@]}"; do btrfs subvolume create "$MNT/${sv%%:*}" >/dev/null; done
  umount "$MNT"

  log "Mounting on $MNT"
  mount -o "subvol=@,$BTRFS_OPTS" "$root" "$MNT"
  trap 'umount -R "$MNT" 2>/dev/null || true' EXIT
  for sv in "${SUBVOLS[@]:1}"; do
    mkdir -p "$MNT/${sv#*:}"
    mount -o "subvol=${sv%%:*},$BTRFS_OPTS" "$root" "$MNT/${sv#*:}"
  done
  mkdir -p "$MNT/boot"
  mount -o fmask=0077,dmask=0077 "$boot" "$MNT/boot"
}

# The local flake /etc/pandaos#pandaos, used by every later nixos-rebuild.
write_config() {
  local dir=$MNT/etc/pandaos keys="" extra=""
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

  [[ -z $ssh_key ]] || keys=" \"$ssh_key\" "
  if [[ $user != panda ]]; then
    extra='    isNormalUser = true;
    extraGroups = [ "wheel" "networkmanager" "panda-mem" ];
'
  fi
  cat >"$dir/local.nix" <<EOF
# Machine settings written by panda-install.
{ lib, ... }:
{
  networking.hostName = "$hostname";
  time.timeZone = "$timezone";

  users.users.$user = {
$extra    hashedPassword = "$password_hash";
    openssh.authorizedKeys.keys = [$keys];
  };
  users.users.panda.initialPassword = lib.mkForce null;
EOF
  [[ $user == panda ]] || echo '  users.users.panda.hashedPassword = "!";' >>"$dir/local.nix"
  echo '}' >>"$dir/local.nix"
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
  disk="" profile="" hostname="" user="" password_hash="" ssh_key="" timezone=""
  yes=0 ssh_key_set=0
  parse_args "$@"
  [[ $EUID -eq 0 ]] || die "run as root: sudo panda-install"
  [[ -d ${PANDA_SYSTEMS:-} && -n ${PANDA_FLAKE_URL:-} ]] ||
    die "PANDA_SYSTEMS/PANDA_FLAKE_URL unset; run the packaged panda-install"
  [[ -d /sys/firmware/efi ]] ||
    die "not booted in UEFI mode; PandaOS uses systemd-boot, which needs UEFI"
  gather
  validate
  check_disk
  confirm
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
