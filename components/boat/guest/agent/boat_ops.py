#!/usr/bin/env python3
"""Detached commands (cmd_*) and file ops (file_*) for boat-agent (stdlib only).

A detached command runs under its own double-forked supervisor, so an agent
restart cannot touch it. Everything about it lives in CMD_ROOT/<cmd_id>/:
  status.json   state, pid, exit, signal, started, ended
  spec.json     how to start it; deleted once started (it holds the env)
  seg.<offset>  merged stdout+stderr; the name is the segment's absolute offset

Run as a script (--supervise DIR) it is that supervisor.
"""
import base64
import errno
import grp
import json
import os
import pwd
import re
import secrets
import select
import shutil
import signal
import subprocess
import sys
import threading
import time

CMD_ROOT = os.environ.get("BOAT_CMD_ROOT", "/var/lib/boat/cmds")
CMD_ID = re.compile(r"^c_[0-9a-f]{16}$")
LOG_SEGMENT = 32 << 20        # two segments are kept: the newest 32-64 MiB
CMD_KEEP = 100                # finished commands kept on disk
START_TIMEOUT_S = 10
EXIT_DRAIN_S = 1.0            # output grace before a finished command reports exit
LOGS_DEFAULT = 1 << 20
LOGS_MAX = 4 << 20            # base64 of this stays under boatd's 8 MiB line limit

CHUNK = 1 << 20               # get_file chunk size
FILE_LIST_MAX = 10000
SEARCH_DEFAULT, SEARCH_MAX = 100, 1000
SEARCH_TIMEOUT_S = 30
SEARCH_TEXT_MAX = 1000
RG = "rg"
UNDELETABLE = {"/", "/proc", "/sys", "/dev", "/run", "/boot", "/usr", "/etc", "/bin", "/lib",
               "/sbin", "/var", "/home"}


class OpError(Exception):
    def __init__(self, message, code=None):
        super().__init__(message)
        self.code = code


_ERRNO_CODES = {
    errno.ENOENT: "not_found",
    errno.ENOTDIR: "not_a_directory",
    errno.EISDIR: "is_a_directory",
    errno.EEXIST: "already_exists",
    errno.ENOTEMPTY: "not_empty",
    errno.EINVAL: "invalid_argument",
    errno.ENAMETOOLONG: "invalid_argument",
    errno.EACCES: "permission_denied",
    errno.EPERM: "permission_denied",
}


def oserror_code(err):
    if isinstance(err, FileNotFoundError):
        return "not_found"
    if isinstance(err, NotADirectoryError):
        return "not_a_directory"
    if isinstance(err, IsADirectoryError):
        return "is_a_directory"
    if isinstance(err, FileExistsError):
        return "already_exists"
    if isinstance(err, PermissionError):
        return "permission_denied"
    return _ERRNO_CODES.get(getattr(err, "errno", None))


def b64d(s):
    try:
        return base64.b64decode(s or "", validate=True)
    except (ValueError, TypeError) as e:
        raise OpError("bad base64: %s" % e, code="invalid_argument")


def user_info(name):
    """pwd entry to switch to, or None to run as ourselves (non-root/tests)."""
    if os.geteuid() != 0 or not name:
        return None
    try:
        return pwd.getpwnam(name)
    except KeyError:
        raise OpError("no such user: %s" % name, code="invalid_user")


def resolve_cwd(cwd, home):
    """Resolve ~, ~/..., or relative cwd against home (never the daemon's '/')."""
    if cwd is None or cwd == "":
        return None
    if not isinstance(cwd, str) or "\0" in cwd:
        raise OpError("cwd must be a string without NUL", code="invalid_argument")
    if os.path.isabs(cwd):
        return cwd
    if not home:
        raise OpError("cwd %s: target user has no home directory" % cwd, code="spawn_failed")
    if cwd == "~":
        return home
    if cwd.startswith("~/"):
        return os.path.normpath(os.path.join(home, cwd[2:]))
    return os.path.normpath(os.path.join(home, cwd))


def _spec_home(sp):
    user = sp.get("user")
    if user is not None:
        try:
            return (pwd.getpwuid(user) if isinstance(user, int) else pwd.getpwnam(user)).pw_dir
        except KeyError:
            pass
    env = sp.get("env")
    if isinstance(env, dict) and env.get("HOME"):
        return env["HOME"]
    return os.environ.get("HOME") or pwd.getpwuid(os.getuid()).pw_dir


