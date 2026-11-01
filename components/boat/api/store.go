package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ponytail: the whole state lives in one JSON file that is rewritten on every
// change and cloned (marshal/unmarshal) per Update. Fine for a single server
// with thousands of keys/webhooks; past ~10 MB of state or ~50 writes/s move
// to SQLite (deliveries first, they are the hottest writer).

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

}

// Store hands out immutable snapshots: Update builds a fresh copy, persists
// it, then swaps it in, so a snapshot obtained earlier is never mutated.
type Store struct {
	mu   sync.Mutex
	path string
	cur  *State
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json"), cur: &State{}}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, s.cur); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
	}
	s.cur.init()
	return s, nil
}

// Snapshot returns the current state. Callers must treat it as read-only.
func (s *Store) Snapshot() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Update applies fn to a deep copy; on error or failed write nothing changes.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.cur)
	if err != nil {
		return err
	}
	next := &State{}
	if err := json.Unmarshal(b, next); err != nil {
		return err
	}
	next.init()
	if err := fn(next); err != nil {
		return err
	}
	out, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, out); err != nil {
		return err
	}
	s.cur = next
	return nil
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
	if err := f.Sync(); err != nil {
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
		d.Sync()
		d.Close()
	}
	return nil
}
