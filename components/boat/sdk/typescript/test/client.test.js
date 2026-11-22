import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createHmac } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Boat, BoatError, BoatTimeoutError, verifyWebhook, verifyWebhookAsync } from "../index.js";

const TOKEN = "boat_test";
const VM = { id: "vm1", name: "dev", size: "small", state: "running", team: "t1" };
const PATCH_KEYS = new Set(["name", "ttl_s", "extend_s", "labels", "idle_stop_s"]);
const log = [];
const files = new Map();
let server, url;

const b64 = (s) => Buffer.from(s).toString("base64");

before(async () => {
  server = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const body = Buffer.concat(chunks);
    log.push({ method: req.method, path: req.url, body: body.toString(), idem: req.headers["idempotency-key"] });
    const json = (code, o) => { res.writeHead(code, { "Content-Type": "application/json" }); res.end(JSON.stringify(o)); };
    if (req.headers.authorization !== `Bearer ${TOKEN}`) return json(401, { error: { code: "unauthorized", message: "bad key", request_id: "req_1" } });
    const k = `${req.method} ${req.url}`;
    if (k === "POST /v1/vms" && JSON.parse(body).count > 1) {
      const r = JSON.parse(body);
      return json(201, { vms: Array.from({ length: r.count }, (_, i) => ({ ...VM, id: `vm${i + 1}`, name: `${r.name}-${i + 1}`, ...(r.name === "envfail" && i === r.count - 1 ? { env_error: "push failed" } : {}) })) });
    }
    if (k === "POST /v1/vms" && JSON.parse(body).name === "envfail") return json(201, { ...VM, ...JSON.parse(body), env_error: "push failed", agent_ready: false });
    if (/^POST \/v1\/vms\/vmenv\/(start|fork|clone)$/.test(k)) return json(200, { ...VM, id: "vmenv2", env_error: "push failed", agent_ready: false });
    if (k === "POST /v1/vms/vm502b/start") return json(502, { error: { code: "env_update_failed", message: "env push failed" }, vm: { id: "vm502b", state: "running" } });
    if (k === "POST /v1/vms/vm502d/start") return json(502, { error: { code: "env_start_failed", message: "batch failed closed" }, vm: { id: "vm502d", state: "stopped" }, vms: [{ id: "vm502d", state: "stopped" }, { id: "vm502e", state: "running" }] });
    if (k === "POST /v1/vms/vm502c/start") return json(502, { error: { code: "env_start_failed", message: "no_env start failed" }, vm: { id: "vm502c", state: "stopped" } });
    if (k === "POST /v1/vms/vm502/start") return json(502, { error: { code: "env_update_failed", message: "env push failed" } });
    if (k === "POST /v1/vms") return json(201, { ...VM, ...JSON.parse(body) });
    if (req.method === "GET" && req.url.split("?")[0] === "/v1/usage") return json(200, { usage: [] });
    if (req.method === "GET" && req.url.split("?")[0] === "/v1/vms/vm1/usage") return json(200, { vm_id: "vm1", seconds: 42, by_size: { default: 42 } });
    if (k === "GET /v1/vms") return json(200, { vms: [VM] });
    if (k === "GET /v1/vms/vm1") return json(200, VM);
    if (req.method === "PATCH" && req.url.startsWith("/v1/vms/")) {
      let patch;
      try {
        patch = JSON.parse(body);
      } catch {
        return json(400, { error: { code: "invalid_json", message: "malformed json" } });
      }
      const keys = patch && typeof patch === "object" && !Array.isArray(patch) ? Object.keys(patch) : [];
      if (keys.length === 0 || keys.some((key) => !PATCH_KEYS.has(key))) {
        return json(400, { error: { code: "invalid_json", message: "unknown or empty patch field" } });
      }
      if (req.url === "/v1/vms/vm1") return json(200, { ...VM, ...patch });
    }
    if (k === "POST /v1/vms/vm1/start") return json(200, { ...VM, state: "running", ...(body.length ? JSON.parse(body) : {}) });
    if (k === "POST /v1/vms/vm1/fork") return json(201, { ...VM, id: "vm2", name: JSON.parse(body).name });
    if (k === "POST /v1/vms/vm1/clone") return json(201, { ...VM, id: "vm3", name: JSON.parse(body).name ?? "dev-clone" });
    if (k === "POST /v1/vms/vm1/stop") return json(200, { ...VM, state: "stopped" });
    if (k === "POST /v1/vms/vm1/ssh") return json(200, { host: "10.0.0.5", port: 22, user: "dev", jump: "jump.example:22" });
    if (k === "GET /v1/vms/vm1/ssh") return json(200, { keys: [{ fingerprint: "SHA256:abc+def/" }] });
    if (req.method === "DELETE" && req.url.startsWith("/v1/vms/") && req.url.includes("/ssh/")) {
      const fpEnc = req.url.split("/ssh/")[1];
      if (!fpEnc || fpEnc.includes("/")) return json(404, { error: { code: "not_found", message: "not found" } });
      return json(200, { ok: true, fingerprint: decodeURIComponent(fpEnc) });
    }
    if (req.method === "POST" && req.url.startsWith("/v1/vms/") && req.url.endsWith("/port-tokens/revoke")) {
      const mid = req.url.slice("/v1/vms/".length, -"/port-tokens/revoke".length);
      if (!mid || mid.includes("/")) return json(404, { error: { code: "not_found", message: "not found" } });
      return json(200, { ok: true, generation: 2 });
    }
    if (k === "PUT /v1/teams/t1/self-token" || k === "PUT /v1/teams/t1/share-allowlist") {
      return json(200, JSON.parse(body));
    }
    if (k === "GET /v1/teams/t1") {
      return json(200, { id: "t1", name: "acme", role: "owner", members: [{ user_id: "u1", role: "owner" }], created: "2026-01-01T00:00:00Z" });
    }
    if (req.method === "DELETE" && req.url.startsWith("/v1/teams/") && req.url.includes("/members/")) {
      return json(200, { ok: true });
    }
    if (k === "POST /v1/keys") {
      const r = body.length ? JSON.parse(body) : {};
      return json(201, {
        id: "k2", user_id: "u1", team_id: r.team_id ?? "t1", name: r.name ?? "key",
        scopes: r.scopes ?? [], created: "2026-01-01T00:00:00Z", token: "boat_new_tok",
        ...(r.expires_in_s ? { expires: "2026-01-02T00:00:00Z" } : {}),
      });
    }
    if (req.method === "GET" && (req.url === "/v1/events" || req.url.startsWith("/v1/events?"))) {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(": keepalive\n\nid: 11\nevent: vm.star");
      setTimeout(() => {
        res.write('ted\ndata: {"seq":11,"type":"vm.started","vm_id":"vm1"}\n\n');
        res.end('id: 12\nevent: vm.stopped\ndata: {"seq":12,"type":"vm.stopped","vm_id":"vm1"}\n\n');
      }, 5);
      return;
    }
    if (k === "POST /v1/vms/vm1/exec" && body.toString().includes("tar xzf")) {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      return res.end(`{"stream":"stdout","data_b64":"${b64(`/home/dev/${JSON.parse(body).cmd[3]}\n`)}"}\n{"exit":0}\n`);
    }
    if (k === "POST /v1/vms/vm1/exec" && JSON.parse(body).timeout_s === 5) {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      return res.end(`{"exit":143,"signal":"SIGTERM","timed_out":true}\n`);
    }
    if (k === "POST /v1/vms/vm1/exec" || k === "POST /v1/vms/vm1/prompt") {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      // split a line across writes to exercise buffering
      res.write(`{"stream":"stdout","data_b64":"${b64("hello ")}"}\n{"stream":"std`);
      setTimeout(() => {
        res.write(`out","data_b64":"${b64("world\n")}"}\n{"stream":"stderr","data_b64":"${b64("oops\n")}"}\n`);
        res.end(`{"exit":2}\n`);
      }, 10);
      return;
    }
    if (req.url.startsWith("/v1/vms/vm1/files?path=")) {
      if (req.method === "PUT") { files.set(req.url, body); return json(200, { ok: true }); }
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      return res.end(files.get(req.url) ?? Buffer.alloc(0));
    }
    if (k === "GET /v1/secrets") return json(200, { secrets: [{ name: "OPENAI_API_KEY", inject: true, updated: "2026-01-01T00:00:00Z" }] });
    if (k === "PUT /v1/secrets/OPENAI_API_KEY") return json(200, { name: "OPENAI_API_KEY" });
    if (k === "POST /v1/vms/vm1/ports") return json(201, { ...JSON.parse(body), guest_port: 8080 });
    if (k === "GET /v1/vms/vm1/ports/8080/url?ttl_s=600") return json(200, { url: "https://8080-vm1.vm.example/?boat_exp=1&boat_sig=ab", token: "1.ab", auth: "private", guest_port: 8080 });
    if (k === "POST /v1/vms/vm1/commands") return json(201, { id: "c_1", cmd: JSON.parse(body).cmd, state: "running" });
    if (req.url.startsWith("/v1/vms/vm1/commands/c_1/logs?follow=1")) {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      return res.end(`{"stream":"stdout","data_b64":"${b64("out")}","next_offset":3}\n{"exit":4}\n`);
    }
    if (k === "POST /v1/vms/vm1/prompts") return json(202, { id: "prm_1", conversation_id: "conv_1", state: "running" });
    if (k === "POST /v1/vms/vm1/prompts/prm_1/steer") return json(202, { id: "prm_2", conversation_id: "conv_1", state: "running" });
    if (k === "POST /v1/environments") return json(201, { ...JSON.parse(body), id: "env_1", version: 1 });
    if (k === "PUT /v1/environments/base") return json(200, { ...JSON.parse(body), id: "env_1", name: "base", version: 2 });
    if (req.method === "POST" && req.url.startsWith("/v1/environments/") && req.url.endsWith("/upgrade")) {
      const ref = decodeURIComponent(req.url.slice("/v1/environments/".length, -"/upgrade".length));
      const parsed = body.length ? JSON.parse(body) : {};
      const ids = parsed.agentIds;
      const upgraded = Array.isArray(ids) ? ids.length : 3;
      const live = Math.min(upgraded, 2);
      const stopped = Math.max(0, upgraded - live);
      return json(200, { environment: ref, version: 2, upgraded, upgraded_live: live, upgraded_stopped: stopped, failed: 0 });
    }
    if (req.method === "GET" && (req.url === "/v1/snapshots" || req.url.startsWith("/v1/snapshots?"))) {
      const vis = new URL(req.url, "http://localhost").searchParams.get("visibility") || "default";
      return json(200, { snapshots: [{ id: "s1", visibility: vis }] });
    }
    if (k === "POST /v1/keys/k1/rotate") return json(200, { id: "k1", token: "boat_new" });
    if (k === "POST /v1/webhooks/w1/rotate") return json(200, { id: "w1", secret: "whsec_new" });
    if (k === "PUT /v1/ai/credentials/anthropic") return json(200, { provider: "anthropic", last4: "1234", updated: "x" });
    if (k === "POST /v1/vms/vm1/resize") return json(200, { ...VM, size: JSON.parse(body).size });
    if (k === "POST /v1/vms/vm1/snapshots/s1/shares") return json(201, { id: "shr1", snapshot_id: "s1", uses: 0, code: "bsh_x" });
    if (k === "GET /v1/vms/vm1/snapshots/s1/download") { res.writeHead(200); return res.end("ARCHIVE"); }
    if (req.url.startsWith("/v1/vms/vm1/files/search?")) return json(200, { matches: [{ path: "/a" }], truncated: false });
    if (k === "PUT /v1/telemetry") return json(200, { configured: true, endpoint: JSON.parse(body).endpoint });
    if (k === "DELETE /v1/vms/vm1") return json(200, { ok: true });
    json(404, { error: { code: "not_found", message: "vm not found", request_id: "req_2" } });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  url = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

test("createVM sends explicit ttlS 0 and omits ttl_s when unset", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.createVM({ name: "dev", ttlS: 0 });
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "dev", ttl_s: 0 });
  await boat.createVM({ name: "dev" });
  assert.equal("ttl_s" in JSON.parse(log.at(-1).body), false);
});

