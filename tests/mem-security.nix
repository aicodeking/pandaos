# The shared /mem repo must not run code planted in its .git:
#  1. A job user (panda-agent) cannot plant anything: .git belongs to the
#     panda-mem service user and is not group-writable.
#  2. Even a hook and core.fsmonitor planted by root (the strongest attacker
#     short of replacing panda-mem) never run when clients write through the
#     socket; refusing the tampered repo is also acceptable.
# Markers go inside .git because the service only has write access to the
# repo (ProtectSystem=strict); a marker elsewhere could not be created even
# if the hook ran, which would make the check vacuous.
{ pkgs, self, t }:
pkgs.testers.runNixOSTest {
  name = "mem-security";
  nodes.machine = {
    imports = [
      self.nixosModules.default
      (t.nodeName "machine")
      t.noFirstboot
    ];
    services.panda-mem.enable = true;
    services.panda-agentd.enable = true;
  };
  testScript =
    { nodes, ... }:
    t.prelude
    + ''
      d = "${nodes.machine.services.panda-mem.dir}"
      g = f"{d}/.git"
      sock = "-socket /run/panda-mem/sock"
      machine.wait_for_unit("panda-mem.service")
      machine.wait_for_file("/run/panda-mem/sock")
      machine.succeed(f"runuser -u panda -- panda-mem write {sock} -kind semantic -title seed -body 'seed body'")


      def script(kind):
          return f"#!/bin/sh\\ntouch {g}/marker-{kind}-$(id -un)\\n"


      def markers():
          return machine.succeed(f"ls -A {g} | grep '^marker-' || true").split()


      def as_user(user, cmd):
          return machine.execute(f"runuser -u {user} -- sh -c {shlex.quote(cmd)} 2>&1")


      with subtest("panda-agent cannot write .git at all"):
          print(machine.succeed(f"stat -c '%a %U:%G %n' {d} {g} {g}/hooks {g}/config || true"))
          rc_hook, out_hook = as_user("panda-agent", f"printf %b {shlex.quote(script('hook'))} > {g}/hooks/pre-commit")
          rc_cfg, out_cfg = as_user("panda-agent", f"git -c safe.directory='*' -C {d} config core.fsmonitor /tmp/x")
          print(f"plant hook: rc={rc_hook} {out_hook!r}; plant fsmonitor: rc={rc_cfg} {out_cfg!r}")
          assert rc_hook != 0 and rc_cfg != 0, "panda-agent could write into the shared repo's .git"
          machine.fail(f"test -e {g}/hooks/pre-commit")
          assert "fsmonitor" not in machine.succeed(f"cat {g}/config")

      with subtest("root plants a hook and core.fsmonitor; plain git fires them"):
          machine.succeed(f"printf %b {shlex.quote(script('hook'))} > {g}/hooks/pre-commit")
          machine.succeed(f"printf %b {shlex.quote(script('fsmonitor'))} > {g}/fsmon.sh")
          machine.succeed(f"chmod 755 {g}/hooks/pre-commit {g}/fsmon.sh")
          machine.succeed(f"git -c safe.directory='*' -C {d} config core.fsmonitor {g}/fsmon.sh")
          # Proves the plant is armed, so "no marker" below means something.
          machine.succeed(f"cd {d} && git -c safe.directory='*' status >/dev/null"
                          " && git -c safe.directory='*' -c user.name=ci -c user.email=ci@localhost"
                          " commit -q --allow-empty --no-verify -m armed-status"
                          " && git -c safe.directory='*' -c user.name=ci -c user.email=ci@localhost"
                          " commit -q --allow-empty -m armed-hook")
          armed = markers()
          print(f"plain git ran: {armed}")
          assert any("hook" in m for m in armed) and any("fsmonitor" in m for m in armed), armed
          machine.succeed(f"rm -f {g}/marker-*")

      with subtest("socket writes by panda and panda-agent run no planted code"):
          for user in ("panda", "panda-agent"):
              rc, out = machine.execute(
                  f"runuser -u {user} -- panda-mem write {sock} -kind semantic"
                  f" -title 'probe {user}' -body 'written by {user}' 2>&1"
              )
              # Refusing a tampered repo is fine; running its code is not.
              print(f"panda-mem write as {user}: rc={rc} {out!r}")
              assert rc == 0 or any(w in out.lower() for w in ("refus", "hook", "config")), \
                  f"write as {user} failed for another reason: {out!r}"
          left = markers()
          assert not left, f"planted git code ran during panda-mem writes: {left}"
    '';
}
