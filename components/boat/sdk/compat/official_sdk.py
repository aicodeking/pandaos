"""boat.dev's official Python SDK (boat-sdk, pinned in requirements.txt) against
this server's /api/v1: create, get, list, rename, commands (sync, and long ones
through the SDK's own run_command / follow_command helpers), files, find,
folders, moves, search and delete, command records, logs and processes, recover,
download URLs, hosted ports (also create's hostedPorts), ssh keys, the desktop (wait_for_desktop), stop,
resume, fork, snapshot listings, saved and named snapshots, share codes,
secrets, the AI proxy switch, webhooks, keys, environments, limits and delete,
each answer parsed by the SDK's own models (they check every id, enum and
required field). Run by ../e2e-live.sh:

    BOAT_URL=https://api.example BOAT_API_KEY=boat_… python3 official_sdk.py

BOAT_CA_FILE: CA bundle for BOAT_URL (e2e-edge.sh: Caddy's internal CA).
BOAT_LIVE_DESKTOP_NAME: name of a running sandbox with a desktop (a full
image) for wait_for_desktop; without it that step prints SKIP.
BOAT_LIVE_FAKE=1: the fake boatd of api/livescripts_test.go answers every
command with "hello", so command output is not compared.
ANTHROPIC_API_KEY (real runs only): the agent steps (prompt, prompt run,
events, conversations, steer, interrupt) run Claude Code with that key, set
through the credential-profile calls; without it they run a stub `claude` on
the sandbox's PATH, so CI needs no key.
"""
import base64
import binascii
import json
import os
import secrets
import shlex
import shutil
import ssl
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
from typing import Any, Callable, Optional, Tuple


DEFAULT_SSH_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q real-check"
GUEST_SSH_AUTHORIZE = "/usr/local/libexec/boat-ssh-authorize"


def build_ed25519_public_key(raw32: bytes, comment: str = "real-check") -> str:
    if len(raw32) != 32:
        raise ValueError("Ed25519 public key requires 32 bytes")
    key_type = b"ssh-ed25519"
    blob = struct.pack(">I", len(key_type)) + key_type + struct.pack(">I", 32) + raw32
    line = "ssh-ed25519 " + base64.b64encode(blob).decode("ascii")
    return f"{line} {comment}" if comment else line


def parse_openssh_ed25519_key(pub_key: str) -> Tuple[str, str]:
    parts = pub_key.strip().split()
    if len(parts) < 2 or parts[0] != "ssh-ed25519":
        raise ValueError("expected ssh-ed25519 OpenSSH public key")
    key_type, key_blob = parts[0], parts[1]
    try:
        raw = base64.b64decode(key_blob.encode("ascii"), validate=True)
    except (ValueError, binascii.Error) as exc:
        raise ValueError("invalid base64 SSH key blob") from exc
    if base64.b64encode(raw).decode("ascii") != key_blob:
        raise ValueError("non-canonical base64 SSH key blob")
    prefix = struct.pack(">I", len(b"ssh-ed25519")) + b"ssh-ed25519" + struct.pack(">I", 32)
    if len(raw) != len(prefix) + 32 or not raw.startswith(prefix):
        raise ValueError("invalid ssh-ed25519 wire key length or header")
    return key_type, key_blob


def generate_ssh_keypair(tmpdir: str, comment: str = "real-check") -> Tuple[Optional[str], str]:
    keygen = shutil.which("ssh-keygen")
    if keygen:
        priv_path = os.path.join(tmpdir, "id_ed25519")
        proc = subprocess.run(
            [keygen, "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", priv_path],
            capture_output=True,
            text=True,
        )
        if proc.returncode == 0 and os.path.isfile(priv_path) and os.path.isfile(priv_path + ".pub"):
            os.chmod(priv_path, 0o600)
            with open(priv_path + ".pub", encoding="utf-8") as fh:
                pub_line = fh.read().strip()
            parse_openssh_ed25519_key(pub_line)
            return priv_path, pub_line
    return None, build_ed25519_public_key(secrets.token_bytes(32), comment=comment)


def _split_host_port(spec: str, default_port: int = 22) -> Tuple[str, int]:
    spec = spec.strip()
    if spec.startswith("[") and "]" in spec:
        idx = spec.index("]")
        host = spec[1:idx]
        rest = spec[idx + 1:]
        if rest.startswith(":") and rest[1:].isdigit():
            return host, int(rest[1:])
        return host, default_port
    if spec.count(":") == 1:
        host, port_s = spec.rsplit(":", 1)
        if port_s.isdigit():
            return host, int(port_s)
    return spec, default_port


def parse_ssh_endpoint(machine_ip: Optional[str], ssh_endpoint: Optional[str]) -> Tuple[str, int, Optional[str]]:
    host = (machine_ip or "").strip()
    port = 22
    jump: Optional[str] = None
    if ssh_endpoint:
        ep = ssh_endpoint.strip()
        if " via " in ep:
            target_part, jump_part = ep.split(" via ", 1)
            jump = jump_part.strip() or None
        else:
            target_part = ep
        parsed_host, parsed_port = _split_host_port(target_part, default_port=22)
        if parsed_host:
            host = parsed_host
        port = parsed_port
    return host, port, jump


def _build_ssh_command(ssh_bin: str, priv_path: str, ssh_user: str, host: str, port: int,
                       jump: Optional[str], remote_cmd: str) -> list:
    common_opts = [
        "-i", priv_path,
        "-o", "IdentitiesOnly=yes",
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "LogLevel=ERROR",
        "-o", "ConnectTimeout=5",
    ]
    argv = [ssh_bin] + list(common_opts)
    if jump:
        j_user = None
        j_spec = jump
        if "@" in jump:
            j_user, j_spec = jump.rsplit("@", 1)
        j_host, j_port = _split_host_port(j_spec, default_port=22)
        proxy = [ssh_bin] + list(common_opts) + ["-W", "%h:%p", "-p", str(j_port)]
        if j_user:
            proxy.append(f"{j_user}@{j_host}")
        else:
            proxy.append(j_host)
        argv += ["-o", "ProxyCommand=" + " ".join(shlex.quote(x) for x in proxy)]
    argv += ["-p", str(port), f"{ssh_user}@{host}", remote_cmd]
    return argv


