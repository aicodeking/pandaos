package main

import (
	"net/url"
	"strings"
	"testing"
)

// r48: free-text display names (boatd gets a DNS slug), environment vars pinned
// per sandbox (edits apply on upgrade or when a start/fork names the environment),
// and a fork that never takes the parent's user labels.

func (e *env) boatdName(id string) any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.vms[id]["name"]
}

func (e *env) lastCreateBody() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.creates[len(e.boatd.creates)-1]
}

func (e *env) lastForkBody() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.forks[len(e.boatd.forks)-1]
}

func (e *env) lastPatchBody() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.patches[len(e.boatd.patches)-1]
}

func (e *env) nativeVM(id string) map[string]any {
	e.t.Helper()
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if st != 200 {
		e.t.Fatalf("GET %s = %d %v", id, st, out)
	}
	return out
}

func TestR48SlugDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"api worker":               "api-worker",
		"  API  Worker! ":          "api-worker",
		"my-vm":                    "my-vm",
		"a--b":                     "a--b",  // already a slug: untouched
		"Ünï cödé":                 "n-c-d", // non-ASCII letters are not slug characters
		"!!!":                      "",
		strings.Repeat("a b ", 40): strings.TrimRight(strings.Repeat("a-b-", 16)[:63], "-"),
	} {
		if got := hostnameOf(in); got != want {
			t.Errorf("hostnameOf(%q) = %q, want %q", in, got, want)
		}
	}
	if got := batchHostname(strings.Repeat("x", 100)); len(got) != 60 {
		t.Errorf("batch hostname length = %d, want 60", len(got))
	}
	if got := batchDisplayName(strings.Repeat("é", 120), 14); len([]rune(got)) != 120 || !strings.HasSuffix(got, "-15") {
		t.Errorf("batch display name = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestR48ValidDisplayNames(t *testing.T) {
	e := setup(t)
	n := e.vmCount()
	for name, body := range map[string]any{
		"121 characters": strings.Repeat("n", 121),
		"newline":        "a\nb",
		"nul":            "a\x00b",
		"tab":            "a\tb",
		"blank":          "   ",
	} {
		if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": body}); st != 400 {
			t.Errorf("%s: create = %d %v, want 400", name, st, out)
		}
	}
	if e.vmCount() != n {
		t.Fatal("a refused create booted a VM")
	}
	id := e.createVM()
	for _, bad := range []string{"", "  ", strings.Repeat("n", 121), "x\ry"} {
		if st, out := e.do(e.owner, "PATCH", "/v1/vms/"+id, map[string]any{"name": bad}); st != 400 {
			t.Errorf("rename to %q = %d %v, want 400", bad, st, out)
		}
	}
	// 120 characters (multi-byte too) and a trimmed name are fine.
	long := strings.Repeat("é", 120)
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": long})
	if st != 201 || out["name"] != long {
		t.Fatalf("120-character name = %d %v", st, out["name"])
	}
	if st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "  web server  "}); st != 201 || out["name"] != "web server" {
		t.Fatalf("trimmed name = %d %v", st, out["name"])
	}
}

func TestR48CreateKeepsDisplayNameBoatdGetsSlug(t *testing.T) {
	e := setup(t)
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "api worker", "size": "small"})
	if st != 201 || out["name"] != "api worker" {
		t.Fatalf("create = %d %v", st, out)
	}
	id := out["id"].(string)
	if got := e.lastCreateBody()["name"]; got != "api-worker" {
		t.Fatalf("boatd create name = %v, want api-worker", got)
	}
	if got := e.boatdName(id); got != "api-worker" {
		t.Fatalf("boatd hostname = %v", got)
	}
	if got := e.nativeVM(id)["name"]; got != "api worker" {
		t.Fatalf("GET name = %v", got)
	}
	_, list := e.do(e.owner, "GET", "/v1/vms", nil)
	if vms := list["vms"].([]any); len(vms) != 1 || vms[0].(map[string]any)["name"] != "api worker" {
		t.Fatalf("list = %v", list)
	}
	// compat: the same name, and the name filters match the display name.
	e.newSandbox(map[string]any{"name": "other"})
	q := "?name=" + url.QueryEscape("api worker")
	boxes := e.ok("GET", "/api/v1/sandboxes"+q, nil, 200)["sandboxes"].([]any)
	if len(boxes) != 1 || boxes[0].(map[string]any)["name"] != "api worker" {
		t.Fatalf("compat ?name= = %v", boxes)
	}
	boxes = e.ok("GET", "/api/v1/sandboxes?namePrefix="+url.QueryEscape("api w"), nil, 200)["sandboxes"].([]any)
	if len(boxes) != 1 {
		t.Fatalf("compat ?namePrefix= = %v", boxes)
	}
	bx := e.ok("GET", "/api/v1/sandboxes/"+id, nil, 200)["sandbox"].(map[string]any)
	if bx["name"] != "api worker" {
		t.Fatalf("compat sandbox name = %v", bx["name"])
	}
	// A name with nothing usable in it: boatd names the VM by its id.
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "!!!"})
	if st != 201 || out["name"] != "!!!" {
		t.Fatalf("punctuation name = %d %v", st, out)
	}
	if got := e.lastCreateBody()["name"]; got != "" {
		t.Fatalf("boatd create name = %v, want \"\" (boatd falls back to the id)", got)
	}
}