def spawn_error_info(err, argv=None, cwd=None):
    """Return (message, code) for an exception raised while spawning a command."""
    if isinstance(err, ValueError):
        return str(err), "invalid_argument"
    if isinstance(err, PermissionError) or getattr(err, "errno", None) in (errno.EACCES, errno.EPERM):
        return str(err), "permission_denied"
    if isinstance(err, FileNotFoundError) or getattr(err, "errno", None) == errno.ENOENT:
        filename = getattr(err, "filename", None)
        if cwd and (filename == cwd or not os.path.exists(cwd)):
            return "cwd %s: %s" % (cwd, getattr(err, "strerror", None) or str(err)), "spawn_failed"
        cmd = argv[0] if argv else None
        if cmd and cmd not in str(err):
            return "command %s: %s" % (cmd, err), "spawn_failed"
        return str(err), "spawn_failed"
    return str(err), "spawn_failed"

# ---------------------------------------------------------------- status

def write_json(path, obj):
    tmp = path + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(obj, f)
    os.replace(tmp, path)


def boot_id():
    try:
        with open("/proc/sys/kernel/random/boot_id") as f:
            return f.read().strip()
    except OSError:
        return ""


def proc_start(pid):
    """Kernel start time of pid (tells a reused pid apart), or None if gone."""
    try:
        with open("/proc/%d/stat" % pid) as f:
            fields = f.read().rpartition(")")[2].split()
        return None if fields[0] == "Z" else fields[19]
    except FileNotFoundError:
        pass
    except OSError:
        return None
    if os.path.isdir("/proc/self"):
        return None  # Linux, and the pid is gone
    try:
        os.kill(pid, 0)  # no /proc (macOS tests)
        return ""
    except ProcessLookupError:
        return None
    except PermissionError:
        return ""


def cmd_dir(cmd_id):
    if not isinstance(cmd_id, str) or not CMD_ID.match(cmd_id):
        raise OpError("No such command: %r" % (cmd_id,), code="not_found")
    return os.path.join(CMD_ROOT, cmd_id)


def read_raw_status(cmd_id):
    try:
        with open(os.path.join(cmd_dir(cmd_id), "status.json")) as f:
            return json.load(f)
    except FileNotFoundError:
        raise OpError("No such command: %s" % cmd_id, code="not_found")


def public_status(raw):
    """What callers see; a `running` whose supervisor is gone becomes `lost`."""
    s = {k: raw[k] for k in ("cmd_id", "argv", "state", "pid", "started", "ended", "exit", "signal")
         if raw.get(k) is not None}
    if raw["state"] == "running" and (
            raw.get("boot_id") != boot_id()
            or proc_start(raw["supervisor"]) != raw.get("supervisor_start")):
        s["state"] = "lost"
    s["running"] = s["state"] == "running"
    return s


def all_statuses():
    try:
        names = os.listdir(CMD_ROOT)
    except FileNotFoundError:
        return []
    out = []
    for name in names:
        try:
            raw = read_raw_status(name)
        except (OpError, ValueError):
            continue  # a directory still starting, or not ours
        if raw["state"] != "starting":
            out.append(public_status(raw))
    return sorted(out, key=lambda s: s.get("started", 0))


_running_lock = threading.Lock()
_running_cache = {
    "root": None,
    "dir_sig": None,
    "active": {},
    "finished": set(),
}


def _stat_sig(st):
    return (st.st_mtime_ns, st.st_ctime_ns, st.st_ino, st.st_dev)


def _file_sig(st):
    return (st.st_mtime_ns, st.st_size, st.st_ino, st.st_dev)


def _read_raw_status_with_sig(cmd_id):
    try:
        with open(os.path.join(cmd_dir(cmd_id), "status.json")) as f:
            st = os.fstat(f.fileno())
            return json.load(f), _file_sig(st)
    except FileNotFoundError:
        raise OpError("No such command: %s" % cmd_id, code="not_found")


def _invalidate_running_cache(cmd_id=None, raw=None):
    with _running_lock:
        if _running_cache["root"] == CMD_ROOT:
            _running_cache["dir_sig"] = None
            if cmd_id is not None:
                _running_cache["finished"].discard(cmd_id)
                if raw is not None and raw.get("state") in ("running", "starting"):
                    _running_cache["active"][cmd_id] = (raw, None)
                else:
                    _running_cache["active"].pop(cmd_id, None)


