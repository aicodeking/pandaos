"""Thin provider adapters: PandaOS boat-api and boat.dev's public API v1.

Both speak plain HTTP through urllib (no SDK, no CLI) so the client-side cost
of a call is the same code on both sides. Each adapter only maps the common
verbs below onto its provider's documented endpoints; timing, polling for
exec-readiness and all workloads live in boatbench.py so they are shared.

  create(name) -> id          stop(id)              snapshot(id, name) -> ref
  exec(id, cmd, timeout)      resume(id)            from_snapshot(ref, name) -> id
  run_true(id)                fork(id, name) -> id  delete(id) / delete_snapshot(ref)
  put_file(id, path, data)    ping(id): one cheap authenticated GET (API round-trip baseline)

ROUND_TRIPS: per lifecycle metric, the fewest sequential API calls its timing
must contain (e.g. create -> exec-ready = the create POST + the first `true`
that succeeds). boatbench subtracts that many baseline round trips to report
lifecycle times net of the harness-to-API distance (localhost vs WAN).
"""
import base64
import json
import os
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

POLL_S = 0.25
STATE_TIMEOUT_S = 900


class ApiError(RuntimeError):
    def __init__(self, status, body, what):
        super().__init__(f"{what}: HTTP {status}: {body[:400]}")
        self.status = status