func TestR48CompatCreateAndBatch(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(map[string]any{"name": "Compat Box"})
	if got := e.lastCreateBody()["name"]; got != "compat-box" {
		t.Fatalf("boatd name = %v", got)
	}
	if got := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)["name"]; got != "Compat Box" {
		t.Fatalf("compat name = %v", got)
	}
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "My Batch", "count": 2})
	if st != 201 {
		t.Fatalf("batch = %d %v", st, out)
	}
	if got := e.lastCreateBody()["name"]; got != "my-batch" {
		t.Fatalf("boatd batch name = %v", got)
	}
	vms := out["vms"].([]any)
	if len(vms) != 2 || vms[0].(map[string]any)["name"] != "My Batch-1" || vms[1].(map[string]any)["name"] != "My Batch-2" {
		t.Fatalf("batch names = %v", vms)
	}
}

func TestR48CreateFromSnapshotAndFork(t *testing.T) {
	e := setup(t)
	src := e.createVM()
	sid := e.r39Snapshot(src, "base")
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "From Snap", "snapshot": sid})
	if st != 201 || out["name"] != "From Snap" || e.lastCreateBody()["name"] != "from-snap" {
		t.Fatalf("from snapshot = %d %v / %v", st, out["name"], e.lastCreateBody()["name"])
	}
	st, out = e.do(e.owner, "POST", "/v1/vms/"+src+"/fork", map[string]any{"name": "Child One"})
	if st != 201 || out["name"] != "Child One" {
		t.Fatalf("fork = %d %v", st, out)
	}
	if got := e.lastForkBody()["name"]; got != "child-one" {
		t.Fatalf("boatd fork name = %v", got)
	}
	child := out["id"].(string)
	if got := e.nativeVM(child)["name"]; got != "Child One" {
		t.Fatalf("GET child name = %v", got)
	}
	if st, out = e.do(e.owner, "POST", "/v1/vms/"+src+"/fork", map[string]any{"name": strings.Repeat("n", 121)}); st != 400 {
		t.Fatalf("fork with a 121-character name = %d %v", st, out)
	}
	cst, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+src+"/fork", map[string]any{"name": "Compat Child"}, nil)
	if cst != 202 || cout["sandbox"].(map[string]any)["name"] != "Compat Child" {
		t.Fatalf("compat fork = %d %v", cst, cout)
	}
	if got := e.lastForkBody()["name"]; got != "compat-child" {
		t.Fatalf("boatd compat fork name = %v", got)
	}
}

func TestR48RenameUpdatesHostnameAndDisplay(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	st, out := e.do(e.owner, "PATCH", "/v1/vms/"+id, map[string]any{"name": "Build Bot #2"})
	if st != 200 || out["name"] != "Build Bot #2" {
		t.Fatalf("rename = %d %v", st, out)
	}
	if got := e.lastPatchBody()["name"]; got != "build-bot-2" {
		t.Fatalf("boatd rename = %v", got)
	}
	if got := e.nativeVM(id)["name"]; got != "Build Bot #2" {
		t.Fatalf("GET name = %v", got)
	}
	// compat PATCH
	cst, cout, _ := e.compat(e.owner, "PATCH", "/api/v1/sandboxes/"+id, map[string]any{"name": "Renamed Again"}, nil)
	if cst != 200 || cout["sandbox"].(map[string]any)["name"] != "Renamed Again" {
		t.Fatalf("compat rename = %d %v", cst, cout)
	}
	if got := e.boatdName(id); got != "renamed-again" {
		t.Fatalf("boatd hostname = %v", got)
	}
	// Nothing usable in the name: the hostname is the VM id.
	if st, out = e.do(e.owner, "PATCH", "/v1/vms/"+id, map[string]any{"name": "???"}); st != 200 || out["name"] != "???" {
		t.Fatalf("rename to punctuation = %d %v", st, out)
	}
	if got := e.boatdName(id); got != id {
		t.Fatalf("boatd hostname = %v, want the id %s", got, id)
	}
	// A patch without a name keeps the display name.
	if st, out = e.do(e.owner, "PATCH", "/v1/vms/"+id, map[string]any{"ttl_s": 120}); st != 200 || out["name"] != "???" {
		t.Fatalf("ttl patch = %d %v", st, out)
	}
}

