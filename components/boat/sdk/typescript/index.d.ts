export type Size = "small" | "default" | "large" | "xlarge";
export type VMState = "creating" | "running" | "stopping" | "stopped" | "starting" | "forking" | "error";
export type Scope = "vms:read" | "vms:write" | "exec" | "admin";
export type Agent = "claude" | "codex" | "opencode";

export interface VM {
  id: string;
  name?: string;
  size?: Size;
  state?: VMState;
  image?: string;
  team?: string;
  labels?: Record<string, string>;
  ports?: unknown[];
  [extra: string]: unknown;
}

export interface CreateVMOptions {
  name?: string;
  size?: Size;
  image?: string;
  ttlS?: number;
  labels?: Record<string, string>;
  env?: Record<string, string>;
  /** Team secret names to inject (omit = all, [] = none). Forks inherit it. */
  secrets?: string[];
  /** Auto-stop after this many idle seconds; -1 = never. */
  idleStopS?: number;
  /** Create from a snapshot id of the same team. */
  snapshot?: string;
}

export interface Port { guest_port: number; host_port?: number; hostname?: string; auth: "private" | "public"; [extra: string]: unknown; }
export interface PortURL {
  url: string;
  auth: "private" | "public";
  guest_port: number;
  /** Private routes: send as header X-Boat-Route-Token, or open url in a browser. */
  token?: string;
  expires_at?: string;
}

export interface ExecEvent {
  stream: "stdout" | "stderr" | "";
  data: Uint8Array;
  /** Set on the final event. */
  exitCode: number | null;
  error: unknown;
}

export interface ExecOptions {
  env?: Record<string, string>;
  cwd?: string;
  timeoutS?: number;
  signal?: AbortSignal;
}

export interface PromptOptions {
  agent?: Agent;
  model?: string;
  cwd?: string;
  timeoutS?: number;
  signal?: AbortSignal;
}

export interface RunResult { stdout: string; stderr: string; exitCode: number; }
export interface SSHInfo { host: string; port: number; user: string; jump?: string; }
export interface DesktopURL { url: string; expires_at: string; }
export interface Team { id: string; name: string; role: string; members?: { user_id: string; role: string }[]; created: string; }
export interface Key { id: string; user_id: string; team_id: string; name: string; scopes: Scope[]; created: string; token?: string; }
export interface Webhook { id: string; url: string; events: string[]; created: string; secret?: string; }
export interface Delivery { id: string; webhook_id: string; event_id: string; event: string; attempt: number; status: number; ok: boolean; error?: string; at: string; }

export declare class BoatError extends Error {
  status: number;
  code: string;
  requestId: string;
}

export interface BoatOptions {
  /** Defaults to $BOAT_URL. */
  url?: string;
  /** Defaults to $BOAT_API_KEY. */
  key?: string;
  fetch?: typeof fetch;
}

export declare class Boat {
  constructor(opts?: BoatOptions);
  me(): Promise<{ user: { id: string; name: string }; team: { id: string; name: string }; role: string; scopes: Scope[]; key_id: string }>;
  images(): Promise<{ id: string; default: boolean }[]>;
  usage(range?: { from?: string; to?: string }): Promise<Record<string, unknown>>;

  listVMs(): Promise<VM[]>;
  getVM(id: string): Promise<VM>;
  createVM(opts?: CreateVMOptions): Promise<VM>;
  start(id: string): Promise<VM>;
  stop(id: string): Promise<VM>;
  fork(id: string, opts?: { name?: string }): Promise<VM>;
  delete(id: string): Promise<void>;
  exec(id: string, cmd: string[], opts?: ExecOptions): AsyncGenerator<ExecEvent>;
  run(id: string, cmd: string[], opts?: ExecOptions): Promise<RunResult>;
  prompt(id: string, prompt: string, opts?: PromptOptions): AsyncGenerator<ExecEvent>;
  readFile(id: string, path: string): Promise<Uint8Array>;
  writeFile(id: string, path: string, data: Uint8Array | string): Promise<void>;
  ports(id: string): Promise<Port[]>;
  addPort(id: string, port: number, opts?: { public?: boolean; auth?: "private" | "public" }): Promise<Port>;
  portUrl(id: string, port: number, opts?: { ttlS?: number }): Promise<PortURL>;
  removePort(id: string, port: number): Promise<void>;
  snapshots(id: string): Promise<unknown>;
  createSnapshot(id: string, opts?: { name?: string }): Promise<unknown>;
  deleteSnapshot(id: string, snapshotId: string): Promise<void>;
  ssh(id: string, opts?: { publicKey?: string }): Promise<SSHInfo>;
  desktop(id: string): Promise<DesktopURL>;

  teams(): Promise<Team[]>;
  /** Operator only. With owner, a new user owns the team and `owner.key.token` is returned once. */
  createTeam(name: string, opts?: { owner?: string }): Promise<Team & { owner?: { user: { id: string; name: string }; key: Key } }>;
  /** Operator only; the team must have no VMs. */
  deleteTeam(id: string): Promise<void>;
  addMember(teamId: string, name: string, opts?: { role?: "owner" | "admin" | "member" }): Promise<{ user: { id: string; name: string }; role: string; key: Key }>;
  keys(): Promise<Key[]>;
  createKey(opts?: { name?: string; scopes?: Scope[]; teamId?: string }): Promise<Key>;
  deleteKey(id: string): Promise<void>;
  secrets(): Promise<{ name: string; updated: string }[]>;
  setSecret(name: string, value: string): Promise<void>;
  deleteSecret(name: string): Promise<void>;
  webhooks(): Promise<Webhook[]>;
  createWebhook(url: string, opts?: { events?: string[] }): Promise<Webhook>;
  deleteWebhook(id: string): Promise<void>;
  deliveries(id: string): Promise<Delivery[]>;
  testWebhook(id: string): Promise<string>;
}

export declare function verifyWebhook(
  secret: string,
  header: string,
  body: string | Uint8Array,
  opts?: { toleranceS?: number; now?: number },
): boolean;