test("create -> prompt -> fork -> stop", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const vm = await boat.createVM({ name: "dev", size: "small", ttlS: 3600 });
  assert.equal(vm.id, "vm1");
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "dev", size: "small", ttl_s: 3600 });
  let out = "";
  let exit = null;
  for await (const ev of boat.prompt(vm.id, "write tests", { agent: "opencode" })) {
    if (ev.stream === "stdout") out += new TextDecoder().decode(ev.data);
    if (ev.exitCode !== null) exit = ev.exitCode;
  }
  assert.equal(out, "hello world\n");
  assert.equal(exit, 2);
  assert.deepEqual(JSON.parse(log.at(-1).body), { prompt: "write tests", agent: "opencode" });
  const fork = await boat.fork(vm.id, { name: "dev-2" });
  assert.equal(fork.id, "vm2");
  assert.equal((await boat.stop(vm.id)).state, "stopped");
});

test("run collects output", async () => {
  const boat = new Boat({ url, key: TOKEN });
  assert.deepEqual(await boat.run("vm1", ["ls"]), { stdout: "hello world\n", stderr: "oops\n", exitCode: 2 });
});

test("files round-trip with escaping", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.writeFile("vm1", "/tmp/a b.txt", new Uint8Array([0, 1, 2]));
  assert.deepEqual([...(await boat.readFile("vm1", "/tmp/a b.txt"))], [0, 1, 2]);
  assert.match(log.at(-1).path, /path=%2Ftmp%2Fa%20b\.txt/);
});

test("errors carry status, code and request id", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await assert.rejects(boat.getVM("nope"), (e) => e instanceof BoatError && e.status === 404 && e.code === "not_found" && e.requestId === "req_2");
  await assert.rejects(new Boat({ url, key: "wrong" }).listVMs(), (e) => e.status === 401);
  assert.throws(() => new Boat({ url: "", key: "" }));
});

test("BoatError preserves its constructor and leaves unknown cursors undefined", () => {
  for (const requestId of [undefined, "req_1"]) {
    const error = new BoatError(503, "unavailable", "try later", requestId);
    assert.ok(error instanceof Error);
    assert.equal(error.name, "BoatError");
    assert.equal(error.status, 503);
    assert.equal(error.code, "unavailable");
    assert.equal(error.requestId, requestId ?? "");
    assert.equal(error.message, `try later (HTTP 503${requestId ? ", " + requestId : ""})`);
    assert.equal(error.nextOffset, undefined);
  }
});

test("secrets and delete", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.setSecret("OPENAI_API_KEY", "sk");
  assert.deepEqual(JSON.parse(log.at(-1).body), { value: "sk" });
  await boat.setSecret("OPENAI_API_KEY", "sk", { inject: true });
  assert.deepEqual(JSON.parse(log.at(-1).body), { value: "sk", inject: true });
  const listed = await boat.secrets();
  assert.deepEqual(listed, [{ name: "OPENAI_API_KEY", inject: true, updated: "2026-01-01T00:00:00Z" }]);
  await boat.delete("vm1");
});

test("ports auth + url and per-VM secrets", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.addPort("vm1", 8080);
  assert.deepEqual(JSON.parse(log.at(-1).body), { port: 8080, auth: "private" });
  await boat.addPort("vm1", 8080, { public: true });
  assert.equal(JSON.parse(log.at(-1).body).auth, "public");
  assert.equal((await boat.portUrl("vm1", 8080, { ttlS: 600 })).token, "1.ab");
  await boat.createVM({ name: "x", secrets: [] });
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "x", secrets: [] });
});

test("detached commands and follow", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const c = await boat.startCommand("vm1", ["sleep", "9"], { cwd: "/tmp" });
  assert.equal(c.id, "c_1");
  assert.deepEqual(JSON.parse(log.at(-1).body), { cmd: ["sleep", "9"], cwd: "/tmp" });
  const evs = [];
  for await (const ev of boat.followCommand("vm1", "c_1")) evs.push(ev);
  assert.equal(new TextDecoder().decode(evs[0].data), "out");
  assert.equal(evs[0].nextOffset, 3);
  assert.equal(evs.at(-1).exitCode, 4);
});

for (const [follow, resource] of [["followCommand", "commands"], ["followPrompt", "prompts"]]) {
  test(`${follow} exposes a clamped cursor before any output for explicit resume`, async () => {
    const requests = [];
    const output = Buffer.from("🐼\n");
    const boat = new Boat({ url: "https://boat.example", key: TOKEN, fetch: async (input) => {
      const u = new URL(input);
      requests.push(u.pathname + u.search);
      // The API clamps offset 9999 to 1004 with empty output, then the guest returns 503.
      if (requests.length === 1) return new Response('{"reconnect":true,"next_offset":1004}\n');
      const body = Buffer.from([
        JSON.stringify({ stream: "stdout", data_b64: b64(output.subarray(0, 2)), next_offset: 1006 }),
        JSON.stringify({ stream: "stdout", data_b64: b64(output.subarray(2)), next_offset: 1009 }),
        JSON.stringify({ exit: 0 }),
      ].join("\n"));
      return new Response(new ReadableStream({ start(controller) {
        controller.enqueue(body.subarray(0, 19));
        controller.enqueue(body.subarray(19));
        controller.close();
      } }));
    } });
    const stream = boat[follow]("vm1", "run_1", { offset: 9999 });
    let reconnect;
    await assert.rejects(stream.next(), (error) => {
      assert.ok(error instanceof BoatError);
      assert.equal(error.status, 409);
      assert.equal(error.code, "reconnect");
      assert.equal(error.requestId, "");
      assert.equal(error.nextOffset, 1004);
      reconnect = error;
      return true;
    });
    const path = `/v1/vms/vm1/${resource}/run_1/logs?follow=1&offset=`;
    assert.deepEqual(requests, [path + "9999"]);
    assert.deepEqual(await stream.next(), { value: undefined, done: true });
    const events = [];
    for await (const event of boat[follow]("vm1", "run_1", { offset: reconnect.nextOffset })) events.push(event);
    assert.deepEqual(requests, [path + "9999", path + "1004"]);
    assert.deepEqual(Buffer.concat(events.map((event) => event.data)), output);
    assert.deepEqual(events.map((event) => event.nextOffset), [1006, 1009, null]);
    assert.deepEqual(events.map((event) => event.stream), ["stdout", "stdout", ""]);
    assert.deepEqual(events.map((event) => event.exitCode), [null, null, 0]);
    assert.deepEqual(events.map((event) => event.error), [null, null, null]);
  });

  test(`${follow} uses only the reconnect wire cursor, including zero`, async (t) => {
    for (const [name, cursor] of [["zero", 0], ["missing", undefined], ["null", null]]) {
      await t.test(name, async () => {
        const boat = new Boat({ url: "https://boat.example", key: TOKEN, fetch: async () => new Response(
          JSON.stringify({ stream: "stdout", data_b64: b64("prior"), next_offset: 1009 }) + "\n" +
          JSON.stringify({ reconnect: true, next_offset: cursor }),
        ) });
        const stream = boat[follow]("vm1", "run_1", { offset: 1004 });
        assert.equal((await stream.next()).value.nextOffset, 1009);
        await assert.rejects(stream.next(), (error) => {
          assert.ok(error instanceof BoatError);
          assert.equal(error.code, "reconnect");
          assert.equal(error.nextOffset, cursor ?? undefined);
          return true;
        });
      });
    }
  });
}

