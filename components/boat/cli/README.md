# boat CLI

## Install and upgrade

**No release exists yet**, so the curl/irm one-liners and `boat upgrade` below
have nothing to download (they fail with a 404) until the first `v*` tag is
published. Install one of these ways today:

- **Build from source** (Go 1.22+):
  ```
  git clone https://github.com/aicodeking/pandaos && cd pandaos/components/boat/cli
  go build -o ~/.local/bin/boat .      # Windows: go build -o boat.exe .
  boat version
  ```
- **CI artifact:** run the `release` workflow by hand (Actions → release → Run
  workflow; a dry run publishes nothing) and download its `boat-assets`
  artifact (kept 7 days): `boat_<os>_<arch>` binaries plus `boat_SHA256SUMS`.
  Check the sum, `chmod +x`, and put it on your PATH as `boat`.

Once a release is published:

```
curl -fsSL https://raw.githubusercontent.com/aicodeking/pandaos/main/components/boat/cli/install.sh | sh
irm https://raw.githubusercontent.com/aicodeking/pandaos/main/components/boat/cli/install.ps1 | iex   # Windows
boat upgrade                 # latest release, in place
boat upgrade --version v0.3.0
```

- **install.sh:** downloads `boat_<os>_<arch>` (linux/darwin, amd64/arm64) and
  `boat_SHA256SUMS` from the GitHub release, checks the SHA-256, and installs to
  `~/.local/bin`. Env: `BOAT_VERSION`, `BOAT_INSTALL_DIR`, `BOAT_REPO`, and
  `BOAT_RELEASE_BASE` for a mirror.
- **install.ps1:** the same for `boat_windows_<arch>.exe`, into
  `%LOCALAPPDATA%\\boat\\bin`, and adds it to your user Path. `boat ssh` needs the Windows
  OpenSSH client.
- **`boat upgrade`:** does the same check, then atomically replaces the running
  binary. If the binary's directory isn't writable (e.g. `/usr/local/bin`), run it
  with sudo or re-run install.sh.

```
boat login --url https://boat.example --key boat_…     # writes ~/.config/boat/config.json (0600)
boat new --name dev --size large --ttl 8h
boat prompt dev "add tests for the parser" --agent codex
boat fork dev --name dev-try2 && boat stop dev
boat ssh dev            # registers ~/.ssh/id_ed25519.pub, then execs ssh (ProxyCommand via the jump host if the server sets one)
boat scp ./a.txt dev:a.txt;  boat scp -r ./dir dev:dir;  boat scp -r dev:out ./   # files API; relative = home
boat exec dev -- make test           # exits with the remote exit code
boat new --name ci --secrets GITHUB_TOKEN   # only this team secret goes in (--no-secrets: none)
boat ports add dev 3000                # private: the gateway wants a token
boat ports url dev 3000                # signed URL (browser) + X-Boat-Route-Token for scripts
boat ports add dev 8080 --public       # open route + raw TCP port
boat secrets rotate OPENAI_API_KEY -   # new value from stdin; prints what it does not change
boat teams create acme --owner alice   # operator only; prints alice's key once
boat exec dev --detach -- npm run build   # prints a command id; see boat cmd
boat cmd logs dev c_123 -f             # follows to the end, exits with its exit code
boat cmd ls|status|signal|stop|history dev [id]
boat files ls|rm|mv|search|info|mkdir|chmod dev ...
boat prompt dev "refactor billing" --async    # background turn: boat prompts ls|status|logs
boat steer dev "skip the integration tests"   # interrupts the turn, resumes its session
boat interrupt dev;  boat conversations dev;  boat prompt dev "more" --resume conv_...
boat env new base --setup-file setup.sh --var A=b --secrets TOKEN;  boat env build base
boat new --env base                    # runs the setup, or starts from the build
boat resize dev large --discard-memory;  boat resume dev --type large
boat snapshots dev download snap-1 -o a.tar.zst;  boat snapshots import a.tar.zst --name copy
boat snapshots dev share snap-1 [--team T];  boat new --from bsh_...;  boat snapshots shared
boat keys rotate k1 --overlap 1h;  boat webhooks rotate wh_1
echo "$ANTHROPIC_API_KEY" | boat ai credentials set anthropic -   # stays on the host
boat ai proxy [on|off];  boat ai usage [vm];  boat ai block|unblock dev
boat telemetry set https://otlp.example -H x-key=v
boat run --from ci-base --type large --push . -- npm test   # new VM, copy in, run, delete; exits with its exit code
boat push dev ./api --to /srv/api      # git-tracked and new files; replaces, never deletes
boat new --from ci-base --count 16     # one request, all or none (1-16); prints every id
boat host dev 3000 [--public]          # expose a port and print its URL
boat forward dev --remote 5432         # sandbox port -> localhost, until Ctrl+C
boat forward dev --reverse --local 7777  # your localhost:7777 -> 127.0.0.1:7777 in the VM
boat ssh-config dev                    # ~/.ssh/boat/<id>.conf, for VS Code and Cursor
boat exec current -- ls                # the last VM created or forked in this shell
```

