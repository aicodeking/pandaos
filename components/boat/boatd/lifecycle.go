package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type CreateReq struct {
	Name      string            `json:"name"`
	Size      string            `json:"size"`
	Image     string            `json:"image"`
	TTLS      int               `json:"ttl_s"`
	IdleStopS int               `json:"idle_stop_s"`
	Team      string            `json:"team"`
	Labels    map[string]string `json:"labels"`
	Env       map[string]string `json:"env"`
	Snapshot  string            `json:"snapshot"` // create from a saved snapshot instead of an image
}

func validEnv(env map[string]string) error {
	for k := range env {
		if !reEnv.MatchString(k) {
			return errf(400, "invalid env name %q", k)
		}
	}
	return nil
}

func (r *CreateReq) validate(c Config) error {
	if r.Team == "" {
		r.Team = "default"
	}
	if r.Size == "" {
		r.Size = c.DefaultSize
	}
	if r.Image == "" {
		r.Image = c.DefaultImage
	}
	switch {
	case r.Name != "" && !reName.MatchString(r.Name):
		return errf(400, "invalid name (lowercase letters, digits, '-', max 63)")
	case !reTeam.MatchString(r.Team):
		return errf(400, "invalid team")
	case !reImage.MatchString(r.Image):
		return errf(400, "invalid image")
	case r.TTLS < 0 || r.IdleStopS < -1:
		return errf(400, "ttl_s and idle_stop_s must be >= 0 (idle_stop_s -1 = never)")
	case len(r.Labels) > 64:
		return errf(400, "too many labels")
	}
	if _, ok := c.Sizes[r.Size]; !ok {
		return errf(400, "unknown size %q", r.Size)
	}
	for k, v := range r.Labels {
		if !reLabel.MatchString(k) || len(v) > 256 {
			return errf(400, "invalid label %q", k)
		}
	}
	return validEnv(r.Env)
}

func (m *Manager) Create(ctx context.Context, r CreateReq) (VM, error) {
	if err := r.validate(m.cfg); err != nil {
		return VM{}, err
	}
	s := m.cfg.Sizes[r.Size]
	base := VM{Name: r.Name, Team: r.Team, Image: r.Image, Size: r.Size, VCPU: s.VCPU, MemMiB: s.MemMiB,
		DiskGiB: s.DiskGiB, Labels: r.Labels, TTLS: r.TTLS, IdleStopS: r.IdleStopS}
	src, restore := m.imageDir(r.Image), false
	if r.Snapshot != "" {
		m.mu.Lock()
		sn, ok := m.snaps[r.Snapshot]
		m.mu.Unlock()
		if !ok || sn.Team != r.Team {
			return VM{}, errf(404, "snapshot not found")
		}
		base.Image, base.Size, base.VCPU, base.MemMiB, base.DiskGiB = sn.Image, sn.Size, sn.VCPU, sn.MemMiB, sn.DiskGiB
		base.FromSnapshot, src, restore = sn.ID, m.snapDir(sn.ID), sn.HasMem
	} else if !exists(filepath.Join(src, diskFile)) || !exists(filepath.Join(src, kernFile)) {
		return VM{}, errf(400, "image %q is not installed", r.Image)
	}
	if err := m.checkFloor(0); err != nil {
		return VM{}, err
	}
	v, err := m.alloc(base, Creating)
	if err != nil {
		return VM{}, err
	}
	unlock := m.lock(v.ID)
	defer unlock()
	if err := m.materialize(v, src, restore); err != nil {
		return m.fail(v.ID, err)
	}
	return m.bootNew(ctx, v, restore, r.Env)
}

// materialize reflink-copies disk, kernel (+ vmstate/mem) from src into the
// VM root, grows the disk to the size quota and hands it to the jail uid.
func (m *Manager) materialize(v VM, src string, withMem bool) error {
	root := m.root(v.ID)
	files := []string{diskFile, kernFile}
	if withMem {
		files = append(files, vmstate, memFile)
	}
	for _, f := range files {
		if err := m.cp(filepath.Join(src, f), filepath.Join(root, f)); err != nil {
			return err
		}
	}
	return m.finishDisk(v)
}

