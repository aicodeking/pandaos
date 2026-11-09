"""boat.dev's official Python SDK (boat-sdk, pinned in requirements.txt) against
this server's /api/v1: create, get, list, rename, commands (sync and
detached), files, stop, resume, fork, saved snapshots, webhooks, limits and
delete, each answer parsed by the SDK's own models (they check every id,
enum and required field). Run by ../e2e-live.sh:

    BOAT_URL=https://api.example BOAT_API_KEY=boat_… python3 official_sdk.py

BOAT_LIVE_FAKE=1: the fake boatd of api/livescripts_test.go answers every
command with "hello", so command output is not compared.
"""
import os
import secrets
import sys
import time

from boat_sdk import ApiClient, BoatApi, Configuration
from boat_sdk.exceptions import ApiException
from boat_sdk.models.command_request import CommandRequest
from boat_sdk.models.create_sandbox_request import CreateSandboxRequest
from boat_sdk.models.file_write_request import FileWriteRequest
from boat_sdk.models.fork_request import ForkRequest
from boat_sdk.models.saved_snapshot_save_request import SavedSnapshotSaveRequest
from boat_sdk.models.update_sandbox_request import UpdateSandboxRequest
from boat_sdk.models.webhook_create_request import WebhookCreateRequest
from boat_sdk.models.webhook_update_request import WebhookUpdateRequest

REAL = not os.environ.get("BOAT_LIVE_FAKE")
base = os.environ["BOAT_URL"].rstrip("/") + "/api/v1"
api = BoatApi(ApiClient(Configuration(host=base, access_token=os.environ["BOAT_API_KEY"])))
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
    det = api.command(sid, CommandRequest(command="sleep 300", detached=True)).actual_instance
    check(det.process_id >= 1, "detached command")
    st = api.command_status(sid, det.process_id)
    check(st.running or not REAL, "detached command runs")
    hist = api.command_history(sid).commands
    check(any(c.process_id == det.process_id for c in hist), "command history")
    check(api.stop_command(sid, det.process_id).signaled, "stop a detached command")

    payload = "hello from the official SDK " + secrets.token_hex(4)
    api.write_file(sid, FileWriteRequest(path="compat/a.txt", content=payload))
    got = api.read_file(sid, path="compat/a.txt")
    check(got.content == payload or not REAL, "write and read a file")
    names = [f.name for f in api.list_files(sid, path="compat").files]
    check("a.txt" in names, "list files")
    check(api.get_file_info(sid, path="compat/a.txt").size == len(payload) or not REAL, "file info")

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

    snap = api.save_saved_snapshot(SavedSnapshotSaveRequest(sandbox_id=sid, name=prefix.replace("_", "-")[:60] + "-s"))
    check(snap.snapshot.source_sandbox_id == sid, "save a snapshot")
    check(any(s.name == snap.snapshot.name for s in api.list_saved_snapshots().snapshots), "list saved snapshots")
    api.delete_saved_snapshot(snap.snapshot.name)

    hook = api.create_webhook(WebhookCreateRequest(name="official", url="https://example.invalid/h", events=["sandbox.ready"]))
    check(hook.secret.startswith("whsec_"), "create a webhook")
    api.update_webhook(hook.webhook.id, WebhookUpdateRequest(events=["sandbox.archived"]))
    check(api.rotate_webhook_signing_secret(hook.webhook.id).secret != hook.secret, "rotate its secret")
    check(any(h.id == hook.webhook.id for h in api.list_webhooks().webhooks), "list webhooks")
    api.delete_webhook(hook.webhook.id)

    lim = api.limits()
    check(lim.active_sandboxes >= 2, "limits")

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
