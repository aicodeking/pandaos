package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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
	Snapshot  string            `json:"snapshot"`  // create from a saved snapshot instead of an image
	FailFast  bool              `json:"fail_fast"` // refuse (no_ready_machine) instead of waiting
	Count     int               `json:"count"`     // > 1: CreateBatch, all or none
	// FreshBoot (with Snapshot): restore only the snapshot's disk and cold boot; the
	// snapshot's memory is never loaded nor copied. The API sets it when the new VM's
	// secret selection is narrower than the source's.
	FreshBoot bool `json:"fresh_boot"`
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
	warm := ""
	if r.Snapshot == "" {
		warm = m.warmTemplate(r.Image, r.Size)
		// A background build must outlive this request.
		defer m.ensureWarm(context.WithoutCancel(ctx), r.Image, r.Size)
	}
	v, err := m.createFrom(ctx, r, warm)
	var refused *apiErr
	if err != nil && warm != "" && !errors.As(err, &refused) { // a quota refusal says nothing about the template
		os.RemoveAll(warm) // maybe from another Firecracker or kernel: the next create boots cold
	}
	return v, err
}

// createFrom creates r, restoring the warm template dir if not "". r is validated.
func (m *Manager) createFrom(ctx context.Context, r CreateReq, warm string) (VM, error) {
	p, err := m.prepareCreate(ctx, r, warm)
	if err != nil {
		return VM{}, err
	}
	t0 := time.Now()
	v, err := m.alloc(p.base, Creating)
	if err != nil {
		return VM{}, p.admission(err)
	}
	return m.finishCreate(ctx, v, p, time.Since(t0))
}

// createPlan is what a create needs once its VM is allocated.
type createPlan struct {
	base     VM
	src      string
	restore  bool
	sameTeam bool // src is the team's own (see copyMem)
	env      map[string]string
	failFast bool
}

// admission labels an admission refusal for a fail_fast request (see failFast).
func (p createPlan) admission(err error) error {
	if p.failFast {
		return failFast(err)
	}
	return err
}

// prepareCreate validates r, waits for the images (or refuses, with fail_fast) and
// works out the source to materialize from.
func (m *Manager) prepareCreate(ctx context.Context, r CreateReq, warm string) (createPlan, error) {
	if err := r.validate(m.cfg); err != nil {
		return createPlan{}, err
	}
	if r.FailFast {
		select {
		case <-m.imagesReady:
		default:
			return createPlan{}, &apiErr{code: http.StatusServiceUnavailable, msg: "images are still being prepared", kind: codeNoReadyMachine}
		}
	} else {
		select {
		case <-m.imagesReady:
		case <-ctx.Done():
			return createPlan{}, ctx.Err()
		}
	}
	s := m.cfg.Sizes[r.Size]
	base := VM{Name: r.Name, Team: r.Team, Image: r.Image, Size: r.Size, VCPU: s.VCPU, MemMiB: s.MemMiB,
		DiskGiB: s.DiskGiB, Labels: r.Labels, TTLS: r.TTLS, IdleStopS: r.IdleStopS}
	src, restore := m.imageDir(r.Image), false
	if r.Snapshot != "" {
		m.mu.Lock()
		sn, ok := m.snaps[r.Snapshot]
		m.mu.Unlock()
		if !ok || sn.PendingCopy || sn.Team != r.Team {
			return createPlan{}, errf(404, "snapshot not found")
		}
		base.Image, base.Size, base.VCPU, base.MemMiB, base.DiskGiB = sn.Image, sn.Size, sn.VCPU, sn.MemMiB, sn.DiskGiB
		base.FromSnapshot, src, restore = sn.ID, m.snapDir(sn.ID), sn.HasMem && !r.FreshBoot
		if sn.NoEnv || sn.Labels["boat.no_env"] == "1" {
			if base.Labels == nil {
				base.Labels = map[string]string{}
			} else {
				base.Labels = maps.Clone(base.Labels)
			}
			base.Labels["boat.no_env"] = "1"
		}
	} else if warm != "" {
		src, restore = warm, true
	} else if !exists(filepath.Join(src, diskFile)) || !exists(filepath.Join(src, kernFile)) {
		return createPlan{}, errf(400, "image %q is not installed", r.Image)
	}
	if err := m.checkFloor(0); err != nil {
		return createPlan{}, err
	}
	return createPlan{base: base, src: src, restore: restore, sameTeam: warm == "", env: r.Env, failFast: r.FailFast}, nil
}

