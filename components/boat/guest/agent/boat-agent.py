#!/usr/bin/env python3
"""boat guest agent: JSON lines over AF_VSOCK port 1024 (stdlib only).

Each request is one JSON object per line; a connection may carry many
requests in sequence. Replies are JSON lines. Errors are {"error": "..."}.
See ../README.md for the op reference.
"""
import base64
import fcntl
import grp
import hmac
import ipaddress
import json
import os
import pwd
import re
import secrets
import select
import shlex
import signal
import socket
import struct
import subprocess
import sys
import threading
import time
import uuid

VERSION = "1"
VSOCK_PORT = 1024
MAX_LINE = 64 << 20          # one request line (base64 chunks included)
CHUNK = 1 << 20              # get_file chunk size
KILL_GRACE_S = 2.0           # SIGTERM -> SIGKILL on timeout
DRAIN_GRACE_S = 1.0          # keep reading after exit (children holding pipes)
DEFAULT_TIMEOUT_S = 600

BOAT_DIR = os.environ.get("BOAT_DIR", "/etc/boat")
ENV_FILE = os.path.join(BOAT_DIR, "env")
IDENTITY_FILE = os.path.join(BOAT_DIR, "identity.json")
EXEC_USER = os.environ.get("BOAT_EXEC_USER", "dev")
IFACE = os.environ.get("BOAT_IFACE", "eth0")

DESKTOP_PORT = 6080           # token-checking proxy, reached by the host
DESKTOP_BACKEND = ("127.0.0.1", 6081)  # KasmVNC websocket/http, local only
DESKTOP_UNIT = "boat-desktop.service"
DESKTOP_DISPLAY = ":1"
TOKEN_COOKIE = "boat_desktop"

ENV_KEY = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
HOSTNAME = re.compile(r"^(?=.{1,63}$)[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$")
MAC = re.compile(r"^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$")

STARTED = time.monotonic()
_activity = {"last": time.monotonic(), "running": 0}
_activity_lock = threading.Lock()
_desktop = {"token": None, "proxy": None}
# A reseed arms exactly one set_identity. A snapshot captures the parent's
# disarmed state, so every restored child needs a fresh reseed first.
_reseed = {"armed": False, "generation": None}
_reseed_lock = threading.Lock()

RANDOM_DEV = "/dev/random"
RNDADDENTROPY = 0x40085203   # _IOW('R', 0x03, int[2])
RNDRESEEDCRNG = 0x5207       # _IO('R', 0x07)
ENTROPY_MIN, ENTROPY_MAX = 32, 4096


class OpError(Exception):
    pass


# ---------------------------------------------------------------- activity

def touch(delta_running=0):
    with _activity_lock:
        _activity["last"] = time.monotonic()
        _activity["running"] += delta_running


def ssh_sessions_active():
    try:
        out = subprocess.run(["who"], capture_output=True, text=True, timeout=5).stdout
    except (OSError, subprocess.SubprocessError):
        return False
    return bool(out.strip())


def desktop_idle_s():
    """Seconds since the last desktop input, or None if no desktop."""
    if _desktop["token"] is None:
        return None
    try:
        r = subprocess.run(["xprintidle"], capture_output=True, text=True, timeout=5,
                           env={"DISPLAY": DESKTOP_DISPLAY, "PATH": os.environ.get("PATH", "")})
        return int(r.stdout.strip()) / 1000.0 if r.returncode == 0 else None
    except (OSError, ValueError, subprocess.SubprocessError):
        return None


def idle_seconds():
    with _activity_lock:
        if _activity["running"]:
            return 0.0
        idle = time.monotonic() - _activity["last"]
    if ssh_sessions_active():
        return 0.0
    d = desktop_idle_s()
    return round(min(idle, d) if d is not None else idle, 1)


# ---------------------------------------------------------------- helpers

def b64d(s):
    try:
        return base64.b64decode(s or "", validate=True)
    except (ValueError, TypeError) as e:
        raise OpError("bad base64: %s" % e)


def b64e(b):
    return base64.b64encode(b).decode()


def run(argv, check=True):
    r = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    if check and r.returncode != 0:
        raise OpError("%s failed: %s" % (" ".join(argv), (r.stderr or r.stdout).strip()))
    return r


