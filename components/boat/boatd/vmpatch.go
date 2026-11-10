package main

import (
	"net/http"
	"time"
)

// PATCH /v1/vms/{id}: rename a VM, set its auto-stop TTL, or push the current
// auto-stop out. All under m.mu, so it never waits behind a stop's snapshot
// and Start/Stop see the new TTL atomically. See PROTOCOL.md "Rename and auto-stop".

// maxPatchS bounds ttl_s and extend_s: a time.Duration overflows past ~292 years.
const maxPatchS = 10 * 365 * 24 * 3600

type PatchReq struct {
	Name    *string `json:"name"`
	TTLS    *int    `json:"ttl_s"`
	ExtendS *int    `json:"extend_s"`
}

func (r PatchReq) validate() error {
	switch {
	case r.Name == nil && r.TTLS == nil && r.ExtendS == nil:
		return errf(400, "set name, ttl_s or extend_s")
	case r.Name != nil && !reName.MatchString(*r.Name):
		return errf(400, "invalid name (lowercase letters, digits, '-', max 63)")
	case r.TTLS != nil && (*r.TTLS < 0 || *r.TTLS > maxPatchS):
		return errf(400, "ttl_s must be 0 (no auto-stop) to %d", maxPatchS)
	case r.ExtendS != nil && (*r.ExtendS < 1 || *r.ExtendS > maxPatchS):
		return errf(400, "extend_s must be 1 to %d", maxPatchS)
	}
	return nil
}

// Patch applies r to VM id: name, then ttl_s, then extend_s. Nothing changes
// unless every part applies.
func (m *Manager) Patch(id string, r PatchReq) (VM, error) {
	if err := r.validate(); err != nil {
		return VM{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.vms[id]
	if !ok {
		return VM{}, errNotFound
	}
	nv := old.clone()
	now := m.now()
	if r.Name != nil && *r.Name != old.Name {
		if m.nameTakenLocked(old.Team, *r.Name, id) {
			return VM{}, errf(http.StatusConflict, "team %s already has a VM named %s", old.Team, *r.Name)
		}
		nv.Name = *r.Name
	}
	if r.TTLS != nil {
		nv.TTLS = *r.TTLS
		if nv.State == Running {
			nv.ExpiresAt = deadline(now, nv.TTLS)
		}
	}
	if r.ExtendS != nil {
		if nv.State != Running {
			return VM{}, errf(http.StatusConflict, "vm is %s: only a running VM has an auto-stop to extend", nv.State)
		}
		if nv.ExpiresAt == nil {
			return VM{}, errf(400, "vm has no ttl_s: set one with ttl_s")
		}
		base := *nv.ExpiresAt
		if base.Before(now) {
			base = now
		}
		t := base.Add(time.Duration(*r.ExtendS) * time.Second)
		nv.ExpiresAt = &t
	}
	if err := m.commitLocked(old, nv, "patch"); err != nil {
		return VM{}, err
	}
	return nv.clone(), nil
}

// deadline: now + ttlS, or no auto-stop for 0.
func deadline(now time.Time, ttlS int) *time.Time {
	if ttlS <= 0 {
		return nil
	}
	t := now.Add(time.Duration(ttlS) * time.Second)
	return &t
}

// nameTakenLocked: does another VM of team carry name? Caller holds m.mu.
func (m *Manager) nameTakenLocked(team, name, except string) bool {
	for _, o := range m.vms {
		if o.ID != except && o.Team == team && o.Name == name {
			return true
		}
	}
	return false
}
