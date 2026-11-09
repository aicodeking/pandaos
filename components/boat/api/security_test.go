package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func keyID(e *env, tok string) string {
	_, me := e.do(tok, "GET", "/v1/me", nil)
	return me["key_id"].(string)
}

// The council's PoC: a vms:read key rotated the owner's bootstrap key.
func TestScopedKeyCannotTouchWiderKey(t *testing.T) {
	e := setup(t)
	ro := e.newKey("vms:read")
	boot := keyID(e, e.owner)
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/keys/" + boot + "/rotate"},
		{"GET", "/v1/keys/" + boot + "/usage"},
		{"DELETE", "/v1/keys/" + boot},
	} {
		if st, out := e.do(ro, c.method, c.path, map[string]any{}); st != 404 {
			t.Errorf("%s %s with vms:read: %d %v", c.method, c.path, st, out)
		}
	}
	_, l := e.raw(ro, "GET", "/v1/keys", nil)
	if strings.Contains(string(l), boot) {
		t.Errorf("vms:read key lists the wider key: %s", l)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/me", nil); st != 200 {
		t.Errorf("owner key stopped working: %d", st)
	}
	// A key can still manage itself, and a key no wider than itself.
	if st, out := e.do(ro, "POST", "/v1/keys/"+keyID(e, ro)+"/rotate", map[string]any{"overlap_s": 60}); st != 200 {
		t.Errorf("self rotation: %d %v", st, out)
	}
	rw := e.newKey("vms:read", "vms:write")
	if st, _ := e.do(rw, "DELETE", "/v1/keys/"+keyID(e, ro), nil); st != 200 {
		t.Errorf("narrower key of the same user: %d", st)
	}
	// An admin-only key is not wider than itself, but owner's key has more.
	adm := e.newKey("admin")
	if st, _ := e.do(adm, "POST", "/v1/keys/"+boot+"/rotate", map[string]any{}); st != 404 {
		t.Errorf("admin-only key rotated a full key: %d", st)
	}
}

// Rotation hands over a working token: an admin must not become an owner, nor a
// co-owner the operator.
func TestRotationCannotClimbRoles(t *testing.T) {
	e := setup(t)
	team := e.teamID()
	mk := func(name, role string) string {
		st, m := e.do(e.owner, "POST", "/v1/teams/"+team+"/members", map[string]any{"name": name, "role": role})
		if st != 201 {
			t.Fatalf("add %s: %d", name, st)
		}
		return m["key"].(map[string]any)["token"].(string)
	}
	admin, owner2, member := mk("adm", "admin"), mk("own2", "owner"), mk("mem", "member")
	boot := keyID(e, e.owner)
	if st, _ := e.do(admin, "POST", "/v1/keys/"+keyID(e, owner2)+"/rotate", map[string]any{}); st != 404 {
		t.Errorf("admin rotated an owner's key: %d", st)
	}
	if st, _ := e.do(owner2, "POST", "/v1/keys/"+boot+"/rotate", map[string]any{}); st != 404 {
		t.Errorf("co-owner rotated the operator's key: %d", st)
	}
	if st, _ := e.do(admin, "POST", "/v1/keys/"+keyID(e, member)+"/rotate", map[string]any{}); st != 200 {
		t.Errorf("admin rotating a member's key: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/keys/"+keyID(e, owner2), nil); st != 200 {
		t.Errorf("operator deleting a co-owner's key: %d", st)
	}
}

func vmEnv(e *env, id string) map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	m, _ := e.boatd.vms[id]["env"].(map[string]any)
	return m
}

func importArchive(t *testing.T, e *env, tok, sid string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import", strings.NewReader("ARCHIVE:"+sid))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 201 {
		t.Fatalf("import: %d %v", resp.StatusCode, out)
	}
	return out["id"].(string)
}

