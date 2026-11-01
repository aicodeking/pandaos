import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createHmac } from "node:crypto";
import { Boat, BoatError, verifyWebhook } from "../index.js";

const TOKEN = "boat_test";
const VM = { id: "vm1", name: "dev", size: "small", state: "running", team: "t1" };
const log = [];
const files = new Map();
let server, url;

const b64 = (s) => Buffer.from(s).toString("base64");

before(async () => {
  server = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const body = Buffer.concat(chunks);
    log.push({ method: req.method, path: req.url, body: body.toString() });
    const json = (code, o) => { res.writeHead(code, { "Content-Type": "application/json" }); res.end(JSON.stringify(o)); };
    if (req.headers.authorization !== `Bearer ${TOKEN}`) return json(401, { error: { code: "unauthorized", message: "bad key", request_id: "req_1" } });
    const k = `${req.method} ${req.url}`;
    if (k === "POST /v1/vms") return json(201, { ...VM, ...JSON.parse(body) });
    if (k === "GET /v1/vms") return json(200, { vms: [VM] });
    if (k === "POST /v1/vms/vm1/fork") return json(201, { ...VM, id: "vm2", name: JSON.parse(body).name });
    if (k === "POST /v1/vms/vm1/stop") return json(200, { ...VM, state: "stopped" });
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

test("secrets and delete", async () => {
  const boat = new Boat({ url, key: TOKEN });
  await boat.setSecret("OPENAI_API_KEY", "sk");
  assert.deepEqual(JSON.parse(log.at(-1).body), { value: "sk" });
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