// Two free-text names with one slug both work: boatd's per-team hostname
// uniqueness is met with the id's tail.
func TestR48RenameSlugCollision(t *testing.T) {
	e := setup(t)
	a := e.createVMWith(map[string]any{"name": "api-worker"})
	b := e.createVM()
	st, out := e.do(e.owner, "PATCH", "/v1/vms/"+b, map[string]any{"name": "api worker"})
	if st != 200 || out["name"] != "api worker" {
		t.Fatalf("colliding rename = %d %v", st, out)
	}
	host, _ := e.boatdName(b).(string)
	if host == "api-worker" || !strings.HasPrefix(host, "api-worker-") {
		t.Fatalf("hostname = %q, want api-worker-<tail>", host)
	}
	if got := e.nativeVM(a)["name"]; got != "api-worker" {
		t.Fatalf("the other VM's name = %v", got)
	}
	// A plain slug that collides is still a 409.
	c := e.createVM()
	if st, out = e.do(e.owner, "PATCH", "/v1/vms/"+c, map[string]any{"name": "api-worker"}); st != 409 {
		t.Fatalf("slug collision = %d %v, want 409", st, out)
	}
}

func TestR48DeleteForgetsName(t *testing.T) {
	e := setup(t)
	id := e.createVMWith(map[string]any{"name": "Gone Soon"})
	if e.srv.store.Snapshot().VMNames[id] != "Gone Soon" {
		t.Fatal("display name not stored")
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+id, nil); st != 200 && st != 204 {
		t.Fatalf("delete = %d %v", st, out)
	}
	if n, ok := e.srv.store.Snapshot().VMNames[id]; ok {
		t.Fatalf("name %q survived the VM", n)
	}
}

// ---- environment pins ----

func (e *env) putEnvVar(env, key, val string) {
	e.t.Helper()
	st, out := e.do(e.owner, "PUT", "/v1/environments/"+env, map[string]any{"env": map[string]string{key: val}})
	if st != 200 {
		e.t.Fatalf("edit environment = %d %v", st, out)
	}
}

func (e *env) guestVar(id, key string) any {
	return e.vmEnv(id)[key]
}

func (e *env) stopStart(id string, body map[string]any) {
	e.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", body); st != 200 {
		e.t.Fatalf("start = %d %v", st, out)
	}
}

func TestR48EnvironmentPin(t *testing.T) {
	e := setup(t)
	if st, out := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "web", "env": map[string]string{"FOO": "v1"}}); st != 201 {
		t.Fatalf("create environment = %d %v", st, out)
	}
	id := e.createVMWith(map[string]any{"environment": "web"})
	if got := e.guestVar(id, "FOO"); got != "v1" {
		t.Fatalf("FOO at create = %v", got)
	}
	vm := e.nativeVM(id)
	if vm["environment_version"] != float64(1) {
		t.Fatalf("environment_version = %v", vm["environment_version"])
	}
	bx := e.ok("GET", "/api/v1/sandboxes/"+id, nil, 200)["sandbox"].(map[string]any)
	if bx["environmentVersion"] != float64(1) {
		t.Fatalf("environmentVersion = %v", bx["environmentVersion"])
	}

	// An edit does not change a plain resume.
	e.putEnvVar("web", "FOO", "v2")
	e.stopStart(id, nil)
	if got := e.guestVar(id, "FOO"); got != "v1" {
		t.Fatalf("FOO after an edit and a plain resume = %v, want v1 (pinned)", got)
	}
	if v := e.nativeVM(id)["environment_version"]; v != float64(1) {
		t.Fatalf("environment_version after a plain resume = %v", v)
	}
	// compat versions: the latest (2) has no sandbox, version 1 has this one.
	envs := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any)
	vs := envs[0].(map[string]any)["versions"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["versionNumber"] != float64(2) || vs[0].(map[string]any)["sandboxCount"] != float64(0) ||
		vs[1].(map[string]any)["versionNumber"] != float64(1) || vs[1].(map[string]any)["sandboxCount"] != float64(1) {
		t.Fatalf("versions before the upgrade = %v", vs)
	}

	// Naming the environment at a start takes its current version.
	e.stopStart(id, map[string]any{"environment": "web"})
	if got := e.guestVar(id, "FOO"); got != "v2" {
		t.Fatalf("FOO after a start naming the environment = %v, want v2", got)
	}
	if v := e.nativeVM(id)["environment_version"]; v != float64(2) {
		t.Fatalf("environment_version after naming it = %v", v)
	}
	// ...and later plain starts keep that pin.
	e.putEnvVar("web", "FOO", "v3")
	e.stopStart(id, nil)
	if got := e.guestVar(id, "FOO"); got != "v2" {
		t.Fatalf("FOO after another edit and a plain resume = %v, want v2", got)
	}

	// A fork inherits the parent's pin, not the environment's current vars.
	st, fork := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, fork)
	}
	child := fork["id"].(string)
	if got := e.guestVar(child, "FOO"); got != "v2" {
		t.Fatalf("child FOO = %v, want v2 (the parent's pin)", got)
	}
	if v := fork["environment_version"]; v != float64(2) {
		t.Fatalf("child environment_version = %v", v)
	}
	e.stopStart(child, nil)
	if got := e.guestVar(child, "FOO"); got != "v2" {
		t.Fatalf("child FOO after a plain resume = %v, want v2", got)
	}
}