func TestForeignDiskGetsNoSecrets(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "s3cret"})
	src := e.createVM()
	own := e.snapshotOf(src)
	imported := importArchive(t, e, e.owner, own)
	writeOnly := e.newKey("vms:write")
	create := func(tok string, body map[string]any) (int, string) {
		st, vm := e.do(tok, "POST", "/v1/vms", body)
		id, _ := vm["id"].(string)
		return st, id
	}

	// Its own disk: as before.
	if st, id := create(writeOnly, map[string]any{"snapshot": own}); st != 201 || vmEnv(e, id)["TOKEN"] != "s3cret" {
		t.Errorf("own snapshot: %d %v", st, vmEnv(e, id))
	}
	// An import: nothing, even for a VM start later on.
	st, id := create(writeOnly, map[string]any{"snapshot": imported})
	if st != 201 || vmEnv(e, id)["TOKEN"] != nil {
		t.Fatalf("imported snapshot got secrets: %d %v", st, vmEnv(e, id))
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if env := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any); env["TOKEN"] != nil {
		t.Errorf("start injected secrets into a foreign VM: %v", env)
	}
	// By name: only for a key that could read the secret from any VM anyway.
	if st, _ := create(writeOnly, map[string]any{"snapshot": imported, "secrets": []string{"TOKEN"}}); st != 403 {
		t.Errorf("vms:write key named a secret for a foreign disk: %d", st)
	}
	if st, named := create(e.owner, map[string]any{"snapshot": imported, "secrets": []string{"TOKEN"}}); st != 201 || vmEnv(e, named)["TOKEN"] != "s3cret" {
		t.Errorf("opt-in by name: %d %v", st, vmEnv(e, named))
	}
	// A default environment selecting the secret is not an opt-in.
	st, _ = e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "base", "is_default": true, "secrets": []string{"TOKEN"}})
	if st != 201 {
		t.Fatalf("environment: %d", st)
	}
	if st, viaEnv := create(writeOnly, map[string]any{"snapshot": imported}); st != 201 || vmEnv(e, viaEnv)["TOKEN"] != nil {
		t.Errorf("default environment leaked a secret: %d %v", st, vmEnv(e, viaEnv))
	}
	// The mark follows the disk: fork, then snapshot of that, then a new VM.
	st, fork := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{})
	if st != 201 {
		t.Fatalf("fork: %d", st)
	}
	forkID := fork["id"].(string)
	if !e.srv.store.Snapshot().ForeignVMs[forkID] {
		t.Error("fork lost the foreign mark")
	}
	if st, again := create(writeOnly, map[string]any{"snapshot": e.snapshotOf(forkID)}); st != 201 || vmEnv(e, again)["TOKEN"] != nil {
		t.Errorf("snapshot of a foreign VM got secrets: %d %v", st, vmEnv(e, again))
	}
	// A share code.
	_, other := e.newTeam("other")
	e.do(other, "PUT", "/v1/secrets/OTHER", map[string]any{"value": "o"})
	_, ln := e.do(e.owner, "POST", "/v1/vms/"+src+"/snapshots/"+own+"/shares", map[string]any{"ttl_s": 600})
	if st, fromCode := create(other, map[string]any{"from": ln["code"]}); st != 201 || vmEnv(e, fromCode)["OTHER"] != nil {
		t.Errorf("share code VM got the taker's secrets: %d %v", st, vmEnv(e, fromCode))
	}
}

func aiStatus(t *testing.T, proxyURL, tok string) int {
	t.Helper()
	resp, _ := aiCall(t, proxyURL, "/anthropic/v1/messages", "x-api-key", tok)
	return resp.StatusCode
}

