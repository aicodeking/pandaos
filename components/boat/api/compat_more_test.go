package main

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var reUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// host, hostedPorts, desktop and sshkey map onto the native port routes,
// desktop links and ssh.
func TestCompatHostDesktopSSHKey(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.boat.example"
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	vm := e.srv.store.Snapshot().CompatIDs[id].VM

	h := e.ok("POST", base+"/host", map[string]any{"port": 3000}, 200)
	u, _ := url.Parse(h["url"].(string))
	if h["type"] != "port.hosted" || h["access"] != "private" || h["isProtected"] != true || u.Host != "3000-"+vm+".vm.boat.example" ||
		u.Query().Get("boat_sig") == "" {
		t.Errorf("private host: %v", h)
	}
	pub := e.ok("POST", base+"/host", map[string]any{"port": 8081, "public": true}, 200)
	if pub["access"] != "public" || pub["isProtected"] != false || strings.Contains(pub["url"].(string), "boat_sig") {
		t.Errorf("public host: %v", pub)
	}
	for _, bad := range []map[string]any{{"port": 0}, {"port": 3000, "localhost": true}} {
		if st, out, _ := e.compat(e.owner, "POST", base+"/host", bad, nil); st != 400 || out["ok"] != false {
			t.Errorf("host %v: %d %v", bad, st, out)
		}
	}

	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "hp",
		"hostedPorts": []any{map[string]any{"port": 5173}, map[string]any{"port": 9000, "public": true}}}, nil)
	hp, _ := out["hostedPorts"].([]any)
	if st != 202 || len(hp) != 2 || hp[0].(map[string]any)["port"] != float64(5173) || hp[1].(map[string]any)["access"] != "public" {
		t.Errorf("hostedPorts: %d %v", st, out)
	}
	for _, bad := range []map[string]any{
		{"hostedPorts": []any{map[string]any{"port": 1}, map[string]any{"port": 1}}},
		{"hostedPorts": []any{map[string]any{"port": 1}}, "count": 2},
	} {
		if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", bad, nil); st != 400 || !strings.HasSuffix(errCode(out), "hosted_ports") {
			t.Errorf("hostedPorts %v: %d %v", bad, st, out)
		}
	}

	d := e.ok("POST", base+"/desktop?vnc=1", map[string]any{}, 200)
	if d["type"] != "desktop.url" || !strings.HasPrefix(d["desktopUrl"].(string), "https://boat.example/desktop/"+vm+"?exp=") ||
		d["provisioning"] != false || d["mode"] != "vnc" {
		t.Errorf("desktop: %v", d)
	}
	if st, _, _ := e.compat(e.owner, "POST", base+"/desktop", map[string]any{"publicAccess": true}, nil); st != 400 {
		t.Errorf("publicAccess: %d", st)
	}

	k := e.ok("POST", base+"/sshkey", map[string]any{"key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q test"}, 200)
	if k["type"] != "ssh_key.configured" || k["sshUser"] == nil {
		t.Errorf("sshkey: %v", k)
	}
	if !strings.Contains(string(e.boatd.files[vm+":/home/dev/.ssh/authorized_keys"]), "AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN") {
		t.Error("sshkey did not authorize the key")
	}
	if st, _, _ := e.compat(e.owner, "POST", base+"/sshkey", map[string]any{"key": "not a key"}, nil); st != 400 {
		t.Errorf("bad key: %d", st)
	}
}

