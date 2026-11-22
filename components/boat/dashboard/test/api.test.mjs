import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { ApiError, createClient, createSession, errorText, errorVMIds, eventDecoder, isMissing, normalizeBase, readLogPages, readNdjson, SESSION_KEY, validKey } from "../web/src/api.js";
import { environmentSecrets, secretPicker } from "../web/src/vms.js";

function fakeFetch(handler) {
  const calls = [];
  const impl = async (url, init) => {
    calls.push({ url, init });
    return handler(url, init);
  };
  return { calls, impl };
}

const jsonRes = (status, body, headers = {}) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });

function client(handler, extra = {}) {
  const f = fakeFetch(handler);
  return { calls: f.calls, api: createClient({ getKey: () => "boat_key", fetchImpl: f.impl, ...extra }) };
}

test("sends the key only in the Authorization header", async () => {
  const { api, calls } = client(() => jsonRes(200, { vms: [{ id: "a" }] }));
  assert.deepEqual(await api.listVMs(), [{ id: "a" }]);
  assert.equal(calls[0].url, "/v1/vms");
  assert.equal(calls[0].init.headers.Authorization, "Bearer boat_key");
  assert.equal(calls[0].init.credentials, "omit");
  assert.ok(!calls[0].url.includes("boat_key"));
});

test("configured base is prefixed and path segments are encoded", async () => {
  const { api, calls } = client(() => jsonRes(200, {}), { base: "https://api.example.com/" });
  await api.getVM("a/../b?x");
  assert.equal(calls[0].url, "https://api.example.com/v1/vms/a%2F..%2Fb%3Fx");
});

test("file paths go through the query string, encoded", async () => {
  const { api, calls } = client(() => new Response("hi"));
  const blob = await api.readFile("v1", "/home/a b&c.txt");
  assert.equal(await blob.text(), "hi");
  assert.equal(calls[0].url, "/v1/vms/v1/files?path=%2Fhome%2Fa+b%26c.txt");
});

test("raw upload sends bytes without a JSON content type", async () => {
  const { api, calls } = client(() => jsonRes(200, { ok: true }));
  await api.writeFile("v1", "/tmp/x", "data");
  assert.equal(calls[0].init.method, "PUT");
  assert.equal(calls[0].init.body, "data");
  assert.equal(calls[0].init.headers["Content-Type"], undefined);
});

test("create VM sends JSON", async () => {
  const { api, calls } = client(() => jsonRes(201, { id: "v" }));
  await api.createVM({ name: "x" });
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.equal(calls[0].init.body, '{"name":"x"}');
});

test("API errors parse the error envelope", async () => {
  const { api } = client(() => jsonRes(403, { error: { code: "limit_reached", message: "too many", request_id: "r1" } }));
  await assert.rejects(api.listKeys(), (err) => {
    assert.ok(err instanceof ApiError);
    assert.equal(err.code, "limit_reached");
    assert.equal(errorText(err), "too many (request r1)");
    return true;
  });
});

test("429 reports Retry-After", async () => {
  const { api } = client(() => jsonRes(429, { error: { code: "rate_limited", message: "slow down" } }, { "Retry-After": "3" }));
  await assert.rejects(api.me(), (err) => errorText(err).includes("Retry in 3s."));
});

test("a plain-text router 404 counts as a missing endpoint", async () => {
  const { api } = client(() => new Response("404 page not found\n", { status: 404 }));
  await assert.rejects(api.listCommands("v"), (err) => isMissing(err) && err.message.startsWith("404 page"));
});

test("405 and 501 count as missing, 403 and 500 do not", () => {
  assert.ok(isMissing(new ApiError(405)));
  assert.ok(isMissing(new ApiError(501)));
  assert.ok(!isMissing(new ApiError(403)));
  assert.ok(!isMissing(new ApiError(500)));
  assert.ok(!isMissing(new Error("x")));
});

test("401 fires onUnauthorized and still rejects", async () => {
  let fired = 0;
  const { api } = client(() => jsonRes(401, { error: { code: "unauthorized", message: "no" } }), { onUnauthorized: () => { fired += 1; } });
  await assert.rejects(api.me());
  assert.equal(fired, 1);
});

