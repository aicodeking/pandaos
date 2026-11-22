# boat (Python SDK)

Uses only the standard library (`http.client` with keep-alive pooling) and needs Python 3.8 or newer. Install it with
`pip install ./components/boat/sdk/python` (the distribution is `pandaos-boat`, not boat.dev's `boat-sdk`; it imports as `boat`), or put the directory on your `PYTHONPATH`.

```python
from boat import Boat, verify_webhook

boat = Boat()  # BOAT_URL / BOAT_API_URL + BOAT_API_KEY (trailing /api/v1 stripped), else ~/.config/boat/config.json; or Boat(url, key)
vm = boat.create_vm(name="demo", ttl_s=3600, secrets=["ANTHROPIC_API_KEY"])  # -> VM; secrets: None = the default-injected ones, [] = none
for ev in boat.prompt(vm.id, "fix the failing test", agent="claude"):  # ExecEvent stream
    print(ev.data.decode(), end="")
res = boat.run(vm.id, ["pytest", "-q"])        # ExecResult(stdout, stderr, exit_code)
fork = boat.fork(vm.id, name="demo-2")
boat.stop(vm.id)
```

**Methods:**

- **Account:** `me`, `images`, `usage`
- **VMs:** `list_vms`, `get_vm`, `create_vm`, `create_vms(count, **opts)` (all-or-none batch, 1–16), `update_vm` (`patch_vm`), `start`, `stop`, `fork`, `clone` (`clone_vm`), `vm_usage`, `delete`
- **Run commands:** `exec` (generator), `run` (raises `BoatError` on stream error frames or missing exit), `prompt` (generator)
- **Files:** `read_file`, `write_file`
- **Ports:** `ports`, `add_port` (`auth="private"` default, or `public=True`), `port_url` (signed URL + `token` for the `X-Boat-Route-Token` header), `remove_port`, `revoke_port_tokens`
- **Snapshots:** `snapshots`, `create_snapshot`, `delete_snapshot`
- **Access & SSH keys:** `ssh` (`grant_ssh`), `ssh_keys`, `revoke_ssh`, `desktop`
- **Detached commands:** `start_command`, `list_commands`, `command_status`, `command_history`, `command_logs`, `follow_command`, `signal_command`, `stop_command`, `processes`
- **File ops:** `list_files`, `delete_file`, `move_file`, `search_files`, `file_info`, `make_folder`, `set_file_mode`
- **Async prompts:** `start_prompt`, `list_prompts`, `prompt_status`, `follow_prompt`, `steer_prompt`, `interrupt_prompt`, `conversations`
- **Environments:** `environments`, `create_environment`, `get_environment`, `update_environment`, `delete_environment`, `build_environment`, `upgrade_environment`; `create_vm(environment=)`
- **Resize:** `resize`
- **Snapshots, beyond one VM:** `team_snapshots`, `delete_team_snapshot`, `download_snapshot`, `import_snapshot`, `share_snapshot`, `snapshot_shares`, `revoke_snapshot_share`, `shared_snapshots`, `take_snapshot_share`
- **AI proxy:** `ai_credentials`, `set_ai_credential`, `delete_ai_credential`, `ai_proxy`, `set_ai_proxy`, `ai_usage`, `set_ai_access`
- **Telemetry:** `telemetry`, `set_telemetry`, `delete_telemetry`
- **Rotation:** `rotate_key`, `key_usage`, `rotate_webhook`, `get_webhook`, `update_webhook`
- **One-shot jobs:** `push(vm, dir, to=)` (same files as `boat push`), `run_job(cmd, push=, cwd=, timeout_s=, keep=, **create_vm_args)` (`boat run`: new VM with a 15-minute TTL renewed every 5 minutes, push, run, delete; a timeout gives exit code 124), `run_jobs(jobs, concurrency=16)` (parallel jobs in input order), `host_port(vm, port, public=)` (`boat host`)
- **Teams & events:** `teams`, `get_team`, `create_team(name, owner=)` and `delete_team` (operator only), `add_member`, `remove_member`, `set_self_token_policy`, `set_share_allowlist`, `iter_events(after=)` (SSE stream)
- **Keys:** `keys`, `create_key(name=, scopes=, team_id=, expires_in_s=)`, `delete_key`
- **Secrets:** `secrets`, `set_secret`, `delete_secret`
- **Webhooks:** `webhooks`, `create_webhook`, `delete_webhook`, `deliveries`, `test_webhook`

Errors are raised as `BoatError(status, code, message, request_id)`.
`verify_webhook(secret, request.headers["X-Boat-Signature"], raw_body)` checks a webhook
signature, and accepts either secret during a rotation.

Example: `examples/quickstart.py`. Tests: `python3 -m unittest discover -s tests`.

Live check against a real server: `BOAT_URL=… BOAT_KEY=… ../e2e-live.sh`. It runs this SDK and the other one through create, exec, private port URL, fork, stop and rm.
