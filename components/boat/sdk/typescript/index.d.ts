export type Size = "small" | "default" | "large" | "xlarge";
export type VMState = "creating" | "running" | "stopping" | "stopped" | "starting" | "forking" | "error";
export type Scope = "vms:read" | "vms:write" | "exec" | "admin";
export type Agent = "claude" | "codex" | "opencode" | "pi" | "prime" | "kimi" | "kimi-code" | "mistral" | "claude-code" | "prime-agent";

export interface VMUpdateOptions {
  name?: string;
  ttlS?: number;
  ttl_s?: number;
  extendS?: number;
  extend_s?: number;
  labels?: Record<string, string>;
  /** Auto-stop after this many idle seconds (-1 = never, 0 = default, >0 = seconds). */
  idleStopS?: number;
  idle_stop_s?: number;
}

export type UpdateOpts = VMUpdateOptions;

export interface StartOptions {
  failFast?: boolean;
  fail_fast?: boolean;
  env?: Record<string, string>;
  noEnv?: boolean;
  no_env?: boolean;
  /** Environment name whose vars/secrets apply; "base" is the team default. Not with noEnv. */
  environment?: string;
  /** Auto-stop TTL in seconds. Omitted preserves the VM's existing ttl_s (including 0); 0 = no auto-stop. */
  ttlS?: number;
  ttl_s?: number;
  /** By default env_error in the response (environment not delivered) throws BoatError code "env_failed"; true returns the VM anyway. */
  allowEnvError?: boolean;
  allow_env_error?: boolean;
}

export interface ForkOptions {
  /** Sent as the Idempotency-Key header (max 255 chars). */
  idempotencyKey?: string;
  name?: string;
  failFast?: boolean;
  fail_fast?: boolean;
  env?: Record<string, string>;
  labels?: Record<string, string>;
  noEnv?: boolean;
  no_env?: boolean;
  /** Environment name whose vars/secrets apply; "base" is the team default. Not with noEnv. */
  environment?: string;
  ttlS?: number;
  ttl_s?: number;
  /** By default env_error in the response (environment not delivered) throws BoatError code "env_failed"; true returns the VM anyway. */
  allowEnvError?: boolean;
  allow_env_error?: boolean;
}

export type CloneOptions = ForkOptions;

export interface VM {
  id: string;
  name?: string;
  size?: Size;
  state?: VMState;
  image?: string;
  team?: string;
  labels?: Record<string, string>;
  ports?: unknown[];
  environment?: string;
  from_build?: boolean;
  setup_cmd_id?: string;
  setup_error?: string;
  env_error?: string;
  agent_ready?: boolean;
  update(patch: VMUpdateOptions): Promise<VM>;
  start(opts?: StartOptions): Promise<VM>;
  stop(opts?: { force?: boolean }): Promise<VM>;
  fork(nameOrOpts?: string | ForkOptions, opts?: ForkOptions): Promise<VM>;
  clone(nameOrOpts?: string | CloneOptions, opts?: CloneOptions): Promise<VM>;
  grantSSH(publicKeyOrOpts?: string | { publicKey?: string }): Promise<SSHInfo>;
  revokeSSH(fingerprint: string): Promise<{ ok: boolean; fingerprint?: string; [extra: string]: unknown }>;
  revokePortTokens(): Promise<{ ok?: boolean; generation?: number; [extra: string]: unknown }>;
  exec(cmd: string[], opts?: ExecOptions): AsyncGenerator<ExecEvent>;
  run(cmd: string[], opts?: ExecOptions): Promise<RunResult>;
  delete(): Promise<void>;
  [extra: string]: unknown;
}

