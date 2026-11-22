package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const sshPolicyHelper = "/usr/local/libexec/boat-ssh-authorize"

type sshPolicyConfig struct {
	Version    int      `json:"version"`
	URL        string   `json:"url"`
	Token      string   `json:"token"`
	VMID       string   `json:"vm_id"`
	TeamID     string   `json:"team_id"`
	Generation int64    `json:"generation"`
	BootEpoch  string   `json:"boot_epoch"`
	Users      []string `json:"users"`
}
type sshPolicyRequest struct {
	Version   int             `json:"version"`
	BootEpoch string          `json:"boot_epoch"`
	Config    sshPolicyConfig `json:"config"`
	HelperB64 string          `json:"helper_b64"`
}
type sshPolicyReply struct {
	Version         int    `json:"version"`
	ConfigDigest    string `json:"config_digest"`
	BootEpoch       string `json:"boot_epoch"`
	Readiness       bool   `json:"readiness"`
	VMID            string `json:"vm_id,omitempty"`
	Team            string `json:"team,omitempty"`
	State           State  `json:"state,omitempty"`
	PendingCopy     bool   `json:"pending_copy,omitempty"`
	SSHPolicyReady  bool   `json:"ssh_policy_ready,omitempty"`
	SSHPolicyDigest string `json:"ssh_policy_digest,omitempty"`
}

type sshPolicyGuestProof struct {
	Version      int    `json:"version"`
	ConfigDigest string `json:"config_digest"`
	BootEpoch    string `json:"boot_epoch"`
	Readiness    bool   `json:"readiness"`
}

type sshPolicyBatchItem struct {
	ID string `json:"id"`
	sshPolicyRequest
}

type sshPolicyBatchRequest struct {
	Items []sshPolicyBatchItem `json:"items"`
}

type sshPolicyBatchReply struct {
	Results map[string]sshPolicyReply `json:"results"`
	Errors  map[string]string         `json:"errors"`
}

func sshNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (m *Manager) newSSHEpoch() (string, error) {
	nonce, err := sshNonce()
	if err != nil {
		return "", err
	}
	return m.incarnation + nonce, nil
}
func (m *Manager) resetSSHBoot(id string) (VM, error) {
	epoch, err := m.newSSHEpoch()
	if err != nil {
		return VM{}, err
	}
	return m.update(id, "", func(v *VM) {
		v.SSHBootEpoch = epoch
		v.SSHPolicyReady = false
		v.SSHPolicyDigest = ""
		v.sshConfig = nil
		v.sshHelperDigest = ""
		v.sshPauseFenced = false
	})
}

func (m *Manager) resetSSHBootBatch(vms []VM) ([]VM, error) {
	out := make([]VM, len(vms))
	for i, v := range vms {
		nv, err := m.resetSSHBoot(v.ID)
		if err != nil {
			return nil, err
		}
		out[i] = nv
	}
	return out, nil
}
func (v VM) sshNetworkReady() bool {
	return v.State == Running && !v.PendingCopy && v.SSHPolicyReady && v.SSHBootEpoch != ""
}

func canonicalSSHConfig(c sshPolicyConfig) ([]byte, string, error) {
	// Map keys are sorted by encoding/json, matching Python sort_keys separators.
	b, err := json.Marshal(map[string]any{"version": c.Version, "url": c.URL, "token": c.Token, "vm_id": c.VMID, "team_id": c.TeamID, "generation": c.Generation, "boot_epoch": c.BootEpoch, "users": c.Users})
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), err
}
func sameSSHBoot(a, b VM) bool {
	return a.ID == b.ID && a.Team == b.Team && a.PID == b.PID && a.SSHBootEpoch == b.SSHBootEpoch && b.State == Running && !b.PendingCopy
}