func TestR48EnvironmentUpgrade(t *testing.T) {
	e := setup(t)
	st, env := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "web", "env": map[string]string{"FOO": "v1"}})
	if st != 201 {
		t.Fatalf("create environment = %d %v", st, env)
	}
	stopped := e.createVMWith(map[string]any{"environment": "web"})
	live := e.createVMWith(map[string]any{"environment": "web"})
	plain := e.createVM() // no environment: untouched
	e.do(e.owner, "POST", "/v1/vms/"+stopped+"/stop", nil)
	e.putEnvVar("web", "FOO", "v2")

	cst, up, _ := e.compat(e.owner, "POST", "/api/v1/environments/"+envUUIDOf(t, e, "web")+"/upgrade", nil, nil)
	if cst != 200 || up["success"] != true || up["upgraded"] != float64(2) || up["upgradedLive"] != float64(1) ||
		up["upgradedStopped"] != float64(1) || up["failed"] != float64(0) || up["latestVersionId"] != envUUID(env["id"].(string), 2) {
		t.Fatalf("compat upgrade = %d %v", cst, up)
	}
	for _, id := range []string{stopped, live} {
		if v := e.nativeVM(id)["environment_version"]; v != float64(2) {
			t.Fatalf("%s environment_version = %v, want 2", id, v)
		}
	}
	if _, ok := e.nativeVM(plain)["environment_version"]; ok {
		t.Fatal("a VM without an environment reports environment_version")
	}
	// A live success acknowledges the guest update; a stopped one applies on resume.
	if got := e.guestVar(live, "FOO"); got != "v2" {
		t.Fatalf("live FOO right after the upgrade = %v, want v2", got)
	}
	e.stopStart(live, nil)
	e.stopStart(stopped, nil)
	for _, id := range []string{stopped, live} {
		if got := e.guestVar(id, "FOO"); got != "v2" {
			t.Fatalf("%s FOO after upgrade and start = %v, want v2", id, got)
		}
	}
	// compat versions are coherent: one version, two sandboxes.
	envs := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any)
	vs := envs[0].(map[string]any)["versions"].([]any)
	if len(vs) != 1 || vs[0].(map[string]any)["sandboxCount"] != float64(2) || vs[0].(map[string]any)["versionNumber"] != float64(2) {
		t.Fatalf("versions after the upgrade = %v", vs)
	}
	// A second upgrade has nothing to do (native route).
	st, nat := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || nat["upgraded"] != float64(0) || nat["version"] != float64(2) {
		t.Fatalf("native upgrade = %d %v", st, nat)
	}
	// Admin only, and an unknown environment is 404.
	if st, _ := e.do(e.memberKey("member"), "POST", "/v1/environments/web/upgrade", nil); st != 403 {
		t.Fatalf("member upgrade = %d, want 403", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/environments/nope/upgrade", nil); st != 404 {
		t.Fatalf("unknown environment upgrade = %d, want 404", st)
	}
}

