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
      { id: "vm_2", name: "stopped-one", size: "large", vcpu: 8, mem_mib: 16384, state: "stopped", ip: "10.200.0.3", image: "ubuntu-24.04", team: "t1", created_at: "2026-01-01T00:00:00Z", ports: [] },
    ],
    ports: [{ guest_port: 8000, hostname: "8000-vm_1.vm.example", auth: "private" }],
    snapshots: [{ id: "snap_1", name: XSS, created_at: "2026-01-01T00:00:00Z" }],
    secrets: [{ name: "API_TOKEN", updated: "2026-01-01T00:00:00Z" }],
    keys: [{ id: "k1", name: "operator", scopes: [], created: "2026-01-01T00:00:00Z" }],
    webhooks: [{ id: "wh1", url: "https://hooks.example/x", events: ["vm.*"], created: "2026-01-01T00:00:00Z" }],
    members: [{ user_id: "u1", role: "owner" }],
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
    ["PUT", /^\/v1\/secrets\/([^/]+)$/, ([, name]) => {
      db.secrets = [...db.secrets.filter((x) => x.name !== name), { name, updated: new Date().toISOString() }];
      return { ok: true };
    }],
    ["GET", /^\/v1\/keys$/, () => ({ keys: db.keys })],
    ["POST", /^\/v1\/keys$/, (_m, body) => {
      db.keys = [...db.keys, { id: "k2", name: body.name, scopes: body.scopes, created: new Date().toISOString() }];
      return [201, { id: "k2", name: body.name, scopes: body.scopes, token: "boat_newtoken_shownonce" }];
    }],
    ["GET", /^\/v1\/webhooks$/, () => ({ webhooks: db.webhooks })],
    ["GET", /^\/v1\/webhooks\/[^/]+\/deliveries$/, () => ({ deliveries: [{ id: "d1", event: XSS, attempt: 1, status: 500, ok: false, error: "boom", at: "2026-01-01T00:00:00Z" }] })],
    ["GET", /^\/v1\/teams\/([^/]+)$/, () => ({ id: "t1", name: "acme", role: "owner", members: db.members })],
  ];
  const featureRoutes = [
    ["GET", /^\/v1\/vms\/[^/]+\/commands$/, () => ({ commands: [{ id: "c1", cmd: ["sleep", "100"], state: "running", started_at: "2026-01-01T00:00:00Z" }] })],
    ["GET", /^\/v1\/vms\/[^/]+\/commands\/c1\/logs$/, (_m, _b, url) => (url.searchParams.get("offset") ? { data_b64: "", next_offset: 13, running: true } : { data_b64: b64("log line one\n"), next_offset: 13, running: true })],
    ["GET", /^\/v1\/vms\/[^/]+\/files\/list$/, () => ({ entries: [{ name: "readme.txt", type: "file", size: 12, mtime: "2026-01-01T00:00:00Z" }, { name: "src", type: "dir" }] })],
    ["GET", /^\/v1\/environments$/, () => ({ environments: [{ id: "e1", name: "base", image: "ubuntu-24.04", size: "small", env: { A: "1" }, is_default: true }] })],
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
      const out = match[1](match[0], raw ? JSON.parse(raw) : {}, url);
      return Array.isArray(out) ? send(out[0], out[1]) : send(200, clone(out));
    });
  });
  return new Promise((ok) => {
    server.listen(0, "127.0.0.1", () => ok({ url: `http://127.0.0.1:${server.address().port}`, log, close: () => { server.closeAllConnections(); server.close(); } }));
  });
}