test("async prompt, steer, rotation, env, AI, resize, share", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const run = await boat.startPrompt("vm1", "go", { agent: "claude", new: true });
  assert.deepEqual(JSON.parse(log.at(-1).body), { prompt: "go", agent: "claude", new: true });
  assert.equal((await boat.steerPrompt("vm1", run.id, "stop")).id, "prm_2");
  assert.equal((await boat.rotateKey("k1", { overlapS: 0 })).token, "boat_new");
  assert.deepEqual(JSON.parse(log.at(-1).body), { overlap_s: 0 });
  assert.equal((await boat.rotateWebhook("w1")).secret, "whsec_new");
  const e = await boat.createEnvironment("base", { setupScript: "npm ci", secrets: [] });
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "base", setup_script: "npm ci", secrets: [] });
  assert.equal(e.version, 1);
  assert.equal((await boat.setAICredential("anthropic", "sk-1234")).last4, "1234");
  assert.equal((await boat.resize("vm1", { size: "large", discardMemory: true })).size, "large");
  assert.deepEqual(JSON.parse(log.at(-1).body), { size: "large", discard_memory: true });
  assert.equal((await boat.shareSnapshot("vm1", "s1", { ttlS: 60 })).code, "bsh_x");
  assert.equal(new TextDecoder().decode(await boat.downloadSnapshot("vm1", "s1")), "ARCHIVE");
  assert.equal((await boat.searchFiles("vm1", "/", "a", { content: true }))[0].path, "/a");
  assert.match(log.at(-1).path, /content=1/);
  assert.equal((await boat.setTelemetry("https://o.example")).configured, true);
});

test("verifyWebhook during a rotation accepts either secret", () => {
  const sign = (sec) => createHmac("sha256", sec).update("1000.{}").digest("hex");
  const h = `t=1000,v1=${sign("new")},v1=${sign("old")}`;
  assert.equal(verifyWebhook("new", h, "{}", { now: 1000 }), true);
  assert.equal(verifyWebhook("old", h, "{}", { now: 1000 }), true);
  assert.equal(verifyWebhook("other", h, "{}", { now: 1000 }), false);
});

test("verifyWebhook", () => {
  const body = '{"type":"vm.stopped"}';
  const sig = createHmac("sha256", "whsec_x").update("1000." + body).digest("hex");
  const h = `t=1000,v1=${sig}`;
  assert.equal(verifyWebhook("whsec_x", h, body, { now: 1001 }), true);
  assert.equal(verifyWebhook("whsec_x", h, body + " ", { now: 1001 }), false);
  assert.equal(verifyWebhook("whsec_x", h, body, { now: 9999 }), false);
  assert.equal(verifyWebhook("whsec_x", "junk", body), false);
});

test("browser module import and ordinary API calls need no Node builtins", () => {
  execFileSync(process.execPath, ["--experimental-vm-modules", "--no-warnings", "--input-type=module", "-e", `
    import assert from "node:assert/strict";
    import { readFileSync } from "node:fs";
    import { createContext, runInContext, SourceTextModule } from "node:vm";
    const src = readFileSync(0, "utf8");
    const requests = [];
    const context = createContext({ TextDecoder, TextEncoder, URLSearchParams, crypto: globalThis.crypto,
      fetch: async (url, options) => {
        requests.push([url, options.headers.Authorization, options.body ?? ""]);
        if (url.endsWith("/exec")) {
          return new Response('{"exit":0}\\n');
        }
        return new Response('{"vms":[{"id":"browser-vm"}]}');
      },
    });
    const module = new SourceTextModule(src, { context,
      importModuleDynamically: (specifier) => { throw new Error("browser cannot import " + specifier); },
    });
    await module.link((specifier) => { throw new Error("browser cannot import " + specifier); });
    await module.evaluate();
    const boat = new module.namespace.Boat({ url: "https://boat.example", key: "browser-key" });
    const browserVm = (await boat.listVMs())[0];
    assert.equal(browserVm.id, "browser-vm");
    await browserVm.run(["cat"], { stdin: "edge-stdin", tty: true });
    assert.deepEqual(requests[0], ["https://boat.example/v1/vms", "Bearer browser-key", ""]);
    assert.deepEqual(JSON.parse(requests[1][2]), { cmd: ["cat"], stdin_b64: "ZWRnZS1zdGRpbg==", tty: true });
    assert.equal(typeof module.namespace.verifyWebhook, "function");
    assert.equal(typeof module.namespace.verifyWebhookAsync, "function");
    const ContextError = runInContext("Error", context);
    assert.throws(
      () => module.namespace.verifyWebhook("whsec_x", "t=1000,v1=deadbeef", "{}", { now: 1000 }),
      (e) => e instanceof module.namespace.BoatError && e instanceof ContextError && e.code === "unsupported_runtime" && /node:crypto/i.test(e.message),
    );
    const edgeSig = ${JSON.stringify(createHmac("sha256", "whsec_edge").update("1000.{\"ok\":true}").digest("hex"))};
    assert.equal(
      await module.namespace.verifyWebhookAsync("whsec_edge", "t=1000,v1=" + edgeSig, '{"ok":true}', { toleranceS: 300, now: 1005 }),
      true,
    );
    assert.equal(
      await module.namespace.verifyWebhookAsync("whsec_edge", "t=1000,v1=" + edgeSig, '{"ok":false}', { toleranceS: 300, now: 1005 }),
      false,
    );
    assert.equal(await module.namespace.verifyWebhookAsync("whsec_edge", null, '{"ok":true}'), false);
    assert.equal(await module.namespace.verifyWebhookAsync("whsec_edge", undefined, '{"ok":true}'), false);
    const shimmed = createContext({ TextDecoder, TextEncoder, URLSearchParams, process: {}, fetch: context.fetch });
    const shimmedMod = new SourceTextModule(src, { context: shimmed,
      importModuleDynamically: (specifier) => { throw new Error("browser cannot import " + specifier); },
    });
    await shimmedMod.link((specifier) => { throw new Error("browser cannot import " + specifier); });
    await shimmedMod.evaluate();
    assert.throws(() => new shimmedMod.namespace.Boat());
  `], { input: readFileSync(new URL("../index.js", import.meta.url)), stdio: ["pipe", "pipe", "pipe"] });

  execFileSync(process.execPath, ["--no-warnings", "--input-type=module", "-e", `
    import assert from "node:assert/strict";
    import { register } from "node:module";
    import { pathToFileURL } from "node:url";
    const hook = 'export async function resolve(specifier, context, nextResolve) {' +
      'if (specifier.startsWith("node:") && context.parentURL?.endsWith("/sdk/typescript/index.js")) {' +
      '  throw new TypeError("browser cannot resolve " + specifier);' +
      '}' +
      'return nextResolve(specifier, context);' +
    '}';
    register("data:text/javascript," + encodeURIComponent(hook));
    const origProcess = globalThis.process;
    Object.defineProperty(globalThis, "process", { value: undefined, configurable: true });
    try {
      const sdk = await import(pathToFileURL(${JSON.stringify(new URL("../index.js", import.meta.url).pathname)}).href);
      const boat = new sdk.Boat({ url: "https://boat.example", key: "k", fetch: async () => new Response('{"vms":[{"id":"loader-vm"}]}') });
      assert.equal((await boat.listVMs())[0].id, "loader-vm");
      assert.throws(() => sdk.verifyWebhook("s", "t=1,v1=aa", "{}"), (e) => e instanceof sdk.BoatError && e.code === "unsupported_runtime");
    } finally {
      Object.defineProperty(globalThis, "process", { value: origProcess, configurable: true });
    }
  `], { stdio: ["ignore", "pipe", "pipe"] });
});

function tree() {
  const root = join(mkdtempSync(join(tmpdir(), "boat-push-")), "proj");
  mkdirSync(join(root, "a"), { recursive: true });
  writeFileSync(join(root, "run.sh"), "x", { mode: 0o755 });
  writeFileSync(join(root, "a", "b.txt"), "x");
  writeFileSync(join(root, "gone.txt"), "x");
  writeFileSync(join(root, "ignored.log"), "x");
  writeFileSync(join(root, ".gitignore"), "*.log\n");
  symlinkSync("run.sh", join(root, "link"));
  return root;
}
const git = (root, ...args) => execFileSync("git", ["-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com", ...args]);
function listTar(bytes) {
  const dir = mkdtempSync(join(tmpdir(), "boat-tar-"));
  writeFileSync(join(dir, "x.tgz"), bytes);
  return execFileSync("tar", ["tzvf", join(dir, "x.tgz")]).toString();
}
const lastPut = () => [...log].reverse().find((l) => l.method === "PUT" && l.path.includes("boat-push"));
const lastPutBytes = () => files.get(lastPut().path);

test("push sends git's view of a repository, and everything but .git elsewhere", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const root = tree();
  let got = await boat.push("vm1", root);
  assert.deepEqual(got, { path: "/home/dev/proj", files: 6 });
  let listing = listTar(lastPutBytes());
  assert.match(listing, /gone\.txt/);
  assert.match(listing, /ignored\.log/);

  git(root, "init", "-q");
  git(root, "add", ".");
  git(root, "commit", "-qm", "x");
  writeFileSync(join(root, "new.txt"), "n");
  execFileSync("rm", [join(root, "gone.txt")]);
  got = await boat.push("vm1", root, { to: "/srv/api" });
  assert.equal(got.path, "/home/dev//srv/api");
  listing = listTar(lastPutBytes());
  for (const want of ["run.sh", "a/b.txt", "new.txt", ".gitignore"]) assert.ok(listing.includes(want), want);
  for (const bad of ["gone.txt", "ignored.log", ".git/"]) assert.ok(!listing.includes(bad), bad);
  assert.match(listing, /-rwxr-xr-x.*run\.sh/);
  assert.match(listing, /link -> run\.sh/);
  await assert.rejects(boat.push("vm1", "/no/such/dir"), /not a directory|ENOENT/);
});

