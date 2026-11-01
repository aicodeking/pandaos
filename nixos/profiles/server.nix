# Server: headless + boat (persistent Firecracker microVMs for agents) on a
# dedicated machine. See docs/boat.md for DNS, TLS and sizing.
#
# Set in /etc/pandaos/local.nix (panda-install --profile server writes them):
#   services.boat.domain = "boat.example.com";
#   services.boat.acmeEmail = "ops@example.com";
# Until then the domain is boat.localhost, which Caddy serves with its own
# local CA (no ACME requests for a name nobody owns).
#
# Already on via the base modules: nftables firewall, key-only ssh, sshguard,
# daily unattended updates with rollback (updates.nix).
{ lib, ... }:
{
  imports = [ ./headless.nix ];

  services.boat = {
    enable = true;
    domain = lib.mkDefault "boat.localhost";
  };
  # Firewall: the gateway opens 80/443 (tcp) + 443 (udp, HTTP/3); the openssh
  # module opens 22 and the jump port 2222 (services.boat.sshGateway.port).

  # Optional second disk for VM data: `panda-install --data-disk DEV` formats
  # it btrfs, labelled PANDADATA. Without it, /var/lib/boat is a subvolume on
  # the system disk (tmpfiles `v`). nofail + short timeout: a missing disk
  # never blocks boot.
  fileSystems."/var/lib/boat" = {
    device = lib.mkDefault "/dev/disk/by-label/PANDADATA";
    fsType = "btrfs";
    options = [
      "compress=zstd:1"
      "noatime"
      "nofail"
      "x-systemd.device-timeout=5s"
    ];
  };

  # Host metrics on :9100, not opened in the firewall: scrape over ssh,
  # WireGuard or add the port to networking.firewall.interfaces.<if>.
  services.prometheus.exporters.node = {
    enable = true;
    enabledCollectors = [ "systemd" ];
  };
}