// A foreign disk's init cannot spend the team's AI credit or read raw keys.
func TestForeignDiskNoAICredit(t *testing.T) {
	e, _, proxy := aiSetup(t)
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"})
	imported := importArchive(t, e, e.owner, e.snapshotOf(e.createVM()))
	_, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": imported})
	id := vm["id"].(string)
	e.boatd.mu.Lock()
	vars := e.boatd.envOps[len(e.boatd.envOps)-1]["vars"].(map[string]any)
	e.boatd.mu.Unlock()
	tok := vars["ANTHROPIC_API_KEY"].(string)
	if !strings.Contains(tok, id) {
		t.Fatalf("last env push is not this VM's: %v", vars)
	}
	if c := aiStatus(t, proxy.URL, tok); c != 403 {
		t.Errorf("foreign VM at the proxy: %d", c)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/vms/"+id+"/ai-access", map[string]any{"enabled": true}); st != 200 {
		t.Fatalf("opt in: %d", st)
	}
	if c := aiStatus(t, proxy.URL, tok); c != 200 {
		t.Errorf("after opt-in: %d", c)
	}
	// Proxy off: raw keys never reach a blocked VM.
	e.do(e.owner, "PUT", "/v1/ai/proxy", map[string]any{"enabled": false})
	n := len(e.boatd.envOps)
	if st, vm2 := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": imported}); st != 201 || envOpsHave(e.boatd.envOps[n:], "ANTHROPIC_API_KEY") || vmEnv(e, vm2["id"].(string))["ANTHROPIC_API_KEY"] != nil {
		t.Errorf("raw key on a foreign VM: %d %v", st, e.boatd.envOps[n:])
	}
}

// Finding 10: the snapshot holds the VM's proxy token; creating a share ends it.
func TestShareRotatesAIToken(t *testing.T) {
	e, _, proxy := aiSetup(t)
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"})
	id := e.createVM()
	old := e.boatd.envOps[0]["vars"].(map[string]any)["ANTHROPIC_API_KEY"].(string)
	if c := aiStatus(t, proxy.URL, old); c != 200 {
		t.Fatalf("token before the share: %d", c)
	}
	sid := e.snapshotOf(id)
	if st, sh := e.do(e.owner, "POST", "/v1/vms/"+id+"/snapshots/"+sid+"/shares", map[string]any{"ttl_s": 600}); st != 201 {
		t.Fatalf("share: %d %v", st, sh)
	}
	if c := aiStatus(t, proxy.URL, old); c != 401 {
		t.Errorf("the token on the shared snapshot still works: %d", c)
	}
	vars := e.boatd.envOps[len(e.boatd.envOps)-1]["vars"].(map[string]any)
	cur, _ := vars["ANTHROPIC_API_KEY"].(string)
	if cur == old || aiStatus(t, proxy.URL, cur) != 200 {
		t.Errorf("the running source VM has no working token: %v", vars)
	}
}