test("exec streams NDJSON events", async () => {
  const lines = [{ stream: "stdout", data_b64: btoa("hi") }, { exit: 0 }];
  const { api, calls } = client(() => new Response(lines.map((l) => JSON.stringify(l)).join("\n") + "\n"));
  const seen = [];
  for await (const event of api.exec("v", { cmd: ["ls"] })) seen.push(event);
  assert.deepEqual(seen, lines);
  assert.equal(calls[0].url, "/v1/vms/v/exec");
});

test("readNdjson handles lines split across chunks and no final newline", async () => {
  const enc = new TextEncoder();
  async function* chunks() {
    yield enc.encode('{"a"');
    yield enc.encode(':1}\n\n{"b":2}');
  }
  const out = [];
  for await (const item of readNdjson(chunks())) out.push(item);
  assert.deepEqual(out, [{ a: 1 }, { b: 2 }]);
});

test("eventDecoder keeps a multi-byte character split across events", () => {
  const bytes = new TextEncoder().encode("é");
  const b64 = (b) => btoa(String.fromCharCode(b));
  const decode = eventDecoder();
  assert.equal(decode({ stream: "stdout", data_b64: b64(bytes[0]) }), "");
  assert.equal(decode({ stream: "stdout", data_b64: b64(bytes[1]) }), "é");
  assert.equal(decode({ exit: 0 }), "");
});

test("command logs follow next_offset until an empty page", async () => {
  const pages = { "": { data_b64: btoa("one "), next_offset: 4 }, 4: { data_b64: btoa("two"), next_offset: 7 }, 7: { data_b64: "", next_offset: 7 } };
  const { api, calls } = client((url) => jsonRes(200, pages[new URL(url, "http://x").searchParams.get("offset") ?? ""]));
  assert.equal(await api.commandLogs("v", "c1"), "one two");
  assert.equal(calls[0].url, "/v1/vms/v/commands/c1/logs");
  assert.equal(calls[1].url, "/v1/vms/v/commands/c1/logs?offset=4");
});

test("readLogPages stops on a next_offset that does not advance, and on the page cap", async () => {
  assert.equal(await readLogPages(async () => ({ data_b64: btoa("x"), next_offset: 0 })), "x");
  let pages = 0;
  await readLogPages(async () => {
    pages += 1;
    return { data_b64: btoa("x"), next_offset: pages };
  });
  assert.equal(pages, 50);
});

test("normalizeBase rejects anything that could hold or leak credentials", () => {
  assert.equal(normalizeBase(""), "");
  assert.equal(normalizeBase("https://api.example.com/"), "https://api.example.com");
  assert.equal(normalizeBase("https://x.example/boat/"), "https://x.example/boat");
  assert.throws(() => normalizeBase("javascript:alert(1)"));
  assert.throws(() => normalizeBase("https://u:p@x.example"));
  assert.throws(() => normalizeBase("https://x.example/?key=1"));
  assert.throws(() => normalizeBase("not a url"));
});

test("validKey rejects whitespace, controls and short values", () => {
  assert.ok(validKey(`boat_${"a".repeat(16)}_${"b".repeat(64)}`));
  assert.ok(!validKey("short"));
  assert.ok(!validKey("has space in key"));
  assert.ok(!validKey("line\nbreakkey"));
  assert.ok(!validKey(null));
});

test("session stores under one key and clears", () => {
  const data = new Map();
  const storage = { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => data.set(k, v), removeItem: (k) => data.delete(k) };
  const session = createSession(storage);
  session.set("x");
  assert.equal(data.get(SESSION_KEY), "x");
  assert.equal(session.get(), "x");
  session.clear();
  assert.equal(session.get(), null);
});

test("putSecret sends inject: true by default and honors explicit false", async () => {
  const { api, calls } = client(() => jsonRes(200, { name: "GH_TOKEN", inject: true }));
  await api.putSecret("GH_TOKEN", "s3cr3t");
  assert.equal(calls[0].url, "/v1/secrets/GH_TOKEN");
  assert.equal(calls[0].init.method, "PUT");
  assert.deepEqual(JSON.parse(calls[0].init.body), { value: "s3cr3t", inject: true });

  await api.putSecret("MANUAL_ONLY", "v2", false);
  assert.deepEqual(JSON.parse(calls[1].init.body), { value: "v2", inject: false });
});