func envUUIDOf(t *testing.T, e *env, name string) string {
	t.Helper()
	for _, x := range e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any) {
		if m := x.(map[string]any); m["name"] == name {
			return m["id"].(string)
		}
	}
	t.Fatalf("no compat environment %q", name)
	return ""
}

// A VM from before r48 (never pinned) replays the live environment until an
// upgrade or a start naming it pins it.
func TestR48UnpinnedVMFollowsEnvironmentUntilUpgrade(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "web", "env": map[string]string{"FOO": "v1"}})
	id := e.createVMWith(map[string]any{"environment": "web"})
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMEnvPins, id); return nil }); err != nil {
		t.Fatal(err)
	}
	e.putEnvVar("web", "FOO", "v2")
	e.stopStart(id, nil)
	if got := e.guestVar(id, "FOO"); got != "v2" {
		t.Fatalf("unpinned FOO = %v, want the live v2", got)
	}
	// Without a previous pin the API cannot certify a live policy removal.
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/environments/"+envUUIDOf(t, e, "web")+"/upgrade", nil, nil); st != 200 || out["upgraded"] != float64(1) {
		t.Fatalf("upgrade = %d %v", st, out)
	}
	e.putEnvVar("web", "FOO", "v3")
	e.stopStart(id, nil)
	if got := e.guestVar(id, "FOO"); got != "v2" {
		t.Fatalf("FOO after the upgrade pinned it = %v, want v2", got)
	}
}

// ---- fork labels ----

func (e *env) labelsOf(id string) map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	l, _ := e.boatd.vms[id]["labels"].(map[string]any)
	return l
}

func TestR48ForkDoesNotCopyUserLabels(t *testing.T) {
	e := setup(t)
	parent := e.createVMWith(map[string]any{"labels": map[string]string{"team": "core", "env": "prod"}})
	// native: omitted labels
	st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, out)
	}
	sent, ok := e.lastForkBody()["labels"].(map[string]any)
	if !ok {
		t.Fatalf("labels sent to boatd = %v, want an explicit map so boatd copies nothing", e.lastForkBody()["labels"])
	}
	for k := range sent {
		if !strings.HasPrefix(k, reservedLabels) {
			t.Fatalf("a user label %q was sent to boatd: %v", k, sent)
		}
	}
	child := out["id"].(string)
	for k := range e.labelsOf(child) {
		if !strings.HasPrefix(k, reservedLabels) {
			t.Fatalf("child has the parent's user label %q", k)
		}
	}
	if l, _ := out["labels"].(map[string]any); l["team"] != nil || l["env"] != nil {
		t.Fatalf("fork answer labels = %v", l)
	}
	// native: labels given
	st, out = e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"labels": map[string]string{"role": "child"}})
	if st != 201 {
		t.Fatalf("fork with labels = %d %v", st, out)
	}
	l := e.labelsOf(out["id"].(string))
	if l["role"] != "child" || l["team"] != nil || l["env"] != nil {
		t.Fatalf("child labels = %v, want only role", l)
	}
	// compat: omitted, then given
	cst, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+parent+"/fork", map[string]any{}, nil)
	if cst != 202 {
		t.Fatalf("compat fork = %d %v", cst, cout)
	}
	if lab, _ := cout["sandbox"].(map[string]any)["labels"].(map[string]any); len(lab) != 0 {
		t.Fatalf("compat fork labels = %v, want none", lab)
	}
	cst, cout, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+parent+"/fork", map[string]any{"labels": map[string]string{"a": "b"}}, nil)
	if cst != 202 {
		t.Fatalf("compat fork with labels = %d %v", cst, cout)
	}
	if lab, _ := cout["sandbox"].(map[string]any)["labels"].(map[string]any); len(lab) != 1 || lab["a"] != "b" {
		t.Fatalf("compat fork labels = %v, want {a: b}", lab)
	}
	// The parent keeps its own.
	if l := e.labelsOf(parent); l["team"] != "core" || l["env"] != "prod" {
		t.Fatalf("parent labels = %v", l)
	}
}

// The reserved boat.* labels still carry over (the secret selection of a fork).
func TestR48ForkKeepsReservedLabels(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/A", map[string]any{"value": "a", "inject": true})
	parent := e.createVMWith(map[string]any{"secrets": []string{"A"}, "labels": map[string]string{"user": "x"}})
	pl := e.labelsOf(parent)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, out)
	}
	cl := e.labelsOf(out["id"].(string))
	if cl[secretsLabel] == nil || cl[secretsLabel] != pl[secretsLabel] || cl["user"] != nil {
		t.Fatalf("child labels = %v, parent %v", cl, pl)
	}
}
