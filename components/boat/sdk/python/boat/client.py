"""Boat API client built on urllib. Python 3.8+."""
import base64
import hashlib
import hmac
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Dict, Iterator, List, Optional


class BoatError(Exception):
    def __init__(self, status: int, code: str, message: str, request_id: str = ""):
        super().__init__(f"{message} (HTTP {status}{', ' + request_id if request_id else ''})")
        self.status, self.code, self.message, self.request_id = status, code, message, request_id


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

    @classmethod
    def from_json(cls, d: Dict[str, Any]) -> "VM":
        return cls(id=d.get("id", ""), name=d.get("name") or "", size=d.get("size") or "",
                   state=d.get("state") or "", image=d.get("image") or "", team=d.get("team") or "",
                   labels=d.get("labels") or {}, raw=d)


@dataclass(frozen=True)
class ExecEvent:
    """One line of an exec/prompt stream: output chunk, or the final exit code."""
    stream: str = ""  # "stdout" | "stderr" | "" for the exit event
    data: bytes = b""
    exit_code: Optional[int] = None
    error: Optional[Any] = None


@dataclass(frozen=True)
class ExecResult:
    stdout: bytes
    stderr: bytes
    exit_code: int


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


def _config() -> Dict[str, str]:
    path = os.environ.get("BOAT_CONFIG") or os.path.join(
        os.environ.get("XDG_CONFIG_HOME") or os.path.expanduser("~/.config"), "boat", "config.json")
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def _q(s: str) -> str:
    return urllib.parse.quote(str(s), safe="")


