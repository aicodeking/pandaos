// Boat SDK: fetch-based, zero dependencies. Types live in index.d.ts.
const nodeCryptoSpecifier = "node:crypto";
let nodeCrypto = null;
if (typeof process !== "undefined" && Boolean(process.versions?.node)) {
  try {
    nodeCrypto = await import(nodeCryptoSpecifier);
  } catch {}
}

const PATCH_VM_FIELDS = ["name", "ttl_s", "extend_s", "labels", "idle_stop_s"];

function createBody(opts = {}) {
  const {
    name,
    size,
    image,
    ttlS,
    ttl_s,
    labels,
    env: e,
    secrets,
    idleStopS,
    idle_stop_s,
    snapshot,
    environment,
    noEnv,
    no_env,
    fromShare,
    from: fromOpt,
    failFast,
    fail_fast,
    selfApi,
    self_api,
  } = opts;
  const ff = failFast ?? fail_fast;
  return clean({
    name,
    size,
    image,
    ttl_s: ttlS ?? ttl_s,
    labels,
    env: e,
    secrets,
    idle_stop_s: idleStopS ?? idle_stop_s,
    snapshot,
    environment,
    no_env: noEnv ?? no_env,
    from: fromShare ?? fromOpt,
    fail_fast: ff || undefined,
    self_api: selfApi ?? self_api,
  });
}

function updateBody(patch = {}) {
  if (!patch || typeof patch !== "object") {
    throw new Error("update requires an options object");
  }
  if ("resumeOnBoot" in patch || "resume_on_boot" in patch) {
    throw new Error("resume_on_boot is host-managed and cannot be set through update");
  }
  const {
    name,
    ttlS,
    ttl_s,
    extendS,
    extend_s,
    labels,
    idleStopS,
    idle_stop_s,
  } = patch;
  const body = clean({
    name,
    ttl_s: ttlS ?? ttl_s,
    extend_s: extendS ?? extend_s,
    labels,
    idle_stop_s: idleStopS ?? idle_stop_s,
  });
  if (Object.keys(body).length === 0) {
    throw new Error(`update requires at least one of: ${PATCH_VM_FIELDS.join(", ")}`);
  }
  return body;
}

function idemHeaders(key) {
  if (key === undefined || key === null) return undefined;
  if (typeof key !== "string" || !key.trim() || key.length > 255) {
    throw new TypeError("idempotencyKey must be a non-empty string of at most 255 characters");
  }
  return { "Idempotency-Key": key };
}

function forkOrCloneBody(nameOrOpts = {}, maybeOpts = {}) {
  const opts = typeof nameOrOpts === "string" ? { ...maybeOpts, name: nameOrOpts } : (nameOrOpts ?? {});
  const { name, failFast, fail_fast, env: e, labels, noEnv, no_env, environment, ttlS, ttl_s } = opts;
  if (environment && (noEnv ?? no_env)) throw new TypeError("environment and noEnv are mutually exclusive");
  const ff = failFast ?? fail_fast;
  return clean({
    name,
    fail_fast: ff || undefined,
    env: e,
    labels,
    no_env: noEnv ?? no_env,
    environment,
    ttl_s: ttlS ?? ttl_s,
  });
}

export class BoatError extends Error {
  constructor(status, code, message, requestId = "", vm = undefined, vms = undefined) {
    super(`${message} (HTTP ${status}${requestId ? ", " + requestId : ""})`);
    if (vm) this.vm = vm; // the VM body some error answers carry (legacy env_update_failed, env_start_failed)
    if (vms) this.vms = vms; // a batch create's VMs (env_start_failed: the failed ones stopped)
    this.name = "BoatError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

function assertEnvDelivered(vms) {
  const bad = vms.filter((v) => String(v?.env_error ?? "").trim());
  if (!bad.length) return;
  const detail = bad.map((v) => `${v.id ?? "?"}: ${String(v.env_error).trim()}`).join("; ");
  throw new BoatError(502, "env_failed", `environment delivery failed for ${bad.length} VM(s) that were created and are still up (${detail}); delete them or pass allowEnvError: true`);
}

export class BoatTimeoutError extends BoatError {
  constructor(message = "timed out") {
    super(0, "timeout", message);
    this.name = "BoatTimeoutError";
  }
}

// The archive lands as root in sticky /tmp; a path outside home may need root.
const UNPACK = `d=$0; case "$d" in /*) ;; *) d=$HOME/$d ;; esac
mkdir -p -- "$d" 2>/dev/null || sudo -n install -d -o "$(id -u)" -g "$(id -g)" -- "$d" || exit 1
tar xzf "$1" -C "$d"; rc=$?
rm -f -- "$1" 2>/dev/null || sudo -n rm -f -- "$1"
[ "$rc" = 0 ] && printf '%s\\n' "$d"; exit "$rc"`;
// runJob has no limit by default; the agent's default exec timeout is 600 s.
const NO_LIMIT_S = 30 * 24 * 60 * 60;
const JOB_IDLE_STOP_S = 15 * 60;
const JOB_TTL_S = 15 * 60;
const JOB_HEARTBEAT_MS = 5 * 60 * 1000;
const EXEC_STREAM_STATUS = {
  not_found: 400,
  invalid_user: 400,
  spawn_failed: 400,
  invalid_argument: 400,
  bad_request: 400,
  invalid_request: 400,
  permission_denied: 403,
  forbidden: 403,
  conflict: 409,
  unsupported: 422,
  not_supported: 422,
};

function apiOrigin(u) {
  let s = String(u ?? "").trim().replace(/\/+$/, "");
  for (const suf of ["/api/v1", "/v1"]) {
    if (s.endsWith(suf)) {
      return s.slice(0, -suf.length).replace(/\/+$/, "");
    }
  }
  return s;
}

function streamError(ev) {
  const raw = ev.error;
  let code = typeof ev.code === "string" ? ev.code : "";
  let message;
  if (raw && typeof raw === "object") {
    if (!code && typeof raw.code === "string") code = raw.code;
    message = String(raw.message ?? raw.error ?? JSON.stringify(raw));
  } else {
    message = String(raw);
  }
  code = code.trim() || "exec_error";
  const status = EXEC_STREAM_STATUS[code] ?? 502;
  return new BoatError(status, code, message || code);
}
const B64_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

const q = encodeURIComponent;
const clean = (o) => Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined && v !== null));
const env = (k) => (typeof process !== "undefined" ? process.env?.[k] : undefined);
const sleepMs = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function isAbortTimeout(err, signal) {
  if (signal?.aborted) return true;
  const name = err?.name;
  return name === "TimeoutError" || name === "AbortError";
}