def verify_ssh_key_check(
    api: Any,
    sid: str,
    *,
    real: bool,
    ssh_key_request_cls: Callable[..., Any],
    run_fn: Callable[[str, str], Any],
    check_fn: Callable[[bool, str], None],
    ssh_runner: Callable[..., subprocess.CompletedProcess] = subprocess.run,
) -> Any:
    """Register an Ed25519 SSH key and verify live SSH authority when real=True."""
    with tempfile.TemporaryDirectory() as tmpdir:
        if real:
            priv_path, pub_key = generate_ssh_keypair(tmpdir, comment="real-check")
        else:
            priv_path, pub_key = None, DEFAULT_SSH_KEY
        key_type, key_blob = parse_openssh_ed25519_key(pub_key)
        key = api.ssh_key(sid, ssh_key_request_cls(key=pub_key))
        ssh_user = getattr(key, "ssh_user", None)
        machine_ip = getattr(key, "machine_ip", None)
        ssh_endpoint = getattr(key, "ssh_endpoint", None)
        check_fn(bool(ssh_user and (machine_ip or not real)), "authorize an ssh key")
        if not real:
            return key

        # Verify live AuthorizedKeysCommand lookup inside the guest (AuthorizedKeysFile is none).
        auth_cmd = (
            f"sudo -n /usr/bin/python3 -I {shlex.quote(GUEST_SSH_AUTHORIZE)} authorize "
            f"{shlex.quote(ssh_user)} {shlex.quote(key_type)} {shlex.quote(key_blob)}"
        )
        auth_res = run_fn(sid, auth_cmd)
        expected_line = f"{key_type} {key_blob}"
        check_fn(
            getattr(auth_res, "exit_code", None) == 0
            and (getattr(auth_res, "stdout", "") or "").strip() == expected_line,
            "the ssh key is authorized via live guest authority",
        )

        # Verify an ungranted Ed25519 key is denied by the live guest authority.
        _, other_blob = parse_openssh_ed25519_key(
            build_ed25519_public_key(secrets.token_bytes(32), comment="ungranted")
        )
        deny_cmd = (
            f"sudo -n /usr/bin/python3 -I {shlex.quote(GUEST_SSH_AUTHORIZE)} authorize "
            f"{shlex.quote(ssh_user)} {shlex.quote(key_type)} {shlex.quote(other_blob)}"
        )
        deny_res = run_fn(sid, deny_cmd)
        check_fn(
            getattr(deny_res, "exit_code", None) == 0
            and (getattr(deny_res, "stdout", "") or "").strip() == "",
            "ungranted ssh key is rejected by live guest authority",
        )

        # When a jump endpoint (or explicit direct SSH check) and ssh client are available,
        # also verify an end-to-end SSH login with the generated private key.
        host, port, jump = parse_ssh_endpoint(machine_ip, ssh_endpoint)
        ssh_bin = shutil.which("ssh")
        if priv_path and ssh_bin and host and (jump or os.environ.get("BOAT_LIVE_SSH_DIRECT") == "1"):
            argv = _build_ssh_command(ssh_bin, priv_path, ssh_user, host, port, jump, "echo real-check-ok")
            ok_login = False
            last_err = ""
            for _ in range(15):
                proc = ssh_runner(argv, capture_output=True, text=True, timeout=10)
                if proc.returncode == 0 and proc.stdout.strip() == "real-check-ok":
                    ok_login = True
                    break
                last_err = (proc.stderr or proc.stdout or "").strip()
                time.sleep(0.5)
            check_fn(ok_login, "live ssh login succeeded" + (f" ({last_err})" if not ok_login and last_err else ""))
        return key


def verify_sharing_unsupported(
    api: Any,
    sid: str,
    *,
    api_exception_cls: type,
    check_fn: Callable[[bool, str], None],
) -> None:
    """Team-scoped sandboxes cannot implement creator-authority share or unshare."""
    for action in ("share", "unshare"):
        try:
            getattr(api, action)(sid)
        except api_exception_cls as exc:
            if exc.status != 501:
                raise
            wire = json.loads(exc.body)
            check_fn(
                wire.get("ok") is False and wire.get("code") == "not_supported"
                and wire.get("status") == 501 and bool(wire.get("message")),
                action + " returns 501 not_supported",
            )
        else:
            raise AssertionError(action + " unexpectedly succeeded; expected 501 not_supported")


def verify_environment_delete_conflict(api, environment_id, *, api_exception_cls, check_fn):
    try:
        api.delete_environment(environment_id)
    except api_exception_cls as exc:
        if exc.status != 409:
            raise
        wire = json.loads(exc.body)
        check_fn(
            wire.get("ok") is False and wire.get("code") == "conflict"
            and wire.get("status") == 409 and bool(wire.get("message")),
            "final environment delete returns 409 conflict",
        )
    else:
        raise AssertionError("final environment delete unexpectedly succeeded")


def verify_environment_upgrade(
    api: Any,
    environment_id: Any,
    *,
    upgrade_request_cls: Callable[..., Any],
    check_fn: Callable[[bool, str], None],
    agent_ids: Optional[list] = None,
) -> Tuple[Any, Any]:
    """Exercise POST /api/v1/environments/{id}/upgrade with omitted body and explicit agentIds."""
    def _valid_counts(resp: Any) -> bool:
        live = getattr(resp, "upgraded_live", None)
        stopped = getattr(resp, "upgraded_stopped", None)
        upgraded = getattr(resp, "upgraded", None)
        failed = getattr(resp, "failed", None)
        return (
            getattr(resp, "success", None) is True
            and isinstance(upgraded, int)
            and isinstance(live, int)
            and isinstance(stopped, int)
            and upgraded == live + stopped
            and failed == 0
            and bool(getattr(resp, "latest_version_id", None))
        )

    all_res = api.upgrade_environment(environment_id)
    check_fn(_valid_counts(all_res), "upgrade environment (all pinned sandboxes)")
    req_ids = [] if agent_ids is None else list(agent_ids)
    targeted = api.upgrade_environment(
        environment_id,
        upgrade_sandbox_environment_request=upgrade_request_cls(agent_ids=req_ids),
    )
    check_fn(
        _valid_counts(targeted)
        and getattr(targeted, "latest_version_id", None) == getattr(all_res, "latest_version_id", None),
        "upgrade environment (targeted agentIds)",
    )
    return all_res, targeted


