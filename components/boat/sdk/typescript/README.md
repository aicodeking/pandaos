# @boat/sdk (TypeScript / JavaScript)

Built on `fetch` with no dependencies, as ESM. Types are in `index.d.ts`. It needs
Node 18 or newer; it also runs in browsers and Deno, except `verifyWebhook`, which
uses `node:crypto`.

```ts
import { Boat, verifyWebhook } from "@boat/sdk";

const boat = new Boat({ url: "https://boat.example", key: process.env.BOAT_API_KEY });
const vm = await boat.createVM({ name: "demo", ttlS: 3600, secrets: ["OPENAI_API_KEY"] }); // omit = the default-injected ones, [] = none
for await (const ev of boat.prompt(vm.id, "fix the failing test", { agent: "codex" })) {
  process.stdout.write(ev.data);            // ev.exitCode is set on the last event
}
const { stdout, exitCode } = await boat.run(vm.id, ["npm", "test"]);
const fork = await boat.fork(vm.id, { name: "demo-2" });
await boat.stop(vm.id);
```

**Methods:**

- **Account:** `me`, `images`, `usage`
- **VMs:** `listVMs`, `getVM`, `createVM`, `start`, `stop`, `fork`, `delete`
- **Run commands:** `exec` (async generator), `run`, `prompt` (async generator)
- **Files:** `readFile`, `writeFile`
- **Ports:** `ports`, `addPort` (`{auth: "private"}` default, or `{public: true}`), `portUrl` (signed URL + `token` for the `X-Boat-Route-Token` header), `removePort`
- **Snapshots:** `snapshots`, `createSnapshot`, `deleteSnapshot`
- **Access:** `ssh`, `desktop`
- **Detached commands:** `startCommand`, `listCommands`, `commandStatus`, `commandHistory`, `commandLogs`, `followCommand`, `signalCommand`, `stopCommand`, `processes`
- **File ops:** `listFiles`, `deleteFile`, `moveFile`, `searchFiles`, `fileInfo`, `makeFolder`, `setFileMode`
- **Async prompts:** `startPrompt`, `listPrompts`, `promptStatus`, `followPrompt`, `steerPrompt`, `interruptPrompt`, `conversations`
- **Environments:** `environments`, `createEnvironment`, `getEnvironment`, `updateEnvironment`, `deleteEnvironment`, `buildEnvironment`; `createVM({environment})`
- **Resize:** `resize`
- **Snapshots, beyond one VM:** `teamSnapshots`, `deleteTeamSnapshot`, `downloadSnapshot`, `importSnapshot`, `shareSnapshot`, `snapshotShares`, `revokeSnapshotShare`, `sharedSnapshots`, `takeSnapshotShare`
- **AI proxy:** `aiCredentials`, `setAICredential`, `deleteAICredential`, `aiProxy`, `setAIProxy`, `aiUsage`, `setAIAccess`
- **Telemetry:** `telemetry`, `setTelemetry`, `deleteTelemetry`
- **Rotation:** `rotateKey`, `keyUsage`, `rotateWebhook`, `getWebhook`, `updateWebhook`
- **Teams:** `teams`, `createTeam(name, {owner})` and `deleteTeam` (operator only), `addMember`
- **Keys:** `keys`, `createKey`, `deleteKey`
- **One-shot jobs:** `push(vm, dir, { to })` (same files as `boat push`; Node only), `runJob(cmd, { push, cwd, timeoutS, keep, ...createVMOptions })` (`boat run`: new VM, push, run, delete; a timeout gives exit code 124), `hostPort(vm, port, { public })` (`boat host`)
- **Secrets:** `secrets`, `setSecret`, `deleteSecret`
- **Webhooks:** `webhooks`, `createWebhook`, `deleteWebhook`, `deliveries`, `testWebhook`

Errors are thrown as `BoatError` with `status`, `code` and `requestId`.

Example: `examples/quickstart.mjs`. Tests: `npm test` (`node --test`).

Live check against a real server: `BOAT_URL=… BOAT_KEY=… ../e2e-live.sh`. It runs this SDK and the other one through create, exec, private port URL, fork, stop and rm.
