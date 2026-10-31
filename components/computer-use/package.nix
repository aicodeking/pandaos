# panda-cu (Go) plus panda-cu-atspi, the small Python AT-SPI helper it runs
# by argv. Input/capture tools come in on PATH: grim (wlr-screencopy), wtype
# (virtual-keyboard), wlrctl (foreign-toplevel), wl-clipboard (data-control).
{
  lib,
  buildGoModule,
  stdenv,
  makeWrapper,
  python3,
  gobject-introspection,
  wrapGAppsNoGuiHook,
  at-spi2-core,
  grim,
  wtype,
  wlrctl,
  wl-clipboard,
}:

let
  version = "0.1.0";
  # gobject-introspection's setup hook collects the Atspi/GLib typelibs of
  # buildInputs; wrapGAppsNoGuiHook bakes GI_TYPELIB_PATH into the wrapper.
  atspi = stdenv.mkDerivation {
    pname = "panda-cu-atspi";
    inherit version;
    src = ./atspi.py;
    dontUnpack = true;
    nativeBuildInputs = [ gobject-introspection wrapGAppsNoGuiHook ];
    buildInputs = [ at-spi2-core (python3.withPackages (ps: [ ps.pygobject3 ])) ];
    installPhase = "install -Dm755 $src $out/bin/panda-cu-atspi";
  };
in
buildGoModule {
  pname = "panda-cu";
  inherit version;
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.fileFilter (f: f.hasExt "go" || f.name == "go.mod") ./.;
  };
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram $out/bin/panda-cu --prefix PATH : ${lib.makeBinPath [ atspi grim wtype wlrctl wl-clipboard ]}
  '';
  passthru = { inherit atspi; };
  meta = {
    description = "Computer use for agents: screenshots, mouse, keyboard and AT-SPI over MCP stdio";
    mainProgram = "panda-cu";
    license = lib.licenses.mit;
  };
}
