// Live SDK round trip, run by ../../e2e-live.sh: create, exec, private port url, fork, stop, rm.
import { request } from "node:http";
import { randomBytes } from "node:crypto";
import { Boat, BoatError } from "../index.js";

function check(cond, what) {
  if (!cond) {
    console.error(`ts live: FAIL: ${what}`);
    process.exit(1);
  }
  console.log(`ok - ${what}`);
}

// fetch cannot set Host, so the gateway check uses node:http.
function gatewayGet(addr, url, token) {
  const u = new URL(url);
  const [host, port] = addr.split(":");
  const headers = { Host: u.host };
  if (token) headers["X-Boat-Route-Token"] = token;
  return new Promise((resolve, reject) => {
    const req = request({ host, port: Number(port), path: u.pathname, headers, timeout: 20000 }, (res) => {
      res.resume();
      resolve(res.statusCode);
    });
    req.on("error", reject);
    req.end();
  });
}

// The route appears when boatd rewrites routes.json; poll until it answers as wanted.
async function gatewayWait(addr, url, want, token) {
  let status = 0;
  for (let i = 0; i < 20; i++) {
    status = await gatewayGet(addr, url, token);
    if (want.includes(status)) break;
    await new Promise((r) => setTimeout(r, 1000));
  }
  return status;
}

// Behaviour only a real guest can show (the fake boatd sets BOAT_LIVE_FAKE=1).
const REAL = !process.env.BOAT_LIVE_FAKE;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const STUB_AGENT = `#!/bin/bash
case "$*" in *sleep-forever*) exec sleep 300 ;; esac
echo '{"type":"result","text":"stub"}'
`;

