import assert from "node:assert/strict";
import { test } from "node:test";
import { aiUsageTotals, createVMSpec, fmtBytes, fmtCmd, fmtDuration, fmtIP, fmtSize, fmtTTL, parseKV, parseList, parseOptionalInt, resizeVMSpec, safeHttpUrl, shellQuote, shouldDeferPoll, sinceISO, sshCommand, topRows, updateVMSpec, usageView, vmActions } from "../web/src/format.js";

test("fmtBytes", () => {
  assert.equal(fmtBytes(0), "0 B");
  assert.equal(fmtBytes(1536), "1.5 KiB");
  assert.equal(fmtBytes(5 * 1024 ** 3), "5.0 GiB");
  assert.equal(fmtBytes(undefined), "—");
});

test("fmtDuration", () => {
  assert.equal(fmtDuration(42), "42s");
  assert.equal(fmtDuration(120), "2m");
  assert.equal(fmtDuration(3660), "1h 1m");
  assert.equal(fmtDuration(90000), "1d 1h");
  assert.equal(fmtDuration(-1), "—");
});

test("fmtTTL counts down, reports expiry, falls back to the configured TTL", () => {
  const now = Date.parse("2026-01-01T00:00:00Z");
  assert.equal(fmtTTL({ expires_at: "2026-01-01T01:00:00Z" }, now), "1h 0m");
  assert.equal(fmtTTL({ expires_at: "2025-12-31T00:00:00Z" }, now), "expired");
  assert.equal(fmtTTL({ ttl_s: 600 }, now), "10m");
  assert.equal(fmtTTL({}, now), "none");
  assert.equal(fmtTTL({ expires_at: "garbage" }, now), "—");
});

test("fmtSize and fmtIP", () => {
  assert.equal(fmtSize({ size: "small", vcpu: 2, mem_mib: 4096 }), "small (2 vCPU, 4 GiB)");
  assert.equal(fmtSize({ size: "small" }), "small");
  assert.equal(fmtIP({ ip: "10.0.0.1", public_ip: "1.2.3.4" }), "1.2.3.4");
  assert.equal(fmtIP({}), "—");
});

test("fmtCmd", () => {
  assert.equal(fmtCmd(["a", "b"]), "a b");
  assert.equal(fmtCmd("a b"), "a b");
  assert.equal(fmtCmd(undefined), "—");
});

test("vmActions follows the state machine", () => {
  assert.deepEqual(vmActions("running"), { start: false, stop: true, fork: true, resize: true, remove: true });
  assert.deepEqual(vmActions("stopped"), { start: true, stop: false, fork: true, resize: true, remove: true });
  assert.deepEqual(vmActions("creating"), { start: false, stop: false, fork: false, resize: false, remove: false });
  assert.deepEqual(vmActions("error"), { start: true, stop: false, fork: false, resize: false, remove: true });
});

test("sshCommand covers the direct, custom port and jump cases", () => {
  assert.equal(sshCommand({ host: "h.example", port: 22, user: "dev" }), "ssh dev@h.example");
  assert.equal(sshCommand({ host: "h", port: 2201, user: "dev" }), "ssh -p 2201 dev@h");
  assert.equal(sshCommand({ host: "10.0.0.2", port: 22, user: "dev", jump: "jump@h:2222" }), "ssh -J jump@h:2222 dev@10.0.0.2");
});

test("sshCommand quotes hostile values so a pasted command cannot inject", () => {
  assert.equal(sshCommand({ host: "h; rm -rf ~", user: "dev" }), "ssh 'dev@h; rm -rf ~'");
  assert.equal(shellQuote("a'b"), `'a'\\''b'`);
  assert.equal(shellQuote("$(reboot)"), "'$(reboot)'");
});

test("safeHttpUrl refuses script and data URLs", () => {
  assert.equal(safeHttpUrl("https://x.example/a?b=1"), "https://x.example/a?b=1");
  assert.equal(safeHttpUrl("javascript:alert(1)"), null);
  assert.equal(safeHttpUrl("data:text/html,x"), null);
  assert.equal(safeHttpUrl("/relative"), null);
  assert.equal(safeHttpUrl(undefined), null);
});

