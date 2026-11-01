{ lib, buildGoModule }:
buildGoModule {
  pname = "boat-api";
  version = "0.1.0";
  src = ./.;
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  # The module basename is `api`.
  postInstall = ''
    mv $out/bin/api $out/bin/boat-api
  '';
  meta = {
    description = "Boat public API server: auth, teams, secrets, webhooks in front of boatd";
    license = lib.licenses.mit;
    mainProgram = "boat-api";
    platforms = lib.platforms.linux;
  };
}
