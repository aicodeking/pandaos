package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Delete tombstones.
//
// A tombstone marks a VM whose delete has started: a crash after it is durable makes
// the next boatd finish the delete, never resume or list the VM. It lives in
// StateDir/tombstones/<id>, outside the team's quota'd subvolume (vms/<id> is a
// symlink into it): a team at its quota, or a host with no free blocks in the
// team's subvolume, can still delete. If the host itself is full, deleteVM
// reclaims payloads and retries; persistent ENOSPC permits deletion without a
// durable marker so deletion can always reclaim space. The older marker, "deleting",
// inside the VM dir, is still honored and migrated.
//
// A delete whose directory removal fails keeps its tombstone and leaves the VM in the
// terminal `error` state: it cannot be started (409), its disk stays committed to its
// team (the VM stays in the table while the dir exists), and a background loop
// retries the removal with backoff, across restarts too.

const (
	tombstoneFile  = "deleting" // legacy marker inside the VM dir
	tombstoneSub   = "tombstones"
	deleteRetryMin = 30 * time.Second
	deleteRetryMax = 10 * time.Minute
)

func (m *Manager) tombstonePath(id string) string {
	return filepath.Join(m.cfg.StateDir, tombstoneSub, id)
}

// ensureTombstoneDir creates StateDir/tombstones and fsyncs StateDir once so
// subsequent deletes only need to fsync the tombstone file and tombstones/.
func (m *Manager) ensureTombstoneDir() error {
	m.tombstoneDirMu.Lock()
	defer m.tombstoneDirMu.Unlock()
	dir := filepath.Join(m.cfg.StateDir, tombstoneSub)
	if m.tombstoneDirSynced.Load() && exists(dir) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	syncFn := m.syncDirFn
	if syncFn == nil {
		syncFn = syncDir
	}
	if err := syncFn(m.cfg.StateDir); err != nil {
		return err
	}
	m.tombstoneDirSynced.Store(true)
	return nil
}

// writeTombstone makes the delete marker durable (file and its tombstones/
// directory entry; tombstones/ itself is pre-created and fsynced once at startup).
// It carries a copy of the VM record (when v has one): a half-removed dir may have
// lost its vm.json, and the VM must still be known, billed against its team and
// torn down after a restart.
func (m *Manager) writeTombstone(id string, v VM) error {
	if !m.tombstoneDirSynced.Load() {
		if err := m.ensureTombstoneDir(); err != nil {
			return err
		}
	}
	writeFile := m.tombstoneWriteFile
	if writeFile == nil {
		writeFile = os.WriteFile
	}
	path := m.tombstonePath(id)
	body := []byte("deleting\n")
	if v.ID == id {
		if b, err := json.Marshal(v); err == nil {
			body = b
		}
	}
	if err := writeFile(path, body, 0o600); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		m.tombstoneDirSynced.Store(false)
		if err := m.ensureTombstoneDir(); err != nil {
			return err
		}
		if err := writeFile(path, body, 0o600); err != nil {
			return err
		}
	}
	return syncFile(path)
}

// readTombstone returns the VM record a tombstone carries, if it has one.
func (m *Manager) readTombstone(id string) (VM, bool) {
	var v VM
	err := readJSON(m.tombstonePath(id), &v)
	return v, err == nil && v.ID == id
}

// doomedVM is how a VM whose delete could not be finished lives on: terminal.
func doomedVM(v VM) VM {
	v.State, v.PID, v.AgentReady, v.PendingCopy, v.ResumeOnBoot = Error, 0, false, false, false
	v.Error = "delete failed: removal is retried in the background"
	return v
}

// tombstoned reports whether id's delete has started (new or legacy marker).
func (m *Manager) tombstoned(id string) bool {
	return exists(m.tombstonePath(id)) || exists(filepath.Join(m.vmDir(id), tombstoneFile))
}

func (m *Manager) clearTombstone(id string) {
	if err := os.Remove(m.tombstonePath(id)); err != nil && !os.IsNotExist(err) {
		log.Printf("%s: removing the delete tombstone: %v", id, err)
	}
}

// tombstonedIDs lists every VM with a marker, new or legacy.
func (m *Manager) tombstonedIDs() []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && id[0] != '.' && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if ents, err := os.ReadDir(filepath.Join(m.cfg.StateDir, tombstoneSub)); err == nil {
		for _, e := range ents {
			add(e.Name())
		}
	}
	legacy, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "vms", "*", tombstoneFile))
	for _, t := range legacy {
		add(filepath.Base(filepath.Dir(t)))
	}
	return ids
}

