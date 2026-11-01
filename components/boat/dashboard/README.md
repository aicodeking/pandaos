# Boat dashboard

A static web dashboard for `boat-api`. It is plain HTML, CSS and ES modules, with no
build step, no dependencies and no CDN. Everything to serve is in `web/`.

## Screens

| Route | Screen |
|---|---|
| sign-in | Paste an API key. It stays in this tab's `sessionStorage` and is checked with `GET /v1/me`. |
| `#/vms` | VM list with state, size, IP and TTL; start, stop, fork, delete |
| `#/vms/new` | Create a VM: name, size, image, environment, snapshot, TTL, idle stop, labels, env, which secrets to inject |
| `#/vms/{id}` | VM detail, in the sections below |
| `#/environments` | Environments: list, create, build, delete. Shown only if the server has them. |
| `#/secrets` | Secret names and dates; add, rotate, delete. Values are write-only. |
| `#/keys` | API keys: list, create (token shown once), revoke |
| `#/webhooks` | Webhooks: list, create (secret shown once), send a test, delete, delivery log |
| `#/team` | Team members: list, add (first key shown once), remove |
| `#/usage` | VM hours, snapshot days and storage, with SVG bar charts over 7, 30 or 90 days |

VM detail sections: connect (signed desktop link, ssh command, add a public key), exec
console, prompt an agent, prompt history, detached commands and logs, files (list, view,
download, upload, delete), ports (add, remove, copy URL), snapshots (create, delete).

These actions ask first: stop, fork, delete VM, make a port public, remove a port, delete
a snapshot or file, stop a command, interrupt a prompt, delete a secret, revoke a key,
delete a webhook or environment, remove a member.

A section hides itself when its endpoint answers 404, 405 or 501, so the dashboard runs
against an older `boat-api`. A 403 becomes a note in that section. Any other error shows
the message and a Retry button.

Not in the dashboard yet, though `boat-api` has them: VM resize, key and webhook rotation,
`/v1/ai/*`, telemetry, conversations, file search and move.

## Serving

Serve `web/` on the same origin as the API. Hash routes mean no rewrite rules, and asset
URLs are relative, so any path prefix works. The URL must end in a slash.

```caddyfile
api.example.com {
	redir /dashboard /dashboard/
	handle_path /dashboard/* {
		root * /path/to/dashboard/web
		header {
			Content-Security-Policy "default-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
			Referrer-Policy no-referrer
			X-Content-Type-Options nosniff
			Cache-Control no-cache
		}
		file_server
	}
	handle {
		reverse_proxy 127.0.0.1:8740
	}
}
```

The page must be on https outside localhost, or the key crosses the network in clear text.
The dashboard shows a warning banner if it is not.

### A different API origin

`web/config.js` sets the API base URL:

```js
export default { apiBase: "https://api.example.com" };
```

`""` (the default) means same origin. With another origin, also:

- change `connect-src 'self'` in the CSP to `connect-src https://api.example.com`;
- make Caddy send CORS headers for that API, allowing `Authorization` and `Content-Type`.
  `boat-api` sends no CORS headers itself, so without them the browser blocks every call.

## Security

- All API data reaches the page through `textContent`, via `h()` in `web/src/ui.js`.
  Nothing uses `innerHTML`. `test/static.test.mjs` fails if that changes.
- The key is sent only in the `Authorization` header. It is never in a URL, `localStorage`
  or a cookie, and it is gone when the tab closes.
- Tokens and signing secrets that are shown once live in a dialog that is removed from the
  DOM when it closes.
- A desktop or port URL from the API is used only if it parses as http(s), so a
  `javascript:` value cannot become a link.
- The ssh command quotes its values, so a hostile host name cannot inject into a pasted command.
- Page JavaScript can read the key, as with any browser client. The CSP is what limits
  that: it blocks injected scripts and sending data to other origins. Keep it on, and
  prefer a key with narrow scopes for day-to-day use.

## Tests

```sh
node --test                                  # needs Node 22+ for the browser tests
BOAT_URL=https://... BOAT_KEY=boat_... ./e2e.sh
```

- `test/api.test.mjs`, `test/format.test.mjs`: the DOM-free client and formatting.
- `test/static.test.mjs`: source rules (no HTML sinks, no external URLs, no inline scripts or
  styles, the key stays out of other storage, this README's CSP matches the tested one).
- `test/browser.test.mjs`: drives every screen in headless Chrome against `testlib/fake-api.mjs`
  with the CSP on. It covers rendering hostile names as text, confirm dialogs, sections
  hiding on 404, phone width, dark mode, and no external requests. It skips when there is
  no browser. Set `BOAT_DASH_SHOTS=DIR` to save screenshots.
- `e2e.sh`: the smoke test against a real API. It serves `web/` with the CSP, proxies `/v1` to
  `BOAT_URL` as Caddy would, signs in, sees a VM it created in the list, opens every screen
  and checks for error boxes, then deletes the VM.
  - With no browser or Node under 22 it prints `SKIP` and exits 0. Set
    `BOAT_E2E_REQUIRE_BROWSER=1` to fail instead.
  - `CHROME=/path` picks the browser. `BOAT_INSECURE=1` accepts a self-signed API certificate.

There is no Playwright, by design: the harness in `testlib/harness.mjs` talks to Chrome over
the DevTools Protocol using Node's built-in `WebSocket`, keeping the no-dependency rule.