export interface CreateVMOptions {
  /** Sent as the Idempotency-Key header (max 255 chars). */
  idempotencyKey?: string;
  name?: string;
  size?: Size;
  image?: string;
  /** Auto-stop TTL in seconds. Omitted = 1 hour (server default); 0 = no auto-stop. */
  ttlS?: number;
  ttl_s?: number;
  labels?: Record<string, string>;
  env?: Record<string, string>;
  /** Team secret names to inject (omit = the default-injected ones, [] = none). Forks inherit it. */
  secrets?: string[];
  /** Auto-stop after this many idle seconds; -1 = never. */
  idleStopS?: number;
  idle_stop_s?: number;
  /** Create from a snapshot id of the same team. */
  snapshot?: string;
  /** Environment name; its setup runs in the VM unless it has a current build. */
  environment?: string;
  /** Ignore the team's default environment. */
  noEnv?: boolean;
  no_env?: boolean;
  /** A share code bsh_...; copied into your team first. */
  fromShare?: string;
  from?: string;
  /** A full host fails at once with BoatError 503 code no_ready_machine. */
  failFast?: boolean;
  fail_fast?: boolean;
  /** Inject a self-scoped Boat API token into the VM. */
  selfApi?: boolean;
  self_api?: boolean;
  /** By default a VM whose environment delivery failed (env_error) throws BoatError code "env_failed" naming the VM id(s); true returns it anyway. */
  allowEnvError?: boolean;
  allow_env_error?: boolean;
}

export type CreateOpts = CreateVMOptions;

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
  /** Optional guest/stream error code when error is present. */
  code?: string | null;
}

export interface ExecOptions {
  env?: Record<string, string>;
  cwd?: string;
  timeoutS?: number;
  stdin?: string | Uint8Array;
  tty?: boolean;
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
export interface JobResult extends RunResult { elapsedS: number; size: string; vmId: string; deleted: boolean; }
export type JobSpec = string[] | (CreateVMOptions & { cmd: string[]; push?: string; cwd?: string; timeoutS?: number; timeout_s?: number; keep?: boolean });
export interface SSHInfo { host: string; port: number; user: string; jump?: string; }
export interface SSHKeyGrant { fingerprint: string; comment?: string; [extra: string]: unknown; }
export interface SelfTokenPolicy { public_ports: boolean; ssh: boolean; }
export interface BoatEvent { seq?: number; type?: string; vm_id?: string; id?: string; event?: string; [extra: string]: unknown; }
export interface DesktopURL { url: string; expires_at: string; }
export interface Team { id: string; name: string; role: string; members?: { user_id: string; role: string }[]; created: string; }
export interface Key { id: string; user_id: string; team_id: string; name: string; scopes: Scope[]; created: string; token?: string; rotated?: string; prev_until?: string; expires?: string; }
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
export interface EnvironmentOptions { name?: string; image?: string; size?: string; setupScript?: string; setup_script?: string; env?: Record<string, string>; secrets?: string[]; resetSecrets?: boolean; reset_secrets?: boolean; isDefault?: boolean; is_default?: boolean; }
export interface EnvironmentUpgradeOptions { agentIds?: Iterable<string>; agent_ids?: Iterable<string>; vmIds?: Iterable<string>; vm_ids?: Iterable<string>; }
export interface EnvironmentUpgradeResult { environment: string; version: number; upgraded: number; upgraded_live: number; upgraded_stopped: number; failed: number; }
export interface Snapshot { id: string; name?: string | null; vm_id?: string; team?: string; [extra: string]: unknown; }
export interface Share { id: string; vm_id?: string; snapshot_id: string; grantee?: string; expires?: string; uses: number; created: string; code?: string; url?: string; }
export interface AICredential { provider: string; last4: string; updated: string; }
export interface AIProxy { enabled: boolean; available: boolean; }
export interface AIUsageRow { team_id?: string; vm_id: string; provider: string; model: string; requests: number; input_tokens: number; output_tokens: number; unmetered?: number; updated?: string; }
export interface AIUsage { usage: AIUsageRow[]; totals: { requests: number; input_tokens: number; output_tokens: number; unmetered: number }; }
export interface Telemetry { configured: boolean; endpoint?: string; header_names?: string[]; updated?: string; }

export declare class BoatError extends Error {
  status: number;
  code: string;
  requestId: string;
  /** Server-provided byte cursor for explicit resume after a reconnect; absent when unknown. */
  nextOffset?: number;
  /** VM body carried by some error answers (legacy 502 env_update_failed on start); pass allowEnvError to get it back instead of a throw. */
  vm?: Record<string, unknown>;
  /** The VMs of a batch create whose fresh boot failed closed (502 env_start_failed): every VM of the batch, the failed ones stopped. */
  vms?: Record<string, unknown>[];
}

export declare class BoatTimeoutError extends BoatError {}

export interface BoatOptions {
  /** Defaults to $BOAT_URL or $BOAT_API_URL (a trailing /api/v1 or /v1 is stripped). */
  url?: string;
  /** Defaults to $BOAT_API_KEY. */
  key?: string;
  fetch?: typeof fetch;
}

export declare class Boat {
  constructor(opts?: BoatOptions);
  me(): Promise<{ user: { id: string; name: string }; team: { id: string; name: string }; role: string; scopes: Scope[]; key_id: string }>;
  images(): Promise<{ id: string; default: boolean }[]>;
  /** from/to: "YYYY-MM-DD", an RFC3339 string, or a Date (sent as RFC3339 UTC). */
  /** A date-only `to` (YYYY-MM-DD) is the end of that UTC day (inclusive day). */
  usage(range?: { from?: string | Date; to?: string | Date }): Promise<Record<string, unknown>>;

