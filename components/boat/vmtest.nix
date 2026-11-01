# boat host module + server profile, without Firecracker (test VMs rarely
# have nested KVM). checks.boat-module (tests/default.nix).
#
# - boatd: refuses to start with a clear error when /dev/kvm is absent
#   (exit 78, no restart loop); with /dev/kvm the stub boatd runs;
# - boat-secrets: 32-byte desktop key, 0600 boat:boat (boat-api + gateway);
# - boat-gateway: port routes from routes.json, re-read live; signed desktop
#   links -> cookie -> desktop port; the on-demand-TLS ask endpoint;
# - Caddy: real config (boat.localhost -> Caddy's local CA, works offline):
#   api. proxies to boat-api with /internal/* blocked; *.vm. routes via TLS;
# - ssh jump: -W only to permitopen targets, no shell, port 2222 jump-only.
{ pkgs, self }:
let
  # Stands in for boatd/boat-api: the module only needs them to run.
  stubBoatd = pkgs.writeShellScriptBin "boatd" ''
    test -r /etc/boat/boatd.json || exit 1
    exec sleep infinity
  '';
  # Records its flags (the module's contract with boat-api), then serves files.
  stubApi = pkgs.writeShellScriptBin "boat-api" ''
    printf '%s\n' "$@" >/var/lib/boat/api/args
    exec ${pkgs.python3}/bin/python3 -m http.server --bind 127.0.0.1 8740 --directory /etc/boat
  '';
  stubCli = pkgs.writeShellScriptBin "boat" "echo stub";
