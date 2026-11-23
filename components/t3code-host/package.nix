# T3 Code's release tarball: `t3`, a Node.js v26 Single Executable Application
# (the app is a ~20.9 MB blob in an ELF note segment), plus the files it loads
# at runtime from the directory of process.execPath (import.meta.dirname in
# the SEA, which does NOT resolve symlinks): client/ (web UI), node_modules/
# (native addons), lib/ (bundled libatomic) and resource-monitor/. Hence
# bin/t3 is an exec wrapper, not a symlink.
# Bump: change version and hash (`nix hash convert --to sri sha256:<hex>`), and
# re-pin cloudflared to what packages/shared/src/relayClient.ts downloads.
{
  lib,
  stdenv,
  fetchurl,
  autoPatchelfHook,
  runtimeShell,
}:
let
  version = "0.1.11";

  # T3 Connect's tunnel. Pinned to the exact build T3 Code v0.1.11 would fetch
  # itself (relayClient.ts); the module points T3CODE_CLOUDFLARED_PATH here so
  # `t3 connect link --headless` never asks to download one.
  cloudflared = stdenv.mkDerivation rec {
    pname = "cloudflared";
    version = "2026.10.4";
    src = fetchurl {
      url = "https://github.com/cloudflare/cloudflared/releases/download/${version}/cloudflared-linux-amd64";
      # sha256 ae5e8add7884b0531d66bbca4570d1de491ff6da51e4ebe7eb160cdf241d9e42
      hash = "sha256-rl6K3XiEsFMdZrvKRXDR3kkf9tpR5Ovn6xYM3yQdnkI=";
    };
    dontUnpack = true;
    dontConfigure = true;
    dontBuild = true;
    # A static Go binary: nothing to patch, and stripping breaks Go's buildinfo.
    dontPatchELF = true;
    dontStrip = true;
    installPhase = ''
      runHook preInstall
      install -Dm0755 "$src" "$out/bin/cloudflared"
      runHook postInstall
    '';
    doInstallCheck = true;
    installCheckPhase = ''
      runHook preInstallCheck
      "$out/bin/cloudflared" --version | grep -Fq ${version}
      runHook postInstallCheck
    '';
    meta = {
      description = "Cloudflare Tunnel client (T3 Connect relay)";
      homepage = "https://github.com/cloudflare/cloudflared";
      license = lib.licenses.asl20;
      mainProgram = "cloudflared";
      platforms = [ "x86_64-linux" ];
      sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    };
  };
in
stdenv.mkDerivation {
  pname = "t3code";
  inherit version;

  src = fetchurl {
    url = "https://github.com/pingdotgg/t3code/releases/download/v${version}/t3-${version}-linux-x64.tar.gz";
    # sha256 2b691c07f5bcb5adea4a863d5324aae7db21bd6d82280cec7787836dcaa013e1
    hash = "sha256-K2kcB/W8ta3qSoY9UySq59shvW2CKAzsd4eDbcqgE+E=";
  };
  sourceRoot = "t3-${version}-linux-x64";

  nativeBuildInputs = [ autoPatchelfHook ];
  buildInputs = [ stdenv.cc.cc.lib ];
  # Voice input's Vulkan backend; optional, the CPU backends load without it.
  autoPatchelfIgnoreMissingDeps = [ "libvulkan.so.1" ];

  dontConfigure = true;
  dontBuild = true;
  # Stripping would drop the SEA blob's note segment: `t3` would start as bare node.
  dontStrip = true;

  installPhase = ''
    runHook preInstall
    # musl builds of the native addons: never loaded on glibc, and patchelf
    # cannot resolve their musl libc.
    find node_modules -depth \( -name '*-musl' -o -name 'musl_*' -o -name '*musl*.node' \) \
      -exec rm -rf {} +
    mkdir -p "$out/lib/t3" "$out/bin"
    cp -a t3 client node_modules lib resource-monitor "$out/lib/t3/"
    # T3 resolves client/ (and node_modules/, lib/, resource-monitor/) from
    # import.meta.dirname, which the SEA derives from process.execPath without
    # resolving symlinks. A symlink (bin/t3, or /run/current-system/sw/bin/t3)
    # would make that dirname bin/ and `t3 serve` 503s on `/`. exec-ing the real
    # binary by absolute path pins execPath to $out/lib/t3/t3.
    cat > "$out/bin/t3" <<EOF
    #!${runtimeShell}
    exec "$out/lib/t3/t3" "\$@"
    EOF
    chmod 0755 "$out/bin/t3"
    runHook postInstall
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    test -f "$out/lib/t3/client/index.html"
    # Bare Node (the SEA blob dropped) prints v26.x.x, not the T3 version.
    HOME="$TMPDIR" T3CODE_HOME="$TMPDIR/.t3" "$out/bin/t3" --version | grep -Fq ${version}
    runHook postInstallCheck
  '';

  passthru = { inherit cloudflared; };

  meta = {
    description = "T3 Code, run here as a headless T3 Code Connect host (t3 serve)";
    homepage = "https://github.com/pingdotgg/t3code";
    license = lib.licenses.mit;
    mainProgram = "t3";
    platforms = [ "x86_64-linux" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}