func (m *Manager) finishDisk(v VM) error {
	disk := filepath.Join(m.root(v.ID), diskFile)
	st, err := os.Stat(disk)
	if err != nil {
		return err
	}
	// Grow only, and only for cold boots: a snapshot's guest already knows its disk size.
	if want := int64(v.DiskGiB) << 30; st.Size() < want && !exists(filepath.Join(m.root(v.ID), vmstate)) {
		if err := os.Truncate(disk, want); err != nil {
			return err
		}
	}
	return m.chownRoot(v)
}

func (m *Manager) cp(src, dst string) error {
	return m.run("", "cp", "--reflink="+m.cfg.Reflink, src, dst)
}

func (m *Manager) chownRoot(v VM) error {
	uid, gid := m.ids(v)
	if uid == 0 {
		return nil
	}
	return filepath.Walk(m.root(v.ID), func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// bootNew starts a freshly materialized VM and gives the guest its identity.
func (m *Manager) bootNew(ctx context.Context, v VM, restore bool, env map[string]string) (VM, error) {
	t0 := time.Now()
	if err := m.syncNet(); err != nil { // anti-spoof rules are in force before its first frame
		return m.fail(v.ID, err)
	}
	v, err := m.boot(ctx, v, restore)
	if err != nil {
		return m.fail(v.ID, err)
	}
	t1 := time.Now()
	if err := m.syncNet(); err != nil { // routes: it is running now
		return m.fail(v.ID, err)
	}
	log.Printf("%s: boot=%dms net=%dms", v.ID, t1.Sub(t0).Milliseconds(), time.Since(t1).Milliseconds())
	return m.initGuest(ctx, v, env, true, restore)
}

func (m *Manager) fail(id string, err error) (VM, error) {
	log.Printf("%s: %v", id, err)
	v, _ := m.get(id)
	m.killWait(id, v.PID)
	delVMNet(m.run, v)
	m.update(id, "failed", func(v *VM) { v.State, v.Error, v.PID = Error, err.Error(), 0 })
	m.syncNet()
	return VM{}, err
}

func (m *Manager) bootArgs(v VM) string {
	args := m.cfg.BootArgs
	var meta struct {
		BootArgs string `json:"boot_args"`
	}
	if readJSON(filepath.Join(m.imageDir(v.Image), "meta.json"), &meta) == nil && meta.BootArgs != "" {
		args = meta.BootArgs
	}
	return fmt.Sprintf("%s ip=%s::%s:255.255.0.0:%s:eth0:off", args, v.IP, m.cfg.gateway(), v.Name)
}

// boot launches firecracker and either cold-boots or restores root/{vmstate,mem}.
func (m *Manager) boot(ctx context.Context, v VM, restore bool) (VM, error) {
	uid, gid := m.ids(v)
	if err := addVMNet(m.cfg, m.run, v, uid); err != nil {
		return v, err
	}
	root := m.root(v.ID)
	os.Remove(filepath.Join(root, apiSock))
	os.Remove(filepath.Join(root, vsockUDS))
	if !restore { // a cold boot has no memory base
		os.Remove(filepath.Join(root, vmstate))
		os.Remove(filepath.Join(root, memFile))
	}
	pid, done, err := m.procs.Start(v, root, uid, gid)
	if err != nil {
		delVMNet(m.run, v)
		return v, err
	}
	m.mu.Lock()
	m.dones[v.ID] = done
	m.mu.Unlock()
	if v, err = m.update(v.ID, "", func(x *VM) { x.PID = pid }); err != nil {
		return v, err
	}
	go m.watch(v.ID, pid, done)
	err = waitSock(filepath.Join(root, apiSock), done, 5*time.Second)
	fc := newFC(filepath.Join(root, apiSock))
	if err == nil && restore {
		if err = fc.restore(ctx, m.cfg.DiffSnapshots); err == nil {
			// vmstate is consumed; mem stays: it backs the guest (MAP_PRIVATE) and is the next diff's base.
			os.Remove(filepath.Join(root, vmstate))
		}
	} else if err == nil {
		err = fc.boot(ctx, v, m.bootArgs(v), m.cfg.DiffSnapshots, m.cfg)
	}
	if err != nil {
		m.killWait(v.ID, pid)
		delVMNet(m.run, v)
		return v, err
	}
	return m.update(v.ID, "", func(x *VM) {
		x.State, x.HasSnapshot, x.Error, x.DiffOK = Running, false, "", m.cfg.DiffSnapshots
		if x.TTLS > 0 {
			t := m.now().Add(time.Duration(x.TTLS) * time.Second)
			x.ExpiresAt = &t
		}
	})
}

// initGuest waits for the agent, then (new VMs) sets identity, (restores)
// fixes the clock, and pushes env. An image without an agent still runs:
// agent_ready stays false.
func (m *Manager) initGuest(ctx context.Context, v VM, env map[string]string, identity, restored bool) (VM, error) {
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	t0 := time.Now()
	err := waitAgent(ctx, sock, time.Duration(m.cfg.BootTimeoutS)*time.Second)
	t1 := time.Now()
	if err == nil && identity {
		m.reseed(ctx, v, sock)
	}
	if err == nil && identity {
		id := map[string]any{"op": "set_identity", "hostname": v.Name, "ipv4_cidr": v.IP + "/16",
			"gw": m.cfg.gateway().String(), "mac": v.MAC, "dns": m.cfg.DNS}
		if v.IPv6 != "" {
			bits := netip.MustParsePrefix(m.cfg.IPv6Prefix).Bits() // on-link prefix; this VM owns one address
			id["ipv6_cidr"], id["ipv6_gw"] = fmt.Sprintf("%s/%d", v.IPv6, bits), m.cfg.ipv6Gateway()
		}
		_, err = agentCall(ctx, sock, id)
	}
	t2 := time.Now()
	if err == nil && restored {
		m.syncClock(ctx, sock)
		if v.DiskGrown {
			if gerr := m.growGuestDisk(ctx, v); gerr != nil {
				log.Printf("%s: grow disk after restore: %v", v.ID, gerr)
			} else {
				v, _ = m.update(v.ID, "", func(x *VM) { x.DiskGrown = false })
			}
		}
	}
	if err == nil && len(env) > 0 {
		_, err = agentCall(ctx, sock, map[string]any{"op": "set_env", "vars": env})
	}
	log.Printf("%s: guest init agent=%dms identity=%dms clock+env=%dms", v.ID, t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), time.Since(t2).Milliseconds())
	msg := ""
	if err != nil {
		msg = err.Error()
		log.Printf("%s: %v", v.ID, err)
	}
	return m.update(v.ID, "", func(x *VM) { x.AgentReady, x.Error = err == nil, msg })
}

// reseed mixes 64 fresh host bytes into the guest CRNG before set_identity
// regenerates machine-id and ssh host keys: forks of one snapshot share the
// parent's RNG state until then.
func (m *Manager) reseed(ctx context.Context, v VM, sock string) {
	b := make([]byte, 64)
	rand.Read(b)
	if _, err := agentCall(ctx, sock, map[string]any{"op": "reseed", "entropy_b64": base64.StdEncoding.EncodeToString(b)}); err != nil {
		log.Printf("%s: reseed: %v", v.ID, err)
	}
}

// syncClock: a restored guest's wall clock is as old as its snapshot.
func (m *Manager) syncClock(ctx context.Context, sock string) {
	a, err := dialAgent(ctx, sock)
	if err != nil {
		return
	}
	defer a.Close()
	now := strconv.FormatInt(m.now().Unix(), 10)
	if a.send(map[string]any{"op": "exec", "argv": []string{"date", "-u", "-s", "@" + now}, "user": "root", "timeout_s": 10}) != nil {
		return
	}
	for {
		if _, r, err := a.recv(); err != nil || r.final() {
			return
		}
	}
}

func (m *Manager) Stop(ctx context.Context, id, reason string) (VM, error) {
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil || v.State == Stopped {
		return v, err
	}
	if v.State != Running {
		return v, errf(http.StatusConflict, "vm is %s", v.State)
	}
	if v, err = m.update(id, reason, func(x *VM) { x.State = Stopping }); err != nil {
		return v, err
	}
	root := m.root(id)
	err = m.checkFloor(uint64(v.MemMiB) << 20) // a diff is usually far smaller; plan for a full one
	if err == nil {
		v, err = m.takeSnapshot(ctx, v)
	}
	snap := err == nil
	if !snap {
		// No snapshot: let the guest flush its disk and reboot (= exit) instead.
		log.Printf("%s: no snapshot, shutting down cold: %v", id, err)
		newFC(filepath.Join(root, apiSock)).resume(ctx)
		agentCall(ctx, filepath.Join(root, vsockUDS), map[string]string{"op": "shutdown"})
		m.mu.Lock()
		done := m.dones[id]
		m.mu.Unlock()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}
	m.killWait(id, v.PID)
	if snap {
		snap = os.Rename(filepath.Join(root, snapState), filepath.Join(root, vmstate)) == nil
	}
	if !snap {
		os.Remove(filepath.Join(root, memFile))
	}
	delVMNet(m.run, v)
	v, err = m.update(id, reason, func(x *VM) {
		x.State, x.PID, x.HasSnapshot, x.SnapSynced, x.AgentReady, x.ExpiresAt = Stopped, 0, snap, false, false, nil
	})
	m.syncNet()
	if snap {
		go m.syncSnapshot(id)
	}
	return v, err
}

func (m *Manager) Start(ctx context.Context, id string, env map[string]string) (VM, error) {
	if err := validEnv(env); err != nil {
		return VM{}, err
	}
	unlock := m.lock(id)
	defer unlock()
	m.mu.Lock()
	old, ok := m.vms[id]
	var err error
	switch {
	case !ok:
		err = errNotFound
	case old.State == Running:
		m.mu.Unlock()
		return old.clone(), nil
	case old.State != Stopped && old.State != Error:
		err = errf(http.StatusConflict, "vm is %s", old.State)
	default:
		if err = m.quotaLocked(old.Team, Size{old.VCPU, old.MemMiB, old.DiskGiB}, id); err == nil {
			nv := old.clone()
			nv.State = Starting
			err = m.commitLocked(old, nv, "")
		}
	}
	m.mu.Unlock()
	if err != nil {
		return VM{}, err
	}
	v, _ := m.get(id)
	root := m.root(id)
	restore := exists(filepath.Join(root, vmstate)) && exists(filepath.Join(root, memFile))
	v, err = m.boot(ctx, v, restore)
	if err != nil && restore {
		log.Printf("%s: restore failed, cold booting: %v", id, err)
		os.Remove(filepath.Join(root, vmstate))
		os.Remove(filepath.Join(root, memFile))
		restore = false
		v, err = m.boot(ctx, v, false)
	}
	if err == nil {
		err = m.syncNet()
	}
	if err != nil {
		return m.fail(id, err)
	}
	return m.initGuest(ctx, v, env, false, restore)
}

// Fork clones a running (paused for the snapshot) or stopped VM.
func (m *Manager) Fork(ctx context.Context, id, name, team string) (VM, error) {
	if name != "" && !reName.MatchString(name) {
		return VM{}, errf(400, "invalid name")
	}
	unlockP := m.lock(id)
	p, err := m.get(id)
	if err == nil && team != "" && p.Team != team {
		err = errNotFound
	}
	if err == nil && p.State != Running && p.State != Stopped {
		err = errf(http.StatusConflict, "vm is %s", p.State)
	}
	if err != nil {
		unlockP()
		return VM{}, err
	}
	base := VM{Name: name, Team: p.Team, Image: p.Image, Size: p.Size, VCPU: p.VCPU, MemMiB: p.MemMiB,
		DiskGiB: p.DiskGiB, Labels: p.Labels, TTLS: p.TTLS, IdleStopS: p.IdleStopS, ParentID: p.ID}
	err = m.checkFloor(uint64(p.MemMiB) << 20)
	var c VM
	if err == nil {
		c, err = m.alloc(base, Forking)
	}
	if err != nil {
		unlockP()
		return VM{}, err
	}
	unlockC := m.lock(c.ID)
	defer unlockC()
	withMem, err := m.freeze(ctx, p, m.root(c.ID))
	unlockP()
	if err == nil {
		err = m.finishDisk(c)
	}
	if err != nil {
		return m.fail(c.ID, err)
	}
	return m.bootNew(ctx, c, withMem, nil)
}

func (m *Manager) Delete(ctx context.Context, id string) (VM, error) {
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil {
		return v, err
	}
	m.killWait(id, v.PID)
	delVMNet(m.run, v)
	m.flushConntrack(v.IP, v.IPv6, v.PublicIP)
	if m.cfg.CgroupParent != "" {
		os.Remove(filepath.Join("/sys/fs/cgroup", m.cfg.CgroupParent, id)) // the jailer's (empty) cgroup
	}
	m.update(id, "deleted", func(x *VM) { x.State, x.PID = Stopped, 0 }) // closes the billing interval
	for _, s := range m.Snapshots(id) {
		m.DeleteSnapshot(id, s.ID)
	}
	if err := m.removeVMDir(id); err != nil {
		return v, err
	}
	m.mu.Lock()
	delete(m.vms, id)
	delete(m.dones, id)
	delete(m.active, id)
	m.mu.Unlock()
	m.ev.publish(Event{Type: "vm.deleted", VMID: id, Team: v.Team, Size: v.Size})
	m.syncNet()
	v.State = "deleted"
	return v, nil
}

// AddPort exposes guest_port: always as a gateway hostname route, and with
// public=true also as a raw TCP host port (DNAT from portMin..portMax).
// Re-adding a port sets its visibility: public=false makes it private again.
func (m *Manager) AddPort(id string, guest int, public bool) (Port, error) {
	if guest < 1 || guest > 65535 {
		return Port{}, errf(400, "port must be 1-65535")
	}
	m.mu.Lock()
	old, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return Port{}, errNotFound
	}
	nv := old.clone()
	p := Port{GuestPort: guest, Hostname: fmt.Sprintf("%d-%s", guest, id)}
	idx := -1
	for i, q := range nv.Ports {
		if q.GuestPort == guest {
			p, idx = q, i
		}
	}
	var err error
	closed := Port{}
	if idx < 0 {
		err = m.portQuotaLocked(nv)
	}
	if err == nil && public && p.HostPort == 0 {
		p.HostPort, err = m.freePortLocked()
	} else if !public && p.HostPort != 0 {
		closed, p.HostPort = p, 0
	}
	if err == nil {
		if idx >= 0 {
			nv.Ports[idx] = p
		} else {
			nv.Ports = append(nv.Ports, p)
		}
		err = m.commitLocked(old, nv, "")
	}
	m.mu.Unlock()
	if err != nil {
		return Port{}, err
	}
	err = m.syncNet()
	if closed.GuestPort != 0 {
		m.flushPort(old, closed)
	}
	return p, err
}

