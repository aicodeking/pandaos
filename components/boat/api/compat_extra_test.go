package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Council round 3: these boat.dev operations answered 404 under /api/v1.
func TestCompatTelemetryAndAI(t *testing.T) {
	e := setup(t)
	if out := e.ok("GET", "/api/v1/telemetry", nil, 200); out["telemetry"] != nil {
		t.Errorf("unset telemetry: %v", out)
	}
	out := e.ok("PUT", "/api/v1/telemetry", map[string]any{"endpoint": "https://otel.example/v1/logs", "headers": map[string]string{"X-Key": "s"}}, 200)
	tel, _ := out["telemetry"].(map[string]any)
	if tel["endpoint"] != "https://otel.example" || len(tel["headers"].([]any)) != 1 || out["test"] == nil {
		t.Errorf("put telemetry: %v", out)
	}
	if e.srv.store.Snapshot().Telemetry[e.teamID()] == nil {
		t.Error("telemetry not stored natively")
	}
	if out := e.ok("DELETE", "/api/v1/telemetry", nil, 200); out["telemetry"] != nil || e.srv.store.Snapshot().Telemetry[e.teamID()] != nil {
		t.Errorf("delete telemetry: %v", out)
	}

	bx := e.newSandbox(nil)
	vm := e.srv.store.Snapshot().CompatIDs[bx].VM
	team := e.teamID()
	e.srv.store.Update(func(st *State) error {
		st.AIUsage[team+"/"+vm+"/anthropic/m"] = &AIUsageRow{TeamID: team, VMID: vm, Provider: "anthropic", Model: "m",
			Requests: 3, InputTokens: 10, OutputTokens: 5, Updated: time.Now().UTC()}
		return nil
	})
	u := e.ok("GET", "/api/v1/ai-usage?days=7", nil, 200)
	if tot := u["totals"].(map[string]any); tot["requests"] != float64(3) || len(u["sandboxes"].([]any)) != 1 {
		t.Errorf("ai-usage: %v", u)
	}
	if st, _, _ := e.compat(e.owner, "GET", "/api/v1/ai-usage?days=0", nil, nil); st != 400 {
		t.Errorf("days=0: %d", st)
	}
	su := e.ok("GET", "/api/v1/sandboxes/"+bx+"/ai-usage", nil, 200)
	if su["sandboxId"] != bx || su["totals"].(map[string]any)["outputTokens"] != float64(5) {
		t.Errorf("sandbox ai-usage: %v", su)
	}
	if a := e.ok("PUT", "/api/v1/sandboxes/"+bx+"/ai-access", map[string]any{"blocked": true}, 200); a["blocked"] != true || a["sandboxId"] != bx {
		t.Errorf("ai-access: %v", a)
	}
	if v := e.srv.store.Snapshot().AIVMs[vm]; v == nil || !v.Blocked {
		t.Error("ai-access not stored natively")
	}
}

func TestCompatEnvVars(t *testing.T) {
	e := setup(t)
	id := e.ok("POST", "/api/v1/environments", map[string]any{"name": "web"}, 200)["environments"].([]any)[0].(map[string]any)["id"].(string)
	st, out, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+id+"/vars/FOO", map[string]any{"value": "bar"}, nil)
	if st != 200 || out["success"] != true {
		t.Fatalf("set var: %d %v", st, out)
	}
	env := func() map[string]string {
		for _, x := range e.srv.store.Snapshot().Environments {
			return x.Env
		}
		return nil
	}
	if env()["FOO"] != "bar" {
		t.Errorf("var not stored: %v", env())
	}
	if st, _, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+id+"/vars/bad-name", map[string]any{"value": "x"}, nil); st != 400 {
		t.Errorf("bad var name: %d", st)
	}
	if st, _, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+id+"/vars/FOO", nil, nil); st != 200 || env()["FOO"] != "" {
		t.Errorf("delete var: %d %v", st, env())
	}
	if st, _, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+id+"/vars/FOO", nil, nil); st != 404 {
		t.Errorf("delete missing var: %d", st)
	}
}

func TestCompatFilesExtra(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	e.ok("PUT", "/api/v1/sandboxes/"+bx+"/files", map[string]any{"path": "a.txt", "content": "x-x-x"}, 200)
	rep := e.ok("POST", "/api/v1/sandboxes/"+bx+"/files/replace", map[string]any{"paths": []string{"a.txt", "missing"}, "pattern": "x", "newValue": "yy"}, 200)
	res := rep["results"].([]any)
	if rep["success"] != false || res[0].(map[string]any)["replacements"] != float64(3) || res[1].(map[string]any)["success"] != false {
		t.Errorf("replace: %v", rep)
	}
	if got := e.ok("GET", "/api/v1/sandboxes/"+bx+"/files?path=a.txt", nil, 200)["content"]; got != "yy-yy-yy" {
		t.Errorf("after replace: %v", got)
	}
	// artifacts: raw bytes
	if st, b := e.raw(e.owner, "GET", "/api/v1/sandboxes/"+bx+"/artifacts?path=a.txt", nil); st != 200 || string(b) != "yy-yy-yy" {
		t.Errorf("artifact: %d %q", st, b)
	}
	// upload-url: a PUT with no API key; a download URL does not upload.
	up := e.ok("POST", "/api/v1/sandboxes/"+bx+"/files/upload-url", map[string]any{"path": "up.bin"}, 200)
	u := up["url"].(string)
	if up["method"] != "PUT" || !strings.Contains(u, fileUploadPath) {
		t.Fatalf("upload-url: %v", up)
	}
	put := func(u string) int {
		req, _ := http.NewRequest("PUT", e.api.URL+u[strings.Index(u, "/api/v1/"):], strings.NewReader("hello"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := put(u); st != 200 {
		t.Fatalf("upload: %d", st)
	}
	if got := e.ok("GET", "/api/v1/sandboxes/"+bx+"/files?path=up.bin", nil, 200)["content"]; got != "hello" {
		t.Errorf("uploaded: %v", got)
	}
	dl := e.ok("POST", "/api/v1/sandboxes/"+bx+"/files/download-url", map[string]any{"path": "up.bin"}, 200)["url"].(string)
	tok := dl[strings.LastIndex(dl, "/")+1:]
	if st := put(fileUploadPath + tok); st != 404 {
		t.Errorf("download token uploaded: %d", st)
	}
	if st, _ := e.raw("", "GET", fileDownloadPath+u[strings.LastIndex(u, "/")+1:], nil); st != 404 {
		t.Errorf("upload token downloaded: %d", st)
	}
}

func TestCompatShareRecover(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/share", nil, nil)
	if st != 200 || out["sandbox"].(map[string]any)["id"] != bx {
		t.Errorf("share: %d %v", st, out)
	}
	n := len(e.boatd.stops)
	if r := e.ok("POST", "/api/v1/sandboxes/"+bx+"/recover", nil, 200); r["status"] != "recovering" {
		t.Errorf("recover: %v", r)
	}
	if len(e.boatd.stops) != n+1 || e.boatd.stops[n]["force"] != true {
		t.Errorf("recover did not force-stop: %v", e.boatd.stops)
	}
}
