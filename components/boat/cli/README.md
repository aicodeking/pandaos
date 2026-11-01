# boat CLI

## Install and upgrade

```
curl -fsSL https://raw.githubusercontent.com/aicodeking/pandaos/main/components/boat/cli/install.sh | sh
boat upgrade                 # latest release, in place
boat upgrade --version v0.3.0
```

- **install.sh:** downloads `boat_<os>_<arch>` (linux/darwin, amd64/arm64) and
  `boat_SHA256SUMS` from the GitHub release, checks the SHA-256, and installs to
  `~/.local/bin`. Env: `BOAT_VERSION`, `BOAT_INSTALL_DIR`, `BOAT_REPO`, and
  `BOAT_RELEASE_BASE` for a mirror.
- **`boat upgrade`:** does the same check, then atomically replaces the running
  binary. If the binary's directory isn't writable (e.g. `/usr/local/bin`), run it
  with sudo or re-run install.sh.
- **From source:** `cd components/boat/cli && go build -o ~/.local/bin/boat .`

```
boat login --url https://boat.example --key boat_…     # writes ~/.config/boat/config.json (0600)
boat new --name dev --size large --ttl 8h
boat prompt dev "add tests for the parser" --agent codex
boat fork dev --name dev-try2 && boat stop dev
boat ssh dev            # registers ~/.ssh/id_ed25519.pub, then execs ssh (ProxyCommand via the jump host if the server sets one)
boat scp ./a.txt dev:/home/ubuntu/   # single files over the HTTP files API
boat exec dev -- make test           # exits with the remote exit code
boat new --name ci --secrets GITHUB_TOKEN   # only this team secret goes in (--no-secrets: none)
boat ports add dev 3000                # private: the gateway wants a token
boat ports url dev 3000                # signed URL (browser) + X-Boat-Route-Token for scripts
boat ports add dev 8080 --public       # open route + raw TCP port
boat secrets rotate OPENAI_API_KEY -   # new value from stdin; prints what it does not change
boat teams create acme --owner alice   # operator only; prints alice's key once
```

Commands: `login whoami images usage new list ssh scp exec prompt fork stop start rm desktop ports snapshots secrets keys teams webhooks upgrade version`. Run `boat help` for the syntax.

- **Output:** `--json` works on every command and can go anywhere before `--`. With it,
  stdout carries one JSON object per line. Lists print one item per line; exec and
  prompt pass the server's `{stream,data_b64}`/`{exit}` lines through unchanged.
  Errors go to stderr as `{"error":{…}}`.
- **VM names:** a `<vm>` argument can be an id or a unique name.
- **Environment:** `BOAT_URL`, `BOAT_API_KEY` and `BOAT_CONFIG` override the config file.

Tests: `go test -race ./...`. They compare output against the golden files in
`testdata/`; run `go test -update` to regenerate them.
