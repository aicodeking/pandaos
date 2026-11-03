# boat: self-hosted persistent Firecracker microVMs (a boat.dev alternative).
#
#   boatd.service        root VM manager (KVM, tap/bridge, nftables, jailer)
#   boat-api.service     public API + webhooks as the unprivileged `boat` user
#   boat-images.service  downloads released guest images into imagesDir
#   boat-secrets.service creates the desktop-link HMAC key once
#   gateway/module.nix   Caddy (TLS) + boat-gateway (routes) + ssh jump host
#
# boatd reads /etc/boat/boatd.json (its Config in boatd/config.go); boat-api
# gets flags. Off unless services.boat.enable.
{ config, lib, pkgs, ... }:
let
  cfg = config.services.boat;
  inherit (lib) mkOption types;

  sizeType = types.submodule {
    options = {
      vcpu = mkOption { type = types.ints.positive; };
      mem_mib = mkOption { type = types.ints.positive; };
      disk_gib = mkOption { type = types.ints.positive; };
    };
  };

  # Image ids boat-api offers: first path component of each images entry.
  imageIds = lib.unique (map (n: lib.head (lib.splitString "/" n)) (lib.attrNames cfg.images));

  imageType = types.submodule {
    options = {
      url = mkOption {
        type = types.strMatching "https://.+";
        description = "Release URL. A `.zst` suffix is decompressed after the hash check.";
      };
      sha256 = mkOption {
        type = types.strMatching "[0-9a-f]{64}";
        description = "Hex SHA-256 of the file at {option}`url` (before decompression).";
      };
    };
  };

  aiProxyPort = 8744;
  credFile = "${cfg.dataDir}/secrets/boat-master-key.cred";
  masterCred = "boat-master-key:${credFile}";
  desktopCredFile = "${cfg.dataDir}/secrets/boat-desktop-key.cred";
  desktopCred = "boat-desktop-key:${desktopCredFile}";
  exportCredFile = "${cfg.dataDir}/secrets/boat-export-key.cred";

  # What services.boat.untrustedTenants sets. boatd refuses to start if a key below
  # is overridden to something weaker (config.go validateUntrusted).
  untrustedSettings = {
    untrustedTenants = true;
    btrfsQgroups = true;
    storageOvercommit = 1; # committed storage fits the disk: no tenant can fill it
    memOvercommit = 1;
    egressMbit = 1000;
    ingressMbit = 1000;
    ioMaxMBps = 500;
    ioMaxIOPS = 20000;
    ctPerVM = 4096;
    diskEngine = "Sync"; # with kernel.io_uring_disabled: no io_uring for a broken-out VMM
    hostDirtyRatio = 20; # a tenant must not be able to dirty half of the host's RAM
    warmPool = false; # templates share ASLR across tenants
    ksmTeam = ""; # a cross-VM side channel
  };

  # The Firecracker that boat-e2e tests (e2e.sh FC_VERSION + FC_SHA256), not
  # whatever nixpkgs carries: snapshots are version-sensitive. Static binaries.
  firecrackerPinned = pkgs.stdenvNoCC.mkDerivation rec {
    pname = "firecracker";
    version = "1.17.0";
    src = pkgs.fetchurl {
      url = "https://github.com/firecracker-microvm/firecracker/releases/download/v${version}/firecracker-v${version}-x86_64.tgz";
      sha256 = "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558";
    };
    sourceRoot = "release-v${version}-x86_64";
    dontConfigure = true;
    dontBuild = true;
    dontFixup = true;
    installPhase = ''
      install -Dm755 firecracker-v${version}-x86_64 $out/bin/firecracker
      install -Dm755 jailer-v${version}-x86_64 $out/bin/jailer
    '';
    meta = {
      license = lib.licenses.asl20;
      platforms = [ "x86_64-linux" ];
      sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    };
  };

  # Last two labels: a heuristic (co.uk looks like a registrable domain), good
  # enough to warn on.
  regDomain = name: lib.concatStringsSep "." (lib.takeEnd 2 (lib.splitString "." name));

  # Exit status for "this host cannot run boatd" (EX_CONFIG): no restart loop.
  exConfig = 78;
  kvmCheck = pkgs.writeShellScript "boatd-kvm-check" ''
    if [ ! -c /dev/kvm ]; then
      echo "boatd: /dev/kvm is missing, so this machine cannot run Firecracker VMs." >&2
      echo "  Bare metal: enable VT-x/AMD-V (SVM) in the firmware setup." >&2
      echo "  Inside a VM: the hypervisor must expose nested virtualization" >&2
      echo "  (kvm_intel nested=1 / kvm_amd nested=1 on the host, CPU model 'host')." >&2
      echo "  Set services.boat.requireKvm = false only for tests with a stub boatd." >&2
      exit ${toString exConfig}
    fi
  '';

  # panda-install --data-disk writes the marker; then dataDir must be that disk.
  dataCheck = pkgs.writeShellScript "boat-data-check" ''
    if [ -e /etc/pandaos/data-disk ] && ! ${pkgs.util-linux}/bin/mountpoint -q ${cfg.dataDir}; then
      echo "boat: ${cfg.dataDir} is not mounted, but this machine has a boat data disk" >&2
      echo "  ($(cat /etc/pandaos/data-disk)). Refusing to write VM data to the system disk." >&2
      echo "  Check: lsblk -f; journalctl -b -u '$(${config.systemd.package}/bin/systemd-escape -p --suffix=mount ${cfg.dataDir})'" >&2
      exit ${toString exConfig}
    fi
  '';

  imagesScript = pkgs.writeShellScript "boat-images" ''
    set -euo pipefail
    export PATH=${lib.makeBinPath [ pkgs.coreutils pkgs.curl pkgs.zstd ]}
    fetch() { # <rel path> <url> <sha256>
      dest=${cfg.imagesDir}/$1 url=$2 sha=$3
      final=''${dest%.zst}
      if [ -e "$final" ] && [ "$(cat "$final.sha256" 2>/dev/null)" = "$sha" ]; then
        return 0
      fi
      mkdir -p "$(dirname "$dest")"
      tmp=$(mktemp "$dest.part.XXXXXX")
      trap 'rm -f "$tmp" "$tmp.out"' RETURN
      echo "boat-images: fetching $url"
      # Called as `fetch ... || rc=1`, where set -e is off: check every step.
      curl -fL --retry 5 --retry-delay 5 --proto =https -o "$tmp" "$url" || return 1
      echo "$sha  $tmp" | sha256sum -c --quiet || { echo "boat-images: sha256 mismatch for $url" >&2; return 1; }
      case "$url" in
        *.zst) zstd -dq -f -o "$tmp.out" "$tmp" || return 1 ;;
        *) mv "$tmp" "$tmp.out" || return 1 ;;
      esac
      chmod 0644 "$tmp.out" && mv -f "$tmp.out" "$final" || return 1
      echo "$sha" >"$final.sha256"
    }
    rc=0
    ${lib.concatStrings (
      lib.mapAttrsToList (
        name: img: "fetch ${lib.escapeShellArgs [ name img.url img.sha256 ]} || rc=1\n"
      ) cfg.images
    )}
    exit $rc
  '';

  # boatd's Config (boatd/config.go); unset fields keep boatd's defaults.
  boatdConfig = {
    stateDir = cfg.dataDir;
    runDir = "/run/boat";
    socket = "/run/boat/boatd.sock";
    socketGroup = "boat";
    btrfsQgroups = true; # boatd goes on without them only if dataDir is not btrfs
    inherit aiProxyPort;
    firecracker = lib.getExe' cfg.firecrackerPackage "firecracker";
    jailer = lib.getExe' cfg.firecrackerPackage "jailer";
    bridge = "boatbr0";
    subnet = "10.200.0.0/16";
    ipv6Prefix = if cfg.ipv6Prefix == null then "" else cfg.ipv6Prefix;
    publicIPv4 = cfg.publicIPv4Pool;
    inherit (cfg) sizes defaultSize;
    defaultTeam = cfg.quotas;
  }
  // lib.optionalAttrs (imageIds != [ ]) { defaultImage = lib.head imageIds; }
  // lib.optionalAttrs cfg.untrustedTenants untrustedSettings
  // cfg.boatdSettings;

  apiArgs = [
    "serve"
    "-data" "${cfg.dataDir}/api"
    "-listen" "127.0.0.1:${toString cfg.apiPort}"
    "-boatd" "/run/boat/boatd.sock"
    "-ssh-user" "dev" # the guest image's login user
    "-desktop-url" "https://desktop.${cfg.domain}"
    "-desktop-key-file" "%d/boat-desktop-key"
    "-ssh-jump-keys-file" "${cfg.dataDir}/ssh/jump_authorized_keys"
    "-vm-domain" cfg.vmDomain
    # Only the AI proxy, on the bridge, for VMs (boatd opens aiProxyPort to them).
    "-ai-proxy-listen" "10.200.0.1:${toString aiProxyPort}"
    "-public-url" "https://api.${cfg.domain}"
  ]
  ++ lib.optionals cfg.sshGateway.enable [ "-ssh-jump" "jump@ssh.${cfg.domain}:${toString cfg.sshGateway.port}" ]
  ++ lib.optionals (imageIds != [ ]) [ "-images" (lib.concatStringsSep "," imageIds) ];
