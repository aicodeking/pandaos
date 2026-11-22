// In-memory boat-api for the browser test. Shapes follow api/openapi.json and boatd's JSON.
// Set `features` to add the endpoints that are newer than the base API.

import http from "node:http";

export const KEY = `boat_${"a".repeat(16)}_${"b".repeat(64)}`;
export const XSS = `<img src=x onerror="window.__pwned=1">`;

const clone = (x) => JSON.parse(JSON.stringify(x));

function seed() {
  return {
    vms: [
      { id: "vm_1", name: XSS, size: "small", vcpu: 2, mem_mib: 4096, state: "running", ip: "10.200.0.2", ttl_s: 3600, expires_at: new Date(Date.now() + 3600000).toISOString(), image: "ubuntu-24.04", team: "t1", created_at: new Date().toISOString(), ports: [] },
      { id: "vm_2", name: "stopped-one", env_error: "push failed", agent_ready: false, size: "large", vcpu: 8, mem_mib: 16384, state: "stopped", ip: "10.200.0.3", image: "ubuntu-24.04", team: "t1", created_at: "2026-01-01T00:00:00Z", ports: [] },
    ],
    ports: [{ guest_port: 8000, hostname: "8000-vm_1.vm.example", auth: "private" }],
    snapshots: [{ id: "snap_1", name: XSS, created_at: "2026-01-01T00:00:00Z" }],
    secrets: [{ name: "API_TOKEN", updated: "2026-01-01T00:00:00Z", inject: true }],
    keys: [{ id: "k1", name: "operator", scopes: [], created: "2026-01-01T00:00:00Z" }],
    webhooks: [{ id: "wh1", url: "https://hooks.example/x", events: ["vm.*"], created: "2026-01-01T00:00:00Z" }],
    members: [{ user_id: "u1", role: "owner" }],
    environments: [{ id: "e1", name: "base", image: "ubuntu-24.04", size: "small", env: { A: "1" }, is_default: true, version: 2, build: { state: "failed", version: 2, error: "apt failed" } }],
    files: {
      "/home": [{ name: "readme.txt", type: "file", size: 12, mtime: "2026-01-01T00:00:00Z" }, { name: "src", type: "dir" }],
      "/home/src": [{ name: "app.js", type: "file", size: 24, mtime: "2026-01-01T00:00:00Z" }],
    },
    aiProxy: { enabled: true, available: true },
    aiCredentials: [{ provider: "anthropic", last4: "9876", updated: "2026-01-01T00:00:00Z" }],
    aiUsage: {
      usage: [{ team_id: "t1", vm_id: "vm_1", provider: "anthropic", model: "claude-sonnet-4", requests: 4, input_tokens: 1200, output_tokens: 450, updated: "2026-01-02T00:00:00Z" }],
      totals: { requests: 4, input_tokens: 1200, output_tokens: 450 },
    },
  };
}

