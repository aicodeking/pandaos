{ lib, buildGoModule }:
buildGoModule {
  pname = "boat-api";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.fileFilter (f: f.hasExt "go" || f.name == "go.mod" || f.name == "openapi.json") ./.;
  };
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
