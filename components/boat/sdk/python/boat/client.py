"""Boat API client built on http.client with keep-alive pooling. Python 3.8+."""
import base64
import datetime
from concurrent.futures import ThreadPoolExecutor
import errno
import hashlib
import hmac
import http.client
import json
import math
import os
import secrets as _secrets
import select
import socket
import ssl
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import weakref
from dataclasses import dataclass, field
from typing import Any, Dict, Iterable, Iterator, List, Mapping, Optional, Sequence, Tuple, Union


class BoatError(Exception):
    """API failure; reconnect errors carry the server's optional next_offset.

    Pass that absolute byte cursor to follow_command/follow_prompt to resume.
    None means the server supplied no cursor; it must not be inferred from
    received byte counts because retained logs may start after a gap.
    """

    def __init__(self, status: int, code: str, message: str, request_id: str = "",
                 next_offset: Optional[int] = None, vm: Optional[Dict[str, Any]] = None,
                 vms: Optional[List[Dict[str, Any]]] = None):
        super().__init__(f"{message} (HTTP {status}{', ' + request_id if request_id else ''})")
        self.status, self.code, self.message, self.request_id = status, code, message, request_id
        self.next_offset = next_offset
        self.vm = vm  # VM body some error answers carry (legacy env_update_failed, env_start_failed)
        self.vms = vms  # a batch create's VMs (env_start_failed: the failed ones stopped)


class BoatTimeoutError(BoatError, TimeoutError):
    """Raised when a caller-supplied timeout budget is invalid or expired."""

    def __init__(self, message: str = "timed out"):
        BoatError.__init__(self, 0, "timeout", message)


@dataclass(frozen=True)
class VM:
    id: str
    name: str = ""
    size: str = ""
    state: str = ""
    image: str = ""
    team: str = ""
    labels: Dict[str, str] = field(default_factory=dict)
    raw: Dict[str, Any] = field(default_factory=dict, repr=False, compare=False)
    _client: Optional["Boat"] = field(default=None, init=False, repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Dict[str, Any], client: Optional["Boat"] = None) -> "VM":
        vm = cls(id=d.get("id", ""), name=d.get("name") or "", size=d.get("size") or "",
                 state=d.get("state") or "", image=d.get("image") or "", team=d.get("team") or "",
                 labels=d.get("labels") or {}, raw=d)
        if client is not None:
            object.__setattr__(vm, "_client", client)
        return vm

    def _require_client(self) -> "Boat":
        if self._client is None:
            raise RuntimeError("VM is not bound to a Boat client")
        return self._client

    def _sync_from(self, updated: "VM") -> "VM":
        for attr in ("name", "size", "state", "image", "team", "labels", "raw"):
            object.__setattr__(self, attr, getattr(updated, attr))
        return updated

    def update(self, name: Optional[str] = None, ttl_s: Optional[int] = None,
               extend_s: Optional[int] = None, labels: Optional[Mapping[str, str]] = None,
               idle_stop_s: Optional[int] = None, **removed: Any) -> "VM":
        updated = self._require_client().update_vm(
            self.id, name=name, ttl_s=ttl_s, extend_s=extend_s,
            labels=labels, idle_stop_s=idle_stop_s, **removed,
        )
        return self._sync_from(updated)

    def start(self, fail_fast: bool = False, env: Optional[Mapping[str, str]] = None,
              no_env: Optional[bool] = None, ttl_s: Optional[int] = None,
              allow_env_error: bool = False, environment: Optional[str] = None) -> "VM":
        return self._sync_from(self._require_client().start(
            self.id, fail_fast=fail_fast, env=env, no_env=no_env, environment=environment, ttl_s=ttl_s, allow_env_error=allow_env_error))

    def stop(self, force: bool = False) -> "VM":
        return self._sync_from(self._require_client().stop(self.id, force=force))

    def fork(self, name: Optional[str] = None, fail_fast: bool = False,
             env: Optional[Mapping[str, str]] = None, labels: Optional[Mapping[str, str]] = None,
             no_env: Optional[bool] = None, ttl_s: Optional[int] = None,
             idempotency_key: Optional[str] = None, allow_env_error: bool = False,
             environment: Optional[str] = None) -> "VM":
        return self._require_client().fork(
            self.id, name=name, fail_fast=fail_fast, env=env,
            labels=labels, no_env=no_env, environment=environment, ttl_s=ttl_s, idempotency_key=idempotency_key,
            allow_env_error=allow_env_error)

    def clone(self, name: Optional[str] = None, fail_fast: bool = False,
              env: Optional[Mapping[str, str]] = None, labels: Optional[Mapping[str, str]] = None,
              no_env: Optional[bool] = None, ttl_s: Optional[int] = None,
              allow_env_error: bool = False, environment: Optional[str] = None) -> "VM":
        return self._require_client().clone_vm(
            self.id, name=name, fail_fast=fail_fast, env=env,
            labels=labels, no_env=no_env, environment=environment, ttl_s=ttl_s, allow_env_error=allow_env_error)

    def grant_ssh(self, public_key: Optional[str] = None) -> "SSHInfo":
        return self._require_client().grant_ssh(self.id, public_key=public_key)

    def revoke_ssh(self, fingerprint: str) -> Dict[str, Any]:
        return self._require_client().revoke_ssh(self.id, fingerprint)

    def revoke_port_tokens(self) -> Dict[str, Any]:
        return self._require_client().revoke_port_tokens(self.id)

    def delete(self) -> None:
        self._require_client().delete(self.id)

    def exec(self, cmd: List[str], env: Optional[Mapping[str, str]] = None, cwd: Optional[str] = None,
             timeout_s: Optional[int] = None, stdin: Union[str, bytes, None] = None,
             tty: bool = False) -> Iterator["ExecEvent"]:
        return self._require_client().exec(self.id, cmd, env=env, cwd=cwd, timeout_s=timeout_s,
                                           stdin=stdin, tty=tty)

    def run(self, cmd: List[str], env: Optional[Mapping[str, str]] = None, cwd: Optional[str] = None,
            timeout_s: Optional[int] = None, stdin: Union[str, bytes, None] = None,
            tty: bool = False, **kw: Any) -> "ExecResult":
        return self._require_client().run(self.id, cmd, env=env, cwd=cwd, timeout_s=timeout_s,
                                          stdin=stdin, tty=tty, **kw)


VMHandle = VM


@dataclass(frozen=True)
class ExecEvent:
    """One output chunk or final exit event.

    next_offset is the server's absolute byte cursor for explicit follow resume,
    or None when absent. It is not a count of bytes received by this iterator.
    """
    stream: str = ""  # "stdout" | "stderr" | "" for the exit event
    data: bytes = b""
    exit_code: Optional[int] = None
    error: Optional[Any] = None
    next_offset: Optional[int] = None
    code: Optional[str] = None


@dataclass(frozen=True)
class ExecResult:
    stdout: bytes
    stderr: bytes
    exit_code: int


@dataclass(frozen=True)
class JobResult:
    """What run_job() returns."""
    stdout: bytes
    stderr: bytes
    exit_code: int
    elapsed_s: float
    size: str
    vm_id: str
    deleted: bool


@dataclass(frozen=True)
class SSHInfo:
    host: str
    port: int
    user: str
    jump: str = ""


@dataclass(frozen=True)
class DesktopURL:
    url: str
    expires_at: str


@dataclass(frozen=True)
class Command:
    id: str
    cmd: List[str] = field(default_factory=list)
    state: str = ""  # running | exited | signaled | lost
    exit: Optional[int] = None
    signal: Optional[str] = None

    @classmethod
    def from_json(cls, d: Dict[str, Any]) -> "Command":
        return cls(id=d["id"], cmd=d.get("cmd") or [], state=d.get("state", ""), exit=d.get("exit"), signal=d.get("signal"))


@dataclass(frozen=True)
class PromptRun:
    id: str
    conversation_id: str
    agent: str = ""
    cmd_id: str = ""
    state: str = ""  # running | completed | failed | interrupted | steered
    exit: Optional[int] = None
    steered_by: str = ""

    @classmethod
    def from_json(cls, d: Dict[str, Any]) -> "PromptRun":
        return cls(id=d["id"], conversation_id=d["conversation_id"], agent=d.get("agent", ""), cmd_id=d.get("cmd_id", ""),
                   state=d.get("state", ""), exit=d.get("exit"), steered_by=d.get("steered_by", ""))


def _copy(src, dst) -> int:
    total = 0
    while True:
        chunk = src.read(1 << 20)
        if not chunk:
            return total
        dst.write(chunk)
        total += len(chunk)


def _config() -> Dict[str, str]:
    path = os.environ.get("BOAT_CONFIG") or os.path.join(
        os.environ.get("XDG_CONFIG_HOME") or os.path.expanduser("~/.config"), "boat", "config.json")
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


# The archive lands as root in sticky /tmp; a path outside home may need root.
_UNPACK = """d=$0; case "$d" in /*) ;; *) d=$HOME/$d ;; esac
mkdir -p -- "$d" 2>/dev/null || sudo -n install -d -o "$(id -u)" -g "$(id -g)" -- "$d" || exit 1
tar xzf "$1" -C "$d"; rc=$?
rm -f -- "$1" 2>/dev/null || sudo -n rm -f -- "$1"
[ "$rc" = 0 ] && printf '%s\\n' "$d"; exit "$rc"
"""
# run_job has no limit by default; the agent's default exec timeout is 600 s.
_NO_LIMIT_S = 30 * 24 * 60 * 60
_JOB_IDLE_STOP_S = 15 * 60
_JOB_TTL_S = 15 * 60
_JOB_HEARTBEAT_S = 5 * 60.0
_DEFAULT_TIMEOUT = object()


