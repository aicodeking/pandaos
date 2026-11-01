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

const boat = new Boat();
const prefix = `${process.env.BOAT_LIVE_PREFIX || "sdk-live"}-ts-${randomBytes(3).toString("hex")}`;
const made = [];
try {
  const vm = await boat.createVM({ name: prefix, secrets: [] });
  made.push(vm.id);
  check(vm.id && vm.name === prefix, `create ${vm.id}`);

  const res = await boat.run(vm.id, ["echo", "boat-live"]);
  check(res.exitCode === 0 && res.stdout.trim() === "boat-live", "exec echo");

  await boat.run(vm.id, ["bash", "-c", "nohup python3 -m http.server 8080 >/dev/null 2>&1 &"]);
  const port = await boat.addPort(vm.id, 8080);
  check(port.auth === "private", "port 8080 is private by default");
  const pu = await boat.portUrl(vm.id, 8080, { ttlS: 600 });
  check(pu.url.includes("boat_sig=") && pu.token, "private port url carries a token");
  const gw = process.env.BOAT_GATEWAY_ADDR;
  if (gw) {
    check([401, 403].includes(await gatewayGet(gw, pu.url)), "gateway refuses the route without a token");
    check((await gatewayGet(gw, pu.url, pu.token)) === 200, "gateway serves the route with the token");
  }

  const child = await boat.fork(vm.id, { name: `${prefix}-fork` });
  made.push(child.id);
  check(child.id !== vm.id, `fork ${child.id}`);

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