def read_env_file(path=None):
    path = path or ENV_FILE
    env = {}
    try:
        with open(path) as f:
            for line in f:
                parts = shlex.split(line, comments=True)
                if parts and parts[0] == "export":
                    parts = parts[1:]
                for p in parts:
                    k, sep, v = p.partition("=")
                    if sep and ENV_KEY.match(k):
                        env[k] = v
    except FileNotFoundError:
        pass
    return env


def user_info(name):
    """pwd entry to switch to, or None to run as ourselves (non-root/tests)."""
    if os.geteuid() != 0 or not name:
        return None
    try:
        return pwd.getpwnam(name)
    except KeyError:
        raise OpError("no such user: %s" % name)


# ---------------------------------------------------------------- exec

def exec_env(req, pw):
    env = {"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
           "LANG": "C.UTF-8", "TERM": "xterm-256color" if req.get("tty") else "dumb"}
    if pw:
        env.update(HOME=pw.pw_dir, USER=pw.pw_name, LOGNAME=pw.pw_name, SHELL=pw.pw_shell)
        env["PATH"] = "%s/.local/bin:%s/.cargo/bin:%s/.bun/bin:%s" % (
            pw.pw_dir, pw.pw_dir, pw.pw_dir, env["PATH"])
    else:
        env["HOME"] = os.environ.get("HOME", "/")
    env.update(read_env_file())
    extra = req.get("env") or {}
    if not isinstance(extra, dict):
        raise OpError("env must be an object")
    for k, v in extra.items():
        if not ENV_KEY.match(str(k)):
            raise OpError("bad env key: %r" % k)
        env[k] = str(v)
    return env


def spawn(req):
    argv = req.get("argv")
    if not isinstance(argv, list) or not argv or not all(isinstance(a, str) for a in argv):
        raise OpError("argv must be a non-empty list of strings")
    pw = user_info(req.get("user", EXEC_USER))
    env = exec_env(req, pw)
    cwd = req.get("cwd") or (pw.pw_dir if pw else None)
    kw = dict(env=env, cwd=cwd, start_new_session=True, close_fds=True)
    if pw:
        kw.update(user=pw.pw_uid, group=pw.pw_gid,
                  extra_groups=os.getgrouplist(pw.pw_name, pw.pw_gid))
    if req.get("tty"):
        import pty
        master, slave = pty.openpty()
        try:
            proc = subprocess.Popen(argv, stdin=slave, stdout=slave, stderr=slave, **kw)
        finally:
            os.close(slave)
        return proc, {master: "stdout"}, os.dup(master)  # the stdin thread owns the dup
    proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, **kw)
    return proc, {proc.stdout.fileno(): "stdout", proc.stderr.fileno(): "stderr"}, proc.stdin


def feed_stdin(sink, data):
    def go():
        try:
            if isinstance(sink, int):
                try:
                    if data:
                        os.write(sink, data + b"\x04")  # ^D = EOF on a cooked pty
                finally:
                    os.close(sink)
            else:
                if data:
                    sink.write(data)
                sink.close()
        except OSError:
            pass
    threading.Thread(target=go, daemon=True).start()


def kill_group(proc, sig):
    try:
        os.killpg(proc.pid, sig)
    except (ProcessLookupError, PermissionError):
        pass


def op_exec(req, send):
    timeout = float(req.get("timeout_s") or DEFAULT_TIMEOUT_S)
    try:
        proc, fds, sink = spawn(req)
    except (OSError, ValueError) as e:
        raise OpError("spawn: %s" % e)
    touch(+1)
    try:
        feed_stdin(sink, b64d(req.get("stdin_b64")))
        timed_out = pump(proc, fds, send, time.monotonic() + timeout)
    finally:
        if proc.stdout is None:  # tty: we own the master fd
            os.close(next(iter(fds)))
        else:  # pipes: close via their file objects, never by raw fd (no double close)
            proc.stdout.close()
            proc.stderr.close()
        touch(-1)
    rc = proc.wait()
    res = {"exit": rc if rc >= 0 else 128 - rc}
    if rc < 0:
        res["signal"] = signal.Signals(-rc).name
    if timed_out:
        res["timed_out"] = True
    send(res)