function randomHex(bytes) {
  if (nodeCrypto) return nodeCrypto.randomBytes(bytes).toString("hex");
  return Array.from(globalThis.crypto.getRandomValues(new Uint8Array(bytes)), (b) => b.toString(16).padStart(2, "0")).join("");
}

function b64encode(bytes) {
  const u8 = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  if (typeof Buffer !== "undefined") return Buffer.from(u8).toString("base64");
  if (typeof btoa === "function") {
    let bin = "";
    for (let i = 0; i < u8.length; i += 0x8000) {
      bin += String.fromCharCode(...u8.subarray(i, i + 0x8000));
    }
    return btoa(bin);
  }
  let out = "";
  for (let i = 0; i < u8.length; i += 3) {
    const a = u8[i];
    const b = i + 1 < u8.length ? u8[i + 1] : 0;
    const c = i + 2 < u8.length ? u8[i + 2] : 0;
    const n = (a << 16) | (b << 8) | c;
    out += B64_ALPHABET[(n >> 18) & 63];
    out += B64_ALPHABET[(n >> 12) & 63];
    out += i + 1 < u8.length ? B64_ALPHABET[(n >> 6) & 63] : "=";
    out += i + 2 < u8.length ? B64_ALPHABET[n & 63] : "=";
  }
  return out;
}

function b64decode(s) {
  if (typeof Buffer !== "undefined") return new Uint8Array(Buffer.from(s, "base64"));
  if (typeof atob === "function") return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
  const cleanStr = String(s).replace(/[\r\n\s]/g, "");
  const pad = cleanStr.endsWith("==") ? 2 : cleanStr.endsWith("=") ? 1 : 0;
  const out = new Uint8Array(((cleanStr.length * 3) >> 2) - pad);
  let o = 0;
  for (let i = 0; i < cleanStr.length; i += 4) {
    const c0 = B64_ALPHABET.indexOf(cleanStr[i]);
    const c1 = B64_ALPHABET.indexOf(cleanStr[i + 1]);
    const c2 = cleanStr[i + 2] === "=" ? 0 : B64_ALPHABET.indexOf(cleanStr[i + 2]);
    const c3 = cleanStr[i + 3] === "=" ? 0 : B64_ALPHABET.indexOf(cleanStr[i + 3]);
    const n = (c0 << 18) | (c1 << 12) | (c2 << 6) | c3;
    if (o < out.length) out[o++] = (n >> 16) & 255;
    if (o < out.length) out[o++] = (n >> 8) & 255;
    if (o < out.length) out[o++] = n & 255;
  }
  return out;
}

export class Boat {
  constructor({ url = env("BOAT_URL") || env("BOAT_API_URL"), key = env("BOAT_API_KEY"), fetch: f = globalThis.fetch } = {}) {
    const origin = apiOrigin(url);
    if (!origin || !key) throw new Error("Boat url and key are required (options or BOAT_URL/BOAT_API_URL/BOAT_API_KEY)");
    this.url = origin;
    this.key = key;
    this.fetch = f;
  }