  listVMs(): Promise<VM[]>;
  getVM(id: string): Promise<VM>;
  createVM(opts?: CreateVMOptions): Promise<VM>;
  create(opts?: CreateVMOptions): Promise<VM>;
  /** count (1-16) VMs in one request, all or none; a name x becomes x-1 … x-N. */
  createVMs(count: number, opts?: CreateVMOptions): Promise<VM[]>;
  update(id: string, patch: VMUpdateOptions): Promise<VM>;
  updateVM(id: string, patch: VMUpdateOptions): Promise<VM>;
  patchVM(id: string, patch: VMUpdateOptions): Promise<VM>;
  /** Exact per-VM usage for a window. since/until accept RFC3339 strings, unix seconds, or Date (sent as RFC3339 UTC). */
  vmUsage(id: string, range?: { since?: string | number | Date; until?: string | number | Date }): Promise<Record<string, unknown>>;
  start(id: string, opts?: StartOptions): Promise<VM>;
  /** force: kill without a snapshot; the next start is a cold boot and loses unsynced writes. */
  stop(id: string, opts?: { force?: boolean }): Promise<VM>;
  fork(id: string, nameOrOpts?: string | ForkOptions, opts?: ForkOptions): Promise<VM>;
  clone(id: string, nameOrOpts?: string | CloneOptions, opts?: CloneOptions): Promise<VM>;
  cloneVM(id: string, nameOrOpts?: string | CloneOptions, opts?: CloneOptions): Promise<VM>;
  delete(id: string): Promise<void>;
  exec(id: string, cmd: string[], opts?: ExecOptions): AsyncGenerator<ExecEvent>;
  run(id: string, cmd: string[], opts?: ExecOptions): Promise<RunResult>;
  /** Copy a local directory (Node only; needs git and tar). `to` defaults to ~/<dir name>. */
  push(id: string, directory: string, opts?: { to?: string; signal?: AbortSignal }): Promise<{ path: string; files: number }>;
  /** `boat run`: new VM (15-minute TTL, renewed every 5 minutes), optional push, run cmd, delete. A timeout gives exitCode 124. */
  runJob(cmd: string[], opts?: CreateVMOptions & { push?: string; cwd?: string; timeoutS?: number; timeout_s?: number; keep?: boolean }): Promise<JobResult>;
  /** Execute an iterable of job specs across isolated VMs in parallel and return results in input order. */
  runJobs(jobs: Iterable<JobSpec>, opts?: { concurrency?: number }): Promise<JobResult[]>;
  /** `boat host`: expose a port (private unless public) and return its URL. */
  hostPort(id: string, port: number, opts?: { public?: boolean; ttlS?: number }): Promise<PortURL>;
  prompt(id: string, prompt: string, opts?: PromptOptions): AsyncGenerator<ExecEvent>;
  readFile(id: string, path: string): Promise<Uint8Array>;
  writeFile(id: string, path: string, data: Uint8Array | string, opts?: { signal?: AbortSignal }): Promise<void>;
  ports(id: string): Promise<Port[]>;
  addPort(id: string, port: number, opts?: { public?: boolean; auth?: "private" | "public" }): Promise<Port>;
  portUrl(id: string, port: number, opts?: { ttlS?: number }): Promise<PortURL>;
  removePort(id: string, port: number): Promise<void>;
  revokePortTokens(vmId: string): Promise<{ ok?: boolean; generation?: number; [extra: string]: unknown }>;
  snapshots(id: string): Promise<{ snapshots: Snapshot[] }>;
  createSnapshot(id: string, opts?: { name?: string }): Promise<Snapshot>;
  deleteSnapshot(id: string, snapshotId: string): Promise<void>;
  grantSSH(id: string, publicKeyOrOpts?: string | { publicKey?: string; public_key?: string }): Promise<SSHInfo>;
  ssh(id: string, publicKeyOrOpts?: string | { publicKey?: string; public_key?: string }): Promise<SSHInfo>;
  sshKeys(id: string): Promise<SSHKeyGrant[]>;
  revokeSSH(vmId: string, fingerprint: string): Promise<{ ok: boolean; fingerprint?: string; [extra: string]: unknown }>;
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
  /** Re-pin sandboxes on this environment to its latest version; pass agentIds/vmIds to target specific sandboxes. */
  upgradeEnvironment(idOrName: string, opts?: EnvironmentUpgradeOptions | Iterable<string>): Promise<EnvironmentUpgradeResult>;