// finishCreate materializes and boots v, allocated from p.
func (m *Manager) finishCreate(ctx context.Context, v VM, p createPlan, allocT time.Duration) (VM, error) {
	return m.finishCreateSync(ctx, v, p, allocT, true)
}

func (m *Manager) finishCreateSync(ctx context.Context, v VM, p createPlan, allocT time.Duration, syncNet bool) (VM, error) {
	t1 := time.Now()
	unlock := m.lock(v.ID)
	defer unlock()
	steps, err := m.materialize(v, p.src, p.restore, p.sameTeam)
	if err != nil {
		return m.failSync(v.ID, err, syncNet)
	}
	log.Printf("%s: create alloc=%dms materialize=%dms (%s)", v.ID, allocT.Milliseconds(), time.Since(t1).Milliseconds(), steps)
	return m.bootNewSync(ctx, v, p.restore, p.env, syncNet)
}

// materialize reflink-copies disk, kernel (+ vmstate/mem) from src into the
// VM root, grows the disk to the size quota and hands it to the jail uid.
// sameTeam: src belongs to v's team (see copyMem). steps: each step's milliseconds,
// for the log (a reflink costs O(extents), see fragmented).
func (m *Manager) materialize(v VM, src string, withMem, sameTeam bool) (steps string, err error) {
	root := m.root(v.ID)
	t := time.Now()
	lap := func(name string) {
		steps += fmt.Sprintf("%s=%d ", name, time.Since(t).Milliseconds())
		t = time.Now()
	}
	files := []string{diskFile, kernFile}
	if withMem {
		files = append(files, vmstate)
		if err := m.copyMem(filepath.Join(src, memFile), filepath.Join(root, memFile), sameTeam); err != nil {
			return steps, err
		}
		lap(memFile)
	}
	for _, f := range files {
		if err := m.cp(filepath.Join(src, f), filepath.Join(root, f)); err != nil {
			return steps, err
		}
		lap(f)
	}
	err = m.finishDisk(v)
	lap("grow+chown")
	return strings.TrimSpace(steps), err
}

func (m *Manager) finishDisk(v VM) error {
	disk := filepath.Join(m.root(v.ID), diskFile)
	st, err := os.Stat(disk)
	if err != nil {
		return err
	}
	// Grow only, and only for cold boots: a snapshot's guest already knows its disk size.
	if want := int64(v.DiskGiB) << 30; st.Size() < want && !exists(filepath.Join(m.root(v.ID), vmstate)) {
		if err := truncateJail(disk, want); err != nil {
			return err
		}
	}
	return m.chownRoot(v)
}

// cp clones src, which may be in a jail dir, to dst.
func (m *Manager) cp(src, dst string) error { return cloneFile(src, dst, m.cfg.Reflink) }

func (m *Manager) chownRoot(v VM) error {
	uid, gid := m.ids(v)
	if uid == 0 {
		return nil
	}
	return filepath.Walk(m.root(v.ID), func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Name() == memFile && fi.Mode().Perm() == 0o444 {
			return nil // sealed: maybe the same inode as another VM's (see copyMem)
		}
		return os.Lchown(p, uid, gid)
	})
}

// bootNew starts a freshly materialized VM and gives the guest its identity.
func (m *Manager) bootNew(ctx context.Context, v VM, restore bool, env map[string]string) (VM, error) {
	return m.bootNewSync(ctx, v, restore, env, true)
}

func (m *Manager) bootNewSync(ctx context.Context, v VM, restore bool, env map[string]string, syncNet bool) (VM, error) {
	defer m.bootIO()()
	t0 := time.Now()
	v, err := m.bootWithSync(ctx, v, restore, syncNet)
	if err != nil {
		return m.failSync(v.ID, err, syncNet)
	}
	t1 := time.Now()
	if syncNet {
		if v.SSHPolicyReady {
			err = m.syncNet()
		} else {
			err = m.syncRoutes()
		}
		if err != nil {
			return m.failSync(v.ID, err, true)
		}
	}
	log.Printf("%s: boot=%dms net=%dms", v.ID, t1.Sub(t0).Milliseconds(), time.Since(t1).Milliseconds())
	return m.initGuest(ctx, v, env, true, restore)
}

const guestInitFactor = 2 // whole guest init, in units of bootTimeoutS

func (m *Manager) fail(id string, err error) (VM, error) {
	return m.failSync(id, err, true)
}