in
{
  imports = [ ./gateway/module.nix ./ops.nix ];

  options.services.boat = {
    enable = lib.mkEnableOption "boat, persistent Firecracker microVMs for agents";
    domain = mkOption {
      type = types.strMatching "[a-z0-9]([a-z0-9.-]*[a-z0-9])?";
      example = "boat.example.com";
      description = "Base domain: api., desktop., *.desktop. and ssh. live under it (see docs/boat.md for DNS).";
    };
    vmDomain = mkOption {
      type = types.strMatching "[a-z0-9]([a-z0-9.-]*[a-z0-9])?";
      default = "vm.${cfg.domain}";
      defaultText = lib.literalExpression ''"vm.''${config.services.boat.domain}"'';
      example = "boatvms.example.net";
      description = ''
        Port routes live at `<port>-<vmid>.<vmDomain>`; that is tenant-controlled
        content. Use a different registrable domain from {option}`domain` (or one
        on the Public Suffix List), so tenant apps cannot set cookies for `api.`
        or the desktops.
      '';
    };
    desktopDomain = mkOption {
      type = types.strMatching "[a-z0-9]([a-z0-9.-]*[a-z0-9])?";
      default = "desktop.${cfg.domain}";
      defaultText = lib.literalExpression ''"desktop.''${config.services.boat.domain}"'';
      description = "Each VM's desktop is its own origin, `<vmid>.<desktopDomain>`.";
    };
    acmeEmail = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "ops@example.com";
      description = "ACME account e-mail for Caddy's certificates.";
    };
    ipv6Prefix = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "2a01:4f8:1:2:b0a7::/80";
      description = "Routed IPv6 prefix; each VM gets a public address from it. Null: IPv4 NAT only.";
    };
    publicIPv4Pool = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [ "203.0.113.10" "203.0.113.11" ];
      description = "Extra routed IPv4 addresses boatd may assign 1:1 to VMs (dedicated IPs).";
    };
    sizes = mkOption {
      type = types.attrsOf sizeType;
      default = {
        small = { vcpu = 2; mem_mib = 4096; disk_gib = 51; };
        default = { vcpu = 4; mem_mib = 8192; disk_gib = 51; };
        large = { vcpu = 8; mem_mib = 16384; disk_gib = 111; };
        xlarge = { vcpu = 16; mem_mib = 32768; disk_gib = 251; };
      };
      description = "VM sizes (`boat new --size NAME`); disks are sparse.";
    };
    defaultSize = mkOption {
      type = types.str;
      default = "default";
      description = "Size used when a create request names none; must be in {option}`sizes`.";
    };
    quotas = mkOption {
      description = "Default per-team limits (boatd `defaultTeam`); 0 = unlimited.";
      default = { };
      type = types.submodule {
        options = {
          maxVMs = mkOption { type = types.ints.unsigned; default = 10; };
          maxVCPU = mkOption { type = types.ints.unsigned; default = 32; };
          maxMemMiB = mkOption { type = types.ints.unsigned; default = 65536; };
          maxTotalVMs = mkOption {
            type = types.ints.unsigned;
            default = 50;
            description = "VMs including stopped ones (their disks stay on the host).";
          };
          maxSnapshots = mkOption { type = types.ints.unsigned; default = 100; };
          maxDiskGiB = mkOption {
            type = types.ints.unsigned;
            default = 1000;
            description = "Committed storage: VM disks, memory files and snapshots.";
          };
          maxPorts = mkOption {
            type = types.ints.unsigned;
            default = 50;
            description = "Port routes across the team's VMs. Each can cost an ACME certificate without DNS-01.";
          };
          allowIntraTeam = mkOption {
            type = types.bool;
            default = false;
            description = "Let a team's VMs reach each other (other teams: never).";
          };
        };
      };
    };
    dataDir = mkOption {
      type = types.strMatching "/.+";
      default = "/var/lib/boat";
      description = "VM disks, snapshots, API database and secrets. Put it on the btrfs data disk.";
    };
    imagesDir = mkOption {
      type = types.strMatching "/.+";
      default = "${cfg.dataDir}/images";
      defaultText = lib.literalExpression ''"''${config.services.boat.dataDir}/images"'';
      readOnly = true;
      description = "Guest images, `<id>/{vmlinux,rootfs.ext4}` (boatd reads `<dataDir>/images`).";
    };
    images = mkOption {
      type = types.attrsOf imageType;
      default = { };
      example = lib.literalExpression ''
        {
          "ubuntu-24.04/vmlinux" = { url = "https://github.com/AICodeKing/pandaos/releases/download/boat-guest-v1/vmlinux"; sha256 = "..."; };
          "ubuntu-24.04/rootfs.ext4.zst" = { url = "https://.../rootfs-slim.ext4.zst"; sha256 = "..."; };
        }
      '';
      description = ''
        Guest images boat-images.service downloads. The name is the path under
        {option}`imagesDir` (a trailing `.zst` is dropped after decompression);
        its first component is the image id boat-api offers (first = default).
      '';
    };
    firecrackerPackage = mkOption {
      type = types.package;
      default = firecrackerPinned;
      defaultText = lib.literalExpression "Firecracker v1.17.0, the release boat-e2e tests";
      description = "Provides bin/firecracker and bin/jailer. Changing it leaves your VMs' snapshots on an untested version.";
    };
    boatdPackage = mkOption {
      type = types.package;
      default = pkgs.callPackage ./boatd/package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./boatd/package.nix { }";
    };
    apiPackage = mkOption {
      type = types.package;
      default = pkgs.callPackage ./api/package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./api/package.nix { }";
      description = "Provides bin/boat-api.";
    };
    cliPackage = mkOption {
      type = types.package;
      default = pkgs.callPackage ./cli/package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./cli/package.nix { }";
      description = "Provides bin/boat (installed system-wide).";
    };
    apiPort = mkOption {
      type = types.port;
      default = 8740;
      description = "boat-api listens on 127.0.0.1 only; Caddy publishes it as api.<domain>.";
    };
    boatdSettings = mkOption {
      type = types.attrsOf types.anything;
      default = { };
      example = { egressMbit = 200; storageOvercommit = 2; idleStopS = 3600; warmPool = true; ksmTeam = "ci"; teams.ci = { maxVMs = 50; }; };
      description = "Extra keys merged into /etc/boat/boatd.json (see boatd/config.go and PROTOCOL.md \"Performance and density\"); they win over the module's.";
    };
    untrustedTenants = lib.mkEnableOption ''
      hosting tenants you do not trust. Turns on what is off by default because it costs
      performance or density: SMT off (`nosmt=force`, half the threads of an SMT host),
      CPU mitigations and flushes on the kernel command line, per-VM egress, ingress, disk and connection
      limits, committed storage capped at the disk's size, memory overcommit 1.0, the
      `Sync` disk engine with io_uring off in the kernel, no swap, and refuses KSM and
      `warmPool`. Needs a reboot to take effect; until then boatd refuses to start.
      Each limit can be raised in {option}`boatdSettings`, not removed
    '';
    stopTimeoutSec = mkOption {
      type = types.ints.positive;
      default = 900;
      description = ''
        How long boatd may take to stop. On stop (reboot, shutdown) it pauses
        every running VM and writes its memory to disk, in parallel; budget the
        total RAM of all running VMs over the disk's write speed.
      '';
    };
    requireKvm = mkOption {
      type = types.bool;
      default = true;
      description = "Refuse to start boatd without /dev/kvm. Turn off only for stub-boatd tests.";
    };
    sshGateway = {
      enable = lib.mkEnableOption "the ssh jump host for VMs" // { default = true; };
      port = mkOption {
        type = types.port;
        default = 2222;
        description = "sshd port reserved for the `jump` user (ProxyJump into VMs).";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    warnings =
      lib.optional (!(lib.hasSuffix ".localhost" cfg.domain) && regDomain cfg.vmDomain == regDomain cfg.domain) ''
        services.boat.vmDomain (${cfg.vmDomain}) shares a registrable domain with ${cfg.domain}: tenant apps
        are same-site with api.${cfg.domain} and can set cookies for ${regDomain cfg.domain}. Use a separate
        domain (or one on the Public Suffix List) before you host untrusted tenants; see docs/boat.md.''
      ++ lib.optional (!(lib.hasSuffix ".localhost" cfg.domain) && regDomain cfg.desktopDomain == regDomain cfg.domain) ''
        services.boat.desktopDomain (${cfg.desktopDomain}) shares a registrable domain with ${cfg.domain}: the
        pages a VM's desktop serves are same-site with api.${cfg.domain}. Use a separate domain before you
        host untrusted tenants; see docs/boat.md.'';

    assertions = [
      {
        assertion = lib.all (n: !(lib.hasPrefix "/" n) && !(lib.elem ".." (lib.splitString "/" n))) (lib.attrNames cfg.images);
        message = "services.boat.images names must be relative paths without '..'.";
      }
      {
        assertion = lib.hasAttr cfg.defaultSize cfg.sizes;
        message = "services.boat.defaultSize must name one of services.boat.sizes.";
      }
      {
        assertion = cfg.untrustedTenants -> !(cfg.boatdSettings.warmPool or false) && (cfg.boatdSettings.ksmTeam or "") == "";
        message = "services.boat.untrustedTenants refuses warmPool (templates share ASLR across tenants) and ksmTeam (a side channel between guests).";
      }
    ];

    boot.kernelModules = [ "kvm" "tun" "vhost_net" "bridge" "nf_tables" "nf_conntrack" ];
    # Firecracker's production host recommendations. Unknown parameters are ignored, so
    # the Intel-only ones are harmless on AMD.
    boot.kernelParams = lib.optionals cfg.untrustedTenants [
      "nosmt=force"
      "mitigations=auto,nosmt"
      "spec_store_bypass_disable=seccomp"
      "spectre_v2_user=seccomp"
      "l1tf=full,force"
      "kvm-intel.vmentry_l1d_flush=always"
      "kvm.nx_huge_pages=force"
    ];
    zramSwap.enable = lib.mkIf cfg.untrustedTenants (lib.mkForce false); # guest memory stays out of swap
    swapDevices = lib.mkIf cfg.untrustedTenants (lib.mkForce [ ]);
    boot.kernel.sysctl = {
      "net.ipv4.ip_forward" = 1;
      # A VM's flows are capped at ctPerVM (default 4096), so about 256 VMs at the cap fit.
      # The kernel's default, about 260k on 16 GiB, is 64: one team could fill it.
      "net.netfilter.nf_conntrack_max" = 1048576;
      "fs.protected_hardlinks" = 1;
      "fs.protected_symlinks" = 1;
    }
    // lib.optionalAttrs cfg.untrustedTenants {
      "kernel.io_uring_disabled" = 2; # diskEngine Sync; absent before Linux 6.6
      "kernel.unprivileged_bpf_disabled" = 1;
      "net.core.bpf_jit_harden" = 2;
      "kernel.kptr_restrict" = 2;
      "kernel.dmesg_restrict" = 1;
    }
    // lib.optionalAttrs (cfg.ipv6Prefix != null) {
      "net.ipv6.conf.all.forwarding" = 1;
      # Forwarding turns off RA on the uplink; keep RA-configured uplinks (OVH) working.
      "net.ipv6.conf.default.accept_ra" = 2;
      # ...but never from a VM: the bridge inherits `default`. udev reapplies
      # this when the interface appears (boatd also writes it).
      "net.ipv6.conf.boatbr0.accept_ra" = 0;
    };
    networking.nftables.enable = true;

    users.users.boat = {
      isSystemUser = true;
      group = "boat";
      description = "boat API";
    };
    users.groups.boat = { };

    environment.etc."boat/boatd.json".text = builtins.toJSON boatdConfig;

    systemd.tmpfiles.rules = [
      # 0751: boat-gateway (no boat group) reads routes.json by name; it cannot
      # list the directory, and boatd's socket is 0660 root:boat.
      "d /run/boat 0751 root boat - -"
      # `v`: a btrfs subvolume when on btrfs (outside root snapshots, reflink
      # clones for fork/snapshot), a plain directory elsewhere.
      "v ${cfg.dataDir} 0750 root boat - -"
      "d ${cfg.imagesDir} 0755 root root - -"
      # Sealed credentials: only systemd (PID 1) reads them, for the units below.
      "d ${cfg.dataDir}/secrets 0700 root root - -"
      "d ${cfg.dataDir}/api 0700 boat boat - -"
      "d ${cfg.dataDir}/ssh 0750 boat boat - -"
    ];

    # Operator entry point to boat-api's admin commands with the sealed master
    # key, e.g. `sudo boat-admin init -user me -team ops`.
    environment.systemPackages = [
      cfg.cliPackage
      (pkgs.writeShellScriptBin "boat-admin" ''
        [ "''${1-}" = init ] && shift
        ${config.systemd.package}/bin/systemd-run --quiet --pipe --wait --collect \
          -p User=boat -p Group=boat \
          -p LoadCredentialEncrypted=${masterCred} \
          -p RequiresMountsFor=${cfg.dataDir} \
          ${lib.getExe' cfg.apiPackage "boat-api"} admin init -data ${cfg.dataDir}/api "$@" || exit
        # The running API reads its users at start.
        ${config.systemd.package}/bin/systemctl try-restart boat-api.service
      '')
    ];

    systemd.services.boat-secrets = {
      description = "boat: create the desktop-link key and the sealed API master key";
      wantedBy = [ "multi-user.target" ];
      after = [ "systemd-tmpfiles-setup.service" ];
      unitConfig.RequiresMountsFor = cfg.dataDir;
      path = [ pkgs.coreutils pkgs.diffutils config.systemd.package ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        UMask = "0077";
      };
      script = ''
        # boat-api's master key (seals tenant secrets and webhook keys) and the
        # desktop/route HMAC key live only as systemd credentials, encrypted
        # with the TPM2 when present and the host key
        # (/var/lib/systemd/credential.secret, not under dataDir), so a backup
        # of dataDir holds no usable key. A plaintext key from an older install
        # is sealed, verified, then removed.
        seal() { # name plaintext-path credential-path
          name=$1 plain=$2 cred=$3
          [ -s "$cred" ] && return 0
          if [ -s "$plain" ]; then src=$plain; else src=$(mktemp); head -c 32 /dev/urandom >"$src"; fi
          systemd-creds encrypt --with-key=auto --name="$name" "$src" "$cred.tmp"
          if ! systemd-creds decrypt --name="$name" "$cred.tmp" - | cmp -s - "$src"; then
            echo "boat-secrets: sealed $name does not decrypt; nothing changed" >&2
            rm -f "$cred.tmp"
            [ "$src" = "$plain" ] || rm -f "$src" # a key generated just now, never used
            exit 1
          fi
          mv "$cred.tmp" "$cred"
          if [ "$src" = "$plain" ]; then shred -u "$plain" 2>/dev/null || rm -f "$plain"; else rm -f "$src"; fi
        }
        seal boat-master-key ${cfg.dataDir}/api/master.key ${credFile}
        seal boat-desktop-key ${cfg.dataDir}/secrets/desktop.key ${desktopCredFile}
        # boatd's HMAC over snapshot exports: whoever has it can get Firecracker to restore
        # a crafted vmstate.
        seal boat-export-key ${cfg.dataDir}/export.key ${exportCredFile}
      '';
    };

    systemd.services.boat-images = {
      description = "boat: download guest images";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" "systemd-tmpfiles-setup.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = imagesScript;
        ProtectSystem = "strict";
        ReadWritePaths = [ cfg.imagesDir ];
        PrivateTmp = true;
        ProtectHome = true;
        NoNewPrivileges = true;
      };
    };

    systemd.services.boatd = {
      description = "boat VM manager (Firecracker)";
      wantedBy = [ "multi-user.target" ];
      wants = [ "boat-images.service" "network-online.target" ];
      requires = [ "boat-secrets.service" ];
      after = [ "boat-images.service" "boat-secrets.service" "network-online.target" ];
      # Persistent VMs: never restart on `nixos-rebuild switch` or the daily
      # auto-upgrade; a new boatd takes effect on reboot or a manual restart.
      restartIfChanged = false;
      stopIfChanged = false;
      path = [
        cfg.firecrackerPackage pkgs.iproute2 pkgs.nftables pkgs.conntrack-tools pkgs.coreutils pkgs.procps pkgs.gnutar pkgs.zstd
        pkgs.e2fsprogs pkgs.btrfs-progs pkgs.util-linux
      ];
      # VM disks live there: never start on (or write to) the bare mountpoint.
      unitConfig.RequiresMountsFor = cfg.dataDir;
      serviceConfig = {
        ExecStartPre = [ dataCheck ] ++ lib.optional cfg.requireKvm kvmCheck;
        ExecStart = lib.getExe' cfg.boatdPackage "boatd";
        # Read from $CREDENTIALS_DIRECTORY/boat-export-key.
        LoadCredentialEncrypted = [ "boat-export-key:${exportCredFile}" ];
        Restart = "on-failure";
        RestartSec = 5;
        RestartPreventExitStatus = exConfig;
        # SIGTERM lets boatd pause/snapshot VMs; then everything in the cgroup dies.
        KillMode = "mixed";
        TimeoutStopSec = cfg.stopTimeoutSec;
        # The host OOM killer should take a VM, not the manager of all of them.
        # boatd sets each Firecracker's oom_score_adj back up (they inherit this).
        OOMScoreAdjust = -900;
        # VMs live in boatd's own cgroup tree (boat-vms, at the cgroup root), so
        # a boatd crash spares them. Do not set ProtectControlGroups here.
        # Root: jailer needs chroot/mknod/setuid/namespaces, boatd needs
        # CAP_NET_ADMIN for taps and nftables. Firecracker itself runs jailed
        # (own uid, chroot, seccomp). Sandbox what does not break that.
        ProtectSystem = "full";
        ProtectHome = true;
        PrivateTmp = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectClock = true;
        RestrictRealtime = true;
        LimitNOFILE = 1048576;
      };
    };

    systemd.services.boat-api = {
      description = "boat public API";
      wantedBy = [ "multi-user.target" ];
      wants = [ "boatd.service" "network-online.target" ];
      after = [ "boatd.service" "boat-secrets.service" "network-online.target" ];
      requires = [ "boat-secrets.service" ];
      unitConfig.RequiresMountsFor = cfg.dataDir;
      serviceConfig = {
        ExecStart = lib.escapeShellArgs ([ (lib.getExe' cfg.apiPackage "boat-api") ] ++ apiArgs);
        ExecStartPre = dataCheck;
        RestartPreventExitStatus = exConfig;
        # Read by boat-api from $CREDENTIALS_DIRECTORY/boat-master-key.
        LoadCredentialEncrypted = [ masterCred desktopCred ];
        OOMScoreAdjust = -900;
        User = "boat";
        Group = "boat";
        Restart = "always";
        RestartSec = 2;
        ReadWritePaths = [ "${cfg.dataDir}/api" "${cfg.dataDir}/ssh" ];
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        NoNewPrivileges = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" "~@privileged" ];
        CapabilityBoundingSet = "";
        # Unix: boatd socket; inet: clients via Caddy and outbound webhooks.
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        UMask = "0027";
      };
    };
  };
}