  resize(id: string, opts: { size?: string; diskGib?: number; discardMemory?: boolean }): Promise<VM>;
  teamSnapshots(opts?: { visibility?: "shared" | "all" }): Promise<Snapshot[]>;
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
  getTeam(id: string): Promise<Team>;
  /** Operator only. With owner, a new user owns the team and `owner.key.token` is returned once. */
  createTeam(name: string, opts?: { owner?: string }): Promise<Team & { owner?: { user: { id: string; name: string }; key: Key } }>;
  /** Operator only; the team must have no VMs. */
  deleteTeam(id: string): Promise<void>;
  addMember(teamId: string, name: string, opts?: { role?: "owner" | "admin" | "member" }): Promise<{ user: { id: string; name: string }; role: string; key: Key }>;
  removeMember(teamId: string, userId: string): Promise<{ ok: boolean }>;
  setSelfTokenPolicy(teamId: string, opts?: { publicPorts?: boolean; public_ports?: boolean; ssh?: boolean }): Promise<SelfTokenPolicy>;
  setShareAllowlist(teamId: string, teams: Iterable<string> | { teams: Iterable<string> }): Promise<{ teams: string[] }>;
  streamEvents(opts?: number | { after?: number; signal?: AbortSignal }): AsyncGenerator<BoatEvent>;
  iterEvents(opts?: number | { after?: number; signal?: AbortSignal }): AsyncGenerator<BoatEvent>;
  keys(): Promise<Key[]>;
  createKey(opts?: { name?: string; scopes?: Scope[]; teamId?: string; team_id?: string; expiresInS?: number; expires_in_s?: number }): Promise<Key>;
  /** New secret in token; the old one works for overlapS more (default 24h, 0 = now). */
  rotateKey(id: string, opts?: { overlapS?: number }): Promise<Key>;
  keyUsage(id: string): Promise<{ id: string; requests: number; last_used?: string }>;
  deleteKey(id: string): Promise<void>;
  secrets(): Promise<{ name: string; inject: boolean; updated: string }[]>;
  /** inject: true marks it default-injected (VMs created without a selection get it); absent keeps the mark. */
  setSecret(name: string, value: string, opts?: { inject?: boolean }): Promise<void>;
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
  header: string | null | undefined,
  body: string | Uint8Array,
  opts?: { toleranceS?: number; now?: number },
): boolean;

export declare function verifyWebhookAsync(
  secret: string,
  header: string | null | undefined,
  body: string | Uint8Array,
  toleranceS?: number | { toleranceS?: number; now?: number },
  opts?: { now?: number },
): Promise<boolean>;