func (m *Manager) failSync(id string, err error, syncNet bool) (VM, error) {
	log.Printf("%s: %v", id, err)
	v, _ := m.get(id)
	m.killWait(id, v.PID)
	delVMNet(m.run, v)
	_, uerr := m.update(id, "failed", func(v *VM) { v.State, v.Error, v.PID = Error, err.Error(), 0 })
	if !syncNet {
		return VM{}, errors.Join(err, uerr)
	}
	return VM{}, errors.Join(err, uerr, m.syncNet())
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
	return m.bootWithSync(ctx, v, restore, true)
}

func (m *Manager) bootWithSync(ctx context.Context, v VM, restore, syncBefore bool) (VM, error) {
	var err error
	if syncBefore {
		v, err = m.resetSSHBoot(v.ID)
		if err != nil {
			return v, err
		}
		if err := m.syncNet(); err != nil { // anti-spoof and quarantine rules are in force before its first frame
			return v, err
		}
	}

	uid, gid := m.ids(v)
	m.markConntrackDirty(v.IP, v.IPv6, v.PublicIP)
	if err := addVMNet(m.cfg, m.run, v, uid, gid); err != nil {
		return v, err
	}
	root := m.root(v.ID)
	os.Remove(filepath.Join(root, apiSock))
	os.Remove(filepath.Join(root, vsockUDS))
	// A new VMM: conns to the last one are stale.
	closeAgentConns(filepath.Join(root, vsockUDS))
	if !restore { // a cold boot has no memory base
		os.Remove(filepath.Join(root, vmstate))
		os.Remove(filepath.Join(root, memFile))
	}
	m.rmVCPUCgroup(v.ID) // the last run's: the jailer's cgroup goes back to a plain domain
	t0 := time.Now()
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
	// Bounded like the guest agent, not by a few seconds: binding the socket is an inode
	// create in the state dir, which a btrfs commit with GiBs in writeback (a snapshot
	// just dumped) stalls for longer than that. A dead firecracker still fails at once.
	err = waitSock(filepath.Join(root, apiSock), done, time.Duration(m.cfg.BootTimeoutS)*time.Second)
	t1 := time.Now()
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
	// The socket wait is where a btrfs commit behind a dump's writeback shows (see writeback.go).
	log.Printf("%s: boot vmm+sock=%dms load=%dms", v.ID, t1.Sub(t0).Milliseconds(), time.Since(t1).Milliseconds())
	if err := m.splitVCPUs(v, pid); err != nil {
		log.Printf("%s: vCPU threads stay with the VMM thread: %v", v.ID, err)
	}
	return m.update(v.ID, "", func(x *VM) {
		x.State, x.HasSnapshot, x.Error, x.DiffOK = Running, false, "", m.cfg.DiffSnapshots
		if m.cfg.standaloneReady(*x) {
			x.SSHPolicyReady = true
		}
		x.ExpiresAt = deadline(m.now(), x.TTLS)
	})
}

// initGuest waits for the agent, then (new VMs) sets identity, (restores)
// fixes the clock, and pushes env. An image without an agent still runs:
// agent_ready stays false.
func (m *Manager) initGuest(ctx context.Context, v VM, env map[string]string, identity, restored bool) (VM, error) {
	// The guest is untrusted and may answer health, then hang on the next op.
	ctx, cancel := context.WithTimeout(ctx, guestInitFactor*time.Duration(m.cfg.BootTimeoutS)*time.Second)
	defer cancel()
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
		if !m.cfg.standaloneReady(v) && v.Team != warmTeam {
			id["defer_ssh_restart"] = true
		}
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
	var ae *agentErr
	// In-process in the agent: `exec date` was 150 ms of cold pages per restore.
	_, err := agentCall(ctx, sock, map[string]any{"op": "set_clock", "unix_ns": m.now().UnixNano()})
	if !errors.As(err, &ae) {
		return
	}
	a, err := dialAgent(ctx, sock) // older agents: no "set_clock" op
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
	return m.stop(ctx, id, reason, false)
}

// stop snapshots a running VM (its next start resumes it) or, with force, kills it
// at once: no guest sync, no snapshot, so the next start cold-boots from its disk.
func (m *Manager) stop(ctx context.Context, id, reason string, force bool) (VM, error) {
	return m.stopOpts(ctx, id, reason, force, true, false)
}

