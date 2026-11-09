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
}

type Secret struct {
	TeamID  string    `json:"team_id"`
	Name    string    `json:"name"`
	Sealed  string    `json:"sealed"`
	Updated time.Time `json:"updated"`
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
	SSHGrants  map[string]*SSHGrant  `json:"ssh_grants"` // key: team_id + "|" + "type base64"
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
}

func secretKey(team, name string) string { return team + "/" + name }

func (st *State) init() {
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
		if err := s.persist(st); err != nil {
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

// persist writes st unless a newer state is on disk already.
func (s *Store) persist(st *State) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if st.seq != 0 && st.seq <= s.written {
		return nil
	}
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

// syncFile is f.Sync; the latency test makes it slow.
var syncFile = func(f *os.File) error { return f.Sync() }

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
