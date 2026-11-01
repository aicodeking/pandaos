import assert from "node:assert/strict";
import { test } from "node:test";
import { ApiError, createClient, createSession, errorText, eventDecoder, isMissing, normalizeBase, readLogPages, readNdjson, SESSION_KEY, validKey } from "../web/src/api.js";

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