def pump(proc, fds, send, deadline):
    """Stream output until EOF (or a short drain after exit). Returns timed_out."""
    fds = dict(fds)
    timed_out, term_at, exited_at = False, None, None
    while fds:
        now = time.monotonic()
        if now > deadline and term_at is None:
            timed_out, term_at = True, now
            kill_group(proc, signal.SIGTERM)
        if term_at is not None and now - term_at > KILL_GRACE_S:
            kill_group(proc, signal.SIGKILL)
        if exited_at is None and proc.poll() is not None:
            exited_at = now
        if exited_at is not None and now - exited_at > DRAIN_GRACE_S:
            break  # ponytail: background children keep their pipes; we stop reading
        ready, _, _ = select.select(list(fds), [], [], 0.1)
        for fd in ready:
            try:
                data = os.read(fd, 65536)
            except OSError:  # EIO on pty once the child side is gone
                data = b""
            if not data:
                del fds[fd]
                continue
            send({"stream": fds[fd], "data_b64": b64e(data)})
    if proc.poll() is None:
        try:
            proc.wait(timeout=max(0.0, deadline - time.monotonic()) + KILL_GRACE_S)
        except subprocess.TimeoutExpired:
            timed_out = True
            kill_group(proc, signal.SIGKILL)
    return timed_out


# ---------------------------------------------------------------- files

def chown_like_parent(path, owner):
    if os.geteuid() != 0:
        return
    if owner:
        user, _, group = str(owner).partition(":")
        pw = user_info(user)
        gid = grp.getgrnam(group).gr_gid if group else pw.pw_gid
        os.chown(path, pw.pw_uid, gid)
        return
    st = os.stat(os.path.dirname(path) or "/")
    os.chown(path, st.st_uid, st.st_gid)


def makedirs_owned(d):
    """mkdir -p, each new dir owned like its parent (so /home/dev/x stays dev's)."""
    if not d or os.path.isdir(d):
        return
    makedirs_owned(os.path.dirname(d))
    os.mkdir(d, 0o755)
    chown_like_parent(d, None)


def op_put_file(req, send):
    path = req.get("path")
    if not isinstance(path, str) or not os.path.isabs(path):
        raise OpError("path must be absolute")
    data = b64d(req.get("data_b64"))
    offset = int(req.get("offset") or 0)
    last = req.get("last", True)
    part = path + ".boat-part"
    makedirs_owned(os.path.dirname(path))
    flags = os.O_WRONLY | os.O_CREAT | (os.O_TRUNC if offset == 0 else 0)
    fd = os.open(part, flags, 0o600)
    try:
        if offset and os.fstat(fd).st_size != offset:
            raise OpError("offset %d != received %d" % (offset, os.fstat(fd).st_size))
        os.lseek(fd, offset, os.SEEK_SET)
        os.write(fd, data)
        size = os.fstat(fd).st_size
    finally:
        os.close(fd)
    if last:
        mode = req.get("mode", 0o644)
        os.chmod(part, int(mode, 8) if isinstance(mode, str) else int(mode))
        chown_like_parent(part, req.get("owner"))
        os.replace(part, path)
        touch()
    send({"ok": True, "size": size, "complete": bool(last)})


def op_get_file(req, send):
    path = req.get("path")
    if not isinstance(path, str) or not os.path.isabs(path):
        raise OpError("path must be absolute")
    try:
        f = open(path, "rb")
    except OSError as e:
        raise OpError("open %s: %s" % (path, e.strerror))
    with f:
        st = os.fstat(f.fileno())
        offset = 0
        while True:
            data = f.read(CHUNK)
            if not data:
                break
            send({"offset": offset, "data_b64": b64e(data)})
            offset += len(data)
    send({"eof": True, "size": offset, "mode": "%o" % (st.st_mode & 0o7777)})


# ---------------------------------------------------------------- identity / env

