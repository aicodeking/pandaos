package main

import (
	"errors"
	"io/fs"
	"log"
	"path/filepath"
	"strconv"
	"time"
)

// A VM's index is its IP, MAC, tap and jail uid. boat-api's ssh jump grants
// (permitopen="<ip>:22") and other consumers of boatd's VM list catch up with a
// delete asynchronously, so an index freed now is quarantined for
// indexQuarantineS before it is handed out again. Quarantine never costs
// capacity: when every free index is quarantined, the one freed longest ago is taken.

const freedFile = "freed.json" // index -> end of its quarantine (RFC 3339), survives restarts

// loadFreed reads the quarantine left by the previous boatd, dropping what expired.
func (m *Manager) loadFreed() {
	m.freed = map[int]time.Time{}
	var raw map[string]time.Time
	if err := readJSON(filepath.Join(m.cfg.StateDir, freedFile), &raw); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("%s: %v (no address quarantine carried over)", freedFile, err)
		}
		return
	}
	now := m.now()
	for k, until := range raw {
		if i, err := strconv.Atoi(k); err == nil && i >= 2 && i <= maxIndex && until.After(now) {
			m.freed[i] = until
		}
	}
}

// freeIndexLocked quarantines idx. Caller holds m.mu.
func (m *Manager) freeIndexLocked(idx int) {
	if m.cfg.IndexQuarantineS <= 0 || idx < 2 {
		return
	}
	now := m.now()
	next := make(map[int]time.Time, len(m.freed)+1)
	raw := make(map[string]time.Time, len(m.freed)+1)
	for i, until := range m.freed {
		if until.After(now) {
			next[i], raw[strconv.Itoa(i)] = until, until
		}
	}
	until := now.Add(time.Duration(m.cfg.IndexQuarantineS) * time.Second)
	next[idx], raw[strconv.Itoa(idx)] = until, until
	m.freed = next
	// No fsync: a host crash restarts every consumer of the VM list as well.
	if err := writeJSONNoSync(filepath.Join(m.cfg.StateDir, freedFile), raw, 0o600); err != nil {
		log.Printf("%s: %v (quarantine of index %d kept in memory only)", freedFile, err, idx)
	}
}

// pickIndexLocked: the lowest free index out of quarantine, else the free one
// whose quarantine ends first, else 0. Caller holds m.mu.
func (m *Manager) pickIndexLocked(used map[int]bool) int {
	now := m.now()
	fallback, fallbackUntil := 0, time.Time{}
	for i := 2; i <= maxIndex; i++ {
		if used[i] {
			continue
		}
		until, held := m.freed[i]
		if m.cfg.IndexQuarantineS <= 0 || !held || !until.After(now) {
			return i
		}
		if fallback == 0 || until.Before(fallbackUntil) {
			fallback, fallbackUntil = i, until
		}
	}
	return fallback
}