const boat = new Boat();
const prefix = `${process.env.BOAT_LIVE_PREFIX || "sdk-live"}-ts-${randomBytes(3).toString("hex")}`;
const made = [];
try {
  const vm = await boat.createVM({ name: prefix, secrets: [] });
  made.push(vm.id);
  check(vm.id && vm.name === prefix, `create ${vm.id}`);

  const res = await boat.run(vm.id, ["echo", "boat-live"]);
  check(res.exitCode === 0 && res.stdout.trim() === "boat-live", "exec echo");

  await boat.run(vm.id, ["bash", "-c", "setsid nohup python3 -m http.server 8080 >/dev/null 2>&1 </dev/null & sleep 1"]);
  const port = await boat.addPort(vm.id, 8080);
  check(port.auth === "private", "port 8080 is private by default");
  const pu = await boat.portUrl(vm.id, 8080, { ttlS: 600 });
  check(pu.url.includes("boat_sig=") && pu.token, "private port url carries a token");
  const gw = process.env.BOAT_GATEWAY_ADDR;
  if (gw) {
    const refused = await gatewayWait(gw, pu.url, [401, 403]);
    check([401, 403].includes(refused), `gateway refuses the route without a token (${refused})`);
    const served = await gatewayWait(gw, pu.url, [200], pu.token);
    check(served === 200, `gateway serves the route with the token (${served})`);
  }

  const child = await boat.fork(vm.id, { name: `${prefix}-fork` });
  made.push(child.id);
  check(child.id !== vm.id, `fork ${child.id}`);

  const follow = async (gen) => {
    let code = null;
    let out = "";
    for await (const ev of gen) {
      out += new TextDecoder().decode(ev.data);
      if (ev.exitCode !== null) code = ev.exitCode;
    }
    return { code, out };
  };

  // detached commands
  const started = await boat.startCommand(vm.id, ["bash", "-c", "echo detached-out; exit 7"]);
  const fc = await follow(boat.followCommand(vm.id, started.id));
  check(!REAL || (fc.code === 7 && fc.out.includes("detached-out")), `follow returns output and exit (${fc.code})`);
  check((await boat.listCommands(vm.id)).some((c) => c.id === started.id), "commands list");
  check((await boat.commandHistory(vm.id)).some((c) => c.id === started.id), "command history");
  const long = await boat.startCommand(vm.id, ["sleep", "300"]);
  await boat.signalCommand(vm.id, long.id, "TERM");
  let st;
  for (let i = 0; i < 30; i++) {
    st = await boat.commandStatus(vm.id, long.id);
    if (st.state !== "running") break;
    await sleep(500);
  }
  check(st.state === "signaled", `signal stops a command (${st.state})`);

  // files
  await boat.makeFolder(vm.id, "/home/dev/live");
  await boat.writeFile(vm.id, "/home/dev/live/a.txt", "needle");
  check((await boat.listFiles(vm.id, "/home/dev/live")).some((e) => e.name === "a.txt"), "file list");
  await boat.moveFile(vm.id, "/home/dev/live/a.txt", "/home/dev/live/b.txt");
  check(new TextDecoder().decode(await boat.readFile(vm.id, "/home/dev/live/b.txt")) === "needle", "file move");
  check((await boat.searchFiles(vm.id, "/home/dev/live", "b.txt")).some((m) => m.path.endsWith("b.txt")), "file search");
  await boat.setFileMode(vm.id, "/home/dev/live/b.txt", "600");
  await boat.deleteFile(vm.id, "/home/dev/live/b.txt");
  check(!(await boat.listFiles(vm.id, "/home/dev/live")).some((e) => e.name === "b.txt"), "file delete");

  // async prompts, with a stub agent on dev's PATH
  await boat.writeFile(vm.id, "/home/dev/.local/bin/claude", STUB_AGENT);
  await boat.setFileMode(vm.id, "/home/dev/.local/bin/claude", "755");
  const slow = await boat.startPrompt(vm.id, "sleep-forever on it", { new: true });
  check(slow.state === "running", "async prompt returns a turn");
  const next = await boat.steerPrompt(vm.id, slow.id, "change of plan");
  check(next.conversation_id === slow.conversation_id && next.id !== slow.id, "steer resumes the conversation");
  check((await boat.promptStatus(vm.id, slow.id)).state === "steered", "the steered turn is marked");
  const fp = await follow(boat.followPrompt(vm.id, next.id));
  check(!REAL || fp.code === 0, `the new turn finishes (${fp.code})`);
  const victim = await boat.startPrompt(vm.id, "sleep-forever again", { new: true });
  check((await boat.interruptPrompt(vm.id, victim.id)).state === "interrupted", "interrupt");
  check((await boat.conversations(vm.id)).length === 2, "conversations");

  // snapshot share as a copy (the Python run covers download + import)
  const snap = await boat.createSnapshot(vm.id, { name: `${prefix}-s` });
  const share = await boat.shareSnapshot(vm.id, snap.id, { ttlS: 600 });
  check(share.code.startsWith("bsh_"), "share code");
  const taken = await boat.takeSnapshotShare({ code: share.code, name: `${prefix}-copy` });
  check(taken.id !== snap.id, "share taken as a copy");
  await boat.revokeSnapshotShare(vm.id, snap.id, share.id);
  await boat.deleteTeamSnapshot(taken.id);

  // environments: setup on create
  const envName = `${prefix}-env`;
  await boat.createEnvironment(envName, { setupScript: "echo built > /home/dev/env-ok", secrets: [] });
  try {
    const evm = await boat.createVM({ name: `${prefix}-e1`, environment: envName });
    made.push(evm.id);
    check(evm.setup_cmd_id, "new VM runs the setup");
    const fs = await follow(boat.followCommand(evm.id, evm.setup_cmd_id));
    check(!REAL || fs.code === 0, `setup succeeded (${fs.code})`);
    check((await boat.run(evm.id, ["cat", "/home/dev/env-ok"])).stdout.trim() === "built", "setup's result is on the VM");
    await boat.delete(evm.id);
    made.pop();
  } finally {
    await boat.deleteEnvironment(envName);
  }

  // rotation
  const key = await boat.createKey({ name: `${prefix}-k`, scopes: ["vms:read"] });
  const rotated = await boat.rotateKey(key.id, { overlapS: 600 });
  check((await new Boat({ url: boat.url, key: key.token }).me()).key_id === key.id, "old key works during the overlap");
  check((await new Boat({ url: boat.url, key: rotated.token }).me()).key_id === key.id, "new key works");
  await boat.deleteKey(key.id);
  const hook = await boat.createWebhook("https://example.invalid/h", { events: ["live.none"] });
  check((await boat.rotateWebhook(hook.id)).secret !== hook.secret, "webhook rotation");
  await boat.deleteWebhook(hook.id);
  check("totals" in (await boat.aiUsage()), "AI usage");

  check((await boat.stop(vm.id)).state === "stopped", "stop");
} finally {
  for (const id of made.reverse()) {
    try {
      await boat.delete(id);
      console.log(`deleted ${id}`);
    } catch (e) {
      console.error(`cleanup ${id}: ${e.message}`);
    }
  }
}
for (const id of made) {
  try {
    await boat.getVM(id);
    check(false, `${id} still exists after rm`);
  } catch (e) {
    if (!(e instanceof BoatError) || e.status !== 404) throw e;
  }
}
console.log("ok - rm");