test("runJob deletes, returns the exit code, gives 124 on a timeout, runs in the pushed dir", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const deletes = () => log.filter((l) => l.method === "DELETE" && l.path === "/v1/vms/vm1").length;
  let before = deletes();
  let res = await boat.runJob(["true"], { size: "large" });
  assert.equal(res.exitCode, 2);
  assert.equal(res.deleted, true);
  assert.equal(res.vmId, "vm1");
  assert.equal(deletes(), before + 1);
  const created = [...log].reverse().find((l) => l.method === "POST" && l.path === "/v1/vms");
  assert.deepEqual(JSON.parse(created.body), { idle_stop_s: 900, ttl_s: 900, size: "large" });

  before = deletes();
  res = await boat.runJob(["sleep", "9"], { timeoutS: 5, keep: true });
  assert.equal(res.exitCode, 124);
  assert.equal(res.deleted, false);
  assert.equal(deletes(), before);

  await boat.runJob(["make"], { push: tree() });
  const execs = () => log.filter((l) => l.path === "/v1/vms/vm1/exec").map((l) => JSON.parse(l.body));
  assert.equal(execs().at(-1).cwd, "/home/dev/proj");
  await boat.runJob(["make"], { cwd: "sub" });
  assert.deepEqual(execs().at(-1).cmd.slice(3), ["sub", "make"]);

  const creates = log.filter((l) => l.path === "/v1/vms").length;
  await assert.rejects(boat.runJob(["make"], { push: "/no/such/dir" }));
  assert.equal(log.filter((l) => l.path === "/v1/vms").length, creates);
});

test("runJob with push and cwd still uploads, and cwd sets the working dir", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const start = log.length;
  await boat.runJob(["make"], { push: tree(), cwd: "/work" });
  const execs = log.slice(start).filter((l) => l.path === "/v1/vms/vm1/exec").map((l) => JSON.parse(l.body));
  assert.ok(execs.length >= 2, "push (unpack) exec runs before the command");
  assert.ok(execs.slice(0, -1).some((e) => JSON.stringify(e.cmd).includes("tar")));
  assert.equal(execs.at(-1).cwd, "/work");
  assert.deepEqual(execs.at(-1).cmd, ["make"]);
});

test("idempotencyKey is sent as Idempotency-Key on create, createVMs and fork", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const last = () => log.at(-1);
  await boat.createVM({ name: "dev", idempotencyKey: "k-create" });
  assert.equal(last().path, "/v1/vms");
  assert.equal(last().idem, "k-create");
  assert.equal("idempotencyKey" in JSON.parse(last().body), false);
  await boat.createVMs(2, { name: "many", idempotencyKey: "k-many" });
  assert.equal(last().idem, "k-many");
  await boat.fork("vm1", { name: "f", idempotencyKey: "k-fork" });
  assert.equal(last().path, "/v1/vms/vm1/fork");
  assert.equal(last().idem, "k-fork");
  await boat.fork("vm1", "named", { idempotencyKey: "k-fork2" });
  assert.equal(last().idem, "k-fork2");
  await boat.createVM({ name: "plain" });
  assert.equal(last().idem, undefined);
  await assert.rejects(boat.createVM({ idempotencyKey: "" }), TypeError);
  await assert.rejects(boat.fork("vm1", { idempotencyKey: "x".repeat(256) }), TypeError);
});

test("usage serializes Date bounds as RFC3339 UTC", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.usage({ from: new Date("2026-01-01T00:00:00Z"), to: new Date("2026-02-01T03:00:00+02:00") });
  assert.deepEqual(Object.fromEntries(new URL(url + log.at(-1).path).searchParams), { from: "2026-01-01T00:00:00.000Z", to: "2026-02-01T01:00:00.000Z" });
});

test("usage passes from and to query params", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.usage({ from: "2026-01-01", to: "2026-02-01" });
  assert.deepEqual(Object.fromEntries(new URL(url + log.at(-1).path).searchParams), { from: "2026-01-01", to: "2026-02-01" });
  await boat.usage({ to: "2026-02-01" });
  assert.equal(log.at(-1).path, "/v1/usage?to=2026-02-01");
});

test("runJob waits for setup before push/exec and preserves cleanup on setup failures", async (t) => {
  const requests = [];
  let created, setupBody, setupStatus, setupFinished;
  const fixture = createServer(async (req, res) => {
    for await (const chunk of req) {} // consume uploads before responding
    requests.push(`${req.method} ${req.url}`);
    if (req.method === "POST" && req.url === "/v1/vms") {
      res.writeHead(201, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ ...VM, ...created }));
    }
    if (req.method === "GET") {
      res.writeHead(setupStatus, { "Content-Type": "application/x-ndjson" });
      res.write(`{"stream":"stdout","data_b64":"${b64("setting up")}"}\n`);
      return setTimeout(() => {
        setupFinished = true;
        res.end(setupBody);
      }, 20);
    }
    if (req.method === "POST") {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      return res.end('{"exit":0}\n');
    }
    res.end("{}");
  });
  t.after(() => new Promise((resolve) => fixture.close(resolve)));
  await new Promise((resolve) => fixture.listen(0, "127.0.0.1", resolve));
  const boat = new Boat({ url: `http://127.0.0.1:${fixture.address().port}`, key: TOKEN });
  const logs = "GET /v1/vms/vm1/commands/setup_1/logs?follow=1&offset=0";
  const root = tree();
  for (const [name, fields, body, status, code, message] of [
    ["success", { setup_cmd_id: "setup_1" }, '{"exit":0}\n', 200],
    ["nonzero exit", { setup_cmd_id: "setup_1" }, '{"exit":7}\n', 200, "setup_failed", /7/],
    ["signal", { setup_cmd_id: "setup_1" }, '{"signal":"SIGTERM"}\n', 200, "setup_failed", /SIGTERM/],
    ["missing exit", { setup_cmd_id: "setup_1" }, "", 200, "setup_failed", /exit/],
    ["start failure", { setup_error: "could not start the setup script" }, "", 200, "setup_failed", /could not start/],
    ["start failure with command id", { setup_error: "could not start", setup_cmd_id: "setup_1" }, "", 200, "setup_failed", /could not start/],
    ["env failure", { env_error: "push failed", agent_ready: false }, "", 200, "env_failed", /push failed/],
    ["HTTP failure", { setup_cmd_id: "setup_1" }, "unavailable", 503, "http_error", /HTTP 503/],
  ]) {
    for (const keep of [false, true]) {
      await t.test(`${name}, keep=${keep}`, async () => {
        requests.length = 0;
        created = fields;
        setupBody = body;
        setupStatus = status;
        setupFinished = false;
        const job = boat.runJob(["true"], { push: root, keep });
        if (code) {
          await assert.rejects(job, (error) => {
            assert.ok(error instanceof BoatError);
            assert.equal(error.code, code);
            assert.match(error.message, message);
            return true;
          });
          assert.deepEqual(requests, ["POST /v1/vms", ...(fields.setup_error || fields.env_error ? [] : [logs]), ...(!keep ? ["DELETE /v1/vms/vm1"] : [])]);
        } else {
          const result = await job;
          assert.equal(result.exitCode, 0);
          assert.equal(result.deleted, !keep);
          assert.equal(setupFinished, true);
          assert.deepEqual(requests.slice(0, 2), ["POST /v1/vms", logs]);
          assert.match(requests[2], /^PUT \/v1\/vms\/vm1\/files\?path=/);
          assert.deepEqual(requests.slice(3), ["POST /v1/vms/vm1/exec", "POST /v1/vms/vm1/exec", ...(!keep ? ["DELETE /v1/vms/vm1"] : [])]);
        }
      });
    }
  }
});