func (m *Manager) stopOpts(ctx context.Context, id, reason string, force, bgSync, resumeOnBoot bool) (VM, error) {
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil || v.State == Stopped {
		return v, err
	}
	if v.State != Running {
		return v, errf(http.StatusConflict, "vm is %s", v.State)
	}
	originalSSH := v.clone()
	if v, err = m.update(id, reason, func(x *VM) { x.State = Stopping }); err != nil {
		return v, err
	}
	if err := m.syncQuarantine(originalSSH); err != nil {
		return m.fail(id, err)
	}
	root := m.root(id)
	t0 := time.Now()
	snap := false
	if !force {
		err = m.checkFloor(uint64(v.MemMiB) << 20) // a diff is usually far smaller; plan for a full one
		if m.cfg.HugePages {
			err = errHugePages // a memory file it could not restore: shut down instead
		}
		dumped := false // tried and failed, as opposed to refused by the floor (which stops cold)
		if err == nil {
			v, err = m.takeSnapshot(ctx, v, false) // syncSnapshot flushes mem and vmstate behind the call
			dumped = true
		}
		snap = err == nil
		if !snap && dumped && reason != shutdownReason && reason != "ttl" {
			// Someone can retry (the caller, or the next idle tick): keep the guest and
			// its memory rather than lose them to a full disk or team quota (EDQUOT).
			// dumpSnapshot left it paused and its files dropped; failSnapshot makes the
			// next one Full.
			log.Printf("%s: stop (%s): memory not saved, VM kept running: %v", id, reason, err)
			rerr := newFC(filepath.Join(root, apiSock)).resume(ctx)
			if rerr == nil {
				v, rerr = m.update(id, "", func(x *VM) { x.State = Running })
				if rerr == nil {
					rerr = m.restoreSSHAfterPause(context.WithoutCancel(ctx), originalSSH)
				}
			}
			if rerr != nil {
				log.Printf("%s: resume after a failed stop: %v", id, rerr)
			}
			return v, errors.Join(errf(http.StatusInsufficientStorage, "stop: memory not saved (free disk space and retry, or stop with force to discard its memory): %v", err), rerr)
		}
		if !snap {
			// No snapshot and no retry (below the disk floor, huge pages, boatd exiting, a TTL):
			// let the guest flush its disk and reboot (= exit) instead.
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
	}
	t1 := time.Now()
	m.killWait(id, v.PID)
	m.dumpDone(id) // its cgroup is gone with the VMM; no setMemLimits follows a stop
	t2 := time.Now()
	if snap {
		snap = os.Rename(filepath.Join(root, snapState), filepath.Join(root, vmstate)) == nil
	}
	if !snap {
		for _, f := range []string{memFile, vmstate, snapState, snapDiff, memNew} {
			os.Remove(filepath.Join(root, f))
		}
	}
	t3 := time.Now()
	delVMNet(m.run, v)
	v, err = m.update(id, reason, func(x *VM) {
		x.State, x.PID, x.HasSnapshot, x.SnapSynced, x.AgentReady, x.ExpiresAt = Stopped, 0, snap, false, false, nil
		if resumeOnBoot {
			x.ResumeOnBoot = true
		}
	})
	t4 := time.Now()
	err = errors.Join(err, m.syncNet())
	log.Printf("%s: stop (%s) snapshot=%dms kill=%dms flush=%dms state=%dms net=%dms", id, map[bool]string{true: "force", false: reason}[force],
		t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), t3.Sub(t2).Milliseconds(), t4.Sub(t3).Milliseconds(), time.Since(t4).Milliseconds())
	if snap && bgSync {
		m.startSyncSnapshot(id)
	}
	return v, err
}

type StartReq struct {
	Env      map[string]string `json:"env"`
	FailFast bool              `json:"fail_fast"`
	TTLS     *int              `json:"ttl_s"`
	// FreshBoot discards the stop snapshot (vmstate + mem) of a stopped VM and cold
	// boots from its disk, never restoring the old memory. The API sets it on a start
	// that must not see a previous run's secrets in guest RAM (a no_env cold start).
	FreshBoot bool `json:"fresh_boot"`
}

type ForkReq struct {
	Name           string            `json:"name"`
	Team           string            `json:"team"`
	FailFast       bool              `json:"fail_fast"`
	Labels         map[string]string `json:"labels"` // absent: only the parent's boat.* labels
	TTLS           *int              `json:"ttl_s"`
	ReprovisionSSH bool              `json:"reprovision_ssh"`
}