func TestStateFloodCaps(t *testing.T) {
	e := setup(t)
	big := strings.Repeat("x", 60<<10)
	put := func(i int) int {
		st, _ := e.do(e.owner, "PUT", fmt.Sprintf("/v1/secrets/S%d", i), map[string]any{"value": big})
		return st
	}
	accepted := 0
	for i := 0; i < 500 && put(i) == 200; i++ {
		accepted++
	}
	if accepted == 500 || accepted*(60<<10) > maxSecretBytesPerTeam {
		t.Errorf("accepted %d secrets of 60 KB", accepted)
	}
	if put(0) != 200 {
		t.Error("replacing a secret at the cap must still work")
	}
	st, out := e.do(e.owner, "PUT", "/v1/secrets/ONE_MORE", map[string]any{"value": big})
	if st != 403 || out["error"].(map[string]any)["code"] != "limit_reached" {
		t.Errorf("past the cap: %d %v", st, out)
	}
	// A different team is not affected.
	_, other := e.newTeam("other")
	if st, _ := e.do(other, "PUT", "/v1/secrets/ONE", map[string]any{"value": big}); st != 200 {
		t.Errorf("other team: %d", st)
	}
	// Count cap, with small values.
	for i := 0; i < maxSecretsPerTeam; i++ {
		e.srv.store.Update(func(s *State) error {
			k := secretKey("t2", fmt.Sprint(i))
			s.Secrets[k] = &Secret{TeamID: "t2", Sealed: "x"}
			return nil
		})
	}
	if secretFits(e.srv.store.Snapshot(), "t2", secretKey("t2", "new"), 1) {
		t.Error("count cap not applied")
	}

	// Shares: expired ones free their slot.
	id := e.createVM()
	sid := e.snapshotOf(id)
	past := time.Now().Add(-time.Hour)
	e.srv.store.Update(func(s *State) error {
		for i := 0; i < maxSharesPerTeam; i++ {
			s.SnapShares[fmt.Sprint("sh", i)] = &SnapShare{ID: fmt.Sprint("sh", i), TeamID: e.teamID(), Expires: &past}
		}
		return nil
	})
	base := "/v1/vms/" + id + "/snapshots/" + sid + "/shares"
	if st, _ := e.do(e.owner, "POST", base, map[string]any{"ttl_s": 600}); st != 201 {
		t.Errorf("expired shares must not count: %d", st)
	}
	e.srv.store.Update(func(s *State) error {
		for i := 1; i < maxSharesPerTeam; i++ {
			s.SnapShares[fmt.Sprint("live", i)] = &SnapShare{ID: fmt.Sprint("live", i), TeamID: e.teamID()}
		}
		return nil
	})
	if st, _ := e.do(e.owner, "POST", base, map[string]any{"ttl_s": 600}); st != 403 {
		t.Errorf("share past the cap: %d", st)
	}

	// Environments: bytes across the team.
	script := strings.Repeat("y", maxSetupScript)
	n := 0
	for ; n < 40; n++ {
		if st, _ := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": fmt.Sprintf("e%d", n), "setup_script": script}); st != 201 {
			break
		}
	}
	if n*maxSetupScript > maxEnvBytesPerTeam || n == 40 {
		t.Errorf("environments accepted: %d of %d bytes", n, maxSetupScript)
	}
	// Updating is the same road: grow the smallest within the cap, then past it.
	e.do(e.owner, "DELETE", "/v1/environments/e0", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "tiny"}); st != 201 {
		t.Fatalf("tiny environment: %d", st)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/environments/tiny", map[string]any{"setup_script": script}); st != 200 {
		t.Errorf("update within the cap: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "tiny2"}); st != 201 {
		t.Fatalf("tiny2: %d", st)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/environments/tiny2", map[string]any{"setup_script": script}); st != 403 {
		t.Errorf("update past the cap: %d", st)
	}
}

func TestCommandHistoryCapsArgv(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	huge := strings.Repeat("z", 512<<10)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"echo", huge}}); st/100 != 2 {
		t.Fatalf("start: %d %v", st, out)
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if len(raw) > 100<<10 {
		t.Errorf("a 512 KiB argv grew state.json to %d bytes", len(raw))
	}
	if got := capArgv(make([]string, 1<<16)); len(got) > maxRecordedArgvBytes+1 {
		t.Errorf("empty args are not capped: %d", len(got))
	}
}

// Auth must not wait for a state rewrite.
func TestReadsDoNotWaitForWriter(t *testing.T) {
	e := setup(t)
	entered, leave, written := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() { close(leave); <-written }() // the write ends before the temp dir goes
	go func() {
		defer close(written)
		e.srv.store.Update(func(*State) error { close(entered); <-leave; return nil })
	}()
	<-entered
	done := make(chan *principal, 1)
	go func() { done <- e.srv.authenticate("Bearer " + e.owner) }()
	select {
	case p := <-done:
		if p == nil {
			t.Error("owner key rejected during a write")
		}
	case <-time.After(2 * time.Second):
		t.Error("authenticate is waiting on a writer")
	}
}

func TestStateSealedAtRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	kr, _ := NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err := os.WriteFile(path, []byte(`{"environments":{"e":{"id":"e","setup_script":"OLD-PLAINTEXT"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := OpenSealedStore(dir, kr)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().Environments["e"].SetupScript != "OLD-PLAINTEXT" {
		t.Error("plaintext state not read")
	}
	if b, _ := os.ReadFile(path); bytes.Contains(b, []byte("OLD-PLAINTEXT")) {
		t.Error("plaintext state left on disk after open")
	}
	err = st.Update(func(s *State) error {
		s.Prompts["v"] = []*PromptRun{{Prompt: "MY-PROMPT"}}
		s.Commands["v"] = []CommandRecord{{Cmd: []string{"MY-ARGV"}}}
		s.Environments["e"].Env = map[string]string{"K": "MY-ENV"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, m := range []string{"MY-PROMPT", "MY-ARGV", "MY-ENV", "OLD-PLAINTEXT"} {
		if bytes.Contains(b, []byte(m)) {
			t.Errorf("%s is plaintext in state.json", m)
		}
	}
	back, err := OpenSealedStore(dir, kr)
	if err != nil || back.Snapshot().Environments["e"].Env["K"] != "MY-ENV" {
		t.Errorf("reopen: %v", err)
	}
	wrong, _ := NewKeyring(bytes.Repeat([]byte{8}, 32))
	if _, err := OpenSealedStore(dir, wrong); err == nil {
		t.Error("opened with the wrong key")
	}
	if _, err := OpenStore(dir); err == nil {
		t.Error("opened sealed state without a key")
	}
}

func putFile(t *testing.T, e *env, tok, id string, body io.Reader, size int64) int {
	t.Helper()
	req, _ := http.NewRequest("PUT", e.api.URL+"/v1/vms/"+id+"/files?path=/f", body)
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestUploadIsNotBuffered(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.boatd.discardPuts = true
	const size = 64 << 20
	payload := make([]byte, size)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if st := putFile(t, e, e.owner, id, bytes.NewReader(payload), size); st != 200 {
		t.Fatalf("upload: %d", st)
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/2 {
		t.Errorf("a %d byte upload allocated %d bytes", size, alloc)
	}
	if e.boatd.putBytes != size {
		t.Errorf("boatd got %d bytes", e.boatd.putBytes)
	}
}

// A body past the limit or cut short never reaches the guest: boatd would commit
// its first part as the whole file.
func TestUploadFailureNeverReachesGuest(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.srv.cfg.MaxFileBytes = 1024
	chunked := io.MultiReader(strings.NewReader(strings.Repeat("a", 2048)))
	if st := putFile(t, e, e.owner, id, chunked, -1); st != 413 {
		t.Errorf("chunked past the limit: %d", st)
	}
	if st := putFile(t, e, e.owner, id, strings.NewReader(strings.Repeat("a", 2048)), 2048); st != 413 {
		t.Errorf("declared past the limit: %d", st)
	}
	c, err := net.Dial("tcp", strings.TrimPrefix(e.api.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "PUT /v1/vms/%s/files?path=/f HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/octet-stream\r\nContent-Length: 500\r\n\r\npartial", id, e.owner)
	c.Close()
	time.Sleep(200 * time.Millisecond)
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if b, ok := e.boatd.files[id+":/f"]; ok {
		t.Errorf("guest got %q", b)
	}
}

func TestInFlightCaps(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	_, other := e.newTeam("other")
	otherVM := func() string {
		_, vm := e.do(other, "POST", "/v1/vms", map[string]any{})
		return vm["id"].(string)
	}()
	team := e.teamID()
	e.srv.uploads = newInflight(1)
	e.srv.streams = newInflight(1)
	release, _ := e.srv.uploads.acquire(team)
	if st := putFile(t, e, e.owner, id, strings.NewReader("x"), 1); st != 429 {
		t.Errorf("upload past the cap: %d", st)
	}
	if st := putFile(t, e, other, otherVM, strings.NewReader("x"), 1); st != 200 {
		t.Errorf("another team's upload: %d", st)
	}
	release()
	if st := putFile(t, e, e.owner, id, strings.NewReader("x"), 1); st != 200 {
		t.Errorf("upload after release: %d", st)
	}
	release, _ = e.srv.streams.acquire(team)
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/vms/" + id + "/exec"},
		{"POST", "/v1/vms/" + id + "/prompt"},
		{"GET", "/v1/vms/" + id + "/files?path=/f"},
	} {
		if st, _ := e.raw(e.owner, c.method, c.path, map[string]any{"cmd": []string{"echo"}, "prompt": "p"}); st != 429 {
			t.Errorf("%s %s past the cap: %d", c.method, c.path, st)
		}
	}
	// Bounded: without the cap this stream would never end.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evReq, _ := http.NewRequestWithContext(ctx, "GET", e.api.URL+"/v1/events", nil)
	evReq.Header.Set("Authorization", "Bearer "+e.owner)
	evResp, err := http.DefaultClient.Do(evReq)
	if err != nil {
		t.Fatal(err)
	}
	evResp.Body.Close()
	if evResp.StatusCode != 429 {
		t.Errorf("events past the cap: %d", evResp.StatusCode)
	}
	if st, _ := e.raw(other, "POST", "/v1/vms/"+otherVM+"/exec", map[string]any{"cmd": []string{"echo", "x"}}); st != 200 {
		t.Errorf("another team's exec: %d", st)
	}
	// Link holders have their own bucket, so they cannot starve the sharing team.
	sid := e.snapshotOf(id)
	_, ln := e.do(e.owner, "POST", "/v1/vms/"+id+"/snapshots/"+sid+"/shares", map[string]any{"ttl_s": 600})
	link := "/v1/snapshot-links/" + ln["code"].(string) + "/download"
	if st, _ := e.raw("", "GET", link, nil); st != 200 {
		t.Errorf("link with the team's streams in use: %d", st)
	}
	relLink, _ := e.srv.streams.acquire("link:" + team)
	if st, _ := e.raw("", "GET", link, nil); st != 429 {
		t.Errorf("link past its cap: %d", st)
	}
	relLink()
	release()
	if len(e.srv.streams.n) != 0 {
		t.Errorf("released keys stay in memory: %v", e.srv.streams.n)
	}
}

func TestAIProxyVMCap(t *testing.T) {
	e, _, proxy := aiSetup(t)
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"})
	id := e.createVM()
	tok := e.boatd.envOps[0]["vars"].(map[string]any)["ANTHROPIC_API_KEY"].(string)
	e.srv.aiInflight = newInflight(1)
	release, _ := e.srv.aiInflight.acquire(id)
	if c := aiStatus(t, proxy.URL, tok); c != 429 {
		t.Errorf("past the VM's cap: %d", c)
	}
	release()
	if c := aiStatus(t, proxy.URL, tok); c != 200 {
		t.Errorf("after release: %d", c)
	}
}

func TestLimiterEvictsRefilledBuckets(t *testing.T) {
	now := time.Now()
	l := newLimiter(1, 2)
	l.now = func() time.Time { return now }
	for i := 0; i < 1000; i++ {
		l.allow(fmt.Sprintf("link:10.0.%d.%d", i>>8, i&255))
	}
	l.allow("busy")
	l.allow("busy")
	// Two tokens at 1/s are back after 2 s, so only the new caller remains.
	now = now.Add(time.Hour)
	l.allow("new")
	if len(l.buckets) != 1 {
		t.Errorf("%d buckets after an idle hour", len(l.buckets))
	}
	// A bucket still in debt is kept: dropping it would refill it early.
	l2 := newLimiter(0.001, 2)
	l2.now = l.now
	l2.allow("y")
	l2.allow("y")
	now = now.Add(limiterSweepEvery)
	l2.allow("z")
	if ok, _ := l2.allow("y"); ok {
		t.Error("a sweep refilled a bucket that was still limited")
	}
}

func TestDenyPrivateSpecialRanges(t *testing.T) {
	for _, a := range []string{
		"192.0.0.1:443", "198.18.0.1:443", "198.19.255.255:443", "168.63.129.16:80", "0.1.2.3:80", "240.0.0.1:80",
		"255.255.255.255:80", "192.0.2.1:80", "203.0.113.9:80", "[::ffff:10.0.0.1]:80", "[::ffff:169.254.169.254]:80",
		"[::ffff:198.18.0.1]:80", "[::a00:1]:80", "[64:ff9b::a00:1]:80", "[2002:a00:1::]:80", "[2001:0:a00:1::]:80",
		"[fd00::1]:80", "[fe80::1%25eth0]:80", "[fe80::1]:80", "[fec0::1]:80", "[ff02::1]:80", "[::]:80", "[2001:db8::1]:80",
	} {
		if denyPrivate("tcp", a, nil) == nil {
			t.Errorf("%s should be denied", a)
		}
	}
	for _, a := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443", "[::ffff:93.184.216.34]:443", "168.63.129.17:443"} {
		if err := denyPrivate("tcp", a, nil); err != nil {
			t.Errorf("%s denied: %v", a, err)
		}
	}
}

// The check is on the dialed address, so a hostname resolving to a private
// address (DNS rebinding at connect time) fails too, not only a literal one.
func TestWebhookDialChecksResolvedAddress(t *testing.T) {
	d := newDispatcher(nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost:9/x", nil)
	_, err := d.client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("localhost: %v", err)
	}
}

func TestDeleteTeamDeletesSnapshots(t *testing.T) {
	e := setup(t)
	tid, owner := e.newTeam("tenant")
	_, vm := e.do(owner, "POST", "/v1/vms", map[string]any{})
	id := vm["id"].(string)
	_, snap := e.do(owner, "POST", "/v1/vms/"+id+"/snapshots", map[string]any{"name": "s"})
	imp := importArchive(t, e, owner, snap["id"].(string))
	mine := e.snapshotOf(e.createVM())
	e.do(owner, "DELETE", "/v1/vms/"+id, nil)
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+tid, nil); st != 200 {
		t.Fatalf("delete team: %d %v", st, out)
	}
	if e.boatd.snaps[imp] != nil {
		t.Error("deleted team's imported snapshot left behind")
	}
	if e.boatd.snaps[mine] == nil {
		t.Error("another team's snapshot deleted")
	}
}

func TestPortTokenRevocation(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.example"
	e.srv.cfg.PortURLTTL = time.Hour
	id := e.createVM()
	_, other := e.newTeam("other")
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 8080}); st != 201 {
		t.Fatalf("add port: %d", st)
	}
	urlPath := "/v1/vms/" + id + "/ports/8080/url"
	_, u := e.do(e.owner, "GET", urlPath, nil)
	var exp int64
	var sig string
	fmt.Sscanf(strings.Replace(u["token"].(string), ".", " ", 1), "%d %s", &exp, &sig)
	label := "8080-" + id
	if sig != signHex([]byte("desktop-key"), fmt.Sprintf("route.%s.%d", label, exp)) {
		t.Error("generation 0 must keep the original payload (the gateway's)")
	}
	if st, _ := e.do(other, "POST", "/v1/vms/"+id+"/port-tokens/revoke", nil); st != 404 {
		t.Errorf("another team revoked: %d", st)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/port-tokens/revoke", nil); st != 200 || out["generation"] != 1.0 {
		t.Fatalf("revoke: %d %v", st, out)
	}
	_, u = e.do(e.owner, "GET", urlPath, nil)
	fmt.Sscanf(strings.Replace(u["token"].(string), ".", " ", 1), "%d %s", &exp, &sig)
	if sig != signHex([]byte("desktop-key"), fmt.Sprintf("route.%s.g1.%d", label, exp)) {
		t.Error("a new token must carry the new generation (the gateway's payload)")
	}
	// What the gateway polls: loopback only, never through Caddy.
	if st, b := e.raw("", "GET", "/internal/route-gens", nil); st != 200 || !strings.Contains(string(b), `"`+id+`":1`) {
		t.Errorf("route gens: %d %s", st, b)
	}
	req, _ := http.NewRequest("GET", e.api.URL+"/internal/route-gens", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("route gens through a proxy: %d", resp.StatusCode)
	}
	// Removing the port revokes too; a later add does not revive old tokens.
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+id+"/ports/8080", nil); st != 200 {
		t.Fatalf("remove port: %d", st)
	}
	if g := e.srv.store.Snapshot().RouteGens[id]; g != 2 {
		t.Errorf("generation after remove: %d", g)
	}
	e.do(e.owner, "DELETE", "/v1/vms/"+id, nil)
	if _, ok := e.srv.store.Snapshot().RouteGens[id]; ok {
		t.Error("a deleted VM stays in the table")
	}
}
