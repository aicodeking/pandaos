# T3 Code Connect host (services.t3code-host, t3-connect). `machine` runs the
# real T3 Code beside pandacode-host, not signed in; `t3only` runs it with
# pandacode-host disabled, so services.t3code-host creates the `agent` user and
# its home itself (ownAgent) and that whole evaluation + runtime path is booted.
# `stub` swaps in a fake `t3` (stubT3 below) whose `connect link` succeeds
# offline, so the signed-in lifecycle of t3-connect and the sign-in unit runs
# end to end: sign-in, stop/start resume, publish-only, a cancelled sign-in,
# pair/devices/status --live output sanitising, approve/revoke, unlink and
# sign-out (failed ones keep the sign-in), and a graceful cgroup-wide stop.
#
# Not covered (needs the T3 Code Connect cloud and a Cloudflare tunnel, which
# CI can neither reach nor own): a real completed sign-in, `connect` or
# `publish-only` mode serving through cloudflared, approving a real device and
# unlinking it. With the real `t3` only the headless sign-in failing cleanly
# offline, and the loopback-only `local` mode, which needs no network.
{ pkgs, self, t }:
let
  stubVersion = "0.1.11";

  # Stands in for `t3 serve`: 200 on /, the version on the environment
  # endpoint, exit 130 on SIGTERM like T3's NodeRuntime, after printing a
  # shutdown line and a secret (the redactor must drain and redact it). Its
  # worker child writes $T3CODE_HOME/child-got-sigterm when it gets SIGTERM,
  # proving the stop signals the whole cgroup gracefully (KillMode=control-group)
  # rather than SIGKILLing everything but the main process.
  stubServer = pkgs.writeText "t3-stub-server.py" ''
    import http.server, json, signal, subprocess, sys

    CHILD = """
    import os, signal, sys, time


    def term(*_):
        home = os.environ.get("T3CODE_HOME", "/nonexistent")
        os.makedirs(home, exist_ok=True)
        with open(os.path.join(home, "child-got-sigterm"), "w") as f:
            f.write("SIGTERM")
        sys.exit(0)


    signal.signal(signal.SIGTERM, term)
    while True:
        time.sleep(3600)
    """


    def shutdown(*_):
        print("stub: shutting down", flush=True)
        print("Token: shutdownstubtok", flush=True)
        sys.exit(130)


    signal.signal(signal.SIGTERM, shutdown)
    worker = subprocess.Popen([sys.executable, "-c", CHILD])


    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/.well-known/t3/environment":
                body, ctype = json.dumps({"serverVersion": "${stubVersion}"}).encode(), "application/json"
            else:
                body, ctype = b"<!doctype html><html></html>", "text/html"
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass


    http.server.HTTPServer(("127.0.0.1", 3773), H).serve_forever()
  '';

  # A fake `t3`: every subcommand t3-connect and the units call, offline. It
  # prints secrets (to prove redaction) and ANSI escapes (to prove sanitising).
  stubT3 = (pkgs.writeShellApplication {
    name = "t3";
    runtimeInputs = [ pkgs.coreutils pkgs.python3 ];
    text = ''
      case "''${1:-} ''${2:-}" in
        "--version "*) echo "${stubVersion}" ;;
        "connect link")
          echo "Open https://app.t3.codes/device"
          echo "Code: ABCD-1234"
          echo "Token: stubsignintok"
          if [ -e "''${T3CODE_HOME:-/nonexistent}/hang-signin" ]; then
            sleep 60 &
            s=$!
            trap 'kill "$s" 2>/dev/null; exit 143' TERM
            echo "Waiting for approval"
            wait "$s"
          fi
          ;;
        "connect unlink")
          if [ -e "''${T3CODE_HOME:-/nonexistent}/fail-unlink" ]; then
            echo "unlink offline" >&2
            exit 1
          fi
          ;;
        "connect logout")
          if [ -e "''${T3CODE_HOME:-/nonexistent}/fail-logout" ]; then
            echo "logout offline" >&2
            exit 1
          fi
          ;;
        "connect status") printf '%s\n' '{"linked":true,"note":"ok\u001b[31m"}' ;;
        "serve "*)
          echo "Token: stubservetok"
          exec python3 ${stubServer}
          ;;
        "pair "*)
          echo "Pairing URL: http://127.0.0.1:3773/?token=stubpairtok"
          # CSI, OSC (window title), CR (overwrite) and UTF-8 C1 CSI.
          printf '\033[31mRED\033[0m\n'
          printf '\033]0;pwned\007\n'
          printf 'fake\rprompt\n'
          printf '\302\23331mC1\n'
          ;;
        "auth connect-device")
          case "''${3:-}" in
            list)
              printf 'device-1 \033[31mLABEL\033[0m\n'
              printf '\033]0;title\007device-2\rspoof\n'
              printf 'device-3 \302\233C1\n'
              ;;
            approve) echo "approved ''${4:-}" ;;
            revoke) echo "revoked ''${4:-}" ;;
            *)
              echo "t3 (stub): unsupported: $*" >&2
              exit 2
              ;;
          esac
          ;;
        *)
          echo "t3 (stub): unsupported: $*" >&2
          exit 2
          ;;
      esac
    '';
  }).overrideAttrs { version = stubVersion; };