def verify_null_ttl_lifecycle(
    api: Any,
    sid: str,
    fork_name: str,
    *,
    resume_request_cls: Callable[..., Any],
    fork_request_cls: Callable[..., Any],
    check_fn: Callable[[bool, str], None],
) -> Tuple[Any, Any]:
    """Resume and fork with ttl_seconds=None (JSON ttlSeconds: null) disabling auto-stop."""
    resumed = api.resume(sid, resume_request=resume_request_cls(ttl_seconds=None))
    rsb = getattr(resumed, "sandbox", None)
    check_fn(
        getattr(rsb, "id", None) == sid
        and getattr(rsb, "ttl_seconds", "unset") is None
        and getattr(rsb, "archive_after", "unset") is None,
        "resume with ttl_seconds=None",
    )
    fork = api.fork(sid, fork_request=fork_request_cls(name=fork_name, ttl_seconds=None))
    fsb = getattr(fork, "sandbox", None)
    kid = getattr(fsb, "id", "") or ""
    check_fn(
        kid.startswith("bx_")
        and kid != sid
        and getattr(fsb, "ttl_seconds", "unset") is None
        and getattr(fsb, "archive_after", "unset") is None,
        "fork " + kid + " with ttl_seconds=None",
    )
    return resumed, fork


def run_driver() -> None:
    from boat_sdk import ApiClient, BoatApi, Configuration
    from boat_sdk.boat_helpers import follow_command, run_command, wait_for_desktop
    from boat_sdk.exceptions import ApiException
    from boat_sdk.models.command_request import CommandRequest
    from boat_sdk.models.create_sandbox_environment_request import CreateSandboxEnvironmentRequest
    from boat_sdk.models.create_sandbox_request import CreateSandboxRequest
    from boat_sdk.models.create_scoped_api_key_request import CreateScopedApiKeyRequest
    from boat_sdk.models.file_folder_request import FileFolderRequest
    from boat_sdk.models.file_move_request import FileMoveRequest
    from boat_sdk.models.file_permissions_request import FilePermissionsRequest
    from boat_sdk.models.file_replace_request import FileReplaceRequest
    from boat_sdk.models.file_url_request import FileUrlRequest
    from boat_sdk.models.file_write_request import FileWriteRequest
    from boat_sdk.models.fork_request import ForkRequest
    from boat_sdk.models.host_port_request import HostPortRequest
    from boat_sdk.models.named_snapshot_save_request import NamedSnapshotSaveRequest
    from boat_sdk.models.prompt_request import PromptRequest
    from boat_sdk.models.resume_request import ResumeRequest
    from boat_sdk.models.saved_snapshot_save_request import SavedSnapshotSaveRequest
    from boat_sdk.models.secrets_update_request import SecretsUpdateRequest
    from boat_sdk.models.set_credential_key_request import SetCredentialKeyRequest
    from boat_sdk.models.set_environment_var_request import SetEnvironmentVarRequest
    from boat_sdk.models.set_sandbox_ai_access_request import SetSandboxAiAccessRequest
    from boat_sdk.models.ssh_key_request import SshKeyRequest
    from boat_sdk.models.steer_request import SteerRequest
    from boat_sdk.models.telemetry_set_request import TelemetrySetRequest
    from boat_sdk.models.update_ai_proxy_request import UpdateAiProxyRequest
    from boat_sdk.models.update_sandbox_environment_request import UpdateSandboxEnvironmentRequest
    from boat_sdk.models.update_sandbox_request import UpdateSandboxRequest
    from boat_sdk.models.upgrade_sandbox_environment_request import UpgradeSandboxEnvironmentRequest
    from boat_sdk.models.webhook_create_request import WebhookCreateRequest
    from boat_sdk.models.webhook_update_request import WebhookUpdateRequest

    REAL = not os.environ.get("BOAT_LIVE_FAKE")
    base = os.environ["BOAT_URL"].rstrip("/") + "/api/v1"
    CA = os.environ.get("BOAT_CA_FILE") or None

    def client(token):
        return BoatApi(ApiClient(Configuration(host=base, access_token=token, ssl_ca_cert=CA)))

    api = client(os.environ["BOAT_API_KEY"])
    print("official python SDK: " + base)
    prefix = os.environ.get("BOAT_LIVE_PREFIX", "sdk-live") + "-off-py-" + secrets.token_hex(3)
    made = []

    def check(ok, what):
        if not ok:
            print("official python SDK: FAIL: " + what, file=sys.stderr)
            sys.exit(1)
        print("ok - " + what)

    def run(sid, cmd):
        return api.command(sid, CommandRequest(command=cmd)).actual_instance

    def ready(sid):
        for _ in range(150):
            sb = api.get(sid).sandbox
            if sb.state in ("ready", "idle", "running"):
                return sb
            time.sleep(1)
        check(False, "sandbox %s became ready" % sid)

    def delete(sid):
        return api.delete_sandbox(x_ascii_confirm_delete=sid, sandbox_id=sid).operation

    LLM_KEY = os.environ.get("ANTHROPIC_API_KEY") if REAL else None
    # A stand-in for Claude Code: a prompt with sleep-forever keeps running (to
    # steer and interrupt); any other answers at once (as the fake boatd does).
    STUB_AGENT = """#!/bin/bash
case "$*" in *sleep-forever*) exec sleep 300 ;; esac
echo '{"type":"result","result":"pong","session_id":"stub-session"}'
"""

    def prompt_done(sid, pid):
        for _ in range(300 if LLM_KEY else 60):
            run_ = api.prompt_run_status(sid, pid).prompt_run
            if run_.done:
                return run_
            time.sleep(1)
        check(False, "prompt %s finished" % pid)

    def agent_steps(sid):
        """prompt, prompt run, events, conversations, steer, interrupt; and the
        credential-profile calls that hold the key."""
        prof = api.list_credential_profiles().profiles
        check(len(prof) == 1 and prof[0].name == "default" and prof[0].is_default, "credential profiles: the team's default")
        if LLM_KEY:
            p = api.set_credential_profile_key("default", "anthropic-api-key", SetCredentialKeyRequest(value=LLM_KEY)).profile
            check("anthropic-api-key" in p.connected, "credential profile key set")
            acc = api.list_credential_accounts("default", "anthropic-api-key").accounts
            check(len(acc) == 1 and acc[0].active and acc[0].label.endswith(LLM_KEY[-4:]), "credential accounts")
            print("agent steps: Claude Code with ANTHROPIC_API_KEY")
        else:
            api.write_file(sid, FileWriteRequest(path=".local/bin/claude", content=STUB_AGENT))
            api.set_file_permissions(sid, FilePermissionsRequest(path=".local/bin/claude", mode="755"))
            print("agent steps: stub claude (no ANTHROPIC_API_KEY)")
        try:
            first = api.prompt(sid, PromptRequest(provider="claude-code", prompt="Reply with the single word pong.", new=True))
            check(first.prompt_id and first.conversation_id, "prompt (%s)" % first.status)
            done = prompt_done(sid, first.prompt_id)
            check(done.status == "finished" or not REAL, "prompt run finishes (%s)" % done.status)
            evs = api.events(sid, conversation=first.conversation_id).events
            kinds = [e.type for e in evs]
            check("prompt" in kinds and "response" in kinds, "events: prompt and response (%s)" % kinds)
            slow_text = "Run the shell command `sleep 300` and wait for it." if LLM_KEY else "sleep-forever on it"
            slow = api.prompt(sid, PromptRequest(provider="claude-code", prompt=slow_text, new=True))
            time.sleep(8 if LLM_KEY else 1)
            st = api.steer(sid, SteerRequest(message="Stop and reply with the word done.", conversation=slow.conversation_id))
            check(st.conversation_id == slow.conversation_id, "steer (%s, %s)" % (st.status, st.mode))
            victim = api.prompt(sid, PromptRequest(provider="claude-code", prompt=slow_text, new=True))
            time.sleep(8 if LLM_KEY else 1)
            intr = api.interrupt(sid, conversation=victim.conversation_id)
            check(intr.conversation_id == victim.conversation_id, "interrupt (%s)" % intr.status)
            convs = api.conversations(sid).conversations
            ids = {c.id for c in convs}
            check({first.conversation_id, slow.conversation_id, victim.conversation_id} <= ids, "conversations (%d)" % len(convs))
        finally:
            if LLM_KEY:
                p = api.delete_credential_profile_key("default", "anthropic-api-key").profile
                check("anthropic-api-key" not in p.connected, "credential profile key removed")

    try:
        created = api.create(create_sandbox_request=CreateSandboxRequest(
            name=prefix, ttl_seconds=3600, labels={"suite": "official"},
            hosted_ports=[HostPortRequest(port=8090), HostPortRequest(port=8091, public=True)]),
            idempotency_key=secrets.token_hex(8))
        sid = created.sandbox.id
        made.append(sid)
        check(sid.startswith("bx_") and created.sandbox.name == prefix, "create " + sid)
        hp = created.hosted_ports or []
        check(len(hp) == 2 and hp[0].port == 8090 and hp[0].is_protected and hp[0].url and hp[1].access == "public"
              and not hp[0].error, "create with hostedPorts (%s)" % hp)
        sb = ready(sid)
        check(sb.archive_after is not None and sb.ttl_seconds == 3600, "archiveAfter and ttlSeconds")
        listed = api.sandboxes(name_prefix=prefix).sandboxes
        check([s.id for s in listed] == [sid], "list by name prefix")

        try:
            upd = api.update(sid, UpdateSandboxRequest(name=prefix + "-r", ttl_seconds=7200)).sandbox
            check(upd.name == prefix + "-r" and upd.ttl_seconds == 7200, "rename and new ttlSeconds (PATCH)")
        except ApiException as e:
            if e.status != 501:
                raise
            print("SKIP - PATCH: this boatd has no PATCH /v1/vms/{id} (501)")

        res = run(sid, "echo boat-compat; echo err >&2; exit 3")
        check(res.exit_code == 3 or not REAL, "command exit code (%r)" % res.exit_code)
        if REAL:
            check(res.stdout == "boat-compat\n" and res.stderr == "err\n", "command output")
        # Long commands: the SDK's run_command (detached + the attach stream) and follow_command.
        long_cmd = "echo out-1; echo err-1 >&2; sleep 2; echo out-2; exit 4"
        frames = list(run_command(api, sid, long_cmd))
        check(frames[0]["type"] == "started" and frames[0]["processId"] >= 1, "run_command started (%r)" % frames[0])
        pid = frames[0]["processId"]
        out = "".join(f["data"] for f in frames if f["type"] in ("stdout", "stderr"))
        check(frames[-1]["type"] == "exit", "run_command ends with exit (%r)" % frames[-1])
        if REAL:
            check(out == "out-1\nerr-1\nout-2\n" and frames[-1]["exitCode"] == 4, "run_command output and exit code (%r %r)" % (out, frames[-1]))
        again = list(follow_command(api, sid, pid))
        check("".join(f.get("data", "") for f in again) == out and again[-1]["type"] == "exit", "follow_command replays the whole output")
        gen = run_command(api, sid, "sleep 300")
        det = next(gen)
        gen.close()  # leaves the command running, as the SDK documents
        check(det["processId"] >= 1, "detached command (run_command)")
        st = api.command_status(sid, det["processId"])
        check(st.running or not REAL, "detached command runs")
        hist = api.command_history(sid).commands
        check(any(c.process_id == det["processId"] for c in hist), "command history")
        check(api.stop_command(sid, det["processId"]).signaled, "stop a detached command")
        rec = next(c for c in hist if c.process_id == pid)
        check(api.get_command(sid, rec.command_id).command.command_id == rec.command_id, "get one command record")
        logs = api.command_logs(sid, rec.command_id, stream="stdout")
        check(logs.command_id == rec.command_id and (not REAL or ("out-1\n" in logs.data and "out-2\n" in logs.data and logs.done)), "command logs (%r)" % logs.data)
        procs = api.processes(sid)
        check(procs.total >= len(procs.processes) and (not REAL or (procs.live and procs.processes)), "processes (%d)" % procs.total)

        agent_steps(sid)

        payload = "hello from the official SDK " + secrets.token_hex(4)
        api.write_file(sid, FileWriteRequest(path="compat/a.txt", content=payload))
        got = api.read_file(sid, path="compat/a.txt")
        check(got.content == payload or not REAL, "write and read a file")
        names = [f.name for f in api.list_files(sid, path="compat").files]
        check("a.txt" in names, "list files")
        check(api.get_file_info(sid, path="compat/a.txt").size == len(payload) or not REAL, "file info")
        found = api.find_in_files(sid, pattern=payload[-8:], path="compat")
        check(not REAL or any(m.file.endswith("compat/a.txt") and m.line == 1 for m in found.matches), "find in files")
        dl = api.create_download_url(sid, FileUrlRequest(path="compat/a.txt", ttl_seconds=120))
        check(dl.method == "GET" and dl.url.startswith(os.environ["BOAT_URL"].rstrip("/") + "/api/v1/"), "download URL " + dl.url.split("/file-downloads/")[0])
        if REAL:
            with urllib.request.urlopen(dl.url, context=ssl.create_default_context(cafile=CA), timeout=30) as resp:
                check(resp.read().decode() == payload, "the download URL serves the file with no API key")
        rep = api.replace_in_files(sid, FileReplaceRequest(paths=["compat/a.txt"], pattern="official", new_value="OFFICIAL"))
        check(len(rep.results) == 1 and rep.results[0].success, "replace in files")
        if REAL:
            payload = payload.replace("official", "OFFICIAL")
            check(api.read_file(sid, path="compat/a.txt").content == payload, "the replace changed the file")
        art = api.artifact(sid, path="compat/a.txt")
        check(art is not None and (not REAL or bytes(art).decode() == payload), "artifact")
        up = api.create_upload_url(sid, FileUrlRequest(path="compat/up.txt", ttl_seconds=120))
        check(up.method == "PUT" and "/api/v1/file-uploads/" in up.url, "upload URL")
        if REAL:
            req = urllib.request.Request(up.url, data=b"uploaded", method="PUT")
            with urllib.request.urlopen(req, context=ssl.create_default_context(cafile=CA), timeout=30) as resp:
                check(resp.status == 200, "the upload URL takes a PUT with no API key")
            check(api.read_file(sid, path="compat/up.txt").content == "uploaded", "the uploaded file")
        api.create_folder(sid, FileFolderRequest(path="compat/dir/sub"))
        api.write_file(sid, FileWriteRequest(path="compat/m.txt", content="move me"))
        mv = api.move_file(sid, FileMoveRequest(source="compat/m.txt", destination="compat/dir/sub/m.txt"))
        check(mv.destination.endswith("compat/dir/sub/m.txt"), "create a folder and move a file into it")
        hits = api.search_files(sid, pattern="m.txt", path="compat")
        check(not REAL or any(f.endswith("compat/dir/sub/m.txt") for f in hits.files), "search files (%r)" % hits.files)
        api.delete_file(sid, path="compat/dir", recursive=True)
        check(not REAL or "dir" not in [f.name for f in api.list_files(sid, path="compat").files], "delete a folder")
        check(api.get_sandbox_ai_usage(sid, days=7).sandbox_id == sid, "sandbox AI usage")
        acc = api.set_sandbox_ai_access(sid, SetSandboxAiAccessRequest(blocked=False))
        check(acc.sandbox_id == sid and acc.blocked is False, "sandbox AI access")
        check(api.get_ai_usage(days=7).totals is not None, "account AI usage")
        if api.get_telemetry().telemetry is None:  # never overwrite a team's real export
            tel = api.set_telemetry(TelemetrySetRequest(endpoint="https://otel.invalid", headers={"X-Probe": "1"}))
            check(tel.telemetry.endpoint == "https://otel.invalid", "set telemetry")
            check(api.delete_telemetry().telemetry is None, "delete telemetry")
        else:
            print("SKIP - telemetry: the team has an export configured")

        hosted = api.host_port(sid, HostPortRequest(port=8080))
        check(hosted.access == "private" and hosted.is_protected and "8080-" in hosted.url, "host a private port")
        check(api.host_port(sid, HostPortRequest(port=8081, public=True)).access == "public", "host a public port")
        verify_sharing_unsupported(api, sid, api_exception_cls=ApiException, check_fn=check)
        verify_ssh_key_check(
            api,
            sid,
            real=REAL,
            ssh_key_request_cls=SshKeyRequest,
            run_fn=run,
            check_fn=check,
        )
        desk_name = os.environ.get("BOAT_LIVE_DESKTOP_NAME")
        if desk_name:
            desk = next(x for x in api.sandboxes(name_prefix=desk_name).sandboxes if x.name == desk_name)
            d = wait_for_desktop(api, desk.id, timeout_seconds=120)
            check(d.desktop_url and not d.provisioning, "wait_for_desktop " + d.desktop_url.split("?")[0])
        else:
            print("SKIP - desktop: BOAT_LIVE_DESKTOP_NAME is not set")

        stopped = api.stop(sid)
        op = stopped.sandbox.stop
        check(stopped.sandbox.state == "archived" and op.status == "completed", "stop")
        check(api.get_stop(sid, op.id).stop.status == "completed", "stop operation")
        check(api.get(sid).sandbox.stop.id == op.id, "sandbox.stop")
        resumed = api.resume(sid, resume_request=ResumeRequest(ttl_seconds=None))
        check(
            resumed.sandbox.id == sid
            and resumed.sandbox.ttl_seconds is None
            and resumed.sandbox.archive_after is None,
            "resume with ttl_seconds=None",
        )
        ready(sid)
        if REAL:
            check(api.read_file(sid, path="compat/a.txt").content == payload, "the file survived stop and resume")

        fork = api.fork(sid, fork_request=ForkRequest(name=prefix + "-fork", ttl_seconds=None))
        kid = fork.sandbox.id
        made.append(kid)
        check(
            kid.startswith("bx_")
            and kid != sid
            and fork.sandbox.ttl_seconds is None
            and fork.sandbox.archive_after is None,
            "fork " + kid + " with ttl_seconds=None",
        )
        ready(kid)
        if REAL:
            check(api.read_file(kid, path="compat/a.txt").content == payload, "the fork has the parent's file")
        check(api.recover(kid).status == "recovering", "recover (power-cycle) the fork")
        ready(kid)

        snap = api.save_saved_snapshot(SavedSnapshotSaveRequest(sandbox_id=sid, name=prefix.replace("_", "-")[:60] + "-s"))
        check(snap.snapshot.source_sandbox_id == sid, "save a snapshot")
        check(any(s.name == snap.snapshot.name for s in api.list_saved_snapshots().snapshots), "list saved snapshots")
        check(api.get_saved_snapshot(snap.snapshot.name).snapshot.name == snap.snapshot.name, "get a saved snapshot")
        share = api.create_snapshot_share(snap.snapshot.name)
        check(share.share_code.startswith("bsh_") and share.share.status == "active", "share code")
        check([x.id for x in api.list_snapshot_shares(snap.snapshot.name).shares] == [share.share.id], "list share codes")
        check(api.revoke_snapshot_share(snap.snapshot.name, share.share.id).share.status == "revoked", "revoke a share code")
        api.delete_saved_snapshot(snap.snapshot.name)
        nname = prefix.replace("_", "-")[:60] + "-n"
        check(api.save_named_snapshot(NamedSnapshotSaveRequest(sandbox_id=sid, name=nname)).snapshot.name == nname, "save a named snapshot")
        check(api.get_named_snapshot(nname).snapshot.source_sandbox_id == sid, "get a named snapshot")
        check(any(x.name == nname for x in api.list_named_snapshots().snapshots), "list named snapshots")
        mine = api.list_sandbox_snapshots(sid).snapshots
        check(mine and all(str(x.sandbox_id) == sid for x in mine), "list the sandbox's snapshots (%d)" % len(mine))
        check(api.get_latest_sandbox_snapshot(sid).snapshot is not None, "latest snapshot")
        down = api.get_snapshot_download(mine[0].id)
        check(str(down.snapshot_id) == str(mine[0].id) and down.sandbox_id == sid and down.expires_in_seconds > 0, "snapshot download (%d chunks)" % len(down.chunks))
        check({str(x.id) for x in mine} <= {str(x.id) for x in api.list_snapshots(limit=200).snapshots}, "the team's snapshots")
        api.delete_named_snapshot(nname)

        before = api.secrets().env_contents
        sec = api.update_secrets(SecretsUpdateRequest(env_contents=before + "OFFICIAL_SDK_SECRET=v-" + secrets.token_hex(4) + "\n"))
        check("OFFICIAL_SDK_SECRET=\n" in sec.env_contents and "v-" not in sec.env_contents, "set a default-injected secret (value never returned)")
        api.update_secrets(SecretsUpdateRequest(env_contents=before))
        check("OFFICIAL_SDK_SECRET" not in api.secrets().env_contents, "unmark it")
        proxy = api.get_ai_proxy()
        check(api.update_ai_proxy(UpdateAiProxyRequest(enabled=proxy.enabled)).enabled == proxy.enabled, "AI proxy switch")

        hook = api.create_webhook(WebhookCreateRequest(name="official", url="https://example.invalid/h", events=["sandbox.ready"]))
        check(hook.secret.startswith("whsec_"), "create a webhook")
        api.update_webhook(hook.webhook.id, WebhookUpdateRequest(events=["sandbox.archived"]))
        check(api.rotate_webhook_signing_secret(hook.webhook.id).secret != hook.secret, "rotate its secret")
        check(api.get_webhook(hook.webhook.id).webhook.events == ["sandbox.archived"], "get a webhook")
        check(any(h.id == hook.webhook.id for h in api.list_webhooks().webhooks), "list webhooks")
        api.delete_webhook(hook.webhook.id)

        key = api.create_scoped_api_key(CreateScopedApiKeyRequest(name=prefix[-30:], preset="read-only"))
        check(key.api_key.scope.actions == ["vms:read"] and key.secret.startswith("boat_"), "create a scoped key")
        reader = client(key.secret)
        check(any(x.id == sid for x in reader.sandboxes().sandboxes), "the read-only key lists sandboxes")
        check(api.rotate_api_key(key.api_key.id).secret != key.secret, "rotate the key")
        check(api.api_key_usage(key.api_key.id).id == key.api_key.id, "key usage")
        check(all(k.id != key.api_key.id for k in api.revoke_api_key(key.api_key.id).api_keys), "revoke the key")

        envs = api.create_environment(CreateSandboxEnvironmentRequest(name=prefix + "-env")).environments
        env = next(e for e in envs if e.name == prefix + "-env")
        upd = api.update_environment(str(env.id), UpdateSandboxEnvironmentRequest(env_contents="GREETING=hi\n")).environment
        check(upd.env_contents == "GREETING=hi\n" and upd.id == env.id, "environment variables")
        check(api.set_environment_var(env.id, "PROBE", SetEnvironmentVarRequest(value="1")).success, "set one environment variable")
        check(api.delete_environment_var(env.id, "PROBE").success, "delete one environment variable")
        check(any(e.id == env.id for e in api.environments().environments), "list environments")
        verify_environment_upgrade(
            api,
            env.id,
            upgrade_request_cls=UpgradeSandboxEnvironmentRequest,
            check_fn=check,
        )
        # An empty account must retain one environment. Existing accounts already
        # have another; only the isolated fixture needs a retained spare.
        if len(envs) == 1:
            verify_environment_delete_conflict(api, str(env.id), api_exception_cls=ApiException, check_fn=check)
            api.create_environment(CreateSandboxEnvironmentRequest(name=prefix + "-spare"))
        deleted_env = api.delete_environment(str(env.id))
        check(deleted_env.success and deleted_env.environment.id == env.id, "delete a non-final environment")
        check(all(e.id != env.id for e in api.environments().environments), "deleted environment is no longer listed")

        lim = api.limits()
        check(lim.active_sandboxes >= 2, "limits")

        n2 = prefix.replace("_", "-")[:60] + "-k"
        api.save_named_snapshot(NamedSnapshotSaveRequest(sandbox_id=kid, name=n2))
        latest = api.get_latest_sandbox_snapshot(kid).snapshot
        op = api.delete_snapshot(latest.id, x_ascii_confirm_delete=str(latest.id)).operation
        check(op.target_id == str(latest.id) and op.kind == "snapshot", "delete one snapshot")
        try:
            api.delete_named_snapshot(n2)
        except ApiException as e:
            if e.status != 404:  # gone already when it was the latest
                raise
        caps = [x for x in api.list_sandbox_snapshots(kid).snapshots]
        gone = api.delete_sandbox_snapshots(kid, x_ascii_confirm_delete=kid).operations
        check(len(gone) <= len(caps) and all(o.kind == "snapshot" for o in gone), "delete the fork's snapshots (%d)" % len(gone))

        for x in list(made):
            op = delete(x)
            check(op.status == "completed" and op.target_id == x, "delete " + x)
            check(api.get_deletion_operation(op.id).operation.target_id == x, "deletion operation")
            made.remove(x)
        try:
            api.get(sid)
            check(False, "a deleted sandbox is gone")
        except ApiException as e:
            check(e.status == 404, "a deleted sandbox is 404")
    finally:
        for x in made:
            try:
                delete(x)
            except Exception as e:  # cleanup only
                print("cleanup %s: %s" % (x, e), file=sys.stderr)
    print("official python SDK: ok")


