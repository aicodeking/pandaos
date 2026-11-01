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

test("verifyWebhook", () => {
  const body = '{"type":"vm.stopped"}';
  const sig = createHmac("sha256", "whsec_x").update("1000." + body).digest("hex");
  const h = `t=1000,v1=${sig}`;
  assert.equal(verifyWebhook("whsec_x", h, body, { now: 1001 }), true);
  assert.equal(verifyWebhook("whsec_x", h, body + " ", { now: 1001 }), false);
  assert.equal(verifyWebhook("whsec_x", h, body, { now: 9999 }), false);
  assert.equal(verifyWebhook("whsec_x", "junk", body), false);
});
