// Pure formatting and parsing helpers. DOM-free.

const DASH = "—";

export function fmtBytes(n) {
  if (!Number.isFinite(n) || n < 0) return DASH;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? value : value.toFixed(1)} ${units[unit]}`;
}

export function fmtDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return DASH;
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

export function fmtTime(iso) {
  if (!iso) return DASH;
  const date = new Date(typeof iso === "number" ? iso * 1000 : iso); // numbers are unix seconds
  return Number.isNaN(date.getTime()) ? DASH : date.toLocaleString();
}

// A running VM counts down from expires_at. A stopped one shows its configured TTL.
export function fmtTTL(vm, now = Date.now()) {
  if (vm.expires_at) {
    const left = (new Date(vm.expires_at).getTime() - now) / 1000;
    if (Number.isNaN(left)) return DASH;
    return left <= 0 ? "expired" : fmtDuration(left);
  }
  return vm.ttl_s ? fmtDuration(vm.ttl_s) : "none";
}

export function fmtSize(vm) {
  const name = vm.size || DASH;
  if (!vm.vcpu || !vm.mem_mib) return name;
  return `${name} (${vm.vcpu} vCPU, ${Math.round(vm.mem_mib / 1024)} GiB)`;
}

export const fmtIP = (vm) => vm.public_ip || vm.ip || DASH;

export function fmtCmd(cmd) {
  return Array.isArray(cmd) ? cmd.join(" ") : String(cmd ?? DASH);
}

// Which lifecycle actions a state allows. Everything else is greyed out while a transition runs.
export function vmActions(state) {
  return {
    start: state === "stopped",
    stop: state === "running",
    fork: state === "running" || state === "stopped",
    remove: !["creating", "stopping", "starting", "forking"].includes(state),
  };
}

// Words safe for a shell stay bare, anything else is single-quoted, so a pasted command cannot inject.
export function shellQuote(word) {
  const text = String(word);
  return /^[\w@%+=:,./-]+$/.test(text) ? text : `'${text.replaceAll("'", `'\\''`)}'`;
}

export function sshCommand({ host, port, user, jump }) {
  const parts = ["ssh"];
  if (jump) parts.push("-J", jump);
  if (port && port !== 22) parts.push("-p", String(port));
  parts.push(user ? `${user}@${host}` : host);
  return parts.map(shellQuote).join(" ");
}

// API data becomes a clickable link only when it is plain http(s), never javascript: or data:.
export function safeHttpUrl(value) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" || url.protocol === "http:" ? url.href : null;
  } catch {
    return null;
  }
}

// "A=1\nB=two" to {A:"1",B:"two"}. Throws on a line with no "=", naming the line.
export function parseKV(text) {
  const entries = String(text ?? "")
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line, i) => {
      const at = line.indexOf("=");
      if (at < 1) throw new Error(`Line ${i + 1} needs KEY=value`);
      return [line.slice(0, at).trim(), line.slice(at + 1)];
    });
  return Object.fromEntries(entries);
}

export function parseList(text) {
  return String(text ?? "")
    .split(/[\s,]+/)
    .filter(Boolean);
}

// An empty string means "not set", so the server default applies.
export function parseOptionalInt(text, label, { min = 0, max = Number.MAX_SAFE_INTEGER } = {}) {
  const trimmed = String(text ?? "").trim();
  if (trimmed === "") return undefined;
  const n = Number(trimmed);
  if (!Number.isInteger(n) || n < min || n > max) throw new Error(`${label} must be a whole number from ${min} to ${max}`);
  return n;
}

export function createVMSpec(form) {
  const labels = parseKV(form.labels);
  const env = parseKV(form.env);
  const spec = {
    name: form.name?.trim() || undefined,
    size: form.size?.trim() || undefined,
    image: form.image || undefined,
    snapshot: form.snapshot?.trim() || undefined,
    environment: form.environment || undefined,
    ttl_s: parseOptionalInt(form.ttl_s, "TTL"),
    idle_stop_s: parseOptionalInt(form.idle_stop_s, "Idle stop"),
    labels: Object.keys(labels).length ? labels : undefined,
    env: Object.keys(env).length ? env : undefined,
    secrets: form.secrets,
  };
  return Object.fromEntries(Object.entries(spec).filter(([, v]) => v !== undefined));
}

export function sinceISO(days, now = Date.now()) {
  return new Date(now - days * 86400000).toISOString();
}

// Largest first. Anything past `limit` folds into a single "Other" row.
export function topRows(rows, limit = 10) {
  const sorted = [...rows].sort((a, b) => b.value - a.value);
  if (sorted.length <= limit) return sorted;
  const rest = sorted.slice(limit - 1).reduce((sum, row) => sum + row.value, 0);
  return [...sorted.slice(0, limit - 1), { label: "Other", value: rest }];
}

export function usageView(report, namesById = {}) {
  const team = report.teams?.[0] ?? {};
  const storage = report.storage?.[0] ?? {};
  const hours = (seconds) => (seconds ?? 0) / 3600;
  return {
    vmHours: hours(team.vm_seconds),
    snapshotDays: team.snapshot_days ?? 0,
    storageBytes: storage.committed_bytes ?? 0,
    bySize: topRows(Object.entries(team.vm_seconds_by_size ?? {}).map(([label, s]) => ({ label, value: hours(s) }))),
    byVM: topRows((report.vms ?? []).map((v) => ({ label: namesById[v.id] || v.id, value: hours(v.vm_seconds) }))),
    bySnapshot: topRows((report.snapshots ?? []).map((s) => ({ label: s.id, value: s.snapshot_days ?? 0 }))),
  };
}
