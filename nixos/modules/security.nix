# PandaOS security defaults: nftables firewall, key-only ssh + sshguard,
# kernel sysctl hardening, wheel-only sudo.
{ lib, ... }:
{
  # nftables backend for the NixOS firewall. Inbound: only ssh (the openssh
  # module opens 22). agentd only dials out to the relay, so it needs no port.
  networking.nftables.enable = true;
  networking.firewall = {
    enable = true;
    allowedTCPPorts = lib.mkDefault [ ];
    allowedUDPPorts = lib.mkDefault [ ];
  };

  services.openssh.settings.MaxAuthTries = 3;

  # sshguard over fail2ban: a few small C processes (~5-10 MB incl. its
  # journalctl follower) vs a ~40 MB Python daemon. Blocks via nft sets.
  services.sshguard.enable = true;

  boot.kernel.sysctl = {
    # Hide kernel pointers and the kernel log from unprivileged users.
    "kernel.kptr_restrict" = 2;
    "kernel.dmesg_restrict" = 1;
    # No eBPF for non-root (agentd/systemd run BPF as root, unaffected).
    "kernel.unprivileged_bpf_disabled" = 1;
    "net.core.bpf_jit_harden" = 2;

    # Anti-spoofing; matches the firewall's default strict reverse-path check.
    "net.ipv4.conf.all.rp_filter" = 1;
    "net.ipv4.conf.default.rp_filter" = 1;

    # No ICMP redirects or source routing.
    "net.ipv4.conf.all.accept_redirects" = 0;
    "net.ipv4.conf.default.accept_redirects" = 0;
    "net.ipv4.conf.all.secure_redirects" = 0;
    "net.ipv4.conf.default.secure_redirects" = 0;
    "net.ipv4.conf.all.send_redirects" = 0;
    "net.ipv4.conf.default.send_redirects" = 0;
    "net.ipv6.conf.all.accept_redirects" = 0;
    "net.ipv6.conf.default.accept_redirects" = 0;
    "net.ipv4.conf.all.accept_source_route" = 0;
    "net.ipv6.conf.all.accept_source_route" = 0;

    # Link/file creation races in sticky world-writable dirs (/tmp).
    "fs.protected_symlinks" = 1;
    "fs.protected_hardlinks" = 1;
    "fs.protected_fifos" = 2;
    "fs.protected_regular" = 2;
  };

  security.sudo = {
    execWheelOnly = true;
    wheelNeedsPassword = true;
  };

  # AppArmor stays off: it ships no enforcing profiles for our services, and
  # security.apparmor pulls apparmor-utils (Python, ~100 MB closure) plus a
  # policy-load unit at boot. Revisit once agent jobs get real profiles.
}