in
pkgs.testers.runNixOSTest {
  name = "boat-module";
  nodes.machine = { pkgs, ... }: {
    imports = [
      self.nixosModules.default
      self.nixosModules.server
    ];
    pandaos.firstboot.enable = false;
    # base.nix pins hostName = "pandaos"; the test driver names nodes.
    networking.hostName = pkgs.lib.mkForce "machine";
    # Local CA for *.localhost; don't try to edit the VM's trust store.
    services.caddy.globalConfig = "skip_install_trust";
    services.boat = {
      boatdPackage = stubBoatd;
      apiPackage = stubApi;
      cliPackage = stubCli;
    };
    # The "VM": an HTTP service the routes point at.
    systemd.services.fake-vm = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 -m http.server --bind 127.0.0.1 8000 --directory /etc/boat";
    };
    users.users.tester.isNormalUser = true;
  };

  testScript = ''
    import base64, hashlib, hmac, json, shlex, time

    D = "boat.localhost"
    machine.wait_for_unit("multi-user.target")

    with subtest("boatd: clear refusal without /dev/kvm, running with it"):
        if machine.execute("test -c /dev/kvm")[0] != 0:
            machine.wait_until_fails("systemctl is-active boatd.service")
            journal = machine.succeed("journalctl -u boatd.service --no-pager")
            assert "/dev/kvm is missing" in journal, journal
            time.sleep(8)
            n = machine.succeed("systemctl show -p NRestarts --value boatd.service").strip()
            assert n == "0", f"boatd restart loop: NRestarts={n}"
        else:
            machine.wait_for_unit("boatd.service")
        cfg = json.loads(machine.succeed("cat /etc/boat/boatd.json"))
        assert cfg["socket"] == "/run/boat/boatd.sock" and cfg["socketGroup"] == "boat", cfg
        assert cfg["firecracker"].endswith("/bin/firecracker") and cfg["jailer"].endswith("/bin/jailer"), cfg
        assert cfg["defaultSize"] in cfg["sizes"] and cfg["stateDir"] == "/var/lib/boat", cfg

    with subtest("secrets + dirs"):
        machine.wait_for_unit("boat-secrets.service")
        st = machine.succeed("stat -c '%a %U %G %s' /var/lib/boat/secrets/desktop.key").strip()
        assert st == "600 boat boat 32", st
        machine.wait_until_succeeds("test -s /var/lib/boat/api/args")
        args = machine.succeed("cat /var/lib/boat/api/args").split("\n")
        for flag, val in [("-desktop-url", f"https://desktop.{D}"), ("-ssh-jump", f"jump@ssh.{D}:2222"), ("-boatd", "/run/boat/boatd.sock")]:
            assert args[args.index(flag) + 1] == val, (flag, args)
        assert machine.succeed("stat -c '%a %U %G' /run/boat").strip() == "750 root boat"

    machine.succeed(
        "echo '{\"vms\":{\"vm1\":{\"ip\":\"127.0.0.1\",\"ports\":[8000],\"desktopPort\":8000}}}' >/run/boat/routes.json"
    )
    machine.wait_for_unit("boat-gateway.service")
    machine.wait_for_open_port(8742)
    machine.wait_for_open_port(8000)
    gw = "curl -s -o /dev/null -w '%{http_code}' "

    with subtest("gateway: port routes, live reload"):
        assert machine.succeed(gw + f"-H 'Host: 8000-vm1.vm.{D}' http://127.0.0.1:8742/boatd.json") == "200"
        assert machine.succeed(gw + f"-H 'Host: 9000-vm1.vm.{D}' http://127.0.0.1:8742/") == "404"
        assert machine.succeed(gw + f"'http://127.0.0.1:8743/ask?domain=8000-vm1.vm.{D}'") == "200"
        assert machine.succeed(gw + "'http://127.0.0.1:8743/ask?domain=evil.example'") == "404"

    with subtest("gateway: signed desktop links"):
        key = base64.b64decode(machine.succeed("base64 -w0 /var/lib/boat/secrets/desktop.key"))
        exp = int(time.time()) + 600
        sig = hmac.new(key, f"vm1.{exp}".encode(), hashlib.sha256).hexdigest()
        tok = f"{exp}.{sig}"
        h = f"-H 'Host: desktop.{D}' "
        assert machine.succeed(gw + h + "http://127.0.0.1:8742/vm1/") == "401"
        assert machine.succeed(gw + h + "'http://127.0.0.1:8742/vm1/?exp=1&sig=00'") == "403"
        hdrs = machine.succeed("curl -s -D - -o /dev/null " + h + f"'http://127.0.0.1:8742/vm1/?exp={exp}&sig={sig}'")
        assert "Set-Cookie: boat_gw=" in hdrs and "Path=/vm1/" in hdrs, hdrs
        assert machine.succeed(gw + h + f"-b 'boat_gw={tok}' http://127.0.0.1:8742/vm1/boatd.json") == "200"

    with subtest("caddy: TLS front (local CA for .localhost)"):
        machine.wait_for_unit("caddy.service")
        machine.wait_for_open_port(443)
        r = f"curl -sk -o /dev/null -w '%{{http_code}}' --resolve api.{D}:443:127.0.0.1 --resolve 8000-vm1.vm.{D}:443:127.0.0.1 "
        machine.wait_until_succeeds(r + f"https://api.{D}/boatd.json | grep -x 200", timeout=60)
        assert machine.succeed(r + f"https://api.{D}/internal/desktop-auth") == "404"
        machine.wait_until_succeeds(r + f"https://8000-vm1.vm.{D}/boatd.json | grep -x 200", timeout=60)

    with subtest("ssh jump: -W to permitopen only, no shell, 2222 is jump-only"):
        machine.wait_for_open_port(2222)
        machine.succeed("ssh-keygen -q -t ed25519 -N ''' -f /root/k")
        pub = machine.succeed("cat /root/k.pub").strip()
        lines = [
            f'restrict,port-forwarding,permitopen="127.0.0.1:22" {pub}',
            f'restrict,port-forwarding,pty,permitopen="127.0.0.1:2222" {pub}',  # rejected by the filter
        ]
        machine.succeed(f"printf '%s\\n' {' '.join(shlex.quote(l) for l in lines)} >/var/lib/boat/ssh/jump_authorized_keys")
        machine.succeed("chown boat:boat /var/lib/boat/ssh/jump_authorized_keys && chmod 640 /var/lib/boat/ssh/jump_authorized_keys")
        out = machine.succeed("/etc/boat/jump-keys /var/lib/boat/ssh/jump_authorized_keys")
        assert out.count("\n") == 1 and "127.0.0.1:22" in out, out
        ssh = "ssh -i /root/k -o BatchMode=yes -o StrictHostKeyChecking=no -p 2222 "
        banner = machine.succeed(f"timeout 10 {ssh} -W 127.0.0.1:22 jump@127.0.0.1 </dev/null | head -c 7 || true")
        assert banner == "SSH-2.0", banner
        machine.fail(f"timeout 10 {ssh} -W 127.0.0.1:2222 jump@127.0.0.1 </dev/null | grep -q SSH-")
        machine.fail(f"timeout 10 {ssh} jump@127.0.0.1 id")
        # tester's key works on 22, so the refusal on 2222 is AllowUsers.
        machine.succeed("install -d -o tester -m 700 ~tester/.ssh && install -o tester -m 600 /root/k.pub ~tester/.ssh/authorized_keys")
        machine.succeed("timeout 10 ssh -i /root/k -o BatchMode=yes -o StrictHostKeyChecking=no tester@127.0.0.1 true")
        machine.fail(f"timeout 10 {ssh} tester@127.0.0.1 true")

    with subtest("gateway: route removal is live"):
        machine.succeed("echo '{\"vms\":{}}' >/run/boat/routes.json")
        assert machine.succeed(gw + f"-H 'Host: 8000-vm1.vm.{D}' http://127.0.0.1:8742/") == "404"
  '';
}