  async request(method, path, { body, raw, signal, headers: extra } = {}) {
    const headers = { Authorization: `Bearer ${this.key}`, ...(extra ?? {}) };
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
      let vm;
      let vms;
      try {
        const parsed = JSON.parse(text);
        e = parsed.error || {};
        vm = parsed.vm ?? (parsed.id ? parsed : undefined);
        if (Array.isArray(parsed.vms)) vms = parsed.vms.filter((v) => v && typeof v === "object");
      } catch {}
      throw new BoatError(res.status, e.code || "http_error", e.message || text.slice(0, 200) || res.statusText, e.request_id || "", vm && typeof vm === "object" ? vm : undefined, vms);
    }
    return res;
  }

  async call(method, path, body, { idempotencyKey } = {}) {
    const res = await this.request(method, path, { body, headers: idemHeaders(idempotencyKey) });
    const text = await res.text();
    return text.trim() ? JSON.parse(text) : null;
  }

  async *stream(path, body, signal) {
    const res = await this.request("POST", path, { body, signal });
    const decoder = new TextDecoder();
    let buf = "";
    const emit = (line) => {
      const ev = JSON.parse(line);
      const code = ev.code ?? (ev.error && typeof ev.error === "object" ? ev.error.code : undefined) ?? null;
      return { stream: ev.stream || "", data: ev.data_b64 ? b64decode(ev.data_b64) : new Uint8Array(), exitCode: ev.timed_out ? 124 : ev.exit ?? null, error: ev.error ?? null, code };
    };
    for await (const chunk of streamChunks(res.body)) {
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
  /** from/to: "YYYY-MM-DD" or RFC3339 string, or a Date (sent as RFC3339 UTC). A date-only `to` is the end of that UTC day (inclusive day). */
  usage({ from, to } = {}) {
    const bound = (v) => (v instanceof Date ? v.toISOString() : v);
    const p = new URLSearchParams(clean({ from: bound(from), to: bound(to) })).toString();
    return this.call("GET", "/v1/usage" + (p ? "?" + p : ""));
  }

  _wrapVM(vm) {
    if (!vm || typeof vm !== "object") return vm;
    const client = this;
    Object.defineProperties(vm, {
      update: {
        value: async (patch = {}) => {
          const updated = await client.update(vm.id, patch);
          Object.assign(vm, updated);
          return updated;
        },
        configurable: true,
        writable: true,
      },
      start: {
        value: async (opts) => {
          const updated = await client.start(vm.id, opts);
          Object.assign(vm, updated);
          return updated;
        },
        configurable: true,
        writable: true,
      },
      stop: {
        value: async (opts) => {
          const updated = await client.stop(vm.id, opts);
          Object.assign(vm, updated);
          return updated;
        },
        configurable: true,
        writable: true,
      },
      fork: {
        value: (nameOrOpts, opts) => client.fork(vm.id, nameOrOpts, opts),
        configurable: true,
        writable: true,
      },
      clone: {
        value: (nameOrOpts, opts) => client.clone(vm.id, nameOrOpts, opts),
        configurable: true,
        writable: true,
      },
      grantSSH: {
        value: (publicKeyOrOpts) => client.grantSSH(vm.id, publicKeyOrOpts),
        configurable: true,
        writable: true,
      },
      revokeSSH: {
        value: (fingerprint) => client.revokeSSH(vm.id, fingerprint),
        configurable: true,
        writable: true,
      },
      revokePortTokens: {
        value: () => client.revokePortTokens(vm.id),
        configurable: true,
        writable: true,
      },
      exec: {
        value: (cmd, opts) => client.exec(vm.id, cmd, opts),
        configurable: true,
        writable: true,
      },
      run: {
        value: (cmd, opts) => client.run(vm.id, cmd, opts),
        configurable: true,
        writable: true,
      },
      delete: {
        value: () => client.delete(vm.id),
        configurable: true,
        writable: true,
      },
    });
    return vm;
  }

  async listVMs() { return (await this.call("GET", "/v1/vms")).vms.map((v) => this._wrapVM(v)); }
  async getVM(id) { return this._wrapVM(await this.call("GET", `/v1/vms/${q(id)}`)); }
  /** opts.idempotencyKey: sent as Idempotency-Key; a retry with the same key and body returns the first answer. */
  /** A VM whose environment delivery failed (env_error) throws BoatError code "env_failed" naming the VM id (it stays up); opts.allowEnvError: true returns it anyway. */
  async createVM(opts = {}) {
    const out = await this.call("POST", "/v1/vms", createBody(opts), { idempotencyKey: opts.idempotencyKey });
    if (!(opts.allowEnvError ?? opts.allow_env_error)) assertEnvDelivered([out]);
    return this._wrapVM(out);
  }
  create(opts = {}) { return this.createVM(opts); }
  /** count (1-16) VMs in one request, all or none; a name x becomes x-1 … x-N. A fresh-boot failure (502 env_start_failed) keeps the VMs stopped and lists them in the error's `vms`. */
  async createVMs(count, opts = {}) {
    const out = await this.call("POST", "/v1/vms", { ...createBody(opts), count }, { idempotencyKey: opts.idempotencyKey });
    const raw = out.vms ?? [out];
    if (!(opts.allowEnvError ?? opts.allow_env_error)) assertEnvDelivered(raw);
    return raw.map((v) => this._wrapVM(v));
  }
  async update(id, patch = {}) {
    return this._wrapVM((await this.call("PATCH", `/v1/vms/${q(id)}`, updateBody(patch))) ?? { id });
  }
  updateVM(id, patch = {}) { return this.update(id, patch); }
  patchVM(id, patch = {}) { return this.update(id, patch); }
  /** Per-VM exact usage. since/until: RFC3339 string, unix seconds (number), or Date (sent as RFC3339 UTC). */
  vmUsage(id, { since, until } = {}) {
    const bound = (v) => (v instanceof Date ? v.toISOString() : (typeof v === "number" ? String(v) : v));
    const p = new URLSearchParams(clean({ since: bound(since), until: bound(until) })).toString();
    return this.call("GET", `/v1/vms/${q(id)}/usage` + (p ? "?" + p : ""));
  }
  /** A response with env_error (environment not delivered) throws BoatError "env_failed" unless opts.allowEnvError: true. */
  async start(id, { failFast, fail_fast, env: e, noEnv, no_env, environment, ttlS, ttl_s, allowEnvError, allow_env_error } = {}) {
    if (environment && (noEnv ?? no_env)) throw new TypeError("environment and noEnv are mutually exclusive");
    const ff = failFast ?? fail_fast;
    const body = clean({
      fail_fast: ff ? true : undefined,
      env: e,
      no_env: noEnv ?? no_env,
      environment,
      ttl_s: ttlS ?? ttl_s,
    });
    const allow = allowEnvError ?? allow_env_error;
    let out;
    try {
      out = (await this.call("POST", `/v1/vms/${q(id)}/start`, Object.keys(body).length ? body : undefined)) ?? { id };
    } catch (err) {
      // legacy servers: bare 502 env_update_failed (VM is up); with allowEnvError hand back the VM when the error carries it
      if (allow && err instanceof BoatError && err.code === "env_update_failed" && err.vm) return this._wrapVM({ ...err.vm, env_error: err.vm.env_error || err.message });
      throw err;
    }
    if (!allow) assertEnvDelivered([out]);
    return this._wrapVM(out);
  }
  /** force: kill without a snapshot; the next start is a cold boot and loses unsynced writes. */
  async stop(id, { force } = {}) {
    return this._wrapVM((await this.call("POST", `/v1/vms/${q(id)}/stop`, force ? { force: true } : undefined)) ?? { id });
  }
  async fork(id, nameOrOpts = {}, maybeOpts = {}) {
    const o = typeof nameOrOpts === "string" ? maybeOpts : nameOrOpts;
    const out = await this.call("POST", `/v1/vms/${q(id)}/fork`, forkOrCloneBody(nameOrOpts, maybeOpts), { idempotencyKey: o?.idempotencyKey });
    if (!(o?.allowEnvError ?? o?.allow_env_error)) assertEnvDelivered([out]);
    return this._wrapVM(out);
  }
  async clone(id, nameOrOpts = {}, maybeOpts = {}) {
    const o = typeof nameOrOpts === "string" ? maybeOpts : nameOrOpts;
    const out = await this.call("POST", `/v1/vms/${q(id)}/clone`, forkOrCloneBody(nameOrOpts, maybeOpts));
    if (!(o?.allowEnvError ?? o?.allow_env_error)) assertEnvDelivered([out]);
    return this._wrapVM(out);
  }
  cloneVM(id, nameOrOpts = {}, maybeOpts = {}) { return this.clone(id, nameOrOpts, maybeOpts); }
  async delete(id) { await this.call("DELETE", `/v1/vms/${q(id)}`); }

  exec(id, cmd, { env: e, cwd, timeoutS, stdin, tty, signal } = {}) {
    let stdinB64;
    if (stdin !== undefined && stdin !== null) {
      const raw = typeof stdin === "string" ? new TextEncoder().encode(stdin) : stdin;
      stdinB64 = b64encode(raw);
    }
    return this.stream(
      `/v1/vms/${q(id)}/exec`,
      clean({ cmd, env: e, cwd, timeout_s: timeoutS, stdin_b64: stdinB64, tty: tty ? true : undefined }),
      signal,
    );
  }
  async run(id, cmd, opts) {
    const out = [], err = [];
    let exitCode = null;
    for await (const ev of this.exec(id, cmd, opts)) {
      if (ev.error) throw streamError(ev);
      (ev.stream === "stderr" ? err : out).push(ev.data);
      if (ev.exitCode !== null) exitCode = ev.exitCode;
    }
    if (exitCode === null) {
      throw new BoatError(502, "stream_ended", "exec stream ended without exit status");
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
  /** Copy a local directory (Node only; needs git and tar). `to` defaults to ~/<dir name>. */
  async push(id, directory, { to, signal, _deadlineMs } = {}) {
    const { packDirectory } = await import("./push.js");
    const { name, data, files } = await packDirectory(directory);
    const tmp = `/tmp/.boat-push-${randomHex(8)}.tgz`;
    let putSignal = signal;
    if (_deadlineMs !== undefined) {
      const remMs = _deadlineMs - Date.now();
      if (remMs <= 0) throw new BoatTimeoutError("push timed out");
      putSignal = AbortSignal.timeout(remMs);
    }
    await this.writeFile(id, tmp, data, { signal: putSignal });
    let runOpts = signal ? { signal } : undefined;
    if (_deadlineMs !== undefined) {
      const remMs = _deadlineMs - Date.now();
      if (remMs <= 0) throw new BoatTimeoutError("push timed out");
      runOpts = { timeoutS: Math.max(1, Math.ceil(remMs / 1000)), signal: AbortSignal.timeout(remMs) };
    }
    const res = await this.run(id, ["bash", "-c", UNPACK, to ?? name, tmp], runOpts);
    if (_deadlineMs !== undefined && res.exitCode === 124) throw new BoatTimeoutError("push timed out");
    if (res.exitCode !== 0) throw new BoatError(0, "push_failed", `unpack on the VM: ${res.stderr.trim()}`);
    return { path: res.stdout.trim(), files };
  }

  async _waitSetupCmd(vmId, cmdId, deadlineMs) {
    let exitCode = null;
    let signal = null;
    let tail = new Uint8Array();
    let offset = 0;
    let backoffMs = 50;
    const tailDetail = () => {
      const text = new TextDecoder().decode(tail).trim();
      return text ? `: ${text.slice(-200)}` : "";
    };
    for (;;) {
      let progressed = false;
      let reqSignal;
      if (deadlineMs !== undefined) {
        const remMs = deadlineMs - Date.now();
        if (remMs <= 0) {
          throw new BoatError(0, "setup_failed", `environment setup timed out${tailDetail()}`);
        }
        reqSignal = AbortSignal.timeout(remMs);
      }
      try {
        for await (const ev of this.followCommand(vmId, cmdId, { offset, signal: reqSignal })) {
          if (ev.data.length) {
            progressed = true;
            const combined = concat([tail, ev.data]);
            tail = combined.length > 2048 ? combined.subarray(combined.length - 2048) : combined;
          }
          if (typeof ev.nextOffset === "number" && ev.nextOffset >= 0) {
            if (ev.nextOffset > offset) progressed = true;
            offset = ev.nextOffset;
          }
          if (ev.exitCode !== null) {
            progressed = true;
            exitCode = ev.exitCode;
          }
          if (ev.error) {
            progressed = true;
            signal = ev.error;
          }
        }
        break;
      } catch (err) {
        if (deadlineMs !== undefined && (Date.now() >= deadlineMs || isAbortTimeout(err, reqSignal))) {
          throw new BoatError(0, "setup_failed", `environment setup timed out${tailDetail()}`);
        }
        if (err instanceof BoatError && err.code === "reconnect") {
          if (typeof err.nextOffset === "number" && err.nextOffset >= 0) {
            if (err.nextOffset > offset) progressed = true;
            offset = err.nextOffset;
          }
          if (progressed) backoffMs = 50;
          let waitMs = backoffMs;
          if (deadlineMs !== undefined) {
            const remMs = deadlineMs - Date.now();
            if (remMs <= 0) {
              throw new BoatError(0, "setup_failed", `environment setup timed out${tailDetail()}`);
            }
            waitMs = Math.min(backoffMs, remMs);
          }
          await sleepMs(waitMs);
          if (deadlineMs !== undefined && Date.now() >= deadlineMs) {
            throw new BoatError(0, "setup_failed", `environment setup timed out${tailDetail()}`);
          }
          if (!progressed) backoffMs = Math.min(backoffMs * 2, 1000);
          continue;
        }
        throw err;
      }
    }
    if (signal) {
      throw new BoatError(0, "setup_failed", `environment setup killed by signal ${signal}${exitCode !== null ? ` (exit ${exitCode})` : ""}${tailDetail()}`);
    }
    if (exitCode === null) {
      throw new BoatError(0, "setup_failed", `environment setup ended without an exit status${tailDetail()}`);
    }
    if (exitCode !== 0) {
      throw new BoatError(0, "setup_failed", `environment setup exited with code ${exitCode}${tailDetail()}`);
    }
  }

  /** `boat run`: new VM, optional push, run cmd, delete. Other options go to createVM.
   * Creates the VM with a 15-minute TTL (renewed every 5 minutes while running). A timeout gives exitCode 124. */
  async runJob(cmd, { push, cwd, timeoutS, timeout_s, keep = false, ...create } = {}) {
    const effTimeoutS = timeoutS ?? timeout_s;
    if (effTimeoutS !== undefined && (typeof effTimeoutS !== "number" || !Number.isFinite(effTimeoutS) || effTimeoutS <= 0)) {
      throw new BoatTimeoutError("timeoutS must be > 0");
    }
    if (push !== undefined) await (await import("./push.js")).assertDirectory(push); // before a VM exists
    const hasCustomTtl = create.ttlS !== undefined || create.ttl_s !== undefined;
    const effTtlS = hasCustomTtl ? (create.ttlS ?? create.ttl_s) : JOB_TTL_S;
    const vm = await this.createVM({
      idleStopS: JOB_IDLE_STOP_S,
      ...(hasCustomTtl ? {} : { ttlS: JOB_TTL_S }),
      ...create,
      allowEnvError: true,
    }); // env_error is handled below, with cleanup
    const start = Date.now();
    const deadlineMs = effTimeoutS !== undefined ? start + effTimeoutS * 1000 : undefined;
    let excActive = false;
    let deleted = false;
    let hbStopped = false;
    let hbTimer = null;
    let hbInFlight = null;
    const hbAbort = new AbortController();
    const hbIntervalMs = this._jobHeartbeatMs ?? JOB_HEARTBEAT_MS;
    const scheduleBeat = () => {
      if (hbStopped || !(typeof effTtlS === "number" && effTtlS > 0) || !(hbIntervalMs > 0)) return;
      hbTimer = setTimeout(() => {
        hbTimer = null;
        if (hbStopped) return;
        hbInFlight = this.request("PATCH", `/v1/vms/${q(vm.id)}`, {
          body: { ttl_s: effTtlS },
          signal: hbAbort.signal,
        }).then((r) => r.text().catch(() => {})).catch(() => {}).finally(() => {
          hbInFlight = null;
          scheduleBeat();
        });
      }, hbIntervalMs);
      if (typeof hbTimer?.unref === "function") hbTimer.unref();
    };
    const stopHeartbeat = async () => {
      hbStopped = true;
      if (hbTimer !== null) {
        clearTimeout(hbTimer);
        hbTimer = null;
      }
      hbAbort.abort();
      if (hbInFlight) await hbInFlight;
    };
    scheduleBeat();
    try {
      if (vm.env_error) {
        throw new BoatError(0, "env_failed", `environment delivery failed: ${vm.env_error}`);
      }
      if (vm.setup_error) {
        throw new BoatError(0, "setup_failed", `environment setup failed: ${vm.setup_error}`);
      }
      if (vm.setup_cmd_id) {
        await this._waitSetupCmd(vm.id, vm.setup_cmd_id, deadlineMs);
      }
      let res = null;
      let where = cwd;
      if (push !== undefined) { // always upload; cwd only sets the working dir (as `boat run`)
        if (deadlineMs !== undefined) {
          const remMs = deadlineMs - Date.now();
          if (remMs <= 0) {
            res = { stdout: "", stderr: "", exitCode: 124 };
          } else {
            const pushSignal = AbortSignal.timeout(remMs);
            try {
              const pushed = (await this.push(vm.id, push, { signal: pushSignal, _deadlineMs: deadlineMs })).path;
              if (where === undefined) where = pushed;
            } catch (err) {
              if (err instanceof BoatTimeoutError || Date.now() >= deadlineMs || isAbortTimeout(err, pushSignal)) {
                res = { stdout: "", stderr: "", exitCode: 124 };
              } else {
                throw err;
              }
            }
          }
        } else {
          const pushed = (await this.push(vm.id, push)).path;
          if (where === undefined) where = pushed;
        }
      }
      if (res === null) {
        let argv = cmd;
        if (where && !where.startsWith("/")) { // relative to home, which only the VM knows
          argv = ["bash", "-c", 'cd -- "$HOME/$0" && exec "$@"', where, ...cmd];
          where = undefined;
        }
        if (deadlineMs === undefined) {
          res = await this.run(vm.id, argv, { cwd: where, timeoutS: NO_LIMIT_S });
        } else {
          const remMs = deadlineMs - Date.now();
          if (remMs <= 0) {
            res = { stdout: "", stderr: "", exitCode: 124 };
          } else {
            const execSignal = AbortSignal.timeout(remMs);
            const execTimeoutS = Math.max(1, Math.ceil(remMs / 1000));
            try {
              res = await this.run(vm.id, argv, { cwd: where, timeoutS: execTimeoutS, signal: execSignal });
            } catch (err) {
              if (Date.now() >= deadlineMs || isAbortTimeout(err, execSignal)) {
                res = { stdout: "", stderr: "", exitCode: 124 };
              } else {
                throw err;
              }
            }
          }
        }
      }
      await stopHeartbeat();
      if (!keep) {
        await this.delete(vm.id);
        deleted = true;
      }
      return { ...res, elapsedS: (Date.now() - start) / 1000, size: vm.size, vmId: vm.id, deleted };
    } catch (err) {
      excActive = true;
      throw err;
    } finally {
      await stopHeartbeat();
      if (!keep && excActive) {
        try {
          await this.delete(vm.id);
        } catch {}
      }
    }
  }

  /** Run an iterable of job specs across isolated VMs in parallel and return results in input order. */
  async runJobs(jobs, { concurrency = 16 } = {}) {
    if (!Number.isInteger(concurrency) || concurrency < 1) {
      throw new RangeError("concurrency must be an integer >= 1");
    }
    const parsed = [];
    let assertDir = null;
    for (const spec of jobs) {
      if (Array.isArray(spec)) {
        if (spec.length === 0) throw new TypeError("job command array must not be empty");
        parsed.push({ cmd: spec.map(String), opts: {} });
      } else if (spec && typeof spec === "object") {
        const { cmd, ...opts } = spec;
        if (!Array.isArray(cmd) || cmd.length === 0) {
          throw new TypeError("each job object must include a non-empty cmd array");
        }
        const effTimeoutS = opts.timeoutS ?? opts.timeout_s;
        if (effTimeoutS !== undefined && (typeof effTimeoutS !== "number" || !Number.isFinite(effTimeoutS) || effTimeoutS <= 0)) {
          throw new BoatTimeoutError("timeoutS must be > 0");
        }
        if (opts.push !== undefined) {
          if (!assertDir) assertDir = (await import("./push.js")).assertDirectory;
          await assertDir(opts.push);
        }
        parsed.push({ cmd: cmd.map(String), opts });
      } else {
        throw new TypeError("each job must be a command array or an object with cmd");
      }
    }
    if (parsed.length === 0) return [];

    const results = new Array(parsed.length);
    let nextIndex = 0;
    let firstError = null;

    const worker = async () => {
      for (;;) {
        if (firstError !== null) return;
        const i = nextIndex++;
        if (i >= parsed.length) return;
        const { cmd, opts } = parsed[i];
        try {
          results[i] = await this.runJob(cmd, opts);
        } catch (err) {
          if (firstError === null) firstError = err;
          return;
        }
      }
    };

    const workerCount = Math.min(concurrency, parsed.length);
    await Promise.all(Array.from({ length: workerCount }, () => worker()));
    if (firstError !== null) throw firstError;
    return results;
  }

  async writeFile(id, path, data, { signal } = {}) {
    const raw = typeof data === "string" ? new TextEncoder().encode(data) : data;
    await (await this.request("PUT", `/v1/vms/${q(id)}/files?path=${q(path)}`, { raw, signal })).text();
  }

  /** `boat host`: expose a port (private unless public) and return its URL. */
  async hostPort(id, port, { public: pub = false, ttlS = 7 * 24 * 60 * 60 } = {}) {
    await this.addPort(id, port, { auth: pub ? "public" : "private" });
    return this.portUrl(id, port, { ttlS });
  }
  async ports(id) { return (await this.call("GET", `/v1/vms/${q(id)}/ports`)).ports; }
  addPort(id, port, { public: pub = false, auth } = {}) {
    return this.call("POST", `/v1/vms/${q(id)}/ports`, { port, auth: auth || (pub ? "public" : "private") });
  }
  portUrl(id, port, { ttlS } = {}) {
    return this.call("GET", `/v1/vms/${q(id)}/ports/${Number(port)}/url${ttlS ? "?ttl_s=" + Number(ttlS) : ""}`);
  }
  async removePort(id, port) { await this.call("DELETE", `/v1/vms/${q(id)}/ports/${Number(port)}`); }
  async revokePortTokens(vmId) {
    if (typeof vmId !== "string" || !vmId.trim() || vmId.trim() === "." || vmId.trim() === "..") {
      throw new TypeError("vmId is required");
    }
    return (await this.call("POST", `/v1/vms/${q(vmId)}/port-tokens/revoke`)) ?? { ok: true };
  }
  snapshots(id) { return this.call("GET", `/v1/vms/${q(id)}/snapshots`); }
  createSnapshot(id, { name } = {}) { return this.call("POST", `/v1/vms/${q(id)}/snapshots`, clean({ name })); }
  async deleteSnapshot(id, sid) { await this.call("DELETE", `/v1/vms/${q(id)}/snapshots/${q(sid)}`); }
  grantSSH(id, publicKeyOrOpts = {}) {
    const publicKey = typeof publicKeyOrOpts === "string"
      ? publicKeyOrOpts
      : publicKeyOrOpts?.publicKey ?? publicKeyOrOpts?.public_key;
    return this.call("POST", `/v1/vms/${q(id)}/ssh`, clean({ public_key: publicKey }));
  }
  ssh(id, publicKeyOrOpts = {}) { return this.grantSSH(id, publicKeyOrOpts); }
  async sshKeys(id) { return (await this.call("GET", `/v1/vms/${q(id)}/ssh`)).keys; }
  async revokeSSH(vmId, fingerprint) {
    if (typeof vmId !== "string" || !vmId.trim() || vmId.trim() === "." || vmId.trim() === "..") {
      throw new TypeError("vmId is required");
    }
    if (typeof fingerprint !== "string" || !fingerprint.trim() || fingerprint.trim() === "." || fingerprint.trim() === "..") {
      throw new TypeError("fingerprint is required");
    }
    return (await this.call("DELETE", `/v1/vms/${q(vmId)}/ssh/${q(fingerprint)}`)) ?? { ok: true };
  }
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
      if (ev.reconnect) {
        const error = new BoatError(409, "reconnect", "the VM stopped answering; resume from nextOffset");
        error.nextOffset = ev.next_offset ?? undefined;
        throw error;
      }
      return { stream: ev.stream || "", data: ev.data_b64 ? b64decode(ev.data_b64) : new Uint8Array(), exitCode: ev.exit ?? null, error: ev.signal ?? null, nextOffset: ev.next_offset ?? null };
    };
    for await (const chunk of streamChunks(res.body)) {
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
  createEnvironment(name, { image, size, setupScript, setup_script, env: e, secrets, isDefault, is_default } = {}) {
    return this.call("POST", "/v1/environments", clean({ name, image, size, setup_script: setupScript ?? setup_script, env: e, secrets, is_default: isDefault ?? is_default }));
  }
  getEnvironment(ref) { return this.call("GET", `/v1/environments/${q(ref)}`); }
  updateEnvironment(ref, { name, image, size, setupScript, setup_script, env: e, secrets, resetSecrets, reset_secrets, isDefault, is_default } = {}) {
    return this.call("PUT", `/v1/environments/${q(ref)}`, clean({ name, image, size, setup_script: setupScript ?? setup_script, env: e, secrets, reset_secrets: resetSecrets ?? reset_secrets, is_default: isDefault ?? is_default }));
  }
  async deleteEnvironment(ref) { await this.call("DELETE", `/v1/environments/${q(ref)}`); }
  buildEnvironment(ref) { return this.call("POST", `/v1/environments/${q(ref)}/build`); }
  upgradeEnvironment(ref, opts) {
    if (opts === undefined || opts === null) return this.call("POST", `/v1/environments/${q(ref)}/upgrade`);
    if (typeof opts === "string") throw new TypeError("upgradeEnvironment options must be an object or iterable of IDs, not a string");
    let targets;
    if (Array.isArray(opts) || typeof opts[Symbol.iterator] === "function") {
      targets = Array.from(opts);
    } else {
      const { agentIds, agent_ids, vmIds, vm_ids } = opts;
      const present = [agentIds, agent_ids, vmIds, vm_ids].filter((v) => v !== undefined);
      if (present.length > 1) throw new TypeError("pass only one of agentIds or vmIds");
      const raw = present[0];
      if (raw === null) throw new TypeError("agentIds must be an array, not null");
      if (raw !== undefined) {
        if (typeof raw === "string") throw new TypeError("agentIds must be an iterable of IDs, not a string");
        targets = Array.from(raw);
      }
    }
    return targets === undefined
      ? this.call("POST", `/v1/environments/${q(ref)}/upgrade`)
      : this.call("POST", `/v1/environments/${q(ref)}/upgrade`, { agentIds: targets });
  }

  async resize(id, { size, diskGib, discardMemory } = {}) {
    return this._wrapVM(await this.call("POST", `/v1/vms/${q(id)}/resize`, clean({ size, disk_gib: diskGib, discard_memory: discardMemory })));
  }
  async teamSnapshots({ visibility } = {}) {
    const p = new URLSearchParams(clean({ visibility })).toString();
    return (await this.call("GET", "/v1/snapshots" + (p ? "?" + p : ""))).snapshots;
  }
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
  getTeam(id) { return this.call("GET", `/v1/teams/${q(id)}`); }
  createTeam(name, { owner } = {}) { return this.call("POST", "/v1/teams", clean({ name, owner })); }
  async deleteTeam(id) { await this.call("DELETE", `/v1/teams/${q(id)}`); }
  addMember(teamId, name, { role = "member" } = {}) { return this.call("POST", `/v1/teams/${q(teamId)}/members`, { name, role }); }
  async removeMember(teamId, userId) { return (await this.call("DELETE", `/v1/teams/${q(teamId)}/members/${q(userId)}`)) ?? { ok: true }; }
  setSelfTokenPolicy(teamId, { publicPorts, public_ports, ssh = false } = {}) {
    const pub = publicPorts ?? public_ports ?? false;
    return this.call("PUT", `/v1/teams/${q(teamId)}/self-token`, { public_ports: Boolean(pub), ssh: Boolean(ssh) });
  }
  setShareAllowlist(teamId, teams) {
    const list = Array.isArray(teams)
      ? teams
      : teams && typeof teams === "object" && "teams" in teams
        ? Array.from(teams.teams)
        : Array.from(teams ?? []);
    return this.call("PUT", `/v1/teams/${q(teamId)}/share-allowlist`, { teams: list });
  }
  async *streamEvents(opts = {}) {
    const { after, signal } = typeof opts === "number" ? { after: opts } : (opts ?? {});
    const path = "/v1/events" + (after !== undefined && after !== null ? `?after=${Number(after)}` : "");
    const res = await this.request("GET", path, { signal });
    const decoder = new TextDecoder();
    let buf = "";
    let evId = "";
    let evType = "";
    let dataLines = [];
    const flushLine = (rawLine) => {
      const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
      if (line === "") {
        if (dataLines.length > 0) {
          const ev = decodeSSEEvent(evId, evType, dataLines.join("\n"));
          evType = "";
          dataLines = [];
          return ev;
        }
        evType = "";
        dataLines = [];
        return null;
      }
      if (line.startsWith(":")) return null;
      const sep = line.indexOf(":");
      const field = sep < 0 ? line : line.slice(0, sep);
      let val = sep < 0 ? "" : line.slice(sep + 1);
      if (val.startsWith(" ")) val = val.slice(1);
      if (field === "id") evId = val;
      else if (field === "event") evType = val;
      else if (field === "data") dataLines.push(val);
      return null;
    };
    for await (const chunk of streamChunks(res.body)) {
      buf += decoder.decode(chunk, { stream: true });
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const rawLine = buf.slice(0, i);
        buf = buf.slice(i + 1);
        const ev = flushLine(rawLine);
        if (ev !== null) yield ev;
      }
    }
    buf += decoder.decode();
    if (buf.length > 0) {
      const ev = flushLine(buf);
      if (ev !== null) yield ev;
    }
    if (dataLines.length > 0) {
      yield decodeSSEEvent(evId, evType, dataLines.join("\n"));
    }
  }
  iterEvents(opts = {}) { return this.streamEvents(opts); }
  async keys() { return (await this.call("GET", "/v1/keys")).keys; }
  createKey({ name = "key", scopes, teamId, team_id, expiresInS, expires_in_s } = {}) {
    return this.call("POST", "/v1/keys", clean({ name, scopes, team_id: teamId ?? team_id, expires_in_s: expiresInS ?? expires_in_s }));
  }
  rotateKey(id, { overlapS } = {}) { return this.call("POST", `/v1/keys/${q(id)}/rotate`, clean({ overlap_s: overlapS })); }
  keyUsage(id) { return this.call("GET", `/v1/keys/${q(id)}/usage`); }
  async deleteKey(id) { await this.call("DELETE", `/v1/keys/${q(id)}`); }
  async secrets() { return (await this.call("GET", "/v1/secrets")).secrets; }
  // opts.inject: true marks it default-injected (VMs created without a selection get it); absent keeps the mark.
  async setSecret(name, value, opts = {}) {
    const body = { value };
    if (opts.inject !== undefined) body.inject = !!opts.inject;
    await this.call("PUT", `/v1/secrets/${q(name)}`, body);
  }
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

async function* streamChunks(stream) {
  if (!stream) return;
  if (typeof stream?.[Symbol.asyncIterator] === "function") {
    for await (const chunk of stream) {
      yield chunk;
    }
    return;
  }
  const reader = stream.getReader();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (value !== undefined) yield value;
    }
  } finally {
    reader.releaseLock();
  }
}

function decodeSSEEvent(evId, evType, rawData) {
  let parsed;
  let ok = false;
  try {
    parsed = JSON.parse(rawData);
    ok = true;
  } catch {}
  if (ok && parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
    const out = { ...parsed };
    if (evId && !("id" in out)) out.id = evId;
    if (evType && !("event" in out)) out.event = evType;
    return out;
  }
  const res = { data: ok ? parsed : rawData };
  if (evId) res.id = evId;
  if (evType) res.event = evType;
  return res;
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

function hexDecodeStrict(s) {
  if (typeof s !== "string" || s.length === 0 || s.length % 2 !== 0 || !/^[0-9a-fA-F]+$/.test(s)) {
    return null;
  }
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = Number.parseInt(s.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

function parseSignatureHeader(header) {
  let t = NaN;
  const sigs = [];
  for (const part of String(header).split(",")) {
    const eq = part.indexOf("=");
    if (eq < 0) continue;
    const k = part.slice(0, eq);
    const v = part.slice(eq + 1);
    if (k === "t") t = Number(v);
    else if (k === "v1" && v) sigs.push(v);
  }
  return { t, sigs };
}

/**
 * Verify X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, `${t}.` + body)).
 * During a secret rotation the header has one v1= per secret; any match passes.
 */
export function verifyWebhook(secret, header, body, { toleranceS = 300, now = Date.now() / 1000 } = {}) {
  if (!nodeCrypto) {
    throw new BoatError(0, "unsupported_runtime", "verifyWebhook requires Node.js (node:crypto is unavailable)");
  }
  if (typeof secret !== "string" || secret.length === 0 || typeof header !== "string" || header.length === 0) {
    return false;
  }
  if (body === null || body === undefined) return false;
  const { t, sigs } = parseSignatureHeader(header);
  if (!Number.isInteger(t) || sigs.length === 0 || Math.abs(now - t) > toleranceS) return false;
  const want = nodeCrypto.createHmac("sha256", secret).update(`${t}.`).update(body).digest();
  return sigs.some((sig) => {
    const got = hexDecodeStrict(sig);
    return got !== null && got.length === want.length && nodeCrypto.timingSafeEqual(got, want);
  });
}

/**
 * WebCrypto HMAC-SHA256 verification for Edge, Cloudflare Workers, Deno, browsers, and Node.
 */
export async function verifyWebhookAsync(secret, header, body, toleranceS = 300, opts = {}) {
  const subtle = globalThis.crypto?.subtle;
  if (!subtle) {
    throw new BoatError(0, "unsupported_runtime", "verifyWebhookAsync requires WebCrypto (globalThis.crypto.subtle is unavailable)");
  }
  let tol = 300;
  let now = opts?.now;
  if (typeof toleranceS === "number") {
    tol = toleranceS;
  } else if (toleranceS && typeof toleranceS === "object") {
    if (toleranceS.toleranceS !== undefined) tol = toleranceS.toleranceS;
    if (toleranceS.now !== undefined) now = toleranceS.now;
  }
  if (now === undefined) now = Date.now() / 1000;
  if (typeof secret !== "string" || secret.length === 0 || typeof header !== "string" || header.length === 0) {
    return false;
  }
  if (body === null || body === undefined) return false;
  const { t, sigs } = parseSignatureHeader(header);
  if (!Number.isInteger(t) || sigs.length === 0 || Math.abs(now - t) > tol) return false;
  const enc = new TextEncoder();
  const keyBytes = enc.encode(secret);
  const bodyBytes = typeof body === "string"
    ? enc.encode(body)
    : body instanceof Uint8Array
      ? body
      : new Uint8Array(body);
  const msg = concat([enc.encode(`${t}.`), bodyBytes]);
  const key = await subtle.importKey("raw", keyBytes, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const want = new Uint8Array(await subtle.sign("HMAC", key, msg));
  let matched = 0;
  for (const sig of sigs) {
    const got = hexDecodeStrict(sig);
    if (!got || got.length !== want.length) continue;
    let diff = 0;
    for (let i = 0; i < want.length; i++) {
      diff |= want[i] ^ got[i];
    }
    if (diff === 0) matched = 1;
  }
  return matched === 1;
}