func validateSSHPolicyReq(v VM, r sshPolicyRequest) (helper, config []byte, digest string, err error) {
	c := r.Config
	u, uerr := url.Parse(c.URL)
	if r.Version != 1 || c.Version != 1 || r.BootEpoch == "" || r.BootEpoch != v.SSHBootEpoch || c.BootEpoch != v.SSHBootEpoch || c.VMID != v.ID || c.TeamID != v.Team {
		return nil, nil, "", errf(409, "SSH policy boot identity mismatch")
	}
	if uerr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/v1/vms/"+v.ID+"/ssh/authorize" || c.Token == "" || c.Generation < 0 || c.Generation > 1<<53-1 || len(c.Users) == 0 || len(c.Users) > 2 {
		return nil, nil, "", errf(400, "invalid SSH policy config")
	}
	if len(c.Users) == 2 && c.Users[0] == c.Users[1] {
		return nil, nil, "", errf(400, "duplicate SSH policy user")
	}
	for _, user := range c.Users {
		if user != "dev" && user != "user" {
			return nil, nil, "", errf(400, "invalid SSH policy user")
		}
	}
	helper, err = base64.StdEncoding.DecodeString(r.HelperB64)
	if err != nil || len(helper) == 0 || len(helper) > 512<<10 {
		return nil, nil, "", errf(400, "invalid SSH policy helper")
	}
	if v.State != Running || v.PendingCopy || v.PID <= 0 || !v.AgentReady {
		return nil, nil, "", errf(503, "SSH policy guest unready")
	}
	config, digest, err = canonicalSSHConfig(c)
	if len(config) > 32768 {
		return nil, nil, "", errf(400, "SSH policy config too large")
	}
	for _, b := range config {
		if b > 127 {
			return nil, nil, "", errf(400, "SSH policy config must be ASCII")
		}
	}
	if err != nil {
		return nil, nil, "", err
	}
	return helper, config, digest, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func canRotateSSHToken(oldRaw, newRaw []byte) bool {
	if len(oldRaw) == 0 || len(newRaw) == 0 {
		return false
	}
	var oldCfg, newCfg sshPolicyConfig
	if json.Unmarshal(oldRaw, &oldCfg) != nil || json.Unmarshal(newRaw, &newCfg) != nil {
		return false
	}
	if oldCfg.Version != 1 || newCfg.Version != 1 ||
		oldCfg.VMID != newCfg.VMID ||
		oldCfg.TeamID != newCfg.TeamID ||
		oldCfg.BootEpoch != newCfg.BootEpoch ||
		oldCfg.URL != newCfg.URL ||
		!slices.Equal(oldCfg.Users, newCfg.Users) {
		return false
	}
	return oldCfg.Token != newCfg.Token || oldCfg.Generation != newCfg.Generation
}

func helperSupportsRotateToken(helper []byte) bool {
	if bytes.HasPrefix(helper, []byte("#!")) || bytes.Contains(helper, []byte("ssh-authorize")) {
		return bytes.Contains(helper, []byte("rotate-token"))
	}
	return true
}

