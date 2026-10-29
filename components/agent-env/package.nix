{ lib, buildGoModule }:

buildGoModule {
  pname = "panda-agents";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.fileFilter (f: f.hasExt "go" || f.name == "go.mod") ./.;
  };
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  meta = {
    description = "Wire agent CLIs to PandaOS: MCP registry sync, AGENTS.md, doctor";
    mainProgram = "panda-agents";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
  };
}