// Snapshot listings with boat.dev's UUID ids, latest, delete-all (captures
// only), delete one, named snapshots and share codes.
func TestCompatSnapshotsNamedShares(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	vm := e.srv.store.Snapshot().CompatIDs[id].VM
	if l := e.ok("GET", base+"/snapshots/latest", nil, 200); l["snapshot"] != nil {
		t.Errorf("latest of none: %v", l)
	}
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", map[string]any{})
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", map[string]any{})

	named := e.ok("POST", "/api/v1/named-snapshots", map[string]any{"sandboxId": id, "name": "base-img"}, 202)
	if named["type"] != "snapshot.named.saving" || named["snapshot"].(map[string]any)["name"] != "base-img" {
		t.Fatalf("named save: %v", named)
	}
	if g := e.ok("GET", "/api/v1/named-snapshots/base-img", nil, 200); g["type"] != "snapshot.named.info" {
		t.Errorf("named get: %v", g)
	}
	if l := e.ok("GET", "/api/v1/named-snapshots", nil, 200); l["type"] != "snapshot.named.list" || len(l["snapshots"].([]any)) != 1 {
		t.Errorf("named list: %v", l)
	}

	list := e.ok("GET", base+"/snapshots", nil, 200)
	snaps := list["snapshots"].([]any)
	first := snaps[0].(map[string]any)
	if len(snaps) != 3 || !reUUID.MatchString(first["id"].(string)) || first["sandboxId"] != id || first["status"] != "completed" {
		t.Fatalf("sandbox snapshots: %v", list)
	}
	if l := e.ok("GET", base+"/snapshots?limit=1", nil, 200); len(l["snapshots"].([]any)) != 1 || l["pageInfo"].(map[string]any)["hasMore"] != true {
		t.Errorf("limit: %v", l)
	}
	if l := e.ok("GET", "/api/v1/snapshots", nil, 200); len(l["snapshots"].([]any)) != 3 {
		t.Errorf("team snapshots: %v", l)
	}
	if l := e.ok("GET", base+"/snapshots/latest", nil, 200); l["snapshot"] == nil {
		t.Errorf("latest: %v", l)
	}

	share := e.ok("POST", "/api/v1/saved-snapshots/base-img/shares", nil, 201)
	sh := share["share"].(map[string]any)
	if !strings.HasPrefix(share["shareCode"].(string), "bsh_") || !strings.HasPrefix(sh["id"].(string), "shr_") || sh["status"] != "active" {
		t.Fatalf("share: %v", share)
	}
	if l := e.ok("GET", "/api/v1/saved-snapshots/base-img/shares", nil, 200); len(l["shares"].([]any)) != 1 {
		t.Errorf("shares: %v", l)
	}
	if rv := e.ok("DELETE", "/api/v1/saved-snapshots/base-img/shares/"+sh["id"].(string), nil, 200); rv["share"].(map[string]any)["status"] != "revoked" {
		t.Errorf("revoke: %v", rv)
	}
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/saved-snapshots/nope/shares", nil, nil); st != 404 {
		t.Errorf("share of no snapshot: %d", st)
	}

	if st, _, _ := e.compat(e.owner, "DELETE", base+"/snapshots", nil, nil); st != 409 {
		t.Errorf("delete-all without confirmation: %d", st)
	}
	confirm := map[string]string{"X-Ascii-Confirm-Delete": id}
	st, del, _ := e.compat(e.owner, "DELETE", base+"/snapshots", nil, confirm)
	if ops, _ := del["operations"].([]any); st != 202 || len(ops) != 2 || ops[0].(map[string]any)["kind"] != "snapshot" {
		t.Errorf("delete-all: %d %v", st, del)
	}
	if st, del, _ = e.compat(e.owner, "DELETE", base+"/snapshots", nil, confirm); st != 202 || len(del["operations"].([]any)) != 0 {
		t.Errorf("second delete-all: %d %v", st, del)
	}
	left := e.ok("GET", base+"/snapshots", nil, 200)["snapshots"].([]any)
	if len(left) != 1 {
		t.Fatalf("delete-all must keep the saved snapshot only: %v", left)
	}
	if d := e.ok("DELETE", "/api/v1/named-snapshots/base-img", nil, 200); d["type"] != "snapshot.named.deleted" {
		t.Errorf("named delete: %v", d)
	}
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", map[string]any{})
	one := e.ok("GET", base+"/snapshots/latest", nil, 200)["snapshot"].(map[string]any)["id"].(string)
	if st, _, _ := e.compat(e.owner, "DELETE", "/api/v1/snapshots/"+one, nil, nil); st != 409 {
		t.Errorf("delete one without confirmation: %d", st)
	}
	st, d1, _ := e.compat(e.owner, "DELETE", "/api/v1/snapshots/"+one, nil, map[string]string{"X-Ascii-Confirm-Delete": one})
	if st != 202 || d1["operation"].(map[string]any)["targetId"] != one {
		t.Errorf("delete one: %d %v", st, d1)
	}
	if l := e.ok("GET", "/api/v1/snapshots", nil, 200); len(l["snapshots"].([]any)) != 0 {
		t.Errorf("left after deleting all: %v", l)
	}
}

