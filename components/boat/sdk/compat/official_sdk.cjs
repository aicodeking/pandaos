// boat.dev's official TypeScript SDK (@boatdev/sdk, pinned in package-lock.json)
// against this server's /api/v1: create, get, list, rename, commands, files,
// stop, resume, fork and delete. Run by ../e2e-live.sh with NODE_PATH at the
// installed node_modules:
//
//   BOAT_URL=https://api.example BOAT_API_KEY=boat_… NODE_PATH=…/node_modules node official_sdk.cjs
//
// BOAT_LIVE_FAKE=1: the fake boatd answers every command with "hello".
"use strict";
const crypto = require("node:crypto");
const { BoatApi, Configuration, ResponseError } = require("@boatdev/sdk");

const REAL = !process.env.BOAT_LIVE_FAKE;
const api = new BoatApi(new Configuration({
  basePath: process.env.BOAT_URL.replace(/\/$/, "") + "/api/v1",
  accessToken: process.env.BOAT_API_KEY,
}));
const prefix = (process.env.BOAT_LIVE_PREFIX || "sdk-live") + "-off-ts-" + crypto.randomBytes(3).toString("hex");
const made = [];

function check(ok, what) {
  if (!ok) {
    console.error("official typescript SDK: FAIL: " + what);
    process.exit(1);
  }
  console.log("ok - " + what);
}

async function ready(sandboxId) {
  for (let i = 0; i < 150; i++) {
    const sb = (await api.get({ sandboxId })).sandbox;
    if (["ready", "idle", "running"].includes(sb.state)) return sb;
    await new Promise((r) => setTimeout(r, 1000));
  }
  check(false, "sandbox " + sandboxId + " became ready");
}

const del = (sandboxId) => api.deleteSandbox({ sandboxId, xAsciiConfirmDelete: sandboxId });

async function main() {
  const created = await api.create({ idempotencyKey: crypto.randomUUID(), createSandboxRequest: { name: prefix, ttlSeconds: 3600 } });
  const sandboxId = created.sandbox.id;
  made.push(sandboxId);
  check(/^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$/.test(sandboxId), "create " + sandboxId);
  const sb = await ready(sandboxId);
  check(sb.archiveAfter instanceof Date && sb.ttlSeconds === 3600, "archiveAfter and ttlSeconds");
  check((await api.sandboxes({ namePrefix: prefix })).sandboxes.map((s) => s.id).join() === sandboxId, "list by name prefix");
  try {
    const upd = (await api.update({ sandboxId, updateSandboxRequest: { name: prefix + "-r" } })).sandbox;
    check(upd.name === prefix + "-r", "rename (PATCH)");
  } catch (e) {
    if (!(e instanceof ResponseError) || e.response.status !== 501) throw e;
    console.log("SKIP - PATCH: this boatd has no PATCH /v1/vms/{id} (501)");
  }
  const res = await api.command({ sandboxId, commandRequest: { command: "echo boat-compat-ts" } });
  check(res.success === true && (!REAL || res.stdout === "boat-compat-ts\n"), "command");
  const payload = "ts " + crypto.randomBytes(4).toString("hex");
  await api.writeFile({ sandboxId, fileWriteRequest: { path: "compat/ts.txt", content: payload } });
  const got = await api.readFile({ sandboxId, path: "compat/ts.txt" });
  check(!REAL || got.content === payload, "write and read a file");
  const stopped = await api.stop({ sandboxId });
  check(stopped.sandbox.state === "archived" && stopped.sandbox.stop.status === "completed", "stop");
  await api.resume({ sandboxId });
  await ready(sandboxId);
  if (REAL) check((await api.readFile({ sandboxId, path: "compat/ts.txt" })).content === payload, "the file survived stop and resume");
  const fork = await api.fork({ sandboxId, forkRequest: { name: prefix + "-fork" } });
  made.push(fork.sandbox.id);
  check(fork.sandbox.id !== sandboxId, "fork " + fork.sandbox.id);
  await ready(fork.sandbox.id);
  check((await api.limits()).activeSandboxes >= 2, "limits");
  for (const id of [...made]) {
    const op = (await del(id)).operation;
    check(op.status === "completed" && op.targetId === id, "delete " + id);
    made.splice(made.indexOf(id), 1);
  }
  try {
    await api.get({ sandboxId });
    check(false, "a deleted sandbox is gone");
  } catch (e) {
    check(e instanceof ResponseError && e.response.status === 404, "a deleted sandbox is 404");
  }
}

main()
  .then(() => console.log("official typescript SDK: ok"))
  .catch(async (e) => {
    let body = "";
    if (e && e.response) body = await e.response.text().catch(() => "");
    console.error("official typescript SDK: FAIL:", e && e.message, body);
    process.exitCode = 1;
  })
  .finally(async () => {
    for (const id of made) await del(id).catch((e) => console.error("cleanup " + id + ": " + e));
  });