def _self_test() -> None:
    key_type, key_blob = parse_openssh_ed25519_key(DEFAULT_SSH_KEY)
    assert key_type == "ssh-ed25519" and key_blob

    class _Req:
        def __init__(self, key: str):
            self.key = key

    class _Resp:
        def __init__(self, ssh_user: str, machine_ip: Optional[str], ssh_endpoint: Optional[str]):
            self.ssh_user = ssh_user
            self.machine_ip = machine_ip
            self.ssh_endpoint = ssh_endpoint

    class _CmdRes:
        def __init__(self, exit_code: int, stdout: str):
            self.exit_code = exit_code
            self.stdout = stdout

    class _FakeApi:
        def __init__(self, endpoint: Optional[str] = None):
            self.endpoint = endpoint
            self.authorized = set()

        def read_file(self, *args, **kwargs):
            raise AssertionError("authorized_keys file must never be read")

        def ssh_key(self, sid: str, req: _Req):
            kt, kb = parse_openssh_ed25519_key(req.key)
            self.authorized.add((sid, kt, kb))
            return _Resp("dev", "10.200.0.2", self.endpoint)

    checks = []
    fake_mock = _FakeApi()
    verify_ssh_key_check(
        fake_mock,
        "bx_mock",
        real=False,
        ssh_key_request_cls=_Req,
        run_fn=lambda sid, cmd: (_ for _ in ()).throw(AssertionError("run_fn not used in mock mode")),
        check_fn=lambda ok, what: (checks.append((ok, what)), None if ok else (_ for _ in ()).throw(AssertionError(what))),
    )
    assert len(checks) == 1 and checks[0][0] is True

    fake_real = _FakeApi(endpoint="10.200.0.2:22 via jump@localhost:2222")
    ssh_calls = []

    def fake_run(sid: str, cmd: str):
        tokens = shlex.split(cmd)
        user, kt, kb = tokens[-3], tokens[-2], tokens[-1]
        assert user == "dev"
        if (sid, kt, kb) in fake_real.authorized:
            return _CmdRes(0, f"{kt} {kb}\n")
        return _CmdRes(0, "")

    def fake_ssh(argv, **kw):
        ssh_calls.append(argv)
        return subprocess.CompletedProcess(argv, 0, stdout="real-check-ok\n", stderr="")

    verify_ssh_key_check(
        fake_real,
        "bx_real",
        real=True,
        ssh_key_request_cls=_Req,
        run_fn=fake_run,
        check_fn=lambda ok, what: None if ok else (_ for _ in ()).throw(AssertionError(what)),
        ssh_runner=fake_ssh,
    )
    assert len(ssh_calls) == 1

    class _SandboxObj:
        def __init__(self, sid: str, ttl_seconds: Optional[int], archive_after: Optional[str]):
            self.id = sid
            self.ttl_seconds = ttl_seconds
            self.archive_after = archive_after

    class _ApiException(Exception):
        def __init__(self, status=501, code="not_supported"):
            self.status = status
            self.body = json.dumps({"ok": False, "status": status, "code": code, "message": "team-scoped access"})

    class _ActionResp:
        def __init__(self, sandbox: _SandboxObj):
            self.sandbox = sandbox

    class _ResumeReq:
        def __init__(self, **kwargs):
            self.model_fields_set = set(kwargs.keys())
            self.ttl_seconds = kwargs.get("ttl_seconds")

    class _ForkReq:
        def __init__(self, **kwargs):
            self.model_fields_set = set(kwargs.keys())
            self.name = kwargs.get("name")
            self.ttl_seconds = kwargs.get("ttl_seconds")

    class _FakeLifecycleApi:
        def __init__(self):
            self.calls = []

        def share(self, sid: str):
            self.calls.append(("share", sid))
            raise _ApiException()

        def unshare(self, sid: str):
            self.calls.append(("unshare", sid))
            raise _ApiException()

        def resume(self, sid: str, resume_request: _ResumeReq):
            assert "ttl_seconds" in resume_request.model_fields_set and resume_request.ttl_seconds is None
            self.calls.append(("resume", sid, resume_request.ttl_seconds))
            return _ActionResp(_SandboxObj(sid, None, None))

        def fork(self, sid: str, fork_request: _ForkReq):
            assert "ttl_seconds" in fork_request.model_fields_set and fork_request.ttl_seconds is None
            self.calls.append(("fork", sid, fork_request.name, fork_request.ttl_seconds))
            return _ActionResp(_SandboxObj("bx_fork2345", None, None))

    lf_api = _FakeLifecycleApi()
    lf_checks = []
    verify_sharing_unsupported(
        lf_api,
        "bx_parent23",
        api_exception_cls=_ApiException,
        check_fn=lambda ok, what: (lf_checks.append((ok, what)), None if ok else (_ for _ in ()).throw(AssertionError(what))),
    )
    verify_null_ttl_lifecycle(
        lf_api,
        "bx_parent23",
        "fork-null-ttl",
        resume_request_cls=_ResumeReq,
        fork_request_cls=_ForkReq,
        check_fn=lambda ok, what: (lf_checks.append((ok, what)), None if ok else (_ for _ in ()).throw(AssertionError(what))),
    )
    assert len(lf_checks) == 4 and all(ok for ok, _ in lf_checks)
    assert lf_api.calls == [
        ("share", "bx_parent23"),
        ("unshare", "bx_parent23"),
        ("resume", "bx_parent23", None),
        ("fork", "bx_parent23", "fork-null-ttl", None),
    ]
    def strict_check(ok, what):
        assert ok, what

    for unexpected in (_ApiException(403), _ApiException(501, "unexpected"), RuntimeError("transport failed"), None):
        class _UnexpectedApi:
            def share(self, sid):
                if unexpected is not None:
                    raise unexpected
                return {"sandbox": {"id": sid}}
        try:
            verify_sharing_unsupported(_UnexpectedApi(), "bx_parent23", api_exception_cls=_ApiException, check_fn=strict_check)
        except (AssertionError, _ApiException, RuntimeError):
            pass
        else:
            raise AssertionError("unexpected sharing response was accepted")

    class _EnvironmentApi:
        def __init__(self, error):
            self.error = error

        def delete_environment(self, environment_id):
            assert environment_id == "env-test"
            if self.error is not None:
                raise self.error

    verify_environment_delete_conflict(_EnvironmentApi(_ApiException(409, "conflict")), "env-test", api_exception_cls=_ApiException, check_fn=strict_check)
    for unexpected in (_ApiException(403), _ApiException(409, "unexpected"), RuntimeError("transport failed"), None):
        try:
            verify_environment_delete_conflict(_EnvironmentApi(unexpected), "env-test", api_exception_cls=_ApiException, check_fn=strict_check)
        except (AssertionError, _ApiException, RuntimeError):
            pass
        else:
            raise AssertionError("unexpected environment deletion response was accepted")

    class _UpgradeReq:
        def __init__(self, agent_ids=None):
            self.agent_ids = agent_ids

    class _UpgradeResp:
        def __init__(self, *, success=True, upgraded=0, upgraded_live=0, upgraded_stopped=0, failed=0,
                     latest_version_id="11111111-2222-3333-4444-555555555555"):
            self.success = success
            self.upgraded = upgraded
            self.upgraded_live = upgraded_live
            self.upgraded_stopped = upgraded_stopped
            self.failed = failed
            self.latest_version_id = latest_version_id

    class _FakeUpgradeApi:
        def __init__(self, resp_all=None, resp_targeted=None):
            self.calls = []
            self.resp_all = resp_all or _UpgradeResp()
            self.resp_targeted = resp_targeted or _UpgradeResp()

        def upgrade_environment(self, environment_id, upgrade_sandbox_environment_request=None):
            if upgrade_sandbox_environment_request is None:
                self.calls.append((environment_id, None))
                return self.resp_all
            self.calls.append((environment_id, list(upgrade_sandbox_environment_request.agent_ids)))
            return self.resp_targeted

    up_api = _FakeUpgradeApi()
    verify_environment_upgrade(up_api, "env-uuid-1", upgrade_request_cls=_UpgradeReq, check_fn=strict_check)
    assert up_api.calls == [("env-uuid-1", None), ("env-uuid-1", [])]

    up_nonzero = _FakeUpgradeApi(
        resp_all=_UpgradeResp(upgraded=3, upgraded_live=2, upgraded_stopped=1),
        resp_targeted=_UpgradeResp(upgraded=1, upgraded_live=1, upgraded_stopped=0),
    )
    verify_environment_upgrade(
        up_nonzero, "env-uuid-1", upgrade_request_cls=_UpgradeReq, check_fn=strict_check, agent_ids=["bx_parent23"]
    )
    assert up_nonzero.calls == [("env-uuid-1", None), ("env-uuid-1", ["bx_parent23"])]

    for bad_resp in (
        _UpgradeResp(success=False, failed=1),
        _UpgradeResp(failed=1),
        _UpgradeResp(latest_version_id=None),
        _UpgradeResp(upgraded=2, upgraded_live=1, upgraded_stopped=0),
    ):
        try:
            verify_environment_upgrade(
                _FakeUpgradeApi(resp_all=bad_resp),
                "env-uuid-1",
                upgrade_request_cls=_UpgradeReq,
                check_fn=strict_check,
            )
        except AssertionError:
            pass
        else:
            raise AssertionError("invalid environment upgrade response was accepted")
    print("official_sdk self-test: ok")


if __name__ == "__main__":
    if "--self-test" in sys.argv[1:]:
        _self_test()
    else:
        run_driver()