func (m *Manager) Start(ctx context.Context, id string, env map[string]string) (VM, error) {
	return m.startVM(ctx, id, env, nil, false)
}

func (m *Manager) startVM(ctx context.Context, id string, env map[string]string, ttlS *int, freshBoot bool) (VM, error) {
	if err := validEnv(env); err != nil {
		return VM{}, err
	}
	if ttlS != nil && (*ttlS < 0 || *ttlS > maxPatchS) {
		return VM{}, errf(400, "ttl_s must be 0 (no auto-stop) to %d", maxPatchS)
	}
	unlock := m.lock(id)
	defer unlock()
	if m.tombstoned(id) {
		return VM{}, errf(http.StatusConflict, "vm is being deleted: its removal failed and is retried in the background")
	}
	m.mu.Lock()
	old, ok := m.vms[id]
	var err error
	var finish func() error // the deferred vm.json write, run after m.mu is released
	switch {
	case !ok:
		err = errNotFound
	case old.PendingCopy:
		err = errf(http.StatusConflict, "copy is awaiting authorization")
	case freshBoot && old.State != Stopped && old.State != Error:
		err = errf(http.StatusConflict, "fresh_boot needs a stopped vm (it is %s): stop it first", old.State)
	case old.State == Running:
		if ttlS != nil {
			nv := old.clone()
			nv.TTLS = *ttlS
			nv.ExpiresAt = deadline(m.now(), nv.TTLS)
			nv, finish := m.commitDeferLocked(old, nv, "")
			m.mu.Unlock()
			if err = finish(); err != nil {
				return VM{}, err
			}
			return nv.clone(), nil
		}
		m.mu.Unlock()
		return old.clone(), nil
	case old.State != Stopped && old.State != Error:
		err = errf(http.StatusConflict, "vm is %s", old.State)
	default:
		if err = m.quotaLocked(old.Team, Size{old.VCPU, old.MemMiB, old.DiskGiB}, id); err == nil {
			nv := old.clone()
			nv.State = Starting
			if ttlS != nil {
				nv.TTLS = *ttlS
			}
			if freshBoot {
				nv.HasSnapshot, nv.SnapSynced = false, false
			}
			nv, finish = m.commitDeferLocked(old, nv, "")
		}
	}
	m.mu.Unlock()
	if finish != nil {
		err = finish()
	}
	if err != nil {
		return VM{}, err
	}
	v, _ := m.get(id)
	root := m.root(id)
	if freshBoot {
		// vmstate first: the pair is only a snapshot while both exist. A removal that
		// fails (other than "already gone") fails the start: booting would restore the
		// memory fresh_boot promises to discard.
		if derr := m.removeMemory(root); derr != nil {
			return m.fail(id, derr)
		}
	}
	restore := !freshBoot && exists(filepath.Join(root, vmstate)) && exists(filepath.Join(root, memFile))
	defer m.bootIO()()
	v, err = m.boot(ctx, v, restore)
	if err != nil && restore {
		log.Printf("%s: restore failed, cold booting: %v", id, err)
		os.Remove(filepath.Join(root, vmstate))
		os.Remove(filepath.Join(root, memFile))
		restore = false
		v, err = m.boot(ctx, v, false)
	}
	if err == nil {
		if v.SSHPolicyReady {
			err = m.syncNet()
		} else {
			err = m.syncRoutes()
		}
	}
	if err != nil {
		return m.fail(id, err)
	}
	return m.initGuest(ctx, v, env, false, restore)
}

// Fork clones a running (paused for the snapshot) or stopped VM. labels nil:
// the child copies the parent's; otherwise it gets exactly these.
func (m *Manager) Fork(ctx context.Context, id, name, team string, labels map[string]string) (VM, error) {
	return m.fork(ctx, id, name, team, labels, false)
}

func (m *Manager) fork(ctx context.Context, id, name, team string, labels map[string]string, pending bool) (VM, error) {
	return m.forkOpts(ctx, id, name, team, labels, nil, pending, false)
}

