# Server: headless + boat (persistent Firecracker microVMs for agents) on a
# dedicated machine. See docs/boat.md for DNS, TLS and sizing.
#
# Set in /etc/pandaos/local.nix (panda-install --profile server writes them):
#   services.boat.domain = "boat.example.com";
#   services.boat.acmeEmail = "ops@example.com";
# Until then the domain is boat.localhost, which Caddy serves with its own
# local CA (no ACME requests for a name nobody owns).
#
# Safe by default: services.boat.untrustedTenants is on (SMT off, CPU mitigations,
# per-VM abuse limits, memOvercommit 1, KSM and warmPool refused; PROTOCOL.md).
# Opt out on a host whose tenants you all trust (trusted density: SMT, memory
# overcommit 1.5 with the balloon, the warm pool, quotas up to host capacity):
#   services.boat.untrustedTenants = false;
# Turning it on needs a reboot (kernel command line); until then boatd refuses
# to start, and boat-reboot-check reports `kernel-params` as pending.
# Hosting strangers? Set it to `true` explicitly too: the domain checks
# (separate vmDomain/desktopDomain, gateway.dnsProvider) then refuse to
# evaluate instead of warning, and updates default to `staged` (fetched,
# verified and built daily, applied by `sudo panda-update run`); set
# pandaos.updates.mode = "automatic" to undo that.
#
# Already on via the base modules: nftables firewall, key-only ssh, sshguard,
# daily signed-release updates with rollback (updates.nix).
{ config, lib, options, ... }:
{
  imports = [ ./headless.nix ];

  services.boat = {
    enable = true;
    domain = lib.mkDefault "boat.localhost";
    untrustedTenants = lib.mkDefault true;
  };
  # A host the operator declared untrusted-tenant (explicitly, not this profile's
  # default) changes only when its operator says so.
  pandaos.updates.mode = lib.mkIf (
    config.services.boat.untrustedTenants && options.services.boat.untrustedTenants.highestPrio < 1000
  ) (lib.mkDefault "staged");
  # Firewall: the gateway opens 80/443 (tcp) + 443 (udp, HTTP/3); the openssh
  # module opens 22 and the jump port 2222 (services.boat.sshGateway.port).

  # VM data disk: `panda-install --data-disk DEV` formats it btrfs (label
  # PANDADATA), writes /etc/pandaos/data-disk, and pins the mount by UUID in
  # /etc/pandaos/disk.nix without nofail (from the first nixos-rebuild on).
  # The prebuilt system copied at install time mounts by label with nofail,
  # so a box without a second disk still boots; there /var/lib/boat is a
  # btrfs subvolume on the system disk. boatd and boat-api refuse to start
  # when /etc/pandaos/data-disk exists but the disk is not mounted, so they
  # never write VM data to the bare mountpoint.
  fileSystems."/var/lib/boat" = {
    device = lib.mkDefault "/dev/disk/by-label/PANDADATA";
    fsType = "btrfs";
    options = lib.mkDefault [
      "compress=zstd:1"
      "noatime"
      "nofail"
      "x-systemd.device-timeout=30s"
    ];
  };

  # A dedicated VM host: full clock speed over power saving. On amd-pstate/intel_pstate
  # (active mode) `performance` also sets the energy-performance preference to performance.
  # Set "powersave" (with EPP balance_performance) to trade latency for watts.
  powerManagement.cpuFreqGovernor = lib.mkDefault "performance";
  # Guest memory on 2 MiB huge pages: off by default (no snapshots or forks then; see
  # services.boat.hugePages). The bench A/Bs it.

  # No name resolution by multicast on a server facing the internet: LLMNR (5355) and
  # mDNS (5353) answer anyone on the link. Only matters if systemd-resolved is turned on.
  services.resolved.settings.Resolve = {
    LLMNR = lib.mkDefault false;
    MulticastDNS = lib.mkDefault false;
  };
  services.avahi.enable = lib.mkDefault false;

  # Host metrics on 127.0.0.1:9100 only: they name units, mounts and tenant
  # load. Scrape through an ssh tunnel, or set listenAddress to an admin
  # (WireGuard) address and open the port on that interface only.
  # boat-reboot-check writes boat_reboot_pending to the textfile collector.
  services.prometheus.exporters.node = {
    enable = true;
    listenAddress = lib.mkDefault "127.0.0.1";
    enabledCollectors = [ "systemd" "textfile" ];
    extraFlags = [ "--collector.textfile.directory=/var/lib/boat-metrics" ];
  };
}