test("runJob rejects non-positive timeoutS immediately and enforces end-to-end deadline + reconnect resume", async (t) => {
  const requests = [];
  const execPayloads = [];
  let setupStreams = [];
  let hangPush = false;
  let hangExec = false;
  let setupDelayMs = 0;

  const fixture = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const rawBody = Buffer.concat(chunks).toString();
    requests.push(`${req.method} ${req.url}`);
    if (req.method === "POST" && req.url === "/v1/vms") {
      res.writeHead(201, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ ...VM, setup_cmd_id: setupStreams.length ? "setup_1" : undefined }));
    }
    if (req.method === "GET" && req.url.startsWith("/v1/vms/vm1/commands/setup_1/logs?")) {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      const nextStream = setupStreams.length > 1 ? setupStreams.shift() : setupStreams[0] ?? '{"exit":0}\n';
      if (setupDelayMs > 0) await new Promise((r) => setTimeout(r, setupDelayMs));
      return res.end(nextStream);
    }
    if (req.method === "PUT" && req.url.startsWith("/v1/vms/vm1/files?")) {
      if (hangPush) return; // let client signal abort
      res.writeHead(200, { "Content-Type": "application/json" });
      return res.end('{"ok":true}');
    }
    if (req.method === "POST" && req.url === "/v1/vms/vm1/exec") {
      const parsed = JSON.parse(rawBody);
      if (rawBody.includes("tar xzf")) {
        res.writeHead(200, { "Content-Type": "application/x-ndjson" });
        return res.end(`{"stream":"stdout","data_b64":"${b64(`/home/dev/${parsed.cmd[3]}\n`)}"}\n{"exit":0}\n`);
      }
      execPayloads.push(parsed);
      if (hangExec) return; // let client signal abort
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      return res.end('{"exit":0}\n');
    }
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end("{}");
  });
  t.after(() => new Promise((resolve) => fixture.close(resolve)));
  await new Promise((resolve) => fixture.listen(0, "127.0.0.1", resolve));
  const boat = new Boat({ url: `http://127.0.0.1:${fixture.address().port}`, key: TOKEN });
  const root = tree();

  // 1. Non-positive timeoutS throws BoatTimeoutError immediately before createVM.
  for (const bad of [0, -1, -0.25, Number.NaN]) {
    requests.length = 0;
    await assert.rejects(
      boat.runJob(["true"], { timeoutS: bad }),
      (e) => e instanceof BoatTimeoutError && e instanceof BoatError && e.code === "timeout",
    );
    assert.deepEqual(requests, []);
  }

  // 2. Reconnect frames resume at next_offset across multiple segments and pass remaining budget to exec.
  requests.length = 0;
  execPayloads.length = 0;
  setupDelayMs = 80;
  setupStreams = [
    `{"stream":"stdout","data_b64":"${b64("abc")}","next_offset":3}\n{"reconnect":true,"next_offset":3}\n`,
    `{"reconnect":true}\n`, // missing next_offset preserves offset=3
    `{"stream":"stdout","data_b64":"${b64("def\n")}","next_offset":9}\n{"reconnect":true,"next_offset":9}\n`,
    `{"exit":0}\n`,
  ];
  let jobRes = await boat.runJob(["make"], { push: root, timeoutS: 3 });
  assert.equal(jobRes.exitCode, 0);
  assert.equal(jobRes.deleted, true);
  assert.deepEqual(
    requests.filter((r) => r.startsWith("GET ")),
    [
      "GET /v1/vms/vm1/commands/setup_1/logs?follow=1&offset=0",
      "GET /v1/vms/vm1/commands/setup_1/logs?follow=1&offset=3",
      "GET /v1/vms/vm1/commands/setup_1/logs?follow=1&offset=3",
      "GET /v1/vms/vm1/commands/setup_1/logs?follow=1&offset=9",
    ],
  );
  assert.ok(execPayloads.at(-1).timeout_s >= 1 && execPayloads.at(-1).timeout_s <= 3);

  // 3. Repeated reconnects obey deadlineMs and fail with setup_failed timed out + cleanup.
  requests.length = 0;
  setupDelayMs = 40;
  setupStreams = [
    `{"stream":"stderr","data_b64":"${b64("warming cache\n")}","next_offset":14}\n{"reconnect":true,"next_offset":14}\n`,
    `{"reconnect":true,"next_offset":14}\n`,
  ];
  await assert.rejects(
    boat.runJob(["make"], { timeoutS: 0.14 }),
    (e) => e instanceof BoatError && e.code === "setup_failed" && /timed out/.test(e.message) && /warming cache/.test(e.message),
  );
  assert.equal(requests.at(-1), "DELETE /v1/vms/vm1");

  // 4. Hung push aborts on remaining deadline -> exitCode 124 and cleans up VM.
  requests.length = 0;
  setupDelayMs = 0;
  setupStreams = [];
  hangPush = true;
  try {
    jobRes = await boat.runJob(["make"], { push: root, timeoutS: 0.15 });
  } finally {
    hangPush = false;
  }
  assert.equal(jobRes.exitCode, 124);
  assert.equal(jobRes.deleted, true);
  assert.equal(requests.at(-1), "DELETE /v1/vms/vm1");

  // 5. Hung exec aborts on remaining client deadline -> exitCode 124 and cleans up VM.
  requests.length = 0;
  execPayloads.length = 0;
  hangExec = true;
  try {
    jobRes = await boat.runJob(["sleep", "99"], { push: root, timeoutS: 0.9 });
  } finally {
    hangExec = false;
  }
  assert.equal(jobRes.exitCode, 124);
  assert.equal(jobRes.deleted, true);
  assert.equal(execPayloads.at(-1).timeout_s, 1);
  assert.equal(requests.at(-1), "DELETE /v1/vms/vm1");
});

test("SDK parity: strict update contract, fork/start/clone, exec/run stdin+tty, and WebCrypto verifyWebhookAsync", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const vm = await boat.getVM("vm1");
  assert.deepEqual(vm, VM); // non-enumerable methods do not pollute plain-object equality

  const updated = await vm.update({
    name: "renamed",
    labels: { env: "prod" },
    idleStopS: 120,
    ttlS: 3600,
    extendS: 300,
  });
  assert.equal(updated.name, "renamed");
  assert.equal(vm.name, "renamed");
  assert.equal(log.at(-1).method, "PATCH");
  assert.equal(log.at(-1).path, "/v1/vms/vm1");
  assert.deepEqual(JSON.parse(log.at(-1).body), {
    name: "renamed",
    ttl_s: 3600,
    extend_s: 300,
    labels: { env: "prod" },
    idle_stop_s: 120,
  });

  // Snake_case options are also accepted and serialized strictly.
  const u2 = await boat.update("vm1", { name: "v2", ttl_s: 600, extend_s: 300, idle_stop_s: -1 });
  assert.equal(u2.name, "v2");
  assert.deepEqual(JSON.parse(log.at(-1).body), {
    name: "v2",
    ttl_s: 600,
    extend_s: 300,
    idle_stop_s: -1,
  });

  // resume_on_boot is host-managed: rejected locally, no request sent.
  const before = log.length;
  await assert.rejects(vm.update({ resumeOnBoot: true }), /host-managed/);
  await assert.rejects(boat.update("vm1", { name: "x", resume_on_boot: false }), /host-managed/);
  assert.equal(log.length, before);

  // Empty update or removed legacy keys (idleTimeoutS / hardTtlS / keepWarm) are rejected locally.
  await assert.rejects(vm.update({}), /update requires at least one of/);
  await assert.rejects(boat.update("vm1", { idleTimeoutS: 120, hardTtlS: 60, keepWarm: true }), /update requires at least one of/);

  // Mock server enforces exact boat-api DisallowUnknownFields parity on PATCH /v1/vms/{id}.
  for (const badField of [{ idle_timeout_s: 120 }, { hard_ttl_s: 3600 }, { keep_warm: false }]) {
    await assert.rejects(
      boat.call("PATCH", "/v1/vms/vm1", badField),
      (e) => e instanceof BoatError && e.status === 400 && e.code === "invalid_json",
    );
  }

  // create / createVM selfApi, start noEnv+ttlS, fork labels+noEnv+ttlS, and clone / cloneVM.
  const selfVm = await boat.create({ name: "self-vm", selfApi: true });
  assert.equal(selfVm.id, "vm1");
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "self-vm", self_api: true });

  const child = await vm.fork({ name: "child-1", failFast: true, env: { FOO: "bar" }, labels: { tier: "worker" }, noEnv: true, ttlS: 900 });
  assert.equal(child.id, "vm2");
  assert.deepEqual(JSON.parse(log.at(-1).body), {
    name: "child-1",
    fail_fast: true,
    env: { FOO: "bar" },
    labels: { tier: "worker" },
    no_env: true,
    ttl_s: 900,
  });

  const cloned = await vm.clone({ name: "clone-1", failFast: true, env: { C: "1" }, labels: { copy: "yes" }, noEnv: true, ttlS: 1200 });
  assert.equal(cloned.id, "vm3");
  assert.equal(cloned.name, "clone-1");
  assert.equal(log.at(-1).path, "/v1/vms/vm1/clone");
  assert.deepEqual(JSON.parse(log.at(-1).body), {
    name: "clone-1",
    fail_fast: true,
    env: { C: "1" },
    labels: { copy: "yes" },
    no_env: true,
    ttl_s: 1200,
  });
  const cloned2 = await boat.cloneVM("vm1", "clone-2", { ttlS: 300 });
  assert.equal(cloned2.name, "clone-2");
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "clone-2", ttl_s: 300 });

  await vm.start({ failFast: true, env: { BOOT: "1" }, noEnv: true, ttlS: 1800 });
  assert.deepEqual(JSON.parse(log.at(-1).body), { fail_fast: true, env: { BOOT: "1" }, no_env: true, ttl_s: 1800 });
  await vm.start({ environment: "staging" });
  assert.deepEqual(JSON.parse(log.at(-1).body), { environment: "staging" });
  await vm.fork({ environment: "base" });
  assert.deepEqual(JSON.parse(log.at(-1).body), { environment: "base" });
  await vm.clone({ environment: "staging" });
  assert.deepEqual(JSON.parse(log.at(-1).body), { environment: "staging" });
  const nLog = log.length;
  await assert.rejects(() => boat.start("vm1", { environment: "x", noEnv: true }), TypeError);
  await assert.rejects(() => boat.fork("vm1", { environment: "x", noEnv: true }), TypeError);
  await assert.rejects(() => boat.clone("vm1", { environment: "x", noEnv: true }), TypeError);
  assert.equal(log.length, nLog);
  await boat.start("vm1");
  assert.equal(log.at(-1).body, "");

  await vm.run(["cat"], { stdin: "hello stdin\n", tty: true });
  assert.deepEqual(JSON.parse(log.at(-1).body), {
    cmd: ["cat"],
    stdin_b64: b64("hello stdin\n"),
    tty: true,
  });

  await boat.run("vm1", ["cat"], { stdin: new Uint8Array([0, 255]), tty: false });
  assert.deepEqual(JSON.parse(log.at(-1).body), {
    cmd: ["cat"],
    stdin_b64: Buffer.from([0, 255]).toString("base64"),
  });

  // WebCrypto verifyWebhookAsync(secret, header, body, ...): matches verifyWebhook parameter order.
  const nowSec = Math.floor(Date.now() / 1000);
  const bodyStr = '{"type":"vm.stopped"}';
  const sign = (sec, ts = nowSec) => createHmac("sha256", sec).update(`${ts}.${bodyStr}`).digest("hex");
  const hdr = `t=${nowSec},v1=${sign("whsec_new")},v1=${sign("whsec_old")}`;
  assert.equal(await verifyWebhookAsync("whsec_new", hdr, bodyStr), true);
  assert.equal(await verifyWebhookAsync("whsec_old", hdr, new TextEncoder().encode(bodyStr), 300), true);
  assert.equal(await verifyWebhookAsync("whsec_wrong", hdr, bodyStr, 300), false);
  assert.equal(await verifyWebhookAsync("whsec_new", hdr, bodyStr + "x", 300), false);
  const oldTs = nowSec - 1000;
  assert.equal(await verifyWebhookAsync("whsec_new", `t=${oldTs},v1=${sign("whsec_new", oldTs)}`, bodyStr, 300), false);
  assert.equal(await verifyWebhookAsync("whsec_new", `t=${nowSec},v1=zzzz`, bodyStr, 300), false);
  assert.equal(await verifyWebhookAsync("whsec_new", "garbage", bodyStr, 300), false);
  assert.equal(await verifyWebhookAsync("whsec_new", null, bodyStr), false);
  assert.equal(await verifyWebhookAsync("whsec_new", undefined, bodyStr), false);
  assert.equal(verifyWebhook("whsec_new", null, bodyStr), false);
  assert.equal(verifyWebhook("whsec_new", undefined, bodyStr), false);
});

