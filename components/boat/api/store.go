package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ponytail: the whole state lives in one JSON file that is rewritten on every
// change and cloned (marshal/unmarshal) per Update. Fine for a single server
// with thousands of keys/webhooks; past ~10 MB of state or ~50 writes/s move
// to SQLite (deliveries first, they are the hottest writer). Writers queue on
// one mutex; readers (auth included) never wait for them.
//
// Update waits for the fsync; Apply does not (a background flusher writes it).
// An fsync can take seconds when the disk holds a lot of dirty data (a VM
// snapshot just written to the same filesystem), so the VM lifecycle paths use
// Apply and keep whatever must survive a crash somewhere durable already (the
// secret selection rides on the VM's boatd labels, see secretsLabel).

type User struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Operator bool      `json:"operator,omitempty"` // instance operator (admin init)
	Created  time.Time `json:"created"`
}

type Member struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

type Team struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Members []Member  `json:"members"`
	Created time.Time `json:"created"`
	// AcceptSharesFrom: teams whose snapshot grants this team sees and takes.
	AcceptSharesFrom []string `json:"accept_shares_from,omitempty"`
	// SelfToken: what a VM's own token may do beyond the default (selftoken.go).
	SelfToken SelfTokenPolicy `json:"self_token,omitzero"`
}

// SelfTokenPolicy: off by default; a team admin opts in.
type SelfTokenPolicy struct {
	PublicPorts bool `json:"public_ports"` // open auth=public ports on its own VM
	SSH         bool `json:"ssh"`          // add ssh keys to its own VM
}

func (t *Team) role(userID string) string {
	for _, m := range t.Members {
		if m.UserID == userID {
			return m.Role
		}
	}
	return ""
}

type APIKey struct {
	ID      string    `json:"id"`
	UserID  string    `json:"user_id"`
	TeamID  string    `json:"team_id"`
	Name    string    `json:"name"`
	Scopes  []string  `json:"scopes"`
	Salt    string    `json:"salt"`
	Hash    string    `json:"hash"`
	Gen     int64     `json:"gen,omitempty"`
	Created time.Time `json:"created"`
	// Rotation overlap: the previous secret keeps working until PrevUntil.
	PrevSalt  string     `json:"prev_salt,omitempty"`
	PrevHash  string     `json:"prev_hash,omitempty"`
	PrevUntil *time.Time `json:"prev_until,omitempty"`
	Rotated   *time.Time `json:"rotated,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"` // nil = never; after it the key is 401
}

type Secret struct {
	TeamID  string    `json:"team_id"`
	Name    string    `json:"name"`
	Sealed  string    `json:"sealed"`
	Updated time.Time `json:"updated"`
	// Inject: an admin marked it default-injected, so a VM created without a
	// selection gets it (docs/boat.md, "Secrets"). Off for every new secret.
	Inject bool `json:"inject,omitempty"`
}

type Webhook struct {
	ID           string    `json:"id"`
	TeamID       string    `json:"team_id"`
	URL          string    `json:"url"`
	Events       []string  `json:"events"`
	SealedSecret string    `json:"sealed_secret"`
	Created      time.Time `json:"created"`
	// Rotation overlap: deliveries also carry a signature with the old secret.
	PrevSealedSecret string     `json:"prev_sealed_secret,omitempty"`
	PrevUntil        *time.Time `json:"prev_until,omitempty"`
	// Registered through /api/v1: boat.dev's name, event names and payloads.
	Name    string    `json:"name,omitempty"`
	Format  string    `json:"format,omitempty"` // "" native | "boatdev"
	Updated time.Time `json:"updated,omitempty"`
}

