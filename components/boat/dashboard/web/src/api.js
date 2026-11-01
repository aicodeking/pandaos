// boat-api client. DOM-free so `node --test` can exercise it.
// The key travels only in the Authorization header, never in a URL.

export const SESSION_KEY = "boat.apiKey";

export class ApiError extends Error {
  constructor(status, code, message, requestId, retryAfter) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.retryAfter = retryAfter;
  }
}

// The endpoint is not on this server (yet): the caller hides its section.
export const isMissing = (err) => err instanceof ApiError && [404, 405, 501].includes(err.status);

export function errorText(err) {
  if (!(err instanceof ApiError)) return err?.message || String(err);
  const wait = err.status === 429 && err.retryAfter ? ` Retry in ${err.retryAfter}s.` : "";
  const ref = err.requestId ? ` (request ${err.requestId})` : "";
  return `${err.message}${wait}${ref}`;
}

// "" means same origin. Anything else must be a bare http(s) origin, plus an optional path.
export function normalizeBase(base = "") {
  if (base === "") return "";
  const url = new URL(base);
  if (url.protocol !== "https:" && url.protocol !== "http:") throw new Error(`apiBase must be http(s): ${base}`);
  if (url.username || url.password || url.search || url.hash) throw new Error(`apiBase must not carry credentials, a query or a fragment: ${base}`);
  return url.href.replace(/\/+$/, "");
}

export function validKey(key) {
  return typeof key === "string" && /^[\x21-\x7e]{8,512}$/.test(key);
}

export function createSession(storage) {
  return {
    get: () => storage.getItem(SESSION_KEY),
    set: (key) => storage.setItem(SESSION_KEY, key),
    clear: () => storage.removeItem(SESSION_KEY),
  };
}

const seg = encodeURIComponent;

function withQuery(path, query) {
  const params = new URLSearchParams(Object.entries(query ?? {}).filter(([, v]) => v !== undefined && v !== ""));
  const text = params.toString();
  return text ? `${path}?${text}` : path;
}

async function toApiError(res) {
  const text = await res.text().catch(() => "");
  let parsed = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    // Non-JSON error, such as the router's plain 404.
  }
  const e = parsed?.error ?? {};
  return new ApiError(
    res.status,
    e.code ?? `http_${res.status}`,
    e.message ?? (text.slice(0, 200) || `HTTP ${res.status}`),
    e.request_id ?? res.headers.get("X-Request-Id") ?? undefined,
    res.headers.get("Retry-After") ?? undefined,
  );
}

// Yields parsed NDJSON objects from a byte stream. Chunks may split a line.
export async function* readNdjson(stream) {
  const decoder = new TextDecoder();
  let pending = "";
  for await (const chunk of stream) {
    pending += decoder.decode(chunk, { stream: true });
    const lines = pending.split("\n");
    pending = lines.pop();
    for (const line of lines) {
      if (line.trim()) yield JSON.parse(line);
    }
  }
  pending += decoder.decode();
  if (pending.trim()) yield JSON.parse(pending);
}

// Turns exec events into text, keeping one decoder per stream so a multi-byte
// character split across two events still decodes.
export function eventDecoder() {
  const decoders = {};
  return (event) => {
    if (!event.data_b64) return "";
    const name = event.stream ?? "stdout";
    decoders[name] ??= new TextDecoder();
    const bytes = Uint8Array.from(atob(event.data_b64), (c) => c.charCodeAt(0));
    return decoders[name].decode(bytes, { stream: true });
  };
}

const MAX_LOG_PAGES = 50;

export function decodeBase64(b64) {
  return new TextDecoder().decode(Uint8Array.from(atob(b64 ?? ""), (c) => c.charCodeAt(0)));
}

// Follows next_offset until a page comes back empty.
// ponytail: the page cap keeps a chatty log from eating the tab; a "load more" button can replace it if logs outgrow it.
export async function readLogPages(fetchPage) {
  const bytes = [];
  let offset = 0;
  for (let page = 0; page < MAX_LOG_PAGES; page += 1) {
    const { data_b64: data, next_offset: next } = await fetchPage(offset);
    if (!data) break;
    bytes.push(data);
    if (next === undefined || next === offset) break;
    offset = next;
  }
  return bytes.map(decodeBase64).join("");
}