def _is_raw_running(raw, cur_boot_id):
    return (raw["state"] == "running"
            and raw.get("boot_id") == cur_boot_id
            and proc_start(raw["supervisor"]) == raw.get("supervisor_start"))


def running_count():
    with _running_lock:
        root = CMD_ROOT
        if _running_cache["root"] != root:
            _running_cache["root"] = root
            _running_cache["dir_sig"] = None
            _running_cache["active"] = {}
            _running_cache["finished"] = set()
        try:
            dir_sig = _stat_sig(os.stat(root))
        except FileNotFoundError:
            _running_cache["dir_sig"] = None
            _running_cache["active"] = {}
            _running_cache["finished"] = set()
            return 0

        active = _running_cache["active"]
        finished = _running_cache["finished"]

        if _running_cache["dir_sig"] != dir_sig:
            try:
                names = set(os.listdir(root))
            except FileNotFoundError:
                _running_cache["dir_sig"] = None
                active.clear()
                finished.clear()
                return 0
            finished.intersection_update(names)
            for cid in list(active):
                if cid not in names:
                    del active[cid]
            for name in names:
                if name in finished or name in active or not CMD_ID.match(name):
                    continue
                active[name] = (None, None)
            _running_cache["dir_sig"] = dir_sig

        if not active:
            return 0

        cur_boot_id = boot_id()
        count = 0
        for cid in list(active):
            raw, sig = active[cid]
            status_path = os.path.join(root, cid, "status.json")
            try:
                cur_sig = _file_sig(os.stat(status_path))
            except FileNotFoundError:
                if not os.path.isdir(os.path.join(root, cid)):
                    del active[cid]
                continue
            if raw is None or cur_sig != sig:
                try:
                    raw, cur_sig = _read_raw_status_with_sig(cid)
                except (OpError, ValueError):
                    continue
            if raw["state"] == "starting":
                active[cid] = (raw, cur_sig)
                continue
            if _is_raw_running(raw, cur_boot_id):
                active[cid] = (raw, cur_sig)
                count += 1
            else:
                del active[cid]
                finished.add(cid)
        return count


def prune():
    finished = [s for s in all_statuses() if not s["running"]]
    pruned = finished[:max(0, len(finished) - CMD_KEEP)]
    for s in pruned:
        shutil.rmtree(cmd_dir(s["cmd_id"]), ignore_errors=True)
    if pruned:
        _invalidate_running_cache()


# ---------------------------------------------------------------- supervisor

class LogWriter:
    """Appends to seg.<absolute offset>, keeping only the newest two segments."""

    def __init__(self, cdir, segment_size):
        self.cdir, self.segment_size = cdir, segment_size
        self.total, self.seg_size, self.fd, self.segs = 0, 0, None, []

    def _roll(self):
        if self.fd is not None:
            os.close(self.fd)
        path = os.path.join(self.cdir, "seg.%d" % self.total)
        self.fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        self.segs.append(path)
        self.seg_size = 0
        while len(self.segs) > 2:
            os.unlink(self.segs.pop(0))

    def write(self, data):
        if self.fd is None or self.seg_size >= self.segment_size:
            self._roll()
        os.write(self.fd, data)
        self.total += len(data)
        self.seg_size += len(data)


def finish_status(status, rc):
    status = dict(status, state="exited", ended=time.time(), exit=rc if rc >= 0 else 128 - rc)
    if rc < 0:
        status["signal"] = signal.Signals(-rc).name
    return status


def spawn_command(cdir, sp, status):
    spec_path = os.path.join(cdir, "spec.json")
    cwd = sp.get("cwd")
    try:
        cwd = resolve_cwd(cwd, _spec_home(sp))
        kw = {k: sp[k] for k in ("user", "group", "extra_groups") if sp.get(k) is not None}
        proc = subprocess.Popen(sp["argv"], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, env=sp["env"], cwd=cwd,
                                start_new_session=True, close_fds=True, **kw)
    except (OpError, OSError, ValueError, subprocess.SubprocessError) as e:
        if isinstance(e, OpError):
            err_msg, err_code = str(e), e.code
        else:
            err_msg, err_code = spawn_error_info(e, sp.get("argv"), cwd)
        failed = dict(status, state="failed", error=err_msg)
        if err_code:
            failed["error_code"] = err_code
        write_json(os.path.join(cdir, "status.json"), failed)
        os.unlink(spec_path)
        return None, status
    status = dict(status, state="running", pid=proc.pid, started=time.time())
    write_json(os.path.join(cdir, "status.json"), status)
    os.unlink(spec_path)  # it holds the environment
    return proc, status