func (m *Manager) forkOpts(ctx context.Context, id, name, team string, labels map[string]string, ttlS *int, pending, reprovisionSSH bool) (VM, error) {
	if name != "" && !reName.MatchString(name) {
		return VM{}, errf(400, "invalid name")
	}
	if ttlS != nil && (*ttlS < 0 || *ttlS > maxPatchS) {
		return VM{}, errf(400, "ttl_s must be 0 (no auto-stop) to %d", maxPatchS)
	}
	if len(labels) > 64 {
		return VM{}, errf(400, "too many labels")
	}
	for k, v := range labels {
		if !reLabel.MatchString(k) || len(v) > 256 {
			return VM{}, errf(400, "invalid label %q", k)
		}
	}
	if m.cfg.HugePages {
		return VM{}, errHugePages
	}
	// Phase 1 (no parent lock): size the child from the parent as it is now. The lock
	// is not held across alloc, whose publishPending fsyncs, and not even taken to read
	// the parent: a second fork of the same parent allocates while the first is in
	// freeze. Phase 2 re-validates under the lock.
	p, base, err := m.forkParent(id, team, name, labels, ttlS, pending)
	if err != nil {
		return VM{}, err
	}
	t0 := time.Now()
	if err = m.checkFloor(uint64(p.MemMiB) << 20); err != nil {
		return VM{}, err
	}
	c, err := m.alloc(base, Forking)
	if err != nil {
		return VM{}, err
	}
	// Phase 2 (parent lock, then child lock: the order every fork takes): the parent
	// must be what the child was sized from, else the allocation is rolled back.
	unlockP := m.lock(id)
	p2, perr := m.get(id)
	if perr != nil || !sameForkParent(p, p2) {
		if perr == nil {
			perr = errf(http.StatusConflict, "vm changed while it was being forked")
		}
		_, derr := m.deleteVM(context.WithoutCancel(ctx), c.ID, false)
		unlockP()
		return VM{}, errors.Join(perr, derr)
	}
	p = p2 // freeze the parent as it is now (its snapshot files may have changed)
	unlockC := m.lock(c.ID)
	defer unlockC()
	if m.freezeGate != nil {
		m.freezeGate(id)
	}
	t1 := time.Now()
	withMem, err := m.freeze(ctx, p, m.root(c.ID), reprovisionSSH)
	unlockP()
	t2 := time.Now()
	if err == nil {
		err = m.finishDisk(c)
	}
	if err != nil {
		return m.fail(c.ID, err)
	}
	if pending {
		c, err = m.update(c.ID, "", func(x *VM) { x.CopyReady, x.HasSnapshot = true, withMem })
		if err == nil && withMem {
			m.startSyncSnapshot(c.ID)
		}
		return c, err
	}
	t3, cid := time.Now(), c.ID
	c, err = m.bootNew(ctx, c, withMem, nil)
	log.Printf("%s: fork of %s alloc=%dms freeze=%dms disk=%dms boot+init=%dms", cid, p.ID, t1.Sub(t0).Milliseconds(),
		t2.Sub(t1).Milliseconds(), t3.Sub(t2).Milliseconds(), time.Since(t3).Milliseconds())
	return c, err
}

// defaultForkTTLS is the child's ttl_s when a fork request omits it.
const defaultForkTTLS = 3600

// forkParent reads the parent (without its lifecycle lock: a fork in freeze holds it)
// and returns it with the child's template. fork re-validates under the lock.
func (m *Manager) forkParent(id, team, name string, labels map[string]string, ttlS *int, pending bool) (VM, VM, error) {
	p, err := m.get(id)
	if err == nil && team != "" && p.Team != team {
		err = errNotFound
	}
	if err == nil && (p.PendingCopy || (p.State != Running && p.State != Stopped)) {
		err = errf(http.StatusConflict, "vm is %s", p.State)
	}
	if err != nil {
		return VM{}, VM{}, err
	}
	base := VM{PendingCopy: pending, Name: name, Team: p.Team, Image: p.Image, Size: p.Size, VCPU: p.VCPU, MemMiB: p.MemMiB,
		DiskGiB: p.DiskGiB, Labels: reservedLabels(p.Labels), TTLS: p.TTLS, IdleStopS: p.IdleStopS, ParentID: p.ID}
	if labels != nil {
		if len(labels) == 0 {
			base.Labels = nil
		} else {
			base.Labels = make(map[string]string, len(labels))
			for k, v := range labels {
				base.Labels[k] = v
			}
		}
	}
	// An omitted ttl_s gives the child the default hour, not the parent's TTL (a parent
	// with auto-stop off must not make a child that never stops); an explicit one,
	// including 0 for no auto-stop, wins.
	base.TTLS = defaultForkTTLS
	if ttlS != nil {
		base.TTLS = *ttlS
	}
	return p, base, nil
}