export function createClient({ base = "", getKey, onUnauthorized = () => {}, fetchImpl = globalThis.fetch }) {
  const root = normalizeBase(base);

  async function send(method, path, { body, raw, signal } = {}) {
    const headers = { Authorization: `Bearer ${getKey()}` };
    if (body !== undefined && raw === undefined) headers["Content-Type"] = "application/json";
    const res = await fetchImpl(root + path, {
      method,
      headers,
      signal,
      body: raw ?? (body === undefined ? undefined : JSON.stringify(body)),
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
    });
    if (res.ok) return res;
    const err = await toApiError(res);
    if (res.status === 401) onUnauthorized(err);
    throw err;
  }

  async function json(method, path, body) {
    const res = await send(method, path, { body });
    const text = await res.text();
    return text ? JSON.parse(text) : {};
  }

  async function* streamed(path, body, signal) {
    const res = await send("POST", path, { body, signal });
    yield* readNdjson(res.body);
  }

  const vm = (id, rest = "") => `/v1/vms/${seg(id)}${rest}`;

  return {
    me: () => json("GET", "/v1/me"),
    images: async () => (await json("GET", "/v1/images")).images ?? [],
    usage: (since) => json("GET", withQuery("/v1/usage", { since })),

    listVMs: async () => (await json("GET", "/v1/vms")).vms ?? [],
    createVM: (spec) => json("POST", "/v1/vms", spec),
    getVM: (id) => json("GET", vm(id)),
    deleteVM: (id) => json("DELETE", vm(id)),
    startVM: (id) => json("POST", vm(id, "/start"), {}),
    stopVM: (id) => json("POST", vm(id, "/stop"), {}),
    forkVM: (id, name) => json("POST", vm(id, "/fork"), name ? { name } : {}),

    exec: (id, spec, signal) => streamed(vm(id, "/exec"), spec, signal),
    prompt: (id, spec, signal) => streamed(vm(id, "/prompt"), spec, signal),
    listPrompts: async (id) => (await json("GET", vm(id, "/prompts"))).prompts ?? [],
    promptLogs: (id, pid) => readLogPages((offset) => json("GET", withQuery(vm(id, `/prompts/${seg(pid)}/logs`), { offset: offset || undefined }))),
    interruptPrompt: (id, pid) => json("POST", vm(id, `/prompts/${seg(pid)}/interrupt`), {}),

    listCommands: async (id) => (await json("GET", vm(id, "/commands"))).commands ?? [],
    startCommand: (id, spec) => json("POST", vm(id, "/commands"), spec),
    stopCommand: (id, cid) => json("DELETE", vm(id, `/commands/${seg(cid)}`)),
    commandLogs: (id, cid) => readLogPages((offset) => json("GET", withQuery(vm(id, `/commands/${seg(cid)}/logs`), { offset: offset || undefined }))),
    commandHistory: async (id) => (await json("GET", vm(id, "/command-history"))).commands ?? [],

    listFiles: async (id, path) => (await json("GET", withQuery(vm(id, "/files/list"), { path }))).entries ?? [],
    readFile: async (id, path) => (await send("GET", withQuery(vm(id, "/files"), { path }))).blob(),
    writeFile: (id, path, data) => send("PUT", withQuery(vm(id, "/files"), { path }), { raw: data }),
    deleteFile: (id, path) => json("DELETE", withQuery(vm(id, "/files"), { path })),

    listPorts: async (id) => (await json("GET", vm(id, "/ports"))).ports ?? [],
    addPort: (id, port, auth) => json("POST", vm(id, "/ports"), { port, auth }),
    removePort: (id, port) => json("DELETE", vm(id, `/ports/${seg(port)}`)),
    portUrl: (id, port) => json("GET", vm(id, `/ports/${seg(port)}/url`)),

    listSnapshots: async (id) => (await json("GET", vm(id, "/snapshots"))).snapshots ?? [],
    createSnapshot: (id, name) => json("POST", vm(id, "/snapshots"), name ? { name } : {}),
    deleteSnapshot: (id, sid) => json("DELETE", vm(id, `/snapshots/${seg(sid)}`)),

    ssh: (id, publicKey) => json("POST", vm(id, "/ssh"), publicKey ? { public_key: publicKey } : {}),
    desktop: (id) => json("GET", vm(id, "/desktop")),

    listSecrets: async () => (await json("GET", "/v1/secrets")).secrets ?? [],
    putSecret: (name, value) => json("PUT", `/v1/secrets/${seg(name)}`, { value }),
    deleteSecret: (name) => json("DELETE", `/v1/secrets/${seg(name)}`),

    listKeys: async () => (await json("GET", "/v1/keys")).keys ?? [],
    createKey: (spec) => json("POST", "/v1/keys", spec),
    deleteKey: (id) => json("DELETE", `/v1/keys/${seg(id)}`),

    listWebhooks: async () => (await json("GET", "/v1/webhooks")).webhooks ?? [],
    createWebhook: (spec) => json("POST", "/v1/webhooks", spec),
    deleteWebhook: (id) => json("DELETE", `/v1/webhooks/${seg(id)}`),
    testWebhook: (id) => json("POST", `/v1/webhooks/${seg(id)}/test`, {}),
    listDeliveries: async (id) => (await json("GET", `/v1/webhooks/${seg(id)}/deliveries`)).deliveries ?? [],

    getTeam: (id) => json("GET", `/v1/teams/${seg(id)}`),
    addMember: (team, spec) => json("POST", `/v1/teams/${seg(team)}/members`, spec),
    removeMember: (team, user) => json("DELETE", `/v1/teams/${seg(team)}/members/${seg(user)}`),

    listEnvironments: async () => (await json("GET", "/v1/environments")).environments ?? [],
    createEnvironment: (spec) => json("POST", "/v1/environments", spec),
    deleteEnvironment: (id) => json("DELETE", `/v1/environments/${seg(id)}`),
    buildEnvironment: (id) => json("POST", `/v1/environments/${seg(id)}/build`, {}),
  };
}
