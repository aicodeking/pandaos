// Browser test harness with no dependencies: a static server that applies the
// README's CSP and proxies /v1 (as Caddy does), plus headless Chrome driven over
// the DevTools Protocol using Node's built-in WebSocket (Node 22+).

import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import http from "node:http";
import https from "node:https";
import { tmpdir } from "node:os";
import { dirname, extname, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const WEB_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..", "web");
export const MOUNT = "/dashboard/";
export const CSP = "default-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'";

const TYPES = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml" };
const CHROME_PATHS = [
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Chromium.app/Contents/MacOS/Chromium",
];
const CHROME_NAMES = ["chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"];

export function findChrome() {
  if (process.env.CHROME) return process.env.CHROME;
  const dirs = (process.env.PATH ?? "").split(":");
  const found = CHROME_NAMES.flatMap((n) => dirs.map((d) => join(d, n))).find(existsSync);
  return found ?? CHROME_PATHS.find(existsSync) ?? null;
}

function proxy(req, res, upstream) {
  const target = new URL(req.url, upstream);
  const client = target.protocol === "https:" ? https : http;
  const out = client.request(target, { method: req.method, headers: { ...req.headers, host: target.host }, rejectUnauthorized: process.env.BOAT_INSECURE !== "1" }, (up) => {
    res.writeHead(up.statusCode, up.headers);
    up.pipe(res);
  });
  out.on("error", (err) => {
    res.writeHead(502, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: { code: "bad_gateway", message: err.message } }));
  });
  req.pipe(out);
}

// Serves web/ at MOUNT with the CSP, and sends /v1 to upstream, giving the browser one origin.
export function startServer(upstream) {
  const server = http.createServer((req, res) => {
    const path = new URL(req.url, "http://x").pathname;
    if (path.startsWith("/v1/")) return proxy(req, res, upstream);
    if (!path.startsWith(MOUNT)) {
      res.writeHead(404);
      return res.end();
    }
    const relative = decodeURIComponent(path.slice(MOUNT.length)) || "index.html";
    const file = resolve(WEB_ROOT, relative);
    if (!file.startsWith(WEB_ROOT + sep) || !existsSync(file)) {
      res.writeHead(404);
      return res.end();
    }
    res.writeHead(200, { "Content-Type": TYPES[extname(file)] ?? "application/octet-stream", "Content-Security-Policy": CSP, "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-store" });
    return res.end(readFileSync(file));
  });
  return new Promise((ok) => {
    server.listen(0, "127.0.0.1", () => ok({ origin: `http://127.0.0.1:${server.address().port}`, close: () => { server.closeAllConnections(); server.close(); } }));
  });
}

function waitForDevtoolsUrl(proc) {
  return new Promise((ok, fail) => {
    let log = "";
    const timer = setTimeout(() => fail(new Error(`Chrome did not start:\n${log}`)), 30000);
    proc.stderr.on("data", (chunk) => {
      log += chunk;
      const match = log.match(/DevTools listening on (ws:\/\/\S+)/);
      if (match) {
        clearTimeout(timer);
        ok(match[1]);
      }
    });
    proc.on("exit", (code) => {
      clearTimeout(timer);
      fail(new Error(`Chrome exited with ${code}:\n${log}`));
    });
  });
}