def supervise(cdir):
    if os.fork():
        os._exit(0)  # the agent reaps this parent; init adopts the supervisor
    os.setsid()
    with open(os.path.join(cdir, "spec.json")) as f:
        sp = json.load(f)
    status = {"cmd_id": os.path.basename(cdir), "argv": sp["argv"], "state": "starting",
              "boot_id": boot_id(), "supervisor": os.getpid(),
              "supervisor_start": proc_start(os.getpid())}
    proc, status = spawn_command(cdir, sp, status)
    if proc is None:
        return
    pipe, exited_at = proc.stdout.fileno(), None
    log = LogWriter(cdir, sp.get("log_segment") or LOG_SEGMENT)
    log._roll()
    status_path = os.path.join(cdir, "status.json")
    while True:
        ready, _, _ = select.select([pipe], [], [], 0.2)
        data = os.read(pipe, 65536) if ready else None
        if data:
            log.write(data)
        # EOF does not mean exited: the command may have just closed stdout.
        rc = proc.wait() if data == b"" else proc.poll()
        now = time.monotonic()
        if rc is not None and exited_at is None:
            exited_at = now
        if status["state"] == "running" and exited_at is not None and (
                data == b"" or now - exited_at >= EXIT_DRAIN_S):
            status = finish_status(status, rc)  # a background child may keep the pipe
            write_json(status_path, status)
        if data == b"":
            return


# ---------------------------------------------------------------- cmd_* ops

def op_cmd_start(req, send, sp):
    """`sp` is the agent's resolved {argv, env, cwd, user, group, extra_groups}."""
    os.makedirs(CMD_ROOT, mode=0o700, exist_ok=True)
    cmd_id = "c_" + secrets.token_hex(8)
    cdir = os.path.join(CMD_ROOT, cmd_id)
    os.mkdir(cdir, 0o700)
    write_json(os.path.join(cdir, "spec.json"), dict(sp, log_segment=LOG_SEGMENT))
    try:
        subprocess.run([sys.executable, "-I", os.path.abspath(__file__), "--supervise", cdir],
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       check=True, timeout=START_TIMEOUT_S)
    except subprocess.SubprocessError as e:
        shutil.rmtree(cdir, ignore_errors=True)
        raise OpError("start: %s" % e)
    end = time.monotonic() + START_TIMEOUT_S
    while time.monotonic() < end:
        try:
            raw = read_raw_status(cmd_id)
        except (OpError, ValueError):
            raw = {"state": "starting"}
        if raw["state"] == "failed":
            shutil.rmtree(cdir, ignore_errors=True)
            raise OpError("start: %s" % raw.get("error"), code=raw.get("error_code"))
        if raw["state"] != "starting":
            _invalidate_running_cache(cmd_id, raw)
            prune()
            reply = {"ok": True, "cmd_id": cmd_id}
            if raw.get("pid") is not None:
                reply["pid"] = raw["pid"]
            send(reply)
            return
        time.sleep(0.01)
    raise OpError("start: supervisor did not report in %d s" % START_TIMEOUT_S)


def op_cmd_list(req, send):
    send({"ok": True, "cmds": all_statuses()})


def op_cmd_status(req, send):
    send(dict(public_status(read_raw_status(req.get("cmd_id"))), ok=True))


def segments(cdir):
    out = []
    for name in os.listdir(cdir):
        if name.startswith("seg."):
            start = int(name[4:])
            out.append((start, os.path.getsize(os.path.join(cdir, name)), name))
    return sorted(out)


def read_log(cdir, offset, limit):
    """(data, start, size, truncated) from the retained segments."""
    segs = segments(cdir)
    if not segs:
        return b"", 0, 0, False
    base, size = segs[0][0], segs[-1][0] + segs[-1][1]
    truncated = offset < base
    start = base if truncated else min(offset, size)
    data, pos = b"", start
    for seg_start, seg_size, name in segs:
        if len(data) >= limit or pos >= seg_start + seg_size:
            continue
        with open(os.path.join(cdir, name), "rb") as f:
            f.seek(pos - seg_start)
            chunk = f.read(limit - len(data))
        data += chunk
        pos += len(chunk)
    return data, start, size, truncated


