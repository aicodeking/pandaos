{ lib, buildGoModule }:
buildGoModule {
  pname = "panda-agentd";
  version = "0.1.0";
  src = ./.;
  vendorHash = null;
  subPackages = [ "." "cmd/fake-relay" ];
  ldflags = [ "-s" "-w" ];
  # The root package builds as `agentd` (module path basename).
  postInstall = ''
    mv $out/bin/agentd $out/bin/panda-agentd
  '';
  meta = {
    description = "PandaOS agent daemon: pairs with PandaCode and runs agent jobs";
    license = lib.licenses.mit;
    mainProgram = "panda-agentd";
    platforms = lib.platforms.linux;
  };
}
