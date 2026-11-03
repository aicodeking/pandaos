"""Boat API client built on urllib. Python 3.8+."""
import base64
import hashlib
import hmac
import json
import os
import secrets as _secrets
import subprocess
import tarfile
import tempfile
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
                                exit_code=124 if ev.get("timed_out") else ev.get("exit"), error=ev.get("error"))

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
                  idle_stop_s: Optional[int] = None, snapshot: Optional[str] = None,
                  environment: Optional[str] = None, no_env: Optional[bool] = None,
                  from_share: Optional[str] = None) -> VM:
        """secrets: team secret names to inject (None = all, [] = none); forks inherit it.
        environment runs that environment's setup (vm.raw["setup_cmd_id"]) unless it has a current build.
        from_share: a share code bsh_..., copied into your team first."""
        body = self._clean({"name": name, "size": size, "image": image, "ttl_s": ttl_s, "labels": labels, "env": env,
                            "secrets": None if secrets is None else list(secrets), "idle_stop_s": idle_stop_s,
                            "snapshot": snapshot, "environment": environment, "no_env": no_env, "from": from_share})
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

    def push(self, vm_id: str, directory: str, to: Optional[str] = None) -> Dict[str, Any]:
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
            self.write_file(vm_id, tmp, packed.read())
        res = self.run(vm_id, ["bash", "-c", _UNPACK, to or os.path.basename(folder), tmp])
        if res.exit_code != 0:
            raise BoatError(0, "push_failed", "unpack on the VM: " + res.stderr.decode(errors="replace").strip())
        return {"path": res.stdout.decode().strip(), "files": len(files)}

    def run_job(self, cmd: List[str], push: Optional[str] = None, cwd: Optional[str] = None,
                timeout_s: Optional[int] = None, keep: bool = False, **create: Any) -> JobResult:
        """`boat run`: new VM, optional push, run cmd, delete. create: create_vm() arguments.
        If this process dies, the VM stops by itself 15 minutes later. A timeout gives exit_code 124."""
        if push is not None and not os.path.isdir(push):  # before a VM exists
            raise ValueError(push + " is not a directory")
        create.setdefault("idle_stop_s", _JOB_IDLE_STOP_S)
        vm = self.create_vm(**create)
        start = time.monotonic()
        deleted = False
        try:
            where = cwd or (self.push(vm.id, push)["path"] if push is not None else None)
            argv = list(cmd)
            if where and not where.startswith("/"):  # relative to home, which only the VM knows
                argv = ["bash", "-c", 'cd -- "$HOME/$0" && exec "$@"', where] + argv
                where = None
            res = self.run(vm.id, argv, cwd=where, timeout_s=timeout_s or _NO_LIMIT_S)
        finally:
            if not keep:
                self.delete(vm.id)
                deleted = True
        return JobResult(res.stdout, res.stderr, res.exit_code, time.monotonic() - start, vm.size, vm.id, deleted)

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

    def follow_command(self, vm_id: str, cmd_id: str, offset: int = 0) -> Iterator[ExecEvent]:
        """Stream the output until the command ends. The last event carries exit_code."""
        return self._follow(f"/v1/vms/{_q(vm_id)}/commands/{_q(cmd_id)}/logs", offset)

    def _follow(self, path: str, offset: int) -> Iterator[ExecEvent]:
        r = self._open("GET", f"{path}?follow=1&offset={int(offset)}", timeout=None)
        with r:
            for line in r:
                if not line.strip():
                    continue
                ev = json.loads(line)
                if ev.get("reconnect"):
                    raise BoatError(409, "reconnect", "the VM stopped answering; resume from next_offset")
                yield ExecEvent(stream=ev.get("stream", ""),
                                data=base64.b64decode(ev["data_b64"]) if ev.get("data_b64") else b"",
                                exit_code=ev.get("exit"), error=ev.get("signal"))

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

    def follow_prompt(self, vm_id: str, prompt_id: str, offset: int = 0) -> Iterator[ExecEvent]:
        return self._follow(f"/v1/vms/{_q(vm_id)}/prompts/{_q(prompt_id)}/logs", offset)

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

    def update_environment(self, ref: str, **fields: Any) -> Dict[str, Any]:
        return self._call("PUT", f"/v1/environments/{_q(ref)}", fields)

    def delete_environment(self, ref: str) -> None:
        self._call("DELETE", f"/v1/environments/{_q(ref)}")

    def build_environment(self, ref: str) -> Dict[str, Any]:
        """Run the setup once and snapshot it. Poll get_environment(ref)["build"]["state"]."""
        return self._call("POST", f"/v1/environments/{_q(ref)}/build")

    # ---- resize, snapshots beyond the VM ----

    def resize(self, vm_id: str, size: Optional[str] = None, disk_gib: Optional[int] = None,
               discard_memory: Optional[bool] = None) -> VM:
        body = self._clean({"size": size, "disk_gib": disk_gib, "discard_memory": discard_memory})
        return VM.from_json(self._call("POST", f"/v1/vms/{_q(vm_id)}/resize", body))

    def team_snapshots(self) -> List[Dict[str, Any]]:
        return self._call("GET", "/v1/snapshots")["snapshots"]

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

    def rotate_key(self, key_id: str, overlap_s: Optional[int] = None) -> Dict[str, Any]:
        """New secret in result["token"]; the old one works for overlap_s more (default 24h, 0 = now)."""
        return self._call("POST", f"/v1/keys/{_q(key_id)}/rotate", self._clean({"overlap_s": overlap_s}))

    def key_usage(self, key_id: str) -> Dict[str, Any]:
        return self._call("GET", f"/v1/keys/{_q(key_id)}/usage")

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