def validate_identity(req):
    host = req.get("hostname")
    if not isinstance(host, str) or not HOSTNAME.match(host):
        raise OpError("bad hostname")
    try:
        ipaddress.IPv4Interface(req["ipv4_cidr"])
        ipaddress.IPv4Address(req["gw"])
        dns = req.get("dns") or []
        dns = [dns] if isinstance(dns, str) else dns
        for d in dns:
            ipaddress.ip_address(d)
        if req.get("ipv6_cidr"):
            ipaddress.IPv6Interface(req["ipv6_cidr"])
        if req.get("ipv6_gw"):
            ipaddress.IPv6Address(req["ipv6_gw"])
    except (KeyError, ValueError) as e:
        raise OpError("bad address: %s" % e)
    if req.get("mac") and not MAC.match(req["mac"]):
        raise OpError("bad mac")
    ident = {k: req.get(k) for k in ("hostname", "ipv4_cidr", "gw", "mac", "ipv6_cidr", "ipv6_gw")}
    ident["dns"] = list(dns)
    return ident


def apply_network(ident):
    dev = IFACE
    if ident.get("mac"):
        run(["ip", "link", "set", dev, "down"])
        run(["ip", "link", "set", dev, "address", ident["mac"]])
    run(["ip", "link", "set", dev, "up"])
    run(["ip", "-4", "addr", "flush", "dev", dev])
    run(["ip", "addr", "add", ident["ipv4_cidr"], "dev", dev])
    run(["ip", "route", "replace", "default", "via", ident["gw"], "dev", dev])
    if ident.get("ipv6_cidr"):
        run(["ip", "-6", "addr", "flush", "dev", dev, "scope", "global"])
        run(["ip", "-6", "addr", "add", ident["ipv6_cidr"], "dev", dev])
        if ident.get("ipv6_gw"):
            run(["ip", "-6", "route", "replace", "default", "via", ident["ipv6_gw"], "dev", dev])
    if ident["dns"]:
        write_atomic("/etc/resolv.conf",
                     "".join("nameserver %s\n" % d for d in ident["dns"]), 0o644)
    host = ident["hostname"]
    write_atomic("/etc/hostname", host + "\n", 0o644)
    write_atomic("/etc/hosts", "127.0.0.1 localhost\n127.0.1.1 %s\n"
                 "::1 localhost ip6-localhost ip6-loopback\n" % host, 0o644)
    run(["hostname", host])


def write_atomic(path, text, mode):
    tmp = path + ".boat-tmp"
    with open(tmp, "w") as f:
        f.write(text)
    os.chmod(tmp, mode)
    if os.path.islink(path):
        os.unlink(path)
    os.replace(tmp, path)


def regenerate_secrets():
    write_atomic("/etc/machine-id", uuid.uuid4().hex + "\n", 0o444)
    for name in os.listdir("/etc/ssh"):
        if name.startswith("ssh_host_"):
            os.unlink(os.path.join("/etc/ssh", name))
    run(["ssh-keygen", "-A"])
    run(["systemctl", "restart", "ssh.service"], check=False)  # also revives a failed sshd


def rnd_ioctl(data):
    """Credit `data` to the kernel pool, then force a CRNG reseed (needs root)."""
    fd = os.open(RANDOM_DEV, os.O_WRONLY)
    try:
        fcntl.ioctl(fd, RNDADDENTROPY, struct.pack("ii", len(data) * 8, len(data)) + data)
        fcntl.ioctl(fd, RNDRESEEDCRNG)
    finally:
        os.close(fd)


def op_reseed(req, send):
    data = b64d(req.get("entropy_b64"))
    if not ENTROPY_MIN <= len(data) <= ENTROPY_MAX:
        raise OpError("entropy must be %d..%d bytes" % (ENTROPY_MIN, ENTROPY_MAX))
    try:
        rnd_ioctl(data)
    except OSError as e:
        raise OpError("reseed: %s" % e)
    with _reseed_lock:
        _reseed.update(armed=True, generation=req.get("generation"))
    send({"ok": True, "bytes": len(data)})


def take_reseed(req):
    """Consume the pending reseed, or refuse: new keys need fresh host entropy."""
    with _reseed_lock:
        if not _reseed["armed"]:
            raise OpError("reseed required before set_identity (no fresh entropy since "
                          "the last restore/set_identity)")
        gen = req.get("generation")
        if gen is not None and gen != _reseed["generation"]:
            raise OpError("reseed generation %r != set_identity generation %r"
                          % (_reseed["generation"], gen))
        _reseed.update(armed=False, generation=None)