test("runJobs runs isolated VMs in parallel, preserves input order, bounds concurrency, and cleans up on error", async (t) => {
  let seq = 0;
  let inFlight = 0;
  let peakInFlight = 0;
  let failBoom = false;
  const createdIds = [];
  const deletedIds = [];

  const jobServer = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const rawBody = Buffer.concat(chunks).toString();
    const json = (code, o) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(JSON.stringify(o));
    };
    if (req.method === "POST" && req.url === "/v1/vms") {
      const parsed = JSON.parse(rawBody);
      const vid = `jobvm-${++seq}`;
      createdIds.push(vid);
      inFlight += 1;
      if (inFlight > peakInFlight) peakInFlight = inFlight;
      return json(201, { ...VM, id: vid, size: parsed.size ?? "small" });
    }
    if (req.method === "POST" && req.url.endsWith("/exec")) {
      const parsed = JSON.parse(rawBody);
      const cmd = parsed.cmd;
      if (failBoom && cmd[0] === "boom") {
        return json(500, { error: { code: "exec_boom", message: "boom" } });
      }
      const delayMs = Number(cmd[1]);
      if (parsed.timeout_s === 1 && delayMs > 500) {
        return; // hang until client AbortSignal.timeout fires
      }
      await new Promise((r) => setTimeout(r, delayMs));
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.end(`{"stream":"stdout","data_b64":"${b64(cmd[2])}"}\n{"exit":${Number(cmd[3])}}\n`);
      return;
    }
    if (req.method === "DELETE" && req.url.startsWith("/v1/vms/")) {
      const vid = req.url.split("/").pop();
      deletedIds.push(vid);
      inFlight -= 1;
      return json(200, { ok: true });
    }
    json(404, { error: { code: "not_found", message: "nope" } });
  });
  t.after(() => new Promise((resolve) => jobServer.close(resolve)));
  await new Promise((resolve) => jobServer.listen(0, "127.0.0.1", resolve));
  const jb = new Boat({ url: `http://127.0.0.1:${jobServer.address().port}`, key: TOKEN });

  // 1. Input order preserved even when job 0 finishes slower; concurrency <= 2; timeout gives 124 + cleanup.
  const results = await jb.runJobs([
    { cmd: ["work", "90", "first", "0"], size: "large" },
    ["work", "10", "second", "3"],
    { cmd: ["work", "2000", "slow", "0"], timeoutS: 0.12 },
    { cmd: ["work", "20", "fourth", "0"] },
  ], { concurrency: 2 });
  assert.deepEqual(results.map((r) => r.stdout), ["first", "second", "", "fourth"]);
  assert.deepEqual(results.map((r) => r.exitCode), [0, 3, 124, 0]);
  assert.equal(results[0].size, "large");
  assert.ok(results.every((r) => r.deleted));
  assert.ok(peakInFlight <= 2);
  assert.deepEqual([...createdIds].sort(), [...deletedIds].sort());

  // 2. Failure in one job awaits in-flight workers and deletes every started VM before rejecting.
  createdIds.length = 0;
  deletedIds.length = 0;
  failBoom = true;
  await assert.rejects(
    jb.runJobs([
      { cmd: ["work", "70", "peer", "0"] },
      { cmd: ["boom"] },
      { cmd: ["work", "40", "queued", "0"] },
    ], { concurrency: 2 }),
    (e) => e instanceof BoatError && e.code === "exec_boom",
  );
  assert.deepEqual([...createdIds].sort(), [...deletedIds].sort());

  // 3. Up-front validation rejects before creating any VM.
  createdIds.length = 0;
  await assert.rejects(jb.runJobs([["true"]], { concurrency: 0 }), RangeError);
  await assert.rejects(jb.runJobs([{ cmd: ["true"], timeoutS: 0 }]), BoatTimeoutError);
  await assert.rejects(jb.runJobs([{ cmd: ["true"], push: "/no/such/dir" }]), /not a directory|ENOENT/);
  assert.deepEqual(createdIds, []);
  assert.deepEqual(await jb.runJobs([]), []);
});

test("grantSSH, sshKeys, revokeSSH, revokePortTokens, team policies, and streamEvents", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const vm = await boat.getVM("vm1");

  const sshInfo = await vm.grantSSH("ssh-ed25519 AAAA test");
  assert.equal(sshInfo.host, "10.0.0.5");
  assert.equal(sshInfo.port, 22);
  assert.equal(log.at(-1).method, "POST");
  assert.equal(log.at(-1).path, "/v1/vms/vm1/ssh");
  assert.deepEqual(JSON.parse(log.at(-1).body), { public_key: "ssh-ed25519 AAAA test" });

  const keys = await boat.sshKeys("vm1");
  assert.equal(keys[0].fingerprint, "SHA256:abc+def/");

  const revOne = await vm.revokeSSH("SHA256:abc+def/=");
  assert.equal(revOne.ok, true);
  assert.equal(revOne.fingerprint, "SHA256:abc+def/=");
  assert.equal(log.at(-1).method, "DELETE");
  assert.equal(log.at(-1).path, "/v1/vms/vm1/ssh/SHA256%3Aabc%2Bdef%2F%3D");

  const revClient = await boat.revokeSSH("vm/1", "SHA256:xy/z+");
  assert.equal(revClient.ok, true);
  assert.equal(log.at(-1).method, "DELETE");
  assert.equal(log.at(-1).path, "/v1/vms/vm%2F1/ssh/SHA256%3Axy%2Fz%2B");

  assert.equal((await vm.revokePortTokens()).generation, 2);
  assert.equal(log.at(-1).path, "/v1/vms/vm1/port-tokens/revoke");
  assert.equal((await boat.revokePortTokens("vm/1")).generation, 2);
  assert.equal(log.at(-1).path, "/v1/vms/vm%2F1/port-tokens/revoke");

  const beforeErrLogLen = log.length;
  for (const badId of [undefined, null, "", "   ", ".", "..", { vmId: "vm1" }]) {
    await assert.rejects(boat.revokePortTokens(badId), (e) => e instanceof TypeError && /vmId is required/.test(e.message));
    await assert.rejects(boat.revokeSSH(badId, "SHA256:abc"), (e) => e instanceof TypeError && /vmId is required/.test(e.message));
  }
  for (const badFp of [undefined, null, "", "   ", ".", "..", { fingerprint: "SHA256:abc" }]) {
    await assert.rejects(boat.revokeSSH("vm1", badFp), (e) => e instanceof TypeError && /fingerprint is required/.test(e.message));
    await assert.rejects(vm.revokeSSH(badFp), (e) => e instanceof TypeError && /fingerprint is required/.test(e.message));
  }
  assert.equal(log.length, beforeErrLogLen);

  const pol = await boat.setSelfTokenPolicy("t1", { publicPorts: true, ssh: true });
  assert.deepEqual(pol, { public_ports: true, ssh: true });
  assert.equal(log.at(-1).path, "/v1/teams/t1/self-token");

  const allow = await boat.setShareAllowlist("t1", ["team_a", "team_b"]);
  assert.deepEqual(allow, { teams: ["team_a", "team_b"] });
  assert.equal(log.at(-1).path, "/v1/teams/t1/share-allowlist");

  const events = [];
  for await (const ev of boat.streamEvents({ after: 10 })) events.push(ev);
  assert.equal(log.at(-1).path, "/v1/events?after=10");
  assert.deepEqual(
    events.map((e) => [e.seq, e.type, e.vm_id]),
    [[11, "vm.started", "vm1"], [12, "vm.stopped", "vm1"]],
  );
});

test("hostPort exposes then returns the URL", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const got = await boat.hostPort("vm1", 8080, { public: true, ttlS: 600 });
  assert.match(got.url, /8080-vm1/);
  const posted = [...log].reverse().find((l) => l.method === "POST" && l.path === "/v1/vms/vm1/ports");
  assert.equal(JSON.parse(posted.body).auth, "public");
});

test("force stop, fail-fast and batch create", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.stop("vm1", { force: true });
  assert.deepEqual(JSON.parse(log.at(-1).body), { force: true });
  await boat.stop("vm1");
  assert.equal(log.at(-1).body, "");
  await boat.fork("vm1", { failFast: true });
  assert.deepEqual(JSON.parse(log.at(-1).body), { fail_fast: true });
  await boat.createVM({ failFast: true });
  assert.deepEqual(JSON.parse(log.at(-1).body), { fail_fast: true });
  const vms = await boat.createVMs(3, { name: "w", secrets: [] });
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "w", secrets: [], count: 3 });
  assert.deepEqual(vms.map((v) => v.name), ["w-1", "w-2", "w-3"]);
});