def _api_origin(url: str) -> str:
    """Normalize base URL, stripping trailing /api/v1 or /v1 as the CLI does."""
    u = url.strip().rstrip("/")
    for suf in ("/api/v1", "/v1"):
        if u.endswith(suf):
            return u[:-len(suf)].rstrip("/")
    return u


def _push_files(folder: str) -> List[str]:
    """In a git repository: tracked and new unignored files, with submodules. Else all but .git."""
    try:
        out = subprocess.run(["git", "-C", folder, "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                             capture_output=True, check=True).stdout.decode()
    except (OSError, subprocess.CalledProcessError):
        found = []
        for root, dirs, names in os.walk(folder):
            dirs[:] = [d for d in dirs if d != ".git"]
            found += [os.path.relpath(os.path.join(root, n), folder) for n in names if n != ".git"]
        return found
    files: List[str] = []
    for name in sorted(set(filter(None, out.split("\0")))):
        path = os.path.join(folder, name)
        if os.path.isdir(path) and not os.path.islink(path):  # a submodule
            files += [os.path.join(name, x) for x in _push_files(path)]
        elif os.path.lexists(path):  # git can track a file deleted here
            files.append(name)
    return files


def _q(s: str) -> str:
    return urllib.parse.quote(str(s), safe="")


_IDEMPOTENT_METHODS = frozenset({"GET", "HEAD", "PUT", "DELETE", "OPTIONS"})
_STALE_CONN_ERRORS = (
    http.client.RemoteDisconnected,
    http.client.BadStatusLine,
    ConnectionResetError,
    BrokenPipeError,
    ConnectionAbortedError,
)
_PATCH_VM_FIELDS = ("name", "ttl_s", "extend_s", "labels", "idle_stop_s")


def _safe_close_conn(conn: Optional[http.client.HTTPConnection]) -> None:
    if conn is None:
        return
    try:
        conn.close()
    except OSError:
        pass


def _close_idle_pool(idle: Dict[Tuple[str, str, int], List[http.client.HTTPConnection]],
                     lock: threading.Lock) -> None:
    conns: List[http.client.HTTPConnection] = []
    with lock:
        for bucket in idle.values():
            conns.extend(bucket)
        idle.clear()
    for conn in conns:
        _safe_close_conn(conn)


def _is_conn_alive(conn: http.client.HTTPConnection) -> bool:
    sock = getattr(conn, "sock", None)
    if sock is None:
        return False
    try:
        pending = getattr(sock, "pending", None)
        if callable(pending) and pending() > 0:
            return False
        fd = sock.fileno()
        if fd < 0:
            return False
        if hasattr(select, "poll"):
            poller = select.poll()
            poller.register(fd, select.POLLIN | select.POLLPRI | select.POLLHUP | select.POLLERR)
            return not poller.poll(0)
        rlist, _, xlist = select.select([sock], [], [sock], 0.0)
        return not (rlist or xlist)
    except (OSError, ValueError):
        return False


class _PooledResponse:
    """File-like wrapper around HTTPResponse that recycles consumed keep-alive connections."""

    def __init__(self, pool: "_ConnectionPool", key: Tuple[str, str, int],
                 conn: http.client.HTTPConnection, resp: http.client.HTTPResponse):
        self._pool = pool
        self._key = key
        self._conn: Optional[http.client.HTTPConnection] = conn
        self._resp = resp
        self._released = False

    @property
    def status(self) -> int:
        return self._resp.status

    @property
    def reason(self) -> str:
        return self._resp.reason

    @property
    def headers(self) -> Any:
        return self._resp.headers

    @property
    def fp(self) -> Any:
        return self._resp.fp

    def isclosed(self) -> bool:
        return self._released or self._resp.isclosed()

    def _finish_if_eof(self) -> None:
        if not self._released and self._resp.isclosed():
            conn = self._conn
            self._conn = None
            self._released = True
            if conn is not None:
                if not self._resp.will_close:
                    self._pool._checkin(self._key, conn)
                else:
                    _safe_close_conn(conn)

    def read(self, amt: Optional[int] = None) -> bytes:
        try:
            data = self._resp.read() if amt is None else self._resp.read(amt)
        except BaseException:
            self.close()
            raise
        self._finish_if_eof()
        return data

    def readline(self, limit: int = -1) -> bytes:
        try:
            line = self._resp.readline() if limit < 0 else self._resp.readline(limit)
        except BaseException:
            self.close()
            raise
        self._finish_if_eof()
        return line

    def close(self) -> None:
        if self._released:
            return
        conn = self._conn
        self._conn = None
        self._released = True
        if self._resp.isclosed() and not self._resp.will_close:
            if conn is not None:
                self._pool._checkin(self._key, conn)
            return
        try:
            self._resp.close()
        except OSError as exc:
            if exc.errno != errno.EBADF:
                raise
        _safe_close_conn(conn)

    def __enter__(self) -> "_PooledResponse":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()

    def __iter__(self) -> "_PooledResponse":
        return self

    def __next__(self) -> bytes:
        line = self.readline()
        if not line:
            raise StopIteration
        return line

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class _ConnectionPool:
    """Thread-safe HTTP/HTTPS keep-alive connection pool keyed by (scheme, host, port)."""

    def __init__(self, max_idle_per_origin: int = 32):
        self._max_idle = max_idle_per_origin
        self._lock = threading.Lock()
        self._idle: Dict[Tuple[str, str, int], List[http.client.HTTPConnection]] = {}
        self._closed = False
        self._ssl_ctx: Optional[ssl.SSLContext] = None
        self._finalizer = weakref.finalize(self, _close_idle_pool, self._idle, self._lock)

    def _get_ssl_ctx(self) -> ssl.SSLContext:
        with self._lock:
            if self._ssl_ctx is None:
                self._ssl_ctx = ssl.create_default_context()
            return self._ssl_ctx

    def _checkout(self, key: Tuple[str, str, int],
                  timeout: Optional[float]) -> Tuple[http.client.HTTPConnection, bool]:
        scheme, host, port = key
        while True:
            conn: Optional[http.client.HTTPConnection] = None
            with self._lock:
                bucket = self._idle.get(key)
                if bucket:
                    conn = bucket.pop()
                    if not bucket:
                        self._idle.pop(key, None)
            if conn is None:
                break
            if _is_conn_alive(conn):
                try:
                    conn.timeout = timeout  # type: ignore[assignment]
                    if conn.sock is not None:
                        conn.sock.settimeout(timeout)
                    return conn, True
                except OSError:
                    _safe_close_conn(conn)
            else:
                _safe_close_conn(conn)
        if scheme == "https":
            return http.client.HTTPSConnection(host, port, timeout=timeout, context=self._get_ssl_ctx()), False
        if scheme == "http":
            return http.client.HTTPConnection(host, port, timeout=timeout), False
        raise ValueError(f"unsupported URL scheme: {scheme!r}")

    def _checkin(self, key: Tuple[str, str, int], conn: http.client.HTTPConnection) -> None:
        if getattr(conn, "sock", None) is None:
            _safe_close_conn(conn)
            return
        evicted: Optional[http.client.HTTPConnection] = None
        close_now = False
        with self._lock:
            if self._closed:
                close_now = True
            else:
                bucket = self._idle.setdefault(key, [])
                bucket.append(conn)
                if len(bucket) > self._max_idle:
                    evicted = bucket.pop(0)
        if close_now:
            _safe_close_conn(conn)
        if evicted is not None:
            _safe_close_conn(evicted)

    def request(self, url: str, method: str, headers: Mapping[str, str],
                body: Any = None, timeout: Optional[float] = None) -> _PooledResponse:
        parsed = urllib.parse.urlsplit(url)
        scheme = (parsed.scheme or "").lower()
        host = parsed.hostname
        if not scheme or not host:
            raise ValueError(f"invalid request URL: {url!r}")
        port = parsed.port or (443 if scheme == "https" else 80)
        target = parsed.path or "/"
        if parsed.query:
            target = f"{target}?{parsed.query}"
        key = (scheme, host, port)

        is_bytes_body = body is None or isinstance(body, (bytes, bytearray, memoryview))
        body_pos: Optional[int] = None
        if not is_bytes_body and hasattr(body, "tell") and hasattr(body, "seek"):
            try:
                body_pos = body.tell()
            except (OSError, ValueError):
                body_pos = None
        can_rewind = is_bytes_body or body_pos is not None
        idempotent = method.upper() in _IDEMPOTENT_METHODS

        req_headers = dict(headers)
        req_headers.setdefault("Connection", "keep-alive")

        for attempt in (0, 1):
            conn, reused = self._checkout(key, timeout)
            sent_request = False
            try:
                conn.request(method, target, body=body, headers=req_headers)
                sent_request = True
                resp = conn.getresponse()
                return _PooledResponse(self, key, conn, resp)
            except _STALE_CONN_ERRORS:
                _safe_close_conn(conn)
                if attempt == 0 and reused and can_rewind and (idempotent or not sent_request):
                    if body_pos is not None:
                        body.seek(body_pos)
                    continue
                raise
            except BaseException:
                _safe_close_conn(conn)
                raise
        raise AssertionError("unreachable")  # pragma: no cover

    def close(self) -> None:
        with self._lock:
            self._closed = True
        _close_idle_pool(self._idle, self._lock)


class Boat:
    """Client. url/key default to BOAT_URL/BOAT_API_URL and BOAT_API_KEY, then ~/.config/boat/config.json."""

    def __init__(self, url: Optional[str] = None, key: Optional[str] = None, timeout: float = 60.0):
        cfg = _config()
        raw_url = url or os.environ.get("BOAT_URL") or os.environ.get("BOAT_API_URL") or cfg.get("url") or ""
        self.url = _api_origin(raw_url)
        self.key = key or os.environ.get("BOAT_API_KEY") or cfg.get("key") or ""
        if not self.url or not self.key:
            raise ValueError("Boat url and key are required (args, BOAT_URL/BOAT_API_URL/BOAT_API_KEY, or `boat login`)")
        self.timeout = timeout
        self._pool = _ConnectionPool()

    def close(self) -> None:
        """Close all pooled keep-alive HTTP/HTTPS connections."""
        self._pool.close()

    def __enter__(self) -> "Boat":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()

    # ---- transport ----

    def _open(self, method: str, path: str, body: Any = None,
              ctype: Optional[str] = None, timeout: Any = _DEFAULT_TIMEOUT,
              extra_headers: Optional[Mapping[str, str]] = None):
        headers = {
            "Authorization": "Bearer " + self.key,
            "User-Agent": "boat-python/0.1.0",
        }
        if extra_headers:
            headers.update(extra_headers)
        if ctype:
            headers["Content-Type"] = ctype
        eff_timeout = self.timeout if timeout is _DEFAULT_TIMEOUT else timeout
        resp = self._pool.request(self.url + path, method=method, headers=headers,
                                  body=body, timeout=eff_timeout)
        if resp.status >= 400:
            try:
                raw = resp.read()
            finally:
                resp.close()
            try:
                parsed = json.loads(raw)
                err = parsed["error"]
                vm = parsed.get("vm") if isinstance(parsed.get("vm"), dict) else (parsed if parsed.get("id") else None)
                vms = parsed.get("vms")
                vms = [v for v in vms if isinstance(v, dict)] if isinstance(vms, list) else None
                raise BoatError(resp.status, err.get("code", ""), err.get("message", ""),
                                err.get("request_id", ""), vm=vm, vms=vms) from None
            except (ValueError, KeyError, TypeError):
                raise BoatError(resp.status, "http_error",
                                raw.decode("utf-8", "replace")[:200] or resp.reason) from None
        return resp

    @staticmethod
    def _idem(key: Optional[str]) -> Optional[Dict[str, str]]:
        if key is None:
            return None
        if not isinstance(key, str) or not key.strip() or len(key) > 255:
            raise ValueError("idempotency_key must be a non-empty string of at most 255 characters")
        return {"Idempotency-Key": key}

    def _call(self, method: str, path: str, body: Any = None, *, timeout: Any = _DEFAULT_TIMEOUT,
              idempotency_key: Optional[str] = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        with self._open(method, path, data, "application/json" if data is not None else None, timeout=timeout,
                        extra_headers=self._idem(idempotency_key)) as r:
            raw = r.read()
        return json.loads(raw) if raw.strip() else None

    def _stream(self, path: str, body: Dict[str, Any], *, _deadline: Optional[float] = None) -> Iterator[ExecEvent]:
        # No read timeout by default: agents can be silent for minutes.
        if _deadline is None:
            open_timeout: Any = None
        else:
            open_timeout = _deadline - time.monotonic()
            if open_timeout <= 0:
                raise socket.timeout("timed out")
        r = self._open("POST", path, json.dumps(body).encode(), "application/json", timeout=open_timeout)
        with r:
            for line in _iter_lines(r, _deadline):
                line = line.strip()
                if not line:
                    continue
                ev = json.loads(line)
                err = ev.get("error")
                err_code = ev.get("code")
                if err_code is None and isinstance(err, dict):
                    err_code = err.get("code")
                yield ExecEvent(stream=ev.get("stream", ""),
                                data=base64.b64decode(ev["data_b64"]) if ev.get("data_b64") else b"",
                                exit_code=124 if ev.get("timed_out") else ev.get("exit"), error=err, code=err_code)

    @staticmethod
    def _clean(d: Dict[str, Any]) -> Dict[str, Any]:
        return {k: v for k, v in d.items() if v is not None}

    def _vm(self, d: Dict[str, Any]) -> VM:
        return VM.from_json(d, client=self)

    # ---- identity ----

    def me(self) -> Dict[str, Any]:
        return self._call("GET", "/v1/me")

    def images(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/images")["images"]

    @staticmethod
    def _usage_bound(v: Any) -> Optional[str]:
        # datetime -> RFC3339 UTC (naive = UTC); date -> YYYY-MM-DD; strings pass through.
        if v is None:
            return None
        if isinstance(v, datetime.datetime):
            if v.tzinfo is not None:
                v = v.astimezone(datetime.timezone.utc)
            return v.replace(tzinfo=None).isoformat(timespec="seconds") + "Z"
        if isinstance(v, datetime.date):
            return v.isoformat()
        return v

    def usage(self, frm: Union[str, datetime.date, datetime.datetime, None] = None,
              to: Union[str, datetime.date, datetime.datetime, None] = None) -> Dict[str, Any]:
        """Usage for a window. frm/to accept "YYYY-MM-DD" or RFC3339 strings, a date, or a datetime
        (serialized as RFC3339 UTC; naive datetimes are taken as UTC). A date-only `to` (string or date) means
        the end of that UTC day: the day is inclusive."""
        q = urllib.parse.urlencode(self._clean({"from": self._usage_bound(frm), "to": self._usage_bound(to)}))
        return self._call("GET", "/v1/usage" + ("?" + q if q else ""))

    # ---- VMs ----

    def list_vms(self) -> List[VM]:
        return [self._vm(v) for v in self._call("GET", "/v1/vms")["vms"]]

    def get_vm(self, vm_id: str) -> VM:
        return self._vm(self._call("GET", f"/v1/vms/{_q(vm_id)}"))

    def create_vm(self, name: Optional[str] = None, size: Optional[str] = None, image: Optional[str] = None,
                  ttl_s: Optional[int] = None, labels: Optional[Dict[str, str]] = None,
                  env: Optional[Dict[str, str]] = None, secrets: Optional[List[str]] = None,
                  idle_stop_s: Optional[int] = None, snapshot: Optional[str] = None,
                  environment: Optional[str] = None, no_env: Optional[bool] = None,
                  from_share: Optional[str] = None, fail_fast: bool = False,
                  self_api: Optional[bool] = None, idempotency_key: Optional[str] = None,
                  allow_env_error: bool = False) -> VM:
        """ttl_s: auto-stop after this many seconds. Omitted (None) = 1 hour (server default); 0 = no auto-stop.
        allow_env_error: by default a VM whose environment delivery failed (env_error in the response)
        raises BoatError(code "env_failed") naming the VM id; the VM stays up, delete it or pass True to get it anyway.
        idempotency_key: sent as Idempotency-Key; a retry with the same key and body returns the first answer.
        secrets: team secret names to inject (None = the secrets an admin marked default-injected, [] = none); forks inherit it.
        environment runs that environment's setup (vm.raw["setup_cmd_id"]) unless it has a current build.
        from_share: a share code bsh_..., copied into your team first.
        fail_fast: a full host raises BoatError(503, "no_ready_machine") at once.
        self_api: inject BOAT_URL/BOAT_API_KEY self-token into the guest (default True, False for foreign disks)."""
        body = self._create_body(name, size, image, ttl_s, labels, env, secrets, idle_stop_s, snapshot,
                                 environment, no_env, from_share, fail_fast, self_api)
        out = self._call("POST", "/v1/vms", body, idempotency_key=idempotency_key)
        if not allow_env_error:
            self._check_env([out])
        return self._vm(out)

    create = create_vm

    @staticmethod
    def _check_env(vms: List[Dict[str, Any]]) -> None:
        bad = [v for v in vms if str(v.get("env_error") or "").strip()]
        if bad:
            detail = "; ".join(f"{v.get('id', '?')}: {str(v['env_error']).strip()}" for v in bad)
            raise BoatError(502, "env_failed",
                            f"environment delivery failed for {len(bad)} VM(s) that were created and are still up ({detail}); "
                            "delete them or pass allow_env_error=True")

    def create_vms(self, count: int, **kw: Any) -> List[VM]:
        """count (1-16) VMs in one request, all or none; a name x becomes x-1 … x-N.
        A fresh-boot failure (502 env_start_failed) keeps the VMs stopped and lists them in the error's `vms`.
        Takes create_vm's keyword arguments; raises BoatError("env_failed") listing the ids of any VM whose
        environment delivery failed unless allow_env_error=True."""
        body = self._create_body(kw.get("name"), kw.get("size"), kw.get("image"), kw.get("ttl_s"), kw.get("labels"),
                                 kw.get("env"), kw.get("secrets"), kw.get("idle_stop_s"), kw.get("snapshot"),
                                 kw.get("environment"), kw.get("no_env"), kw.get("from_share"),
                                 kw.get("fail_fast", False), kw.get("self_api"))
        body["count"] = count
        out = self._call("POST", "/v1/vms", body, idempotency_key=kw.get("idempotency_key"))
        raw = out["vms"] if "vms" in out else [out]
        if not kw.get("allow_env_error", False):
            self._check_env(raw)
        return [self._vm(v) for v in raw]

    def _create_body(self, name, size, image, ttl_s, labels, env, secrets, idle_stop_s, snapshot,
                     environment, no_env, from_share, fail_fast, self_api=None) -> Dict[str, Any]:
        return self._clean({"name": name, "size": size, "image": image, "ttl_s": ttl_s, "labels": labels, "env": env,
                            "secrets": None if secrets is None else list(secrets), "idle_stop_s": idle_stop_s,
                            "snapshot": snapshot, "environment": environment, "no_env": no_env, "from": from_share,
                            "fail_fast": True if fail_fast else None, "self_api": self_api})

    def update_vm(self, vm_id: str, name: Optional[str] = None,
                  ttl_s: Optional[int] = None,
                  extend_s: Optional[int] = None,
                  labels: Optional[Mapping[str, str]] = None,
                  idle_stop_s: Optional[int] = None, **removed: Any) -> VM:
        if "resume_on_boot" in removed:
            raise ValueError("resume_on_boot is host-managed and cannot be set through update_vm")
        if removed:
            raise TypeError("update_vm() got an unexpected keyword argument %r" % next(iter(removed)))
        body = self._clean({
            "name": name,
            "ttl_s": ttl_s,
            "extend_s": extend_s,
            "labels": None if labels is None else dict(labels),
            "idle_stop_s": idle_stop_s,
        })
        if not body:
            raise ValueError(
                "update_vm requires at least one of: " + ", ".join(_PATCH_VM_FIELDS)
            )
        return self._vm(self._call("PATCH", f"/v1/vms/{_q(vm_id)}", body) or {"id": vm_id})

    update = update_vm
    patch_vm = update_vm

    def vm_usage(self, vm_id: str,
                 since: Union[str, int, datetime.datetime, None] = None,
                 until: Union[str, int, datetime.datetime, None] = None) -> Dict[str, Any]:
        """Exact per-VM usage for a window. since/until accept RFC3339 strings, unix seconds (int), or datetime."""
        def _bound(v: Any) -> Optional[str]:
            if v is None:
                return None
            if isinstance(v, bool):
                raise TypeError("since/until cannot be bool")
            if isinstance(v, int):
                return str(v)
            if isinstance(v, datetime.datetime):
                return self._usage_bound(v)
            return str(v)
        q = urllib.parse.urlencode(self._clean({"since": _bound(since), "until": _bound(until)}))
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/usage" + ("?" + q if q else ""))

    def start(self, vm_id: str, fail_fast: bool = False,
              env: Optional[Mapping[str, str]] = None,
              no_env: Optional[bool] = None, environment: Optional[str] = None,
              ttl_s: Optional[int] = None, allow_env_error: bool = False) -> VM:
        """allow_env_error: by default env_error in the response (environment not delivered) raises BoatError("env_failed")."""
        if environment and no_env:
            raise ValueError("environment and no_env are mutually exclusive")
        body = self._clean({
            "fail_fast": True if fail_fast else None,
            "env": None if env is None else dict(env),
            "no_env": no_env,
            "environment": environment,
            "ttl_s": ttl_s,
        })
        try:
            out = self._call("POST", f"/v1/vms/{_q(vm_id)}/start", body or None) or {"id": vm_id}
        except BoatError as e:
            # legacy servers: bare 502 env_update_failed (VM is up); with allow_env_error return the VM if the error carries it
            if allow_env_error and e.code == "env_update_failed" and e.vm:
                return self._vm(dict(e.vm, env_error=e.vm.get("env_error") or e.message))
            raise
        if not allow_env_error:
            self._check_env([out])
        return self._vm(out)

    def stop(self, vm_id: str, force: bool = False) -> VM:
        """force: kill without a snapshot; the next start is a cold boot and loses unsynced writes."""
        body = {"force": True} if force else None
        return self._vm(self._call("POST", f"/v1/vms/{_q(vm_id)}/stop", body) or {"id": vm_id})

    def fork(self, vm_id: str, name: Optional[str] = None, fail_fast: bool = False,
             env: Optional[Mapping[str, str]] = None,
             labels: Optional[Mapping[str, str]] = None,
             no_env: Optional[bool] = None, environment: Optional[str] = None,
             ttl_s: Optional[int] = None, idempotency_key: Optional[str] = None,
             allow_env_error: bool = False) -> VM:
        """allow_env_error: by default env_error in the response raises BoatError("env_failed") (the fork stays up)."""
        if environment and no_env:
            raise ValueError("environment and no_env are mutually exclusive")
        body = self._clean({
            "name": name,
            "fail_fast": True if fail_fast else None,
            "env": None if env is None else dict(env),
            "labels": None if labels is None else dict(labels),
            "no_env": no_env,
            "environment": environment,
            "ttl_s": ttl_s,
        })
        out = self._call("POST", f"/v1/vms/{_q(vm_id)}/fork", body, idempotency_key=idempotency_key)
        if not allow_env_error:
            self._check_env([out])
        return self._vm(out)

    def clone_vm(self, vm_id: str, name: Optional[str] = None, fail_fast: bool = False,
                 env: Optional[Mapping[str, str]] = None,
                 labels: Optional[Mapping[str, str]] = None,
                 no_env: Optional[bool] = None, environment: Optional[str] = None,
                 ttl_s: Optional[int] = None, allow_env_error: bool = False) -> VM:
        """allow_env_error: by default env_error in the response raises BoatError("env_failed") (the clone stays up)."""
        if environment and no_env:
            raise ValueError("environment and no_env are mutually exclusive")
        body = self._clean({
            "name": name,
            "fail_fast": True if fail_fast else None,
            "env": None if env is None else dict(env),
            "labels": None if labels is None else dict(labels),
            "no_env": no_env,
            "environment": environment,
            "ttl_s": ttl_s,
        })
        out = self._call("POST", f"/v1/vms/{_q(vm_id)}/clone", body)
        if not allow_env_error:
            self._check_env([out])
        return self._vm(out)

    clone = clone_vm

    def delete(self, vm_id: str) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}")

    def exec(self, vm_id: str, cmd: List[str], env: Optional[Mapping[str, str]] = None, cwd: Optional[str] = None,
             timeout_s: Optional[int] = None, stdin: Union[str, bytes, None] = None,
             tty: bool = False, *, _deadline: Optional[float] = None) -> Iterator[ExecEvent]:
        """Stream a command's output. The last event carries exit_code."""
        stdin_b64: Optional[str] = None
        if stdin is not None:
            raw_stdin = stdin.encode("utf-8") if isinstance(stdin, str) else bytes(stdin)
            stdin_b64 = base64.b64encode(raw_stdin).decode("ascii")
        body = self._clean({
            "cmd": list(cmd),
            "env": None if env is None else dict(env),
            "cwd": cwd,
            "timeout_s": timeout_s,
            "stdin_b64": stdin_b64,
            "tty": True if tty else None,
        })
        return self._stream(f"/v1/vms/{_q(vm_id)}/exec", body, _deadline=_deadline)

    def run(self, vm_id: str, cmd: List[str], **kw: Any) -> ExecResult:
        """exec() collected into stdout/stderr/exit_code."""
        return _collect(self.exec(vm_id, cmd, **kw))

    def push(self, vm_id: str, directory: str, to: Optional[str] = None, *,
             _deadline: Optional[float] = None) -> Dict[str, Any]:
        """Copy a local directory to `to` on the VM (default ~/<dir name>; relative = from home).
        Same files as `boat push`. One archive is capped by the API's -max-file-bytes (256 MiB by default)."""
        folder = os.path.abspath(directory)
        if not os.path.isdir(folder):
            raise ValueError(folder + " is not a directory")
        files = _push_files(folder)
        tmp = "/tmp/.boat-push-" + _secrets.token_hex(8) + ".tgz"
        with tempfile.TemporaryFile() as packed:
            with tarfile.open(fileobj=packed, mode="w:gz") as tar:
                for name in files:
                    tar.add(os.path.join(folder, name), arcname=name, recursive=False)
            packed.seek(0)
            if _deadline is None:
                self.write_file(vm_id, tmp, packed.read())
            else:
                rem = _deadline - time.monotonic()
                if rem <= 0:
                    raise socket.timeout("timed out")
                self.write_file(vm_id, tmp, packed.read(), _timeout=rem)
        unpack_kw: Dict[str, Any] = {}
        if _deadline is not None:
            rem = _deadline - time.monotonic()
            if rem <= 0:
                raise socket.timeout("timed out")
            unpack_kw = {"timeout_s": max(1, int(math.ceil(rem))), "_deadline": _deadline}
        res = self.run(vm_id, ["bash", "-c", _UNPACK, to or os.path.basename(folder), tmp], **unpack_kw)
        if _deadline is not None and res.exit_code == 124:
            raise socket.timeout("timed out")
        if res.exit_code != 0:
            raise BoatError(0, "push_failed", "unpack on the VM: " + res.stderr.decode(errors="replace").strip())
        return {"path": res.stdout.decode().strip(), "files": len(files)}

    def run_job(self, cmd: List[str], push: Optional[str] = None, cwd: Optional[str] = None,
                timeout_s: Optional[float] = None, keep: bool = False, **create: Any) -> JobResult:
        """`boat run`: new VM, optional push, run cmd, delete. create: create_vm() arguments.
        Creates the VM with a 15-minute TTL (renewed every 5 minutes while running): if this process dies,
        the VM stops by itself within 15 minutes. A timeout gives exit_code 124."""
        if push is not None and not os.path.isdir(push):  # before a VM exists
            raise ValueError(push + " is not a directory")
        if timeout_s is not None and timeout_s <= 0:
            raise BoatTimeoutError("timeout_s must be > 0")
        create.setdefault("idle_stop_s", _JOB_IDLE_STOP_S)
        create.setdefault("ttl_s", _JOB_TTL_S)
        eff_ttl = create["ttl_s"]
        create["allow_env_error"] = True  # checked below, after which the VM is cleaned up
        vm = self.create_vm(**create)
        start = time.monotonic()
        deadline = start + timeout_s if timeout_s is not None else None
        deleted = False
        exc_active = False
        stop_beat = threading.Event()
        beat_thread: Optional[threading.Thread] = None
        if isinstance(eff_ttl, int) and not isinstance(eff_ttl, bool) and eff_ttl > 0 and _JOB_HEARTBEAT_S > 0:
            def _heartbeat() -> None:
                while not stop_beat.wait(_JOB_HEARTBEAT_S):
                    try:
                        self._call("PATCH", f"/v1/vms/{_q(vm.id)}", {"ttl_s": eff_ttl},
                                   timeout=min(self.timeout, 15.0))
                    except Exception:
                        pass

            beat_thread = threading.Thread(target=_heartbeat, name=f"boat-ttl-{vm.id}", daemon=True)
            beat_thread.start()
        try:
            vm_raw = getattr(vm, "raw", None)
            raw = vm_raw if isinstance(vm_raw, dict) else {}
            setup_error = raw.get("setup_error")
            if isinstance(setup_error, str):
                setup_error = setup_error.strip()
            env_error = raw.get("env_error")
            if isinstance(env_error, str):
                env_error = env_error.strip()
            if env_error:
                raise BoatError(0, "env_failed", f"environment delivery failed: {env_error}")
            if setup_error:
                raise BoatError(0, "setup_failed", f"environment setup failed: {setup_error}")
            setup_cmd_id = raw.get("setup_cmd_id")
            if isinstance(setup_cmd_id, str):
                setup_cmd_id = setup_cmd_id.strip()
            if setup_cmd_id:
                self._wait_setup(vm.id, str(setup_cmd_id), deadline)
            res: Optional[ExecResult] = None
            where = cwd
            if push is not None:  # always upload; cwd only sets the working dir (as `boat run`)
                if deadline is not None and deadline - time.monotonic() <= 0:
                    res = ExecResult(b"", b"", 124)
                else:
                    try:
                        pushed = self.push(vm.id, push, _deadline=deadline)["path"]
                        if where is None:
                            where = pushed
                    except BaseException as exc:
                        if deadline is not None and _is_timeout(exc):
                            res = ExecResult(b"", b"", 124)
                        else:
                            raise
            if res is None:
                argv = list(cmd)
                if where and not where.startswith("/"):  # relative to home, which only the VM knows
                    argv = ["bash", "-c", 'cd -- "$HOME/$0" && exec "$@"', where] + argv
                    where = None
                if deadline is None:
                    exec_timeout = _NO_LIMIT_S
                else:
                    rem = deadline - time.monotonic()
                    if rem <= 0:
                        res = ExecResult(b"", b"", 124)
                    else:
                        exec_timeout = max(1, int(math.ceil(rem)))
                if res is None:
                    try:
                        res = self.run(vm.id, argv, cwd=where, timeout_s=exec_timeout, _deadline=deadline)
                    except BaseException as exc:
                        if deadline is not None and _is_timeout(exc):
                            res = ExecResult(b"", b"", 124)
                        else:
                            raise
        except BaseException:
            exc_active = True
            raise
        finally:
            stop_beat.set()
            if beat_thread is not None:
                beat_thread.join(timeout=5.0)
            if not keep:
                if exc_active:
                    try:
                        self.delete(vm.id)
                        deleted = True
                    except Exception:
                        pass
                else:
                    self.delete(vm.id)
                    deleted = True
        return JobResult(res.stdout, res.stderr, res.exit_code, time.monotonic() - start, vm.size, vm.id, deleted)

    def run_jobs(self, jobs: Iterable[Union[Mapping[str, Any], Sequence[str]]], *,
                 concurrency: int = 16) -> List[JobResult]:
        """Run an iterable of job specs across isolated VMs in parallel and return results in input order."""
        if isinstance(concurrency, bool) or not isinstance(concurrency, int) or concurrency < 1:
            raise ValueError("concurrency must be >= 1")
        parsed: List[Tuple[List[str], Dict[str, Any]]] = []
        for spec in jobs:
            if isinstance(spec, Mapping):
                kw = dict(spec)
                if "cmd" not in kw:
                    raise ValueError("each job mapping must include 'cmd'")
                raw_cmd = kw.pop("cmd")
                if isinstance(raw_cmd, (str, bytes)) or not isinstance(raw_cmd, Sequence) or not raw_cmd:
                    raise ValueError("job 'cmd' must be a non-empty sequence of arguments")
                cmd = [str(x) for x in raw_cmd]
                push = kw.get("push")
                if push is not None and not os.path.isdir(push):
                    raise ValueError(str(push) + " is not a directory")
                timeout_s = kw.get("timeout_s")
                if timeout_s is not None and timeout_s <= 0:
                    raise BoatTimeoutError("timeout_s must be > 0")
                parsed.append((cmd, kw))
            elif not isinstance(spec, (str, bytes)) and isinstance(spec, Sequence) and spec:
                parsed.append(([str(x) for x in spec], {}))
            else:
                raise ValueError("each job must be a command sequence or a mapping with 'cmd'")
        if not parsed:
            return []

        results: List[Optional[JobResult]] = [None] * len(parsed)
        first_err: List[BaseException] = []
        err_lock = threading.Lock()
        aborted = threading.Event()

        def worker(index: int, cmd: List[str], kw: Dict[str, Any]) -> None:
            if aborted.is_set():
                return
            try:
                results[index] = self.run_job(cmd, **kw)
            except BaseException as exc:
                aborted.set()
                with err_lock:
                    if not first_err:
                        first_err.append(exc)

        with ThreadPoolExecutor(max_workers=min(concurrency, len(parsed))) as pool:
            futures = [pool.submit(worker, i, cmd, kw) for i, (cmd, kw) in enumerate(parsed)]
            for fut in futures:
                fut.result()
        if first_err:
            raise first_err[0]
        return [r for r in results if r is not None]

    def _wait_setup(self, vm_id: str, cmd_id: str, deadline: Optional[float]) -> None:
        out: List[bytes] = []
        err: List[bytes] = []
        exit_code: Optional[int] = None
        signal: Optional[Any] = None
        offset = 0
        backoff = 0.05
        while True:
            progressed = False
            try:
                if deadline is None:
                    events = self.follow_command(vm_id, cmd_id, offset=offset)
                else:
                    rem = deadline - time.monotonic()
                    if rem <= 0:
                        raise socket.timeout("timed out")
                    events = self.follow_command(vm_id, cmd_id, offset=offset, timeout=rem)
                for ev in events:
                    if ev.data or ev.exit_code is not None or ev.error:
                        progressed = True
                    if ev.next_offset is not None and ev.next_offset >= 0:
                        if ev.next_offset > offset:
                            progressed = True
                        offset = ev.next_offset
                    if ev.stream == "stderr":
                        err.append(ev.data)
                    elif ev.data:
                        out.append(ev.data)
                    if ev.exit_code is not None:
                        exit_code = ev.exit_code
                    if ev.error:
                        signal = ev.error
                break
            except BoatError as exc:
                if exc.code != "reconnect":
                    raise
                if exc.next_offset is not None and exc.next_offset >= 0:
                    if exc.next_offset > offset:
                        progressed = True
                    offset = exc.next_offset
                if progressed:
                    backoff = 0.05
                if deadline is not None:
                    rem = deadline - time.monotonic()
                    if rem <= 0:
                        detail = _tail_text(err, out)
                        raise BoatError(0, "setup_failed",
                                        "environment setup timed out" + (f": {detail}" if detail else "")) from None
                    sleep_s = min(backoff, rem)
                else:
                    sleep_s = backoff
                time.sleep(sleep_s)
                if deadline is not None and deadline - time.monotonic() <= 0:
                    detail = _tail_text(err, out)
                    raise BoatError(0, "setup_failed",
                                    "environment setup timed out" + (f": {detail}" if detail else "")) from None
                if not progressed:
                    backoff = min(backoff * 2.0, 1.0)
            except BaseException as exc:
                if _is_timeout(exc):
                    detail = _tail_text(err, out)
                    raise BoatError(0, "setup_failed",
                                    "environment setup timed out" + (f": {detail}" if detail else "")) from None
                raise
        detail = _tail_text(err, out)
        suffix = f": {detail}" if detail else ""
        if signal:
            code_note = f" (exit {exit_code})" if exit_code is not None else ""
            raise BoatError(0, "setup_failed", f"environment setup terminated by signal {signal}{code_note}{suffix}")
        if exit_code is None:
            raise BoatError(0, "setup_failed", f"environment setup ended without an exit code{suffix}")
        if exit_code != 0:
            raise BoatError(0, "setup_failed", f"environment setup exited with code {exit_code}{suffix}")

    _wait_setup_cmd = _wait_setup

    def prompt(self, vm_id: str, prompt: str, agent: str = "claude", model: Optional[str] = None,
               cwd: Optional[str] = None, timeout_s: Optional[int] = None) -> Iterator[ExecEvent]:
        """Run an agent CLI headless in the VM with team secrets as env: claude, codex, opencode, pi, prime, kimi or mistral (aliases claude-code, prime-agent)."""
        body = self._clean({"prompt": prompt, "agent": agent, "model": model, "cwd": cwd, "timeout_s": timeout_s})
        return self._stream(f"/v1/vms/{_q(vm_id)}/prompt", body)

    def read_file(self, vm_id: str, path: str) -> bytes:
        with self._open("GET", f"/v1/vms/{_q(vm_id)}/files?path={_q(path)}") as r:
            return r.read()

    def write_file(self, vm_id: str, path: str, data: bytes, *, _timeout: Any = _DEFAULT_TIMEOUT) -> None:
        with self._open("PUT", f"/v1/vms/{_q(vm_id)}/files?path={_q(path)}", bytes(data),
                        "application/octet-stream", timeout=_timeout) as r:
            r.read()

    def ports(self, vm_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/ports")["ports"]

    def add_port(self, vm_id: str, port: int, public: bool = False, auth: Optional[str] = None) -> Dict[str, Any]:
        """auth="private" (default): the gateway route needs a token from port_url().
        public=True / auth="public": open route plus a raw TCP host port."""
        mode = auth or ("public" if public else "private")
        return self._call("POST", f"/v1/vms/{_q(vm_id)}/ports", {"port": port, "auth": mode})

    def host_port(self, vm_id: str, port: int, public: bool = False, ttl_s: int = 7 * 24 * 60 * 60) -> Dict[str, Any]:
        """`boat host`: expose a port (private unless public=True) and return its URL."""
        self.add_port(vm_id, port, auth="public" if public else "private")
        return self.port_url(vm_id, port, ttl_s=ttl_s)

    def port_url(self, vm_id: str, port: int, ttl_s: Optional[int] = None) -> Dict[str, Any]:
        """{url, auth, token?, expires_at?}; send token as header X-Boat-Route-Token, or open url in a browser."""
        q = f"?ttl_s={int(ttl_s)}" if ttl_s else ""
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/ports/{int(port)}/url{q}")

    def remove_port(self, vm_id: str, port: int) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/ports/{int(port)}")

    def revoke_port_tokens(self, vm_id: str) -> Dict[str, Any]:
        if not isinstance(vm_id, str) or not vm_id.strip() or vm_id.strip() in (".", ".."):
            raise ValueError("vm_id is required")
        return self._call("POST", f"/v1/vms/{_q(vm_id)}/port-tokens/revoke") or {"ok": True}

    def snapshots(self, vm_id: str) -> Any:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/snapshots")

    def create_snapshot(self, vm_id: str, name: Optional[str] = None) -> Dict[str, Any]:
        return self._call("POST", f"/v1/vms/{_q(vm_id)}/snapshots", self._clean({"name": name}))

    def delete_snapshot(self, vm_id: str, snapshot_id: str) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/snapshots/{_q(snapshot_id)}")

    def ssh(self, vm_id: str, public_key: Optional[str] = None) -> SSHInfo:
        d = self._call("POST", f"/v1/vms/{_q(vm_id)}/ssh", self._clean({"public_key": public_key}))
        return SSHInfo(host=d["host"], port=int(d["port"]), user=d["user"], jump=d.get("jump", ""))

    grant_ssh = ssh

    def ssh_keys(self, vm_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/ssh")["keys"]

    def revoke_ssh(self, vm_id: str, fingerprint: str) -> Dict[str, Any]:
        if not isinstance(vm_id, str) or not vm_id.strip() or vm_id.strip() in (".", ".."):
            raise ValueError("vm_id is required")
        if not isinstance(fingerprint, str) or not fingerprint.strip() or fingerprint.strip() in (".", ".."):
            raise ValueError("fingerprint is required")
        return self._call("DELETE", f"/v1/vms/{_q(vm_id)}/ssh/{_q(fingerprint)}") or {"ok": True}

    def desktop(self, vm_id: str) -> DesktopURL:
        d = self._call("GET", f"/v1/vms/{_q(vm_id)}/desktop")
        return DesktopURL(url=d["url"], expires_at=d["expires_at"])

    # ---- detached commands ----

    def start_command(self, vm_id: str, cmd: List[str], env: Optional[Dict[str, str]] = None,
                      cwd: Optional[str] = None) -> Command:
        """Run in the background; the command outlives this call."""
        d = self._call("POST", f"/v1/vms/{_q(vm_id)}/commands", self._clean({"cmd": list(cmd), "env": env, "cwd": cwd}))
        return Command.from_json(d)

    def list_commands(self, vm_id: str) -> List[Command]:
        return [Command.from_json(c) for c in self._call("GET", f"/v1/vms/{_q(vm_id)}/commands")["commands"]]

    def command_status(self, vm_id: str, cmd_id: str) -> Command:
        return Command.from_json(self._call("GET", f"/v1/vms/{_q(vm_id)}/commands/{_q(cmd_id)}"))

    def command_history(self, vm_id: str) -> List[Dict[str, Any]]:
        """Commands started through the API; also works when the VM is stopped."""
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/command-history")["commands"]

    def command_logs(self, vm_id: str, cmd_id: str, offset: int = 0, limit: Optional[int] = None) -> Dict[str, Any]:
        """One chunk: {data_b64, next_offset, running, exit}."""
        q = urllib.parse.urlencode(self._clean({"offset": offset, "limit": limit}))
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/commands/{_q(cmd_id)}/logs?{q}")

    def follow_command(self, vm_id: str, cmd_id: str, offset: int = 0,
                       timeout: Optional[float] = None) -> Iterator[ExecEvent]:
        """Stream until exit; the last event carries exit_code.

        Resume explicitly with offset=event.next_offset or, on BoatError with
        code='reconnect', offset=error.next_offset when that cursor is not None.
        Cursors are absolute byte offsets, including any retained-log gap.
        No retry is automatic, and missing cursors remain None.
        """
        return self._follow(f"/v1/vms/{_q(vm_id)}/commands/{_q(cmd_id)}/logs", offset, timeout=timeout)

    def _follow(self, path: str, offset: int, timeout: Optional[float] = None) -> Iterator[ExecEvent]:
        deadline = None if timeout is None else time.monotonic() + timeout
        r = self._open("GET", f"{path}?follow=1&offset={int(offset)}", timeout=timeout)
        with r:
            for line in _iter_lines(r, deadline):
                if not line.strip():
                    continue
                ev = json.loads(line)
                if ev.get("reconnect"):
                    raise BoatError(409, "reconnect", "the VM stopped answering; resume from next_offset",
                                    next_offset=ev.get("next_offset"))
                yield ExecEvent(stream=ev.get("stream", ""),
                                data=base64.b64decode(ev["data_b64"]) if ev.get("data_b64") else b"",
                                exit_code=ev.get("exit"), error=ev.get("signal"),
                                next_offset=ev.get("next_offset"))

    def signal_command(self, vm_id: str, cmd_id: str, signal: str = "TERM") -> None:
        self._call("POST", f"/v1/vms/{_q(vm_id)}/commands/{_q(cmd_id)}/signal", {"signal": signal})

    def stop_command(self, vm_id: str, cmd_id: str) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/commands/{_q(cmd_id)}")

    def processes(self, vm_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/processes")["processes"]

    # ---- file operations ----

    def list_files(self, vm_id: str, path: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/files/list?path={_q(path)}")["entries"]

    def delete_file(self, vm_id: str, path: str, recursive: bool = False) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/files?path={_q(path)}" + ("&recursive=1" if recursive else ""))

    def move_file(self, vm_id: str, src: str, dst: str, overwrite: bool = False) -> None:
        self._call("POST", f"/v1/vms/{_q(vm_id)}/files/move", {"from": src, "to": dst, "overwrite": overwrite})

    def search_files(self, vm_id: str, path: str, query: str, content: bool = False, regex: bool = False,
                     limit: Optional[int] = None) -> List[Dict[str, Any]]:
        """File names by default; content=True searches inside files."""
        q = self._clean({"path": path, "q": query, "content": "1" if content else None,
                         "regex": "1" if regex else None, "limit": limit})
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/files/search?" + urllib.parse.urlencode(q))["matches"]

    def file_info(self, vm_id: str, path: str) -> Dict[str, Any]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/files/info?path={_q(path)}")

    def make_folder(self, vm_id: str, path: str) -> None:
        self._call("POST", f"/v1/vms/{_q(vm_id)}/files/folder", {"path": path})

    def set_file_mode(self, vm_id: str, path: str, mode: str) -> None:
        self._call("POST", f"/v1/vms/{_q(vm_id)}/files/permissions", {"path": path, "mode": mode})

    # ---- async prompts and conversations ----

    def start_prompt(self, vm_id: str, prompt: str, agent: Optional[str] = None, model: Optional[str] = None,
                     cwd: Optional[str] = None, conversation_id: Optional[str] = None,
                     new: Optional[bool] = None) -> PromptRun:
        """Start a turn in the background. Continues the latest conversation unless new or conversation_id."""
        body = self._clean({"prompt": prompt, "agent": agent, "model": model, "cwd": cwd,
                            "conversation_id": conversation_id, "new": new})
        return PromptRun.from_json(self._call("POST", f"/v1/vms/{_q(vm_id)}/prompts", body))

    def list_prompts(self, vm_id: str) -> List[PromptRun]:
        return [PromptRun.from_json(p) for p in self._call("GET", f"/v1/vms/{_q(vm_id)}/prompts")["prompts"]]

    def prompt_status(self, vm_id: str, prompt_id: str) -> PromptRun:
        return PromptRun.from_json(self._call("GET", f"/v1/vms/{_q(vm_id)}/prompts/{_q(prompt_id)}"))

    def follow_prompt(self, vm_id: str, prompt_id: str, offset: int = 0,
                      timeout: Optional[float] = None) -> Iterator[ExecEvent]:
        """Stream a prompt with the same explicit cursor resume contract as follow_command.

        Use a non-None event.next_offset or reconnect BoatError.next_offset as
        offset to resume at the server's absolute byte position. No automatic retry.
        """
        return self._follow(f"/v1/vms/{_q(vm_id)}/prompts/{_q(prompt_id)}/logs", offset, timeout=timeout)

    def steer_prompt(self, vm_id: str, prompt_id: str, prompt: str) -> PromptRun:
        """Interrupt the turn and resume its session with this message. Returns the new turn."""
        d = self._call("POST", f"/v1/vms/{_q(vm_id)}/prompts/{_q(prompt_id)}/steer", {"prompt": prompt})
        return PromptRun.from_json(d)

    def interrupt_prompt(self, vm_id: str, prompt_id: str) -> PromptRun:
        return PromptRun.from_json(self._call("POST", f"/v1/vms/{_q(vm_id)}/prompts/{_q(prompt_id)}/interrupt"))

    def conversations(self, vm_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/conversations")["conversations"]

    # ---- environments ----

    def environments(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/environments")["environments"]

    def create_environment(self, name: str, image: Optional[str] = None, size: Optional[str] = None,
                           setup_script: Optional[str] = None, env: Optional[Dict[str, str]] = None,
                           secrets: Optional[List[str]] = None, is_default: Optional[bool] = None) -> Dict[str, Any]:
        return self._call("POST", "/v1/environments", self._clean({
            "name": name, "image": image, "size": size, "setup_script": setup_script, "env": env,
            "secrets": secrets, "is_default": is_default}))

    def get_environment(self, ref: str) -> Dict[str, Any]:
        """By id or name."""
        return self._call("GET", f"/v1/environments/{_q(ref)}")

    def update_environment(self, ref: str, reset_secrets: Optional[bool] = None, **fields: Any) -> Dict[str, Any]:
        """Update an environment by id or name (reset_secrets: bool | None = None clears stored secrets when True)."""
        body = dict(fields)
        if reset_secrets is not None:
            body["reset_secrets"] = reset_secrets
        return self._call("PUT", f"/v1/environments/{_q(ref)}", body)

    def delete_environment(self, ref: str) -> None:
        self._call("DELETE", f"/v1/environments/{_q(ref)}")

    def build_environment(self, ref: str) -> Dict[str, Any]:
        """Run the setup once and snapshot it. Poll get_environment(ref)["build"]["state"]."""
        return self._call("POST", f"/v1/environments/{_q(ref)}/build")

    def upgrade_environment(self, ref: str, vm_ids: Optional[Iterable[str]] = None, *,
                            agent_ids: Optional[Iterable[str]] = None) -> Dict[str, Any]:
        """Re-pin sandboxes on this environment to its latest version.
        Pass vm_ids (or agent_ids) to target specific sandboxes; omit to upgrade all on the environment."""
        if vm_ids is not None and agent_ids is not None:
            raise ValueError("pass only one of vm_ids or agent_ids")
        targets = agent_ids if agent_ids is not None else vm_ids
        if targets is None:
            return self._call("POST", f"/v1/environments/{_q(ref)}/upgrade")
        if isinstance(targets, (str, bytes)):
            raise TypeError("vm_ids/agent_ids must be an iterable of IDs, not a string")
        return self._call("POST", f"/v1/environments/{_q(ref)}/upgrade", {"agentIds": list(targets)})

    # ---- resize, snapshots beyond the VM ----

    def resize(self, vm_id: str, size: Optional[str] = None, disk_gib: Optional[int] = None,
               discard_memory: Optional[bool] = None) -> VM:
        body = self._clean({"size": size, "disk_gib": disk_gib, "discard_memory": discard_memory})
        return self._vm(self._call("POST", f"/v1/vms/{_q(vm_id)}/resize", body))

    def team_snapshots(self, visibility: Optional[str] = None) -> List[Dict[str, Any]]:
        """List team snapshots; visibility may be 'shared' or 'all'."""
        q = urllib.parse.urlencode(self._clean({"visibility": visibility}))
        return self._call("GET", "/v1/snapshots" + ("?" + q if q else ""))["snapshots"]

    def delete_team_snapshot(self, snapshot_id: str) -> None:
        self._call("DELETE", f"/v1/snapshots/{_q(snapshot_id)}")

    def download_snapshot(self, vm_id: str, snapshot_id: str, dest) -> int:
        """Write the archive to dest (a path or binary file). Returns the bytes written."""
        r = self._open("GET", f"/v1/vms/{_q(vm_id)}/snapshots/{_q(snapshot_id)}/download", timeout=None)
        with r:
            if hasattr(dest, "write"):
                return _copy(r, dest)
            with open(dest, "wb") as f:
                return _copy(r, f)

    def import_snapshot(self, data, name: Optional[str] = None, image: Optional[str] = None,
                        size: Optional[str] = None) -> Dict[str, Any]:
        """data: bytes, or a binary file (streamed). image/size apply to a disk-only import."""
        q = urllib.parse.urlencode(self._clean({"name": name, "image": image, "size": size}))
        with self._open("POST", "/v1/snapshots/import?" + q, data, "application/octet-stream", timeout=None) as r:
            return json.loads(r.read())

    def share_snapshot(self, vm_id: str, snapshot_id: str, team_id: Optional[str] = None,
                       ttl_s: Optional[int] = None) -> Dict[str, Any]:
        """team_id grants that team read access; otherwise a signed link: result["code"] is shown once."""
        body = self._clean({"team_id": team_id, "ttl_s": ttl_s})
        return self._call("POST", f"/v1/vms/{_q(vm_id)}/snapshots/{_q(snapshot_id)}/shares", body)

    def snapshot_shares(self, vm_id: str, snapshot_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/snapshots/{_q(snapshot_id)}/shares")["shares"]

    def revoke_snapshot_share(self, vm_id: str, snapshot_id: str, share_id: str) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/snapshots/{_q(snapshot_id)}/shares/{_q(share_id)}")

    def shared_snapshots(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/shared-snapshots")["shares"]

    def take_snapshot_share(self, share_id: Optional[str] = None, code: Optional[str] = None,
                            name: Optional[str] = None) -> Dict[str, Any]:
        """Copy a shared snapshot into your team."""
        body = self._clean({"share_id": share_id, "code": code, "name": name})
        return self._call("POST", "/v1/snapshots/import-share", body)

    # ---- AI credentials proxy ----

    def ai_credentials(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/ai/credentials")["credentials"]

    def set_ai_credential(self, provider: str, value: str) -> Dict[str, Any]:
        """The key stays on the host. VMs get a per-VM token through the proxy."""
        return self._call("PUT", f"/v1/ai/credentials/{_q(provider)}", {"value": value})

    def delete_ai_credential(self, provider: str) -> None:
        self._call("DELETE", f"/v1/ai/credentials/{_q(provider)}")

    def ai_proxy(self) -> Dict[str, Any]:
        return self._call("GET", "/v1/ai/proxy")

    def set_ai_proxy(self, enabled: bool) -> Dict[str, Any]:
        return self._call("PUT", "/v1/ai/proxy", {"enabled": enabled})

    def ai_usage(self, vm_id: Optional[str] = None) -> Dict[str, Any]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/ai-usage" if vm_id else "/v1/ai/usage")

    def set_ai_access(self, vm_id: str, enabled: bool) -> None:
        self._call("PUT", f"/v1/vms/{_q(vm_id)}/ai-access", {"enabled": enabled})

    # ---- telemetry ----

    def telemetry(self) -> Dict[str, Any]:
        return self._call("GET", "/v1/telemetry")

    def set_telemetry(self, endpoint: str, headers: Optional[Dict[str, str]] = None) -> Dict[str, Any]:
        return self._call("PUT", "/v1/telemetry", self._clean({"endpoint": endpoint, "headers": headers}))

    def delete_telemetry(self) -> None:
        self._call("DELETE", "/v1/telemetry")

    # ---- teams, keys, secrets, webhooks ----

    def teams(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/teams")["teams"]

    def get_team(self, team_id: str) -> Dict[str, Any]:
        return self._call("GET", f"/v1/teams/{_q(team_id)}")

    def create_team(self, name: str, owner: Optional[str] = None) -> Dict[str, Any]:
        """Operator only. With owner, a new user owns the team; result["owner"]["key"]["token"]."""
        return self._call("POST", "/v1/teams", self._clean({"name": name, "owner": owner}))

    def delete_team(self, team_id: str) -> None:
        """Operator only; the team must have no VMs."""
        self._call("DELETE", f"/v1/teams/{_q(team_id)}")

    def add_member(self, team_id: str, name: str, role: str = "member") -> Dict[str, Any]:
        return self._call("POST", f"/v1/teams/{_q(team_id)}/members", {"name": name, "role": role})

    def remove_member(self, team_id: str, user_id: str) -> Dict[str, Any]:
        return self._call("DELETE", f"/v1/teams/{_q(team_id)}/members/{_q(user_id)}") or {"ok": True}

    def set_self_token_policy(self, team_id: str, public_ports: bool = False,
                              ssh: bool = False) -> Dict[str, Any]:
        return self._call("PUT", f"/v1/teams/{_q(team_id)}/self-token",
                          {"public_ports": bool(public_ports), "ssh": bool(ssh)})

    def set_share_allowlist(self, team_id: str, teams: Iterable[str]) -> Dict[str, Any]:
        return self._call("PUT", f"/v1/teams/{_q(team_id)}/share-allowlist",
                          {"teams": list(teams)})

    def iter_events(self, after: Optional[int] = None, *,
                    timeout: Optional[float] = None) -> Iterator[Dict[str, Any]]:
        """Stream team lifecycle SSE events from GET /v1/events."""
        path = "/v1/events" + (f"?after={int(after)}" if after is not None else "")
        deadline = None if timeout is None else time.monotonic() + timeout
        r = self._open("GET", path, timeout=timeout)
        with r:
            ev_id = ""
            ev_type = ""
            data_lines: List[str] = []
            for raw_line in _iter_lines(r, deadline):
                line = raw_line.decode("utf-8", "replace").rstrip("\r\n")
                if not line:
                    if data_lines:
                        yield _decode_sse_event(ev_id, ev_type, "\n".join(data_lines))
                    ev_type = ""
                    data_lines = []
                    continue
                if line.startswith(":"):
                    continue
                field_name, sep, val = line.partition(":")
                if sep and val.startswith(" "):
                    val = val[1:]
                if field_name == "id":
                    ev_id = val
                elif field_name == "event":
                    ev_type = val
                elif field_name == "data":
                    data_lines.append(val)
            if data_lines:
                yield _decode_sse_event(ev_id, ev_type, "\n".join(data_lines))

    def keys(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/keys")["keys"]

    def create_key(self, name: str = "key", scopes: Optional[List[str]] = None,
                   team_id: Optional[str] = None, expires_in_s: Optional[int] = None) -> Dict[str, Any]:
        """Mint an API key. expires_in_s sets the key lifetime in seconds (omit = never); the returned dict includes `expires` when set."""
        return self._call("POST", "/v1/keys", self._clean({
            "name": name, "scopes": scopes, "team_id": team_id, "expires_in_s": expires_in_s,
        }))

    def rotate_key(self, key_id: str, overlap_s: Optional[int] = None) -> Dict[str, Any]:
        """New secret in result["token"]; the old one works for overlap_s more (default 24h, 0 = now)."""
        return self._call("POST", f"/v1/keys/{_q(key_id)}/rotate", self._clean({"overlap_s": overlap_s}))

    def key_usage(self, key_id: str) -> Dict[str, Any]:
        return self._call("GET", f"/v1/keys/{_q(key_id)}/usage")

    def delete_key(self, key_id: str) -> None:
        self._call("DELETE", f"/v1/keys/{_q(key_id)}")

    def secrets(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/secrets")["secrets"]

    def set_secret(self, name: str, value: str, inject: Optional[bool] = None) -> None:
        """Set a team secret (admin). inject=True marks it default-injected: VMs
        created without a secrets selection get it. None keeps the current mark
        (a new secret is not injected)."""
        body: Dict[str, Any] = {"value": value}
        if inject is not None:
            body["inject"] = inject
        self._call("PUT", f"/v1/secrets/{_q(name)}", body)

    def delete_secret(self, name: str) -> None:
        self._call("DELETE", f"/v1/secrets/{_q(name)}")

    def webhooks(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/webhooks")["webhooks"]

    def create_webhook(self, url: str, events: Optional[List[str]] = None) -> Dict[str, Any]:
        """Returns the webhook including its signing `secret` (shown once)."""
        return self._call("POST", "/v1/webhooks", self._clean({"url": url, "events": events}))

    def get_webhook(self, webhook_id: str) -> Dict[str, Any]:
        return self._call("GET", f"/v1/webhooks/{_q(webhook_id)}")

    def update_webhook(self, webhook_id: str, url: Optional[str] = None, events: Optional[List[str]] = None) -> Dict[str, Any]:
        return self._call("PATCH", f"/v1/webhooks/{_q(webhook_id)}", self._clean({"url": url, "events": events}))

    def rotate_webhook(self, webhook_id: str, overlap_s: Optional[int] = None) -> Dict[str, Any]:
        """New secret in result["secret"]; deliveries carry both signatures during the overlap."""
        return self._call("POST", f"/v1/webhooks/{_q(webhook_id)}/rotate", self._clean({"overlap_s": overlap_s}))

    def delete_webhook(self, webhook_id: str) -> None:
        self._call("DELETE", f"/v1/webhooks/{_q(webhook_id)}")

    def deliveries(self, webhook_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/webhooks/{_q(webhook_id)}/deliveries")["deliveries"]

    def test_webhook(self, webhook_id: str) -> str:
        return self._call("POST", f"/v1/webhooks/{_q(webhook_id)}/test")["event_id"]


def _decode_sse_event(ev_id: str, ev_type: str, raw_data: str) -> Dict[str, Any]:
    try:
        parsed = json.loads(raw_data)
    except ValueError:
        parsed = None
    if isinstance(parsed, dict):
        out = dict(parsed)
        if ev_id and "id" not in out:
            out["id"] = ev_id
        if ev_type and "event" not in out:
            out["event"] = ev_type
        return out
    res: Dict[str, Any] = {"data": raw_data if parsed is None else parsed}
    if ev_id:
        res["id"] = ev_id
    if ev_type:
        res["event"] = ev_type
    return res


def _response_socket(r: Any) -> Any:
    fp = getattr(r, "fp", None)
    for candidate in (
        getattr(getattr(fp, "raw", None), "_sock", None),
        getattr(getattr(getattr(fp, "fp", None), "raw", None), "_sock", None),
        getattr(r, "_sock", None),
    ):
        if candidate is not None:
            return candidate
    return None


def _set_sock_timeout(r: Any, sock: Any, value: Optional[float]) -> bool:
    if sock is None:
        return False
    isclosed = getattr(r, "isclosed", None)
    if callable(isclosed) and isclosed():
        return False
    fileno = getattr(sock, "fileno", None)
    if callable(fileno):
        try:
            if fileno() < 0:
                return False
        except OSError as exc:
            if exc.errno == errno.EBADF:
                return False
            raise
    try:
        sock.settimeout(value)
        return True
    except OSError as exc:
        if exc.errno == errno.EBADF:
            return False
        raise


def _iter_lines(r: Any, deadline: Optional[float]) -> Iterator[bytes]:
    if deadline is None or not hasattr(r, "readline"):
        yield from r
        return
    sock = _response_socket(r)
    prev: Optional[float] = None
    has_prev = False
    if sock is not None and hasattr(sock, "gettimeout"):
        isclosed = getattr(r, "isclosed", None)
        if not (callable(isclosed) and isclosed()):
            try:
                fileno = getattr(sock, "fileno", None)
                if not callable(fileno) or fileno() >= 0:
                    prev = sock.gettimeout()
                    has_prev = True
            except OSError as exc:
                if exc.errno != errno.EBADF:
                    raise
    try:
        while True:
            rem = deadline - time.monotonic()
            if rem <= 0:
                raise socket.timeout("timed out")
            _set_sock_timeout(r, sock, rem)
            line = r.readline()
            if not line:
                return
            yield line
    finally:
        if has_prev:
            _set_sock_timeout(r, sock, prev)


def _is_timeout(exc: BaseException) -> bool:
    if isinstance(exc, (TimeoutError, socket.timeout)):
        return True
    if isinstance(exc, urllib.error.URLError) and isinstance(exc.reason, (TimeoutError, socket.timeout)):
        return True
    return False


def _tail_text(err: List[bytes], out: List[bytes]) -> str:
    raw = b"".join(err).strip() or b"".join(out).strip()
    if not raw:
        return ""
    text = raw[-512:].decode("utf-8", "replace").strip()
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    return lines[-1][:200] if lines else ""


_EXEC_STREAM_STATUS = {
    "not_found": 400,
    "invalid_user": 400,
    "spawn_failed": 400,
    "invalid_argument": 400,
    "bad_request": 400,
    "invalid_request": 400,
    "permission_denied": 403,
    "forbidden": 403,
    "conflict": 409,
    "unsupported": 422,
    "not_supported": 422,
}


def _stream_error(ev: ExecEvent) -> BoatError:
    raw_err = ev.error
    code = ev.code or ""
    if isinstance(raw_err, dict):
        if not code:
            code = str(raw_err.get("code") or "")
        message = str(raw_err.get("message") or raw_err.get("error") or raw_err)
    else:
        message = str(raw_err)
    code = code.strip() or "exec_error"
    status = _EXEC_STREAM_STATUS.get(code, 502)
    return BoatError(status, code, message or code)


def _collect(events: Iterator[ExecEvent]) -> ExecResult:
    out, err = [], []
    code: Optional[int] = None
    for ev in events:
        if ev.error:
            raise _stream_error(ev)
        if ev.stream == "stderr":
            err.append(ev.data)
        elif ev.data:
            out.append(ev.data)
        if ev.exit_code is not None:
            code = ev.exit_code
    if code is None:
        raise BoatError(502, "stream_ended", "exec stream ended without exit status")
    return ExecResult(b"".join(out), b"".join(err), code)


def verify_webhook(secret: str, signature_header: str, body: bytes, tolerance_s: int = 300,
                   now: Optional[float] = None) -> bool:
    """Check X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, f"{t}." + body)).

    During a secret rotation the header has one v1= per secret; any match passes,
    so a receiver holding either secret keeps working.
    """
    ts = None
    sigs = []
    for part in signature_header.split(","):
        key, _, value = part.partition("=")
        if key == "t":
            try:
                ts = int(value)
            except ValueError:
                return False
        elif key == "v1":
            sigs.append(value)
    if ts is None or not sigs:
        return False
    if abs((time.time() if now is None else now) - ts) > tolerance_s:
        return False
    want = hmac.new(secret.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    return any(hmac.compare_digest(want, sig) for sig in sigs)
