"""boat.dev's official Python SDK (boat-sdk, pinned in requirements.txt) against
this server's /api/v1: create, get, list, rename, commands (sync, and long ones
through the SDK's own run_command / follow_command helpers), files, find,
folders, moves, search and delete, command records, logs and processes, recover,
download URLs, hosted ports, ssh keys, the desktop (wait_for_desktop), stop,
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
import os
import secrets
import ssl
import sys
import time
import urllib.request

from boat_sdk import ApiClient, BoatApi, Configuration
from boat_sdk.boat_helpers import follow_command, run_command, wait_for_desktop
from boat_sdk.exceptions import ApiException
from boat_sdk.models.command_request import CommandRequest
from boat_sdk.models.create_sandbox_environment_request import CreateSandboxEnvironmentRequest
from boat_sdk.models.create_sandbox_request import CreateSandboxRequest
from boat_sdk.models.create_scoped_api_key_request import CreateScopedApiKeyRequest
from boat_sdk.models.file_replace_request import FileReplaceRequest
from boat_sdk.models.file_url_request import FileUrlRequest
from boat_sdk.models.file_write_request import FileWriteRequest
from boat_sdk.models.fork_request import ForkRequest
from boat_sdk.models.host_port_request import HostPortRequest
from boat_sdk.models.named_snapshot_save_request import NamedSnapshotSaveRequest
from boat_sdk.models.secrets_update_request import SecretsUpdateRequest
from boat_sdk.models.set_environment_var_request import SetEnvironmentVarRequest
from boat_sdk.models.set_sandbox_ai_access_request import SetSandboxAiAccessRequest
from boat_sdk.models.ssh_key_request import SshKeyRequest
from boat_sdk.models.telemetry_set_request import TelemetrySetRequest
from boat_sdk.models.update_ai_proxy_request import UpdateAiProxyRequest
from boat_sdk.models.saved_snapshot_save_request import SavedSnapshotSaveRequest
from boat_sdk.models.file_permissions_request import FilePermissionsRequest
from boat_sdk.models.file_folder_request import FileFolderRequest
from boat_sdk.models.file_move_request import FileMoveRequest
from boat_sdk.models.prompt_request import PromptRequest
from boat_sdk.models.set_credential_key_request import SetCredentialKeyRequest
from boat_sdk.models.steer_request import SteerRequest
from boat_sdk.models.update_sandbox_environment_request import UpdateSandboxEnvironmentRequest
from boat_sdk.models.update_sandbox_request import UpdateSandboxRequest
from boat_sdk.models.webhook_create_request import WebhookCreateRequest
from boat_sdk.models.webhook_update_request import WebhookUpdateRequest

REAL = not os.environ.get("BOAT_LIVE_FAKE")
base = os.environ["BOAT_URL"].rstrip("/") + "/api/v1"
CA = os.environ.get("BOAT_CA_FILE") or None


def client(token):
    return BoatApi(ApiClient(Configuration(host=base, access_token=token, ssl_ca_cert=CA)))


api = client(os.environ["BOAT_API_KEY"])
print("official python SDK: " + base)
SSH_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q official-sdk"
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
    created = api.create(create_sandbox_request=CreateSandboxRequest(name=prefix, ttl_seconds=3600, labels={"suite": "official"}),
                         idempotency_key=secrets.token_hex(8))
    sid = created.sandbox.id
    made.append(sid)
    check(sid.startswith("bx_") and created.sandbox.name == prefix, "create " + sid)
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
    check(api.share(sid).sandbox.id == sid, "share with the team")
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
    key = api.ssh_key(sid, SshKeyRequest(key=SSH_KEY))
    check(key.ssh_user and (key.machine_ip or not REAL), "authorize an ssh key")
    if REAL:
        check(SSH_KEY in api.read_file(sid, path="/home/user/.ssh/authorized_keys").content, "the ssh key is authorized")
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
    resumed = api.resume(sid)
    check(resumed.sandbox.id == sid, "resume")
    ready(sid)
    if REAL:
        check(api.read_file(sid, path="compat/a.txt").content == payload, "the file survived stop and resume")

    fork = api.fork(sid, fork_request=ForkRequest(name=prefix + "-fork"))
    kid = fork.sandbox.id
    made.append(kid)
    check(kid.startswith("bx_") and kid != sid, "fork " + kid)
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
    api.delete_environment(str(env.id))

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
