package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestR23UploadTokenRedactionAndSensitiveReadAuditing(t *testing.T) {
	e := setup(t)
	e.srv.cfg.PublicURL = "https://api.example.test"
	bx := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + bx

	up := e.ok("POST", base+"/files/upload-url", map[string]any{"path": "/tmp/secret.txt"}, 200)
	upURL, _ := url.Parse(up["url"].(string))
	tokenSeg := strings.TrimPrefix(upURL.Path, fileUploadPath)
	if tokenSeg == "" || tokenSeg == upURL.Path {
		t.Fatalf("unexpected upload path %q", upURL.Path)
	}

	req, err := http.NewRequest("PUT", e.api.URL+upURL.Path, strings.NewReader("payload-data"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload status = %d, want 200", resp.StatusCode)
	}

	auditText := e.audit.String()
	if strings.Contains(auditText, tokenSeg) {
		t.Fatalf("audit log leaked raw upload capability token %q:\n%s", tokenSeg, auditText)
	}
	if !strings.Contains(auditText, `"path":"/api/v1/file-uploads/REDACTED"`) {
		t.Fatalf("audit log missing redacted upload path:\n%s", auditText)
	}

	e.audit.Reset()
	sensitivePaths := []string{
		"/api/v1/api-keys",
		"/api/v1/secrets",
		"/api/v1/telemetry",
		"/api/v1/credential-profiles",
		base + "/files?path=/tmp/secret.txt",
		base + "/files/list?path=/tmp",
		base + "/artifacts?path=/tmp/secret.txt",
	}
	for _, p := range sensitivePaths {
		req, _ := http.NewRequest("GET", e.api.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+e.owner)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("GET %s status = %d, want 200", p, res.StatusCode)
		}
	}

	lines := strings.Split(strings.TrimSpace(e.audit.String()), "\n")
	audited := map[string]bool{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		var ae auditEntry
		if err := json.Unmarshal([]byte(line), &ae); err != nil {
			t.Fatalf("unmarshal audit entry %q: %v", line, err)
		}
		audited[ae.Path] = true
	}
	for _, wantPath := range []string{
		"/api/v1/api-keys",
		"/api/v1/secrets",
		"/api/v1/telemetry",
		"/api/v1/credential-profiles",
		base + "/files",
		base + "/files/list",
		base + "/artifacts",
	} {
		if !audited[wantPath] {
			t.Errorf("sensitive GET %s was not written to audit log (got %v)", wantPath, audited)
		}
	}
}

func TestR23StreamQuotasOnCompatStreams(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + bx

	e.srv.streams = newInflight(1)
	rel, ok := e.srv.streams.acquire(e.teamID())
	if !ok {
		t.Fatal("failed to acquire initial stream slot")
	}
	defer rel()

	for _, p := range []string{
		base + "/commands/1/stream",
		base + "/command-history/c_1/logs",
		base + "/artifacts?path=/tmp/a",
	} {
		st, out, _ := e.compat(e.owner, "GET", p, nil, nil)
		if st != 429 || errCode(out) != "rate_limited" {
			t.Errorf("GET %s while stream quota full: status=%d out=%v, want 429 rate_limited", p, st, out)
		}
	}
}

