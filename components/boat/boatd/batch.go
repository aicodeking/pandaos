package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// maxBatch bounds POST /v1/vms {"count": N}.
const maxBatch = 16

// CreateBatch creates r.Count VMs, all or none. All of them are admitted (team quota
// and host capacity) under one hold of the manager lock before any boots, so a batch
// never half-fits; if any of them then fails to boot, all are deleted and the first
// error is returned. A name "x" becomes x-1 … x-N.
func (m *Manager) CreateBatch(ctx context.Context, r CreateReq) ([]VM, error) {
	if r.Count < 1 || r.Count > maxBatch {
		return nil, errf(400, "count must be 1..%d", maxBatch)
	}
	if err := r.validate(m.cfg); err != nil {
		return nil, err
	}
	warm := ""
	if r.Snapshot == "" {
		warm = m.warmTemplate(r.Image, r.Size)
		defer m.ensureWarm(context.WithoutCancel(ctx), r.Image, r.Size)
	}
	p, err := m.prepareCreate(ctx, r, warm)
	if err != nil {
		return nil, err
	}
	bases := make([]VM, r.Count)
	for i := range bases {
		bases[i] = p.base
		if r.Name != "" && r.Count > 1 {
			bases[i].Name = fmt.Sprintf("%s-%d", r.Name, i+1)
			if !reName.MatchString(bases[i].Name) {
				return nil, errf(400, "name %q too long for count %d", r.Name, r.Count)
			}
		}
	}
	t0 := time.Now()
	vms, err := m.allocBatch(bases)
	if err != nil {
		return nil, p.admission(err)
	}
	allocT := time.Since(t0)

	out := make([]VM, len(vms))
	errs := make([]error, len(vms))
	var wg sync.WaitGroup
	for i, v := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = m.finishCreate(ctx, v, p, allocT)
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			m.rollback(ctx, vms)
			return nil, e
		}
	}
	return out, nil
}

// allocBatch allocates every base or none.
func (m *Manager) allocBatch(bases []VM) ([]VM, error) {
	m.mu.Lock()
	var vms []VM
	var err error
	for _, b := range bases {
		var v VM
		if v, err = m.allocLocked(b, Creating); err != nil {
			break
		}
		vms = append(vms, v)
	}
	m.mu.Unlock()
	if err != nil {
		m.rollback(context.Background(), vms)
		return nil, err
	}
	return vms, nil
}

func (m *Manager) rollback(ctx context.Context, vms []VM) {
	ctx = context.WithoutCancel(ctx) // a client gone must not leave half a batch
	for _, v := range vms {
		if _, err := m.Delete(ctx, v.ID); err != nil && err != errNotFound {
			log.Printf("%s: batch rollback: %v", v.ID, err)
		}
	}
}
