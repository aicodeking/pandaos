"""Thin provider adapters: PandaOS boat-api and boat.dev's public API v1.

Both speak plain HTTP/1.1 keep-alive through stdlib http.client (no SDK, no CLI)
so the client-side cost of a call is the same code on both sides and WAN TLS
connections are pooled across requests. Each adapter only maps the common
verbs below onto its provider's documented endpoints; timing, polling for
exec-readiness and all workloads live in boatbench.py so they are shared.

  create(name) -> id          stop(id)              snapshot(id, name) -> ref
  exec(id, cmd, timeout)      resume(id)            from_snapshot(ref, name) -> id
  run_true(id)                fork(id, name) -> id  delete(id) / delete_snapshot(ref)
  put_file(id, path, data)    ping(id): one cheap authenticated GET (API round-trip baseline)
  close(): close pooled HTTP/HTTPS keep-alive sockets

ROUND_TRIPS: per lifecycle metric, the fewest sequential API calls its timing
must contain (e.g. create -> exec-ready = the create POST + the first `true`
that succeeds). boatbench subtracts that many baseline round trips to report
lifecycle times net of the harness-to-API distance (localhost vs WAN).
"""
import base64
import errno
import http.client
import json
import os
import select
import socket
import ssl
import subprocess
import threading
import time
import urllib.parse
import uuid

POLL_S = 0.25
POLL_SCHEDULE_S = (0.05, 0.05, 0.05, 0.10, 0.10, 0.20)
STATE_TIMEOUT_S = 900
MAX_IDLE_CONNS_PER_ORIGIN = 16

_STALE_CONN_ERRORS = (
    http.client.RemoteDisconnected,
    http.client.BadStatusLine,
    ConnectionResetError,
    BrokenPipeError,
    ConnectionAbortedError,
)


def poll_sleep(attempt):
    """Adaptive fast polling (50 ms -> 100 ms -> 200 ms); honours explicit POLL_S overrides in tests."""
    if POLL_S != 0.25:
        return POLL_S
    return POLL_SCHEDULE_S[min(max(0, attempt), len(POLL_SCHEDULE_S) - 1)]


class ApiError(RuntimeError):
    def __init__(self, status, body, what):
        super().__init__(f"{what}: HTTP {status}: {body[:400]}")
        self.status = status


def _is_conn_alive(sock):
    """Non-blocking readability probe so server-closed idle sockets are discarded before send."""
    if sock is None:
        return False
    try:
        fd = sock.fileno()
        if fd < 0:
            return False
        pending = getattr(sock, "pending", None)
        if callable(pending) and pending() > 0:
            return False
        rlist, _, xlist = select.select([sock], [], [sock], 0)
        if xlist:
            return False
        if not rlist:
            return True
        prev_timeout = sock.gettimeout()
        try:
            if isinstance(sock, ssl.SSLSocket):
                if hasattr(socket, "MSG_DONTWAIT"):
                    raw = socket.socket.recv(sock, 1, socket.MSG_PEEK | socket.MSG_DONTWAIT)
                    if not raw:
                        return False
                sock.setblocking(False)
                data = sock.recv(1)
                return False if data or data == b"" else True
            if hasattr(socket, "MSG_DONTWAIT"):
                sock.recv(1, socket.MSG_PEEK | socket.MSG_DONTWAIT)
            else:
                sock.setblocking(False)
                sock.recv(1, socket.MSG_PEEK)
            return False
        except (ssl.SSLWantReadError, BlockingIOError):
            return True
        except OSError as exc:
            if exc.errno in (errno.EAGAIN, errno.EWOULDBLOCK):
                return True
            return False
        finally:
            try:
                sock.settimeout(prev_timeout)
            except OSError:
                pass
    except (OSError, ValueError):
        return False