func TestR23TrustedClientIPAndBucketCap(t *testing.T) {
	// 1. Direct non-loopback peer ignores X-Forwarded-For and X-Real-Ip completely.
	r1 := httptest.NewRequest("GET", "/v1/snapshot-links/x/download", nil)
	r1.RemoteAddr = "203.0.113.50:44321"
	r1.Header.Set("X-Forwarded-For", "198.51.100.1, 198.51.100.2")
	r1.Header.Set("X-Real-Ip", "198.51.100.3")
	if got := trustedClientIP(r1); got != "203.0.113.50" {
		t.Fatalf("non-loopback trustedClientIP = %q, want 203.0.113.50", got)
	}

	// 2. Loopback peer extracts RIGHTMOST valid IP from X-Forwarded-For (ignoring forged leftmost hops).
	r2 := httptest.NewRequest("GET", "/v1/snapshot-links/x/download", nil)
	r2.RemoteAddr = "127.0.0.1:54321"
	r2.Header.Set("X-Forwarded-For", "198.51.100.99, not-an-ip")
	r2.Header.Add("X-Forwarded-For", "192.0.2.10, 203.0.113.77")
	if got := trustedClientIP(r2); got != "203.0.113.77" {
		t.Fatalf("loopback rightmost XFF trustedClientIP = %q, want 203.0.113.77", got)
	}

	// 3. Loopback peer falls back to X-Real-Ip, then loopback IP.
	r3 := httptest.NewRequest("GET", "/v1/snapshot-links/x/download", nil)
	r3.RemoteAddr = "[::1]:54321"
	r3.Header.Set("X-Forwarded-For", "garbage, bogus")
	r3.Header.Set("X-Real-Ip", "203.0.113.88")
	if got := trustedClientIP(r3); got != "203.0.113.88" {
		t.Fatalf("loopback X-Real-Ip fallback = %q, want 203.0.113.88", got)
	}
	r3.Header.Set("X-Real-Ip", "still-garbage")
	if got := trustedClientIP(r3); got != "::1" {
		t.Fatalf("loopback final fallback = %q, want ::1", got)
	}

	// 4. Spoofing leftmost X-Forwarded-For cannot evade file-download rate limit through loopback Caddy.
	e := setup(t)
	e.srv.limiter = newLimiter(0.01, 5)
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest("GET", fileDownloadPath+"invalidtoken", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d, 203.0.113.200", i))
		w := httptest.NewRecorder()
		e.srv.ServeHTTP(w, req)
		if i == 7 && w.Code != 429 {
			t.Fatalf("expected 429 after burst from same rightmost IP despite rotating leftmost XFF, got %d", w.Code)
		}
	}

	// 5. Limiter bucket map is strictly capped at maxRateBuckets.
	lim := newLimiter(1, 5)
	now := time.Now()
	lim.now = func() time.Time { return now }
	for i := 0; i < maxRateBuckets+600; i++ {
		lim.allow(fmt.Sprintf("ip-%d", i))
	}
	if len(lim.buckets) > maxRateBuckets {
		t.Fatalf("limiter buckets = %d, exceeds maxRateBuckets (%d)", len(lim.buckets), maxRateBuckets)
	}
}

func TestR23LockFreeSSHAuthUnderSlowFsync(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	if st, out := e.do(e.owner, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub}); st != 200 {
		t.Fatalf("grant ssh: %d %v", st, out)
	}
	e.assertSSH(vm, r11Pub, 200)
	if code, lines := e.jumpSSH(r11Pub); code != 200 || len(lines) == 0 {
		t.Fatalf("initial jumpSSH: %d %v", code, lines)
	}

	slowFsync(t, 700*time.Millisecond)
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.srv.store.Update(func(st *State) error {
			close(entered)
			st.VMEnvs["fsync-holder"] = "holding"
			return nil
		})
	}()
	<-entered
	time.Sleep(15 * time.Millisecond) // ensure Update is inside persist/fsync

	t0 := time.Now()
	code, _ := e.lookupSSH(vm, r11Pub)
	guestDur := time.Since(t0)
	if code != 200 || guestDur >= 50*time.Millisecond {
		t.Errorf("authorizeGuestSSH took %v (status %d) while Store.Update blocked in 700ms fsync; want <50ms", guestDur, code)
	}

	t1 := time.Now()
	jCode, jLines := e.jumpSSH(r11Pub)
	jumpDur := time.Since(t1)
	if jCode != 200 || len(jLines) == 0 || jumpDur >= 50*time.Millisecond {
		t.Errorf("authorizeJumpSSH took %v (status %d, lines %d) while Store.Update blocked in 700ms fsync; want <50ms", jumpDur, jCode, len(jLines))
	}

	if err := <-done; err != nil {
		t.Fatalf("concurrent Update failed: %v", err)
	}
}

