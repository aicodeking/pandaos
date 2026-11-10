// boat.dev's official TypeScript SDK (@boatdev/sdk, pinned in package-lock.json)
// against this server's /api/v1: create, get, list, rename, commands (long ones
// through the SDK's runCommand / followCommand), files, find, download URLs,
// hosted ports, ssh keys, waitForDesktop, stop, resume, fork, snapshot
// listings, named snapshots, share codes, secrets, the AI proxy switch and
// delete. Run by ../e2e-live.sh with NODE_PATH at the installed node_modules
// (NODE_EXTRA_CA_CERTS for a private CA; BOAT_LIVE_DESKTOP_NAME as in
// official_sdk.py):
//
//   BOAT_URL=https://api.example BOAT_API_KEY=boat_… NODE_PATH=…/node_modules node official_sdk.cjs
//
// BOAT_LIVE_FAKE=1: the fake boatd answers every command with "hello".
"use strict";
const crypto = require("node:crypto");
const { BoatApi, Configuration, ResponseError, runCommand, followCommand, waitForDesktop } = require("@boatdev/sdk");

const REAL = !process.env.BOAT_LIVE_FAKE;
const api = new BoatApi(new Configuration({
  basePath: process.env.BOAT_URL.replace(/\/$/, "") + "/api/v1",
  accessToken: process.env.BOAT_API_KEY,
}));
console.log("official typescript SDK: " + process.env.BOAT_URL.replace(/\/$/, "") + "/api/v1");
const SSH_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q official-sdk-ts";
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

async function collect(gen) {
  const frames = [];
  for await (const f of gen) frames.push(f);
  return frames;
}

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
  // Long commands: the SDK's runCommand (detached + attach stream), then followCommand.
  const frames = await collect(runCommand(api, sandboxId, "echo ts-1; sleep 2; echo ts-2 >&2; exit 5"));
  check(frames[0].type === "started" && frames[0].processId >= 1, "runCommand started");
  const out = frames.filter((f) => f.type === "stdout" || f.type === "stderr").map((f) => f.data).join("");
  const last = frames[frames.length - 1];
  check(last.type === "exit" && (!REAL || (out === "ts-1\nts-2\n" && last.exitCode === 5)), "runCommand output and exit (" + JSON.stringify(last) + ")");
  const again = await collect(followCommand(api, sandboxId, frames[0].processId));
  check(again.map((f) => f.data || "").join("") === out && again[again.length - 1].type === "exit", "followCommand replays the output");
  const payload = "ts " + crypto.randomBytes(4).toString("hex");
  await api.writeFile({ sandboxId, fileWriteRequest: { path: "compat/ts.txt", content: payload } });
  const got = await api.readFile({ sandboxId, path: "compat/ts.txt" });
  check(!REAL || got.content === payload, "write and read a file");
  const found = await api.findInFiles({ sandboxId, pattern: payload.slice(-8), path: "compat" });
  check(!REAL || found.matches.some((m) => m.file.endsWith("compat/ts.txt")), "find in files");
  const dl = await api.createDownloadUrl({ sandboxId, fileUrlRequest: { path: "compat/ts.txt", ttlSeconds: 120 } });
  check(dl.method === "GET", "download URL");
  if (REAL) check((await (await fetch(dl.url)).text()) === payload, "the download URL serves the file with no API key");
  const hosted = await api.hostPort({ sandboxId, hostPortRequest: { port: 8080 } });
  check(hosted.access === "private" && hosted.isProtected === true, "host a port");
  const key = await api.sshKey({ sandboxId, sshKeyRequest: { key: SSH_KEY } });
  check(Boolean(key.sshUser), "authorize an ssh key");
  if (process.env.BOAT_LIVE_DESKTOP_NAME) {
    const name = process.env.BOAT_LIVE_DESKTOP_NAME;
    const desk = (await api.sandboxes({ namePrefix: name })).sandboxes.find((x) => x.name === name);
    const d = await waitForDesktop(api, desk.id, { timeoutMs: 120000 });
    check(Boolean(d.desktopUrl) && !d.provisioning, "waitForDesktop");
  } else {
    console.log("SKIP - desktop: BOAT_LIVE_DESKTOP_NAME is not set");
  }
  const nname = prefix.replace(/_/g, "-").slice(0, 60) + "-n";
  await api.saveNamedSnapshot({ namedSnapshotSaveRequest: { sandboxId, name: nname } });
  check((await api.getNamedSnapshot({ name: nname })).snapshot.sourceSandboxId === sandboxId, "named snapshot");
  const share = await api.createSnapshotShare({ name: nname });
  check(share.shareCode.startsWith("bsh_"), "share code");
  check((await api.listSnapshotShares({ name: nname })).shares.length === 1, "list share codes");
  await api.revokeSnapshotShare({ name: nname, shareId: share.share.id });
  const mine = (await api.listSandboxSnapshots({ sandboxId })).snapshots;
  check(mine.length >= 1 && (await api.getLatestSandboxSnapshot({ sandboxId })).snapshot !== null, "sandbox snapshots and latest");
  await api.deleteNamedSnapshot({ name: nname });
  const proxy = await api.getAiProxy();
  check((await api.updateAiProxy({ updateAiProxyRequest: { enabled: proxy.enabled } })).enabled === proxy.enabled, "AI proxy switch");
  const before = (await api.secrets()).envContents;
  const sec = await api.updateSecrets({ secretsUpdateRequest: { envContents: before + "OFFICIAL_TS_SECRET=x\n" } });
  check(sec.envContents.includes("OFFICIAL_TS_SECRET=\n"), "default-injected secret");
  await api.updateSecrets({ secretsUpdateRequest: { envContents: before } });
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
