// Drives the real page in headless Chrome against the fake API, under the real CSP.
// Skipped when no browser is installed or Node lacks WebSocket.

import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { after, before, describe, test } from "node:test";
import { KEY, startFakeApi, XSS } from "../testlib/fake-api.mjs";
import { externalRequests, findChrome, launch, MOUNT, signIn, startServer } from "../testlib/harness.mjs";

const chrome = findChrome();
const skip = !chrome ? "no Chrome" : typeof WebSocket !== "function" ? "Node 22+ needed" : false;
const SHOTS = process.env.BOAT_DASH_SHOTS;

async function shot(page, name) {
  if (!SHOTS) return;
  mkdirSync(SHOTS, { recursive: true });
  writeFileSync(join(SHOTS, `${name}.png`), await page.screenshot());
}

const has = (text) => `document.body.innerText.includes(${JSON.stringify(text)})`;
const openDialog = `document.querySelector("dialog[open]")`;

function suite(name, features, body) {
  describe(name, { skip }, () => {
    const env = {};
    before(async () => {
      env.api = await startFakeApi({ features });
      env.server = await startServer(env.api.url);
      env.page = await launch(chrome);
      await env.page.goto(`${env.server.origin}${MOUNT}`);
      await signIn(env.page, KEY);
      await env.page.waitFor(`document.querySelector("td a")`);
    });
    after(async () => {
      await env.page?.close();
      env.server?.close();
      env.api?.close();
    });
    body(env);
  });
}

