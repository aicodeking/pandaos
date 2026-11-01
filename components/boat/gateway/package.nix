{ lib, buildGoModule }:
buildGoModule {
  pname = "boat-gateway";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.fileFilter (f: f.hasExt "go" || f.name == "go.mod") ./.;
  };
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  meta = {
    description = "boat HTTP router: per-VM port routes and signed desktop links behind Caddy";
    mainProgram = "boat-gateway";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
  };
}