// Launches Chrome and opens one page. Records requests, CSP errors and console errors.
export async function launch(chromePath, { width = 1280, height = 900 } = {}) {
  const profile = mkdtempSync(join(tmpdir(), "boat-dash-"));
  const args = ["--headless=new", "--remote-debugging-port=0", `--user-data-dir=${profile}`, `--window-size=${width},${height}`, "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions", "--disable-gpu", "about:blank"];
  if (process.getuid?.() === 0) args.push("--no-sandbox");
  const proc = spawn(chromePath, args, { stdio: ["ignore", "ignore", "pipe"] });
  const wsUrl = await waitForDevtoolsUrl(proc);
  const port = new URL(wsUrl).port;
  const target = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" })).json();
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((ok, fail) => {
    ws.onopen = ok;
    ws.onerror = () => fail(new Error("could not connect to Chrome"));
  });

  let nextId = 0;
  const pending = new Map();
  const requests = [];
  const problems = [];
  ws.onmessage = (msg) => {
    const data = JSON.parse(msg.data);
    if (data.id && pending.has(data.id)) {
      const { ok, fail } = pending.get(data.id);
      pending.delete(data.id);
      if (data.error) fail(new Error(data.error.message));
      else ok(data.result);
    } else if (data.method === "Network.requestWillBeSent") {
      requests.push({ url: data.params.request.url, headers: data.params.request.headers });
    } else if (data.method === "Runtime.exceptionThrown") {
      problems.push(`exception: ${data.params.exceptionDetails.exception?.description ?? data.params.exceptionDetails.text}`);
    } else if (data.method === "Log.entryAdded" && data.params.entry.level === "error") {
      problems.push(`log: ${data.params.entry.text} ${data.params.entry.url ?? ""}`);
    }
  };
  const send = (method, params = {}) => new Promise((ok, fail) => {
    nextId += 1;
    pending.set(nextId, { ok, fail });
    ws.send(JSON.stringify({ id: nextId, method, params }));
  });
  await Promise.all(["Page.enable", "Runtime.enable", "Network.enable", "Log.enable"].map((m) => send(m)));

  async function evaluate(expression) {
    const { result, exceptionDetails } = await send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
    if (exceptionDetails) throw new Error(`${expression}: ${exceptionDetails.exception?.description ?? exceptionDetails.text}`);
    return result.value;
  }

  async function waitFor(expression, timeoutMs = 15000) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      if (await evaluate(`Boolean(${expression})`)) return;
      if (Date.now() > deadline) {
        const body = await evaluate("document.body.innerText").catch(() => "(no body)");
        throw new Error(`timed out waiting for: ${expression}\nPage text:\n${body}`);
      }
      await new Promise((r) => setTimeout(r, 50));
    }
  }

  return {
    send,
    evaluate,
    waitFor,
    requests,
    problems,
    goto: (url) => send("Page.navigate", { url }),
    // Finds a <button> or <a> by exact text, optionally inside a scope selector, and clicks it.
    click: (text, scope = "body") => evaluate(`(() => {
      const el = [...document.querySelector(${JSON.stringify(scope)}).querySelectorAll("button, a, summary")].find((b) => b.textContent.trim() === ${JSON.stringify(text)});
      if (!el) throw new Error("no clickable: " + ${JSON.stringify(text)});
      el.click();
    })()`),
    // Sets a value through the labelled control, proving the label is wired to it.
    fill: (label, value) => evaluate(`(() => {
      const l = [...document.querySelectorAll("label")].find((x) => x.textContent.trim() === ${JSON.stringify(label)});
      if (!l || !l.control) throw new Error("no control labelled " + ${JSON.stringify(label)});
      l.control.value = ${JSON.stringify(value)};
      l.control.dispatchEvent(new Event("input", { bubbles: true }));
    })()`),
    resize: (w, h) => send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: false }),
    emulateDark: (dark) => send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] }),
    screenshot: async () => Buffer.from((await send("Page.captureScreenshot", { format: "png" })).data, "base64"),
    close: async () => {
      ws.close();
      const exited = new Promise((r) => proc.once("exit", r));
      proc.kill("SIGKILL");
      await exited;
      rmSync(profile, { recursive: true, force: true });
    },
  };
}

// Everything the page requested must be on its own origin (or a local scheme).
export function externalRequests(requests, origin) {
  return requests.map((r) => r.url).filter((url) => !url.startsWith(`${origin}/`) && !/^(data|blob|about):/.test(url));
}

export const signIn = async (page, key) => {
  await page.waitFor(`document.querySelector("form input[type=password]")`);
  await page.fill("API key", key);
  await page.evaluate(`document.querySelector("form").requestSubmit()`);
};
