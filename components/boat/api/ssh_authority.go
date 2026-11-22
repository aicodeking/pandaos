package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sshLookupPrefix = "boatssh_"

type sshPolicyAttestation struct {
	Version         int    `json:"version"`
	BootEpoch       string `json:"boot_epoch"`
	ConfigDigest    string `json:"config_digest"`
	Readiness       bool   `json:"readiness"`
	VMID            string `json:"vm_id,omitempty"`
	Team            string `json:"team,omitempty"`
	State           string `json:"state,omitempty"`
	PendingCopy     bool   `json:"pending_copy,omitempty"`
	SSHPolicyReady  bool   `json:"ssh_policy_ready,omitempty"`
	SSHPolicyDigest string `json:"ssh_policy_digest,omitempty"`
}

func (s *server) readSSHPolicyHelper() ([]byte, error) {
	path := strings.TrimSpace(s.cfg.SSHPolicyHelperFile)
	if path == "" {
		return nil, errors.New("SSH policy helper is not configured")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("SSH policy helper %s is a directory", path)
	}
	modNano, size := fi.ModTime().UnixNano(), fi.Size()
	if size == 0 || size > 1<<20 {
		return nil, errors.New("invalid SSH policy helper artifact size")
	}
	s.helperMu.Lock()
	defer s.helperMu.Unlock()
	if s.helperPath == path && s.helperModNano == modNano && s.helperSize == size && len(s.helperBytes) == int(size) {
		return s.helperBytes, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || len(b) > 1<<20 {
		return nil, errors.New("invalid SSH policy helper artifact size")
	}
	s.helperPath, s.helperModNano, s.helperSize, s.helperBytes = path, modNano, size, b
	return b, nil
}

// Canonical map encoding is shared with the Python guest helper: sorted keys,
// compact UTF-8 JSON and no newline. No credentials are placed in tenant env.
func (s *server) sshPolicyConfig(st *State, vm VM) (map[string]any, error) {
	id, team, epoch := str(vm, "id"), str(vm, "team"), str(vm, "ssh_boot_epoch")
	tok := st.VMTokens[id]
	u, err := url.Parse(s.vmAPIURL())
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || id == "" || team == "" || epoch == "" || tok == nil || tok.Retired || tok.TeamID != team || st.Teams[team] == nil {
		return nil, errSSHAuthority
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/vms/" + url.PathEscape(id) + "/ssh/authorize"
	message := fmt.Sprintf("ssh-auth-v1|%s|%s|%d|%s|copy-v1=%t", team, id, tok.Gen, epoch, st.CopyCredentialsV1)
	users := slices.Clone(s.cfg.SSHPolicyUsers)
	if len(users) == 0 {
		users = []string{s.cfg.SSHUser}
	}
	for _, user := range users {
		if user != "dev" && user != "user" {
			return nil, errSSHAuthority
		}
	}
	slices.Sort(users)
	users = slices.Compact(users)
	return map[string]any{"version": 1, "url": u.String(), "token": sshLookupPrefix + signHex(s.keys.vmKey, message), "vm_id": id, "team_id": team, "generation": tok.Gen, "boot_epoch": epoch, "users": users}, nil
}

func sshConfigDigest(config map[string]any) string {
	b, _ := json.Marshal(config)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func boolField(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func (s *server) sshPolicyConfigured() bool {
	return s.cfg.SSHPolicyHelperFile != ""
}

func (s *server) sshServerConfigured() bool {
	if !s.sshPolicyConfigured() {
		return false
	}
	u, err := url.Parse(s.vmAPIURL())
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (s *server) attestedBootEpoch(id string) string {
	if id == "" {
		return ""
	}
	s.attestedMu.Lock()
	got := ""
	if s.attestedEpoch != nil {
		got = s.attestedEpoch[id]
	}
	s.attestedMu.Unlock()
	epoch, _, ok := strings.Cut(got, "|")
	if !ok {
		return ""
	}
	return epoch
}

func (s *server) getVMSSHView(ctx context.Context, id string) (VM, error) {
	var vm VM
	err := s.boatd.call(ctx, "GET", vmPath(id)+"?view=ssh", nil, &vm)
	var be *boatdError
	if errors.As(err, &be) && be.Status == http.StatusBadRequest {
		vm = nil
		err = s.boatd.call(ctx, "GET", vmPath(id), nil, &vm)
	}
	return vm, err
}

func (s *server) enterPolicy(id string) {
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	if s.inFlightPolicy == nil {
		s.inFlightPolicy = map[string]int{}
	}
	s.inFlightPolicy[id]++
}

// notePendingEpoch records a boot epoch whose policy push is in flight for id:
// the guest may present the matching lookup bearer before the epoch is
// attested. Dropped when the last in-flight push for id ends.
func (s *server) notePendingEpoch(id, epoch string) {
	if id == "" || epoch == "" {
		return
	}
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	if s.pendingEpochs == nil {
		s.pendingEpochs = map[string]map[string]bool{}
	}
	if s.pendingEpochs[id] == nil {
		s.pendingEpochs[id] = map[string]bool{}
	}
	if len(s.pendingEpochs[id]) < 8 {
		s.pendingEpochs[id][epoch] = true
	}
}

// knownBootEpochs: the attested epoch plus epochs with a push in flight.
func (s *server) knownBootEpochs(id string) []string {
	var out []string
	if e := s.attestedBootEpoch(id); e != "" {
		out = append(out, e)
	}
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	for e := range s.pendingEpochs[id] {
		out = append(out, e)
	}
	return out
}

func (s *server) leavePolicy(id string) {
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	if s.inFlightPolicy == nil {
		return
	}
	if s.inFlightPolicy[id] <= 1 {
		delete(s.inFlightPolicy, id)
		delete(s.pendingEpochs, id)
	} else {
		s.inFlightPolicy[id]--
	}
}

func (s *server) isPolicyInFlight(id string) bool {
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	return s.inFlightPolicy != nil && s.inFlightPolicy[id] > 0
}

func (s *server) recordAttested(id, epoch, digest string) {
	if id == "" || epoch == "" {
		return
	}
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	if s.attestedEpoch == nil {
		s.attestedEpoch = map[string]string{}
	}
	s.attestedEpoch[id] = epoch + "|" + digest
}

func (s *server) clearAttested(id string) {
	s.attestedMu.Lock()
	defer s.attestedMu.Unlock()
	delete(s.attestedEpoch, id)
}

func (s *server) isEpochAttested(id string, vm VM) bool {
	epoch := str(vm, "ssh_boot_epoch")
	if id == "" || epoch == "" {
		return false
	}
	s.attestedMu.Lock()
	got := ""
	if s.attestedEpoch != nil {
		got = s.attestedEpoch[id]
	}
	s.attestedMu.Unlock()
	return strings.HasPrefix(got, epoch+"|")
}

func (s *server) sshPolicyCurrent(st *State, vm VM) bool {
	config, err := s.sshPolicyConfig(st, vm)
	return err == nil && str(vm, "state") == "running" && vm["pending_copy"] != true && vm["ssh_policy_ready"] == true && str(vm, "ssh_policy_digest") == sshConfigDigest(config)
}

// prepareSSHPolicy performs no state mutation or guest I/O while holding an
// authority lock. Boatd fences installation against the live boot incarnation.
func (s *server) prepareSSHPolicy(ctx context.Context, id, team string) (VM, error) {
	return s.prepareSSHPolicyFromVM(ctx, id, team, nil)
}

func (s *server) prepareSSHPolicyFromVM(ctx context.Context, id, team string, knownVM VM) (VM, error) {
	s.enterPolicy(id)
	defer s.leavePolicy(id)
	vm := knownVM
	if vm == nil || str(vm, "ssh_boot_epoch") == "" || str(vm, "team") != team || str(vm, "id") != id {
		var err error
		vm, err = s.getVMSSHView(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	if str(vm, "team") != team || str(vm, "id") != id || str(vm, "state") != "running" || vm["pending_copy"] == true {
		return nil, errSSHAuthority
	}
	config, err := s.sshPolicyConfig(s.store.Snapshot(), vm)
	if err != nil {
		return nil, err
	}
	epoch := str(vm, "ssh_boot_epoch")
	s.notePendingEpoch(id, epoch)
	digest := sshConfigDigest(config)
	if vm["ssh_policy_ready"] == true && str(vm, "ssh_policy_digest") == digest {
		if !s.sshPolicyCurrent(s.store.Snapshot(), vm) {
			return nil, errSSHAuthority
		}
		s.recordAttested(id, epoch, digest)
		s.clearSSHDeferredVM(id)
		return vm, nil
	}
	helper, err := s.readSSHPolicyHelper()
	if err != nil {
		return nil, err
	}
	var att sshPolicyAttestation
	body := map[string]any{"version": 1, "boot_epoch": epoch, "config": config, "helper_b64": base64.StdEncoding.EncodeToString(helper)}
	if err := s.boatd.call(ctx, "POST", vmPath(id, "ssh-policy"), body, &att); err != nil {
		return nil, err
	}
	if att.Version != 1 || !att.Readiness || att.BootEpoch != epoch || att.ConfigDigest != digest {
		return nil, errors.New("SSH policy attestation mismatch")
	}
	var fresh VM
	if att.VMID == id && att.Team == team && att.State == "running" && !att.PendingCopy && att.SSHPolicyReady && (att.SSHPolicyDigest == "" || att.SSHPolicyDigest == digest) {
		fresh = make(VM, len(vm)+4)
		for k, v := range vm {
			fresh[k] = v
		}
		fresh["id"] = id
		fresh["team"] = team
		fresh["state"] = att.State
		fresh["pending_copy"] = false
		fresh["ssh_boot_epoch"] = att.BootEpoch
		fresh["ssh_policy_ready"] = true
		fresh["ssh_policy_digest"] = digest
	} else {
		fresh, err = s.getVMSSHView(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	if str(fresh, "id") != id || str(fresh, "team") != team || str(fresh, "ssh_boot_epoch") != epoch || !s.sshPolicyCurrent(s.store.Snapshot(), fresh) {
		return nil, errSSHAuthority
	}
	s.recordAttested(id, str(fresh, "ssh_boot_epoch"), digest)
	s.clearSSHDeferredVM(id)
	return fresh, nil
}

// ensureRunningVMSSHPolicy re-attests adopted or ResumeOnBoot running VMs so
// they leave quarantine after boatd restarts or finishes booting them.
func (s *server) ensureRunningVMSSHPolicy(ctx context.Context, vm VM) error {
	if !s.sshPolicyConfigured() {
		return nil
	}
	id, team := str(vm, "id"), str(vm, "team")
	if id == "" || team == "" || str(vm, "state") != "running" || boolField(vm, "pending_copy") || s.isPolicyInFlight(id) {
		return nil
	}
	st := s.store.Snapshot()
	if boolField(vm, "ssh_policy_ready") && s.isEpochAttested(id, vm) && s.sshPolicyCurrent(st, vm) {
		return nil
	}
	ok, err := s.ensureAdoptedVMToken(id, team)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if s.sshPolicyCurrent(s.store.Snapshot(), vm) {
		if cfg, cerr := s.sshPolicyConfig(s.store.Snapshot(), vm); cerr == nil {
			s.recordAttested(id, str(vm, "ssh_boot_epoch"), sshConfigDigest(cfg))
			return nil
		}
	}
	_, err = s.prepareSSHPolicyFromVM(ctx, id, team, vm)
	return err
}

// vmLifecycleGate tracks in-flight API-driven VM creations (createVM /
// envBuildSteps) per team so SSE vm.state=running handlers wait for the
// creating request to finish recording VMTokens and provisioning SSH policy
// instead of racing bootVMs. Waiting snapshots only already-started creates,
// preventing starvation under continuous back-to-back creates.
type vmLifecycleGate struct {
	mu     sync.Mutex
	active map[string]map[chan struct{}]struct{}
}

func (g *vmLifecycleGate) begin(team string) func() {
	if team == "" {
		return func() {}
	}
	ch := make(chan struct{})
	g.mu.Lock()
	if g.active == nil {
		g.active = map[string]map[chan struct{}]struct{}{}
	}
	set := g.active[team]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		g.active[team] = set
	}
	set[ch] = struct{}{}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if set := g.active[team]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(g.active, team)
				}
			}
			g.mu.Unlock()
			close(ch)
		})
	}
}

func (g *vmLifecycleGate) hasActive(team string) bool {
	if team == "" {
		return false
	}
	g.mu.Lock()
	n := len(g.active[team])
	g.mu.Unlock()
	return n > 0
}

func (g *vmLifecycleGate) waitExisting(ctx context.Context, team string) bool {
	if team == "" {
		return true
	}
	g.mu.Lock()
	set := g.active[team]
	if len(set) == 0 {
		g.mu.Unlock()
		return true
	}
	wait := make([]chan struct{}, 0, len(set))
	for ch := range set {
		wait = append(wait, ch)
	}
	g.mu.Unlock()
	for _, ch := range wait {
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (s *server) reconcileRunningVM(ctx context.Context, vmID string) {
	s.reconcileRunningVMForTeam(ctx, vmID, "")
}

// triggerReconcileRunningVM coalesces concurrent SSE vm.state=running events
// for the same vmID into a single-flight reconcile goroutine with a dirty flag
// so a burst of duplicate events cannot spawn an N-way goroutine/GET storm.
func (s *server) triggerReconcileRunningVM(ctx context.Context, vmID, team string) {
	if vmID == "" || ctx.Err() != nil || s.store.closedFlag.Load() {
		return
	}
	s.reconcileMu.Lock()
	if s.reconcileActive == nil {
		s.reconcileActive = map[string]bool{}
		s.reconcileDirty = map[string]string{}
	}
	if s.reconcileActive[vmID] {
		if team != "" || s.reconcileDirty[vmID] == "" {
			s.reconcileDirty[vmID] = team
		}
		s.reconcileMu.Unlock()
		return
	}
	s.reconcileActive[vmID] = true
	delete(s.reconcileDirty, vmID)
	s.reconcileMu.Unlock()

	go func() {
		curTeam := team
		done := false
		defer func() {
			if !done {
				s.reconcileMu.Lock()
				delete(s.reconcileActive, vmID)
				delete(s.reconcileDirty, vmID)
				s.reconcileMu.Unlock()
			}
		}()
		for {
			s.reconcileRunningVMForTeam(ctx, vmID, curTeam)
			s.reconcileMu.Lock()
			nextTeam, dirty := s.reconcileDirty[vmID]
			delete(s.reconcileDirty, vmID)
			if !dirty || ctx.Err() != nil || s.store.closedFlag.Load() {
				delete(s.reconcileActive, vmID)
				done = true
				s.reconcileMu.Unlock()
				return
			}
			if nextTeam != "" {
				curTeam = nextTeam
			}
			s.reconcileMu.Unlock()
		}
	}()
}

func (s *server) reconcileRunningVMForTeam(ctx context.Context, vmID, team string) {
	if vmID == "" {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Wait for any in-flight create/builder on this team so SSE vm.state=running
	// does not race bootVMs before coalesceCreateStore records VMTokens[vmID]
	// and provisions SSH policy.
	if team != "" && !s.vmCreates.waitExisting(rctx, team) {
		return
	}
	// Serialize with startVM / forkVM release / refreshCredentials / concurrent reconcile.
	unlock := startLocks.lock(vmID)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()

	st := s.store.Snapshot()
	needToken := (s.sshPolicyConfigured() || s.vmAPIURL() != "") && st.VMTokens[vmID] == nil
	if st.VMTransitions[vmID] == nil && !needToken && !s.sshPolicyConfigured() {
		return
	}

	var vm VM
	if err := s.boatd.call(rctx, "GET", vmPath(vmID), nil, &vm); err != nil {
		return
	}
	if str(vm, "state") != "running" || boolField(vm, "pending_copy") {
		return
	}
	// If team was unknown on entry and a create/builder for this VM's team is in
	// flight, release the per-VM lock while waiting so we never race createVM.
	if vmTeam := str(vm, "team"); team == "" && vmTeam != "" && s.vmCreates.hasActive(vmTeam) {
		unlock()
		unlock = nil
		if !s.vmCreates.waitExisting(rctx, vmTeam) {
			return
		}
		unlock = startLocks.lock(vmID)
		st = s.store.Snapshot()
		needToken = (s.sshPolicyConfigured() || s.vmAPIURL() != "") && st.VMTokens[vmID] == nil
		if st.VMTransitions[vmID] == nil && !needToken && !s.sshPolicyConfigured() {
			return
		}
		vm = nil
		if err := s.boatd.call(rctx, "GET", vmPath(vmID), nil, &vm); err != nil {
			return
		}
		if str(vm, "state") != "running" || boolField(vm, "pending_copy") {
			return
		}
	}

	// Fence incomplete cold credential transitions fail-closed. startVM holds
	// startLocks.lock(vmID) throughout an active transition, so reaching here
	// with VMTransitions[vmID] != nil means an externally started VM resumed
	// before a crashed transition completed.
	if s.store.Snapshot().VMTransitions[vmID] != nil {
		if err := s.boatd.call(rctx, "POST", vmPath(vmID, "stop"), map[string]any{"force": true}, nil); err != nil {
			log.Printf("vm %s: stop running VM with pending credential transition: %v", vmID, err)
		}
		return
	}

	if err := s.reconcileSingleRunningVMCredentials(rctx, vmID, vm); err != nil {
		log.Printf("vm %s: reconcile credentials on running state: %v", vmID, err)
	}
	if err := s.ensureRunningVMSSHPolicy(rctx, vm); err != nil {
		log.Printf("vm %s: re-attest running SSH policy: %v", vmID, err)
	}
}

type sshLookupRequest struct {
	User    string `json:"user"`
	KeyType string `json:"key_type"`
	KeyBlob string `json:"key_blob"`
}

func readSSHLookup(w http.ResponseWriter, r *http.Request) (sshLookupRequest, bool) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	defer controller.SetReadDeadline(time.Time{})
	var in sshLookupRequest
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "invalid SSH lookup", 400)
		return in, false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || in.User == "" || strings.ContainsAny(in.KeyType+in.KeyBlob, " \t\r\n") || !reSSHKey.MatchString(in.KeyType+" "+in.KeyBlob) {
		http.Error(w, "invalid SSH lookup", 400)
		return in, false
	}
	return in, true
}

func (s *server) authorizeGuestSSH(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Unauthenticated route: throttle per client IP like the link downloads.
	// 429 + Retry-After (the guest helper treats every non-200/204 as "no key"
	// and retries).
	if ok, wait := s.limiter.allow("sshauth:" + trustedClientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	// Every failure to prove "this bearer belongs to this VM" is the same 401:
	// unknown VM, malformed id, retired token and wrong bearer are
	// indistinguishable, and boatd is never asked before the proof.
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "+sshLookupPrefix)
	id := r.PathValue("id")
	if !ok || len(bearer) != 64 || !reID.MatchString(id) {
		w.WriteHeader(401)
		return
	}
	st := s.store.Snapshot()
	tok := st.VMTokens[id]
	if tok == nil || tok.Retired || st.Teams[tok.TeamID] == nil {
		w.WriteHeader(401)
		return
	}
	// The bearer must verify against the locally known token material before
	// boatd is asked anything, in flight or not: a push in flight only adds the
	// epoch being installed (not attested yet) to the epochs that can verify.
	// Unknown epoch or wrong bearer: the same 401 as an unknown VM.
	matched := false
	for _, epoch := range s.knownBootEpochs(id) {
		cfg, cerr := s.sshPolicyConfig(st, VM{"id": id, "team": tok.TeamID, "ssh_boot_epoch": epoch})
		if cerr == nil && hmac.Equal([]byte(cfg["token"].(string)), []byte(sshLookupPrefix+bearer)) {
			matched = true
		}
	}
	if !matched {
		w.WriteHeader(401)
		return
	}
	in, ok := readSSHLookup(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	vm, err := s.getVMSSHView(ctx, id)
	if err != nil {
		var upstream *boatdError
		if errors.As(err, &upstream) && upstream.Status == 404 {
			w.WriteHeader(401) // same answer as a bad bearer: no VM-existence oracle
			return
		}
		w.WriteHeader(503)
		return
	}

	status, out := s.guestSSHDecision(vm, id, sshLookupPrefix+bearer, in)
	if out == nil {
		w.WriteHeader(status)
		return
	}
	// The guest helper rejects chunked responses, even for large accepted keys.
	body, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)+1))
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func (s *server) guestSSHDecision(vm VM, id, bearer string, in sshLookupRequest) (int, map[string]string) {
	st := s.store.Snapshot()
	config, err := s.sshPolicyConfig(st, vm)
	if err != nil || str(vm, "id") != id || !hmac.Equal([]byte(config["token"].(string)), []byte(bearer)) {
		return 401, nil
	}
	if !s.sshPolicyCurrent(st, vm) {
		return 503, nil
	}
	if !slices.Contains(config["users"].([]string), in.User) {
		return 204, nil
	}
	pub := in.KeyType + " " + in.KeyBlob
	now := time.Now()
	team := str(vm, "team")
	granted := false
	for _, g := range st.sshGrantsByPub()[pub] {
		if g.TeamID == team && grantAlive(st, g, now) && (slices.Contains(g.VMs, id) || slices.Contains(g.VMs, "*") || slices.Contains(g.VMs, "")) {
			granted = true
			break
		}
	}
	if !granted {
		return 204, nil
	}
	return 200, map[string]string{"authorized_key": pub}
}

func (s *server) authorizeJumpSSH(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !directLoopback(r) {
		w.WriteHeader(403)
		return
	}
	in, ok := readSSHLookup(w, r)
	if !ok {
		return
	}
	if in.User != "jump" {
		w.WriteHeader(204)
		return
	}
	pub := in.KeyType + " " + in.KeyBlob
	st, now := s.store.Snapshot(), time.Now()
	candidates := st.sshGrantsByPub()[pub]
	teamWide := map[string]bool{}
	type targetVM struct {
		id   string
		team string
	}
	var targets []targetVM
	seenVM := map[string]bool{}
	liveCount := 0
	for _, g := range candidates {
		if !grantAlive(st, g, now) {
			continue
		}
		liveCount++
		for _, id := range g.VMs {
			if id == "" || id == "*" {
				teamWide[g.TeamID] = true
			} else if !seenVM[id] {
				seenVM[id] = true
				targets = append(targets, targetVM{id: id, team: g.TeamID})
			}
		}
	}
	if liveCount == 0 {
		w.WriteHeader(204)
		return
	}
	perTeam := map[string]int{}
	for _, t := range targets {
		perTeam[t.team]++
		if perTeam[t.team] > 4 {
			teamWide[t.team] = true
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	var inventory []VM
	for team := range teamWide {
		var list struct {
			VMs []VM `json:"vms"`
		}
		q := "/v1/vms?team=" + url.QueryEscape(team) + "&view=ssh"
		if err := s.boatd.call(ctx, "GET", q, nil, &list); err != nil {
			var be *boatdError
			if errors.As(err, &be) && be.Status == http.StatusBadRequest {
				err = s.boatd.call(ctx, "GET", "/v1/vms?team="+url.QueryEscape(team), nil, &list)
			}
			if err != nil {
				w.WriteHeader(503)
				return
			}
		}
		for _, vm := range list.VMs {
			if str(vm, "team") == team {
				inventory = append(inventory, vm)
			}
		}
	}
	var pending []string
	for _, t := range targets {
		if !teamWide[t.team] {
			pending = append(pending, t.id)
		}
	}
	if len(pending) == 1 {
		var vm VM
		if err := s.boatd.call(ctx, "GET", vmPath(pending[0]), nil, &vm); err != nil {
			var be *boatdError
			if !errors.As(err, &be) || be.Status != http.StatusNotFound {
				w.WriteHeader(503)
				return
			}
		} else {
			inventory = append(inventory, vm)
		}
	} else if len(pending) > 1 {
		var (
			mu      sync.Mutex
			hardErr bool
			wg      sync.WaitGroup
			sem     = make(chan struct{}, min(len(pending), 16))
		)
		for _, id := range pending {
			wg.Add(1)
			sem <- struct{}{}
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				var vm VM
				if err := s.boatd.call(ctx, "GET", vmPath(id), nil, &vm); err != nil {
					var be *boatdError
					if !errors.As(err, &be) || be.Status != http.StatusNotFound {
						mu.Lock()
						hardErr = true
						mu.Unlock()
					}
					return
				}
				mu.Lock()
				inventory = append(inventory, vm)
				mu.Unlock()
			}(id)
		}
		wg.Wait()
		if hardErr {
			w.WriteHeader(503)
			return
		}
	}
	status, lines := s.jumpSSHDecision(inventory, in)
	if status != 200 {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, 200, map[string]any{"authorized_keys": lines})
}

func (s *server) jumpSSHDecision(inventory []VM, in sshLookupRequest) (int, []string) {
	st, now := s.store.Snapshot(), time.Now()
	vms := map[string]VM{}
	for _, vm := range inventory {
		if s.sshPolicyCurrent(st, vm) {
			vms[str(vm, "id")] = vm
		}
	}
	grants := map[string]*SSHGrant{}
	pub := in.KeyType + " " + in.KeyBlob
	for k, g := range st.sshGrantsByPub()[pub] {
		if grantAlive(st, g, now) {
			grants[k] = g
		}
	}
	lines := strings.TrimSpace(string(renderJumpKeys(grants, vms)))
	if lines == "" {
		return 204, nil
	}
	return 200, strings.Split(lines, "\n")
}

func (s *server) provisionSSHPolicyLogged(ctx context.Context, id, team string) {
	s.provisionSSHPolicyLoggedWithVM(ctx, id, team, nil)
}

func (s *server) provisionSSHPolicyLoggedWithVM(ctx context.Context, id, team string, knownVM VM) {
	if s.cfg.SSHPolicyHelperFile == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.prepareSSHPolicyFromVM(ctx, id, team, knownVM); err != nil {
		log.Printf("vm %s: SSH policy remains unready: %v", id, err)
	}
}

func (s *server) provisionSSHPolicyBatch(ctx context.Context, vms []VM, team string) {
	if s.cfg.SSHPolicyHelperFile == "" || !s.sshPolicyConfigured() || len(vms) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	helper, err := s.readSSHPolicyHelper()
	if err != nil {
		for _, vm := range vms {
			log.Printf("vm %s: SSH policy remains unready: %v", str(vm, "id"), err)
		}
		return
	}
	helperB64 := base64.StdEncoding.EncodeToString(helper)
	type batchMeta struct {
		id     string
		epoch  string
		digest string
	}
	var (
		items []map[string]any
		metas []batchMeta
	)
	for _, raw := range vms {
		id := str(raw, "id")
		if id == "" {
			continue
		}
		s.enterPolicy(id)
		defer s.leavePolicy(id)
		vm := raw
		if str(vm, "ssh_boot_epoch") == "" || str(vm, "team") != team || boolField(vm, "pending_copy") {
			var fetched VM
			if err := s.boatd.call(ctx, "GET", vmPath(id), nil, &fetched); err != nil {
				log.Printf("vm %s: SSH policy remains unready: %v", id, err)
				continue
			}
			vm = fetched
		}
		if str(vm, "id") != id || str(vm, "team") != team || str(vm, "state") != "running" || boolField(vm, "pending_copy") {
			log.Printf("vm %s: SSH policy remains unready: %v", id, errSSHAuthority)
			continue
		}
		cfg, cerr := s.sshPolicyConfig(s.store.Snapshot(), vm)
		if cerr != nil {
			log.Printf("vm %s: SSH policy remains unready: %v", id, cerr)
			continue
		}
		epoch := str(vm, "ssh_boot_epoch")
		s.notePendingEpoch(id, epoch)
		digest := sshConfigDigest(cfg)
		metas = append(metas, batchMeta{id: id, epoch: epoch, digest: digest})
		items = append(items, map[string]any{
			"id":         id,
			"version":    1,
			"boot_epoch": epoch,
			"config":     cfg,
			"helper_b64": helperB64,
		})
	}
	if len(items) == 0 {
		return
	}
	var batchResp struct {
		Results map[string]sshPolicyAttestation `json:"results"`
		Errors  map[string]string               `json:"errors"`
	}
	if err := s.boatd.call(ctx, "POST", "/v1/vms/ssh-policy-batch", map[string]any{"items": items}, &batchResp); err != nil {
		var be *boatdError
		if errors.As(err, &be) && (be.Status == http.StatusNotFound || be.Status == http.StatusMethodNotAllowed) {
			sem := make(chan struct{}, min(len(metas), 16))
			var wg sync.WaitGroup
			for _, m := range metas {
				wg.Add(1)
				sem <- struct{}{}
				go func(id string) {
					defer wg.Done()
					defer func() { <-sem }()
					if _, perr := s.prepareSSHPolicy(ctx, id, team); perr != nil {
						log.Printf("vm %s: SSH policy remains unready: %v", id, perr)
					}
				}(m.id)
			}
			wg.Wait()
			return
		}
		for _, m := range metas {
			log.Printf("vm %s: SSH policy remains unready: %v", m.id, err)
		}
		return
	}
	sem := make(chan struct{}, min(len(metas), 16))
	var wg sync.WaitGroup
	for _, m := range metas {
		if emsg := batchResp.Errors[m.id]; emsg != "" {
			log.Printf("vm %s: SSH policy remains unready: %s", m.id, emsg)
			continue
		}
		att, ok := batchResp.Results[m.id]
		if !ok || att.Version != 1 || !att.Readiness || att.BootEpoch != m.epoch || att.ConfigDigest != m.digest {
			log.Printf("vm %s: SSH policy remains unready: attestation mismatch", m.id)
			continue
		}
		if att.VMID == m.id && att.Team == team && att.State == "running" && !att.PendingCopy && att.SSHPolicyReady && (att.SSHPolicyDigest == "" || att.SSHPolicyDigest == m.digest) {
			synth := VM{"id": m.id, "team": team, "state": att.State, "pending_copy": false, "ssh_boot_epoch": att.BootEpoch, "ssh_policy_ready": true, "ssh_policy_digest": m.digest}
			if !s.sshPolicyCurrent(s.store.Snapshot(), synth) {
				log.Printf("vm %s: SSH policy remains unready: %v", m.id, errSSHAuthority)
				continue
			}
			s.recordAttested(m.id, m.epoch, m.digest)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m batchMeta) {
			defer wg.Done()
			defer func() { <-sem }()
			fresh, err := s.getVMSSHView(ctx, m.id)
			if err != nil {
				log.Printf("vm %s: SSH policy remains unready: %v", m.id, err)
				return
			}
			if str(fresh, "id") != m.id || str(fresh, "team") != team || str(fresh, "ssh_boot_epoch") != m.epoch || !s.sshPolicyCurrent(s.store.Snapshot(), fresh) {
				log.Printf("vm %s: SSH policy remains unready: %v", m.id, errSSHAuthority)
				return
			}
			s.recordAttested(m.id, m.epoch, m.digest)
		}(m)
	}
	wg.Wait()
}