test("secretPicker omits secrets until the checked set changes, and reset re-baselines", () => {
  const prevNode = globalThis.Node;
  const prevDoc = globalThis.document;
  class FakeNode {
    constructor(tag) {
      this.tagName = tag.toUpperCase();
      this.checked = false;
      this.attributes = {};
      this.children = [];
    }
    setAttribute(k, v) { this.attributes[k] = v; }
    addEventListener() {}
    append(...items) { this.children.push(...items); }
  }
  globalThis.Node = FakeNode;
  globalThis.document = { createElement: (tag) => new FakeNode(tag) };
  try {
    assert.equal(secretPicker([]).value(), undefined);

    const allChecked = secretPicker([
      { name: "AUTO_ONE", inject: true },
      { name: "LEGACY_UNSET" },
      "STRING_FALLBACK",
    ]);
    assert.equal(allChecked.value(), undefined);
    assert.equal(allChecked.noEnv(), false);

    const mixed = secretPicker([
      { name: "AUTO_ONE", inject: true },
      { name: "PICKED_ONLY", inject: false },
    ]);
    assert.equal(mixed.value(), undefined);

    const inputs = mixed.el.children.filter((c) => c instanceof FakeNode && c.tagName === "LABEL").map((l) => l.children[0]);
    assert.equal(inputs[0].checked, true);
    assert.equal(inputs[1].checked, false);
    inputs[0].checked = false;
    assert.deepEqual(mixed.value(), []);
    inputs[0].checked = true;
    assert.equal(mixed.value(), undefined);
    inputs[1].checked = true;
    assert.deepEqual(mixed.value(), ["AUTO_ONE", "PICKED_ONLY"]);
    mixed.reset(["PICKED_ONLY"]);
    assert.equal(inputs[0].checked, false);
    assert.equal(inputs[1].checked, true);
    assert.equal(mixed.value(), undefined);
    const fromEnv = secretPicker([{ name: "A", inject: true }, { name: "B", inject: false }], ["B"]);
    assert.equal(fromEnv.value(), undefined);
    fromEnv.noEnvInput.checked = true;
    assert.equal(fromEnv.noEnv(), true);
  } finally {
    globalThis.Node = prevNode;
    globalThis.document = prevDoc;
  }
});

test("secretPicker carry keeps No secrets and the checked set across a rebuild (tick-then-load and load-then-tick)", () => {
  const prevNode = globalThis.Node;
  const prevDoc = globalThis.document;
  class FakeNode {
    constructor(tag) { this.tagName = tag.toUpperCase(); this.checked = false; this.attributes = {}; this.children = []; }
    setAttribute(k, v) { this.attributes[k] = v; }
    addEventListener() {}
    append(...items) { this.children.push(...items); }
  }
  globalThis.Node = FakeNode;
  globalThis.document = { createElement: (tag) => new FakeNode(tag) };
  try {
    const list = [{ name: "A", inject: true }, { name: "B", inject: true }];
    // tick, then the lists load and the picker is rebuilt
    const early = secretPicker([]);
    early.noEnvInput.checked = true;
    const rebuilt = secretPicker(list, undefined, early.state());
    assert.equal(rebuilt.noEnv(), true);
    // load, then tick (and a changed secret set) before a second rebuild
    const loaded = secretPicker(list);
    assert.equal(loaded.noEnv(), false);
    loaded.noEnvInput.checked = true;
    const boxes = loaded.el.children.filter((c) => c.tagName === "LABEL").map((l) => l.children[0]);
    boxes[1].checked = false;
    const again = secretPicker(list, undefined, loaded.state());
    assert.equal(again.noEnv(), true);
    assert.deepEqual(again.value(), ["A"]);
    // without carry state nothing changes
    assert.equal(secretPicker(list).noEnv(), false);
  } finally {
    globalThis.Node = prevNode;
    globalThis.document = prevDoc;
  }
});