// sameForkParent: is b still the VM a fork sized its child from (same run state,
// process and shape)?
func sameForkParent(a, b VM) bool {
	return a.State == b.State && a.PID == b.PID && !b.PendingCopy && a.Team == b.Team && a.Image == b.Image &&
		a.Size == b.Size && a.VCPU == b.VCPU && a.MemMiB == b.MemMiB && a.DiskGiB == b.DiskGiB &&
		a.TTLS == b.TTLS && a.IdleStopS == b.IdleStopS && maps.Equal(a.Labels, b.Labels)
}

// releaseCopy is internal: the API has durably revoked the captured credentials.
//
// freshBoot discards the child's copy of the parent's memory (vmstate + mem) and cold
// boots it from the reflinked disk, so nothing the parent held in RAM (team secrets of
// a run the child must not inherit) reaches the child. The discard happens before the
// copy is authorized: a failed removal leaves it pending.
func (m *Manager) releaseCopy(ctx context.Context, id string, env map[string]string, freshBoot bool) (VM, error) {
	if err := validEnv(env); err != nil {
		return VM{}, err
	}
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil {
		return v, err
	}
	if !v.PendingCopy {
		return v, nil
	} // an acknowledged or lost release is never replayed
	if !v.CopyReady || v.State != Forking {
		return VM{}, errf(409, "copy is incomplete")
	}
	m.mu.Lock()
	err = m.quotaLocked(v.Team, Size{v.VCPU, v.MemMiB, v.DiskGiB}, v.ID)
	m.mu.Unlock()
	if err != nil {
		return VM{}, err
	}
	restore := v.HasSnapshot
	if freshBoot {
		if err := m.discardMemory(v); err != nil {
			return VM{}, err
		}
		restore = false
	}
	v, err = m.update(id, "", func(x *VM) {
		x.PendingCopy, x.CopyReady = false, false
		if freshBoot {
			x.HasSnapshot, x.SnapSynced = false, false
		}
	})
	if err != nil {
		return VM{}, err
	}
	return m.bootNew(ctx, v, restore, env)
}

// discardMemory removes a VM's memory snapshot (vmstate first: the pair is only a
// snapshot while both exist) and grows the disk, which a cold boot is allowed to.
func (m *Manager) discardMemory(v VM) error {
	if err := m.removeMemory(m.root(v.ID)); err != nil {
		return err
	}
	return m.finishDisk(v)
}

// removeMemory deletes root's vmstate then mem (the pair is only a snapshot while both
// exist). A file that is already gone is fine; any other failure is returned.
func (m *Manager) removeMemory(root string) error {
	rm := m.removeFile
	if rm == nil {
		rm = os.Remove
	}
	for _, f := range []string{vmstate, memFile} {
		if err := rm(filepath.Join(root, f)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("discard %s: %w", f, err)
		}
	}
	return nil
}

func (m *Manager) Delete(ctx context.Context, id string) (VM, error) {
	return m.deleteVM(ctx, id, true)
}