class _ConnPool:
    """Thread-safe HTTP/1.1 keep-alive connection pool keyed by (scheme, host, port)."""

    MAX_IDLE_CONNS_PER_ORIGIN = MAX_IDLE_CONNS_PER_ORIGIN

    def __init__(self):
        self._lock = threading.Lock()
        self._idle = {}
        self._ssl_ctx = None

    def _get_ssl_context(self):
        with self._lock:
            if self._ssl_ctx is None:
                self._ssl_ctx = ssl.create_default_context()
            return self._ssl_ctx

    def _new_conn(self, scheme, host, port, timeout):
        if scheme == "https":
            return http.client.HTTPSConnection(host, port, timeout=timeout, context=self._get_ssl_context())
        if scheme == "http":
            return http.client.HTTPConnection(host, port, timeout=timeout)
        raise ValueError(f"unsupported URL scheme: {scheme!r}")

    def _checkout(self, key, timeout):
        scheme, host, port = key
        stale = []
        chosen = None
        with self._lock:
            bucket = self._idle.get(key)
            while bucket:
                conn = bucket.pop()
                if conn.sock is not None and _is_conn_alive(conn.sock):
                    conn.timeout = timeout
                    try:
                        conn.sock.settimeout(timeout)
                        chosen = conn
                        break
                    except OSError:
                        pass
                stale.append(conn)
            if bucket is not None and not bucket:
                self._idle.pop(key, None)
        for dead in stale:
            try:
                dead.close()
            except OSError:
                pass
        if chosen is not None:
            return chosen, True
        return self._new_conn(scheme, host, port, timeout), False

    def _checkin(self, key, conn):
        if conn.sock is None:
            return
        evicted = []
        with self._lock:
            bucket = self._idle.setdefault(key, [])
            bucket.append(conn)
            while len(bucket) > self.MAX_IDLE_CONNS_PER_ORIGIN:
                evicted.append(bucket.pop(0))
        for old in evicted:
            try:
                old.close()
            except OSError:
                pass

    def request(self, method, url, key, body=None, raw=None, timeout=660, ctype="application/json", headers=None,
                poll=False):
        parts = urllib.parse.urlsplit(url)
        scheme = parts.scheme.lower()
        host = parts.hostname
        if not host:
            raise ValueError(f"missing host in URL: {url!r}")
        port = parts.port or (443 if scheme == "https" else 80)
        target = (parts.path or "/") + (f"?{parts.query}" if parts.query else "")
        pool_key = (scheme, host, port)

        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        hdrs = {
            "Authorization": f"Bearer {key}",
            "Connection": "keep-alive",
            "User-Agent": "boatbench/1",
        }
        if headers:
            for hk, hv in headers.items():
                if hk.lower() == "idempotency-key":
                    hdrs["Idempotency-Key"] = hv
                else:
                    hdrs[hk] = hv
        if data is not None:
            hdrs["Content-Type"] = ctype
            hdrs["Content-Length"] = str(len(data))
        elif method.upper() in ("POST", "PUT", "PATCH"):
            hdrs.setdefault("Content-Length", "0")

        idempotent = (
            method.upper() in ("GET", "HEAD", "OPTIONS", "PUT", "DELETE")
            or bool(poll)
            or bool(hdrs.get("Idempotency-Key"))
        )
        conn, reused = self._checkout(pool_key, timeout)
        for attempt in range(2):
            sent_request = False
            orig_send = conn.send

            def _tracked_send(chunk, _orig=orig_send):
                nonlocal sent_request
                sent_request = True
                return _orig(chunk)

            conn.send = _tracked_send
            try:
                conn.request(method, target, body=data, headers=hdrs)
                sent_request = True
                conn.send = orig_send
                resp = conn.getresponse()
                resp_bytes = resp.read()
                will_close = resp.will_close or resp.getheader("Connection", "").lower() == "close"
                if will_close:
                    conn.close()
                else:
                    self._checkin(pool_key, conn)
                text = resp_bytes.decode("utf-8", "replace")
                if resp.status >= 400:
                    raise ApiError(resp.status, text, f"{method} {url}")
                return text
            except ApiError:
                raise
            except _STALE_CONN_ERRORS:
                conn.send = orig_send
                try:
                    conn.close()
                except OSError:
                    pass
                if reused and attempt == 0 and (idempotent or not sent_request):
                    conn = self._new_conn(scheme, host, port, timeout)
                    reused = False
                    continue
                raise
            except BaseException:
                conn.send = orig_send
                try:
                    conn.close()
                except OSError:
                    pass
                raise

    def idle_count(self):
        with self._lock:
            return sum(len(v) for v in self._idle.values())

    def close(self):
        with self._lock:
            conns = [c for bucket in self._idle.values() for c in bucket]
            self._idle.clear()
        for c in conns:
            try:
                c.close()
            except OSError:
                pass


_DEFAULT_POOL = _ConnPool()
_ACTIVE_POOL = threading.local()