test("browser ReadableStream without Symbol.asyncIterator works in stream() and followLogs()", async () => {
  const enc = new TextEncoder();
  const makeReaderOnlyStream = (chunks) => {
    let i = 0;
    let released = false;
    const stream = {
      [Symbol.asyncIterator]: undefined,
      get released() { return released; },
      getReader() {
        return {
          async read() {
            if (i >= chunks.length) return { done: true, value: undefined };
            return { done: false, value: chunks[i++] };
          },
          releaseLock() { released = true; },
        };
      },
    };
    return stream;
  };

  const streams = [];
  const customFetch = async (reqUrl) => {
    if (reqUrl.endsWith("/exec")) {
      const s = makeReaderOnlyStream([
        enc.encode(`{"stream":"stdout","data_b64":"${b64("part1-")}"}\n{"str`),
        enc.encode(`eam":"stdout","data_b64":"${b64("part2")}"}\n{"exit":0}\n`),
      ]);
      streams.push(s);
      return { ok: true, status: 200, headers: new Headers(), body: s };
    }
    if (reqUrl.includes("/commands/c_1/logs?follow=1")) {
      const s = makeReaderOnlyStream([
        enc.encode(`{"stream":"stdout","data_b64":"${b64("log-")}"`),
        enc.encode(`,"next_offset":4}\n{"exit":7}\n`),
      ]);
      streams.push(s);
      return { ok: true, status: 200, headers: new Headers(), body: s };
    }
    throw new Error(`unexpected URL ${reqUrl}`);
  };

  const boat = new Boat({ url: "https://boat.example", key: TOKEN, fetch: customFetch });
  assert.equal(typeof makeReaderOnlyStream([])[Symbol.asyncIterator], "undefined");

  const runRes = await boat.run("vm1", ["echo", "ok"]);
  assert.deepEqual(runRes, { stdout: "part1-part2", stderr: "", exitCode: 0 });
  assert.equal(streams[0].released, true);

  const followEvents = [];
  for await (const ev of boat.followCommand("vm1", "c_1")) {
    followEvents.push({
      stream: ev.stream,
      text: new TextDecoder().decode(ev.data),
      exitCode: ev.exitCode,
      nextOffset: ev.nextOffset,
    });
  }
  assert.deepEqual(followEvents, [
    { stream: "stdout", text: "log-", exitCode: null, nextOffset: 4 },
    { stream: "", text: "", exitCode: 7, nextOffset: null },
  ]);
  assert.equal(streams[1].released, true);

  // Early break also releases the reader lock.
  for await (const _ev of boat.exec("vm1", ["echo", "early"])) {
    break;
  }
  assert.equal(streams[2].released, true);
});

test("updateEnvironment resetSecrets and teamSnapshots visibility", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const updated = await boat.updateEnvironment("base", { resetSecrets: true, setupScript: "echo ok" });
  assert.equal(updated.name, "base");
  assert.equal(log.at(-1).method, "PUT");
  assert.equal(log.at(-1).path, "/v1/environments/base");
  assert.deepEqual(JSON.parse(log.at(-1).body), { reset_secrets: true, setup_script: "echo ok" });

  const defSnaps = await boat.teamSnapshots();
  assert.equal(log.at(-1).path, "/v1/snapshots");
  assert.equal(defSnaps[0].visibility, "default");

  const sharedSnaps = await boat.teamSnapshots({ visibility: "shared" });
  assert.equal(log.at(-1).path, "/v1/snapshots?visibility=shared");
  assert.equal(sharedSnaps[0].visibility, "shared");

  const allSnaps = await boat.teamSnapshots({ visibility: "all" });
  assert.equal(log.at(-1).path, "/v1/snapshots?visibility=all");
  assert.equal(allSnaps[0].visibility, "all");
});

test("fork/clone/start throw env_failed on env_error unless allowEnvError", async () => {
  const boat = new Boat({ url, key: TOKEN });
  for (const call of [(o) => boat.fork("vmenv", o), (o) => boat.clone("vmenv", o), (o) => boat.start("vmenv", o)]) {
    await assert.rejects(call({}), (e) => e instanceof BoatError && e.code === "env_failed" && /push failed/.test(e.message) && /vmenv2/.test(e.message));
    assert.equal((await call({ allowEnvError: true })).env_error, "push failed");
  }
});

test("createVM/createVMs throw env_failed on env_error unless allowEnvError; start surfaces env_update_failed", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await assert.rejects(boat.createVM({ name: "envfail" }), (e) => e instanceof BoatError && e.code === "env_failed" && /push failed/.test(e.message) && /vm1/.test(e.message));
  assert.equal((await boat.createVM({ name: "envfail", allowEnvError: true })).env_error, "push failed");
  await assert.rejects(boat.createVMs(2, { name: "envfail" }), (e) => e.code === "env_failed" && /vm2/.test(e.message) && !/vm1:/.test(e.message));
  assert.equal((await boat.createVMs(2, { name: "envfail", allowEnvError: true })).length, 2);
  assert.equal((await boat.createVM({ name: "fine" })).id, "vm1");
  await assert.rejects(boat.start("vm502"), (e) => e instanceof BoatError && e.status === 502 && e.code === "env_update_failed");
});

test("start: legacy 502 env_update_failed with a VM body returns the VM only with allowEnvError", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await assert.rejects(boat.start("vm502b"), (e) => e instanceof BoatError && e.code === "env_update_failed" && e.vm?.id === "vm502b");
  const vm = await boat.start("vm502b", { allowEnvError: true });
  assert.equal(vm.id, "vm502b");
  assert.match(vm.env_error, /env push failed/);
  await assert.rejects(boat.start("vm502", { allowEnvError: true }), (e) => e.code === "env_update_failed");
});

test("errors expose vms (a batch's VMs) next to vm", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await assert.rejects(boat.start("vm502d"), (e) => e instanceof BoatError && e.code === "env_start_failed" && e.vm?.id === "vm502d" && e.vms?.length === 2 && e.vms[1].id === "vm502e" && e.vms[0].state === "stopped");
  await assert.rejects(boat.start("vm502c"), (e) => e.vms === undefined && e.vm?.id === "vm502c");
});

test("start: env_start_failed always throws and exposes the stopped VM, even with allowEnvError", async () => {
  const boat = new Boat({ url, key: TOKEN });
  for (const opts of [undefined, { allowEnvError: true }]) {
    await assert.rejects(boat.start("vm502c", opts), (e) => e instanceof BoatError && e.status === 502 && e.code === "env_start_failed" && e.vm?.id === "vm502c" && e.vm?.state === "stopped");
  }
});

test("upgradeEnvironment calls POST /v1/environments/{ref}/upgrade with optional targeting", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const all = await boat.upgradeEnvironment("base");
  assert.equal(log.at(-1).method, "POST");
  assert.equal(log.at(-1).path, "/v1/environments/base/upgrade");
  assert.equal(log.at(-1).body, "");
  assert.deepEqual(all, { environment: "base", version: 2, upgraded: 3, upgraded_live: 2, upgraded_stopped: 1, failed: 0 });

  const byVm = await boat.upgradeEnvironment("env/1", { vmIds: ["vm1", "vm2"] });
  assert.equal(log.at(-1).path, "/v1/environments/env%2F1/upgrade");
  assert.deepEqual(JSON.parse(log.at(-1).body), { agentIds: ["vm1", "vm2"] });
  assert.equal(byVm.upgraded, 2);

  const byAgent = await boat.upgradeEnvironment("base", { agentIds: new Set(["bx_1"]) });
  assert.deepEqual(JSON.parse(log.at(-1).body), { agentIds: ["bx_1"] });
  assert.equal(byAgent.upgraded, 1);

  const byIter = await boat.upgradeEnvironment("base", ["vm3"]);
  assert.deepEqual(JSON.parse(log.at(-1).body), { agentIds: ["vm3"] });
  assert.equal(byIter.upgraded, 1);

  const empty = await boat.upgradeEnvironment("base", { agentIds: [] });
  assert.deepEqual(JSON.parse(log.at(-1).body), { agentIds: [] });
  assert.equal(empty.upgraded, 0);

  assert.throws(() => boat.upgradeEnvironment("base", "vm1"), TypeError);
  assert.throws(() => boat.upgradeEnvironment("base", { agentIds: null }), TypeError);
  assert.throws(() => boat.upgradeEnvironment("base", { agentIds: ["vm1"], vmIds: ["vm2"] }), TypeError);
});

test("patchVM aliases updateVM and vmUsage queries GET /v1/vms/{id}/usage", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const patched = await boat.patchVM("vm1", { name: "patched", ttlS: 120 });
  assert.equal(log.at(-1).method, "PATCH");
  assert.equal(log.at(-1).path, "/v1/vms/vm1");
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "patched", ttl_s: 120 });
  assert.equal(patched.name, "patched");

  const u0 = await boat.vmUsage("vm1");
  assert.equal(log.at(-1).method, "GET");
  assert.equal(log.at(-1).path, "/v1/vms/vm1/usage");
  assert.equal(u0.seconds, 42);

  await boat.vmUsage("vm1", { since: 1700000000, until: new Date("2026-02-01T00:00:00Z") });
  assert.equal(log.at(-1).path, "/v1/vms/vm1/usage?since=1700000000&until=2026-02-01T00%3A00%3A00.000Z");
});