def op_sync(req, send):
    os.sync()  # flushes every mounted filesystem; boatd calls this before pause
    send({"ok": True})


def op_set_identity(req, send):
    ident = validate_identity(req)
    take_reseed(req)
    apply_network(ident)
    regenerate_secrets()
    os.makedirs(os.path.dirname(IDENTITY_FILE), exist_ok=True)
    write_atomic(IDENTITY_FILE, json.dumps(ident), 0o600)
    send({"ok": True})


def restore_identity():
    """Re-apply the last set_identity at boot (it overrides the cmdline ip=)."""
    try:
        with open(IDENTITY_FILE) as f:
            apply_network(json.load(f))
    except FileNotFoundError:
        pass
    except (OSError, ValueError, OpError) as e:
        log("restore identity: %s" % e)


def op_set_env(req, send):
    env = req.get("vars")
    if not isinstance(env, dict):
        raise OpError("vars must be an object")
    lines = []
    for k, v in sorted(env.items()):
        if not ENV_KEY.match(str(k)):
            raise OpError("bad env key: %r" % k)
        lines.append("export %s=%s\n" % (k, shlex.quote(str(v))))
    os.makedirs(os.path.dirname(ENV_FILE), exist_ok=True)
    write_atomic(ENV_FILE, "".join(lines), 0o600)
    try:  # login shells of the VM user must be able to source it
        pw = user_info(EXEC_USER)
        if pw:
            os.chown(ENV_FILE, pw.pw_uid, pw.pw_gid)
    except OpError:
        pass
    send({"ok": True, "count": len(lines)})


# ---------------------------------------------------------------- desktop

def check_token(head, token):
    """Return (ok, redirect_location_or_None) for an HTTP request head."""
    if not token:
        return False, None
    try:
        line = head.split(b"\r\n", 1)[0].decode("latin-1")
        target = line.split(" ")[1]
    except IndexError:
        return False, None
    path, _, query = target.partition("?")
    params = [p for p in query.split("&") if p]
    rest = [p for p in params if not p.startswith("token=")]
    for p in params:
        if p.startswith("token=") and hmac.compare_digest(p[6:], token):
            # relative Location, so a host-side path prefix survives
            loc = (path.rsplit("/", 1)[-1] or "./") + ("?" + "&".join(rest) if rest else "")
            return True, loc
    for line in head.decode("latin-1").split("\r\n")[1:]:
        name, _, value = line.partition(":")
        if name.strip().lower() == "cookie":
            for c in value.split(";"):
                k, _, v = c.strip().partition("=")
                if k == TOKEN_COOKIE and hmac.compare_digest(v, token):
                    return True, None
    return False, None