class _CallTracker:
    def _init_calls(self):
        self._call_lock = threading.Lock()
        self.http_calls = 0
        self.poll_http_calls = 0
        self._tls = threading.local()
        self._pool = _ConnPool()
        self._calls = self

    def _bump_calls(self, poll=False):
        with self._call_lock:
            self.http_calls += 1
            if poll:
                self.poll_http_calls += 1
        if not poll:
            self._tls.calls = getattr(self._tls, "calls", 0) + 1

    def record(self, poll=False):
        self._bump_calls(poll=poll)

    def note_serial_poll(self, n: int = 1):
        self._tls.calls = getattr(self._tls, "calls", 0) + n

    def thread_calls(self):
        return getattr(self._tls, "calls", 0)

    def _request(self, method, url, key, body=None, raw=None, timeout=660, ctype="application/json",
                 headers=None, poll=False):
        self._bump_calls(poll=poll)
        prev = getattr(_ACTIVE_POOL, "pool", None)
        _ACTIVE_POOL.pool = self._pool
        try:
            if _http is _DEFAULT_HTTP:
                return _http(method, url, key, body=body, raw=raw, timeout=timeout, ctype=ctype,
                             headers=headers, poll=poll)
            try:
                return _http(method, url, key, body=body, raw=raw, timeout=timeout, ctype=ctype,
                             headers=headers, poll=poll)
            except TypeError as exc:
                if "poll" not in str(exc):
                    raise
                return _http(method, url, key, body=body, raw=raw, timeout=timeout, ctype=ctype, headers=headers)
        finally:
            _ACTIVE_POOL.pool = prev

    def close(self):
        self._pool.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


def _http(method, url, key, body=None, raw=None, timeout=660, ctype="application/json", headers=None, poll=False):
    pool = getattr(_ACTIVE_POOL, "pool", None) or _DEFAULT_POOL
    return pool.request(method, url, key, body=body, raw=raw, timeout=timeout, ctype=ctype, headers=headers, poll=poll)


_DEFAULT_HTTP = _http


def _wait(pred, what, timeout=STATE_TIMEOUT_S):
    end = time.monotonic() + timeout
    attempt = 0
    while time.monotonic() < end:
        if pred():
            return
        time.sleep(poll_sleep(attempt))
        attempt += 1
    raise TimeoutError(f"{what}: not done after {timeout}s")


