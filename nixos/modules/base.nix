# PandaOS base: lean defaults shared by every profile.
{ config, lib, pkgs, ... }:
{
  imports = [
    ./security.nix
    ./updates.nix
    ./firstboot.nix
    ./snapshots.nix
    ./boot.nix
    ./cache.nix
  ];

  system.stateVersion = "26.05";

  # Docs: man pages only.
  documentation = {
    enable = true;
    man.enable = true;
    nixos.enable = false;
    info.enable = false;
    doc.enable = false;
  };
  environment.defaultPackages = lib.mkDefault [ ];
  programs.command-not-found.enable = false;

  # Memory and disk.
  zramSwap.enable = true;
  boot.tmp.useTmpfs = true;
  boot.supportedFilesystems = [ "btrfs" ];
  services.journald.extraConfig = ''
    SystemMaxUse=50M
  '';
  # No systemd-nspawn/nixos-container tooling (Perl script + units); agentd
  # uses plain systemd-run units.
  boot.enableContainers = false;

  nix = {
    settings = {
      auto-optimise-store = true;
      # Disk-full guard: when free space drops below 1 GiB during a build or
      # download, Nix garbage-collects until 3 GiB are free.
      min-free = 1024 * 1024 * 1024;
      max-free = 3 * 1024 * 1024 * 1024;
      experimental-features = [
        "nix-command"
        "flakes"
      ];
    };
    gc = {
      automatic = true;
      dates = "weekly";
      options = "--delete-older-than 14d";
    };
  };

  # Network: nmtui for wifi, key-only ssh, firewall on (see security.nix).
  networking = {
    hostName = lib.mkDefault "pandaos";
    networkmanager.enable = true;
    # NetworkManager turns ModemManager on by default (a daemon plus
    # libmbim/libqmi). PandaOS boxes don't use cellular modems.
    modemmanager.enable = false;
    firewall.enable = true;
  };
  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      PermitRootLogin = "no";
    };
  };

  # Shown in /etc/issue on the console.
  services.getty.greetingLine = ''<<< Welcome to PandaOS (\m) - \l >>>'';

  # Repair kit.
  environment.systemPackages = with pkgs; [
    btrfs-progs
    gitMinimal
    curl
    rsync
    htop
    ncdu
    tmux
    nano
    parted
    gptfdisk
    smartmontools
    testdisk
    pciutils
    usbutils
    lsof
    strace
    iproute2
    dnsutils
  ];

  users.users.panda = {
    isNormalUser = true;
    extraGroups = [
      "wheel"
      "networkmanager"
      "panda-mem"
    ];
    # No default password: the installer sets a hashed one via local.nix;
    # otherwise firstboot.nix prompts for one on the console.
  };

  # PandaCode Connect: the box shows up in the owner's PandaCode app once
  # signed in (`panda-connect sign-in`); idle until then.
  services.pandacode-host.enable = lib.mkDefault true;
  # Superseded by PandaCode Connect; kept as an optional generic job runner.
  services.panda-agentd = {
    enable = lib.mkDefault false;
    # Placeholder relay; set your own when enabling.
    relayUrl = lib.mkDefault "https://connect.pandacode.invalid";
  };
  # The agent runtime (components/agent-env: agents.slice, oomd, the
  # /etc/panda/mcp.d registry) is on through pandaos.agents.enable's default.
  # Workspace checkpoints in ~/work (btrfs; plain dirs elsewhere).
  services.panda-ckpt.enable = lib.mkDefault true;
  # Read-only box status MCP server; mutations stay off.
  services.panda-sys.enable = lib.mkDefault true;
  services.panda-mem = {
    enable = lib.mkDefault true;
    # Users allowed on the panda-mem socket.
    clients = [ "panda" ] ++ lib.optional config.services.panda-agentd.enable "panda-agent";
  };
}