def op_cmd_logs(req, send):
    raw = read_raw_status(req.get("cmd_id"))
    offset, limit = int(req.get("offset") or 0), int(req.get("limit") or LOGS_DEFAULT)
    if offset < 0 or limit <= 0:
        raise OpError("offset and limit must not be negative", code="invalid_argument")
    limit = min(limit, LOGS_MAX)
    cdir = cmd_dir(raw["cmd_id"])
    for _ in range(3):
        try:
            data, start, size, truncated = read_log(cdir, offset, limit)
            break
        except FileNotFoundError:  # a segment rotated away mid-read
            time.sleep(0.01)
    else:
        raise OpError("logs: rotating too fast to read")
    st = public_status(raw)
    # exit/signal are always present (null while running or not applicable), so a
    # caller can tell this agent from an older one and skip a cmd_status call.
    send({"ok": True, "data_b64": base64.b64encode(data).decode(), "offset": start,
          "next_offset": start + len(data), "size": size, "truncated": truncated,
          "running": st["running"], "exit": st.get("exit"), "signal": st.get("signal")})


def parse_signal(value):
    try:
        if isinstance(value, bool):
            raise ValueError
        if isinstance(value, int):
            return signal.Signals(value)
        name = value.upper()
        return signal.Signals[name if name.startswith("SIG") else "SIG" + name]
    except (KeyError, ValueError, AttributeError):
        raise OpError("bad signal: %r" % (value,), code="invalid_argument")


def op_cmd_signal(req, send):
    sig = parse_signal(req.get("signal"))
    s = public_status(read_raw_status(req.get("cmd_id")))
    if not s["running"]:
        raise OpError("command is not running: %s" % s["state"], code="not_running")
    try:
        os.killpg(s["pid"], sig)
    except ProcessLookupError:
        raise OpError("command is not running", code="not_running")
    send({"ok": True})


# ---------------------------------------------------------------- file_* ops

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
    d = d.rstrip("/")
    if not d or os.path.isdir(d):
        return
    makedirs_owned(os.path.dirname(d))
    try:
        os.mkdir(d, 0o755)
    except FileExistsError:
        if os.path.isdir(d):
            return
        raise
    chown_like_parent(d, None)


def op_put_file(req, send):
    path = req.get("path")
    if not isinstance(path, str) or not os.path.isabs(path):
        raise OpError("path must be absolute", code="invalid_argument")
    data = b64d(req.get("data_b64"))
    offset = int(req.get("offset") or 0)
    last = req.get("last", True)
    part = path + ".boat-part"
    makedirs_owned(os.path.dirname(path))
    flags = os.O_WRONLY | os.O_CREAT | (os.O_TRUNC if offset == 0 else 0)
    fd = os.open(part, flags, 0o600)
    try:
        if offset and os.fstat(fd).st_size != offset:
            raise OpError("offset %d != received %d" % (offset, os.fstat(fd).st_size), code="invalid_argument")
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
    send({"ok": True, "size": size, "complete": bool(last)})


def op_get_file(req, send):
    path = req.get("path")
    if not isinstance(path, str) or not os.path.isabs(path):
        raise OpError("path must be absolute", code="invalid_argument")
    try:
        f = open(path, "rb")
    except OSError as e:
        raise OpError("open %s: %s" % (path, e.strerror), code=oserror_code(e))
    with f:
        st = os.fstat(f.fileno())
        offset = 0
        while True:
            data = f.read(CHUNK)
            if not data:
                break
            send({"offset": offset, "data_b64": base64.b64encode(data).decode()})
            offset += len(data)
    send({"eof": True, "size": offset, "mode": "%o" % (st.st_mode & 0o7777)})


def abs_path(value, field="path"):
    if not isinstance(value, str) or not os.path.isabs(value) or "\0" in value:
        raise OpError("%s must be an absolute path" % field, code="invalid_argument")
    return value


def entry_type(mode_is):
    if mode_is.is_symlink():
        return "symlink"
    if mode_is.is_dir(follow_symlinks=False):
        return "dir"
    return "file" if mode_is.is_file(follow_symlinks=False) else "other"