// portQuotaLocked: one more port on v, within maxPortsPerVM and the team's maxPorts?
func (m *Manager) portQuotaLocked(v VM) error {
	if lim := m.cfg.MaxPortsPerVM; lim > 0 && len(v.Ports) >= lim {
		return errf(http.StatusTooManyRequests, "quota: a VM may expose %d ports", lim)
	}
	n := 1
	for _, o := range m.vms {
		if o.Team == v.Team {
			n += len(o.Ports)
		}
	}
	if lim := m.cfg.team(v.Team).MaxPorts; lim > 0 && n > lim {
		return errf(http.StatusTooManyRequests, "quota: team %s may expose %d ports", v.Team, lim)
	}
	return nil
}

func (m *Manager) freePortLocked() (int, error) {
	used := map[int]bool{}
	for _, v := range m.vms {
		for _, p := range v.Ports {
			used[p.HostPort] = true
		}
	}
	for h := m.cfg.PortMin; h <= m.cfg.PortMax; h++ {
		if !used[h] {
			return h, nil
		}
	}
	return 0, errf(http.StatusServiceUnavailable, "no free host ports")
}

func (m *Manager) RemovePort(id string, guest int) error {
	closed := Port{}
	v, err := m.update(id, "", func(v *VM) {
		out := []Port{}
		for _, p := range v.Ports {
			if p.GuestPort != guest {
				out = append(out, p)
			} else {
				closed = p
			}
		}
		v.Ports = out
	})
	if err == nil {
		err = m.syncNet() // the port stays open until the rules load: tell the caller
	}
	if closed.GuestPort != 0 {
		m.flushPort(v, closed)
	}
	return err
}

func (m *Manager) Usage(since time.Time) (usageReport, error) {
	recs, err := m.usage.read()
	if err != nil {
		return usageReport{}, err
	}
	now := m.now()
	m.mu.Lock()
	for _, v := range m.vms {
		if v.State == Running && v.StartedAt != nil {
			recs = append(recs, usageRec{"vm", v.ID, v.Team, v.Size, *v.StartedAt, now})
		}
	}
	for _, s := range m.snaps {
		recs = append(recs, usageRec{"snapshot", s.ID, s.Team, s.Size, s.CreatedAt, now})
	}
	m.mu.Unlock()
	rep := summarize(recs, since, now)
	st, disk := m.storage()
	rep.Disk, rep.Storage = &disk, []teamStorage{}
	for _, t := range st {
		rep.Storage = append(rep.Storage, *t)
	}
	sort.Slice(rep.Storage, func(i, j int) bool { return rep.Storage[i].Team < rep.Storage[j].Team })
	return rep, nil
}