type Delivery struct {
	ID        string    `json:"id"`
	WebhookID string    `json:"webhook_id"`
	EventID   string    `json:"event_id"`
	Event     string    `json:"event"`
	Attempt   int       `json:"attempt"`
	Status    int       `json:"status"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

const maxDeliveriesPerHook = 100

type State struct {
	Users      map[string]*User      `json:"users"`
	Teams      map[string]*Team      `json:"teams"`
	Keys       map[string]*APIKey    `json:"keys"`
	Secrets    map[string]*Secret    `json:"secrets"` // key: team_id + "/" + name
	Webhooks   map[string]*Webhook   `json:"webhooks"`
	Deliveries map[string][]Delivery `json:"deliveries"` // by webhook id, newest last
	SSHGrants  map[string]*SSHGrant  `json:"ssh_grants"` // key: grantKey(team, "type base64", minting key)
	// VMSecrets: secret names selected at create (absent = all team secrets).
	VMSecrets map[string][]string `json:"vm_secrets"`
	// Commands: detached command history per VM (newest last, capped).
	Commands map[string][]CommandRecord `json:"commands"`
	// Prompts and Conversations per VM (newest last, capped).
	Prompts       map[string][]*PromptRun    `json:"prompts"`
	Conversations map[string][]*Conversation `json:"conversations"`
	Environments  map[string]*Environment    `json:"environments"`
	// VMEnvs: the environment a VM was created from, by VM id.
	VMEnvs map[string]string `json:"vm_envs"`
	// VMEnvPins: the environment version a VM is pinned to and that version's vars,
	// by VM id (r48, boat.dev's pinned environment). A plain start replays the pin,
	// not the environment's current vars; it moves only when a start or fork names
	// an environment, a fork inherits the parent's, or the environment is upgraded.
	VMEnvPins     map[string]*EnvPin               `json:"vm_env_pins,omitempty"`
	VMTransitions map[string]*CredentialTransition `json:"vm_credential_transitions,omitempty"`
	// VMNames: the display name (free text, up to 120 characters) of a VM, by VM id.
	// boatd keeps only the derived DNS slug (r48).
	VMNames map[string]string `json:"vm_names,omitempty"`
	// VMExtraEnv: the per-sandbox env the caller gave (create, start/resume, fork),
	// by VM id: replayed on every later start, over the environment's vars. The
	// state file is sealed.
	VMExtraEnv map[string]map[string]string `json:"vm_extra_env"`
	// VMEnvErrors: per VM, why its last environment push failed (cleared by the
	// next successful push, a stop or the VM's deletion). While set the VM is
	// reported agent_ready=false with env_error.
	VMEnvErrors map[string]string `json:"vm_env_errors,omitempty"`
	// NoEnvRuns: the VMs that are no_env (sticky since r46), by VM id: the time of
	// the latest cold start that marked it. Set by a no_env create, start or fork
	// (and inherited by forks, clones and creates from snapshots of such a VM);
	// kept across stop, start, type change and recover; only the VM's deletion
	// (or a rolled-back first marking) clears it. A no_env VM gets no team secrets,
	// also for prompts and commands.
	NoEnvRuns  map[string]time.Time      `json:"no_env_runs,omitempty"`
	SnapShares map[string]*SnapShare     `json:"snap_shares"`
	AICreds    map[string]*AICredential  `json:"ai_creds"` // key: team_id + "/" + provider
	AIVMs      map[string]*AIVM          `json:"ai_vms"`
	AIProxyOff map[string]bool           `json:"ai_proxy_off"` // team id: proxy off, raw keys injected
	AIUsage    map[string]*AIUsageRow    `json:"ai_usage"`     // key: team_id + "/" + vm_id + "/" + provider + "/" + model
	Telemetry  map[string]*TeamTelemetry `json:"telemetry"`
	// AIUsageDays: the same counters per UTC day (key: AIUsage key + "/" + YYYY-MM-DD),
	// kept aiUsageRetentionDays days. AIUsage stays the lifetime total. Rows that
	// predate this field were attributed to their Updated day once (AIUsageDaysV1).
	AIUsageDays   map[string]*AIUsageRow `json:"ai_usage_days,omitempty"`
	AIUsageDaysV1 bool                   `json:"ai_usage_days_v1,omitempty"`
	// aiIdx: per-team index of the two tables above (aiusage_days.go); not stored.
	aiIdx *aiUsageIndex
	// EnvErrorSpans: per VM, the intervals during which it was running with an
	// environment that never reached the guest (compat state "error"). They are
	// not billed (see envspans.go).
	EnvErrorSpans map[string][]EnvErrorSpan `json:"env_error_spans,omitempty"`
	// EnvErrorCutoffs: tracked VMs and their latest discarded exclusion end (zero
	// before any loss). Deletion removes the entry. Persist even an
	// empty map to distinguish tracked history from legacy state with unknown loss.
	EnvErrorCutoffs map[string]time.Time `json:"env_error_cutoffs"`
	// Legacy state cannot prove exclusion coverage before its first tracked load.
	EnvErrorLegacyBefore time.Time `json:"env_error_legacy_before,omitzero"`
	// ForeignVMs: VMs whose disk came from a share, an import, or such a VM.
	ForeignVMs map[string]bool `json:"foreign_vms"`
	// RouteGens: per VM, bumped to revoke its port tokens (see SignRoute).
	RouteGens map[string]int `json:"route_gens"`
	// PortMinters: per VM, the API keys that minted its private port links;
	// deleting one (or removing its member) bumps the VM's RouteGens.
	PortMinters    map[string][]string `json:"port_minters,omitempty"`
	PortMinterGens map[string][]string `json:"port_minter_gens,omitempty"`
	// DesktopVMs: VMs with a minted desktop link; the gateway approves a TLS
	// certificate for <vmid>.<desktop domain> only for these.
	DesktopVMs map[string]bool `json:"desktop_vms,omitempty"`
	// boat.dev operations kept across restarts (compat_stops.go, compat.go,
	// compat_idem.go). Op maps keep their team (and VM) under "_" keys.
	StopOps     map[string]map[string]any `json:"stop_ops,omitempty"`     // stop id -> op
	StopLast    map[string]string         `json:"stop_last,omitempty"`    // vm id -> its last stop id
	DeletionOps map[string]map[string]any `json:"deletion_ops,omitempty"` // op id -> op
	Idempotency map[string]*IdemRecord    `json:"idempotency,omitempty"`  // team + "\x00" + key -> answer
	idemIdx     *idemIndex                // in-memory reverse index of Idempotency (compat_idem.go); not stored
	// VMTokens: per VM, the generation of its own API token (selftoken.go).
	VMTokens map[string]*VMToken `json:"vm_tokens"`
	// CompatIDs: boat.dev bx_ sandbox ids (compat_ids.go), and the reverse.
	CompatIDs  map[string]*CompatID `json:"compat_ids"`
	CompatByVM map[string]string    `json:"compat_by_vm"`
	// CompatSetups: boat.dev create files/setupScript status per VM (compat_setup.go).
	CompatSetups map[string]*CompatSetup `json:"compat_setups,omitempty"`
	// CommandSeq: the last ProcessID given out per VM.
	CommandSeq map[string]int `json:"command_seq"`

	// One-time generation upgrade; startup still confirms durability every run.
	CopyCredentialsV1 bool `json:"copy_credentials_v1,omitempty"`

	// Seq is a persisted, monotonic counter for ordering events that are recorded in
	// different maps (for example a no_env mark against a stop): take a number with
	// nextSeq inside a Store.Update/Apply callback and store it next to the event.
	// It only grows, so two numbers taken in any order compare in that order, also
	// across restarts. It is separate from the unexported seq below, which counts
	// state versions for the disk flusher.
	Seq uint64 `json:"seq,omitempty"`

	seq         uint64 // in memory: bumped per change, so the flusher never writes an older state over a newer one
	sshPubOnce  sync.Once
	sshPubIndex map[string]map[string]*SSHGrant
}

func (st *State) sshGrantsByPub() map[string]map[string]*SSHGrant {
	if st == nil {
		return nil
	}
	st.sshPubOnce.Do(func() {
		idx := map[string]map[string]*SSHGrant{}
		for k, g := range st.SSHGrants {
			if g == nil || len(g.VMs) == 0 || g.KeyID == "" || strings.HasPrefix(g.KeyID, pendingSSHKeyPrefix) {
				continue
			}
			byKey := idx[g.PublicKey]
			if byKey == nil {
				byKey = map[string]*SSHGrant{}
				idx[g.PublicKey] = byKey
			}
			byKey[k] = g
		}
		st.sshPubIndex = idx
	})
	return st.sshPubIndex
}

// CommandRecord remembers a detached command after its VM stops or forgets it.
type CommandRecord struct {
	ID         string     `json:"id"`
	Cmd        []string   `json:"cmd"`
	Cwd        string     `json:"cwd,omitempty"`
	Kind       string     `json:"kind"` // command | prompt | setup
	KeyID      string     `json:"key_id,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	State      string     `json:"state"`
	Exit       *int       `json:"exit"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ProcessID  int        `json:"process_id,omitempty"` // boat.dev's integer id of a detached command (per VM)
}

type PromptRun struct {
	ID             string     `json:"id"`
	VMID           string     `json:"vm_id"`
	ConversationID string     `json:"conversation_id"`
	Agent          string     `json:"agent"`
	Model          string     `json:"model,omitempty"`
	Prompt         string     `json:"prompt"`
	CmdID          string     `json:"cmd_id"`
	State          string     `json:"state"` // running | completed | failed | interrupted | steered
	Exit           *int       `json:"exit"`
	Created        time.Time  `json:"created"`
	Finished       *time.Time `json:"finished,omitempty"`
	SteeredBy      string     `json:"steered_by,omitempty"`
}

type Conversation struct {
	ID          string    `json:"id"`
	VMID        string    `json:"vm_id"`
	Agent       string    `json:"agent"`
	Model       string    `json:"model,omitempty"`
	Session     string    `json:"session,omitempty"` // the agent CLI's own session id
	PromptCount int       `json:"prompt_count"`
	LastPrompt  string    `json:"last_prompt"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

// Environment = base image + setup script + env vars + secret selection.
type Environment struct {
	ID          string            `json:"id"`
	TeamID      string            `json:"team_id"`
	Name        string            `json:"name"`
	Image       string            `json:"image,omitempty"`
	Size        string            `json:"size,omitempty"`
	SetupScript string            `json:"setup_script,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Secrets     *[]string         `json:"secrets,omitempty"`
	IsDefault   bool              `json:"is_default"`
	Version     int               `json:"version"` // bumped when image/script/env change
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
	Build       *EnvBuild         `json:"build,omitempty"`
}

// EnvPin is the environment version a VM runs on and the vars of that version.
type EnvPin struct {
	Version int               `json:"version"`
	Vars    map[string]string `json:"vars,omitempty"`
}

// pinOf is the pin of env as it is now (nil for no environment).
func pinOf(env *Environment) *EnvPin {
	if env == nil {
		return nil
	}
	return &EnvPin{Version: env.Version, Vars: maps.Clone(env.Env)}
}

// EnvBuild: a setup run saved as a snapshot, used for fast start. The builder
// VM stays stopped because deleting a VM deletes its snapshots.
type EnvBuild struct {
	State      string    `json:"state"` // building | ready | failed
	Version    int       `json:"version"`
	VMID       string    `json:"vm_id,omitempty"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

// SnapShare grants another team (Grantee) or the holder of a code read access.
type SnapShare struct {
	ID         string     `json:"id"`
	TeamID     string     `json:"team_id"`
	VMID       string     `json:"vm_id"`
	SnapshotID string     `json:"snapshot_id"`
	Grantee    string     `json:"grantee,omitempty"`
	CodeSalt   string     `json:"code_salt,omitempty"`
	CodeHash   string     `json:"code_hash,omitempty"`
	Expires    *time.Time `json:"expires,omitempty"`
	Uses       int        `json:"uses"`
	Created    time.Time  `json:"created"`
	// KeyID minted a code link: the link dies with it (keyAlive). Empty on
	// links from before this field; those keep their expiry (at most 30 days).
	KeyID          string `json:"key_id,omitempty"`
	MintedByKeyGen int64  `json:"minted_by_key_gen,omitempty"`
}

type AICredential struct {
	TeamID   string    `json:"team_id"`
	Provider string    `json:"provider"`
	Sealed   string    `json:"sealed"`
	Last4    string    `json:"last4"`
	Updated  time.Time `json:"updated"`
}

// AIVM is a VM's proxy identity. Gen changes at every start and stop, which
// invalidates the previous per-VM token.
type AIVM struct {
	TeamID  string `json:"team_id"`
	Gen     int    `json:"gen"`
	Blocked bool   `json:"blocked,omitempty"`
	Retired bool   `json:"retired,omitempty"`
}

type AIUsageRow struct {
	TeamID       string `json:"team_id"`
	VMID         string `json:"vm_id"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	// Unmetered counts the requests (of Requests) whose successful answer showed no
	// usage object, so they were counted with 0 tokens (see aiShouldMeter).
	Unmetered int64     `json:"unmetered"`
	Updated   time.Time `json:"updated"`
	// Day (UTC, YYYY-MM-DD) is set on the rows of State.AIUsageDays only.
	Day string `json:"day,omitempty"`
}

// TeamTelemetry is a team's own OTLP endpoint for VM lifecycle events.
type TeamTelemetry struct {
	Endpoint      string    `json:"endpoint"`
	SealedHeaders string    `json:"sealed_headers,omitempty"`
	HeaderNames   []string  `json:"header_names,omitempty"`
	Updated       time.Time `json:"updated"`
}

// SSHGrant records which team VMs a public key was registered for, so the
// jump host's authorized_keys can permit exactly those VM ips.
type SSHGrant struct {
	TeamID     string   `json:"team_id"`
	PublicKey  string   `json:"public_key"` // "type base64", comment stripped
	VMs        []string `json:"vms"`
	CleanupVMs []string `json:"cleanup_vms,omitempty"`
	// KeyID/UserID minted the grant; empty = from before grants were owned.
	KeyID           string    `json:"key_id,omitempty"`
	CreatedByKeyGen int64     `json:"created_by_key_gen,omitempty"`
	UserID          string    `json:"user_id,omitempty"`
	Created         time.Time `json:"created,omitzero"`
}

func secretKey(team, name string) string { return team + "/" + name }

func (st *State) init() {
	if st.VMTokens == nil {
		st.VMTokens = map[string]*VMToken{}
	}
	if st.CompatIDs == nil {
		st.CompatIDs = map[string]*CompatID{}
	}
	if st.CompatByVM == nil {
		st.CompatByVM = map[string]string{}
	}
	if st.CommandSeq == nil {
		st.CommandSeq = map[string]int{}
	}
	if st.Users == nil {
		st.Users = map[string]*User{}
	}
	if st.Teams == nil {
		st.Teams = map[string]*Team{}
	}
	if st.Keys == nil {
		st.Keys = map[string]*APIKey{}
	}
	if st.Secrets == nil {
		st.Secrets = map[string]*Secret{}
	}
	if st.Webhooks == nil {
		st.Webhooks = map[string]*Webhook{}
	}
	if st.Deliveries == nil {
		st.Deliveries = map[string][]Delivery{}
	}
	if st.SSHGrants == nil {
		st.SSHGrants = map[string]*SSHGrant{}
	}
	if st.VMSecrets == nil {
		st.VMSecrets = map[string][]string{}
	}
	if st.Commands == nil {
		st.Commands = map[string][]CommandRecord{}
	}
	if st.Prompts == nil {
		st.Prompts = map[string][]*PromptRun{}
	}
	if st.Conversations == nil {
		st.Conversations = map[string][]*Conversation{}
	}
	if st.Environments == nil {
		st.Environments = map[string]*Environment{}
	}
	if st.VMEnvs == nil {
		st.VMEnvs = map[string]string{}
	}
	if st.VMExtraEnv == nil {
		st.VMExtraEnv = map[string]map[string]string{}
	}
	if st.VMEnvPins == nil {
		st.VMEnvPins = map[string]*EnvPin{}
	}
	if st.VMTransitions == nil {
		st.VMTransitions = map[string]*CredentialTransition{}
	}
	if st.VMNames == nil {
		st.VMNames = map[string]string{}
	}
	if st.VMEnvErrors == nil {
		st.VMEnvErrors = map[string]string{}
	}
	if st.NoEnvRuns == nil {
		st.NoEnvRuns = map[string]time.Time{}
	}
	if st.SnapShares == nil {
		st.SnapShares = map[string]*SnapShare{}
	}
	if st.AICreds == nil {
		st.AICreds = map[string]*AICredential{}
	}
	if st.AIVMs == nil {
		st.AIVMs = map[string]*AIVM{}
	}
	if st.AIProxyOff == nil {
		st.AIProxyOff = map[string]bool{}
	}
	if st.AIUsage == nil {
		st.AIUsage = map[string]*AIUsageRow{}
	}
	if st.AIUsageDays == nil {
		st.AIUsageDays = map[string]*AIUsageRow{}
	}
	if st.EnvErrorSpans == nil {
		st.EnvErrorSpans = map[string][]EnvErrorSpan{}
	}
	if st.EnvErrorCutoffs == nil {
		st.EnvErrorCutoffs = map[string]time.Time{}
	}
	if st.Telemetry == nil {
		st.Telemetry = map[string]*TeamTelemetry{}
	}
	if st.ForeignVMs == nil {
		st.ForeignVMs = map[string]bool{}
	}
	if st.RouteGens == nil {
		st.RouteGens = map[string]int{}
	}
	if st.PortMinters == nil {
		st.PortMinters = map[string][]string{}
	}
	if st.PortMinterGens == nil {
		st.PortMinterGens = map[string][]string{}
	}
	if st.DesktopVMs == nil {
		st.DesktopVMs = map[string]bool{}
	}
	if st.StopOps == nil {
		st.StopOps = map[string]map[string]any{}
	}
	if st.StopLast == nil {
		st.StopLast = map[string]string{}
	}
	if st.DeletionOps == nil {
		st.DeletionOps = map[string]map[string]any{}
	}
	if st.Idempotency == nil {
		st.Idempotency = map[string]*IdemRecord{}
	}
}

// Store hands out immutable snapshots: Update builds a fresh copy, persists
// it, then swaps it in, so a snapshot obtained earlier is never mutated.
type Store struct {
	mu   sync.Mutex // writers only
	path string
	cur  atomic.Pointer[State]
	kr   *Keyring // nil = plaintext state.json (tests)

	credentialPushes sync.Map // VM id -> *sync.Mutex, same lifetime as its token records

	wmu        sync.Mutex    // disk writes
	written    uint64        // seq of the state on disk; under wmu
	writtenSeq atomic.Uint64 // lock-free mirror of written
	writes     atomic.Uint64
	writing    atomic.Bool // true while wmu is held for writeAtomic
	closed     bool        // under wmu: Close ran, nothing is written any more
	closedFlag atomic.Bool // lock-free mirror of closed
	kick       chan struct{}

	lazyPending atomic.Bool // a delayed Kick is scheduled (ApplyLazy)
}

// sealedPrefix marks a state.json sealed under the master key.
const (
	sealedPrefix = "boatseal1:"
	stateAAD     = "boat-api state v1"
)

// OpenStore keeps state.json in plaintext.
func OpenStore(dir string) (*Store, error) { return openStore(dir, nil) }

// OpenSealedStore seals state.json: it holds env vars, setup scripts, argv and
// prompts. A plaintext file from an older version is sealed on open.
func OpenSealedStore(dir string, kr *Keyring) (*Store, error) { return openStore(dir, kr) }

func openStore(dir string, kr *Keyring) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json"), kr: kr, kick: make(chan struct{}, 1)}
	st := &State{}
	b, err := os.ReadFile(s.path)
	plain := false
	migratedEnvSpans := false
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		plain = !strings.HasPrefix(string(b), sealedPrefix)
		if b, err = s.decode(b); err != nil {
			return nil, fmt.Errorf("open %s: %w", s.path, err)
		}
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
		if st.EnvErrorCutoffs == nil {
			st.EnvErrorLegacyBefore = time.Now().UTC()
			migratedEnvSpans = true
		}
	}
	st.init()
	migrateOperator(st)
	migrateAIUsageDays(st)
	s.cur.Store(st)
	if migratedEnvSpans || plain && kr != nil {
		if err := s.writeState(st); err != nil { // seq 0, so persist would skip it
			return nil, fmt.Errorf("initialize %s: %w", s.path, err)
		}
	}
	go s.flusher()
	return s, nil
}

func (s *Store) decode(b []byte) ([]byte, error) {
	sealed, ok := strings.CutPrefix(string(b), sealedPrefix)
	if !ok {
		return b, nil
	}
	if s.kr == nil {
		return nil, errors.New("state is sealed and no key was given")
	}
	return s.kr.Open(sealed, stateAAD)
}

var (
	errStoreClosed = errors.New("store is closed")
	errNoChange    = errors.New("no state change")
)

// encode serializes and optionally seals an immutable State snapshot.
// Safe to call without holding s.mu or s.wmu.
func (s *Store) encode(st *State) ([]byte, error) {
	out, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if s.kr != nil {
		sealed, err := s.kr.Seal(out, stateAAD)
		if err != nil {
			return nil, err
		}
		out = []byte(sealedPrefix + sealed)
	}
	return out, nil
}

// persist writes st unless a newer state is on disk already.
// Serialization and AEAD sealing happen outside s.wmu so disk-write waits
// do not stall on CPU work, and already-persisted states return lock-free.
func (s *Store) persist(st *State) error {
	if s.closedFlag.Load() {
		return errStoreClosed
	}
	if st.seq <= s.writtenSeq.Load() {
		return nil
	}
	out, err := s.encode(st)
	if err != nil {
		return err
	}
	return s.persistEncoded(st.seq, out)
}

func (s *Store) persistEncoded(seq uint64, out []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if seq <= s.written {
		return nil
	}
	return s.writeEncodedLocked(seq, out)
}

// A state is written only when it is newer than the one on disk. A state
// loaded from disk and never changed (seq 0) is never written back: another
// process (boat-api admin init) may have replaced the file since, and a
// shutdown Close must not put the old contents back over it.
func (s *Store) persistLocked(st *State) error {
	if st.seq <= s.written {
		return nil
	}
	return s.writeState(st)
}

// writeState writes st unconditionally. Caller holds s.wmu, or owns s alone (open).
func (s *Store) writeState(st *State) error {
	out, err := s.encode(st)
	if err != nil {
		return err
	}
	return s.writeEncodedLocked(st.seq, out)
}

// writeEncodedLocked writes already-encoded state bytes. Caller holds s.wmu, or owns s alone (open).
func (s *Store) writeEncodedLocked(seq uint64, out []byte) error {
	s.writing.Store(true)
	err := writeAtomic(s.path, out)
	s.writing.Store(false)
	if err != nil {
		return err
	}
	s.writes.Add(1)
	s.written = max(s.written, seq)
	s.writtenSeq.Store(s.written)
	return nil
}

// migrateOperator handles state written before the operator role: the oldest
// user is the one admin init created, so it becomes the operator. In memory
// only; it is recomputed on each load until an Update persists it.
func migrateOperator(st *State) {
	var oldest *User
	for _, u := range st.Users {
		if u.Operator {
			return
		}
		if oldest == nil || u.Created.Before(oldest.Created) {
			oldest = u
		}
	}
	if oldest != nil {
		log.Printf("boat-api: no operator in state; promoting the oldest user %s (%s)", oldest.ID, oldest.Name)
		oldest.Operator = true
	}
}

// nextSeq returns the next value of State.Seq. Call it only on the copy a
// Store.Update/Apply callback receives (never on a Snapshot).
func (st *State) nextSeq() uint64 {
	st.Seq++
	return st.Seq
}

// Snapshot returns the current state. Callers must treat it as read-only.
func (s *Store) Snapshot() *State { return s.cur.Load() }

// Update applies fn to a deep copy and waits until it is on disk; on error or
// failed write nothing changes.
func (s *Store) Update(fn func(*State) error) error {
	// If a background flush is mid-fsync, wait for it before taking s.mu so
	// concurrent Apply/ApplyLazy/Stage calls do not stall behind flusher I/O.
	if s.writing.Load() {
		s.wmu.Lock()
		s.wmu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.next(fn)
	if err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.cur.Store(next)
	return nil
}

// Apply applies fn to a deep copy and returns without waiting for the disk:
// the flusher writes it shortly after. A crash before that loses the change,
// so use it only for state that is rebuilt, re-derived or harmless to lose.
func (s *Store) Apply(fn func(*State) error) error {
	s.mu.Lock()
	next, err := s.next(fn)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.cur.Store(next)
	s.mu.Unlock()
	s.Kick()
	return nil
}

// ApplyLazy is Apply with a delayed, coalesced disk write: fn is swapped into
// memory at once (readers see it immediately), and the flusher is woken only
// after delay, so any number of ApplyLazy calls inside the window share one
// write. A crash inside the window loses those changes; use it only for state
// whose loss is tolerable (idempotency records: a retry re-runs the operation).
// Any Update/Flush in between persists the lazy change too, since it writes
// the whole current state.
func (s *Store) ApplyLazy(fn func(*State) error, delay time.Duration) error {
	s.mu.Lock()
	next, err := s.next(fn)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.cur.Store(next)
	s.mu.Unlock()
	if s.lazyPending.CompareAndSwap(false, true) {
		time.AfterFunc(delay, func() {
			s.lazyPending.Store(false)
			s.Kick()
		})
	}
	return nil
}

// Stage applies fn to a deep copy and swaps it into memory without waking the
// background flusher; callers must follow it immediately with Update (or Kick)
// so a throwaway disk write does not race ahead of the final Update.
func (s *Store) Stage(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.next(fn)
	if err != nil {
		return err
	}
	s.cur.Store(next)
	return nil
}

// Kick wakes the background flusher if there is unwritten state.
func (s *Store) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// UpdateOrApply decides atomically under s.mu whether the mutation requires a
// synchronous disk barrier (sync == true) or can be flushed asynchronously.
func (s *Store) UpdateOrApply(fn func(*State) (sync bool, err error)) error {
	s.mu.Lock()
	syncWrite := false
	next, err := s.next(func(st *State) error {
		var ferr error
		syncWrite, ferr = fn(st)
		return ferr
	})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if syncWrite {
		err = s.persist(next)
		if err == nil {
			s.cur.Store(next)
		}
		s.mu.Unlock()
		return err
	}
	s.cur.Store(next)
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return nil
}

func cloneTimePtr(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneStringMapsMap(m map[string]map[string]string) map[string]map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]map[string]string, len(m))
	for k, v := range m {
		out[k] = maps.Clone(v)
	}
	return out
}

func cloneStringSlicesMap(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

func cloneAnyVal(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return cloneAnyMap(x)
	case map[string]string:
		return maps.Clone(x)
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = cloneAnyVal(elem)
		}
		return out
	case []string:
		return slices.Clone(x)
	case []byte:
		return slices.Clone(x)
	case *time.Time:
		return cloneTimePtr(x)
	case *int:
		return cloneIntPtr(x)
	case *string:
		if x == nil {
			return (*string)(nil)
		}
		cp := *x
		return &cp
	case *bool:
		if x == nil {
			return (*bool)(nil)
		}
		cp := *x
		return &cp
	default:
		return v
	}
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAnyVal(v)
	}
	return out
}

func cloneAnyMapOfMaps(m map[string]map[string]any) map[string]map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAnyMap(v)
	}
	return out
}

// cowVMToken returns a writable copy of st.VMTokens[id] installed back into
// st.VMTokens, or nil if absent. Store write callbacks that mutate an existing
// VMToken must go through this so published snapshots can safely share
// immutable *VMToken pointers.
func (st *State) cowVMToken(id string) *VMToken {
	t := st.VMTokens[id]
	if t == nil {
		return nil
	}
	cp := *t
	st.VMTokens[id] = &cp
	return &cp
}

// cowAIVM returns a writable copy of st.AIVMs[id] installed back into st.AIVMs,
// or nil if absent. Store write callbacks that mutate an existing AIVM must go
// through this so published snapshots can safely share immutable *AIVM pointers.
func (st *State) cowAIVM(id string) *AIVM {
	a := st.AIVMs[id]
	if a == nil {
		return nil
	}
	cp := *a
	st.AIVMs[id] = &cp
	return &cp
}

// clone deep-copies every field, map, slice, and pointer of State in memory.
func (cur *State) clone() *State { return cur.cloneMode(true) }

// cloneForWrite builds the next State for Store mutations. High-cardinality
// maps whose entries are immutable or copy-on-write (Deliveries, Commands,
// AIUsage, AIUsageDays, StopOps, DeletionOps, Idempotency, VMTokens, AIVMs,
// CompatIDs) shallow-copy the map header and share untouched entries across
// snapshots.
func (cur *State) cloneForWrite() *State { return cur.cloneMode(false) }

func (cur *State) cloneMode(deep bool) *State {
	if cur == nil {
		st := &State{}
		st.init()
		return st
	}
	next := &State{
		Users:                make(map[string]*User, len(cur.Users)),
		Teams:                make(map[string]*Team, len(cur.Teams)),
		Keys:                 make(map[string]*APIKey, len(cur.Keys)),
		Secrets:              make(map[string]*Secret, len(cur.Secrets)),
		Webhooks:             make(map[string]*Webhook, len(cur.Webhooks)),
		SSHGrants:            make(map[string]*SSHGrant, len(cur.SSHGrants)),
		VMSecrets:            cloneStringSlicesMap(cur.VMSecrets),
		Prompts:              make(map[string][]*PromptRun, len(cur.Prompts)),
		Conversations:        make(map[string][]*Conversation, len(cur.Conversations)),
		Environments:         make(map[string]*Environment, len(cur.Environments)),
		VMEnvs:               maps.Clone(cur.VMEnvs),
		VMEnvPins:            make(map[string]*EnvPin, len(cur.VMEnvPins)),
		VMTransitions:        make(map[string]*CredentialTransition, len(cur.VMTransitions)),
		VMNames:              maps.Clone(cur.VMNames),
		VMExtraEnv:           cloneStringMapsMap(cur.VMExtraEnv),
		VMEnvErrors:          maps.Clone(cur.VMEnvErrors),
		NoEnvRuns:            maps.Clone(cur.NoEnvRuns),
		SnapShares:           make(map[string]*SnapShare, len(cur.SnapShares)),
		AICreds:              make(map[string]*AICredential, len(cur.AICreds)),
		AIProxyOff:           maps.Clone(cur.AIProxyOff),
		AIUsageDaysV1:        cur.AIUsageDaysV1,
		aiIdx:                cur.sharedAIUsageIndex(),
		Seq:                  cur.Seq,
		EnvErrorSpans:        make(map[string][]EnvErrorSpan, len(cur.EnvErrorSpans)),
		EnvErrorCutoffs:      maps.Clone(cur.EnvErrorCutoffs),
		EnvErrorLegacyBefore: cur.EnvErrorLegacyBefore,
		Telemetry:            make(map[string]*TeamTelemetry, len(cur.Telemetry)),
		ForeignVMs:           maps.Clone(cur.ForeignVMs),
		RouteGens:            maps.Clone(cur.RouteGens),
		PortMinters:          cloneStringSlicesMap(cur.PortMinters),
		PortMinterGens:       cloneStringSlicesMap(cur.PortMinterGens),
		DesktopVMs:           maps.Clone(cur.DesktopVMs),
		StopLast:             maps.Clone(cur.StopLast),
		CompatByVM:           maps.Clone(cur.CompatByVM),
		CompatSetups:         make(map[string]*CompatSetup, len(cur.CompatSetups)),
		CommandSeq:           maps.Clone(cur.CommandSeq),
		CopyCredentialsV1:    cur.CopyCredentialsV1,
		seq:                  cur.seq,
	}
	if deep {
		next.Deliveries = make(map[string][]Delivery, len(cur.Deliveries))
		for k, ds := range cur.Deliveries {
			next.Deliveries[k] = slices.Clone(ds)
		}
		next.Commands = make(map[string][]CommandRecord, len(cur.Commands))
		for k, cmds := range cur.Commands {
			if cmds == nil {
				next.Commands[k] = nil
				continue
			}
			cpSlice := make([]CommandRecord, len(cmds))
			for i, c := range cmds {
				c.Cmd = slices.Clone(c.Cmd)
				c.Exit = cloneIntPtr(c.Exit)
				c.FinishedAt = cloneTimePtr(c.FinishedAt)
				cpSlice[i] = c
			}
			next.Commands[k] = cpSlice
		}
		next.AIUsage = make(map[string]*AIUsageRow, len(cur.AIUsage))
		for k, r := range cur.AIUsage {
			if r == nil {
				next.AIUsage[k] = nil
				continue
			}
			cp := *r
			next.AIUsage[k] = &cp
		}
		next.AIUsageDays = make(map[string]*AIUsageRow, len(cur.AIUsageDays))
		for k, r := range cur.AIUsageDays {
			if r == nil {
				next.AIUsageDays[k] = nil
				continue
			}
			cp := *r
			next.AIUsageDays[k] = &cp
		}
		next.StopOps = cloneAnyMapOfMaps(cur.StopOps)
		next.DeletionOps = cloneAnyMapOfMaps(cur.DeletionOps)
		next.Idempotency = make(map[string]*IdemRecord, len(cur.Idempotency))
		for k, rec := range cur.Idempotency {
			if rec == nil {
				next.Idempotency[k] = nil
				continue
			}
			cp := *rec
			cp.Body = slices.Clone(rec.Body)
			cp.VMIDs = slices.Clone(rec.VMIDs)
			cp.RouteGens = maps.Clone(rec.RouteGens)
			next.Idempotency[k] = &cp
		}
		next.VMTokens = make(map[string]*VMToken, len(cur.VMTokens))
		for k, vt := range cur.VMTokens {
			if vt == nil {
				next.VMTokens[k] = nil
				continue
			}
			cp := *vt
			next.VMTokens[k] = &cp
		}
		next.AIVMs = make(map[string]*AIVM, len(cur.AIVMs))
		for k, a := range cur.AIVMs {
			if a == nil {
				next.AIVMs[k] = nil
				continue
			}
			cp := *a
			next.AIVMs[k] = &cp
		}
		next.CompatIDs = make(map[string]*CompatID, len(cur.CompatIDs))
		for k, cid := range cur.CompatIDs {
			if cid == nil {
				next.CompatIDs[k] = nil
				continue
			}
			cp := *cid
			next.CompatIDs[k] = &cp
		}
	} else {
		next.Deliveries = maps.Clone(cur.Deliveries)
		next.Commands = maps.Clone(cur.Commands)
		next.AIUsage = maps.Clone(cur.AIUsage)
		next.AIUsageDays = maps.Clone(cur.AIUsageDays)
		next.StopOps = maps.Clone(cur.StopOps)
		next.DeletionOps = maps.Clone(cur.DeletionOps)
		next.Idempotency = maps.Clone(cur.Idempotency)
		next.idemIdx = cur.idemIdx
		next.VMTokens = maps.Clone(cur.VMTokens)
		next.AIVMs = maps.Clone(cur.AIVMs)
		next.CompatIDs = maps.Clone(cur.CompatIDs)
	}
	for k, u := range cur.Users {
		if u == nil {
			next.Users[k] = nil
			continue
		}
		cp := *u
		next.Users[k] = &cp
	}
	for k, t := range cur.Teams {
		if t == nil {
			next.Teams[k] = nil
			continue
		}
		cp := *t
		cp.Members = slices.Clone(t.Members)
		cp.AcceptSharesFrom = slices.Clone(t.AcceptSharesFrom)
		next.Teams[k] = &cp
	}
	for k, ak := range cur.Keys {
		if ak == nil {
			next.Keys[k] = nil
			continue
		}
		cp := *ak
		cp.Scopes = slices.Clone(ak.Scopes)
		cp.PrevUntil = cloneTimePtr(ak.PrevUntil)
		cp.Rotated = cloneTimePtr(ak.Rotated)
		cp.Expires = cloneTimePtr(ak.Expires)
		next.Keys[k] = &cp
	}
	for k, sec := range cur.Secrets {
		if sec == nil {
			next.Secrets[k] = nil
			continue
		}
		cp := *sec
		next.Secrets[k] = &cp
	}
	for k, wh := range cur.Webhooks {
		if wh == nil {
			next.Webhooks[k] = nil
			continue
		}
		cp := *wh
		cp.Events = slices.Clone(wh.Events)
		cp.PrevUntil = cloneTimePtr(wh.PrevUntil)
		next.Webhooks[k] = &cp
	}
	for k, g := range cur.SSHGrants {
		if g == nil {
			next.SSHGrants[k] = nil
			continue
		}
		cp := *g
		cp.VMs = slices.Clone(g.VMs)
		cp.CleanupVMs = slices.Clone(g.CleanupVMs)
		next.SSHGrants[k] = &cp
	}
	for k, prs := range cur.Prompts {
		if prs == nil {
			next.Prompts[k] = nil
			continue
		}
		cpSlice := make([]*PromptRun, len(prs))
		for i, pr := range prs {
			if pr != nil {
				cp := *pr
				cp.Exit = cloneIntPtr(pr.Exit)
				cp.Finished = cloneTimePtr(pr.Finished)
				cpSlice[i] = &cp
			}
		}
		next.Prompts[k] = cpSlice
	}
	for k, convs := range cur.Conversations {
		if convs == nil {
			next.Conversations[k] = nil
			continue
		}
		cpSlice := make([]*Conversation, len(convs))
		for i, c := range convs {
			if c != nil {
				cp := *c
				cpSlice[i] = &cp
			}
		}
		next.Conversations[k] = cpSlice
	}
	for k, t := range cur.VMTransitions {
		next.VMTransitions[k] = cloneTransition(t)
	}
	for k, e := range cur.Environments {
		if e == nil {
			next.Environments[k] = nil
			continue
		}
		cp := *e
		cp.Env = maps.Clone(e.Env)
		if e.Secrets != nil {
			s := slices.Clone(*e.Secrets)
			cp.Secrets = &s
		}
		if e.Build != nil {
			b := *e.Build
			cp.Build = &b
		}
		next.Environments[k] = &cp
	}
	for k, pin := range cur.VMEnvPins {
		if pin == nil {
			next.VMEnvPins[k] = nil
			continue
		}
		next.VMEnvPins[k] = &EnvPin{Version: pin.Version, Vars: maps.Clone(pin.Vars)}
	}
	for k, sh := range cur.SnapShares {
		if sh == nil {
			next.SnapShares[k] = nil
			continue
		}
		cp := *sh
		cp.Expires = cloneTimePtr(sh.Expires)
		next.SnapShares[k] = &cp
	}
	for k, c := range cur.AICreds {
		if c == nil {
			next.AICreds[k] = nil
			continue
		}
		cp := *c
		next.AICreds[k] = &cp
	}
	for k, sp := range cur.EnvErrorSpans {
		next.EnvErrorSpans[k] = slices.Clone(sp)
	}
	for k, tt := range cur.Telemetry {
		if tt == nil {
			next.Telemetry[k] = nil
			continue
		}
		cp := *tt
		cp.HeaderNames = slices.Clone(tt.HeaderNames)
		next.Telemetry[k] = &cp
	}
	for k, cs := range cur.CompatSetups {
		if cs == nil {
			next.CompatSetups[k] = nil
			continue
		}
		cp := *cs
		next.CompatSetups[k] = &cp
	}
	next.init()
	return next
}

// next clones the current state and applies fn. Caller holds s.mu.
func (s *Store) next(fn func(*State) error) (*State, error) {
	cur := s.cur.Load()
	next := cur.cloneForWrite()
	next.seq = cur.seq + 1
	if err := fn(next); err != nil {
		return nil, err
	}
	return next, nil
}

// flusher writes what Apply left in memory, retrying failed writes.
func (s *Store) flusher() {
	for range s.kick {
		for {
			err := s.Flush()
			if err == nil {
				break
			}
			if errors.Is(err, errStoreClosed) {
				break
			}
			if _, serr := os.Stat(filepath.Dir(s.path)); errors.Is(serr, os.ErrNotExist) {
				log.Printf("boat-api: write %s: %v; the state directory is gone, not retrying", s.path, err)
				break
			}
			log.Printf("boat-api: write %s: %v; retrying", s.path, err)
			time.Sleep(time.Second)
		}
	}
}

// Flush writes the current state if it is newer than the one on disk.
func (s *Store) Flush() error { return s.persist(s.cur.Load()) }

// Close writes what Apply left in memory; after it nothing is written (shutdown, tests).
func (s *Store) Close() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.closedFlag.Store(true)
	return s.persistLocked(s.cur.Load())
}

// syncDelay makes every fsync slower (the latency test).
var syncDelay atomic.Int64

func syncFile(f *os.File) error {
	if d := syncDelay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	return f.Sync()
}

func writeAtomic(path string, data []byte) error { return writeAtomicMode(path, data, 0o600) }

func writeAtomicMode(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := syncFile(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncFile(d)
}