def _http(method, url, key, body=None, raw=None, timeout=660, ctype="application/json", headers=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", f"Bearer {key}")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    if data is not None:
        req.add_header("Content-Type", ctype)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        raise ApiError(e.code, e.read().decode("utf-8", "replace"), f"{method} {url}") from None


def _wait(pred, what, timeout=STATE_TIMEOUT_S):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if pred():
            return
        time.sleep(POLL_S)
    raise TimeoutError(f"{what}: not done after {timeout}s")


class PandaOS:
    """components/boat/api (README: Endpoints). Size names are boatd's config."""

    name = "pandaos"
    ROUND_TRIPS = {"create_ready_s": 2, "fork_ready_s": 2, "resume_ready_s": 2, "from_snapshot_ready_s": 2,
                   "stop_s": 1, "snapshot_s": 1, "concurrent10_wall_s": 2, "exec_rtt_p50_ms": 1}

    def __init__(self, url=None, key=None, size=None):
        self.url = (url or os.environ["BOAT_URL"]).rstrip("/")
        self.key = key or os.environ["BOAT_KEY"]
        self.size = size or os.environ.get("BOATBENCH_PANDAOS_SIZE", "default")

    def _j(self, method, path, body=None):
        out = _http(method, self.url + path, self.key, body)
        return json.loads(out) if out.strip() else {}

    def create(self, name, snapshot=None):
        body = {"name": name, "size": self.size, "ttl_s": 7200, "secrets": [], "no_env": True}
        if snapshot:
            body = {"name": name, "snapshot": snapshot, "ttl_s": 7200, "secrets": [], "no_env": True}
        return self._j("POST", "/v1/vms", body)["id"]

    def exec(self, vm, cmd, timeout=600):
        return self._exec(vm, ["bash", "-lc", cmd], timeout)

    def run_true(self, vm):
        return self._exec(vm, ["true"], 30)[0]

    def _exec(self, vm, argv, timeout):
        out = _http("POST", f"{self.url}/v1/vms/{vm}/exec", self.key,
                    {"cmd": argv, "timeout_s": timeout}, timeout=timeout + 60)
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
        _http("PUT", f"{self.url}/v1/vms/{vm}/files?path={urllib.parse.quote(path)}&mode=644", self.key,
              raw=data, ctype="application/octet-stream")

    def fork(self, vm, name):
        return self._j("POST", f"/v1/vms/{vm}/fork", {"name": name})["id"]

    def stop(self, vm):
        self._j("POST", f"/v1/vms/{vm}/stop")  # synchronous: returns once stopped

    def resume(self, vm):
        self._j("POST", f"/v1/vms/{vm}/start", {})

    def snapshot(self, vm, name):
        return self._j("POST", f"/v1/vms/{vm}/snapshots", {"name": name})["id"]

    def from_snapshot(self, ref, name):
        return self.create(name, snapshot=ref)

    def delete(self, vm):
        self._j("DELETE", f"/v1/vms/{vm}")

    def delete_snapshot(self, ref):
        self._j("DELETE", f"/v1/snapshots/{ref}")

    def ping(self, vm):
        self._j("GET", f"/v1/vms/{vm}")


class BoatDev:
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
                   "stop_s": 2, "snapshot_s": 2, "concurrent10_wall_s": 2, "exec_rtt_p50_ms": 1}

    def __init__(self, base=None, key=None, size=None):
        self.url = (base or os.environ.get("BOATDEV_API_BASE", "https://boat.dev/api/v1")).rstrip("/")
        self.key = key or os.environ["BOATDEV_API_KEY"]
        self.size = size or os.environ.get("BOATBENCH_BOATDEV_SIZE", "default")

    def _j(self, method, path, body=None, timeout=660):
        out = _http(method, self.url + path, self.key, body, timeout=timeout)
        return json.loads(out) if out.strip() else {}

    def _state(self, sid):
        return self._j("GET", f"/sandboxes/{sid}").get("sandbox", {})

    def create(self, name, frm=None):
        body = {"name": name, "type": self.size, "ttlSeconds": 7200, "noEnv": True}
        if frm:
            body["from"] = frm
        return self._j("POST", "/sandboxes", body)["sandbox"]["id"]

    def exec(self, sid, cmd, timeout=600):
        r = self._j("POST", f"/sandboxes/{sid}/commands",
                    {"command": "bash -lc " + _sh_quote(cmd), "timeoutSeconds": min(timeout, 600)},
                    timeout=timeout + 60)
        return r.get("exitCode"), r.get("stdout", ""), r.get("stderr", "")

    def run_true(self, sid):
        try:
            return self._j("POST", f"/sandboxes/{sid}/commands", {"command": "true", "timeoutSeconds": 30},
                           timeout=90).get("exitCode")
        except ApiError as e:
            if e.status == 409:  # sandbox_starting / not ready yet: caller retries
                return None
            raise

    def put_file(self, sid, path, data):
        self._j("PUT", f"/sandboxes/{sid}/files",
                {"path": path, "content": base64.b64encode(data).decode(), "encoding": "base64"})

    def fork(self, sid, name):
        # The fork body has no name (labels instead); the 202's `id` is the fork's id and
        # its `sandbox` may be null.
        return self._j("POST", f"/sandboxes/{sid}/fork",
                       {"labels": {"boatbench": name}, "ttlSeconds": 7200, "noEnv": True})["id"]

    def stop(self, sid):
        self._j("POST", f"/sandboxes/{sid}/stop", {})

        def stopped():
            sb = self._state(sid)
            if (sb.get("stop") or {}).get("status") == "superseded":
                raise RuntimeError(f"stop {sid} superseded: {sb.get('stop')}")
            return sb.get("state") == "archived" or (sb.get("stop") or {}).get("status") == "completed"
        _wait(stopped, f"stop {sid}")

    def resume(self, sid):
        self._j("POST", f"/sandboxes/{sid}/resume", {"ttlSeconds": 7200})

    def snapshot(self, sid, name):
        self._j("POST", "/saved-snapshots", {"sandboxId": sid, "name": name})

        def ready():
            r = self._j("GET", f"/saved-snapshots/{name}")
            st = (r.get("snapshot") or {}).get("status")
            if st == "failed":
                raise RuntimeError(f"saved snapshot {name} failed: {r}")
            return st == "ready"
        _wait(ready, f"snapshot {name}")
        return name

    def from_snapshot(self, ref, name):
        return self.create(name, frm=ref)

    def delete(self, sid):
        _http("DELETE", f"{self.url}/sandboxes/{sid}", self.key, headers={"X-Ascii-Confirm-Delete": sid})

    def delete_snapshot(self, ref):
        self._j("DELETE", f"/saved-snapshots/{ref}")

    def ping(self, sid):
        self._state(sid)


class Host:
    """No VM: the same scripts on the machine running the harness, to measure
    VM / host overhead. Only workloads a and d apply: there is no lifecycle.
    BOATBENCH_HOST_DIR is $HOME for the scripts, so fio can hit the store the VMs use."""

    name, size = "host", "host"
    PARTS = frozenset("ad")
    ROUND_TRIPS = {}

    def __init__(self):
        self.env = dict(os.environ, HOME=os.environ.get("BOATBENCH_HOST_DIR", os.path.expanduser("~")))

    def create(self, name):
        return "host"

    def exec(self, vm, cmd, timeout=600):
        r = subprocess.run(["bash", "-lc", cmd], env=self.env, capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout, r.stderr

    def run_true(self, vm):
        return 0

    def delete(self, vm):
        pass

    def ping(self, vm):
        pass


def _sh_quote(s):
    return "'" + s.replace("'", "'\"'\"'") + "'"


DRIVERS = {"pandaos": PandaOS, "boatdev": BoatDev, "host": Host}
