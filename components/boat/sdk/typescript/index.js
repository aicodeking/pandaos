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
    const { name, size, image, ttlS, labels, env: e, secrets, idleStopS, snapshot, environment, noEnv, fromShare } = opts;
    return this.call("POST", "/v1/vms", clean({ name, size, image, ttl_s: ttlS, labels, env: e, secrets, idle_stop_s: idleStopS, snapshot, environment, no_env: noEnv, from: fromShare }));
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

  startCommand(id, cmd, { env: e, cwd } = {}) { return this.call("POST", `/v1/vms/${q(id)}/commands`, clean({ cmd, env: e, cwd })); }
  async listCommands(id) { return (await this.call("GET", `/v1/vms/${q(id)}/commands`)).commands; }
  commandStatus(id, cid) { return this.call("GET", `/v1/vms/${q(id)}/commands/${q(cid)}`); }
  async commandHistory(id) { return (await this.call("GET", `/v1/vms/${q(id)}/command-history`)).commands; }
  commandLogs(id, cid, { offset = 0, limit } = {}) {
    const p = new URLSearchParams(clean({ offset, limit })).toString();
    return this.call("GET", `/v1/vms/${q(id)}/commands/${q(cid)}/logs?${p}`);
  }
  followCommand(id, cid, { offset = 0, signal } = {}) { return this.followLogs(`/v1/vms/${q(id)}/commands/${q(cid)}/logs`, offset, signal); }
  async *followLogs(path, offset, signal) {
    const res = await this.request("GET", `${path}?follow=1&offset=${Number(offset)}`, { signal });
    const decoder = new TextDecoder();
    let buf = "";
    const emit = (line) => {
      const ev = JSON.parse(line);
      if (ev.reconnect) throw new BoatError(409, "reconnect", "the VM stopped answering; resume from nextOffset");
      return { stream: ev.stream || "", data: ev.data_b64 ? b64decode(ev.data_b64) : new Uint8Array(), exitCode: ev.exit ?? null, error: ev.signal ?? null, nextOffset: ev.next_offset ?? null };
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
  async signalCommand(id, cid, signal = "TERM") { await this.call("POST", `/v1/vms/${q(id)}/commands/${q(cid)}/signal`, { signal }); }
  async stopCommand(id, cid) { await this.call("DELETE", `/v1/vms/${q(id)}/commands/${q(cid)}`); }
  async processes(id) { return (await this.call("GET", `/v1/vms/${q(id)}/processes`)).processes; }

  async listFiles(id, path) { return (await this.call("GET", `/v1/vms/${q(id)}/files/list?path=${q(path)}`)).entries; }
  async deleteFile(id, path, { recursive = false } = {}) { await this.call("DELETE", `/v1/vms/${q(id)}/files?path=${q(path)}${recursive ? "&recursive=1" : ""}`); }
  async moveFile(id, from, to, { overwrite = false } = {}) { await this.call("POST", `/v1/vms/${q(id)}/files/move`, { from, to, overwrite }); }
  async searchFiles(id, path, query, { content, regex, limit } = {}) {
    const p = new URLSearchParams(clean({ path, q: query, content: content ? "1" : undefined, regex: regex ? "1" : undefined, limit })).toString();
    return (await this.call("GET", `/v1/vms/${q(id)}/files/search?${p}`)).matches;
  }
  fileInfo(id, path) { return this.call("GET", `/v1/vms/${q(id)}/files/info?path=${q(path)}`); }
  async makeFolder(id, path) { await this.call("POST", `/v1/vms/${q(id)}/files/folder`, { path }); }
  async setFileMode(id, path, mode) { await this.call("POST", `/v1/vms/${q(id)}/files/permissions`, { path, mode }); }

  startPrompt(id, prompt, { agent, model, cwd, conversationId, new: fresh } = {}) {
    return this.call("POST", `/v1/vms/${q(id)}/prompts`, clean({ prompt, agent, model, cwd, conversation_id: conversationId, new: fresh }));
  }
  async listPrompts(id) { return (await this.call("GET", `/v1/vms/${q(id)}/prompts`)).prompts; }
  promptStatus(id, pid) { return this.call("GET", `/v1/vms/${q(id)}/prompts/${q(pid)}`); }
  followPrompt(id, pid, { offset = 0, signal } = {}) { return this.followLogs(`/v1/vms/${q(id)}/prompts/${q(pid)}/logs`, offset, signal); }
  steerPrompt(id, pid, prompt) { return this.call("POST", `/v1/vms/${q(id)}/prompts/${q(pid)}/steer`, { prompt }); }
  interruptPrompt(id, pid) { return this.call("POST", `/v1/vms/${q(id)}/prompts/${q(pid)}/interrupt`); }
  async conversations(id) { return (await this.call("GET", `/v1/vms/${q(id)}/conversations`)).conversations; }

  async environments() { return (await this.call("GET", "/v1/environments")).environments; }
  createEnvironment(name, { image, size, setupScript, env: e, secrets, isDefault } = {}) {
    return this.call("POST", "/v1/environments", clean({ name, image, size, setup_script: setupScript, env: e, secrets, is_default: isDefault }));
  }
  getEnvironment(ref) { return this.call("GET", `/v1/environments/${q(ref)}`); }
  updateEnvironment(ref, { name, image, size, setupScript, env: e, secrets, isDefault } = {}) {
    return this.call("PUT", `/v1/environments/${q(ref)}`, clean({ name, image, size, setup_script: setupScript, env: e, secrets, is_default: isDefault }));
  }
  async deleteEnvironment(ref) { await this.call("DELETE", `/v1/environments/${q(ref)}`); }
  buildEnvironment(ref) { return this.call("POST", `/v1/environments/${q(ref)}/build`); }

  resize(id, { size, diskGib, discardMemory } = {}) { return this.call("POST", `/v1/vms/${q(id)}/resize`, clean({ size, disk_gib: diskGib, discard_memory: discardMemory })); }
  async teamSnapshots() { return (await this.call("GET", "/v1/snapshots")).snapshots; }
  async deleteTeamSnapshot(sid) { await this.call("DELETE", `/v1/snapshots/${q(sid)}`); }
  async downloadSnapshot(id, sid) {
    const res = await this.request("GET", `/v1/vms/${q(id)}/snapshots/${q(sid)}/download`);
    return new Uint8Array(await res.arrayBuffer());
  }
  async importSnapshot(data, { name, image, size } = {}) {
    const p = new URLSearchParams(clean({ name, image, size })).toString();
    const res = await this.request("POST", `/v1/snapshots/import?${p}`, { raw: data });
    return res.json();
  }
  shareSnapshot(id, sid, { teamId, ttlS } = {}) { return this.call("POST", `/v1/vms/${q(id)}/snapshots/${q(sid)}/shares`, clean({ team_id: teamId, ttl_s: ttlS })); }
  async snapshotShares(id, sid) { return (await this.call("GET", `/v1/vms/${q(id)}/snapshots/${q(sid)}/shares`)).shares; }
  async revokeSnapshotShare(id, sid, share) { await this.call("DELETE", `/v1/vms/${q(id)}/snapshots/${q(sid)}/shares/${q(share)}`); }
  async sharedSnapshots() { return (await this.call("GET", "/v1/shared-snapshots")).shares; }
  takeSnapshotShare({ shareId, code, name } = {}) { return this.call("POST", "/v1/snapshots/import-share", clean({ share_id: shareId, code, name })); }

  async aiCredentials() { return (await this.call("GET", "/v1/ai/credentials")).credentials; }
  setAICredential(provider, value) { return this.call("PUT", `/v1/ai/credentials/${q(provider)}`, { value }); }
  async deleteAICredential(provider) { await this.call("DELETE", `/v1/ai/credentials/${q(provider)}`); }
  aiProxy() { return this.call("GET", "/v1/ai/proxy"); }
  setAIProxy(enabled) { return this.call("PUT", "/v1/ai/proxy", { enabled }); }
  aiUsage(vmId) { return this.call("GET", vmId ? `/v1/vms/${q(vmId)}/ai-usage` : "/v1/ai/usage"); }
  async setAIAccess(id, enabled) { await this.call("PUT", `/v1/vms/${q(id)}/ai-access`, { enabled }); }

  telemetry() { return this.call("GET", "/v1/telemetry"); }
  setTelemetry(endpoint, { headers } = {}) { return this.call("PUT", "/v1/telemetry", clean({ endpoint, headers })); }
  async deleteTelemetry() { await this.call("DELETE", "/v1/telemetry"); }

  async teams() { return (await this.call("GET", "/v1/teams")).teams; }
  createTeam(name, { owner } = {}) { return this.call("POST", "/v1/teams", clean({ name, owner })); }
  async deleteTeam(id) { await this.call("DELETE", `/v1/teams/${q(id)}`); }
  addMember(teamId, name, { role = "member" } = {}) { return this.call("POST", `/v1/teams/${q(teamId)}/members`, { name, role }); }
  async keys() { return (await this.call("GET", "/v1/keys")).keys; }
  createKey({ name = "key", scopes, teamId } = {}) { return this.call("POST", "/v1/keys", clean({ name, scopes, team_id: teamId })); }
  rotateKey(id, { overlapS } = {}) { return this.call("POST", `/v1/keys/${q(id)}/rotate`, clean({ overlap_s: overlapS })); }
  keyUsage(id) { return this.call("GET", `/v1/keys/${q(id)}/usage`); }
  async deleteKey(id) { await this.call("DELETE", `/v1/keys/${q(id)}`); }
  async secrets() { return (await this.call("GET", "/v1/secrets")).secrets; }
  async setSecret(name, value) { await this.call("PUT", `/v1/secrets/${q(name)}`, { value }); }
  async deleteSecret(name) { await this.call("DELETE", `/v1/secrets/${q(name)}`); }
  async webhooks() { return (await this.call("GET", "/v1/webhooks")).webhooks; }
  createWebhook(url, { events } = {}) { return this.call("POST", "/v1/webhooks", clean({ url, events })); }
  getWebhook(id) { return this.call("GET", `/v1/webhooks/${q(id)}`); }
  updateWebhook(id, { url, events } = {}) { return this.call("PATCH", `/v1/webhooks/${q(id)}`, clean({ url, events })); }
  rotateWebhook(id, { overlapS } = {}) { return this.call("POST", `/v1/webhooks/${q(id)}/rotate`, clean({ overlap_s: overlapS })); }
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

/**
 * Verify X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, `${t}.` + body)).
 * During a secret rotation the header has one v1= per secret; any match passes.
 */
export function verifyWebhook(secret, header, body, { toleranceS = 300, now = Date.now() / 1000 } = {}) {
  let t = NaN;
  const sigs = [];
  for (const part of String(header).split(",")) {
    const [k, v] = part.split("=", 2);
    if (k === "t") t = Number(v);
    else if (k === "v1" && v) sigs.push(v);
  }
  if (!Number.isInteger(t) || sigs.length === 0 || Math.abs(now - t) > toleranceS) return false;
  const want = createHmac("sha256", secret).update(`${t}.`).update(body).digest();
  return sigs.some((sig) => {
    const got = Buffer.from(sig, "hex");
    return got.length === want.length && timingSafeEqual(got, want);
  });
}