func containsUnsupportedTokenMsg(s string) bool {
	low := strings.ToLower(s)
	for _, needle := range []string{"unknown", "usage", "unsupported", "unrecognized", "invalid choice", "arguments"} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

func isUnsupportedRotateToken(err error) bool {
	var execErr *sshPolicyExecErr
	if errors.As(err, &execErr) && execErr.mode == "rotate-token" && !execErr.timedOut {
		if execErr.exitCode == 2 || containsUnsupportedTokenMsg(execErr.stderr) {
			return true
		}
	}
	var ae *agentErr
	if errors.As(err, &ae) && containsUnsupportedTokenMsg(ae.msg) {
		return true
	}
	return false
}

func helperSupportsFastInstall(helper []byte) bool {
	return bytes.Contains(helper, []byte("install-and-verify"))
}

func (m *Manager) committedSSHPolicyReply(expected VM, digest string) (sshPolicyReply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, ok := m.vms[expected.ID]
	if !ok || !sameSSHBoot(expected, now) || !now.sshNetworkReady() || now.sshPauseFenced || now.SSHPolicyDigest != digest {
		return sshPolicyReply{}, false
	}
	return sshPolicyReply{
		Version:         1,
		ConfigDigest:    digest,
		BootEpoch:       now.SSHBootEpoch,
		Readiness:       true,
		VMID:            now.ID,
		Team:            now.Team,
		State:           now.State,
		PendingCopy:     now.PendingCopy,
		SSHPolicyReady:  now.SSHPolicyReady,
		SSHPolicyDigest: now.SSHPolicyDigest,
	}, true
}

func (m *Manager) installSSHPolicyGuest(ctx context.Context, v VM, helper, config []byte, digest, helperDigest string) error {
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	check := func() error {
		now, e := m.get(v.ID)
		if e != nil {
			return e
		}
		if !sameSSHBoot(v, now) {
			return errf(409, "SSH boot changed during installation")
		}
		return nil
	}
	err := check()
	if err == nil {
		err = trustedRootAgent(ctx, sock)
	}
	if err == nil {
		err = check()
	}
	if err != nil {
		return err
	}
	if helperSupportsFastInstall(helper) {
		err = runSSHPolicyArgsOpt(ctx, sock, []string{"install-and-verify", "--expect-helper-sha256", helperDigest}, false, config, digest, v.SSHBootEpoch, check)
		if err != nil && ctx.Err() == nil && check() == nil {
			_, err = agentCall(ctx, sock, map[string]any{"op": "put_file", "owner": "root", "path": sshPolicyHelper, "mode": "0555", "offset": 0, "last": true, "data_b64": base64.StdEncoding.EncodeToString(helper)})
			if err == nil {
				err = check()
			}
			if err == nil {
				err = runSSHPolicyArgsOpt(ctx, sock, []string{"install-and-verify"}, false, config, digest, v.SSHBootEpoch, check)
			}
		}
		if err == nil {
			err = check()
		}
	} else {
		_, err = agentCall(ctx, sock, map[string]any{"op": "put_file", "owner": "root", "path": sshPolicyHelper, "mode": "0555", "offset": 0, "last": true, "data_b64": base64.StdEncoding.EncodeToString(helper)})
		if err == nil {
			err = check()
		}
		if err == nil {
			err = runSSHPolicyChecked(ctx, sock, "install", config, digest, v.SSHBootEpoch, check)
		}
		if err == nil {
			err = check()
		}
		if err == nil {
			err = runSSHPolicyChecked(ctx, sock, "verify", config, digest, v.SSHBootEpoch, check)
		}
		if err == nil {
			err = check()
		}
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	current, ok := m.vms[v.ID]
	if !ok || !sameSSHBoot(v, current) {
		m.mu.Unlock()
		return errf(409, "SSH boot changed during installation")
	}
	ready := current.clone()
	ready.SSHPolicyReady = true
	ready.SSHPolicyDigest = digest
	ready.sshConfig = config
	ready.sshHelperDigest = helperDigest
	ready.sshPauseFenced = false
	err = m.commitLocked(current, ready, "")
	m.mu.Unlock()
	return err
}

func (m *Manager) rotateSSHTokenGuest(ctx context.Context, v VM, config []byte, digest, helperDigest string) (bool, error) {
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	check := func() error {
		now, e := m.get(v.ID)
		if e != nil {
			return e
		}
		if !sameSSHBoot(v, now) || !now.SSHPolicyReady || now.sshHelperDigest != helperDigest {
			return errf(409, "SSH boot changed during token rotation")
		}
		return nil
	}
	if err := check(); err != nil {
		return false, err
	}
	if err := trustedRootAgent(ctx, sock); err != nil {
		return false, err
	}
	if err := check(); err != nil {
		return false, err
	}
	if err := runSSHPolicyChecked(ctx, sock, "rotate-token", config, digest, v.SSHBootEpoch, check); err != nil {
		if isUnsupportedRotateToken(err) {
			return true, nil
		}
		return false, err
	}
	if err := check(); err != nil {
		return false, err
	}
	m.mu.Lock()
	current, ok := m.vms[v.ID]
	if !ok || !sameSSHBoot(v, current) || !current.SSHPolicyReady || current.sshHelperDigest != helperDigest {
		m.mu.Unlock()
		return false, errf(409, "SSH boot changed during token rotation")
	}
	ready := current.clone()
	ready.SSHPolicyReady = true
	ready.SSHPolicyDigest = digest
	ready.sshConfig = config
	ready.sshHelperDigest = helperDigest
	ready.sshPauseFenced = false
	err := m.commitLocked(current, ready, "")
	m.mu.Unlock()
	return false, err
}

func (m *Manager) installSSHPolicy(ctx context.Context, id string, r sshPolicyRequest) (sshPolicyReply, error) {
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil {
		return sshPolicyReply{}, err
	}
	helper, config, digest, err := validateSSHPolicyReq(v, r)
	if err != nil {
		return sshPolicyReply{}, err
	}
	helperDigest := sha256Hex(helper)

	// Fast O(1) no-op when this exact policy + helper is already verified and published on this boot.
	if v.SSHPolicyReady && !v.sshPauseFenced && !m.isSSHPausedInNft(v) && sameSSHBoot(v, v) && v.SSHPolicyDigest == digest && v.sshHelperDigest == helperDigest && m.isPublishedReadyInNft(v) {
		if rep, ok := m.committedSSHPolicyReply(v, digest); ok {
			return rep, nil
		}
	}

	// Non-disruptive live token rotation when only token/generation changed on the same boot + helper
	// (including running VMs transiently SSH-pause-fenced during fork/snapshot reprovisioning).
	if v.SSHPolicyReady && sameSSHBoot(v, v) && v.sshHelperDigest == helperDigest && m.isPublishedReadyInNft(v) &&
		canRotateSSHToken(v.sshConfig, config) && helperSupportsRotateToken(helper) {
		rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
		unsupported, rerr := m.rotateSSHTokenGuest(rctx, v, config, digest, helperDigest)
		rcancel()
		if rerr == nil && !unsupported {
			if v.sshPauseFenced || m.isSSHPausedInNft(v) {
				if cerr := m.syncClearSSHPause(v); cerr != nil {
					_, uerr := m.update(id, "", func(x *VM) {
						x.SSHPolicyReady = false
						x.SSHPolicyDigest = ""
						x.sshConfig = nil
						x.sshHelperDigest = ""
						x.sshPauseFenced = false
					})
					return sshPolicyReply{}, errors.Join(cerr, uerr, m.syncNet())
				}
			}
			rep, ok := m.committedSSHPolicyReply(v, digest)
			if !ok {
				_, uerr := m.update(id, "", func(x *VM) {
					x.SSHPolicyReady = false
					x.SSHPolicyDigest = ""
					x.sshConfig = nil
					x.sshHelperDigest = ""
					x.sshPauseFenced = false
				})
				return sshPolicyReply{}, errors.Join(errf(409, "SSH boot changed before publication"), uerr, m.syncNet())
			}
			return rep, nil
		}
		if !unsupported {
			_, uerr := m.update(id, "", func(x *VM) {
				x.SSHPolicyReady = false
				x.SSHPolicyDigest = ""
				x.sshConfig = nil
				x.sshHelperDigest = ""
				x.sshPauseFenced = false
			})
			return sshPolicyReply{}, errors.Join(rerr, uerr, m.syncNet())
		}
	}

	// Changing credentials fences existing ingress first, even on reinstall failure.
	// If this boot epoch is already quarantined in nftables and has never been ready,
	// skip the redundant pre-fence nft reload.
	alreadyFenced := !v.SSHPolicyReady && !v.sshPauseFenced && m.isQuarantinedInNft(v)
	if v.SSHPolicyReady || v.sshPauseFenced || v.SSHPolicyDigest != "" || v.sshConfig != nil || v.sshHelperDigest != "" {
		if _, err = m.update(id, "", func(x *VM) {
			x.SSHPolicyReady = false
			x.SSHPolicyDigest = ""
			x.sshConfig = nil
			x.sshHelperDigest = ""
			x.sshPauseFenced = false
		}); err != nil {
			return sshPolicyReply{}, err
		}
	}
	if !alreadyFenced {
		if err = m.syncNet(); err != nil {
			return sshPolicyReply{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = m.installSSHPolicyGuest(ctx, v, helper, config, digest, helperDigest); err != nil {
		return sshPolicyReply{}, err
	}
	if err = m.syncNet(); err != nil {
		_, uerr := m.update(id, "", func(x *VM) { x.SSHPolicyReady = false })
		return sshPolicyReply{}, errors.Join(err, uerr, m.syncNet())
	}
	rep, ok := m.committedSSHPolicyReply(v, digest)
	if !ok {
		return sshPolicyReply{}, errf(409, "SSH boot changed before publication")
	}
	return rep, nil
}

func (m *Manager) installSSHPolicyBatch(ctx context.Context, req sshPolicyBatchRequest) (sshPolicyBatchReply, error) {
	rep := sshPolicyBatchReply{Results: map[string]sshPolicyReply{}, Errors: map[string]string{}}
	if len(req.Items) == 0 {
		return rep, nil
	}
	if len(req.Items) > maxBatch {
		return sshPolicyBatchReply{}, errf(400, "batch items must be at most %d", maxBatch)
	}
	ids := make([]string, 0, len(req.Items))
	seen := make(map[string]bool, len(req.Items))
	for _, it := range req.Items {
		if it.ID == "" || seen[it.ID] {
			return sshPolicyBatchReply{}, errf(400, "duplicate or empty vm id in batch")
		}
		seen[it.ID] = true
		ids = append(ids, it.ID)
	}
	sort.Strings(ids)
	unlocks := make([]func(), 0, len(ids))
	for _, id := range ids {
		unlocks = append(unlocks, m.lock(id))
	}
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()

	type batchTarget struct {
		v            VM
		helper       []byte
		config       []byte
		digest       string
		helperDigest string
	}
	rotateTargets := make([]batchTarget, 0, len(req.Items))
	rawFull := make([]batchTarget, 0, len(req.Items))
	needPreFence := false
	for _, it := range req.Items {
		v, err := m.get(it.ID)
		if err != nil {
			rep.Errors[it.ID] = err.Error()
			continue
		}
		helper, config, digest, err := validateSSHPolicyReq(v, it.sshPolicyRequest)
		if err != nil {
			if !v.SSHPolicyReady && !m.isQuarantinedInNft(v) {
				needPreFence = true
			}
			rep.Errors[it.ID] = err.Error()
			continue
		}
		helperDigest := sha256Hex(helper)
		if v.SSHPolicyReady && !v.sshPauseFenced && !m.isSSHPausedInNft(v) && sameSSHBoot(v, v) && v.SSHPolicyDigest == digest && v.sshHelperDigest == helperDigest && m.isPublishedReadyInNft(v) {
			if committedRep, ok := m.committedSSHPolicyReply(v, digest); ok {
				rep.Results[v.ID] = committedRep
				continue
			}
		}
		if v.SSHPolicyReady && sameSSHBoot(v, v) && v.sshHelperDigest == helperDigest && m.isPublishedReadyInNft(v) &&
			canRotateSSHToken(v.sshConfig, config) && helperSupportsRotateToken(helper) {
			rotateTargets = append(rotateTargets, batchTarget{v: v, helper: helper, config: config, digest: digest, helperDigest: helperDigest})
			continue
		}
		rawFull = append(rawFull, batchTarget{v: v, helper: helper, config: config, digest: digest, helperDigest: helperDigest})
	}

	if len(rotateTargets) > 0 {
		var (
			rwg    sync.WaitGroup
			rmu    sync.Mutex
			fenced bool
		)
		for _, rt := range rotateTargets {
			rwg.Add(1)
			go func(t batchTarget) {
				defer rwg.Done()
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				unsupported, rerr := m.rotateSSHTokenGuest(rctx, t.v, t.config, t.digest, t.helperDigest)
				if rerr == nil && !unsupported && (t.v.sshPauseFenced || m.isSSHPausedInNft(t.v)) {
					rerr = m.syncClearSSHPause(t.v)
				}
				rmu.Lock()
				defer rmu.Unlock()
				if unsupported {
					rawFull = append(rawFull, t)
					return
				}
				if rerr != nil {
					_, uerr := m.update(t.v.ID, "", func(x *VM) {
						x.SSHPolicyReady = false
						x.SSHPolicyDigest = ""
						x.sshConfig = nil
						x.sshHelperDigest = ""
						x.sshPauseFenced = false
					})
					fenced = true
					rep.Errors[t.v.ID] = errors.Join(rerr, uerr).Error()
					return
				}
				committedRep, ok := m.committedSSHPolicyReply(t.v, t.digest)
				if !ok {
					_, _ = m.update(t.v.ID, "", func(x *VM) {
						x.SSHPolicyReady = false
						x.SSHPolicyDigest = ""
						x.sshConfig = nil
						x.sshHelperDigest = ""
						x.sshPauseFenced = false
					})
					fenced = true
					rep.Errors[t.v.ID] = "SSH boot changed before publication"
					return
				}
				rep.Results[t.v.ID] = committedRep
			}(rt)
		}
		rwg.Wait()
		if fenced {
			needPreFence = true
		}
	}

	targets := make([]batchTarget, 0, len(rawFull))
	for _, ft := range rawFull {
		v, err := m.get(ft.v.ID)
		if err != nil {
			rep.Errors[ft.v.ID] = err.Error()
			continue
		}
		alreadyFenced := !v.SSHPolicyReady && !v.sshPauseFenced && m.isQuarantinedInNft(v)
		if v.SSHPolicyReady || v.sshPauseFenced || v.SSHPolicyDigest != "" || v.sshConfig != nil || v.sshHelperDigest != "" {
			nv, uerr := m.update(v.ID, "", func(x *VM) {
				x.SSHPolicyReady = false
				x.SSHPolicyDigest = ""
				x.sshConfig = nil
				x.sshHelperDigest = ""
				x.sshPauseFenced = false
			})
			if uerr != nil {
				rep.Errors[v.ID] = uerr.Error()
				needPreFence = true
				continue
			}
			v = nv
		}
		if !alreadyFenced {
			needPreFence = true
		}
		ft.v = v
		targets = append(targets, ft)
	}
	if needPreFence {
		if err := m.syncNet(); err != nil {
			return sshPolicyBatchReply{}, err
		}
	}
	if len(targets) == 0 {
		return rep, nil
	}

	var (
		wg        sync.WaitGroup
		resMu     sync.Mutex
		committed = make([]batchTarget, 0, len(targets))
	)
	for _, t := range targets {
		wg.Add(1)
		go func(t batchTarget) {
			defer wg.Done()
			ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			err := m.installSSHPolicyGuest(ictx, t.v, t.helper, t.config, t.digest, t.helperDigest)
			resMu.Lock()
			defer resMu.Unlock()
			if err != nil {
				rep.Errors[t.v.ID] = err.Error()
				return
			}
			committed = append(committed, t)
		}(t)
	}
	wg.Wait()
	if len(committed) == 0 {
		return rep, nil
	}

	if err := m.syncNet(); err != nil {
		rerrs := []error{err}
		for _, t := range committed {
			_, uerr := m.update(t.v.ID, "", func(x *VM) { x.SSHPolicyReady = false })
			rerrs = append(rerrs, uerr)
		}
		rerrs = append(rerrs, m.syncNet())
		return sshPolicyBatchReply{}, errors.Join(rerrs...)
	}

	for _, t := range committed {
		committedRep, ok := m.committedSSHPolicyReply(t.v, t.digest)
		if !ok {
			rep.Errors[t.v.ID] = "SSH boot changed before publication"
			continue
		}
		rep.Results[t.v.ID] = committedRep
	}
	return rep, nil
}

// Require an explicit capability before launching a process: old agents ignore
// unknown exec fields and would otherwise inherit tenant loader variables.
func trustedRootAgent(ctx context.Context, sock string) error {
	raw, _, err := agentRaw(ctx, sock, map[string]string{"op": "health"})
	if err != nil {
		return err
	}
	var health struct {
		TrustedRootExec bool `json:"trusted_root_exec"`
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		return err
	}
	if !health.TrustedRootExec {
		return errf(503, "guest agent lacks trusted root execution")
	}
	return nil
}

// exec streams must be consumed completely; a successful exit alone is not proof
// that the expected policy and teardown were verified.
func runSSHPolicy(ctx context.Context, sock, mode string, config []byte, digest, epoch string, fences ...func() error) error {
	return runSSHPolicyOpt(ctx, sock, mode, true, config, digest, epoch, fences...)
}

func runSSHPolicyChecked(ctx context.Context, sock, mode string, config []byte, digest, epoch string, fences ...func() error) error {
	return runSSHPolicyOpt(ctx, sock, mode, false, config, digest, epoch, fences...)
}

type sshPolicyExecErr struct {
	mode     string
	exitCode int
	timedOut bool
	stderr   string
}

func (e *sshPolicyExecErr) Error() string {
	return fmt.Sprintf("SSH policy %s failed", e.mode)
}

func runSSHPolicyOpt(ctx context.Context, sock, mode string, checkAgent bool, config []byte, digest, epoch string, fences ...func() error) error {
	return runSSHPolicyArgsOpt(ctx, sock, []string{mode}, checkAgent, config, digest, epoch, fences...)
}

func runSSHPolicyArgsOpt(ctx context.Context, sock string, args []string, checkAgent bool, config []byte, digest, epoch string, fences ...func() error) error {
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	check := func() error {
		if len(fences) != 0 {
			return fences[0]()
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	if checkAgent {
		if err := trustedRootAgent(ctx, sock); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
	}
	argv := append([]string{"/usr/bin/python3", "-I", sshPolicyHelper}, args...)
	req := map[string]any{
		"op":                "exec",
		"user":              "root",
		"trusted_root_exec": true,
		"argv":              argv,
		"stdin_b64":         base64.StdEncoding.EncodeToString(config),
		"timeout_s":         20,
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return err
	}
	line := append(reqBytes, '\n')

	var a *agentConn
	for attempt := 0; ; attempt++ {
		var reused bool
		a, reused, err = dialStream(ctx, sock)
		if err != nil {
			return err
		}
		if err = check(); err != nil {
			if a.unbind != nil {
				a.unbind()
			}
			a.Close()
			return err
		}
		n, werr := a.c.Write(line)
		if werr == nil {
			break
		}
		if a.unbind != nil {
			a.unbind()
		}
		a.Close()
		// Safe retry matching sendExec: the guest acts on complete lines only,
		// so a short write on a stale pooled conn cannot have executed the helper.
		if reused && attempt == 0 && n < len(line) && ctx.Err() == nil {
			continue
		}
		return werr
	}
	returned := false
	defer func() {
		if !returned {
			if a.unbind != nil {
				a.unbind()
			}
			a.Close()
		}
	}()
	var stdout, stderr bytes.Buffer
	for {
		_, rep, err := a.recv()
		if err != nil {
			return err
		}
		if rep.Error != "" {
			if check() == nil {
				returned = true
				a.finishStream(sock, rep)
			}
			return &agentErr{msg: rep.Error, code: rep.Code}
		}
		if rep.Stream != "" {
			data, err := base64.StdEncoding.DecodeString(rep.Data)
			if err != nil {
				return err
			}
			if len(data) > 64<<10 || stdout.Len()+len(data) > 64<<10 {
				return fmt.Errorf("SSH policy response too large")
			}
			if rep.Stream == "stdout" {
				stdout.Write(data)
			} else if rep.Stream == "stderr" && stderr.Len() < 4096 {
				rem := 4096 - stderr.Len()
				if len(data) > rem {
					data = data[:rem]
				}
				stderr.Write(data)
			}
		}
		if rep.Exit != nil {
			if err := check(); err != nil {
				return err
			}
			if *rep.Exit != 0 || rep.TimedOut {
				if !rep.TimedOut {
					returned = true
					a.finishStream(sock, rep)
				}
				return &sshPolicyExecErr{mode: mode, exitCode: *rep.Exit, timedOut: rep.TimedOut, stderr: stderr.String()}
			}
			var proof sshPolicyGuestProof
			dec := json.NewDecoder(&stdout)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&proof); err != nil {
				return fmt.Errorf("SSH policy proof: %w", err)
			}
			var trailing any
			if err := dec.Decode(&trailing); err != io.EOF {
				return fmt.Errorf("SSH policy trailing output")
			}
			if proof.Version != 1 || proof.ConfigDigest != digest || proof.BootEpoch != epoch || !proof.Readiness {
				return fmt.Errorf("SSH policy proof mismatch")
			}
			returned = true
			a.finishStream(sock, rep)
			return nil
		}
	}
}

// A source pause/resume continues the same VMM identity, unlike a restored
// child or a new process. Callers already hold m.lock; never wait for an API
// installer here, since it needs that same lock to complete.
func (m *Manager) quarantineSSH(v VM) error {
	if _, err := m.update(v.ID, "", func(x *VM) {
		x.SSHPolicyReady = false
		x.sshPauseFenced = false
	}); err != nil {
		return err
	}
	return m.syncQuarantine(v)
}

// quarantineSSHPause applies a transient SSH-only pause-fence on an already-ready
// running VM across a fork/snapshot pause with reprovision_ssh:true. New inbound
// TCP/22 SYNs are dropped in nftables while existing HTTP/WebSocket/SSE sessions
// and routes.json stay live, preserving v.SSHPolicyReady, v.sshConfig, and
// v.sshHelperDigest so the immediately following installSSHPolicy hits rotate-token.
// Fails closed to full quarantine if applying the pause-fence fails.
func (m *Manager) quarantineSSHPause(v VM) error {
	if _, err := m.update(v.ID, "", func(x *VM) { x.sshPauseFenced = true }); err != nil {
		return err
	}
	if err := m.syncSSHPause(v); err != nil {
		return errors.Join(err, m.quarantineSSH(v))
	}
	return nil
}
func (m *Manager) restoreSSHAfterPause(ctx context.Context, original VM) error {
	if !original.SSHPolicyReady {
		return nil
	}
	if len(original.sshConfig) == 0 {
		if !m.cfg.standaloneReady(original) {
			return fmt.Errorf("SSH policy configuration absent after pause")
		}
		m.mu.Lock()
		current, ok := m.vms[original.ID]
		if !ok || !sameSSHBoot(original, current) {
			m.mu.Unlock()
			return errf(409, "SSH boot changed during pause")
		}
		nv := current.clone()
		nv.SSHPolicyReady = true
		err := m.commitLocked(current, nv, "")
		m.mu.Unlock()
		if err != nil {
			return err
		}
		if err := m.syncUnquarantine(nv); err != nil {
			_, uerr := m.update(original.ID, "", func(x *VM) { x.SSHPolicyReady = false })
			return errors.Join(err, uerr, m.syncNet())
		}
		return nil
	}
	current, err := m.get(original.ID)
	if err != nil {
		return err
	}
	if !sameSSHBoot(original, current) {
		return errf(409, "SSH boot changed during pause")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	check := func() error {
		now, err := m.get(original.ID)
		if err != nil {
			return err
		}
		if !sameSSHBoot(original, now) {
			return errf(409, "SSH boot changed during pause verification")
		}
		return nil
	}
	if err := runSSHPolicy(ctx, filepath.Join(m.root(original.ID), vsockUDS), "verify", original.sshConfig, original.SSHPolicyDigest, original.SSHBootEpoch, check); err != nil {
		return err
	}
	m.mu.Lock()
	current, ok := m.vms[original.ID]
	if !ok || !sameSSHBoot(original, current) {
		m.mu.Unlock()
		return errf(409, "SSH boot changed during pause verification")
	}
	nv := current.clone()
	nv.SSHPolicyReady = true
	err = m.commitLocked(current, nv, "")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := m.syncUnquarantine(nv); err != nil {
		_, uerr := m.update(original.ID, "", func(x *VM) { x.SSHPolicyReady = false })
		return errors.Join(err, uerr, m.syncNet())
	}
	return nil
}