class Boat:
    """Client. url/key default to BOAT_URL/BOAT_API_KEY, then ~/.config/boat/config.json."""

    def __init__(self, url: Optional[str] = None, key: Optional[str] = None, timeout: float = 60.0):
        cfg = _config()
        self.url = (url or os.environ.get("BOAT_URL") or cfg.get("url") or "").rstrip("/")
        self.key = key or os.environ.get("BOAT_API_KEY") or cfg.get("key") or ""
        if not self.url or not self.key:
            raise ValueError("Boat url and key are required (args, BOAT_URL/BOAT_API_KEY, or `boat login`)")
        self.timeout = timeout

    # ---- transport ----

    def _open(self, method: str, path: str, body: Optional[bytes] = None,
              ctype: Optional[str] = None, timeout: Optional[float] = None):
        req = urllib.request.Request(self.url + path, data=body, method=method)
        req.add_header("Authorization", "Bearer " + self.key)
        req.add_header("User-Agent", "boat-python/0.1.0")
        if ctype:
            req.add_header("Content-Type", ctype)
        try:
            return urllib.request.urlopen(req, timeout=timeout if timeout is not None else self.timeout)
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                err = json.loads(raw)["error"]
                raise BoatError(e.code, err.get("code", ""), err.get("message", ""), err.get("request_id", "")) from None
            except (ValueError, KeyError, TypeError):
                raise BoatError(e.code, "http_error", raw.decode("utf-8", "replace")[:200] or e.reason) from None

    def _call(self, method: str, path: str, body: Any = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        with self._open(method, path, data, "application/json" if data is not None else None) as r:
            raw = r.read()
        return json.loads(raw) if raw.strip() else None

    def _stream(self, path: str, body: Dict[str, Any]) -> Iterator[ExecEvent]:
        # No read timeout: agents can be silent for minutes.
        r = self._open("POST", path, json.dumps(body).encode(), "application/json", timeout=None)
        with r:
            for line in r:
                line = line.strip()
                if not line:
                    continue
                ev = json.loads(line)
                yield ExecEvent(stream=ev.get("stream", ""),
                                data=base64.b64decode(ev["data_b64"]) if ev.get("data_b64") else b"",
                                exit_code=ev.get("exit"), error=ev.get("error"))

    @staticmethod
    def _clean(d: Dict[str, Any]) -> Dict[str, Any]:
        return {k: v for k, v in d.items() if v is not None}

    # ---- identity ----

    def me(self) -> Dict[str, Any]:
        return self._call("GET", "/v1/me")

    def images(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/images")["images"]

    def usage(self, frm: Optional[str] = None, to: Optional[str] = None) -> Dict[str, Any]:
        q = urllib.parse.urlencode(self._clean({"from": frm, "to": to}))
        return self._call("GET", "/v1/usage" + ("?" + q if q else ""))

    # ---- VMs ----

    def list_vms(self) -> List[VM]:
        return [VM.from_json(v) for v in self._call("GET", "/v1/vms")["vms"]]

    def get_vm(self, vm_id: str) -> VM:
        return VM.from_json(self._call("GET", f"/v1/vms/{_q(vm_id)}"))

    def create_vm(self, name: Optional[str] = None, size: Optional[str] = None, image: Optional[str] = None,
                  ttl_s: Optional[int] = None, labels: Optional[Dict[str, str]] = None,
                  env: Optional[Dict[str, str]] = None, secrets: Optional[List[str]] = None,
                  idle_stop_s: Optional[int] = None, snapshot: Optional[str] = None) -> VM:
        """secrets: team secret names to inject (None = all, [] = none); forks inherit it."""
        body = self._clean({"name": name, "size": size, "image": image, "ttl_s": ttl_s, "labels": labels, "env": env,
                            "secrets": None if secrets is None else list(secrets), "idle_stop_s": idle_stop_s,
                            "snapshot": snapshot})
        return VM.from_json(self._call("POST", "/v1/vms", body))

    def start(self, vm_id: str) -> VM:
        return VM.from_json(self._call("POST", f"/v1/vms/{_q(vm_id)}/start") or {"id": vm_id})

    def stop(self, vm_id: str) -> VM:
        return VM.from_json(self._call("POST", f"/v1/vms/{_q(vm_id)}/stop") or {"id": vm_id})

    def fork(self, vm_id: str, name: Optional[str] = None) -> VM:
        return VM.from_json(self._call("POST", f"/v1/vms/{_q(vm_id)}/fork", self._clean({"name": name})))

    def delete(self, vm_id: str) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}")

    def exec(self, vm_id: str, cmd: List[str], env: Optional[Dict[str, str]] = None, cwd: Optional[str] = None,
             timeout_s: Optional[int] = None) -> Iterator[ExecEvent]:
        """Stream a command's output. The last event carries exit_code."""
        body = self._clean({"cmd": list(cmd), "env": env, "cwd": cwd, "timeout_s": timeout_s})
        return self._stream(f"/v1/vms/{_q(vm_id)}/exec", body)

    def run(self, vm_id: str, cmd: List[str], **kw: Any) -> ExecResult:
        """exec() collected into stdout/stderr/exit_code."""
        return _collect(self.exec(vm_id, cmd, **kw))

    def prompt(self, vm_id: str, prompt: str, agent: str = "claude", model: Optional[str] = None,
               cwd: Optional[str] = None, timeout_s: Optional[int] = None) -> Iterator[ExecEvent]:
        """Run claude/codex/opencode headless in the VM with team secrets as env."""
        body = self._clean({"prompt": prompt, "agent": agent, "model": model, "cwd": cwd, "timeout_s": timeout_s})
        return self._stream(f"/v1/vms/{_q(vm_id)}/prompt", body)

    def read_file(self, vm_id: str, path: str) -> bytes:
        with self._open("GET", f"/v1/vms/{_q(vm_id)}/files?path={_q(path)}") as r:
            return r.read()

    def write_file(self, vm_id: str, path: str, data: bytes) -> None:
        with self._open("PUT", f"/v1/vms/{_q(vm_id)}/files?path={_q(path)}", bytes(data), "application/octet-stream") as r:
            r.read()

    def ports(self, vm_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/ports")["ports"]

    def add_port(self, vm_id: str, port: int, public: bool = False, auth: Optional[str] = None) -> Dict[str, Any]:
        """auth="private" (default): the gateway route needs a token from port_url().
        public=True / auth="public": open route plus a raw TCP host port."""
        mode = auth or ("public" if public else "private")
        return self._call("POST", f"/v1/vms/{_q(vm_id)}/ports", {"port": port, "auth": mode})

    def port_url(self, vm_id: str, port: int, ttl_s: Optional[int] = None) -> Dict[str, Any]:
        """{url, auth, token?, expires_at?}; send token as header X-Boat-Route-Token, or open url in a browser."""
        q = f"?ttl_s={int(ttl_s)}" if ttl_s else ""
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/ports/{int(port)}/url{q}")

    def remove_port(self, vm_id: str, port: int) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/ports/{int(port)}")

    def snapshots(self, vm_id: str) -> Any:
        return self._call("GET", f"/v1/vms/{_q(vm_id)}/snapshots")

    def create_snapshot(self, vm_id: str, name: Optional[str] = None) -> Dict[str, Any]:
        return self._call("POST", f"/v1/vms/{_q(vm_id)}/snapshots", self._clean({"name": name}))

    def delete_snapshot(self, vm_id: str, snapshot_id: str) -> None:
        self._call("DELETE", f"/v1/vms/{_q(vm_id)}/snapshots/{_q(snapshot_id)}")

    def ssh(self, vm_id: str, public_key: Optional[str] = None) -> SSHInfo:
        d = self._call("POST", f"/v1/vms/{_q(vm_id)}/ssh", self._clean({"public_key": public_key}))
        return SSHInfo(host=d["host"], port=int(d["port"]), user=d["user"], jump=d.get("jump", ""))

    def desktop(self, vm_id: str) -> DesktopURL:
        d = self._call("GET", f"/v1/vms/{_q(vm_id)}/desktop")
        return DesktopURL(url=d["url"], expires_at=d["expires_at"])

    # ---- teams, keys, secrets, webhooks ----

    def teams(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/teams")["teams"]

    def create_team(self, name: str, owner: Optional[str] = None) -> Dict[str, Any]:
        """Operator only. With owner, a new user owns the team; result["owner"]["key"]["token"]."""
        return self._call("POST", "/v1/teams", self._clean({"name": name, "owner": owner}))

    def delete_team(self, team_id: str) -> None:
        """Operator only; the team must have no VMs."""
        self._call("DELETE", f"/v1/teams/{_q(team_id)}")

    def add_member(self, team_id: str, name: str, role: str = "member") -> Dict[str, Any]:
        return self._call("POST", f"/v1/teams/{_q(team_id)}/members", {"name": name, "role": role})

    def keys(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/keys")["keys"]

    def create_key(self, name: str = "key", scopes: Optional[List[str]] = None,
                   team_id: Optional[str] = None) -> Dict[str, Any]:
        return self._call("POST", "/v1/keys", self._clean({"name": name, "scopes": scopes, "team_id": team_id}))

    def delete_key(self, key_id: str) -> None:
        self._call("DELETE", f"/v1/keys/{_q(key_id)}")

    def secrets(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/secrets")["secrets"]

    def set_secret(self, name: str, value: str) -> None:
        self._call("PUT", f"/v1/secrets/{_q(name)}", {"value": value})

    def delete_secret(self, name: str) -> None:
        self._call("DELETE", f"/v1/secrets/{_q(name)}")

    def webhooks(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/webhooks")["webhooks"]

    def create_webhook(self, url: str, events: Optional[List[str]] = None) -> Dict[str, Any]:
        """Returns the webhook including its signing `secret` (shown once)."""
        return self._call("POST", "/v1/webhooks", self._clean({"url": url, "events": events}))

    def delete_webhook(self, webhook_id: str) -> None:
        self._call("DELETE", f"/v1/webhooks/{_q(webhook_id)}")

    def deliveries(self, webhook_id: str) -> List[Dict[str, Any]]:
        return self._call("GET", f"/v1/webhooks/{_q(webhook_id)}/deliveries")["deliveries"]

    def test_webhook(self, webhook_id: str) -> str:
        return self._call("POST", f"/v1/webhooks/{_q(webhook_id)}/test")["event_id"]


def _collect(events: Iterator[ExecEvent]) -> ExecResult:
    out, err, code = [], [], -1
    for ev in events:
        if ev.stream == "stderr":
            err.append(ev.data)
        elif ev.data:
            out.append(ev.data)
        if ev.exit_code is not None:
            code = ev.exit_code
    return ExecResult(b"".join(out), b"".join(err), code)


def verify_webhook(secret: str, signature_header: str, body: bytes, tolerance_s: int = 300,
                   now: Optional[float] = None) -> bool:
    """Check X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, f"{t}." + body))."""
    try:
        parts = dict(p.split("=", 1) for p in signature_header.split(","))
        ts = int(parts["t"])
    except (ValueError, KeyError):
        return False
    if abs((time.time() if now is None else now) - ts) > tolerance_s:
        return False
    want = hmac.new(secret.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, parts.get("v1", ""))