class PandaOS(_CallTracker):
    """components/boat/api (README: Endpoints). Size names are boatd's config."""

    name = "pandaos"
    ROUND_TRIPS = {"create_ready_s": 2, "fork_ready_s": 2, "resume_ready_s": 2, "from_snapshot_ready_s": 2,
                   "stop_s": 1, "snapshot_s": 1, "concurrent10_wall_s": 2, "concurrent8_wall_s": 2,
                   "exec_rtt_p50_ms": 1, "exec_rtt_p95_ms": 1}

    def __new__(cls, url=None, key=None, size=None, no_env=None, environment=None,
                setup_script=None, api_mode="native"):
        if cls is PandaOS and api_mode == "compat":
            return PandaOSCompat(
                url=url, key=key, size=size,
                no_env=True if no_env is None else no_env,
                environment=environment, setup_script=setup_script,
            )
        if api_mode != "native":
            raise ValueError(f"unsupported api_mode: {api_mode!r} (expected 'native' or 'compat')")
        return super().__new__(cls)

    def __init__(self, url=None, key=None, size=None, no_env=None, environment=None,
                 setup_script=None, api_mode="native"):
        self._init_calls()
        self.url = (url or os.environ["BOAT_URL"]).rstrip("/")
        self.key = key or os.environ["BOAT_KEY"]
        self.size = size or os.environ.get("BOATBENCH_PANDAOS_SIZE", "default")
        self.no_env = True if no_env is None else bool(no_env)
        self.environment = environment
        self.setup_script = setup_script
        self.api_mode = "native"

    def _j(self, method, path, body=None, poll=False, headers=None):
        out = self._request(method, self.url + path, self.key, body=body, headers=headers, poll=poll)
        return json.loads(out) if out.strip() else {}

    def create(self, name, snapshot=None):
        if self.no_env:
            body = {"name": name, "size": self.size, "ttl_s": 7200, "secrets": [], "no_env": True}
            if snapshot:
                body = {"name": name, "snapshot": snapshot, "ttl_s": 7200, "secrets": [], "no_env": True}
        else:
            body = {"name": name, "ttl_s": 7200}
            if snapshot:
                body["snapshot"] = snapshot
            else:
                body["size"] = self.size
            if self.environment:
                body["environment"] = self.environment
        return self._j("POST", "/v1/vms", body, headers={"Idempotency-Key": uuid.uuid4().hex})["id"]

    def exec(self, vm, cmd, timeout=600):
        return self._exec(vm, ["bash", "-lc", cmd], timeout)

    def run_true(self, vm, poll=True):
        return self._exec(vm, ["true"], 30, poll=poll)[0]

    def _exec(self, vm, argv, timeout, poll=False):
        out = self._request("POST", f"{self.url}/v1/vms/{vm}/exec", self.key,
                            body={"cmd": argv, "timeout_s": timeout}, timeout=timeout + 60, poll=poll)
        so, se, rc = [], [], None
        for line in out.splitlines():
            if not line.strip():
                continue
            f = json.loads(line)
            if "exit" in f:
                rc = f["exit"]
            elif f.get("stream") in ("stdout", "stderr"):
                (so if f["stream"] == "stdout" else se).append(base64.b64decode(f.get("data_b64", "")))
        return rc, b"".join(so).decode("utf-8", "replace"), b"".join(se).decode("utf-8", "replace")

    def put_file(self, vm, path, data):
        self._request("PUT", f"{self.url}/v1/vms/{vm}/files?path={urllib.parse.quote(path)}&mode=644", self.key,
                      raw=data, ctype="application/octet-stream")

    def fork(self, vm, name):
        return self._j("POST", f"/v1/vms/{vm}/fork", {"name": name},
                       headers={"Idempotency-Key": uuid.uuid4().hex})["id"]

    def stop(self, vm):
        self._j("POST", f"/v1/vms/{vm}/stop")  # synchronous: returns once stopped

    def resume(self, vm):
        self._j("POST", f"/v1/vms/{vm}/start", {})

    def snapshot(self, vm, name, on_accepted=None):
        ref = self._j("POST", f"/v1/vms/{vm}/snapshots", {"name": name})["id"]
        if on_accepted:
            on_accepted(ref)
        return ref

    def from_snapshot(self, ref, name):
        return self.create(name, snapshot=ref)

    def delete(self, vm):
        self._j("DELETE", f"/v1/vms/{vm}")

    def delete_snapshot(self, ref):
        self._j("DELETE", f"/v1/snapshots/{ref}")

    def ping(self, vm):
        self._j("GET", f"/v1/vms/{vm}")


