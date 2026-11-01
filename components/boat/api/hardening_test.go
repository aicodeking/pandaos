package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTeam has the operator create a team owned by a new user; returns the
// team id and the owner's token.
func (e *env) newTeam(name string) (string, string) {
	e.t.Helper()
	st, out := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": name, "owner": name + "-owner"})
	if st != 201 {
		e.t.Fatalf("create team: %d %v", st, out)
	}
	return out["id"].(string), out["owner"].(map[string]any)["key"].(map[string]any)["token"].(string)
}

func TestOperatorOnlyTeams(t *testing.T) {
	e := setup(t)
	if _, me := e.do(e.owner, "GET", "/v1/me", nil); me["operator"] != true {
		t.Fatalf("admin init user must be the operator: %v", me)
	}
	tid, owner := e.newTeam("tenant")
	if _, me := e.do(owner, "GET", "/v1/me", nil); me["operator"] == true || me["role"] != "owner" {
		t.Fatalf("tenant owner: %v", me)
	}
	if st, _ := e.do(owner, "POST", "/v1/teams", map[string]any{"name": "sneaky"}); st != 403 {
		t.Errorf("team owner (admin scope, not operator) created a team: %d", st)
	}
	_, m := e.do(owner, "POST", "/v1/teams/"+tid+"/members", map[string]any{"name": "mem"})
	member := m["key"].(map[string]any)["token"].(string)
	if st, _ := e.do(member, "POST", "/v1/teams", map[string]any{"name": "x"}); st != 403 {
		t.Errorf("member created a team: %d", st)
	}
	if st, _ := e.do(owner, "DELETE", "/v1/teams/"+tid, nil); st != 403 {
		t.Errorf("tenant deleted a team: %d", st)
	}
	// the operator's own read-only key cannot create teams either
	ro := e.newKey("vms:read")
	if st, _ := e.do(ro, "POST", "/v1/teams", map[string]any{"name": "x"}); st != 403 {
		t.Errorf("operator read-only key created a team: %d", st)
	}

	// delete: refused while VMs exist, then everything of the team goes
	st, vm := e.do(owner, "POST", "/v1/vms", map[string]any{"name": "t1"})
	if st != 201 {
		t.Fatal(st)
	}
	e.do(owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "v"})
	if st, _ := e.do(e.owner, "DELETE", "/v1/teams/"+tid, nil); st != 409 {
		t.Errorf("delete team with VMs: %d", st)
	}
	e.do(owner, "DELETE", "/v1/vms/"+vm["id"].(string), nil)
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+tid, nil); st != 200 {
		t.Fatalf("delete team: %d %v", st, out)
	}
	if st, _ := e.do(owner, "GET", "/v1/me", nil); st != 401 {
		t.Errorf("keys of a deleted team still work: %d", st)
	}
	snap := e.srv.store.Snapshot()
	for _, s := range snap.Secrets {
		if s.TeamID == tid {
			t.Error("secrets of a deleted team kept")
		}
	}
	if len(snap.Users) != 1 {
		t.Errorf("orphan users kept: %d", len(snap.Users))
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID(), nil); st != 409 {
		t.Errorf("operator deleted the team of its own key: %d", st)
	}
}

func TestTeamLimits(t *testing.T) {
	e := setup(t)
	e.srv.cfg.MaxKeysPerTeam = 2 // the bootstrap key is #1
	e.newKey()
	st, out := e.do(e.owner, "POST", "/v1/keys", map[string]any{})
	if st != 403 || out["error"].(map[string]any)["code"] != "limit_reached" {
		t.Errorf("third key: %d %v", st, out)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/teams/"+e.teamID()+"/members", map[string]any{"name": "bob"}); st != 403 {
		t.Errorf("member invite past the key limit: %d", st)
	}
	e.srv.cfg.MaxHooksPerTeam = 1
	if st, _ := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": "https://h.example/a"}); st != 201 {
		t.Fatalf("first hook: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": "https://h.example/b"}); st != 403 {
		t.Errorf("second hook: %d", st)
	}
}