test("parseKV keeps = in values, names the bad line", () => {
  assert.deepEqual(parseKV("A=1\n\n B=x=y \n"), { A: "1", B: "x=y" });
  assert.deepEqual(parseKV(""), {});
  assert.throws(() => parseKV("A=1\nnope"), /Line 2/);
  assert.throws(() => parseKV("=x"), /Line 1/);
});

test("parseList splits on commas and spaces", () => {
  assert.deepEqual(parseList("vm.*, vm.stopped  x"), ["vm.*", "vm.stopped", "x"]);
  assert.deepEqual(parseList(""), []);
});

test("parseOptionalInt: blank is unset, garbage throws", () => {
  assert.equal(parseOptionalInt("", "TTL"), undefined);
  assert.equal(parseOptionalInt(" 60 ", "TTL"), 60);
  assert.throws(() => parseOptionalInt("1.5", "TTL"), /TTL must/);
  assert.throws(() => parseOptionalInt("-1", "TTL"), /TTL must/);
});

test("createVMSpec keeps explicit TTL 0, allows idle_stop_s -1, and omits blank fields", () => {
  assert.equal(createVMSpec({ ttl_s: "0" }).ttl_s, 0);
  assert.equal("ttl_s" in createVMSpec({ ttl_s: " " }), false);
  assert.equal(createVMSpec({ idle_stop_s: "-1" }).idle_stop_s, -1);
  assert.equal(createVMSpec({ idle_stop_s: "0" }).idle_stop_s, 0);
  assert.throws(() => createVMSpec({ idle_stop_s: "-2" }), /Idle stop/);
});

test("createVMSpec drops blanks, so server defaults apply", () => {
  assert.deepEqual(createVMSpec({ name: " ", size: "", image: "", snapshot: "", ttl_s: "", idle_stop_s: "", labels: "", env: "" }), {});
});

test("createVMSpec distinguishes all secrets (absent) from none ([])", () => {
  const spec = createVMSpec({ name: "a", size: "small", image: "u", ttl_s: "60", labels: "x=y", env: "E=1", secrets: [] });
  assert.deepEqual(spec, { name: "a", size: "small", image: "u", ttl_s: 60, labels: { x: "y" }, env: { E: "1" }, secrets: [] });
  assert.ok(!("secrets" in createVMSpec({ name: "a" })));
});

test("sinceISO", () => {
  assert.equal(sinceISO(1, Date.parse("2026-01-02T00:00:00Z")), "2026-01-01T00:00:00.000Z");
  // Anchored to UTC midnight: any time within a day yields the same window.
  const a = sinceISO(7, Date.parse("2026-01-10T01:02:03Z")), b = sinceISO(7, Date.parse("2026-01-10T23:59:59Z"));
  assert.equal(a, "2026-01-03T00:00:00.000Z");
  assert.equal(a, b);
});

test("topRows sorts and folds the tail into Other", () => {
  const rows = [1, 5, 3, 4, 2].map((value) => ({ label: `r${value}`, value }));
  assert.deepEqual(topRows(rows, 3).map((r) => [r.label, r.value]), [["r5", 5], ["r4", 4], ["Other", 6]]);
  assert.equal(topRows(rows, 5).length, 5);
});

test("usageView reads boatd's report and tolerates an empty one", () => {
  const view = usageView({
    teams: [{ vm_seconds: 7200, snapshot_days: 2.5, vm_seconds_by_size: { small: 3600, large: 3600 } }],
    vms: [{ id: "vm1", vm_seconds: 7200 }],
    snapshots: [{ id: "s1", snapshot_days: 2.5 }],
    storage: [{ committed_bytes: 1024 }],
  }, { vm1: "web" });
  assert.equal(view.vmHours, 2);
  assert.equal(view.snapshotDays, 2.5);
  assert.equal(view.storageBytes, 1024);
  assert.deepEqual(view.byVM, [{ label: "web", value: 2 }]);
  assert.equal(view.bySize.length, 2);
  const empty = usageView({});
  assert.equal(empty.vmHours, 0);
  assert.deepEqual(empty.bySize, []);
});

