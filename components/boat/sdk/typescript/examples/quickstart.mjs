// create -> prompt -> fork -> stop. Needs BOAT_URL and BOAT_API_KEY.
import { Boat } from "../index.js";

const boat = new Boat();
const vm = await boat.createVM({ name: "demo", size: "default", ttlS: 4 * 3600 });
console.log("created", vm.id, vm.state);

for await (const ev of boat.prompt(vm.id, "Create hello.py that prints the date, then run it", { agent: "claude" })) {
  if (ev.data.length) (ev.stream === "stderr" ? process.stderr : process.stdout).write(ev.data);
  if (ev.exitCode !== null) console.log("\nagent exited", ev.exitCode);
}

const fork = await boat.fork(vm.id, { name: "demo-try-2" });
console.log("forked", fork.id);
await boat.stop(vm.id);
await boat.stop(fork.id);
console.log("stopped both; resume with boat.start(id)");
