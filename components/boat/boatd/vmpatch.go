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
	Name         *string           `json:"name"`
	TTLS         *int              `json:"ttl_s"`
	ExtendS      *int              `json:"extend_s"`
	Labels       map[string]string `json:"labels"`
	IdleStopS    *int              `json:"idle_stop_s"`
	ResumeOnBoot *bool             `json:"resume_on_boot"`
}

func (r PatchReq) validate() error {
	switch {
	case r.Name == nil && r.TTLS == nil && r.ExtendS == nil && r.Labels == nil && r.IdleStopS == nil && r.ResumeOnBoot == nil:
		return errf(400, "set name, ttl_s, extend_s, labels, idle_stop_s or resume_on_boot")
	case r.Name != nil && !reName.MatchString(*r.Name):
		return errf(400, "invalid name (lowercase letters, digits, '-', max 63)")
	case r.TTLS != nil && (*r.TTLS < 0 || *r.TTLS > maxPatchS):
		return errf(400, "ttl_s must be 0 (no auto-stop) to %d", maxPatchS)
	case r.ExtendS != nil && (*r.ExtendS < 1 || *r.ExtendS > maxPatchS):
		return errf(400, "extend_s must be 1 to %d", maxPatchS)
	case r.IdleStopS != nil && (*r.IdleStopS < -1 || *r.IdleStopS > maxPatchS):
		return errf(400, "idle_stop_s must be -1 (never) to %d", maxPatchS)
	case len(r.Labels) > 64:
		return errf(400, "too many labels (max 64)")
	}
	for k, v := range r.Labels {
		if !reLabel.MatchString(k) || len(v) > 256 {
			return errf(400, "invalid label %q", k)
		}
	}
	return nil
}

// Patch applies r to VM id atomically under m.mu. Nothing changes unless every part applies.
func (m *Manager) Patch(id string, r PatchReq) (VM, error) {
	if err := r.validate(); err != nil {
		return VM{}, err
	}
	m.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			m.mu.Unlock()
		}
	}()
	old, ok := m.vms[id]
	if !ok || old.PendingCopy {
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
	if r.Labels != nil {
		if len(r.Labels) == 0 {
			nv.Labels = nil
		} else {
			nv.Labels = make(map[string]string, len(r.Labels))
			for k, v := range r.Labels {
				nv.Labels[k] = v
			}
		}
	}
	if r.IdleStopS != nil {
		nv.IdleStopS = *r.IdleStopS
	}
	if r.ResumeOnBoot != nil {
		nv.ResumeOnBoot = *r.ResumeOnBoot
	}
	// The vm.json write waits outside m.mu: behind a stop's fsync of this VM, a patch
	// must not hold every other VM up.
	nv, finish := m.commitDeferLocked(old, nv, "patch")
	m.mu.Unlock()
	unlocked = true
	if err := finish(); err != nil {
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
	taken := false
	m.eachVMLocked(func(o VM) {
		if o.ID != except && o.Team == team && o.Name == name {
			taken = true
		}
	})
	return taken
}