// finishTombstones completes every delete a crash interrupted: a VM carrying a
// tombstone is torn down for good (processes, network, snapshots, billing, dir), and
// is never resumed. A dir that cannot be removed keeps its tombstone (a legacy marker
// is migrated to the new place first) and is retried in the background; reconcile
// loads such a VM as a terminal `error` VM.
func (m *Manager) finishTombstones() {
	for _, id := range m.tombstonedIDs() {
		dir := m.vmDir(id)
		var v VM
		have := readJSON(filepath.Join(dir, "vm.json"), &v) == nil && v.ID == id
		if !have {
			v, have = m.readTombstone(id)
		}
		if !exists(m.tombstonePath(id)) {
			if err := m.writeTombstone(id, v); err != nil {
				log.Printf("%s: migrating the legacy delete marker: %v", id, err)
			}
		}
		if have {
			if v.PID > 0 && m.procs.Alive(v.PID, id) {
				m.procs.Kill(v.PID)
			}
			delVMNet(m.run, v)
			delNeighbours(m.cfg, m.run, v)
			m.rmVCPUCgroup(id)
			if v.StartedAt != nil && v.State != Stopped && v.State != Error && !m.usage.hasVM(id, *v.StartedAt) {
				if err := m.usage.append(usageRec{"vm", id, v.Team, v.Size, *v.StartedAt, m.now()}); err != nil {
					log.Printf("usage: %v", err)
				}
			}
		}
		for _, s := range m.Snapshots(id) {
			m.DeleteSnapshot(id, s.ID)
		}
		if err := m.removeVMDir(id); err != nil {
			log.Printf("%s: finishing an interrupted delete: %v (retrying in the background)", id, err)
			continue
		}
		m.clearTombstone(id)
		log.Printf("%s: finished a delete that a crash interrupted", id)
	}
}

// removeVMState removes the VM's directory after quiescing its vm.json writes. On
// failure the writes are reopened: the VM stays, and its later state changes must
// reach vm.json again.
func (m *Manager) removeVMState(id string) error {
	m.mu.Lock()
	delete(m.dirty, id) // flushDirty must not write into the dir being removed
	p := m.persist[id]
	m.mu.Unlock()
	if p != nil {
		p.closeWrites() // waits for a write in flight (outside m.mu); none lands after
	}
	err := m.removeVMDir(id)
	if err != nil && p != nil {
		m.mu.Lock()
		seq := p.seq
		m.mu.Unlock()
		p.reopen(seq)
	}
	return err
}

// forgetDeleted drops a VM whose dir is gone from every table.
func (m *Manager) forgetDeleted(v VM) {
	id := v.ID
	m.mu.Lock()
	m.freeIndexLocked(v.Index)
	delete(m.vms, id)
	delete(m.dirty, id)
	delete(m.persist, id)
	m.storageGen++
	m.stateGen++
	delete(m.dones, id)
	delete(m.active, id)
	m.mu.Unlock()
	m.ev.publish(Event{Type: "vm.deleted", VMID: id, Team: v.Team, Size: v.Size})
}

// deleteFailed handles a dir that could not be removed: the VM goes to the terminal
// error state (start is refused while its tombstone exists), keeps counting against
// its team, and the removal is retried in the background.
func (m *Manager) deleteFailed(id string, v VM, cause error) (VM, error) {
	msg := "delete failed: " + cause.Error() + " (removal is retried in the background)"
	m.update(id, "delete failed", func(x *VM) { x.State, x.PID, x.AgentReady, x.Error = Error, 0, false, msg })
	m.scheduleDeleteRetry(id)
	return v, fmt.Errorf("delete: %w (the VM is in the error state and its removal is retried in the background)", cause)
}

func (m *Manager) scheduleDeleteRetry(id string) {
	m.mu.Lock()
	if m.deleteRetries == nil {
		m.deleteRetries = map[string]bool{}
	}
	if m.deleteRetries[id] {
		m.mu.Unlock()
		return
	}
	m.deleteRetries[id] = true
	m.mu.Unlock()
	go m.deleteRetryLoop(id)
}

func (m *Manager) retryLeftoverTombstones() {
	for _, id := range m.tombstonedIDs() {
		m.scheduleDeleteRetry(id)
	}
}

func (m *Manager) deleteRetryLoop(id string) {
	delay, limit := m.deleteRetryDelay, deleteRetryMax
	if delay <= 0 {
		delay = deleteRetryMin
	}
	if limit < delay {
		limit = delay
	}
	for {
		select {
		case <-m.watchStop:
			return
		case <-time.After(delay):
		}
		if m.retryDelete(id) {
			m.mu.Lock()
			delete(m.deleteRetries, id)
			m.mu.Unlock()
			return
		}
		if delay *= 2; delay > limit {
			delay = limit
		}
	}
}

// retryDelete reports whether the delete is finished.
func (m *Manager) retryDelete(id string) bool {
	unlock := m.lock(id)
	defer unlock()
	v, verr := m.get(id)
	if verr == nil {
		for _, s := range m.Snapshots(id) {
			m.DeleteSnapshot(id, s.ID)
		}
	}
	if err := m.removeVMState(id); err != nil {
		log.Printf("%s: retrying a failed delete: %v", id, err)
		return false
	}
	m.clearTombstone(id)
	log.Printf("%s: finished a delete that had failed", id)
	if verr == nil {
		m.forgetDeleted(v)
		if err := m.syncNet(); err != nil {
			log.Printf("%s: %v", id, err)
		}
	}
	return true
}