test("member removal confirmation copy accurately describes immediate API key and SSH grant revocation", () => {
  const src = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "web", "src", "account.js"), "utf8");
  assert.match(src, /immediately revokes their API keys and SSH access grants/i);
  assert.doesNotMatch(src, /ssh keys they registered stay in VMs/i);
});

test("readNdjson supports browser ReadableStream without Symbol.asyncIterator and releases lock", async () => {
  const enc = new TextEncoder();
  const makeReaderOnlyStream = (chunks) => {
    let i = 0;
    let released = false;
    return {
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
  };

  const s1 = makeReaderOnlyStream([
    enc.encode('{"stream":"stdout","data_b64":"aGk="}\n{"ex'),
    enc.encode('it":0}'),
  ]);
  assert.equal(typeof s1[Symbol.asyncIterator], "undefined");
  const out = [];
  for await (const item of readNdjson(s1)) {
    out.push(item);
  }
  assert.deepEqual(out, [{ stream: "stdout", data_b64: "aGk=" }, { exit: 0 }]);
  assert.equal(s1.released, true);

  // Early break also releases the reader lock.
  const s2 = makeReaderOnlyStream([
    enc.encode('{"a":1}\n{"b":2}\n'),
  ]);
  for await (const _item of readNdjson(s2)) {
    break;
  }
  assert.equal(s2.released, true);
});

test("errors carry the deduped vm/vms ids and errorText names them", async () => {
  const body = { error: { code: "env_start_failed", message: "start failed" }, vm: { id: "bx_1" }, vms: [{ id: "bx_1" }, { id: "bx_2" }] };
  const { api } = client(() => jsonRes(502, body));
  const err = await api.createVM({}).catch((e) => e);
  assert.deepEqual(err.vmIds, ["bx_1", "bx_2"]);
  assert.deepEqual(errorVMIds(err), ["bx_1", "bx_2"]);
  assert.match(errorText(err), /VM bx_1, VM bx_2 were kept stopped/);
  const one = await client(() => jsonRes(502, { error: { message: "x" }, vm: "bx_9" })).api.createVM({}).catch((e) => e);
  assert.match(errorText(one), /VM bx_9 was kept stopped/);
  const none = await client(() => jsonRes(400, { error: { message: "bad" } })).api.createVM({}).catch((e) => e);
  assert.deepEqual(none.vmIds, []);
  assert.doesNotMatch(errorText(none), /kept stopped/);
});

test("environmentSecrets baselines blank selection on the default environment", () => {
  const list = [
    { name: "a", is_default: false, secrets: ["X"] },
    { name: "d", is_default: true, secrets: ["Y"] },
    { name: "n", secrets: [] },
  ];
  assert.deepEqual(environmentSecrets(list, ""), ["Y"]);
  assert.deepEqual(environmentSecrets(list, "a"), ["X"]);
  assert.deepEqual(environmentSecrets(list, "n"), []);
  assert.equal(environmentSecrets([{ name: "d", is_default: true, secrets: null }], ""), undefined);
  assert.deepEqual(environmentSecrets([{ name: "d", is_default: true, secrets: [] }], ""), []);
  // ids win over names: two environments named "base" resolve by id.
  const dup = [{ id: "e1", name: "base", secrets: ["A"] }, { id: "e2", name: "base", secrets: ["B"] }];
  assert.deepEqual(environmentSecrets(dup, "e2"), ["B"]);
  assert.equal(environmentSecrets([{ name: "a", secrets: ["X"] }], ""), undefined);
  assert.equal(environmentSecrets([], ""), undefined);
});

test("upgradeEnvironment sends POST /v1/environments/{id}/upgrade with optional agentIds", async () => {
  const { api, calls } = client(() => jsonRes(200, { environment: "e1", version: 2, upgraded: 2, upgraded_live: 1, upgraded_stopped: 1, failed: 0 }));
  const all = await api.upgradeEnvironment("env/1");
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].url, "/v1/environments/env%2F1/upgrade");
  assert.deepEqual(JSON.parse(calls[0].init.body), {});
  assert.equal(all.upgraded, 2);

  await api.upgradeEnvironment("e1", ["vm1", "vm2"]);
  assert.equal(calls[1].url, "/v1/environments/e1/upgrade");
  assert.deepEqual(JSON.parse(calls[1].init.body), { agentIds: ["vm1", "vm2"] });
});