### Differences from boat.dev

- **`boat run`:** the last line has no cost, because this server has no pricing, and JSON
  has no `dollars`. A job whose CLI dies stops after 15 idle minutes but its disk stays until
  you delete it: this server has no snapshots-off VMs, and no `extend` to move the stop forward.
  `--personal` does not exist. `--keep` stops the same way.
- **`boat push` and `run --push`:** go through the files API rather than SSH, so one archive
  is capped by the API's `-max-file-bytes` (256 MiB by default). No ssh key is needed.
- **`boat new --count`:** one request; the server creates the batch all or none (boatd checks quotas
  and host room for all of them under one lock, and deletes them all if one fails to boot). At most
  16 per call; `--name x` gives `x-1` … `x-N`. `--wait-setup` does not combine with it.
- **`--fail-fast`** on `new`, `fork`, `start`/`resume`: a full host answers 503 `no_ready_machine` at
  once. Without it the answer is 503 `host_capacity` (also at once: boatd never queues).
- **`boat stop --force`:** kills the VM without a snapshot; the next start is a cold boot from disk
  and loses what the guest had not written out.
- **`boat new --env NAME`** names an environment, as it did before; `-e KEY=VALUE` sets a
  variable. `boat run` follows boat.dev: `-e`/`--env` for variables, `--environment` for the name.
- **`current`:** needs no shell function. It is keyed by the parent process, so each shell has
  its own, and a script or CI step has none until it creates a VM. It expires after 7 days.
- **`self` and `host` in a VM:** the CLI and a `host` symlink are in both guest images. boat-api
  puts `BOAT_ID` in every VM, and `BOAT_URL` + `BOAT_API_KEY` (a key for that VM only) when the
  operator sets `-vm-api-url` or `-public-url`. Without that, store a scoped key as the team secrets
  `BOAT_API_KEY` and `BOAT_URL` and restart the VM. `self` is `BOAT_ID` if set, else the hostname.
- **`boat host`:** a private URL is a signed link that lasts 7 days, not a token that never
  expires, so running it again gives a new link. `--title` and `--localhost` do not exist.
- **`boat ssh-config`:** boat.dev documents a script instead of a command; this writes the same
  file, with `StrictHostKeyChecking accept-new`, because the API does not return the host key.
- **Not here, because the server has no support yet:** `--status`.
- **`boat prompt --provider pi|prime|kimi|mistral`** (aliases `claude-code`, `prime-agent`): their
  flags come from each CLI's `--help` and have not been run in CI; see docs/boat.md.

Commands: `login whoami images usage new run push list info ps vm-usage ssh ssh-config forward scp exec cmd files prompt prompts conversations steer interrupt env fork stop start resume rm delete resize desktop ports host snapshots secrets keys teams webhooks ai telemetry upgrade version`. Run `boat help` for the syntax.

- **Output:** `--json` works on every command and can go anywhere before `--`. With it,
  stdout carries one JSON object per line. Lists print one item per line; exec and
  prompt pass the server's `{stream,data_b64}`/`{exit}` lines through unchanged.
  Errors go to stderr as `{"error":{…}}`, and stdout ends with boat.dev's
  `{"event":"error","error","code","status","message"}` line. `boat new --json` prints
  the VM with `"event":"ready"`, so boat.dev's scripts work unchanged:
  `id=$(boat new --json | jq -r 'select(.event == "ready") | .id')`,
  `boat delete "$id" --yes --json` (`--yes`/`-y` are accepted; this CLI never asks).
- **VM names:** a `<vm>` argument can be an id or a unique name.
- **Environment:** `BOAT_URL` (or boat.dev's `BOAT_API_URL`; a trailing `/api/v1` is
  dropped), `BOAT_API_KEY` and `BOAT_CONFIG` override the config file.

Tests: `go test -race ./...`. They compare output against the golden files in
`testdata/`; run `go test -update` to regenerate them.