test("shouldDeferPoll waits for a dialog or fresh focus, but not forever", () => {
  const base = { focusInside: true, lastFocusAt: 1000, now: 2000 };
  assert.equal(shouldDeferPoll(base), true);
  assert.equal(shouldDeferPoll({ ...base, now: 1000 + 30000 }), false);
  assert.equal(shouldDeferPoll({ ...base, focusInside: false }), false);
  assert.equal(shouldDeferPoll({ focusInside: false, dialogOpen: true }), true);
  assert.equal(shouldDeferPoll({ focusInside: false }), false);
});

test("vmActions enables Start on a running VM only when env_error is set (Retry environment)", () => {
  assert.equal(vmActions("running").start, false);
  assert.equal(vmActions("running", "  ").start, false);
  assert.equal(vmActions("running", "push failed").start, true);
  assert.equal(vmActions("stopping", "push failed").start, false);
});

test("createVMSpec: no_env drops secrets and environment; unchanged secrets stay omitted", () => {
  assert.deepEqual(createVMSpec({ environment: "base", secrets: undefined }), { environment: "base" });
  assert.deepEqual(createVMSpec({ environment: "env_123" }), { environment: "env_123" });
  assert.deepEqual(createVMSpec({ environment: "" }), {});
  assert.deepEqual(createVMSpec({ environment: "base", secrets: ["A"], no_env: true }), { secrets: [], no_env: true });
  assert.deepEqual(createVMSpec({ no_env: true }), { secrets: [], no_env: true });
  assert.deepEqual(createVMSpec({ secrets: [] }), { secrets: [] });
});

test("resizeVMSpec validates size/disk_gib and omits unset fields", () => {
  assert.deepEqual(resizeVMSpec({ size: "large", disk_gib: "", discard_memory: false }), { size: "large" });
  assert.deepEqual(resizeVMSpec({ size: "xlarge", disk_gib: "80", discard_memory: true }), { size: "xlarge", disk_gib: 80, discard_memory: true });
  assert.deepEqual(resizeVMSpec({ size: "  ", disk_gib: "40" }), { disk_gib: 40 });
  assert.throws(() => resizeVMSpec({ size: "", disk_gib: "" }), /Give a size/);
  assert.throws(() => resizeVMSpec({ disk_gib: "0" }), /Disk \(GiB\)/);
});

test("updateVMSpec validates name, TTL and idle stop", () => {
  assert.deepEqual(updateVMSpec({ name: "web", ttl_s: "0", idle_stop_s: "-1" }), { name: "web", ttl_s: 0, idle_stop_s: -1 });
  assert.deepEqual(updateVMSpec({ name: "renamed", ttl_s: "", idle_stop_s: "" }), { name: "renamed" });
  assert.throws(() => updateVMSpec({ name: " ", ttl_s: "", idle_stop_s: "" }), /Set a name/);
  assert.throws(() => updateVMSpec({ idle_stop_s: "-2" }), /Idle stop/);
});

test("aiUsageTotals uses server totals or sums rows safely", () => {
  const fromTotals = aiUsageTotals({
    usage: [{ vm_id: "v1", requests: 2, input_tokens: 10, output_tokens: 5 }],
    totals: { requests: 9, input_tokens: 100, output_tokens: 50 },
  });
  assert.deepEqual(fromTotals.totals, { requests: 9, input_tokens: 100, output_tokens: 50 });
  const summed = aiUsageTotals({
    usage: [
      { vm_id: "v1", requests: 2, input_tokens: 10, output_tokens: 5 },
      { vm_id: "v2", requests: 3, input_tokens: 20, output_tokens: 15 },
    ],
  });
  assert.deepEqual(summed.totals, { requests: 5, input_tokens: 30, output_tokens: 20 });
  assert.deepEqual(aiUsageTotals({}).totals, { requests: 0, input_tokens: 0, output_tokens: 0 });
});