test("updateEnvironment sends PUT /v1/environments/{id}", async () => {
  const { api, calls } = client(() => jsonRes(200, { id: "e1", name: "base", version: 3 }));
  const updated = await api.updateEnvironment("env/1", { name: "base", env: { FOO: "bar" } });
  assert.equal(calls[0].init.method, "PUT");
  assert.equal(calls[0].url, "/v1/environments/env%2F1");
  assert.deepEqual(JSON.parse(calls[0].init.body), { name: "base", env: { FOO: "bar" } });
  assert.equal(updated.version, 3);
});

test("resizeVM and updateVM target /resize and PATCH /v1/vms/{id}", async () => {
  const { api, calls } = client(() => jsonRes(200, { id: "vm_1", size: "large" }));
  await api.resizeVM("vm/1", { size: "large", disk_gib: 60, discard_memory: true });
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].url, "/v1/vms/vm%2F1/resize");
  assert.deepEqual(JSON.parse(calls[0].init.body), { size: "large", disk_gib: 60, discard_memory: true });

  await api.updateVM("vm_1", { name: "renamed", ttl_s: 0 });
  assert.equal(calls[1].init.method, "PATCH");
  assert.equal(calls[1].url, "/v1/vms/vm_1");
  assert.deepEqual(JSON.parse(calls[1].init.body), { name: "renamed", ttl_s: 0 });
});

test("rotateKey sends optional overlap_s including explicit 0", async () => {
  const { api, calls } = client(() => jsonRes(200, { id: "k1", token: "boat_rot" }));
  await api.rotateKey("k/1");
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].url, "/v1/keys/k%2F1/rotate");
  assert.deepEqual(JSON.parse(calls[0].init.body), {});

  await api.rotateKey("k1", 0);
  assert.deepEqual(JSON.parse(calls[1].init.body), { overlap_s: 0 });

  await api.rotateKey("k1", 3600);
  assert.deepEqual(JSON.parse(calls[2].init.body), { overlap_s: 3600 });
});

test("AI credentials, proxy and usage endpoints match boat-api", async () => {
  const { api, calls } = client((url, init) => {
    if (url === "/v1/ai/credentials") return jsonRes(200, { credentials: [{ provider: "anthropic", last4: "1234" }] });
    if (url.startsWith("/v1/ai/credentials/")) return jsonRes(200, init.method === "DELETE" ? { ok: true } : { provider: "openai", last4: "9999" });
    if (url === "/v1/ai/proxy") return jsonRes(200, { enabled: false, available: true });
    if (url === "/v1/ai/usage") return jsonRes(200, { usage: [], totals: { requests: 0, input_tokens: 0, output_tokens: 0 } });
    return jsonRes(404, {});
  });
  assert.deepEqual(await api.listAICredentials(), [{ provider: "anthropic", last4: "1234" }]);
  await api.putAICredential("openai", "sk-test-9999");
  assert.equal(calls[1].init.method, "PUT");
  assert.equal(calls[1].url, "/v1/ai/credentials/openai");
  assert.deepEqual(JSON.parse(calls[1].init.body), { value: "sk-test-9999" });
  await api.deleteAICredential("openai");
  assert.equal(calls[2].init.method, "DELETE");
  assert.equal(calls[2].url, "/v1/ai/credentials/openai");
  assert.deepEqual(await api.getAIProxy(), { enabled: false, available: true });
  await api.setAIProxy(false);
  assert.equal(calls[4].init.method, "PUT");
  assert.deepEqual(JSON.parse(calls[4].init.body), { enabled: false });
  const usage = await api.aiUsage();
  assert.equal(calls[5].url, "/v1/ai/usage");
  assert.equal(usage.totals.requests, 0);
});
