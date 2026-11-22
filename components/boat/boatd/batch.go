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
	vms, err = m.resetSSHBootBatch(vms)
	if err == nil {
		err = m.syncNet()
	}
	if err != nil {
		m.rollback(ctx, vms)
		return nil, err
	}
	allocT := time.Since(t0)

	out := make([]VM, len(vms))
	errs := make([]error, len(vms))
	var wg sync.WaitGroup
	for i, v := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = m.finishCreateSync(ctx, v, p, allocT, false)
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			m.rollback(ctx, vms)
			return nil, e
		}
	}
	needNft := false
	for _, v := range out {
		if v.SSHPolicyReady {
			needNft = true
			break
		}
	}
	if needNft {
		err = m.syncNet()
	} else {
		err = m.syncRoutes()
	}
	if err != nil {
		m.rollback(ctx, vms)
		return nil, err
	}
	return out, nil
}

// allocBatch allocates every base or none, running conntrack flushes concurrently
// with a bounded worker pool instead of sequentially per VM.
func (m *Manager) allocBatch(bases []VM) ([]VM, error) {
	if len(bases) == 0 {
		return nil, nil
	}
	reserved := make([]VM, 0, len(bases))
	m.mu.Lock()
	var err error
	for _, b := range bases {
		var v VM
		if v, err = m.reserveVMLocked(b, Creating); err != nil {
			for _, r := range reserved {
				delete(m.allocating, r.ID)
			}
			break
		}
		reserved = append(reserved, v)
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	made := make([]VM, 0, len(reserved))
	for _, v := range reserved {
		if err = m.makeVMDir(v); err != nil {
			madeIDs := map[string]bool{}
			for _, md := range made {
				madeIDs[md.ID] = true
			}
			for _, r := range reserved {
				removeDir := ""
				if madeIDs[r.ID] {
					removeDir = r.ID
				}
				m.dropReservation(r.ID, removeDir)
			}
			return nil, err
		}
		made = append(made, v)
	}
	m.flushConntrackBatch(reserved)
	vms := make([]VM, 0, len(reserved))
	for i, v := range reserved {
		m.mu.Lock()
		delete(m.allocating, v.ID)
		if err = m.commitLocked(VM{}, v, "created"); err != nil {
			m.allocating[v.ID] = v
			m.mu.Unlock()
			for _, rem := range reserved[i:] {
				m.dropReservation(rem.ID, rem.ID)
			}
			m.rollback(context.Background(), vms)
			return nil, err
		}
		m.mu.Unlock()
		vms = append(vms, v)
	}
	return vms, nil
}

func (m *Manager) rollback(ctx context.Context, vms []VM) {
	ctx = context.WithoutCancel(ctx) // a client gone must not leave half a batch
	for _, v := range vms {
		if _, err := m.deleteVM(ctx, v.ID, false); err != nil && err != errNotFound {
			log.Printf("%s: batch rollback: %v", v.ID, err)
		}
	}
	if len(vms) > 0 {
		if err := m.syncNet(); err != nil {
			log.Printf("batch rollback syncNet: %v", err)
		}
	}
}
