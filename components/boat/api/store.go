package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	VMEnvs     map[string]string         `json:"vm_envs"`
	SnapShares map[string]*SnapShare     `json:"snap_shares"`
	AICreds    map[string]*AICredential  `json:"ai_creds"` // key: team_id + "/" + provider
	AIVMs      map[string]*AIVM          `json:"ai_vms"`
	AIProxyOff map[string]bool           `json:"ai_proxy_off"` // team id: proxy off, raw keys injected
	AIUsage    map[string]*AIUsageRow    `json:"ai_usage"`     // key: team_id + "/" + vm_id + "/" + provider + "/" + model
	Telemetry  map[string]*TeamTelemetry `json:"telemetry"`
	// ForeignVMs: VMs whose disk came from a share, an import, or such a VM.
	ForeignVMs map[string]bool `json:"foreign_vms"`
	// RouteGens: per VM, bumped to revoke its port tokens (see SignRoute).
	RouteGens map[string]int `json:"route_gens"`
	// PortMinters: per VM, the API keys that minted its private port links;
	// deleting one (or removing its member) bumps the VM's RouteGens.
	PortMinters map[string][]string `json:"port_minters,omitempty"`
	// DesktopVMs: VMs with a minted desktop link; the gateway approves a TLS
	// certificate for <vmid>.<desktop domain> only for these.
	DesktopVMs map[string]bool `json:"desktop_vms,omitempty"`
	// boat.dev operations kept across restarts (compat_stops.go, compat.go,
	// compat_idem.go). Op maps keep their team (and VM) under "_" keys.
	StopOps     map[string]map[string]any `json:"stop_ops,omitempty"`     // stop id -> op
	StopLast    map[string]string         `json:"stop_last,omitempty"`    // vm id -> its last stop id
	DeletionOps map[string]map[string]any `json:"deletion_ops,omitempty"` // op id -> op
	Idempotency map[string]*IdemRecord    `json:"idempotency,omitempty"`  // team + "\x00" + key -> answer
	// VMTokens: per VM, the generation of its own API token (selftoken.go).
	VMTokens map[string]*VMToken `json:"vm_tokens"`
	// CompatIDs: boat.dev bx_ sandbox ids (compat_ids.go), and the reverse.
	CompatIDs  map[string]*CompatID `json:"compat_ids"`
	CompatByVM map[string]string    `json:"compat_by_vm"`
	// CompatSetups: boat.dev create files/setupScript status per VM (compat_setup.go).
	CompatSetups map[string]*CompatSetup `json:"compat_setups,omitempty"`
	// CommandSeq: the last ProcessID given out per VM.
	CommandSeq map[string]int `json:"command_seq"`

	seq uint64 // in memory: bumped per change, so the flusher never writes an older state over a newer one
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
	KeyID string `json:"key_id,omitempty"`
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
}

type AIUsageRow struct {
	TeamID       string    `json:"team_id"`
	VMID         string    `json:"vm_id"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	Requests     int64     `json:"requests"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Updated      time.Time `json:"updated"`
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
	TeamID    string   `json:"team_id"`
	PublicKey string   `json:"public_key"` // "type base64", comment stripped
	VMs       []string `json:"vms"`
	// KeyID/UserID minted the grant; empty = from before grants were owned.
	KeyID   string    `json:"key_id,omitempty"`
	UserID  string    `json:"user_id,omitempty"`
	Created time.Time `json:"created,omitzero"`
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

	wmu     sync.Mutex // disk writes
	written uint64     // seq of the state on disk; under wmu
	closed  bool       // under wmu: Close ran, nothing is written any more
	kick    chan struct{}
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
	}
	st.init()
	migrateOperator(st)
	s.cur.Store(st)
	if plain && kr != nil {
		if err := s.writeState(st); err != nil { // seq 0, so persist would skip it
			return nil, fmt.Errorf("seal %s: %w", s.path, err)
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

var errStoreClosed = errors.New("store is closed")

// persist writes st unless a newer state is on disk already.
func (s *Store) persist(st *State) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	return s.persistLocked(st)
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
	out, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	if s.kr != nil {
		sealed, err := s.kr.Seal(out, stateAAD)
		if err != nil {
			return err
		}
		out = []byte(sealedPrefix + sealed)
	}
	if err := writeAtomic(s.path, out); err != nil {
		return err
	}
	s.written = max(s.written, st.seq)
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

// Snapshot returns the current state. Callers must treat it as read-only.
func (s *Store) Snapshot() *State { return s.cur.Load() }

// Update applies fn to a deep copy and waits until it is on disk; on error or
// failed write nothing changes.
func (s *Store) Update(fn func(*State) error) error {
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
	if err == nil {
		s.cur.Store(next)
	}
	s.mu.Unlock()
	if err == nil {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return err
}

// next clones the current state and applies fn. Caller holds s.mu.
func (s *Store) next(fn func(*State) error) (*State, error) {
	cur := s.cur.Load()
	b, err := json.Marshal(cur)
	if err != nil {
		return nil, err
	}
	next := &State{}
	if err := json.Unmarshal(b, next); err != nil {
		return nil, err
	}
	next.init()
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
	if d, err := os.Open(dir); err == nil {
		syncFile(d)
		d.Close()
	}
	return nil
}