func (m *Manager) deleteVM(ctx context.Context, id string, syncNet bool) (VM, error) {
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil {
		return v, err
	}
	// Normally the tombstone is durable before teardown, so a crash makes the
	// next boatd finish the delete. ENOSPC alone permits teardown first: deleting
	// must still reclaim space when the host cannot allocate even a marker.
	err = m.writeTombstone(id, v)
	if err != nil && !errors.Is(err, syscall.ENOSPC) {
		return v, fmt.Errorf("delete: tombstone: %w", err)
	}
	m.killWait(id, v.PID)
	delVMNet(m.run, v)
	delNeighbours(m.cfg, m.run, v)
	m.flushConntrack(v.IP, v.IPv6, v.PublicIP)
	if m.cfg.CgroupParent != "" {
		m.rmVCPUCgroup(id)
		os.Remove(filepath.Join(cgroupRoot, m.cfg.CgroupParent, id)) // the jailer's (empty) cgroup
	}
	m.updateInMemory(id, "deleted", func(x *VM) { x.State, x.PID = Stopped, 0 }) // closes the billing interval without a redundant vm.json fsync
	for _, s := range m.Snapshots(id) {
		m.DeleteSnapshot(id, s.ID)
	}
	if errors.Is(err, syscall.ENOSPC) {
		// Keep vm.json until the retry: it is still the recovery record while
		// there is no durable tombstone. Unlink payloads before syncfs so btrfs
		// commits delayed refs and releases their extents before the retry.
		for _, f := range []string{vmstate, memFile, diskFile, kernFile} {
			if uerr := os.Remove(filepath.Join(m.root(id), f)); uerr != nil && !os.IsNotExist(uerr) {
				log.Printf("%s: delete: reclaiming %s before tombstone retry: %v", id, f, uerr)
			}
		}
		flush := m.syncFSFn
		if flush == nil {
			flush = syncFS
		}
		if ferr := flush(m.cfg.StateDir); ferr != nil {
			log.Printf("%s: delete: syncing filesystem before tombstone retry: %v", id, ferr)
		}
		if err = m.writeTombstone(id, v); err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				return v, fmt.Errorf("delete: tombstone retry: %w", err)
			}
			log.Printf("WARNING: %s: delete: tombstone still has no space after reclaim; proceeding without a durable marker: %v", id, err)
		}
	}
	if err := m.removeVMState(id); err != nil {
		return m.deleteFailed(id, v, err)
	}
	m.clearTombstone(id)
	m.forgetDeleted(v)
	var nerr error
	if syncNet {
		nerr = m.syncNet()
	}
	v.State = "deleted"
	return v, nerr
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
		var finish func() error
		nv, finish = m.commitDeferLocked(old, nv, "")
		m.mu.Unlock()
		err = finish() // outside m.mu: behind another write of this VM's vm.json, not every VM
	} else {
		m.mu.Unlock()
	}
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
	for _, p := range m.cfg.ReservedPorts {
		used[p] = true
	}
	if m.cfg.HostHTTPSPort > 0 {
		used[m.cfg.HostHTTPSPort] = true
	}
	if m.cfg.AIProxyPort > 0 {
		used[m.cfg.AIProxyPort] = true
	}
	probe := m.portProbe
	if probe == nil {
		probe = tcpPortAvailable
	}
	for h := m.cfg.PortMin; h <= m.cfg.PortMax; h++ {
		if !used[h] && probe(h) {
			return h, nil
		}
	}
	return 0, errf(http.StatusServiceUnavailable, "no free host ports")
}

// tcpPortAvailable probes whether TCP port h can be bound locally without
// colliding with an existing host service on wildcard or loopback addresses.
func tcpPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	ln4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln4.Close()
	ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err == nil {
		ln6.Close()
	} else if errors.Is(err, syscall.EADDRINUSE) {
		return false
	}
	return true
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

// usageCacheTTL bounds how stale a Usage() report may be (Until, the running VMs'
// and snapshots' open intervals, the storage walk). Reports are also dropped the
// moment the ledger gets a record or a VM or snapshot is created, deleted or changes
// state, so a poller never sees a closed interval missing; the TTL only covers
// time passing and file sizes growing.
var usageCacheTTL = 5 * time.Second

// usageSinceGranularity: `since` of a usage query is rounded down to it.
const usageSinceGranularity = time.Minute

// Usage returns the usage report from since. Identical calls within usageCacheTTL
// (and no change in between) share one assembly: one ledger read, one report.
//
// since is rounded down to the minute (usageSinceGranularity), so a poller whose
// window rolls forward by the second still hits one entry per minute; the report's
// `since` is the rounded value.
func (m *Manager) Usage(since time.Time) (usageReport, error) {
	return m.usageCache.get(m, since.Truncate(usageSinceGranularity))
}

// usageGens identifies everything a report is assembled from, apart from the clock.
func (m *Manager) usageGens() usageGens {
	m.mu.Lock()
	defer m.mu.Unlock()
	return usageGens{m.storageGen, m.stateGen, m.usage.gen.Load(), len(m.vms), len(m.snaps)}
}

// buildUsage assembles the report: it re-reads and re-summarises the ledger.
func (m *Manager) buildUsage(since time.Time) (usageReport, error) {
	recs, aggs, err := m.usage.readAll(since)
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
	applyAggs(&rep, aggs, since)
	st, disk := m.storage()
	rep.Disk, rep.Storage = &disk, []teamStorage{}
	for _, t := range st {
		rep.Storage = append(rep.Storage, *t)
	}
	sort.Slice(rep.Storage, func(i, j int) bool { return rep.Storage[i].Team < rep.Storage[j].Team })
	return rep, nil
}

// reservedLabels returns a new map of only the reserved labels (keys starting
// with "boat."); nil when there are none. User labels never pass to a fork.
func reservedLabels(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if strings.HasPrefix(k, "boat.") {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}