suite("base API only: newer endpoints are absent", false, (env) => {
  test("VM list shows state, size, IP, TTL, with API data rendered as text", async () => {
    const { page } = env;
    const text = await page.evaluate("document.body.innerText");
    assert.match(text, /59m|1h 0m/);
    for (const want of ["running", "small (2 vCPU, 4 GiB)", "10.200.0.2"]) assert.ok(text.includes(want), want);
    assert.ok(text.includes(XSS), "the script-like name is shown literally");
    assert.doesNotMatch(text, /\b(null|undefined|\[object)/);
    assert.equal(await page.evaluate(`document.querySelectorAll("#content img").length`), 0);
    assert.equal(await page.evaluate("window.__pwned ?? 0"), 0);
    await shot(page, "vm-list");
  });

  test("key lives in sessionStorage only and never in a URL", async () => {
    const { page } = env;
    assert.equal(await page.evaluate(`sessionStorage.getItem("boat.apiKey")`), KEY);
    assert.equal(await page.evaluate("localStorage.length"), 0);
    assert.ok(!page.requests.some((r) => r.url.includes(KEY)));
  });

  test("nav hides Environments when the endpoint is missing", async () => {
    await env.page.waitFor(`[...document.querySelectorAll("nav a")].some((a) => a.textContent === "Usage")`);
    assert.equal(await env.page.evaluate(`[...document.querySelectorAll("nav a")].find((a) => a.textContent === "Environments").hidden`), true);
  });

  test("delete asks first, Cancel keeps the VM, and focus starts on Cancel", async () => {
    const { page } = env;
    await page.evaluate(`document.querySelector('button[aria-label="Delete stopped-one"]').click()`);
    await page.waitFor(openDialog);
    assert.equal(await page.evaluate(`document.activeElement.textContent`), "Cancel");
    await page.click("Cancel", "dialog[open]");
    await page.waitFor(`!${openDialog}`);
    assert.ok(await page.evaluate(has("stopped-one")));
  });

  test("stop asks first and then changes state", async () => {
    const { page } = env;
    await page.evaluate(`document.querySelector('button[aria-label^="Stop <img"]').click()`);
    await page.waitFor(openDialog);
    await page.click("Stop VM", "dialog[open]");
    await page.waitFor(`document.querySelector('button[aria-label^="Start <img"]') && !document.querySelector('button[aria-label^="Start <img"]').disabled`);
    await page.evaluate(`document.querySelector('button[aria-label^="Start <img"]').click()`);
    await page.waitFor(`document.querySelector('button[aria-label^="Stop <img"]') && !document.querySelector('button[aria-label^="Stop <img"]').disabled`);
  });

  test("create VM reaches the detail page", async () => {
    const { page } = env;
    await page.click("Create VM");
    await page.waitFor(`location.hash === "#/vms/new" && [...document.querySelectorAll("label")].some((l) => l.textContent === "Name")`);
    await page.waitFor(has("API_TOKEN"));
    await shot(page, "create-vm");
    await page.fill("Name", "made-in-test");
    await page.fill("TTL (seconds)", "600");
    await page.evaluate(`document.querySelector("#content form").requestSubmit()`);
    await page.waitFor(`document.querySelector("h1")?.textContent === "made-in-test"`);
  });

  test("detail hides detached commands and prompt history, keeps the rest", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/vms/vm_1`);
    await page.waitFor(`document.querySelector("h1")?.textContent.includes("img")`);
    await page.waitFor(`[...document.querySelectorAll("summary h2")].find((h) => h.textContent === "Detached commands")?.closest("details").hidden`);
    await page.waitFor(`[...document.querySelectorAll("summary h2")].find((h) => h.textContent === "Prompt history")?.closest("details").hidden`);
    for (const title of ["Exec console", "Files", "Ports", "Snapshots", "Connect", "Prompt an agent"]) {
      assert.equal(await page.evaluate(`[...document.querySelectorAll("summary h2")].find((h) => h.textContent === ${JSON.stringify(title)}).closest("details").hidden`), false, title);
    }
    await shot(page, "vm-detail");
  });

  test("exec streams output as text and shows the exit code", async () => {
    const { page } = env;
    await page.waitFor(`[...document.querySelectorAll("label")].some((l) => l.textContent === "Command")`);
    await page.fill("Command", "uname -a");
    await page.evaluate(`[...document.querySelectorAll("details")].find((d) => d.querySelector("h2").textContent === "Exec console").querySelector("form").requestSubmit()`);
    await page.waitFor(`document.querySelector(".output")?.textContent.includes("[exit 3]")`);
    const out = await page.evaluate(`document.querySelector(".output").textContent`);
    assert.ok(out.includes("ran: bash -lc uname -a"));
    assert.ok(out.includes(XSS));
    assert.equal(await page.evaluate(`document.querySelectorAll(".output img").length`), 0);
    assert.equal(await page.evaluate(`document.querySelector(".output .stderr").textContent`), "warn\n");
  });

  test("ssh command and desktop link", async () => {
    const { page } = env;
    await page.click("Show ssh command");
    await page.waitFor(has("ssh -J jump@host.example:2222 dev@10.200.0.2"));
    await page.click("Get desktop link");
    await page.waitFor(`document.querySelector('a[target="_blank"]')`);
    assert.equal(await page.evaluate(`document.querySelector('a[target="_blank"]').rel`), "noopener noreferrer");
  });

  test("secrets screen shows names, and its value field is a password", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/secrets`);
    await page.waitFor(has("API_TOKEN"));
    assert.equal(await page.evaluate(`[...document.querySelectorAll("label")].find((l) => l.textContent === "Value").control.type`), "password");
    await shot(page, "secrets");
  });

  test("new API key token is shown once, then gone from the DOM", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/keys`);
    await page.waitFor(has("operator"));
    assert.ok(await page.evaluate(has("this session")));
    await page.fill("Name", "ci");
    await page.evaluate(`document.querySelector("#content form").requestSubmit()`);
    await page.waitFor(`document.querySelector("dialog[open] input")?.value === "boat_newtoken_shownonce"`);
    await page.click("Done", "dialog[open]");
    await page.waitFor(has("ci"));
    assert.ok(!(await page.evaluate(`document.documentElement.outerHTML.includes("boat_newtoken_shownonce")`)));
    assert.equal(await page.evaluate("Object.keys(sessionStorage).length"), 1);
  });

  test("webhooks show the delivery log as text", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/webhooks`);
    await page.waitFor(has("hooks.example"));
    await page.click("Deliveries");
    await page.waitFor(has("boom"));
    assert.equal(await page.evaluate(`document.querySelectorAll("#content img").length`), 0);
    await shot(page, "webhooks");
  });

  test("team lists members", async () => {
    await env.page.goto(`${env.server.origin}${MOUNT}#/team`);
    await env.page.waitFor(has("owner"));
    assert.ok(await env.page.evaluate(has("you")));
  });

  test("usage draws SVG charts with labels", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/usage`);
    await page.waitFor(`document.querySelector("svg rect.chart-bar")`);
    assert.ok(await page.evaluate(has("VM hours by size")));
    assert.ok((await page.evaluate(`document.querySelector("svg[role=img]").getAttribute("aria-label")`)).includes("small"));
    await shot(page, "usage");
  });

  test("works at phone width with no sideways scrolling, in dark mode too", async () => {
    const { page } = env;
    await page.resize(375, 800);
    await page.emulateDark(true);
    for (const route of ["#/vms", "#/vms/vm_1", "#/usage", "#/secrets"]) {
      await page.goto(`${env.server.origin}${MOUNT}${route}`);
      await page.waitFor(`document.querySelector("#content h1")`);
      await new Promise((r) => setTimeout(r, 300));
      const over = await page.evaluate("document.documentElement.scrollWidth - document.documentElement.clientWidth");
      assert.ok(over <= 0, `${route} overflows by ${over}px`);
      await shot(page, `phone-${route.replace(/\W+/g, "-")}`);
    }
    assert.notEqual(await page.evaluate(`getComputedStyle(document.body).backgroundColor`), "rgb(246, 247, 249)");
    await page.resize(1280, 900);
    await page.emulateDark(false);
  });

  test("no external requests, CSP violations or uncaught exceptions anywhere above", () => {
    const { page, server } = env;
    assert.deepEqual(externalRequests(page.requests, server.origin), []);
    assert.deepEqual(page.problems.filter((p) => /Content Security Policy|exception/.test(p)), []);
  });

  test("sign out clears the key and returns to sign-in", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/vms`);
    await page.waitFor(`[...document.querySelectorAll("button")].some((b) => b.textContent === "Sign out")`);
    await page.click("Sign out");
    await page.waitFor(`document.querySelector("input[type=password]")`);
    assert.equal(await page.evaluate(`sessionStorage.getItem("boat.apiKey")`), null);
  });

  test("a bad key stays on the sign-in form with the error, and stores nothing", async () => {
    const { page } = env;
    await signIn(page, `boat_wrong_${"c".repeat(20)}`);
    await page.waitFor(has("bad key"));
    assert.equal(await page.evaluate(`sessionStorage.getItem("boat.apiKey")`), null);
    assert.ok(await page.evaluate(`document.querySelector("input[type=password]")`));
  });
});

