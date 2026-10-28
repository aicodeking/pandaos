# panda-browser

Optional headless Chromium that gives agents a reliable browser over the
Chrome DevTools Protocol (CDP). **Off by default.**

```nix
imports = [ ./components/browser/module.nix ];
services.panda-browser.enable = true;
# services.panda-browser.package = pkgs.ungoogled-chromium;  # also works
# services.panda-browser.port = 9222;
# services.panda-browser.extraArgs = [ "--lang=en-US" ];
# services.panda-browser.memoryMax = "2G";
# services.panda-browser.fonts = true;   # DejaVu, so pages render text
```

The unit `panda-browser.service` runs `chromium --headless=new` as a
`DynamicUser` with a fresh profile in `/run/panda-browser` on every start
(the runtime dir is removed on stop). It restarts on failure.

## Connecting

Discover the endpoint from `/etc/panda/browser.json`:

```json
{"cdp":"http://127.0.0.1:9222"}
```

Playwright:

```js
const { cdp } = JSON.parse(fs.readFileSync('/etc/panda/browser.json'));
const browser = await chromium.connectOverCDP(cdp);
const page = await (browser.contexts()[0] ?? await browser.newContext()).newPage();
```

Playwright MCP:

```sh
npx @playwright/mcp --cdp-endpoint http://127.0.0.1:9222
```

Raw: `curl http://127.0.0.1:9222/json/version` returns `webSocketDebuggerUrl`.

## Closure cost

Chromium plus its runtime deps (GTK, Mesa, NSS, ffmpeg, …) add roughly
500 MB+ to the system closure; DejaVu adds about 10 MB. That is why the
service is opt-in. Measure the real delta with
`nix path-info -Sh <toplevel>` with and without the option.

## Security model

- CDP is bound to `127.0.0.1` only and the port is never added to the
  firewall. Do not add it.
- CDP has **no authentication**. Any local user or process can drive the
  browser: read its pages, cookies, and run JS in it. Use this only on
  single-tenant boxes where every local user is trusted.
- Chromium keeps its own sandbox (no `--no-sandbox`). It uses unprivileged
  user namespaces, so the kernel must allow them (the NixOS default; a
  hardened profile that disables them breaks the browser). For that reason
  the unit sets `RestrictNamespaces=false` and does not set `PrivateUsers`,
  `CapabilityBoundingSet` or `SystemCallFilter`. `MemoryDenyWriteExecute`
  is off because V8 JITs. The rest (`ProtectSystem=strict`, `ProtectHome`,
  `PrivateTmp`, `PrivateDevices`, `NoNewPrivileges`, kernel protections,
  `RestrictAddressFamilies`, `MemoryMax`) is on.
- The profile is ephemeral: a restart drops cookies and logins.

## VM test outline (for the tests worker)

Two nodes: `machine` (browser on) and `peer` (plain client).

```python
machine.wait_for_unit("panda-browser.service")
machine.wait_for_open_port(9222)
out = machine.succeed("curl -sf http://127.0.0.1:9222/json/version")
assert "webSocketDebuggerUrl" in out
machine.succeed("jq -e '.cdp == \"http://127.0.0.1:9222\"' /etc/panda/browser.json")
# Not listening on any non-loopback address:
machine.fail("ss -ltnH 'sport = :9222' | grep -v -e '127.0.0.1' -e '\\[::1\\]'")
# Not reachable from another node:
peer.fail("curl -sf --max-time 5 http://machine:9222/json/version")
# Fresh profile per start:
machine.succeed("systemctl restart panda-browser.service")
machine.wait_for_open_port(9222)
```

Node config: `machine` imports this module with
`services.panda-browser.enable = true;` and has `curl`, `jq`, `iproute2`;
`peer` has `curl`. Give `machine` at least 2 GB RAM.