test("run, vm.run and runJob throw BoatError on exec stream error frames or missing exit", async () => {
  const cases = [
    [{ error: "binary not found", code: "spawn_failed" }, 400, "spawn_failed", /binary not found/],
    [{ error: "no such cwd", code: "not_found" }, 400, "not_found", /no such cwd/],
    [{ error: "unknown user", code: "invalid_user" }, 400, "invalid_user", /unknown user/],
    [{ error: "bad arg", code: "invalid_argument" }, 400, "invalid_argument", /bad arg/],
    [{ error: "denied", code: "permission_denied" }, 403, "permission_denied", /denied/],
    [{ error: "agent disconnected" }, 502, "exec_error", /agent disconnected/],
    [null, 502, "stream_ended", /without exit status/],
  ];
  let currentFrame = null;
  const deleted = [];
  const fakeFetch = async (input, init = {}) => {
    const path = new URL(input).pathname;
    if (init.method === "POST" && path === "/v1/vms") {
      return new Response(JSON.stringify({ ...VM, id: "vmerr" }), { status: 201 });
    }
    if (init.method === "DELETE") {
      deleted.push(path);
      return new Response(JSON.stringify({ ok: true }), { status: 200 });
    }
    if (init.method === "POST" && path.endsWith("/exec")) {
      const frames = [{ stream: "stdout", data_b64: b64("partial") }];
      if (currentFrame !== null) frames.push(currentFrame);
      return new Response(frames.map((f) => JSON.stringify(f)).join("\n") + "\n", {
        status: 200,
        headers: { "Content-Type": "application/x-ndjson" },
      });
    }
    return new Response(JSON.stringify({ error: { code: "not_found", message: "not found" } }), { status: 404 });
  };

  const boat = new Boat({ url: "https://boat.example", key: TOKEN, fetch: fakeFetch });
  const vm = await boat.createVM({ name: "errtest" });
  for (const [frame, status, code, msgRe] of cases) {
    currentFrame = frame;
    for (const fn of [() => boat.run("vmerr", ["nope"]), () => vm.run(["nope"])]) {
      await assert.rejects(fn, (e) => e instanceof BoatError && e.status === status && e.code === code && msgRe.test(e.message));
    }
  }
  currentFrame = { error: "spawn failed", code: "spawn_failed" };
  deleted.length = 0;
  await assert.rejects(
    boat.runJob(["missing"]),
    (e) => e instanceof BoatError && e.status === 400 && e.code === "spawn_failed",
  );
  assert.deepEqual(deleted, ["/v1/vms/vmerr"]);
});

test("runJob creates VM with ttl_s=900 and renews via PATCH before DELETE", async () => {
  const events = [];
  const srv = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : null;
    events.push({ method: req.method, path: req.url, body });
    if (req.method === "POST" && req.url === "/v1/vms") {
      res.writeHead(201, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ ...VM, id: "vmhb", ...body }));
    }
    if (req.method === "POST" && req.url === "/v1/vms/vmhb/exec") {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      await new Promise((r) => setTimeout(r, 180));
      return res.end(`{"stream":"stdout","data_b64":"${b64("ok\n")}"}\n{"exit":0}\n`);
    }
    if (req.method === "PATCH" && req.url === "/v1/vms/vmhb") {
      res.writeHead(200, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ ...VM, id: "vmhb", ...body }));
    }
    if (req.method === "DELETE" && req.url === "/v1/vms/vmhb") {
      res.writeHead(200, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ ok: true }));
    }
    res.writeHead(404);
    res.end();
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  try {
    const hbUrl = `http://127.0.0.1:${srv.address().port}`;
    const boat = new Boat({ url: hbUrl, key: TOKEN });
    boat._jobHeartbeatMs = 50;
    const res = await boat.runJob(["sleep", "1"]);
    assert.equal(res.exitCode, 0);
    assert.equal(res.deleted, true);
    const created = events.find((e) => e.method === "POST" && e.path === "/v1/vms");
    assert.equal(created.body.ttl_s, 900);
    assert.equal(created.body.idle_stop_s, 900);
    const patches = events.filter((e) => e.method === "PATCH");
    assert.ok(patches.length >= 2, `expected >= 2 PATCH renewals, got ${patches.length}`);
    for (const p of patches) {
      assert.equal(p.path, "/v1/vms/vmhb");
      assert.deepEqual(p.body, { ttl_s: 900 });
    }
    const delIdx = events.findIndex((e) => e.method === "DELETE");
    assert.ok(events.every((e, i) => e.method !== "PATCH" || i < delIdx), "no PATCH may land after DELETE");

    // Explicit ttlS: 0 disables heartbeat renewals.
    events.length = 0;
    await boat.runJob(["true"], { ttlS: 0 });
    assert.equal(events.filter((e) => e.method === "PATCH").length, 0);
  } finally {
    srv.close();
  }
});

test("createKey expiresInS, getTeam, removeMember, and BOAT_API_URL /api/v1 stripping", async () => {
  const boat = new Boat({ url, key: TOKEN });
  const k = await boat.createKey({ name: "ci", scopes: ["vms:read"], expiresInS: 3600 });
  assert.equal(log.at(-1).method, "POST");
  assert.equal(log.at(-1).path, "/v1/keys");
  assert.deepEqual(JSON.parse(log.at(-1).body), { name: "ci", scopes: ["vms:read"], expires_in_s: 3600 });
  assert.equal(k.expires, "2026-01-02T00:00:00Z");

  const team = await boat.getTeam("t1");
  assert.equal(log.at(-1).method, "GET");
  assert.equal(log.at(-1).path, "/v1/teams/t1");
  assert.equal(team.id, "t1");
  assert.equal(team.members[0].user_id, "u1");

  const removed = await boat.removeMember("t1", "u/2");
  assert.equal(log.at(-1).method, "DELETE");
  assert.equal(log.at(-1).path, "/v1/teams/t1/members/u%2F2");
  assert.equal(removed.ok, true);

  const prevUrl = process.env.BOAT_URL;
  const prevApiUrl = process.env.BOAT_API_URL;
  const prevKey = process.env.BOAT_API_KEY;
  try {
    delete process.env.BOAT_URL;
    process.env.BOAT_API_URL = `${url}/api/v1/`;
    process.env.BOAT_API_KEY = TOKEN;
    const compat = new Boat();
    assert.equal(compat.url, url);
    assert.equal((await compat.getVM("vm1")).id, "vm1");
  } finally {
    if (prevUrl === undefined) delete process.env.BOAT_URL; else process.env.BOAT_URL = prevUrl;
    if (prevApiUrl === undefined) delete process.env.BOAT_API_URL; else process.env.BOAT_API_URL = prevApiUrl;
    if (prevKey === undefined) delete process.env.BOAT_API_KEY; else process.env.BOAT_API_KEY = prevKey;
  }
  assert.equal(new Boat({ url: `${url}/api/v1`, key: TOKEN }).url, url);
  assert.equal(new Boat({ url: `${url}/v1/`, key: TOKEN }).url, url);
});

test("index.d.ts declarations cover Snapshot, Key.expires, AIUsage.totals.unmetered, snake_case aliases, and Boat methods", () => {
  const dts = readFileSync(new URL("../index.d.ts", import.meta.url), "utf8");
  assert.match(dts, /snapshots\(id:\s*string\):\s*Promise<\{\s*snapshots:\s*Snapshot\[\]\s*\}>/);
  assert.match(dts, /createSnapshot\(id:\s*string,\s*opts\?:\s*\{\s*name\?:\s*string\s*\}\):\s*Promise<Snapshot>/);
  assert.match(dts, /interface Key\s*\{[^}]*\bexpires\?:\s*string/);
  assert.match(dts, /interface AIUsage\s*\{[^}]*totals:\s*\{[^}]*\bunmetered:\s*number/);
  assert.match(dts, /getTeam\(id:\s*string\):\s*Promise<Team>/);
  assert.match(dts, /removeMember\(teamId:\s*string,\s*userId:\s*string\):\s*Promise<\{\s*ok:\s*boolean\s*\}>/);
  assert.match(dts, /createKey\([^)]*expiresInS\?:\s*number[^)]*expires_in_s\?:\s*number/);
  assert.match(dts, /interface ExecEvent\s*\{[^}]*\bcode\?:\s*string\s*\|\s*null/);

  const ifaceBody = (name) => {
    const m = dts.match(new RegExp(`export interface ${name}\\s*\\{([\\s\\S]*?)\\n\\}`));
    assert.ok(m, `missing interface ${name}`);
    return m[1];
  };
  for (const alias of ["ttl_s", "idle_stop_s", "no_env", "from", "fail_fast", "self_api", "allow_env_error"]) {
    assert.match(ifaceBody("CreateVMOptions"), new RegExp(`\\b${alias}\\?:`));
  }
  for (const alias of ["fail_fast", "no_env", "ttl_s", "allow_env_error"]) {
    assert.match(ifaceBody("StartOptions"), new RegExp(`\\b${alias}\\?:`));
    assert.match(ifaceBody("ForkOptions"), new RegExp(`\\b${alias}\\?:`));
  }
  for (const alias of ["setup_script", "reset_secrets", "is_default"]) {
    assert.match(ifaceBody("EnvironmentOptions"), new RegExp(`\\b${alias}\\?:`));
  }
  for (const alias of ["agent_ids", "vm_ids"]) {
    assert.match(ifaceBody("EnvironmentUpgradeOptions"), new RegExp(`\\b${alias}\\?:`));
  }

  // Every public method on Boat.prototype (excluding private/transport helpers) is declared in index.d.ts.
  const boatClassMatch = dts.match(/export declare class Boat\s*\{([\s\S]*?)\n\}/);
  assert.ok(boatClassMatch, "missing Boat declaration");
  const boatDecl = boatClassMatch[1];
  const skip = new Set(["constructor", "request", "call", "stream", "followLogs"]);
  for (const name of Object.getOwnPropertyNames(Boat.prototype)) {
    if (name.startsWith("_") || skip.has(name)) continue;
    assert.match(boatDecl, new RegExp(`\\b${name}\\(`), `Boat.${name} missing from index.d.ts`);
  }
});