func TestTeamRateLimit(t *testing.T) {
	e := setup(t)
	k2 := e.newKey()
	_, other := e.newTeam("other")
	e.srv.teamLim = newLimiter(0.001, 2)
	e.do(e.owner, "GET", "/v1/me", nil)
	e.do(k2, "GET", "/v1/me", nil)
	st, out := e.do(k2, "GET", "/v1/me", nil)
	if st != 429 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "team") {
		t.Errorf("keys of one team must share the team bucket: %d %v", st, out)
	}
	if st, _ := e.do(other, "GET", "/v1/me", nil); st != 200 {
		t.Errorf("other team limited: %d", st)
	}
}

func auditPaths(e *env) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(e.audit.String()), "\n") {
		var a auditEntry
		if json.Unmarshal([]byte(l), &a) == nil {
			out = append(out, a.Method+" "+a.Path)
		}
	}
	return out
}

func TestAuditSensitiveReads(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.audit.Reset()
	e.raw(e.owner, "GET", "/v1/vms", nil)
	e.raw(e.owner, "GET", "/v1/vms/"+id+"/files?path=/etc/hostname", nil)
	e.raw(e.owner, "GET", "/v1/secrets", nil)
	e.raw(e.owner, "GET", "/v1/vms/"+id+"/desktop", nil)
	got := strings.Join(auditPaths(e), "\n")
	for _, want := range []string{"GET /v1/vms/" + id + "/files", "GET /v1/secrets", "GET /v1/vms/" + id + "/desktop"} {
		if !strings.Contains(got, want) {
			t.Errorf("not audited: %s\n%s", want, got)
		}
	}
	if !strings.Contains(e.audit.String(), `"query":"path=/etc/hostname"`) {
		t.Errorf("file read must record which file: %s", e.audit.String())
	}
	if strings.Contains(got, "GET /v1/vms\n") || strings.HasSuffix(got, "GET /v1/vms") {
		t.Errorf("plain list should not be audited:\n%s", got)
	}
}

func TestAuditRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := openRotating(p, 100, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { now = now.Add(time.Second); return now }
	line := []byte(strings.Repeat("x", 59) + "\n") // 60 bytes: one per file
	for i := 0; i < 5; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	if n := len(r.rotated()); n != 2 {
		t.Errorf("kept %d rotated files, want 2", n)
	}
	if st, _ := os.Stat(p); st.Size() != 60 {
		t.Errorf("live file size %d", st.Size())
	}

	// age-based retention
	old := filepath.Join(dir, "audit.jsonl.20000101T000000.000000000Z")
	os.WriteFile(old, []byte("x\n"), 0o600)
	os.Chtimes(old, now.Add(-200*24*time.Hour), now.Add(-200*24*time.Hour))
	r2, _ := openRotating(p, 0, 0, 90*24*time.Hour)
	r2.Close()
	if _, err := os.Stat(old); err == nil {
		t.Error("file past max age was kept")
	}
}

func TestMasterKeySource(t *testing.T) {
	data := t.TempDir()
	c := common{dataDir: data}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if p, create, warn := c.masterKeyPath(); p != filepath.Join(data, "master.key") || !create || warn == "" {
		t.Errorf("fallback: %s %v %q", p, create, warn)
	}
	creds := t.TempDir()
	os.WriteFile(filepath.Join(creds, "boat-master-key"), []byte(randHex(32)), 0o400)
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	if p, create, warn := c.masterKeyPath(); p != filepath.Join(creds, "boat-master-key") || create || warn != "" {
		t.Errorf("credential: %s %v %q", p, create, warn)
	}
	if _, kr, err := c.open(); err != nil || kr == nil {
		t.Errorf("open with credential: %v", err)
	}
	// systemd's ACL grant shows up as group-readable; plain files stay strict
	os.Chmod(filepath.Join(creds, "boat-master-key"), 0o440)
	if _, err := LoadMasterKey(filepath.Join(creds, "boat-master-key"), false); err != nil {
		t.Errorf("credential with an ACL mask refused: %v", err)
	}
	plain := filepath.Join(t.TempDir(), "k")
	os.WriteFile(plain, []byte(randHex(32)), 0o440)
	if _, err := LoadMasterKey(plain, false); err == nil {
		t.Error("group-readable plain key file accepted")
	}
}