class BoatDev(_CallTracker):
    """boat.dev Public API v1 (base https://boat.dev/api/v1), as its OpenAPI
    (https://docs.boat.dev/openapi/boat-v1.yaml) describes it. test_boatdev.py
    checks every request below against the parts of that spec in
    boatdev-openapi.json, and parses the spec's own response examples.

    Create, fork and resume answer 202 and the machine comes up after; the
    shared exec-ready loop in boatbench.py measures until `true` succeeds.
    Stop and saved snapshots are async operations polled to completion.
    Delete needs X-Ascii-Confirm-Delete = the sandbox id, or it is a 409 and
    the sandbox keeps billing until its TTL.

    Plan limits: the $20 plan allows 12 starts a minute and 60 an hour. One
    repeat of workload b starts 4 machines (create, from-snapshot, fork, resume)
    and c starts 10, so a full 5-repeat run needs the $100 plan (30/min, 210/h).
    """

    name = "boatdev"
    READY = ("ready", "idle", "running")
    # stop and saved snapshot are a POST plus at least one GET poll.
    ROUND_TRIPS = {"create_ready_s": 2, "fork_ready_s": 2, "resume_ready_s": 2, "from_snapshot_ready_s": 2,
                   "stop_s": 2, "snapshot_s": 2, "concurrent10_wall_s": 2, "concurrent8_wall_s": 2,
                   "exec_rtt_p50_ms": 1, "exec_rtt_p95_ms": 1}

    def __init__(self, base=None, key=None, size=None):
        self._init_calls()
        self.url = (base or os.environ.get("BOATDEV_API_BASE", "https://boat.dev/api/v1")).rstrip("/")
        self.key = key or os.environ["BOATDEV_API_KEY"]
        self.size = size or os.environ.get("BOATBENCH_BOATDEV_SIZE", "default")

    def _j(self, method, path, body=None, timeout=660, poll=False, headers=None):
        out = self._request(method, self.url + path, self.key, body=body, timeout=timeout, headers=headers, poll=poll)
        return json.loads(out) if out.strip() else {}

    def _state(self, sid, poll=False):
        return self._j("GET", f"/sandboxes/{sid}", poll=poll).get("sandbox", {})

    def create(self, name, frm=None):
        body = {"name": name, "type": self.size, "ttlSeconds": 7200, "noEnv": True}
        if frm:
            body["from"] = frm
        return self._j("POST", "/sandboxes", body, headers={"Idempotency-Key": str(uuid.uuid4())})["sandbox"]["id"]

    def exec(self, sid, cmd, timeout=600):
        r = self._j("POST", f"/sandboxes/{sid}/commands",
                    {"command": "bash -lc " + _sh_quote(cmd), "timeoutSeconds": min(timeout, 600)},
                    timeout=timeout + 60)
        return r.get("exitCode"), r.get("stdout", ""), r.get("stderr", "")

    def run_true(self, sid, poll=True):
        try:
            return self._j("POST", f"/sandboxes/{sid}/commands", {"command": "true", "timeoutSeconds": 30},
                           timeout=90, poll=poll).get("exitCode")
        except ApiError as e:
            if e.status == 409:  # sandbox_starting / not ready yet: caller retries
                return None
            raise

    def _poll(self, path_or_pred, done=None, what="poll", timeout=STATE_TIMEOUT_S):
        polls = 0
        if callable(path_or_pred) and done is None:
            def pred():
                nonlocal polls
                before = self.poll_http_calls
                ok = path_or_pred()
                if self.poll_http_calls > before:
                    polls += 1
                return ok
            _wait(pred, what, timeout=timeout)
        else:
            def pred():
                nonlocal polls
                polls += 1
                return done(self._j("GET", path_or_pred, poll=True))
            _wait(pred, what, timeout=timeout)
        if polls >= 1:
            self._calls.note_serial_poll(1)

    def put_file(self, sid, path, data):
        self._j("PUT", f"/sandboxes/{sid}/files",
                {"path": path, "content": base64.b64encode(data).decode(), "encoding": "base64"})

    def fork(self, sid, name):
        # The fork body has no name (labels instead); the 202's `id` is the fork's id and
        # its `sandbox` may be null.
        return self._j("POST", f"/sandboxes/{sid}/fork",
                       {"labels": {"boatbench": name}, "ttlSeconds": 7200, "noEnv": True},
                       headers={"Idempotency-Key": str(uuid.uuid4())})["id"]

    def stop(self, sid):
        self._j("POST", f"/sandboxes/{sid}/stop", {})

        def stopped(r):
            sb = r.get("sandbox") or {}
            if (sb.get("stop") or {}).get("status") == "superseded":
                raise RuntimeError(f"stop {sid} superseded: {sb.get('stop')}")
            return sb.get("state") == "archived" or (sb.get("stop") or {}).get("status") == "completed"
        self._poll(f"/sandboxes/{sid}", stopped, f"stop {sid}")

    def resume(self, sid):
        self._j("POST", f"/sandboxes/{sid}/resume", {"ttlSeconds": 7200})

    def snapshot(self, sid, name, on_accepted=None):
        self._j("POST", "/saved-snapshots", {"sandboxId": sid, "name": name})
        # The accepted name is already the deletion reference, even if readiness times out.
        if on_accepted:
            on_accepted(name)

        def ready(r):
            st = (r.get("snapshot") or {}).get("status")
            if st == "failed":
                raise RuntimeError(f"saved snapshot {name} failed: {r}")
            return st == "ready"
        self._poll(f"/saved-snapshots/{name}", ready, f"snapshot {name}")
        return name

    def from_snapshot(self, ref, name):
        return self.create(name, frm=ref)

    def delete(self, sid):
        self._request("DELETE", f"{self.url}/sandboxes/{sid}", self.key, headers={"X-Ascii-Confirm-Delete": sid})

    def delete_snapshot(self, ref):
        self._j("DELETE", f"/saved-snapshots/{ref}")

    def ping(self, sid):
        self._state(sid)