export function startFakeApi({ features = false } = {}) {
  const db = seed();
  const log = [];
  const b64 = (s) => Buffer.from(s).toString("base64");

  const routes = [
    ["GET", /^\/v1\/me$/, () => ({ user: { id: "u1", name: "alice" }, team: { id: "t1", name: "acme" }, role: "owner", scopes: ["vms:read", "vms:write", "exec", "admin"], key_id: "k1", operator: true })],
    ["GET", /^\/v1\/images$/, () => ({ images: [{ id: "ubuntu-24.04", default: true }] })],
    ["GET", /^\/v1\/usage$/, () => ({ teams: [{ team: "t1", vm_seconds: 7200, snapshot_days: 1.5, vm_seconds_by_size: { small: 3600, large: 3600 } }], vms: [{ id: "vm_1", vm_seconds: 7200 }], snapshots: [{ id: "snap_1", snapshot_days: 1.5 }], storage: [{ committed_bytes: 1073741824 }] })],
    ["GET", /^\/v1\/vms$/, () => ({ vms: db.vms })],
    ["POST", /^\/v1\/vms$/, (_m, body) => {
      const vm = { id: `vm_${db.vms.length + 1}${Date.now()}`, state: "running", size: "default", ip: "10.200.0.9", team: "t1", created_at: new Date().toISOString(), ...body };
      db.vms = [...db.vms, vm];
      return [201, vm];
    }],
    ["GET", /^\/v1\/vms\/([^/]+)$/, ([, id]) => db.vms.find((v) => v.id === id) ?? [404, { error: { code: "not_found", message: "no such VM" } }]],
    ["DELETE", /^\/v1\/vms\/([^/]+)$/, ([, id]) => {
      db.vms = db.vms.filter((v) => v.id !== id);
      return { ok: true };
    }],
    ["POST", /^\/v1\/vms\/([^/]+)\/(start|stop)$/, ([, id, action]) => {
      db.vms = db.vms.map((v) => (v.id === id ? { ...v, state: action === "stop" ? "stopped" : "running" } : v));
      return db.vms.find((v) => v.id === id);
    }],
    ["POST", /^\/v1\/vms\/([^/]+)\/fork$/, ([, id], body) => {
      const child = { ...db.vms.find((v) => v.id === id), id: `vm_f${Date.now()}`, name: body.name || "fork" };
      db.vms = [...db.vms, child];
      return [201, child];
    }],
    ["GET", /^\/v1\/vms\/[^/]+\/ports$/, () => ({ ports: db.ports })],
    ["GET", /^\/v1\/vms\/[^/]+\/ports\/\d+\/url$/, () => ({ url: "https://8000-vm_1.vm.example/?boat_exp=1&boat_sig=s", auth: "private" })],
    ["GET", /^\/v1\/vms\/[^/]+\/snapshots$/, () => ({ snapshots: db.snapshots })],
    ["GET", /^\/v1\/vms\/[^/]+\/desktop$/, () => ({ url: "https://desktop.example/vm_1?exp=1&sig=s", expires_at: "2030-01-01T00:00:00Z" })],
    ["POST", /^\/v1\/vms\/[^/]+\/ssh$/, () => ({ host: "10.200.0.2", port: 22, user: "dev", jump: "jump@host.example:2222" })],
    ["GET", /^\/v1\/secrets$/, () => ({ secrets: db.secrets })],
    ["PUT", /^\/v1\/secrets\/([^/]+)$/, ([, name], body) => {
      const prev = db.secrets.find((x) => x.name === name);
      const inject = typeof body.inject === "boolean" ? body.inject : (prev?.inject ?? false);
      const updated = new Date().toISOString();
      db.secrets = [...db.secrets.filter((x) => x.name !== name), { name, updated, inject }];
      return { name, updated, inject, ...(prev ? { rotated: true } : {}) };
    }],
    ["GET", /^\/v1\/keys$/, () => ({ keys: db.keys })],
    ["POST", /^\/v1\/keys$/, (_m, body) => {
      const expires = body.expires_in_s ? new Date(Date.now() + body.expires_in_s * 1000).toISOString() : undefined;
      const item = { id: `k${db.keys.length + 1}`, name: body.name, scopes: body.scopes, created: new Date().toISOString(), ...(expires ? { expires } : {}) };
      db.keys = [...db.keys, item];
      return [201, { ...item, token: "boat_newtoken_shownonce" }];
    }],
    ["GET", /^\/v1\/webhooks$/, () => ({ webhooks: db.webhooks })],
    ["GET", /^\/v1\/webhooks\/[^/]+\/deliveries$/, () => ({ deliveries: [{ id: "d1", event: XSS, attempt: 1, status: 500, ok: false, error: "boom", at: "2026-01-01T00:00:00Z" }] })],
    ["GET", /^\/v1\/teams\/([^/]+)$/, () => ({ id: "t1", name: "acme", role: "owner", members: db.members })],
  ];
  const featureRoutes = [
    ["GET", /^\/v1\/vms\/[^/]+\/commands$/, () => ({ commands: [{ id: "c1", cmd: ["sleep", "100"], state: "running", started_at: "2026-01-01T00:00:00Z" }] })],
    ["GET", /^\/v1\/vms\/[^/]+\/commands\/c1\/logs$/, (_m, _b, url) => (url.searchParams.get("offset") ? { data_b64: "", next_offset: 13, running: true } : { data_b64: b64("log line one\n"), next_offset: 13, running: true })],
    ["GET", /^\/v1\/vms\/[^/]+\/files\/list$/, (_m, _b, url) => {
      const dir = (url.searchParams.get("path") || "/home").replace(/\/+$/, "") || "/";
      return { entries: db.files[dir] ?? [] };
    }],
    ["PUT", /^\/v1\/vms\/[^/]+\/files$/, (_m, _b, url, rawLen) => {
      const target = url.searchParams.get("path") || "";
      const idx = target.lastIndexOf("/");
      const dir = idx <= 0 ? "/" : target.slice(0, idx);
      const name = target.slice(idx + 1);
      const list = db.files[dir] ?? [];
      db.files[dir] = [...list.filter((e) => e.name !== name), { name, type: "file", size: rawLen, mtime: new Date().toISOString() }];
      return { ok: true };
    }],
    ["DELETE", /^\/v1\/vms\/[^/]+\/files$/, (_m, _b, url) => {
      const target = url.searchParams.get("path") || "";
      const idx = target.lastIndexOf("/");
      const dir = idx <= 0 ? "/" : target.slice(0, idx);
      const name = target.slice(idx + 1);
      db.files[dir] = (db.files[dir] ?? []).filter((e) => e.name !== name);
      return { ok: true };
    }],
    ["GET", /^\/v1\/environments$/, () => ({ environments: db.environments })],
    ["PUT", /^\/v1\/environments\/([^/]+)$/, ([, id], body) => {
      let updated = null;
      db.environments = db.environments.map((e) => {
        if (e.id !== id && e.name !== id) return e;
        updated = { ...e, ...body, version: (e.version ?? 1) + 1 };
        return updated;
      });
      return updated ?? [404, { error: { code: "not_found", message: "no such environment" } }];
    }],
    ["POST", /^\/v1\/environments\/([^/]+)\/upgrade$/, ([, id], body) => {
      const env = db.environments.find((e) => e.id === id || e.name === id);
      if (!env) return [404, { error: { code: "not_found", message: "no such environment" } }];
      const count = Array.isArray(body.agentIds) ? body.agentIds.length : 2;
      return { environment: env.id, version: env.version ?? 1, upgraded: count, upgraded_live: count, upgraded_stopped: 0, failed: 0 };
    }],
    ["POST", /^\/v1\/vms\/([^/]+)\/resize$/, ([, id], body) => {
      const sizes = { small: [2, 4096], default: [4, 8192], large: [8, 16384], xlarge: [16, 32768] };
      let updated = null;
      db.vms = db.vms.map((v) => {
        if (v.id !== id) return v;
        const size = body.size || v.size;
        const [vcpu, mem_mib] = sizes[size] ?? [v.vcpu, v.mem_mib];
        updated = { ...v, size, vcpu, mem_mib, ...(body.disk_gib ? { disk_gib: body.disk_gib } : {}) };
        return updated;
      });
      return updated ?? [404, { error: { code: "not_found", message: "no such VM" } }];
    }],
    ["PATCH", /^\/v1\/vms\/([^/]+)$/, ([, id], body) => {
      let updated = null;
      db.vms = db.vms.map((v) => {
        if (v.id !== id) return v;
        updated = { ...v, ...body };
        return updated;
      });
      return updated ?? [404, { error: { code: "not_found", message: "no such VM" } }];
    }],
    ["POST", /^\/v1\/keys\/([^/]+)\/rotate$/, ([, id], body) => {
      const k = db.keys.find((x) => x.id === id);
      if (!k) return [404, { error: { code: "not_found", message: "no such key" } }];
      const rotated = new Date().toISOString();
      const overlap = body.overlap_s ?? 86400;
      const prev_until = overlap > 0 ? new Date(Date.now() + overlap * 1000).toISOString() : undefined;
      return { ...k, rotated, ...(prev_until ? { prev_until } : {}), token: "boat_rotated_shownonce" };
    }],
    ["GET", /^\/v1\/ai\/proxy$/, () => db.aiProxy],
    ["PUT", /^\/v1\/ai\/proxy$/, (_m, body) => {
      db.aiProxy = { ...db.aiProxy, enabled: Boolean(body.enabled) };
      return db.aiProxy;
    }],
    ["GET", /^\/v1\/ai\/credentials$/, () => ({ credentials: db.aiCredentials })],
    ["PUT", /^\/v1\/ai\/credentials\/([^/]+)$/, ([, provider], body) => {
      const item = { provider, last4: String(body.value ?? "").slice(-4), updated: new Date().toISOString() };
      db.aiCredentials = [...db.aiCredentials.filter((c) => c.provider !== provider), item];
      return item;
    }],
    ["DELETE", /^\/v1\/ai\/credentials\/([^/]+)$/, ([, provider]) => {
      db.aiCredentials = db.aiCredentials.filter((c) => c.provider !== provider);
      return { ok: true };
    }],
    ["GET", /^\/v1\/ai\/usage$/, () => db.aiUsage],
    ["GET", /^\/v1\/vms\/[^/]+\/prompts$/, () => ({ prompts: [{ id: "p1", prompt: "fix it", agent: "claude", state: "completed" }] })],
    ["GET", /^\/v1\/vms\/[^/]+\/prompts\/p1\/logs$/, (_m, _b, url) => (url.searchParams.get("offset") ? { data_b64: "" } : { data_b64: b64("agent said hello\n"), next_offset: 17 })],
  ];

  const server = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (c) => { raw += c; });
    req.on("end", () => {
      const url = new URL(req.url, "http://x");
      const path = url.pathname;
      log.push(`${req.method} ${path}`);
      const send = (status, body) => {
        res.writeHead(status, { "Content-Type": "application/json", "X-Request-Id": "req_test" });
        res.end(JSON.stringify(body));
      };
      if (req.headers.authorization !== `Bearer ${KEY}`) return send(401, { error: { code: "unauthorized", message: "bad key", request_id: "req_test" } });
      if (req.method === "POST" && /\/exec$/.test(path)) {
        const spec = JSON.parse(raw);
        res.writeHead(200, { "Content-Type": "application/x-ndjson" });
        res.write(`${JSON.stringify({ stream: "stdout", data_b64: b64(`ran: ${spec.cmd.join(" ")}\n${XSS}\n`) })}\n`);
        res.write(`${JSON.stringify({ stream: "stderr", data_b64: b64("warn\n") })}\n`);
        return res.end(`${JSON.stringify({ exit: 3 })}\n`);
      }
      const table = features ? [...routes, ...featureRoutes] : routes;
      const match = table.map(([m, re, fn]) => (m === req.method ? [path.match(re), fn] : [null])).find(([m]) => m);
      if (!match) {
        res.writeHead(404, { "Content-Type": "text/plain" });
        return res.end("404 page not found\n");
      }
      let parsedBody = {};
      if (raw && (req.headers["content-type"] || "").includes("application/json")) {
        parsedBody = JSON.parse(raw);
      }
      const out = match[1](match[0], parsedBody, url, Buffer.byteLength(raw));
      return Array.isArray(out) ? send(out[0], out[1]) : send(200, clone(out));
    });
  });
  return new Promise((ok) => {
    server.listen(0, "127.0.0.1", () => ok({ url: `http://127.0.0.1:${server.address().port}`, log, db, close: () => { server.closeAllConnections(); server.close(); } }));
  });
}
