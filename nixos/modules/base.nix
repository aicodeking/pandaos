# PandaOS base: lean defaults shared by every profile.
{ lib, pkgs, ... }:
{
  imports = [
    ./security.nix
    ./updates.nix
    ./firstboot.nix
    ./snapshots.nix
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

  nix = {
    settings = {
      auto-optimise-store = true;
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
    git
    curl
    rsync
    htop
    ncdu
    tmux
    vim
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

  services.panda-agentd = {
    enable = lib.mkDefault true;
    # PandaConnect isn't defined yet; placeholder relay.
    relayUrl = lib.mkDefault "https://connect.pandacode.invalid";
  };
  services.panda-mem.enable = lib.mkDefault true;
}
