// End-to-end smoke test against a real boat-api. Run through ../e2e.sh.
// Loads the page, signs in, and sees a VM it creates (and deletes) itself in the list.

import { externalRequests, launch, MOUNT, signIn, startServer } from "./harness.mjs";

const { BOAT_URL, BOAT_KEY, CHROME_PATH } = process.env;
const VM_TTL_S = 600;
const SETTLE_MS = 500;

async function api(origin, method, path, body) {
  const res = await fetch(origin + path, {
    method,
    headers: { Authorization: `Bearer ${BOAT_KEY}`, "Content-Type": "application/json" },
    body: body && JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`);
  return res.json();
}

new URL(BOAT_URL); // fails here, before a browser starts, if BOAT_URL is not a URL
const server = await startServer(BOAT_URL);
const page = await launch(CHROME_PATH);
const name = `dash-e2e-${Date.now()}`;
let vm = null;
let failed = false;
try {
  vm = await api(server.origin, "POST", "/v1/vms", { name, ttl_s: VM_TTL_S });
  await page.goto(`${server.origin}${MOUNT}`);
  await signIn(page, BOAT_KEY);
  await page.waitFor(`[...document.querySelectorAll("td a")].some((a) => a.textContent === ${JSON.stringify(name)})`, 30000);

  // Every screen must render against the real API with no error box.
  for (const route of [`#/vms/${encodeURIComponent(vm.id)}`, "#/vms/new", "#/secrets", "#/keys", "#/webhooks", "#/team", "#/usage"]) {
    await page.goto(`${server.origin}${MOUNT}${route}`);
    await page.waitFor(`document.querySelector("#content h1") && !document.querySelector("#content .muted")?.textContent.startsWith("Loading")`);
    await new Promise((r) => setTimeout(r, SETTLE_MS));
    const errors = await page.evaluate(`[...document.querySelectorAll("#content .error, .toast-error")].map((e) => e.textContent).filter(Boolean).join("; ")`);
    if (errors) throw new Error(`${route}: ${errors}`);
  }

  const stored = await page.evaluate(`JSON.stringify({ local: localStorage.length, url: location.href })`);
  if (JSON.parse(stored).local !== 0) throw new Error("localStorage is not empty");
  if (page.requests.some((r) => r.url.includes(BOAT_KEY))) throw new Error("the API key appeared in a URL");
  const external = externalRequests(page.requests, server.origin);
  if (external.length) throw new Error(`external requests: ${external.join(", ")}`);
  const blocked = page.problems.filter((p) => /Content Security Policy|exception/.test(p));
  if (blocked.length) throw new Error(`page problems: ${blocked.join("; ")}`);
  process.stdout.write(`PASS dashboard signed in and listed VM ${name}\n`);
} catch (err) {
  failed = true;
  process.stderr.write(`FAIL ${err.stack}\n`);
} finally {
  if (vm) await api(server.origin, "DELETE", `/v1/vms/${encodeURIComponent(vm.id)}`).catch((err) => process.stderr.write(`cleanup: ${err.message}\n`));
  await page.close();
  server.close();
}
process.exit(failed ? 1 : 0);