def splice(a, b):
    try:
        while True:
            data = a.recv(65536)
            if not data:
                break
            b.sendall(data)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def proxy_conn(client, backend_addr):
    with client:
        head = b""
        client.settimeout(15)
        try:
            while b"\r\n\r\n" not in head and len(head) < 65536:
                chunk = client.recv(4096)
                if not chunk:
                    return
                head += chunk
        except OSError:
            return
        ok, loc = check_token(head, _desktop["token"])
        if not ok:
            client.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            return
        if loc is not None:
            client.sendall(("HTTP/1.1 302 Found\r\nLocation: %s\r\nSet-Cookie: %s=%s; HttpOnly; "
                            "SameSite=Lax\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                            % (loc, TOKEN_COOKIE, _desktop["token"])).encode())
            return
        client.settimeout(None)
        try:
            backend = socket.create_connection(backend_addr, timeout=10)
        except OSError:
            client.sendall(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
            return
        with backend:
            backend.settimeout(None)
            backend.sendall(head)
            t = threading.Thread(target=splice, args=(backend, client), daemon=True)
            t.start()
            splice(client, backend)
            t.join()


def start_proxy(port=DESKTOP_PORT, backend_addr=DESKTOP_BACKEND, host="0.0.0.0"):
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((host, port))
    srv.listen(64)

    def loop():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return  # listener closed
            threading.Thread(target=proxy_conn, args=(c, backend_addr), daemon=True).start()
    threading.Thread(target=loop, daemon=True).start()
    return srv


def wait_port(addr, timeout):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            socket.create_connection(addr, timeout=1).close()
            return True
        except OSError:
            time.sleep(0.25)
    return False


def desktop_running():
    return run(["systemctl", "is-active", "--quiet", DESKTOP_UNIT], check=False).returncode == 0


def op_desktop(req, send):
    action = req.get("action")
    if action == "start":
        if not os.path.exists("/etc/systemd/system/" + DESKTOP_UNIT):
            raise OpError("desktop is not installed in this image (slim variant)")
        token = req.get("token") or secrets.token_urlsafe(32)
        if not re.match(r"^[A-Za-z0-9_-]{16,128}$", token):
            raise OpError("token must be 16-128 url-safe chars")
        run(["systemctl", "start", DESKTOP_UNIT])
        if not wait_port(DESKTOP_BACKEND, 30):
            raise OpError("desktop did not come up on %s:%d" % DESKTOP_BACKEND)
        if _desktop["proxy"] is None:
            _desktop["proxy"] = start_proxy()
        _desktop["token"] = token
        touch()
        send({"ok": True, "port": DESKTOP_PORT, "token": token, "path": "/?token=" + token})
    elif action == "stop":
        _desktop["token"] = None
        run(["systemctl", "stop", DESKTOP_UNIT], check=False)
        send({"ok": True})
    elif action == "status":
        up = desktop_running()
        send({"ok": True, "running": up and _desktop["token"] is not None,
              "port": DESKTOP_PORT if up else None})
    else:
        raise OpError("action must be start|stop|status")


# ---------------------------------------------------------------- server

def op_health(req, send):
    send({"ok": True, "uptime": round(time.monotonic() - STARTED, 1),
          "idle_s": idle_seconds(), "version": VERSION})


def op_shutdown(req, send):
    send({"ok": True})

    def bye():
        time.sleep(0.5)
        os.sync()
        # Firecracker has no ACPI poweroff; a reboot (reboot=k) makes it exit.
        subprocess.run(["systemctl", "reboot"])
    threading.Thread(target=bye, daemon=True).start()


OPS = {"health": op_health, "exec": op_exec, "put_file": op_put_file,
       "get_file": op_get_file, "set_identity": op_set_identity, "set_env": op_set_env,
       "desktop": op_desktop, "shutdown": op_shutdown, "reseed": op_reseed, "sync": op_sync}


def handle_conn(conn):
    lock = threading.Lock()

    def send(obj):
        data = (json.dumps(obj, separators=(",", ":")) + "\n").encode()
        with lock:
            conn.sendall(data)

    with conn, conn.makefile("rb") as rfile:
        while True:
            line = rfile.readline(MAX_LINE + 1)
            if not line:
                return
            if len(line) > MAX_LINE:
                send({"error": "request too large"})
                return
            if not line.strip():
                continue
            try:
                req = json.loads(line)
                if not isinstance(req, dict):
                    raise OpError("request must be an object")
                fn = OPS.get(req.get("op"))
                if fn is None:
                    raise OpError("unknown op: %r" % req.get("op"))
                fn(req, send)
            except (OpError, ValueError, TypeError) as e:
                send({"error": str(e)})
            except OSError as e:
                if isinstance(e, (BrokenPipeError, ConnectionResetError)):
                    return
                send({"error": "%s: %s" % (type(e).__name__, e)})


def log(msg):
    print("boat-agent: %s" % msg, file=sys.stderr, flush=True)


def listen(tcp=None):
    if tcp:
        host, _, port = tcp.rpartition(":")
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.bind((host or "127.0.0.1", int(port)))
    else:
        s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
        s.bind((socket.VMADDR_CID_ANY, VSOCK_PORT))
    s.listen(64)
    return s


def main(argv):
    tcp = argv[argv.index("--tcp") + 1] if "--tcp" in argv else None
    if not tcp:
        restore_identity()
    srv = listen(tcp)
    log("listening on %s" % (tcp or "vsock:%d" % VSOCK_PORT))
    while True:
        conn, _ = srv.accept()
        threading.Thread(target=handle_conn, args=(conn,), daemon=True).start()


if __name__ == "__main__":
    main(sys.argv[1:])