func TestR23SSHHelperCacheAndBatchPolicy(t *testing.T) {
	e := setup(t)
	e.enableSSH()

	b1, err := e.srv.readSSHPolicyHelper()
	if err != nil || string(b1) != "mock-policy-helper" {
		t.Fatalf("first readSSHPolicyHelper: %q %v", b1, err)
	}
	b2, err := e.srv.readSSHPolicyHelper()
	if err != nil || !bytes.Equal(b1, b2) {
		t.Fatalf("cached readSSHPolicyHelper: %q %v", b2, err)
	}
	// Modify file content + size; stat check must immediately invalidate cache.
	updated := []byte("updated-mock-policy-helper-v2")
	if err := os.WriteFile(e.srv.cfg.SSHPolicyHelperFile, updated, 0600); err != nil {
		t.Fatal(err)
	}
	b3, err := e.srv.readSSHPolicyHelper()
	if err != nil || !bytes.Equal(b3, updated) {
		t.Fatalf("readSSHPolicyHelper after update = %q (%v), want %q", b3, err, updated)
	}

	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "batch", "count": 4}); st != 201 {
		t.Fatalf("batch create: %d %v", st, out)
	} else {
		e.boatd.mu.Lock()
		batchCalls, singleCalls := e.boatd.sshPolicyBatchCalls, e.boatd.sshPolicyCalls
		e.boatd.mu.Unlock()
		if batchCalls != 1 || singleCalls != 0 {
			t.Errorf("batch SSH policy calls: batch=%d single=%d, want batch=1 single=0", batchCalls, singleCalls)
		}
		for _, item := range out["vms"].([]any) {
			id := item.(map[string]any)["id"].(string)
			if st, _ := e.do(e.owner, "POST", vmPath(id, "ssh"), map[string]any{"public_key": r11Pub}); st != 200 {
				t.Fatalf("grant ssh on %s: %d", id, st)
			}
			e.assertSSH(id, r11Pub, 200)
		}
	}

	// Fallback when boatd does not implement /v1/vms/ssh-policy-batch (404).
	e.boatd.mu.Lock()
	e.boatd.noSSHPolicyBatch = true
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "fb", "count": 3}); st != 201 {
		t.Fatalf("fallback batch create: %d %v", st, out)
	} else {
		for _, item := range out["vms"].([]any) {
			id := item.(map[string]any)["id"].(string)
			if st, _ := e.do(e.owner, "POST", vmPath(id, "ssh"), map[string]any{"public_key": r11Pub}); st != 200 {
				t.Fatalf("grant ssh on %s: %d", id, st)
			}
			e.assertSSH(id, r11Pub, 200)
		}
	}

	// Fork and snapshot prepare carry reprovision_ssh: true when SSHPolicyHelperFile is set.
	parent := e.createVM()
	if st, out := e.do(e.owner, "POST", vmPath(parent, "fork"), map[string]any{"name": "forked"}); st != 201 {
		t.Fatalf("fork: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "POST", vmPath(parent, "snapshots"), map[string]any{"name": "s1"}); st != 201 {
		t.Fatalf("snapshot: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	lastFork := e.boatd.forks[len(e.boatd.forks)-1]
	lastSnap := e.boatd.snapPrepares[len(e.boatd.snapPrepares)-1]
	e.boatd.mu.Unlock()
	if lastFork["reprovision_ssh"] != true {
		t.Errorf("fork prepare missing reprovision_ssh=true: %v", lastFork)
	}
	if lastSnap["reprovision_ssh"] != true {
		t.Errorf("snapshot prepare missing reprovision_ssh=true: %v", lastSnap)
	}
}

func TestR23RetiredTombstoneCap(t *testing.T) {
	e := setup(t)
	err := e.srv.store.Update(func(st *State) error {
		st.ForeignVMs["vm-foreign-keep"] = true
		retireCredentials(st, "vm-foreign-keep")
		for i := 0; i < maxRetiredVMTombstones+300; i++ {
			retireCredentials(st, fmt.Sprintf("vm-ephemeral-%05d", i))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st := e.srv.store.Snapshot()
	if len(st.VMTokens) > maxRetiredVMTombstones+1 {
		t.Errorf("VMTokens tombstones = %d, want <= %d", len(st.VMTokens), maxRetiredVMTombstones+1)
	}
	if len(st.AIVMs) > maxRetiredVMTombstones+1 {
		t.Errorf("AIVMs tombstones = %d, want <= %d", len(st.AIVMs), maxRetiredVMTombstones+1)
	}
	if tok := st.VMTokens["vm-foreign-keep"]; tok == nil || !tok.Retired {
		t.Errorf("foreign VM tombstone was pruned: %+v", tok)
	}
	newest := fmt.Sprintf("vm-ephemeral-%05d", maxRetiredVMTombstones+299)
	if tok := st.VMTokens[newest]; tok == nil || !tok.Retired {
		t.Errorf("newest retired tombstone %s was pruned: %+v", newest, tok)
	}
}

func TestR23RichPatchVMAndClone(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/KEEP_SEC", map[string]any{"value": "keep-val"})
	e.do(e.owner, "PUT", "/v1/secrets/OTHER_SEC", map[string]any{"value": "other-val"})

	st, created := e.do(e.owner, "POST", "/v1/vms", map[string]any{
		"name":    "orig",
		"secrets": []string{"KEEP_SEC"},
		"labels":  map[string]string{"tier": "dev"},
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, created)
	}
	id := created["id"].(string)

	// Reserved boat.* labels are rejected on PATCH.
	if st, _ := e.do(e.owner, "PATCH", vmPath(id), map[string]any{"labels": map[string]string{"boat.secrets": ""}}); st != 400 {
		t.Errorf("PATCH accepted reserved boat.* label: %d", st)
	}
	// Empty PATCH is rejected.
	if st, _ := e.do(e.owner, "PATCH", vmPath(id), map[string]any{}); st != 400 {
		t.Errorf("empty PATCH accepted: %d", st)
	}

	// resume_on_boot is the host's: a tenant PATCH refuses it (r41).
	if st, out := e.do(e.owner, "PATCH", vmPath(id), map[string]any{"resume_on_boot": true}); st != 400 {
		t.Errorf("PATCH resume_on_boot: %d %v, want 400", st, out)
	}
	// Rich PATCH updates labels and idle_stop_s while preserving boat.secrets.
	st, patched := e.do(e.owner, "PATCH", vmPath(id), map[string]any{
		"labels":      map[string]string{"tier": "prod", "owner": "alice"},
		"idle_stop_s": int64(300),
	})
	if st != 200 {
		t.Fatalf("PATCH: %d %v", st, patched)
	}
	pl, _ := patched["labels"].(map[string]any)
	if pl["tier"] != "prod" || pl["owner"] != "alice" || pl[secretsLabel] != "KEEP_SEC" {
		t.Errorf("PATCH labels did not preserve %s: %v", secretsLabel, pl)
	}
	if patched["idle_stop_s"] != float64(300) || patched["resume_on_boot"] == true {
		t.Errorf("PATCH fields missing: %v", patched)
	}

	// Verify selection still holds after simulated store loss because boat.secrets remained intact.
	e.srv.store.Update(func(s *State) error { delete(s.VMSecrets, id); return nil })
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	if st, out := e.do(e.owner, "POST", vmPath(id, "start"), nil); st != 200 {
		t.Fatalf("start after PATCH: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	startEnv, _ := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if startEnv["KEEP_SEC"] != "keep-val" || startEnv["OTHER_SEC"] != nil {
		t.Errorf("secret selection broken after PATCH: %v", startEnv)
	}

	// POST /v1/vms/{id}/clone works as alias for fork, supporting ttl_s and no_env.
	st, cloned := e.do(e.owner, "POST", vmPath(id, "clone"), map[string]any{
		"name":   "cloned-vm",
		"ttl_s":  900,
		"no_env": true,
		"env":    map[string]string{"CLONE_VAR": "yes"},
	})
	if st != 201 || cloned["name"] != "cloned-vm" || cloned["ttl_s"] != float64(900) {
		t.Fatalf("clone: %d %v", st, cloned)
	}
	cid := cloned["id"].(string)
	if cl, _ := cloned["labels"].(map[string]any); cl[secretsLabel] != "" || cl[noEnvLabel] != "1" {
		t.Errorf("clone with no_env lacks durable no-secret labels: %v", cl)
	}
	e.boatd.mu.Lock()
	var cloneEnv map[string]any
	for _, op := range e.boatd.envOps {
		if op["vm"] == cid {
			cloneEnv, _ = op["vars"].(map[string]any)
		}
	}
	e.boatd.mu.Unlock()
	if cloneEnv["CLONE_VAR"] != "yes" || cloneEnv["KEEP_SEC"] != nil || cloneEnv["OTHER_SEC"] != nil {
		t.Errorf("clone with no_env leaked team secrets: %v", cloneEnv)
	}
}

func TestR23NoEnvOnCompatResumeAndCompatFork(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret"})
	e.do(e.owner, "POST", "/v1/environments", map[string]any{
		"name":       "default-env",
		"is_default": true,
		"env":        map[string]string{"ENV_DEFAULT": "from-default"},
	})

	bx := e.newSandbox(map[string]any{"ttlSeconds": 600})
	base := "/api/v1/sandboxes/" + bx
	id := e.nativeOf(bx)

	// r43: resume and fork take an environment; a running sandbox cannot switch
	// environments in place (409), and an unknown one is refused (404).
	if st, _, _ := e.compat(e.owner, "POST", base+"/resume", map[string]any{"environment": "default-env"}, nil); st != 409 {
		t.Errorf("resume of a running sandbox with environment: %d, want 409", st)
	}
	if st, _, _ := e.compat(e.owner, "POST", base+"/fork", map[string]any{"environment": "no-such-env"}, nil); st != 404 {
		t.Errorf("fork with an unknown environment: %d, want 404", st)
	}

	// compatResume with noEnv: true + ttlSeconds: 1800 injects no team secrets and no env vars.
	e.ok("POST", base+"/stop", nil, 202)
	res := e.ok("POST", base+"/resume", map[string]any{
		"noEnv":      true,
		"ttlSeconds": 1800,
		"env":        map[string]string{"RUN_VAR": "r1"},
	}, 202)
	if res["sandbox"].(map[string]any)["ttlSeconds"] != float64(1800) {
		t.Errorf("resume ttlSeconds: %v", res["sandbox"])
	}
	e.boatd.mu.Lock()
	rEnv, _ := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if rEnv["RUN_VAR"] != "r1" || rEnv["TEAM_SEC"] != nil || rEnv["ENV_DEFAULT"] != nil {
		t.Errorf("compatResume noEnv leaked secrets/env: %v", rEnv)
	}

	// compatFork with noEnv: true + ttlSeconds: null strips secrets permanently on child while parent keeps its selection.
	forkOut := e.ok("POST", base+"/fork", map[string]any{
		"noEnv":      true,
		"ttlSeconds": nil,
		"env":        map[string]string{"FORK_VAR": "f1"},
	}, 202)
	childBX := forkOut["id"].(string)
	childID := e.nativeOf(childBX)
	if forkOut["sandbox"].(map[string]any)["ttlSeconds"] != nil {
		t.Errorf("fork infinite ttlSeconds = %v, want nil", forkOut["sandbox"].(map[string]any)["ttlSeconds"])
	}
	// Stop and normal-resume child: child must STILL get zero team secrets!
	e.ok("POST", "/api/v1/sandboxes/"+childBX+"/stop", nil, 202)
	e.ok("POST", "/api/v1/sandboxes/"+childBX+"/resume", nil, 202)
	e.boatd.mu.Lock()
	childStartEnv, _ := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if childStartEnv["TEAM_SEC"] != nil || childStartEnv["ENV_DEFAULT"] != nil {
		t.Errorf("child normal resume after noEnv fork leaked secrets: %v (vm=%s)", childStartEnv, childID)
	}
	// Parent still has its 1800s TTL and TEAM_SEC on normal resume.
	parentSB := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any)
	if parentSB["ttlSeconds"] != float64(1800) {
		t.Errorf("parent ttlSeconds changed to %v, want 1800", parentSB["ttlSeconds"])
	}
	_ = id
}

func TestR23PassSecretsRestoreAndAtomicCompatEnvVar(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "v1"})

	envs := e.ok("POST", "/api/v1/environments", map[string]any{"name": "toggle-sec"}, 200)["environments"].([]any)
	envID := envs[0].(map[string]any)["id"].(string)

	// Toggle passSecrets: true -> false -> true.
	st, out, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+envID, map[string]any{"passSecrets": false}, nil)
	if st != 200 || out["environment"].(map[string]any)["passSecrets"] != false {
		t.Fatalf("disable passSecrets: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "PUT", "/api/v1/environments/"+envID, map[string]any{"passSecrets": true}, nil)
	if st != 200 || out["environment"].(map[string]any)["passSecrets"] != true {
		t.Fatalf("restore passSecrets: %d %v", st, out)
	}

	// Native conflict check: secrets + reset_secrets -> 400.
	var nativeID string
	for k := range e.srv.store.Snapshot().Environments {
		nativeID = k
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/environments/"+nativeID, map[string]any{"secrets": []string{}, "reset_secrets": true}); st != 400 {
		t.Errorf("conflicting secrets + reset_secrets accepted: %d", st)
	}

	// Concurrent PUT /api/v1/environments/{id}/vars/{key} must not lose updates.
	const workers = 20
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("CONC_%02d", i)
			val := fmt.Sprintf("val-%02d", i)
			st, _, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+envID+"/vars/"+key, map[string]any{"value": val}, nil)
			if st != 200 {
				t.Errorf("concurrent PUT %s: status %d", key, st)
			}
		}(i)
	}
	wg.Wait()

	gotEnv := e.srv.store.Snapshot().Environments[nativeID]
	for i := 0; i < workers; i++ {
		key := fmt.Sprintf("CONC_%02d", i)
		want := fmt.Sprintf("val-%02d", i)
		if gotEnv.Env[key] != want {
			t.Errorf("lost concurrent env var %s: got %q, want %q", key, gotEnv.Env[key], want)
		}
	}
}

func TestR23UnshareKimiCodeAliasAndWebhookSeparation(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.boat.example"
	bx := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + bx
	vmID := e.nativeOf(bx)

	privHost := e.ok("POST", base+"/host", map[string]any{"port": 3000}, 200)
	privURL, _ := url.Parse(privHost["url"].(string))
	oldSig := privURL.Query().Get("boat_sig")
	pubHost := e.ok("POST", base+"/host", map[string]any{"port": 8080, "public": true}, 200)
	if pubHost["access"] != "public" {
		t.Fatalf("expected public port before unshare: %v", pubHost)
	}

	genBefore := e.srv.store.Snapshot().RouteGens[vmID]
	unshSt, unshared, _ := e.compat(e.owner, "POST", base+"/unshare", nil, nil)
	if unshSt != 501 || errCode(unshared) != "not_supported" || unshared["ok"] != false {
		t.Fatalf("unshare response: %d %v", unshSt, unshared)
	}
	if genAfter := e.srv.store.Snapshot().RouteGens[vmID]; genAfter != genBefore {
		t.Errorf("unshare changed RouteGens[%s]: before=%d after=%d", vmID, genBefore, genAfter)
	}
	st, portsOut := e.do(e.owner, "GET", vmPath(vmID, "ports"), nil)
	if st != 200 {
		t.Fatalf("listPorts: %d %v", st, portsOut)
	}
	for _, raw := range portsOut["ports"].([]any) {
		pm := raw.(map[string]any)
		if portNum(pm["guest_port"]) == 8080 && pm["auth"] != "public" {
			t.Errorf("unsupported unshare changed public port: %v", pm)
		}
	}
	newPriv := e.ok("POST", base+"/host", map[string]any{"port": 3000}, 200)
	newURL, _ := url.Parse(newPriv["url"].(string))
	if newURL.Query().Get("boat_sig") != oldSig {
		t.Error("unsupported unshare rotated port token signature")
	}

	// Native sync and async prompt routes accept "kimi-code" -> "kimi".
	if st, _ := e.raw(e.owner, "POST", vmPath(vmID, "prompt"), map[string]any{"prompt": "hi kimi", "agent": "kimi-code"}); st != 200 {
		t.Fatalf("sync prompt kimi-code: %d", st)
	}
	e.boatd.mu.Lock()
	lastExec := e.boatd.execs[len(e.boatd.execs)-1]
	e.boatd.mu.Unlock()
	if cmd, _ := lastExec["cmd"].([]any); len(cmd) == 0 || cmd[0] != "kimi" {
		t.Errorf("sync prompt kimi-code ran %v, want kimi", cmd)
	}
	if st, run := e.do(e.owner, "POST", vmPath(vmID, "prompts"), map[string]any{"prompt": "async kimi", "agent": "kimi-code"}); st != 202 || run["agent"] != "kimi" {
		t.Fatalf("async prompt kimi-code: %d %v", st, run)
	}

	// Webhook format separation + testHook formatting per hook.Format.
	var mu sync.Mutex
	gotHeaders := map[string]http.Header{}
	gotBodies := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		b.ReadFrom(r.Body)
		mu.Lock()
		gotHeaders[r.URL.Path] = r.Header.Clone()
		gotBodies[r.URL.Path] = b.String()
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st, natHook := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": srv.URL + "/native", "events": []string{"vm.*"}})
	if st != 201 {
		t.Fatalf("create native hook: %d %v", st, natHook)
	}
	natID := natHook["id"].(string)

	bdHookResp := e.ok("POST", "/api/v1/webhooks", map[string]any{"name": "bd", "url": srv.URL + "/boatdev", "events": []string{"sandbox.ready"}}, 201)
	bdSecret := bdHookResp["secret"].(string)
	bdID := bdHookResp["webhook"].(map[string]any)["id"].(string)

	// GET /v1/webhooks excludes boatdev hooks; GET /api/v1/webhooks excludes native hooks.
	_, natList := e.do(e.owner, "GET", "/v1/webhooks", nil)
	natHooks := natList["webhooks"].([]any)
	if len(natHooks) != 1 || natHooks[0].(map[string]any)["id"] != natID {
		t.Errorf("GET /v1/webhooks leaked boatdev hook: %v", natHooks)
	}
	bdList := e.ok("GET", "/api/v1/webhooks", nil, 200)["webhooks"].([]any)
	if len(bdList) != 1 || bdList[0].(map[string]any)["id"] != bdID {
		t.Errorf("GET /api/v1/webhooks leaked native hook: %v", bdList)
	}

	// POST /v1/webhooks/{id}/test formats native vs boatdev deliveries per hook.Format.
	if st, _ := e.do(e.owner, "POST", "/v1/webhooks/"+natID+"/test", nil); st != 202 {
		t.Fatalf("test native hook: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/webhooks/"+bdID+"/test", nil); st != 202 {
		t.Fatalf("test boatdev hook: %d", st)
	}

	waitFor(t, "both test deliveries", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotHeaders["/native"] != nil && gotHeaders["/boatdev"] != nil
	})

	mu.Lock()
	nh, nb := gotHeaders["/native"], gotBodies["/native"]
	bh, bb := gotHeaders["/boatdev"], gotBodies["/boatdev"]
	mu.Unlock()

	if nh.Get("X-Boat-Event") != "webhook.test" || nh.Get("X-Boat-Signature") == "" || nh.Get("X-Ascii-Event") != "" {
		t.Errorf("native testHook headers wrong: %v (body=%s)", nh, nb)
	}
	if bh.Get("X-Ascii-Event") != "webhook.test" || bh.Get("X-Boat-Event") != "" {
		t.Errorf("boatdev testHook headers wrong: %v (body=%s)", bh, bb)
	}
	mac := hmac.New(sha256.New, []byte(bdSecret))
	mac.Write([]byte(bh.Get("X-Ascii-Delivery") + "." + bh.Get("X-Ascii-Timestamp") + "." + bb))
	if wantSig := "v1=" + hex.EncodeToString(mac.Sum(nil)); bh.Get("X-Ascii-Signature") != wantSig {
		t.Errorf("boatdev testHook signature = %q, want %q", bh.Get("X-Ascii-Signature"), wantSig)
	}
	var bdPayload map[string]any
	if err := json.Unmarshal([]byte(bb), &bdPayload); err != nil || bdPayload["type"] != "webhook.test" || bdPayload["createdAt"] == nil {
		t.Errorf("boatdev testHook payload wrong: %s (%v)", bb, err)
	}
}
