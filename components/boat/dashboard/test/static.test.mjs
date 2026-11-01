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