def op_file_list(req, send):
    path = abs_path(req.get("path"))
    entries, truncated = [], False
    with os.scandir(path) as it:
        for e in sorted(it, key=lambda x: x.name):
            if len(entries) >= FILE_LIST_MAX:
                truncated = True
                break
            try:
                st = e.stat(follow_symlinks=False)
            except FileNotFoundError:
                continue  # removed while listing
            entries.append({"name": e.name, "type": entry_type(e), "size": st.st_size,
                            "mode": "%o" % (st.st_mode & 0o7777), "mtime": int(st.st_mtime)})
    send({"ok": True, "entries": entries, "truncated": truncated})


def op_file_delete(req, send):
    path = abs_path(req.get("path"))
    norm = "/" + os.path.normpath(path).lstrip("/")
    if norm in UNDELETABLE:
        raise OpError("refusing to delete %s" % norm, code="protected_path")
    if os.path.isdir(path) and not os.path.islink(path):
        if req.get("recursive"):
            shutil.rmtree(path)  # does not follow symlinks
        else:
            try:
                os.rmdir(path)
            except OSError as e:
                if e.errno in (errno.ENOTEMPTY, errno.EEXIST):
                    raise OpError("%s is a non-empty directory; set recursive" % path, code="not_empty")
                raise
    else:
        os.unlink(path)
    send({"ok": True})


def op_file_move(req, send):
    src, dst = abs_path(req.get("from"), "from"), abs_path(req.get("to"), "to")
    os.lstat(src)  # a missing source reports "No such file" before anything else
    if os.path.lexists(dst):
        if not req.get("overwrite"):
            raise OpError("%s exists; set overwrite" % dst, code="already_exists")
        if os.path.isdir(dst) and not os.path.islink(dst):
            raise OpError("%s is a directory; delete it first" % dst, code="conflict")
    makedirs_owned(os.path.dirname(dst))
    try:
        os.replace(src, dst)
    except OSError as e:
        if e.errno != 18:  # EXDEV: another filesystem
            raise
        shutil.move(src, dst)
    send({"ok": True})


def start_rg(argv):
    try:
        proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE)
    except FileNotFoundError:
        raise OpError("search: ripgrep is not installed")
    timer = threading.Timer(SEARCH_TIMEOUT_S, proc.kill)
    timer.start()
    return proc, timer


def op_file_search(req, send):
    path = abs_path(req.get("path"))
    query = req.get("query")
    if not isinstance(query, str) or not query:
        raise OpError("query must be a non-empty string", code="invalid_argument")
    os.stat(path)
    limit = max(1, min(int(req.get("limit") or SEARCH_DEFAULT), SEARCH_MAX))
    content = req.get("content", True)
    if content:
        argv = [RG, "--no-config", "--json", "--no-messages"]
        argv += [] if req.get("regex") else ["--fixed-strings"]
        argv += ["--", query, path]
    else:
        argv = [RG, "--no-config", "--files", "--no-messages", path]
    proc, timer = start_rg(argv)
    matches, truncated, needle = [], False, query.lower()
    try:
        for line in proc.stdout:
            m = parse_rg_line(line, content, needle)
            if m is None:
                continue
            if len(matches) >= limit:
                truncated = True
                break
            matches.append(m)
    finally:
        timer.cancel()
        if truncated:
            proc.kill()
        err = proc.stderr.read(4096) if not truncated else b""
        rc = proc.wait()
        proc.stdout.close()
        proc.stderr.close()
    if rc < 0 and not truncated:
        truncated = True  # the timeout killed it; what we have is partial
    if rc == 2 and not matches:
        raise OpError("search: %s" % err.decode("utf-8", "replace").strip(), code="invalid_argument")
    send({"ok": True, "matches": matches, "truncated": truncated})


def rg_text(obj):
    if "text" in obj:
        return obj["text"]
    return base64.b64decode(obj.get("bytes", "")).decode("utf-8", "replace")


def parse_rg_line(line, content, needle):
    if not content:
        name = line.decode("utf-8", "replace").rstrip("\n")
        return {"path": name} if needle in os.path.basename(name).lower() else None
    try:
        msg = json.loads(line)
    except ValueError:
        return None
    if msg.get("type") != "match":
        return None
    d = msg["data"]
    return {"path": rg_text(d["path"]), "line": d["line_number"],
            "text": rg_text(d["lines"]).rstrip("\n")[:SEARCH_TEXT_MAX]}


if __name__ == "__main__":
    if sys.argv[1:2] == ["--supervise"]:
        supervise(sys.argv[2])
