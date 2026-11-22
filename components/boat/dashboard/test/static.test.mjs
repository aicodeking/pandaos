// Source-level guards for the rules a strict CSP and the security brief depend on.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { CSP } from "../testlib/harness.mjs";

const WEB = join(dirname(fileURLToPath(import.meta.url)), "..", "web");
const files = readdirSync(WEB, { recursive: true }).map(String).filter((f) => /\.(js|html|css)$/.test(f));
const read = (f) => readFileSync(join(WEB, f), "utf8");
const scripts = files.filter((f) => f.endsWith(".js"));

test("no HTML-parsing DOM sinks", () => {
  for (const f of scripts) {
    assert.doesNotMatch(read(f), /\.(innerHTML|outerHTML)\b|insertAdjacentHTML|document\.write|\beval\(|new Function\(|DOMParser/, f);
  }
});

test("the key is never persisted beyond sessionStorage", () => {
  for (const f of scripts) {
    assert.doesNotMatch(read(f), /localStorage|document\.cookie|indexedDB/, f);
    assert.doesNotMatch(read(f), /console\.(log|info|debug)/, f);
  }
});

test("no external resources in HTML and CSS", () => {
  for (const f of files.filter((x) => !x.endsWith(".js"))) {
    assert.doesNotMatch(read(f), /https?:\/\/|@import|url\(/, f);
  }
});

test("no absolute URLs in scripts, other than comments and the SVG namespace", () => {
  for (const f of scripts) {
    const code = read(f).split("\n").filter((line) => !line.trim().startsWith("//")).join("\n");
    const urls = [...code.matchAll(/https?:\/\/[^"'`\s)]+/g)].map((m) => m[0]).filter((u) => u !== "http://www.w3.org/2000/svg" && !/example\.com|\.\*$/.test(u)); // namespace, placeholder, input pattern
    assert.deepEqual(urls, [], f);
  }
});

test("index.html has no inline script, style or event handler, which a strict CSP would block", () => {
  const html = read("index.html");
  assert.doesNotMatch(html, /<style|\sstyle=|\son\w+=/);
  for (const tag of html.match(/<script[^>]*>[^<]*<\/script>/g)) {
    assert.match(tag, /<script type="module" src="[^"]+"><\/script>/);
  }
});

test("scripts set no inline style, which style-src 'self' would block", () => {
  for (const f of scripts) assert.doesNotMatch(read(f), /\bstyle:|setAttribute\("style"|\.style\./, f);
});

test("asset URLs are relative, so the page works under any path", () => {
  const html = read("index.html");
  for (const [, url] of html.matchAll(/(?:src|href)="([^"]+)"/g)) assert.ok(!url.startsWith("/"), url);
});

test("the README's CSP is the one the browser tests run under", () => {
  assert.ok(readFileSync(join(WEB, "..", "README.md"), "utf8").includes(CSP));
});

test("apiBase ships as same origin", () => {
  assert.match(read("config.js"), /apiBase: ""/);
});

test("start refreshes the VM view after every attempt, success or failure", () => {
  const src = read("src/vms.js");
  assert.match(src, /startVM\(vm\.id\)[\s\S]*?finally \{\s*await refresh\(\)/);
});

test("create and fork surface env_error as an error toast, not a plain success", () => {
  const src = read("src/vms.js");
  assert.match(src, /createVM\([\s\S]*?env_error[\s\S]*?toast\([^\n]*not delivered[^\n]*, true\);\s*\n?\s*else toast\(`Created/);
  assert.match(src, /forkVM\([\s\S]*?env_error[\s\S]*?toast\([^\n]*not delivered[^\n]*, true\);\s*\n?\s*else toast\(`Forked/);
});

test("prompt form is replaced by a note while env_error or provisioning", () => {
  const src = read("src/vm-detail.js");
  assert.match(src, /function promptSection[\s\S]*?envGated\(vm\)[\s\S]*?Prompts are disabled[\s\S]*?const pane/);
});

test("detached-command form is replaced by a note while env is pending/failed", () => {
  const src = read("src/vm-detail.js");
  assert.match(src, /function commandsSection[\s\S]*?envGated\(vm\)[\s\S]*?Commands are disabled[\s\S]*?const logs/);
});

test("vm detail poll re-renders prompt and command sections when the env gate changes", () => {
  const src = read("src/vm-detail.js");
  assert.match(src, /envGateKey\(latest\) !== gateKey[\s\S]*?promptEl\.replaceWith\(nextPrompt\)[\s\S]*?commandsEl\.replaceWith\(nextCommands\)/);
  assert.match(src, /return \[[\s\S]*?promptEl,[\s\S]*?commandsEl,/);
});

test("envBadge shows a pending env badge for env_pending (list and header)", () => {
  assert.match(read("src/ui.js"), /env_pending === true[\s\S]*?"pending env"/);
  assert.match(read("src/vms.js"), /envBadge\(vm\)/);
  assert.match(read("src/vm-detail.js"), /envBadge\(vm\)/);
});

test("env gate covers starting/creating/forking/stopping and keys on the state", async () => {
  const src = read("src/vm-detail.js");
  assert.match(src, /GATED_STATES = \["provisioning", "starting", "creating", "forking", "stopping"\]/);
  const fn = (name) => new Function(`${src.match(new RegExp(`const GATED_STATES[^\\n]*`))[0]}
    ${src.match(/export function envGated[\s\S]*?\n}\n/)[0].replace("export ", "")}
    ${src.match(/export function envGateKey[\s\S]*?\n}\n/)[0].replace("export ", "")}
    return ${name};`)();
  const gated = fn("envGated"), key = fn("envGateKey");
  for (const state of ["starting", "creating", "forking", "stopping", "provisioning"]) assert.equal(gated({ state }), true, state);
  assert.equal(gated({ state: "running" }), false);
  assert.equal(gated({ state: "running", env_pending: true }), true);
  assert.notEqual(key({ state: "starting" }), key({ state: "running" }));
  assert.notEqual(key({ state: "starting" }), key({ state: "forking" }));
  assert.notEqual(key({ state: "running" }), key({ state: "stopped" }));
  assert.notEqual(key({ state: "stopped" }), key({ state: "error" }));
});

test("stopping copy says stopping; stopped refresh uses command-history", () => {
  const src = read("src/vm-detail.js");
  const gatedWhy = new Function(`${src.match(/export function gatedWhy[\s\S]*?\n}\n/)[0].replace("export ", "")} return gatedWhy;`)();
  assert.match(gatedWhy({ state: "stopping" }, "Commands"), /VM is stopping/);
  assert.doesNotMatch(gatedWhy({ state: "stopping" }, "Prompts"), /environment/);
  assert.match(gatedWhy({ state: "starting" }, "Prompts"), /environment is being delivered/);
  assert.match(src, /vm\.state === "running" \? ctx\.api\.listCommands\(vm\.id\) : ctx\.api\.commandHistory\(vm\.id\)/);
});
