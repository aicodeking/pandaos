# boat host module + server profile, without Firecracker (test VMs rarely
# have nested KVM). checks.boat-module (tests/default.nix).
#
# - boatd: refuses to start with a clear error when /dev/kvm is absent
#   (exit 78, no restart loop); with /dev/kvm the stub boatd runs;
# - boat-secrets: 32-byte desktop key, 0600 boat:boat (boat-api + gateway);
#   the API master key only as a systemd credential, loaded into boat-api;
# - data-disk guard: with /etc/pandaos/data-disk but nothing mounted, boat-api
#   refuses to start (exit 78);
# - boat-reboot-check: boat_reboot_pending 0 in the textfile metrics;
# - boat-gateway + Caddy (boat.localhost -> Caddy's local CA, works offline):
#   private port routes need a token, public ones don't; each desktop is its
#   own origin with a host-only cookie that does not open another VM's;
#   api. proxies to boat-api with /internal/* blocked; routes re-read live;
# - ssh jump: -W only to permitopen targets of running VMs, no shell,
#   port 2222 jump-only.
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
    wc -c <"$CREDENTIALS_DIRECTORY/boat-master-key" >/var/lib/boat/api/credlen
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
    import base64, hashlib, hmac, json, re, shlex, time

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

    with subtest("secrets, sealed master key, dirs"):
        machine.wait_for_unit("boat-secrets.service")
        st = machine.succeed("stat -c '%a %U %G %s' /var/lib/boat/secrets/desktop.key").strip()
        assert st == "600 boat boat 32", st
        machine.succeed("test -s /var/lib/boat/secrets/boat-master-key.cred")
        machine.fail("test -e /var/lib/boat/api/master.key")
        machine.wait_until_succeeds("test -s /var/lib/boat/api/args")
        args = machine.succeed("cat /var/lib/boat/api/args").split("\n")
        for flag, val in [
            ("-desktop-url", f"https://desktop.{D}"),
            ("-ssh-jump", f"jump@ssh.{D}:2222"),
            ("-ssh-jump-keys-file", "/var/lib/boat/ssh/jump_authorized_keys"),
            ("-boatd", "/run/boat/boatd.sock"),
        ]:
            assert args[args.index(flag) + 1] == val, (flag, args)
        # The stub saw the decrypted credential (32 bytes) and nothing else.
        assert machine.succeed("cat /var/lib/boat/api/credlen").strip() == "32"
        assert machine.succeed("stat -c '%a %U %G' /run/boat").strip() == "750 root boat"

    with subtest("data-disk guard"):
        machine.succeed("mkdir -p /etc/pandaos && echo 'PANDADATA test-uuid /dev/vdz' >/etc/pandaos/data-disk")
        machine.fail("systemctl restart boat-api.service")
        assert "Refusing to write VM data" in machine.succeed("journalctl -u boat-api.service --no-pager")
        machine.succeed("rm /etc/pandaos/data-disk && systemctl restart boat-api.service")

    with subtest("reboot check"):
        machine.succeed("systemctl start boat-reboot-check.service")
        prom = machine.succeed("cat /var/lib/boat-metrics/boat.prom")
        assert "boat_reboot_pending 0" in prom, prom

    machine.succeed(
        "echo '{\"vms\":{\"vm1\":{\"ip\":\"127.0.0.1\",\"ports\":[8000],\"desktopPort\":8000},"
        + "\"vm2\":{\"ip\":\"127.0.0.1\",\"ports\":[8000],\"public\":[8000],\"desktopPort\":8000}}}' >/run/boat/routes.json"
    )
    machine.wait_for_unit("boat-gateway.service")
    machine.wait_for_open_port(8742)
    machine.wait_for_open_port(8000)
    key = base64.b64decode(machine.succeed("base64 -w0 /var/lib/boat/secrets/desktop.key"))
    exp = int(time.time()) + 600

    def mac(payload):
        return hmac.new(key, payload.encode(), hashlib.sha256).hexdigest()

    with subtest("caddy + gateway over TLS: private/public routes, per-VM desktop origins"):
        machine.wait_for_unit("caddy.service")
        machine.wait_for_open_port(443)
        hosts = ["api", "desktop", "vm1.desktop", "vm2.desktop", "8000-vm1.vm", "8000-vm2.vm"]
        resolve = " ".join(f"--resolve {h}.{D}:443:127.0.0.1" for h in hosts)
        code = f"curl -sk -o /dev/null -w '%{{http_code}}' {resolve} "
        machine.wait_until_succeeds(code + f"https://api.{D}/boatd.json | grep -x 200", timeout=60)
        assert machine.succeed(code + f"https://api.{D}/internal/desktop-auth") == "404"
        machine.wait_until_succeeds(code + f"https://8000-vm2.vm.{D}/boatd.json | grep -x 200", timeout=60)
        assert machine.succeed(code + f"https://8000-vm1.vm.{D}/boatd.json") == "401"
        rt = f"{exp}.{mac(f'route.8000-vm1.{exp}')}"
        assert machine.succeed(code + f"-H 'X-Boat-Route-Token: {rt}' https://8000-vm1.vm.{D}/boatd.json") == "200"
        bad = f"{exp}.{mac(f'route.8000-vm2.{exp}')}"
        assert machine.succeed(code + f"-H 'X-Boat-Route-Token: {bad}' https://8000-vm1.vm.{D}/boatd.json") == "401"

        q = f"exp={exp}&sig={mac(f'vm1.{exp}')}"
        loc = machine.succeed(f"curl -sk -o /dev/null -w '%{{redirect_url}}' {resolve} 'https://desktop.{D}/vm1?{q}'")
        assert loc == f"https://vm1.desktop.{D}/?{q}", loc
        hdrs = machine.succeed(f"curl -sk -D - -o /dev/null {resolve} 'https://vm1.desktop.{D}/?{q}'")
        m = re.search(r"(?im)^set-cookie: (__Host-boat_gw=[^;]+);(.*)$", hdrs)
        assert m and "SameSite=Strict" in m.group(2) and "Domain" not in m.group(2), hdrs
        ck = m.group(1)
        assert machine.succeed(code + f"-b '{ck}' https://vm1.desktop.{D}/boatd.json") == "200"
        assert machine.succeed(code + f"https://vm1.desktop.{D}/boatd.json") == "401"
        # VM A's session on VM B's origin: refused.
        assert machine.succeed(code + f"-b '{ck}' https://vm2.desktop.{D}/boatd.json") == "401"

    with subtest("ssh jump: -W to permitopen only, no shell, 2222 is jump-only"):
        machine.wait_for_open_port(2222)
        machine.succeed("ssh-keygen -q -t ed25519 -N ''' -f /root/k")
        pub = machine.succeed("cat /root/k.pub").strip()
        lines = [
            f'restrict,port-forwarding,permitopen="127.0.0.1:22" {pub}',
            f'restrict,port-forwarding,pty,permitopen="127.0.0.1:2222" {pub}',  # rejected by the filter
            f'restrict,port-forwarding,permitopen="10.200.9.9:22" {pub}',  # no such running VM
        ]
        machine.succeed(f"printf '%s\\n' {' '.join(shlex.quote(l) for l in lines)} >/var/lib/boat/ssh/jump_authorized_keys")
        machine.succeed("chown boat:boat /var/lib/boat/ssh/jump_authorized_keys && chmod 640 /var/lib/boat/ssh/jump_authorized_keys")
        out = machine.succeed("runuser -u boat -- /etc/boat/jump-keys /var/lib/boat/ssh/jump_authorized_keys /run/boat/routes.json")
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

    with subtest("route removal is live (gateway + jump grants)"):
        machine.succeed("echo '{\"vms\":{}}' >/run/boat/routes.json")
        assert machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' -H 'Host: 8000-vm2.vm.{D}' http://127.0.0.1:8742/") == "404"
        machine.fail(f"timeout 10 {ssh} -W 127.0.0.1:22 jump@127.0.0.1 </dev/null | grep -q SSH-")
  '';
}
