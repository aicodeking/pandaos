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
  /** Environment name; its setup runs in the VM unless it has a current build. */
  environment?: string;
  /** Ignore the team's default environment. */
  noEnv?: boolean;
  /** A share code bsh_...; copied into your team first. */
  fromShare?: string;
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
export interface Key { id: string; user_id: string; team_id: string; name: string; scopes: Scope[]; created: string; token?: string; rotated?: string; prev_until?: string; }
export interface Webhook { id: string; url: string; events: string[]; created: string; secret?: string; prev_until?: string; }
export interface Delivery { id: string; webhook_id: string; event_id: string; event: string; attempt: number; status: number; ok: boolean; error?: string; at: string; }

export interface Command { id: string; cmd: string[]; state: "running" | "exited" | "signaled" | "lost"; exit?: number | null; signal?: string | null; started_at?: string | null; finished_at?: string | null; pid?: number | null; }
export interface CommandRecord { id: string; cmd: string[]; cwd?: string; kind: "command" | "prompt" | "setup"; key_id?: string; started_at: string; state: string; exit?: number | null; finished_at?: string; }
export interface LogChunk { data_b64: string; next_offset: number; running: boolean; exit?: number | null; signal?: string | null; offset?: number; size?: number; truncated?: boolean; }
export interface FollowEvent extends ExecEvent { nextOffset: number | null; }
export interface FileEntry { name: string; type: "file" | "dir" | "symlink" | "other"; size?: number; mode?: string; mtime?: number; }
export interface FileMatch { path: string; line?: number; text?: string; }
export interface PromptRun { id: string; vm_id: string; conversation_id: string; agent: string; model?: string; prompt: string; cmd_id: string; state: "running" | "completed" | "failed" | "interrupted" | "steered"; exit?: number | null; created: string; finished?: string; steered_by?: string; }
export interface ConversationInfo { id: string; vm_id: string; agent: string; model?: string; prompt_count: number; last_prompt: string; running: boolean; created: string; updated: string; }
export interface EnvBuild { state: "building" | "ready" | "failed"; version: number; vm_id?: string; snapshot_id?: string; error?: string; at: string; }
export interface Environment { id: string; team_id: string; name: string; image?: string; size?: string; setup_script?: string; env?: Record<string, string>; secrets?: string[]; is_default: boolean; version: number; created: string; updated: string; build?: EnvBuild; }
export interface EnvironmentOptions { name?: string; image?: string; size?: string; setupScript?: string; env?: Record<string, string>; secrets?: string[]; isDefault?: boolean; }
export interface Snapshot { id: string; name?: string | null; vm_id?: string; team?: string; [extra: string]: unknown; }
export interface Share { id: string; vm_id?: string; snapshot_id: string; grantee?: string; expires?: string; uses: number; created: string; code?: string; url?: string; }
export interface AICredential { provider: string; last4: string; updated: string; }
export interface AIProxy { enabled: boolean; available: boolean; }
export interface AIUsageRow { team_id?: string; vm_id: string; provider: string; model: string; requests: number; input_tokens: number; output_tokens: number; updated?: string; }
export interface AIUsage { usage: AIUsageRow[]; totals: { requests: number; input_tokens: number; output_tokens: number }; }
export interface Telemetry { configured: boolean; endpoint?: string; header_names?: string[]; updated?: string; }

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

  startCommand(id: string, cmd: string[], opts?: { env?: Record<string, string>; cwd?: string }): Promise<Command>;
  listCommands(id: string): Promise<Command[]>;
  commandStatus(id: string, cmdId: string): Promise<Command>;
  commandHistory(id: string): Promise<CommandRecord[]>;
  commandLogs(id: string, cmdId: string, opts?: { offset?: number; limit?: number }): Promise<LogChunk>;
  followCommand(id: string, cmdId: string, opts?: { offset?: number; signal?: AbortSignal }): AsyncGenerator<FollowEvent>;
  signalCommand(id: string, cmdId: string, signal?: "TERM" | "INT" | "KILL" | "HUP"): Promise<void>;
  stopCommand(id: string, cmdId: string): Promise<void>;
  processes(id: string): Promise<{ pid: number; ppid: number; user: string; elapsed_s: number; command: string }[]>;

  listFiles(id: string, path: string): Promise<FileEntry[]>;
  deleteFile(id: string, path: string, opts?: { recursive?: boolean }): Promise<void>;
  moveFile(id: string, from: string, to: string, opts?: { overwrite?: boolean }): Promise<void>;
  searchFiles(id: string, path: string, query: string, opts?: { content?: boolean; regex?: boolean; limit?: number }): Promise<FileMatch[]>;
  fileInfo(id: string, path: string): Promise<{ path: string; type: string; size: number; mode: string; mtime: number }>;
  makeFolder(id: string, path: string): Promise<void>;
  setFileMode(id: string, path: string, mode: string): Promise<void>;

  /** Start a background turn. Continues the latest conversation unless new or conversationId. */
  startPrompt(id: string, prompt: string, opts?: { agent?: Agent; model?: string; cwd?: string; conversationId?: string; new?: boolean }): Promise<PromptRun>;
  listPrompts(id: string): Promise<PromptRun[]>;
  promptStatus(id: string, promptId: string): Promise<PromptRun>;
  followPrompt(id: string, promptId: string, opts?: { offset?: number; signal?: AbortSignal }): AsyncGenerator<FollowEvent>;
  /** Interrupts the turn and resumes its session with this message; returns the new turn. */
  steerPrompt(id: string, promptId: string, prompt: string): Promise<PromptRun>;
  interruptPrompt(id: string, promptId: string): Promise<PromptRun>;
  conversations(id: string): Promise<ConversationInfo[]>;

  environments(): Promise<Environment[]>;
  createEnvironment(name: string, opts?: EnvironmentOptions): Promise<Environment>;
  getEnvironment(idOrName: string): Promise<Environment>;
  updateEnvironment(idOrName: string, opts: EnvironmentOptions): Promise<Environment>;
  deleteEnvironment(idOrName: string): Promise<void>;
  buildEnvironment(idOrName: string): Promise<Environment>;

  resize(id: string, opts: { size?: string; diskGib?: number; discardMemory?: boolean }): Promise<VM>;
  teamSnapshots(): Promise<Snapshot[]>;
  deleteTeamSnapshot(snapshotId: string): Promise<void>;
  downloadSnapshot(id: string, snapshotId: string): Promise<Uint8Array>;
  importSnapshot(data: Uint8Array, opts?: { name?: string; image?: string; size?: string }): Promise<Snapshot>;
  /** teamId grants that team read access; otherwise a signed link whose code is shown once. */
  shareSnapshot(id: string, snapshotId: string, opts?: { teamId?: string; ttlS?: number }): Promise<Share>;
  snapshotShares(id: string, snapshotId: string): Promise<Share[]>;
  revokeSnapshotShare(id: string, snapshotId: string, shareId: string): Promise<void>;
  sharedSnapshots(): Promise<Share[]>;
  takeSnapshotShare(opts: { shareId?: string; code?: string; name?: string }): Promise<Snapshot>;

  aiCredentials(): Promise<AICredential[]>;
  /** The key stays on the host; VMs get a per-VM token through the proxy. */
  setAICredential(provider: string, value: string): Promise<AICredential>;
  deleteAICredential(provider: string): Promise<void>;
  aiProxy(): Promise<AIProxy>;
  setAIProxy(enabled: boolean): Promise<AIProxy>;
  aiUsage(vmId?: string): Promise<AIUsage>;
  setAIAccess(id: string, enabled: boolean): Promise<void>;

  telemetry(): Promise<Telemetry>;
  setTelemetry(endpoint: string, opts?: { headers?: Record<string, string> }): Promise<Telemetry>;
  deleteTelemetry(): Promise<void>;

  teams(): Promise<Team[]>;
  /** Operator only. With owner, a new user owns the team and `owner.key.token` is returned once. */
  createTeam(name: string, opts?: { owner?: string }): Promise<Team & { owner?: { user: { id: string; name: string }; key: Key } }>;
  /** Operator only; the team must have no VMs. */
  deleteTeam(id: string): Promise<void>;
  addMember(teamId: string, name: string, opts?: { role?: "owner" | "admin" | "member" }): Promise<{ user: { id: string; name: string }; role: string; key: Key }>;
  keys(): Promise<Key[]>;
  createKey(opts?: { name?: string; scopes?: Scope[]; teamId?: string }): Promise<Key>;
  /** New secret in token; the old one works for overlapS more (default 24h, 0 = now). */
  rotateKey(id: string, opts?: { overlapS?: number }): Promise<Key>;
  keyUsage(id: string): Promise<{ id: string; requests: number; last_used?: string }>;
  deleteKey(id: string): Promise<void>;
  secrets(): Promise<{ name: string; updated: string }[]>;
  setSecret(name: string, value: string): Promise<void>;
  deleteSecret(name: string): Promise<void>;
  webhooks(): Promise<Webhook[]>;
  createWebhook(url: string, opts?: { events?: string[] }): Promise<Webhook>;
  getWebhook(id: string): Promise<Webhook>;
  updateWebhook(id: string, opts: { url?: string; events?: string[] }): Promise<Webhook>;
  /** New secret; deliveries carry both signatures during the overlap. */
  rotateWebhook(id: string, opts?: { overlapS?: number }): Promise<Webhook>;
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