suite("newer endpoints present", true, (env) => {
  test("detached commands list, logs and file listing appear", async () => {
    const { page } = env;
    await page.goto(`${env.server.origin}${MOUNT}#/vms/vm_1`);
    await page.waitFor(`document.querySelector("h1")?.textContent.includes("img")`);
    await page.waitFor(`[...document.querySelectorAll("code")].some((c) => c.textContent === "sleep 100")`);
    await page.evaluate(`document.querySelectorAll("details").forEach((d) => { d.open = true; })`);
    const logsIn = (title) => `[...document.querySelectorAll("details")].find((d) => d.querySelector("h2").textContent === "${title}").querySelector("td button").click()`;
    await page.waitFor(has("sleep 100"));
    await page.evaluate(logsIn("Detached commands"));
    await page.waitFor(has("log line one"));
    await page.waitFor(has("readme.txt"));
    await page.click("src/");
    await page.waitFor(`[...document.querySelectorAll("label")].find((l) => l.textContent === "Path").control.value === "/home/src"`);
    await page.waitFor(has("fix it"));
    await page.evaluate(logsIn("Prompt history"));
    await page.waitFor(has("agent said hello"));
    await shot(page, "vm-detail-full");
  });

  test("nav shows Environments and the screen lists them", async () => {
    const { page } = env;
    await page.waitFor(`[...document.querySelectorAll("nav a")].some((a) => a.textContent === "Environments" && !a.hidden)`);
    await page.click("Environments", "nav");
    await page.waitFor(has("base"));
    await page.goto(`${env.server.origin}${MOUNT}#/vms/new`);
    await page.waitFor(`[...document.querySelectorAll("label")].find((l) => l.textContent === "Environment template")?.closest(".field").hidden === false`);
  });
});