func envOf(m map[string]any) map[string]any {
	e, _ := m["env"].(map[string]any)
	return e
}

func TestPerVMSecretSelection(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/A", map[string]any{"value": "a"})
	e.do(e.owner, "PUT", "/v1/secrets/B", map[string]any{"value": "b"})
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"secrets": []string{"NOPE"}}); st != 400 {
		t.Errorf("unknown secret: %d", st)
	}
	_, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"secrets": []string{"A"}})
	id := vm["id"].(string)
	if env := envOf(e.boatd.vms[id]); env["A"] != "a" || env["B"] != nil {
		t.Errorf("create env: %v", env)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if env := envOf(e.boatd.starts[len(e.boatd.starts)-1]); env["A"] != "a" || env["B"] != nil {
		t.Errorf("start env: %v", env)
	}
	e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"})
	if env := envOf(e.boatd.execs[len(e.boatd.execs)-1]); env["B"] != nil {
		t.Errorf("prompt env leaks unselected secret: %v", env)
	}
	_, child := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", nil)
	e.do(e.owner, "POST", "/v1/vms/"+child["id"].(string)+"/start", nil)
	if env := envOf(e.boatd.starts[len(e.boatd.starts)-1]); env["B"] != nil || env["A"] != "a" {
		t.Errorf("fork must keep the parent's selection: %v", env)
	}
	_, none := e.do(e.owner, "POST", "/v1/vms", map[string]any{"secrets": []string{}})
	if env := envOf(e.boatd.vms[none["id"].(string)]); len(env) != 0 {
		t.Errorf("secrets: [] must inject nothing: %v", env)
	}
	e.do(e.owner, "POST", "/v1/vms/"+none["id"].(string)+"/start", nil)
	if env := envOf(e.boatd.starts[len(e.boatd.starts)-1]); len(env) != 0 {
		t.Errorf("start of a no-secrets VM: %v", env)
	}
	_, all := e.do(e.owner, "POST", "/v1/vms", map[string]any{})
	if env := envOf(e.boatd.vms[all["id"].(string)]); env["A"] != "a" || env["B"] != "b" {
		t.Errorf("default must stay all secrets: %v", env)
	}
	e.do(e.owner, "DELETE", "/v1/vms/"+id, nil)
	if _, ok := e.srv.store.Snapshot().VMSecrets[id]; ok {
		t.Error("selection kept after delete")
	}
	_, out := e.do(e.owner, "PUT", "/v1/secrets/A", map[string]any{"value": "a2"})
	if out["rotated"] != true || !strings.Contains(out["note"].(string), "snapshots") {
		t.Errorf("rotation note: %v", out)
	}
}

