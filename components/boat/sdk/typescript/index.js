// Boat SDK: fetch-based, zero dependencies. Types live in index.d.ts.
import { createHmac, timingSafeEqual } from "node:crypto";

export class BoatError extends Error {
  constructor(status, code, message, requestId = "") {
    super(`${message} (HTTP ${status}${requestId ? ", " + requestId : ""})`);
    this.name = "BoatError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

const q = encodeURIComponent;
const clean = (o) => Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined && v !== null));
const env = (k) => (typeof process !== "undefined" ? process.env[k] : undefined);

function b64decode(s) {
  if (typeof Buffer !== "undefined") return new Uint8Array(Buffer.from(s, "base64"));
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

export class Boat {
  constructor({ url = env("BOAT_URL"), key = env("BOAT_API_KEY"), fetch: f = globalThis.fetch } = {}) {
    if (!url || !key) throw new Error("Boat url and key are required (options or BOAT_URL/BOAT_API_KEY)");
    this.url = url.replace(/\/+$/, "");
    this.key = key;
    this.fetch = f;
  }

  async request(method, path, { body, raw, signal } = {}) {
    const headers = { Authorization: `Bearer ${this.key}` };
    let payload;
    if (raw !== undefined) {
      payload = raw;
      headers["Content-Type"] = "application/octet-stream";
    } else if (body !== undefined) {
      payload = JSON.stringify(body);
      headers["Content-Type"] = "application/json";
    }
    const res = await this.fetch(this.url + path, { method, headers, body: payload, signal });
    if (!res.ok) {
      const text = await res.text();
      let e = {};
      try {
        e = JSON.parse(text).error || {};
      } catch {}
      throw new BoatError(res.status, e.code || "http_error", e.message || text.slice(0, 200) || res.statusText, e.request_id || "");
    }
    return res;
  }

  async call(method, path, body) {
    const res = await this.request(method, path, { body });
    const text = await res.text();
    return text.trim() ? JSON.parse(text) : null;
  }

  async *stream(path, body, signal) {
    const res = await this.request("POST", path, { body, signal });
    const decoder = new TextDecoder();
    let buf = "";
    const emit = (line) => {
      const ev = JSON.parse(line);
      return { stream: ev.stream || "", data: ev.data_b64 ? b64decode(ev.data_b64) : new Uint8Array(), exitCode: ev.exit ?? null, error: ev.error ?? null };
    };
    for await (const chunk of res.body) {
      buf += decoder.decode(chunk, { stream: true });
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i).trim();
        buf = buf.slice(i + 1);
        if (line) yield emit(line);
      }
    }
    if (buf.trim()) yield emit(buf.trim());
  }

  me() { return this.call("GET", "/v1/me"); }
  async images() { return (await this.call("GET", "/v1/images")).images; }
  usage({ from, to } = {}) {
    const p = new URLSearchParams(clean({ from, to })).toString();
    return this.call("GET", "/v1/usage" + (p ? "?" + p : ""));
  }

  async listVMs() { return (await this.call("GET", "/v1/vms")).vms; }
  getVM(id) { return this.call("GET", `/v1/vms/${q(id)}`); }
  createVM(opts = {}) {
    const { name, size, image, ttlS, labels, env: e, secrets, idleStopS, snapshot } = opts;
    return this.call("POST", "/v1/vms", clean({ name, size, image, ttl_s: ttlS, labels, env: e, secrets, idle_stop_s: idleStopS, snapshot }));
  }
  start(id) { return this.call("POST", `/v1/vms/${q(id)}/start`); }
  stop(id) { return this.call("POST", `/v1/vms/${q(id)}/stop`); }
  fork(id, { name } = {}) { return this.call("POST", `/v1/vms/${q(id)}/fork`, clean({ name })); }
  async delete(id) { await this.call("DELETE", `/v1/vms/${q(id)}`); }

  exec(id, cmd, { env: e, cwd, timeoutS, signal } = {}) {
    return this.stream(`/v1/vms/${q(id)}/exec`, clean({ cmd, env: e, cwd, timeout_s: timeoutS }), signal);
  }
  async run(id, cmd, opts) {
    const out = [], err = [];
    let exitCode = -1;
    for await (const ev of this.exec(id, cmd, opts)) {
      (ev.stream === "stderr" ? err : out).push(ev.data);
      if (ev.exitCode !== null) exitCode = ev.exitCode;
    }
    const text = (parts) => new TextDecoder().decode(concat(parts));
    return { stdout: text(out), stderr: text(err), exitCode };
  }
  prompt(id, prompt, { agent = "claude", model, cwd, timeoutS, signal } = {}) {
    return this.stream(`/v1/vms/${q(id)}/prompt`, clean({ prompt, agent, model, cwd, timeout_s: timeoutS }), signal);
  }

  async readFile(id, path) {
    const res = await this.request("GET", `/v1/vms/${q(id)}/files?path=${q(path)}`);
    return new Uint8Array(await res.arrayBuffer());
  }
  async writeFile(id, path, data) {
    const raw = typeof data === "string" ? new TextEncoder().encode(data) : data;
    await (await this.request("PUT", `/v1/vms/${q(id)}/files?path=${q(path)}`, { raw })).text();
  }

  async ports(id) { return (await this.call("GET", `/v1/vms/${q(id)}/ports`)).ports; }
  addPort(id, port, { public: pub = false, auth } = {}) {
    return this.call("POST", `/v1/vms/${q(id)}/ports`, { port, auth: auth || (pub ? "public" : "private") });
  }
  portUrl(id, port, { ttlS } = {}) {
    return this.call("GET", `/v1/vms/${q(id)}/ports/${Number(port)}/url${ttlS ? "?ttl_s=" + Number(ttlS) : ""}`);
  }
  async removePort(id, port) { await this.call("DELETE", `/v1/vms/${q(id)}/ports/${Number(port)}`); }
  snapshots(id) { return this.call("GET", `/v1/vms/${q(id)}/snapshots`); }
  createSnapshot(id, { name } = {}) { return this.call("POST", `/v1/vms/${q(id)}/snapshots`, clean({ name })); }
  async deleteSnapshot(id, sid) { await this.call("DELETE", `/v1/vms/${q(id)}/snapshots/${q(sid)}`); }
  ssh(id, { publicKey } = {}) { return this.call("POST", `/v1/vms/${q(id)}/ssh`, clean({ public_key: publicKey })); }
  desktop(id) { return this.call("GET", `/v1/vms/${q(id)}/desktop`); }

  async teams() { return (await this.call("GET", "/v1/teams")).teams; }
  createTeam(name, { owner } = {}) { return this.call("POST", "/v1/teams", clean({ name, owner })); }
  async deleteTeam(id) { await this.call("DELETE", `/v1/teams/${q(id)}`); }
  addMember(teamId, name, { role = "member" } = {}) { return this.call("POST", `/v1/teams/${q(teamId)}/members`, { name, role }); }
  async keys() { return (await this.call("GET", "/v1/keys")).keys; }
  createKey({ name = "key", scopes, teamId } = {}) { return this.call("POST", "/v1/keys", clean({ name, scopes, team_id: teamId })); }
  async deleteKey(id) { await this.call("DELETE", `/v1/keys/${q(id)}`); }
  async secrets() { return (await this.call("GET", "/v1/secrets")).secrets; }
  async setSecret(name, value) { await this.call("PUT", `/v1/secrets/${q(name)}`, { value }); }
  async deleteSecret(name) { await this.call("DELETE", `/v1/secrets/${q(name)}`); }
  async webhooks() { return (await this.call("GET", "/v1/webhooks")).webhooks; }
  createWebhook(url, { events } = {}) { return this.call("POST", "/v1/webhooks", clean({ url, events })); }
  async deleteWebhook(id) { await this.call("DELETE", `/v1/webhooks/${q(id)}`); }
  async deliveries(id) { return (await this.call("GET", `/v1/webhooks/${q(id)}/deliveries`)).deliveries; }
  async testWebhook(id) { return (await this.call("POST", `/v1/webhooks/${q(id)}/test`)).event_id; }
}

function concat(parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

/** Verify X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, `${t}.` + body)). */
export function verifyWebhook(secret, header, body, { toleranceS = 300, now = Date.now() / 1000 } = {}) {
  const parts = Object.fromEntries(String(header).split(",").map((p) => p.split("=", 2)));
  const t = Number(parts.t);
  if (!Number.isInteger(t) || !parts.v1 || Math.abs(now - t) > toleranceS) return false;
  const want = createHmac("sha256", secret).update(`${t}.`).update(body).digest();
  const got = Buffer.from(parts.v1, "hex");
  return got.length === want.length && timingSafeEqual(got, want);
}
