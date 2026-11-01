# boat CLI

```
boat login --url https://boat.example --key boat_…     # writes ~/.config/boat/config.json (0600)
boat new --name dev --size large --ttl 8h
boat prompt dev "add tests for the parser" --agent codex
boat fork dev --name dev-try2 && boat stop dev
boat ssh dev            # registers ~/.ssh/id_ed25519.pub, then execs ssh (ProxyCommand via the jump host if the server sets one)
boat scp ./a.txt dev:/home/ubuntu/   # single files over the HTTP files API
boat exec dev -- make test           # exits with the remote exit code
```

Commands: `login whoami images usage new list ssh scp exec prompt fork stop start rm desktop ports snapshots secrets keys teams webhooks version`. Run `boat help` for the syntax.

- **Output:** `--json` works on every command and can go anywhere before `--`. With it,
  stdout carries one JSON object per line. Lists print one item per line; exec and
  prompt pass the server's `{stream,data_b64}`/`{exit}` lines through unchanged.
  Errors go to stderr as `{"error":{…}}`.
- **VM names:** a `<vm>` argument can be an id or a unique name.
- **Environment:** `BOAT_URL`, `BOAT_API_KEY` and `BOAT_CONFIG` override the config file.

Tests: `go test -race ./...`. They compare output against the golden files in
`testdata/`; run `go test -update` to regenerate them.