func TestPrivatePortRoutes(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if st, _ := e.do(e.owner, "GET", "/v1/vms/"+id+"/ports/8080/url", nil); st != 501 {
		t.Errorf("unconfigured: %d", st)
	}
	e.srv.cfg.VMDomain = "vm.boat.example"
	e.srv.cfg.PortURLTTL = time.Hour

	st, p := e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 8080})
	if st != 201 || p["auth"] != "private" || e.boatd.portReqs[0]["public"] != false {
		t.Fatalf("default must be private without a raw TCP port: %d %v %v", st, p, e.boatd.portReqs)
	}
	_, u := e.do(e.owner, "GET", "/v1/vms/"+id+"/ports/8080/url", nil)
	pu, err := url.Parse(u["url"].(string))
	if err != nil || pu.Scheme != "https" || pu.Host != "8080-"+id+".vm.boat.example" {
		t.Fatalf("url: %v", u)
	}
	exp := pu.Query().Get("boat_exp")
	var expN int64
	fmt.Sscan(exp, &expN)
	// gateway contract: hex(HMAC-SHA256(desktopKey, "route.<port>-<vmid>.<exp>"))
	if want := signHex([]byte("desktop-key"), "route.8080-"+id+"."+exp); pu.Query().Get("boat_sig") != want || u["token"] != exp+"."+want {
		t.Errorf("signature does not verify: %v", u)
	}
	if d := time.Until(time.Unix(expN, 0)); d < 59*time.Minute || d > time.Hour+time.Minute {
		t.Errorf("default ttl: %v", d)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/"+id+"/ports/8080/url?ttl_s=10", nil); st != 400 {
		t.Errorf("ttl too short: %d", st)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/"+id+"/ports/9999/url", nil); st != 404 {
		t.Errorf("unexposed port: %d", st)
	}

	st, p = e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 3000, "auth": "public"})
	if st != 201 || p["auth"] != "public" || e.boatd.portReqs[1]["public"] != true {
		t.Fatalf("public: %d %v", st, p)
	}
	_, u = e.do(e.owner, "GET", "/v1/vms/"+id+"/ports/3000/url", nil)
	if u["url"] != "https://3000-"+id+".vm.boat.example/" || u["token"] != nil {
		t.Errorf("public url must be plain: %v", u)
	}
	_, l := e.do(e.owner, "GET", "/v1/vms/"+id+"/ports", nil)
	auths := map[float64]any{}
	for _, x := range l["ports"].([]any) {
		m := x.(map[string]any)
		auths[m["guest_port"].(float64)] = m["auth"]
	}
	if auths[8080] != "private" || auths[3000] != "public" {
		t.Errorf("ports ls auth: %v", auths)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 1, "auth": "open"}); st != 400 {
		t.Errorf("bad auth: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+id+"/ports/3000", nil); st != 200 {
		t.Errorf("remove: %d", st)
	}
	_, other := e.newTeam("other")
	if st, _ := e.do(other, "GET", "/v1/vms/"+id+"/ports/8080/url", nil); st != 404 {
		t.Errorf("cross-team port url: %d", st)
	}
	e.srv.cfg.VMDomain = "http://vm.local:8742" // dev form keeps scheme and port
	if _, u := e.do(e.owner, "GET", "/v1/vms/"+id+"/ports/8080/url", nil); !strings.HasPrefix(u["url"].(string), "http://8080-"+id+".vm.local:8742/?") {
		t.Errorf("dev url: %v", u)
	}
}

func TestMigrateOperator(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := map[string]any{"users": map[string]any{
		"a": map[string]any{"id": "a", "name": "first", "created": old},
		"b": map[string]any{"id": "b", "name": "later", "created": old.Add(time.Hour)},
	}}
	b, _ := json.Marshal(raw)
	os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600)
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := st.Snapshot()
	if !snap.Users["a"].Operator || snap.Users["b"].Operator {
		t.Errorf("oldest user must become the only operator: %+v %+v", snap.Users["a"], snap.Users["b"])
	}
}

func TestAuditRotateRenameFailureKeepsLogging(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	r, err := openRotating(p, 10, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	r.Write([]byte("0123456789"))
	// a directory at the rotation target makes the rename fail
	os.Mkdir(p+".20260101T000000.000000000Z", 0o700)
	os.WriteFile(filepath.Join(p+".20260101T000000.000000000Z", "x"), nil, 0o600)
	if _, err := r.Write([]byte("more\n")); err == nil {
		t.Fatal("expected a rotate error")
	}
	if _, err := r.Write([]byte("")); err != nil {
		t.Errorf("file left unusable after a failed rotation: %v", err)
	}
	r.Close()
}