// /secrets is the default-injected team secrets: names only, admin only.
func TestCompatSecretsAndAIProxy(t *testing.T) {
	e := setup(t)
	s := e.ok("GET", "/api/v1/secrets", nil, 200)
	if s["envContents"] != "" || s["environmentId"] == nil {
		t.Errorf("empty secrets: %v", s)
	}
	up := e.ok("POST", "/api/v1/secrets", map[string]any{"envContents": "# team\nOPENAI_API_KEY=sk-1\nexport GH_TOKEN=\"ghp 2\"\n"}, 200)
	if up["envContents"] != "GH_TOKEN=\nOPENAI_API_KEY=\n" || strings.Contains(jsonNum(up), "sk-1") {
		t.Errorf("update: %v", up)
	}
	var tid string
	for k := range e.srv.store.Snapshot().Teams {
		tid = k
	}
	if sec := e.srv.store.Snapshot().Secrets[secretKey(tid, "OPENAI_API_KEY")]; sec == nil || !sec.Inject {
		t.Errorf("not default-injected: %v", sec)
	}
	// Left out: unmarked, kept. NAME= keeps a value.
	up = e.ok("POST", "/api/v1/secrets", map[string]any{"envContents": "GH_TOKEN=\n"}, 200)
	if sec := e.srv.store.Snapshot().Secrets[secretKey(tid, "OPENAI_API_KEY")]; up["envContents"] != "GH_TOKEN=\n" || sec == nil || sec.Inject {
		t.Errorf("unmark: %v %v", up, sec)
	}
	for _, bad := range []map[string]any{{"envContents": "lower=x"}, {"envContents": "NEW_ONE="}, {"secretFiles": []any{map[string]any{"path": "a", "contents": "b"}}}} {
		if st, out, _ := e.compat(e.owner, "POST", "/api/v1/secrets", bad, nil); st != 400 {
			t.Errorf("%v: %d %v", bad, st, out)
		}
	}
	reader := e.newKey("vms:read")
	if st, _, _ := e.compat(reader, "GET", "/api/v1/secrets", nil, nil); st != 403 {
		t.Errorf("non-admin reads /secrets: %d", st)
	}

	a := e.ok("GET", "/api/v1/ai-proxy", nil, 200)
	if a["enabled"] != true || a["type"] != "ai_proxy.info" {
		t.Errorf("ai-proxy: %v", a)
	}
	if a := e.ok("PUT", "/api/v1/ai-proxy", map[string]any{"enabled": false}, 200); a["enabled"] != false {
		t.Errorf("ai-proxy off: %v", a)
	}
	if !e.srv.store.Snapshot().AIProxyOff[tid] {
		t.Error("switch not stored")
	}
}

// find and download-url: the URL works without a key, only for its path, and
// not after it expires.
func TestCompatFindAndDownloadURL(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	e.ok("PUT", base+"/files", map[string]any{"path": "dl/report.txt", "content": "report body"}, 200)
	f := e.ok("GET", base+"/files/find?pattern=report&path=dl", nil, 200)
	if f["type"] != "file.find" || f["matches"] == nil {
		t.Errorf("find: %v", f)
	}
	if st, _, _ := e.compat(e.owner, "GET", base+"/files/find", nil, nil); st != 400 {
		t.Errorf("find without pattern: %d", st)
	}

	d := e.ok("POST", base+"/files/download-url", map[string]any{"path": "dl/report.txt", "ttlSeconds": 60}, 200)
	u := d["url"].(string)
	if d["method"] != "GET" || !strings.HasPrefix(u, e.api.URL+fileDownloadPath) || d["path"] != "/home/dev/dl/report.txt" {
		t.Fatalf("download-url: %v", d)
	}
	get := func(u string) (int, string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, body := get(u); st != 200 || body != "report body" {
		t.Errorf("download: %d %q", st, body)
	}
	tok := strings.TrimPrefix(u, e.api.URL+fileDownloadPath)
	payload, sig, _ := strings.Cut(tok, ".")
	if st, _ := get(e.api.URL + fileDownloadPath + payload + "." + strings.Repeat("0", len(sig))); st != 404 {
		t.Errorf("forged signature: %d", st)
	}
	g, _ := e.srv.openFileGrant(tok, time.Now())
	g.Path = "/etc/shadow"
	if st, _ := get(e.api.URL + fileDownloadPath + payload + "." + strings.Split(e.srv.signFileGrant(g), ".")[1]); st != 404 {
		t.Errorf("another path with the old signature: %d", st)
	}
	g.Path, g.Exp = "/home/dev/dl/report.txt", time.Now().Add(-time.Second).Unix()
	if st, _ := get(e.api.URL + fileDownloadPath + e.srv.signFileGrant(g)); st != 404 {
		t.Errorf("expired URL: %d", st)
	}
	if strings.Contains(e.audit.String(), payload) {
		t.Error("the audit log holds the download token")
	}
	for _, bad := range []map[string]any{{"path": ""}, {"path": "dl/report.txt", "ttlSeconds": 3601}} {
		if st, _, _ := e.compat(e.owner, "POST", base+"/files/download-url", bad, nil); st != 400 {
			t.Errorf("%v: %d", bad, st)
		}
	}
	// A missing file is 404 (files/info); the fake exec answers every stat, so the real-VM e2e checks it.
}