in
pkgs.testers.runNixOSTest {
  name = "t3code-host";
  nodes.machine = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      (t.nodeName "machine")
      t.noFirstboot
    ];
    virtualisation.memorySize = 2048;
    environment.systemPackages = [ pkgs.curl ];
  };
  # Signed-in lifecycle against stubT3 (offline).
  nodes.stub = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      (t.nodeName "stub")
      t.noFirstboot
    ];
    virtualisation.memorySize = 1024;
    environment.systemPackages = [ pkgs.curl ];
    services.pandacode-host.enable = pkgs.lib.mkForce false;
    services.t3code-host.cloudflaredPackage = null;
    services.t3code-host.package = stubT3;
  };
  nodes.t3only = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      (t.nodeName "t3only")
      t.noFirstboot
    ];
    virtualisation.memorySize = 2048;
    environment.systemPackages = [ pkgs.curl ];
    services.pandacode-host.enable = pkgs.lib.mkForce false;
  };
  testScript =
    { nodes, ... }:
    let
      host = nodes.machine.services.t3code-host;
      home = nodes.machine.users.users.${host.user}.home;
    in
    t.prelude
    + ''
      marker = "${host.enabledMarker}"
      port = ${toString host.port}
      unit = "t3code-host.service"


      def status():
          # Must work without root: run it as the host user.
          return json.loads(machine.succeed("runuser -u ${host.user} -- t3-connect status --json"))


      def show(prop, u=unit):
          return machine.succeed(f"systemctl show -p {prop} --value {u}").strip()


      def assert_off():
          st = status()
          assert st["enabled"] is False and st["service"] == "inactive", st
          machine.fail(f"test -e {marker}")


      machine.wait_for_unit("multi-user.target")

      with subtest("real t3 and cloudflared binaries execute as ${host.user}"):
          # package.nix sets dontStrip, which keeps the Node SEA blob appended to
          # the binary, while autoPatchelfHook patches its dynamic deps. `t3
          # --version` checks the app version (${host.package.version}): bare
          # Node (the SEA blob dropped) would print its own v26.x.x instead.
          out = machine.succeed("runuser -u ${host.user} -- env HOME=${home} ${host.package}/bin/t3 --version 2>&1")
          print(out)
          assert "${host.package.version}" in out, out
          out = machine.succeed("runuser -u ${host.user} -- env HOME=${home} ${host.cloudflaredPackage}/bin/cloudflared --version 2>&1")
          print(out)
          assert "2026.10.4" in out, out

      with subtest("disabled by default: status, skipped start, no restarts, healthy"):
          st = status()
          assert st["enabled"] is False and st["signedIn"] is False, st
          assert st["service"] == "inactive", st
          assert st["host"] == "${host.host}" == "127.0.0.1" and st["port"] == port == 3773, st
          machine.fail(f"test -e {marker}")
          # Starting it by hand is a no-op: the unit's condition skips it.
          machine.succeed(f"systemctl start {unit}")
          time.sleep(10)
          assert show("ConditionResult") == "no", show("ConditionResult")
          assert show("ActiveState") == "inactive", show("ActiveState")
          assert int(show("NRestarts")) == 0, show("NRestarts")
          machine.succeed("panda-health --settle 10")

      with subtest("hardening: no TIOCSTI injection, sandboxed host and sign-in units"):
          # An agent process must not push keystrokes into the operator's tty.
          tiocsti = machine.succeed("sysctl -n dev.tty.legacy_tiocsti").strip()
          assert tiocsti == "0", tiocsti
          for u in (unit, "t3code-sign-in.service"):
              for prop, want in (
                  ("User", "${host.user}"),
                  ("NoNewPrivileges", "yes"),
                  ("ProtectSystem", "strict"),
                  ("PrivateDevices", "yes"),
              ):
                  got = show(prop, u)
                  assert got == want, f"{u}: {prop}={got!r}, want {want!r}"
          # TERM to every process in the cgroup (children shut down gracefully);
          # the redactor ignores TERM and drains until its input closes.
          assert show("KillMode") == "control-group", show("KillMode")

      with subtest("t3-connect start: real t3 serve, loopback only, HTTP, redacted, pairs, in agents.slice"):
          machine.succeed("t3-connect start")
          machine.wait_for_unit(unit)
          machine.succeed(f"test -e {marker}")
          st = status()
          assert st["enabled"] is True and st["mode"] == "local" and st["service"] == "active", st

          assert show("Slice") == "agents.slice", show("Slice")
          assert show("Delegate") == "yes", show("Delegate")
          cg = show("ControlGroup")
          machine.succeed(f"test -d /sys/fs/cgroup{cg}/host")

          machine.wait_for_open_port(port, addr="127.0.0.1", timeout=120)
          listen = machine.succeed(f"ss -ltnH 'sport = :{port}'").strip()
          print(listen)
          assert listen, "nothing listens on the T3 port"
          for line in listen.splitlines():
              local = line.split()[3]
              assert local == f"127.0.0.1:{port}", f"non-loopback listener: {line}"
          # t3 serve binds the TCP socket before migrations finish and answers 503
          # (rejectRequestsUntilServing) until HTTP serving activates.
          machine.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=60)
          html = machine.succeed(f"curl -fsS http://127.0.0.1:{port}/")
          assert "<html" in html.lower() or "<!doctype html" in html.lower(), html[:200]
          env = json.loads(machine.succeed(f"curl -fsS http://127.0.0.1:{port}/.well-known/t3/environment"))
          print(env)
          # Only now: during startup/migrations the version probe may get a 503.
          st = status()
          assert st.get("runningVersion") == "${host.package.version}" == "0.1.11", st

          out = machine.succeed("t3-connect pair --ttl 5m --label ci-test 2>/dev/null")
          print(out)
          assert re.search(r"https?://\S+|[Tt]oken", out), f"no pairing URL/token on stdout: {out}"

          journal = machine.succeed(f"journalctl -o cat -u {unit}")
          # t3 serve prints its pairing token at startup: redaction must have
          # fired on it, so the negative checks below cannot pass vacuously.
          assert "[redacted]" in journal, journal
          for pat in (r"Token: [A-Za-z0-9]", r"token=[A-Za-z0-9]"):
              m = re.search(pat, journal)
              assert m is None, f"unredacted secret in the journal: {m.group(0)}..."

          machine.succeed("panda-health --settle 10")

          # A failed sign-in while local runs: the sign-in unit stops the host
          # (Conflicts=), fails offline, and its ExecStopPost brings the host
          # back in local mode.
          rc, out = machine.execute(
              "timeout 300 systemd-run --quiet --pipe --wait --collect t3-connect sign-in 2>&1"
          )
          print(out)
          assert rc != 0 and rc != 124, f"rc={rc}: {out}"
          machine.wait_for_unit(unit)
          machine.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=120)
          st = status()
          assert st["enabled"] is True and st["mode"] == "local" and st["service"] == "active", st
          machine.succeed(f"test -e {marker}")

          # stop cancels a pending sign-in too, and nothing restarts the host after.
          machine.succeed("timeout 30 t3-connect sign-in --no-wait 2>&1")
          machine.succeed("t3-connect stop")
          machine.wait_until_fails("systemctl is-active t3code-sign-in.service", timeout=60)
          machine.wait_until_fails(f"systemctl is-active {unit}", timeout=60)
          time.sleep(5)
          assert show("ActiveState", "t3code-sign-in.service") in ("inactive", "failed")
          assert show("ActiveState") in ("inactive", "failed"), show("ActiveState")
          assert_off()

      # The sign-in runs as t3code-sign-in.service with no terminal: here with
      # no network, so it must fail cleanly instead of waiting on a prompt
      # (e.g. cloudflared's interactive install confirmation).
      with subtest("headless t3-connect sign-in offline: clean failure, no prompt, no marker"):
          rc, out = machine.execute(
              "timeout 300 systemd-run --quiet --pipe --wait --collect t3-connect sign-in 2>&1"
          )
          print(out)
          assert rc != 0 and rc != 124, f"rc={rc}: {out}"
          journal = machine.succeed("journalctl -o cat -u t3code-sign-in.service")
          print(journal)
          assert "Download and install version" not in journal, journal
          assert "Download and install version" not in out, out
          assert_off()
          machine.succeed("panda-health --settle 10")

          # --no-wait: returns at once, the unit goes on alone.
          out = machine.succeed("timeout 30 t3-connect sign-in --no-wait 2>&1")
          assert "journalctl -u t3code-sign-in" in out, out
          machine.wait_until_fails("systemctl is-active t3code-sign-in.service", timeout=300)
          assert show("ActiveState", "t3code-sign-in.service") in ("inactive", "failed")
          assert_off()

      # T3-only box: services.pandacode-host disabled, so services.t3code-host
      # owns the `agent` user, its home and panda-mem membership.
      t3only.wait_for_unit("multi-user.target")

      with subtest("t3only (pandacode-host disabled): agent user, home, panda-mem, local host in agents.slice"):
          t3only.succeed("id ${host.user}")
          perm = t3only.succeed("stat -c '%a %U:%G' ${home}").strip()
          assert perm == "700 ${host.user}:${host.user}", perm
          groups = t3only.succeed("id -nG ${host.user}").split()
          assert "panda-mem" in groups, groups

          t3only.succeed("t3-connect start")
          t3only.wait_for_unit(unit)
          t3only.succeed(f"test -e {marker}")
          assert t3only.succeed(f"systemctl show -p Slice --value {unit}").strip() == "agents.slice"
          assert t3only.succeed(f"systemctl show -p Delegate --value {unit}").strip() == "yes"
          cg = t3only.succeed(f"systemctl show -p ControlGroup --value {unit}").strip()
          t3only.succeed(f"test -d /sys/fs/cgroup{cg}/host")
          t3only.wait_for_open_port(port, addr="127.0.0.1", timeout=120)
          t3only.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=60)
          code = t3only.succeed(f"curl -sS -o /dev/null -w '%{{http_code}}' http://127.0.0.1:{port}/").strip()
          assert code == "200", code
          t3only.succeed("panda-health --settle 10")

          t3only.succeed("t3-connect stop")
          t3only.wait_until_fails(f"systemctl is-active {unit}", timeout=60)
          assert t3only.succeed(f"systemctl show -p ActiveState --value {unit}").strip() == "inactive"
          t3only.fail(f"test -e {marker}")

      # Signed-in lifecycle against stubT3: `connect link` succeeds offline (or,
      # with hang-signin, waits like a pending device code).
      stub.wait_for_unit("multi-user.target")
      last_mode_file = "/var/lib/t3code-host/last-mode"
      sign_in_unit = "t3code-sign-in.service"


      def stub_status(args="--json"):
          return json.loads(stub.succeed(f"runuser -u ${host.user} -- t3-connect status {args}"))


      def stub_show(prop, u=unit):
          return stub.succeed(f"systemctl show -p {prop} --value {u}").strip()


      def stub_last_mode():
          return stub.succeed(f"cat {last_mode_file}").strip()


      def stub_serving(mode, signed_in):
          stub.wait_for_unit(unit)
          stub.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=60)
          stub.succeed(f"test -e {marker}")
          st = stub_status()
          assert st["enabled"] is True and st["signedIn"] is signed_in, st
          assert st["signingIn"] is False, st
          assert st["mode"] == mode and st["service"] == "active", st
          assert st["runningVersion"] == "${nodes.stub.services.t3code-host.package.version}" == "0.1.11", st
          return st


      def stub_off(signed_in):
          st = stub_status()
          assert st["enabled"] is False and st["signedIn"] is signed_in, st
          assert st["signingIn"] is False, st
          assert st["mode"] == "off" and st["service"] == "inactive", st
          stub.fail(f"test -e {marker}")
          return st


      def assert_clean(out):
          # No CSI/OSC introducer, BEL, CR or UTF-8 C1 CSI reaches root's terminal.
          for bad in ("\x1b", "\x07", "\r", "\u009b"):
              assert bad not in out, f"{bad!r} in {out!r}"


      with subtest("stub: signed-in lifecycle (sign-in, stop/start, publish-only, cancel, pair, unlink, sign-out)"):
          out = stub.succeed("${nodes.stub.services.t3code-host.package}/bin/t3 --version")
          assert out.strip() == "0.1.11", out

          # 1. sign-in: the done hook writes the connect marker and last-mode,
          # starts the host; the device code is in the journal, the token is not.
          out = stub.succeed("timeout 120 t3-connect sign-in 2>&1")
          print(out)
          stub_serving("connect", True)
          assert stub_last_mode() == "connect", stub_last_mode()
          stub.wait_until_succeeds(f"journalctl -o cat -u {sign_in_unit} | grep -qF 'Code: ABCD-1234'", timeout=30)
          journal = stub.succeed(f"journalctl -o cat -u {sign_in_unit}")
          print(journal)
          assert "[redacted]" in journal, journal
          assert "stubsignintok" not in journal, journal
          stub.wait_until_succeeds(f"journalctl -o cat -u {unit} | grep -qF 'Token: [redacted]'", timeout=30)
          journal = stub.succeed(f"journalctl -o cat -u {unit}")
          assert "stubservetok" not in journal, journal

          # 2. stop: off, still signed in; last-mode kept. KillMode=control-group
          # TERMs the server's worker child too (not a bare SIGKILL), and the
          # redactor drains and redacts the server's shutdown output.
          child_marker = "${home}/.t3/child-got-sigterm"
          stub.succeed(f"rm -f {child_marker}")
          stub.succeed("t3-connect stop")
          stub_off(True)
          assert stub_last_mode() == "connect", stub_last_mode()
          stub.wait_until_succeeds(f"test -e {child_marker}", timeout=10)
          stub.wait_until_succeeds(f"journalctl -o cat -u {unit} | grep -qF 'stub: shutting down'", timeout=30)
          journal = stub.succeed(f"journalctl -o cat -u {unit}")
          assert "shutdownstubtok" not in journal, journal

          # 3. start resumes connect.
          stub.succeed("t3-connect start")
          stub_serving("connect", True)

          # 4. publish-only survives stop -> start.
          stub.succeed("timeout 120 t3-connect sign-in --publish-only 2>&1")
          stub_serving("publish-only", True)
          assert stub_last_mode() == "publish-only", stub_last_mode()
          stub.succeed("t3-connect stop")
          stub_off(True)
          stub.succeed("t3-connect start")
          stub_serving("publish-only", True)

          # 5. A pending (managed) sign-in cancelled by stop: nothing restarts
          # the host and last-mode is not overwritten with connect.
          stub.succeed("install -d -o ${host.user} -g ${host.user} -m 0700 ${home}/.t3")
          stub.succeed("runuser -u ${host.user} -- touch ${home}/.t3/hang-signin")
          out = stub.succeed("timeout 30 t3-connect sign-in --no-wait 2>&1")
          assert "journalctl -u t3code-sign-in" in out, out
          stub.wait_until_succeeds(f"systemctl is-active {sign_in_unit}", timeout=60)
          stub.wait_until_succeeds(f"journalctl -o cat -u {sign_in_unit} | grep -qF 'Waiting for approval'", timeout=60)
          assert stub_show("ActiveState", sign_in_unit) == "active", stub_show("ActiveState", sign_in_unit)
          assert stub_show("ActiveState") == "inactive", stub_show("ActiveState")
          st = stub_status()
          assert st["signingIn"] is True, st
          out = stub.succeed("t3-connect logs --sign-in -o cat -b")
          assert "Code: ABCD-1234" in out, out
          stub.succeed("t3-connect stop")
          stub.wait_until_fails(f"systemctl is-active {sign_in_unit}", timeout=60)
          assert stub_status()["signingIn"] is False
          assert stub_show("ActiveState") == "inactive", stub_show("ActiveState")
          time.sleep(5)
          assert stub_show("ActiveState", sign_in_unit) in ("inactive", "failed"), stub_show("ActiveState", sign_in_unit)
          assert stub_show("ActiveState") == "inactive", stub_show("ActiveState")
          stub_off(True)
          assert stub_last_mode() == "publish-only", stub_last_mode()
          stub.succeed("rm -f ${home}/.t3/hang-signin")

          # 6. pair, devices and status --live reach root's terminal sanitised
          # (CSI, OSC, CR, C1); approve and revoke pass the device code through.
          stub.succeed("t3-connect start")
          stub_serving("publish-only", True)
          out = stub.succeed("t3-connect pair 2>/dev/null")
          print(repr(out))
          assert "http://127.0.0.1:3773/?token=stubpairtok" in out, out
          assert_clean(out)
          out = stub.succeed("t3-connect devices 2>&1")
          print(repr(out))
          assert "device-1" in out, out
          assert_clean(out)
          out = stub.succeed("t3-connect status --live")
          print(repr(out))
          assert "linked: true" in out, out
          assert_clean(out)
          st = json.loads(stub.succeed("t3-connect status --live --json"))
          assert st["cloud"]["linked"] is True, st
          out = stub.succeed("t3-connect approve K7QM-2XFD 2>&1")
          assert "approved K7QM-2XFD" in out, out
          out = stub.succeed("t3-connect revoke K7QM-2XFD 2>&1")
          assert "revoked K7QM-2XFD" in out, out

          # 7a. A failed unlink (cloud unreachable) must not claim signedIn:false
          # nor drop last-mode: start still resumes publish-only.
          stub.succeed("runuser -u ${host.user} -- touch ${home}/.t3/fail-unlink")
          out = stub.fail("t3-connect unlink 2>&1")
          print(out)
          stub_off(True)
          assert stub_last_mode() == "publish-only", stub_last_mode()
          stub.succeed("rm -f ${home}/.t3/fail-unlink")
          stub.succeed("t3-connect start")
          stub_serving("publish-only", True)

          # 7. unlink: marker and last-mode gone, host stopped; start is local.
          stub.succeed("t3-connect unlink")
          stub_off(False)
          stub.fail(f"test -e {last_mode_file}")
          stub.succeed("t3-connect start")
          stub_serving("local", False)
          stub.fail(f"test -e {last_mode_file}")

          # 8. sign-out from a signed-in host: off, signed out, nothing to resume.
          stub.succeed("timeout 120 t3-connect sign-in 2>&1")
          stub_serving("connect", True)
          # 8a. A failed sign-out keeps the sign-in and last-mode.
          stub.succeed("runuser -u ${host.user} -- touch ${home}/.t3/fail-logout")
          out = stub.fail("t3-connect sign-out 2>&1")
          print(out)
          stub_off(True)
          assert stub_last_mode() == "connect", stub_last_mode()
          stub.succeed("rm -f ${home}/.t3/fail-logout")
          stub.succeed("t3-connect start")
          stub_serving("connect", True)
          stub.succeed("t3-connect sign-out")
          stub_off(False)
          stub.fail(f"test -e {last_mode_file}")
          stub.succeed("t3-connect start")
          stub_serving("local", False)
          stub.succeed("t3-connect stop")
          stub_off(False)
    '';
}