class PandaOSCompat(BoatDev):
    """PandaOS boat-api over its /api/v1 boat.dev-compatible surface.

    Uses BOAT_URL (appending /api/v1 unless already present) and BOAT_KEY.
    Defaults to no_env=True like PandaOS and BoatDev so benchmarks compare
    like-for-like; pass no_env=False (or --with-env) to exercise the
    environment/setup path.
    """

    name = "pandaos-compat"
    api_mode = "compat"

    def __init__(self, url=None, key=None, size=None, no_env=True, environment=None, setup_script=None):
        base = (url or os.environ["BOAT_URL"]).rstrip("/")
        if not base.endswith("/api/v1"):
            base = base + "/api/v1"
        super().__init__(
            base=base,
            key=key or os.environ["BOAT_KEY"],
            size=size or os.environ.get("BOATBENCH_PANDAOS_SIZE", "default"),
        )
        self.no_env = bool(no_env)
        self.environment = environment
        self.setup_script = setup_script

    def create(self, name, frm=None):
        body = {"name": name, "type": self.size, "ttlSeconds": 7200}
        if self.no_env:
            body["noEnv"] = True
        else:
            if self.environment:
                body["environment"] = self.environment
            if self.setup_script and not frm:
                body["setupScript"] = self.setup_script
        if frm:
            body["from"] = frm
        resp = self._j("POST", "/sandboxes", body, headers={"Idempotency-Key": str(uuid.uuid4())})
        sb = resp.get("sandbox") or {}
        return sb.get("id") or resp["id"]

    def fork(self, sid, name):
        body = {"labels": {"boatbench": name}, "ttlSeconds": 7200}
        if self.no_env:
            body["noEnv"] = True
        elif self.environment:
            body["environment"] = self.environment
        return self._j("POST", f"/sandboxes/{sid}/fork", body,
                       headers={"Idempotency-Key": str(uuid.uuid4())})["id"]

    def resume(self, sid):
        body = {"ttlSeconds": 7200}
        if self.no_env:
            body["noEnv"] = True
        elif self.environment:
            body["environment"] = self.environment
        self._j("POST", f"/sandboxes/{sid}/resume", body)

    @staticmethod
    def _stop_done(r, sid):
        sb = r.get("sandbox") or {}
        op = r.get("operation") or sb.get("stop") or {}
        if op.get("status") == "superseded":
            raise RuntimeError(f"stop {sid} superseded: {op}")
        return sb.get("state") == "archived" or op.get("status") == "completed"

    def stop(self, sid):
        r = self._j("POST", f"/sandboxes/{sid}/stop", {})
        if self._stop_done(r, sid):
            return
        self._poll(f"/sandboxes/{sid}", lambda x: self._stop_done(x, sid), f"stop {sid}")

    def snapshot(self, sid, name, on_accepted=None):
        r = self._j("POST", "/saved-snapshots", {"sandboxId": sid, "name": name})
        if on_accepted:
            on_accepted(name)
        st = (r.get("snapshot") or {}).get("status")
        if st == "failed":
            raise RuntimeError(f"saved snapshot {name} failed: {r}")
        if st == "ready":
            return name

        def ready(x):
            status = (x.get("snapshot") or {}).get("status")
            if status == "failed":
                raise RuntimeError(f"saved snapshot {name} failed: {x}")
            return status == "ready"
        self._poll(f"/saved-snapshots/{name}", ready, f"snapshot {name}")
        return name


class Host:
    """No VM: the same scripts on the machine running the harness, to measure
    VM / host overhead. Only workloads a, d and g apply: there is no lifecycle. g runs N
    Node loops side by side on the host (host-native density baseline).
    BOATBENCH_HOST_DIR is $HOME for the scripts, so fio can hit the store the VMs use."""

    name, size = "host", "host"
    PARTS = frozenset("adg")
    ROUND_TRIPS = {}

    def __init__(self):
        self.env = dict(os.environ, HOME=os.environ.get("BOATBENCH_HOST_DIR", os.path.expanduser("~")))

    def create(self, name):
        return "host"

    def exec(self, vm, cmd, timeout=600):
        r = subprocess.run(["bash", "-lc", cmd], env=self.env, capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout, r.stderr

    def run_true(self, vm, poll=True):
        return 0

    def delete(self, vm):
        pass

    def ping(self, vm):
        pass

    def close(self):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


def _sh_quote(s):
    return "'" + s.replace("'", "'\"'\"'") + "'"


PandaBoat = PandaOS
DRIVERS = {"pandaos": PandaOS, "pandaos-compat": PandaOSCompat, "boatdev": BoatDev, "host": Host}
