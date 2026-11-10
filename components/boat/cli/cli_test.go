package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var vm1 = obj{"id": "vm1", "name": "dev", "size": "default", "state": "running", "image": "ubuntu-24.04", "team": "team_1"}

// fakeAPI serves deterministic responses and logs each request.
func fakeAPI(t *testing.T, log *bytes.Buffer) *httptest.Server {
	mux := http.NewServeMux()
	j := func(code int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
	}
	mux.HandleFunc("GET /v1/me", j(200, obj{"user": obj{"id": "usr_1", "name": "alice"}, "team": obj{"id": "team_1", "name": "acme"}, "role": "owner", "scopes": []string{"vms:read", "vms:write", "exec", "admin"}, "key_id": "k1"}))
	mux.HandleFunc("GET /v1/vms", j(200, obj{"vms": []obj{vm1, {"id": "vm2", "name": "ci", "size": "large", "state": "stopped", "image": "ubuntu-24.04"}}}))
	mux.HandleFunc("GET /v1/vms/vm1", j(200, vm1))
	mux.HandleFunc("GET /v1/vms/{id}", j(404, obj{"error": obj{"code": "not_found", "message": "vm not found", "request_id": "req_x"}}))
	mux.HandleFunc("POST /v1/vms", j(201, obj{"id": "vm3", "name": "new", "size": "small", "state": "creating", "image": "ubuntu-24.04"}))
	mux.HandleFunc("POST /v1/vms/vm1/stop", j(200, obj{"id": "vm1", "name": "dev", "size": "default", "state": "stopped", "image": "ubuntu-24.04"}))
	mux.HandleFunc("POST /v1/vms/vm1/fork", j(201, obj{"id": "vm4", "name": "dev-2", "size": "default", "state": "forking", "image": "ubuntu-24.04"}))
	mux.HandleFunc("DELETE /v1/vms/vm1", j(200, obj{"ok": true}))
	stream := func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("hi\n"))})
		enc.Encode(obj{"stream": "stderr", "data_b64": base64.StdEncoding.EncodeToString([]byte("warn\n"))})
		enc.Encode(obj{"exit": 3})
	}
	mux.HandleFunc("POST /v1/vms/vm1/exec", stream)
	mux.HandleFunc("POST /v1/vms/vm1/prompt", stream)
	mux.HandleFunc("POST /v1/vms/vm1/ssh", j(200, obj{"host": "10.0.0.5", "port": 22, "user": "ubuntu", "jump": "boat@gw.example:2222"}))
	mux.HandleFunc("GET /v1/vms/vm1/desktop", j(200, obj{"url": "https://boat.example/desktop/vm1?exp=1&sig=ab", "expires_at": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("GET /v1/vms/vm1/ports", j(200, obj{"ports": []obj{{"guest_port": 3000, "host_port": 20001, "hostname": "3000-vm1", "auth": "public"}}}))
	mux.HandleFunc("POST /v1/vms/vm1/ports", j(201, obj{"guest_port": 8080, "hostname": "8080-vm1", "auth": "private"}))
	mux.HandleFunc("GET /v1/vms/vm1/ports/8080/url", j(200, obj{"url": "https://8080-vm1.vm.example/?boat_exp=1&boat_sig=ab", "token": "1.ab", "auth": "private", "guest_port": 8080, "expires_at": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("POST /v1/teams", j(201, obj{"id": "team_2", "name": "tenant", "role": "", "owner": obj{"user": obj{"id": "usr_2", "name": "bob"}, "key": obj{"id": "k9", "token": "boat_owner"}}}))
	mux.HandleFunc("GET /v1/vms/vm1/files", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "remote-data") })
	mux.HandleFunc("PUT /v1/vms/vm1/files", j(200, obj{"ok": true, "path": "/tmp/x", "bytes": 5}))
	mux.HandleFunc("GET /v1/secrets", j(200, obj{"secrets": []obj{{"name": "ANTHROPIC_API_KEY", "updated": "2026-01-01T00:00:00Z"}}}))
	mux.HandleFunc("PUT /v1/secrets/OPENAI_API_KEY", j(200, obj{"name": "OPENAI_API_KEY", "updated": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("PUT /v1/secrets/ANTHROPIC_API_KEY", j(200, obj{"name": "ANTHROPIC_API_KEY", "updated": "2026-01-01T00:00:00Z", "rotated": true, "note": "running VMs keep the old value"}))
	mux.HandleFunc("POST /v1/keys", j(201, obj{"id": "k2", "name": "ci", "scopes": []string{"vms:read"}, "token": "boat_x_y"}))
	mux.HandleFunc("GET /v1/teams", j(200, obj{"teams": []obj{{"id": "team_1", "name": "acme", "role": "owner"}}}))
	mux.HandleFunc("GET /v1/webhooks", j(200, obj{"webhooks": []obj{{"id": "wh_1", "url": "https://h.example", "events": []string{"vm.*"}}}}))
	cmdv := obj{"id": "c_1", "cmd": []string{"sleep", "9"}, "state": "running", "exit": nil, "started_at": "2026-01-01T00:00:00Z"}
	run := obj{"id": "prm_1", "vm_id": "vm1", "conversation_id": "conv_1", "agent": "claude", "cmd_id": "c_2", "state": "running", "prompt": "hi"}
	envv := obj{"id": "env_1", "name": "base", "image": "ubuntu-24.04", "size": "large", "version": 1, "is_default": false}
	share := obj{"id": "shr1", "snapshot_id": "snap1", "uses": 0, "created": "2026-01-01T00:00:00Z", "code": "bsh_x_y"}
	key := obj{"id": "k2", "name": "ci", "scopes": []string{"vms:read"}, "token": "boat_k2_new", "prev_until": "2026-01-02T00:00:00Z"}
	hook := obj{"id": "wh_1", "url": "https://h.example", "events": []string{"vm.*"}, "secret": "whsec_new", "prev_until": "2026-01-02T00:00:00Z"}
	logs := base64.StdEncoding.EncodeToString([]byte("out\n"))
	follow := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") != "1" {
			j(200, obj{"data_b64": logs, "next_offset": 4, "running": false, "exit": 0})(w, r)
			return
		}
		enc := json.NewEncoder(w)
		enc.Encode(obj{"stream": "stdout", "data_b64": logs, "next_offset": 4})
		enc.Encode(obj{"exit": 5})
	}
	mux.HandleFunc("POST /v1/vms/vm1/commands", j(201, obj{"id": "c_1", "cmd": []string{"sleep", "9"}, "state": "running"}))
	mux.HandleFunc("GET /v1/vms/vm1/commands", j(200, obj{"commands": []obj{cmdv}}))
	mux.HandleFunc("GET /v1/vms/vm1/commands/c_1", j(200, cmdv))
	mux.HandleFunc("GET /v1/vms/vm1/commands/c_1/logs", follow)
	mux.HandleFunc("POST /v1/vms/vm1/commands/c_1/signal", j(200, obj{"ok": true, "id": "c_1", "signal": "INT"}))
	mux.HandleFunc("DELETE /v1/vms/vm1/commands/c_1", j(200, obj{"ok": true, "id": "c_1", "signal": "TERM"}))
	mux.HandleFunc("GET /v1/vms/vm1/command-history", j(200, obj{"commands": []obj{{"id": "c_1", "kind": "command", "state": "exited", "exit": 0, "cmd": []string{"ls"}}}}))
	mux.HandleFunc("GET /v1/vms/vm1/processes", j(200, obj{"processes": []obj{{"pid": 1, "user": "root", "elapsed_s": 9, "command": "init"}}}))
	mux.HandleFunc("GET /v1/vms/vm1/files/list", j(200, obj{"path": "/home", "entries": []obj{{"name": "a", "type": "file", "size": 1, "mode": "644"}}, "truncated": false}))
	mux.HandleFunc("DELETE /v1/vms/vm1/files", j(200, obj{"ok": true, "path": "/x"}))
	mux.HandleFunc("POST /v1/vms/vm1/files/move", j(200, obj{"ok": true, "from": "/a", "to": "/b"}))
	mux.HandleFunc("GET /v1/vms/vm1/files/search", j(200, obj{"matches": []obj{{"path": "/a", "line": 1, "text": "hit"}}, "truncated": false}))
	mux.HandleFunc("GET /v1/vms/vm1/files/info", j(200, obj{"path": "/a", "type": "file", "size": 1, "mode": "644", "mtime": 1}))
	mux.HandleFunc("POST /v1/vms/vm1/files/folder", j(200, obj{"ok": true, "path": "/d"}))
	mux.HandleFunc("POST /v1/vms/vm1/files/permissions", j(200, obj{"ok": true, "path": "/a", "mode": "600"}))
	mux.HandleFunc("POST /v1/vms/vm1/prompts", j(202, run))
	mux.HandleFunc("GET /v1/vms/vm1/prompts", j(200, obj{"prompts": []obj{run}}))
	mux.HandleFunc("GET /v1/vms/vm1/prompts/prm_1", j(200, run))
	mux.HandleFunc("GET /v1/vms/vm1/prompts/prm_1/logs", follow)
	mux.HandleFunc("POST /v1/vms/vm1/prompts/prm_1/steer", j(202, run))
	mux.HandleFunc("POST /v1/vms/vm1/prompts/prm_1/interrupt", j(200, run))
	mux.HandleFunc("GET /v1/vms/vm1/conversations", j(200, obj{"conversations": []obj{{"id": "conv_1", "agent": "claude", "prompt_count": 2, "running": false, "last_prompt": "hi"}}}))
	mux.HandleFunc("GET /v1/environments", j(200, obj{"environments": []obj{envv}}))
	mux.HandleFunc("POST /v1/environments", j(201, envv))
	mux.HandleFunc("GET /v1/environments/base", j(200, envv))
	mux.HandleFunc("PUT /v1/environments/base", j(200, envv))
	mux.HandleFunc("DELETE /v1/environments/base", j(200, obj{"ok": true}))
	mux.HandleFunc("POST /v1/environments/base/build", j(202, envv))
	mux.HandleFunc("POST /v1/vms/vm1/resize", j(200, vm1))
	mux.HandleFunc("POST /v1/vms/vm1/start", j(200, vm1))
	mux.HandleFunc("GET /v1/vms/vm1/usage", j(200, obj{"id": "vm1", "vm_seconds": 42}))
	mux.HandleFunc("POST /v1/keys/k2/rotate", j(200, key))
	mux.HandleFunc("GET /v1/keys/k2/usage", j(200, obj{"id": "k2", "requests": 3, "last_used": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("POST /v1/webhooks/wh_1/rotate", j(200, hook))
	mux.HandleFunc("GET /v1/webhooks/wh_1", j(200, obj{"id": "wh_1", "url": "https://h.example", "events": []string{"vm.*"}}))
	mux.HandleFunc("PATCH /v1/webhooks/wh_1", j(200, obj{"id": "wh_1", "url": "https://n.example", "events": []string{"vm.*"}}))
	mux.HandleFunc("GET /v1/snapshots", j(200, obj{"snapshots": []obj{{"id": "snap1", "name": "s", "vm_id": "vm1"}}}))
	mux.HandleFunc("DELETE /v1/snapshots/snap1", j(200, obj{"ok": true}))
	mux.HandleFunc("GET /v1/shared-snapshots", j(200, obj{"shares": []obj{share}}))
	mux.HandleFunc("POST /v1/snapshots/import", j(201, obj{"id": "snap2", "name": "imp", "team": "team_1"}))
	mux.HandleFunc("POST /v1/snapshots/import-share", j(201, obj{"id": "snap3", "name": "", "team": "team_1"}))
	mux.HandleFunc("GET /v1/vms/vm1/snapshots/snap1/download", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ARCHIVE") })
	mux.HandleFunc("POST /v1/vms/vm1/snapshots/snap1/shares", j(201, share))
	mux.HandleFunc("GET /v1/vms/vm1/snapshots/snap1/shares", j(200, obj{"shares": []obj{{"id": "shr1", "uses": 1}}}))
	mux.HandleFunc("DELETE /v1/vms/vm1/snapshots/snap1/shares/shr1", j(200, obj{"ok": true}))
	mux.HandleFunc("GET /v1/ai/credentials", j(200, obj{"credentials": []obj{{"provider": "anthropic", "last4": "3456", "updated": "2026-01-01T00:00:00Z"}}}))
	mux.HandleFunc("PUT /v1/ai/credentials/anthropic", j(200, obj{"provider": "anthropic", "last4": "3456", "updated": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("DELETE /v1/ai/credentials/anthropic", j(200, obj{"ok": true}))
	mux.HandleFunc("GET /v1/ai/proxy", j(200, obj{"enabled": true, "available": true}))
	mux.HandleFunc("PUT /v1/ai/proxy", j(200, obj{"enabled": false, "available": true}))
	mux.HandleFunc("GET /v1/ai/usage", j(200, obj{"usage": []obj{{"vm_id": "vm1", "provider": "anthropic", "model": "m", "requests": 1, "input_tokens": 2, "output_tokens": 3}}}))
	mux.HandleFunc("GET /v1/vms/vm1/ai-usage", j(200, obj{"usage": []obj{}}))
	mux.HandleFunc("PUT /v1/vms/vm1/ai-access", j(200, obj{"id": "vm1", "enabled": false}))
	mux.HandleFunc("GET /v1/telemetry", j(200, obj{"configured": false}))
	mux.HandleFunc("PUT /v1/telemetry", j(200, obj{"configured": true, "endpoint": "https://o.example", "header_names": []string{"X"}}))
	mux.HandleFunc("DELETE /v1/telemetry", j(200, obj{"ok": true}))
	mux.HandleFunc("GET /v1/usage", j(200, obj{"team": "team_1", "vm_seconds": obj{"default": 42}, "starts": 2}))
	mux.HandleFunc("PATCH /v1/vms/vm1", j(200, obj{"id": "vm1", "name": "dev", "ttl_s": 7200, "expires_at": "2099-01-01T00:00:00Z"}))
	mux.HandleFunc("GET /api/v1/limits", j(200, obj{"ok": true, "type": "limits.info", "activeSandboxes": 2, "maxActiveSandboxes": -1, "canStart": true}))
	mux.HandleFunc("POST /api/v1/saved-snapshots", j(202, obj{"ok": true, "type": "snapshot.saved.saving", "status": "saving",
		"snapshot": obj{"name": "web-stack", "snapshotId": "snap9", "status": "ready", "sourceSandboxId": "vm1", "createdAt": "2026-01-01T00:00:00Z"}}))
	mux.HandleFunc("DELETE /api/v1/saved-snapshots/web-stack", j(200, obj{"ok": true, "type": "snapshot.saved.deleted", "name": "web-stack", "status": "deleted"}))
	bdHook := obj{"id": "wh_0123456789abcdef01234567", "name": "prod", "url": "https://h.example", "events": []string{"sandbox.ready", "sandbox.error"}}
	mux.HandleFunc("POST /api/v1/webhooks", j(201, obj{"ok": true, "type": "webhook.created", "webhook": bdHook, "secret": "whsec_s"}))
	mux.HandleFunc("GET /api/v1/webhooks", j(200, obj{"ok": true, "type": "webhook.list", "webhooks": []obj{bdHook}}))
	mux.HandleFunc("POST /api/v1/webhooks/wh_0123456789abcdef01234567/rotate", j(200, obj{"ok": true, "webhook": bdHook, "secret": "whsec_t"}))
	mux.HandleFunc("DELETE /api/v1/webhooks/wh_0123456789abcdef01234567", j(200, obj{"ok": true, "id": "wh_0123456789abcdef01234567"}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		fmt.Fprintf(log, "%s %s %s\n", r.Method, r.URL.RequestURI(), bytes.TrimSpace(b))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGolden(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAAC3Nza me@laptop\n"), 0o600)
	local := filepath.Join(home, "up.txt")
	os.WriteFile(local, []byte("hello"), 0o600)
	cases := []struct {
		name string
		args []string
	}{
		{"list_json", []string{"list", "--json"}},
		{"list_text", []string{"list"}},
		{"new_json", []string{"new", "--name", "new", "--size", "small", "--ttl", "2h", "--json"}},
		{"new_snapshot_json", []string{"new", "--snapshot", "snap-1", "--idle-stop", "never", "--json"}},
		{"stop_by_name_json", []string{"stop", "dev", "--json"}},
		{"fork_json", []string{"fork", "vm1", "--name", "dev-2", "--json"}},
		{"rm_json", []string{"--json", "rm", "vm1"}},
		{"exec_json", []string{"exec", "--json", "vm1", "--", "ls", "--json"}},
		{"exec_text", []string{"exec", "vm1", "--", "ls"}},
		{"prompt_json", []string{"prompt", "vm1", "fix the tests", "--agent", "codex", "--json"}},
		{"ssh_json", []string{"ssh", "vm1", "--json"}},
		{"desktop_json", []string{"desktop", "vm1", "--json"}},
		{"ports_ls_json", []string{"ports", "ls", "vm1", "--json"}},
		{"ports_add_json", []string{"ports", "add", "vm1", "8080", "--json"}},
		{"ports_add_public_json", []string{"ports", "add", "vm1", "8080", "--public", "--json"}},
		{"ports_add_conflict", []string{"ports", "add", "vm1", "8080", "--public", "--auth", "private"}},
		{"ports_url_json", []string{"ports", "url", "vm1", "8080", "--ttl", "2h", "--json"}},
		{"ports_url_text", []string{"ports", "url", "vm1", "8080"}},
		{"ports_ls_text", []string{"ports", "ls", "vm1"}},
		{"new_secrets_json", []string{"new", "--name", "new", "--secrets", "A,B", "--json"}},
		{"new_no_secrets_json", []string{"new", "--name", "new", "--no-secrets", "--json"}},
		{"secrets_rotate_text", []string{"secrets", "rotate", "ANTHROPIC_API_KEY", "sk-2"}},
		{"secrets_rotate_missing", []string{"secrets", "rotate", "NOPE", "x"}},
		{"teams_create_owner_text", []string{"teams", "create", "tenant", "--owner", "bob"}},
		{"scp_up_json", []string{"scp", "--json", local, "vm1:/tmp/x"}},
		{"secrets_ls_json", []string{"secrets", "ls", "--json"}},
		{"secrets_set_json", []string{"secrets", "set", "OPENAI_API_KEY", "sk-1", "--json"}},
		{"secrets_set_no_inject_json", []string{"secrets", "set", "OPENAI_API_KEY", "sk-1", "--no-inject", "--json"}},
		{"keys_create_json", []string{"keys", "create", "--name", "ci", "--scopes", "vms:read", "--json"}},
		{"teams_json", []string{"teams", "--json"}},
		{"webhooks_json", []string{"webhooks", "ls", "--json"}},
		{"usage_json", []string{"usage", "--json"}},
		{"whoami_json", []string{"whoami", "--json"}},
		{"error_json", []string{"stop", "nope", "--json"}},
		{"usage_error", []string{"bogus"}},
		{"cmd_start_json", []string{"cmd", "start", "vm1", "--cwd", "/tmp", "--json", "--", "sleep", "9"}},
		{"cmd_start_follow_text", []string{"cmd", "start", "vm1", "-f", "--", "sleep", "9"}},
		{"exec_detach_json", []string{"exec", "vm1", "--detach", "--json", "--", "sleep", "9"}},
		{"cmd_ls_json", []string{"cmd", "ls", "vm1", "--json"}},
		{"cmd_status_json", []string{"cmd", "status", "vm1", "c_1", "--json"}},
		{"cmd_logs_json", []string{"cmd", "logs", "vm1", "c_1", "--offset", "2", "--json"}},
		{"cmd_logs_follow_json", []string{"cmd", "logs", "vm1", "c_1", "-f", "--json"}},
		{"cmd_signal_json", []string{"cmd", "signal", "vm1", "c_1", "--signal", "INT", "--json"}},
		{"cmd_stop_json", []string{"cmd", "stop", "vm1", "c_1", "--json"}},
		{"cmd_history_json", []string{"cmd", "history", "vm1", "--json"}},
		{"ps_json", []string{"ps", "vm1", "--json"}},
		{"files_ls_json", []string{"files", "ls", "vm1", "/home", "--json"}},
		{"files_rm_json", []string{"files", "rm", "vm1", "/x", "-r", "--json"}},
		{"files_mv_json", []string{"files", "mv", "vm1", "/a", "/b", "--overwrite", "--json"}},
		{"files_search_json", []string{"files", "search", "vm1", "/", "hit", "--content", "--regex", "--limit", "5", "--json"}},
		{"files_info_json", []string{"files", "info", "vm1", "/a", "--json"}},
		{"files_mkdir_json", []string{"files", "mkdir", "vm1", "/d", "--json"}},
		{"files_chmod_json", []string{"files", "chmod", "vm1", "600", "/a", "--json"}},
		{"prompt_async_json", []string{"prompt", "vm1", "hi", "--async", "--json"}},
		{"prompt_new_follow_json", []string{"prompt", "vm1", "hi", "--new", "--json"}},
		{"prompt_resume_text", []string{"prompt", "vm1", "hi", "--resume", "conv_1"}},
		{"prompts_ls_json", []string{"prompts", "ls", "vm1", "--json"}},
		{"prompts_status_json", []string{"prompts", "status", "vm1", "prm_1", "--json"}},
		{"prompts_logs_json", []string{"prompts", "logs", "vm1", "prm_1", "--json"}},
		{"conversations_json", []string{"conversations", "vm1", "--json"}},
		{"steer_json", []string{"steer", "vm1", "skip tests", "--json"}},
		{"interrupt_json", []string{"interrupt", "vm1", "--prompt-id", "prm_1", "--json"}},
		{"env_ls_json", []string{"env", "--json"}},
		{"env_new_json", []string{"env", "new", "base", "--size", "large", "--setup", "npm ci", "--var", "A=b", "--secrets", "S1,S2", "--json"}},
		{"env_show_json", []string{"env", "show", "base", "--json"}},
		{"env_set_json", []string{"env", "set", "base", "--setup", "make", "--json"}},
		{"env_default_json", []string{"env", "default", "base", "--json"}},
		{"env_build_json", []string{"env", "build", "base", "--json"}},
		{"env_rm_json", []string{"env", "rm", "base", "--json"}},
		{"new_env_json", []string{"new", "--env", "base", "--json"}},
		{"new_from_json", []string{"new", "--from", "bsh_x_y", "--no-env", "--json"}},
		{"resize_json", []string{"resize", "vm1", "large", "--discard-memory", "--json"}},
		{"resume_type_json", []string{"resume", "vm1", "--type", "large", "--json"}},
		{"info_json", []string{"info", "vm1", "--json"}},
		{"vm_usage_json", []string{"vm-usage", "vm1", "--json"}},
		{"keys_rotate_json", []string{"keys", "rotate", "k2", "--overlap", "1h", "--json"}},
		{"keys_rotate_text", []string{"keys", "rotate", "k2"}},
		{"keys_usage_json", []string{"keys", "usage", "k2", "--json"}},
		{"webhooks_rotate_json", []string{"webhooks", "rotate", "wh_1", "--json"}},
		{"webhooks_show_json", []string{"webhooks", "show", "wh_1", "--json"}},
		{"webhooks_update_json", []string{"webhooks", "update", "wh_1", "--url", "https://n.example", "--json"}},
		{"snapshots_team_json", []string{"snapshots", "--json"}},
		{"snapshots_shared_json", []string{"snapshots", "shared", "--json"}},
		{"snapshots_delete_json", []string{"snapshots", "delete", "snap1", "--json"}},
		{"snapshots_take_json", []string{"snapshots", "take", "--code", "bsh_x_y", "--json"}},
		{"snapshots_download_json", []string{"snapshots", "vm1", "download", "snap1", "-o", local + ".dl", "--json"}},
		{"snapshots_import_json", []string{"snapshots", "import", local, "--name", "imp", "--json"}},
		{"snapshots_share_json", []string{"snapshots", "vm1", "share", "snap1", "--ttl", "1h", "--json"}},
		{"snapshots_shares_json", []string{"snapshots", "vm1", "shares", "snap1", "--json"}},
		{"snapshots_unshare_json", []string{"snapshots", "vm1", "unshare", "snap1", "shr1", "--json"}},
		{"ai_credentials_ls_json", []string{"ai", "credentials", "--json"}},
		{"ai_credentials_set_json", []string{"ai", "credentials", "set", "anthropic", "sk-1234", "--json"}},
		{"ai_credentials_rm_json", []string{"ai", "credentials", "rm", "anthropic", "--json"}},
		{"ai_proxy_json", []string{"ai", "proxy", "--json"}},
		{"ai_proxy_off_json", []string{"ai", "proxy", "off", "--json"}},
		{"ai_usage_json", []string{"ai", "usage", "--json"}},
		{"ai_usage_vm_json", []string{"ai", "usage", "vm1", "--json"}},
		{"ai_block_json", []string{"ai", "block", "vm1", "--json"}},
		{"telemetry_json", []string{"telemetry", "--json"}},
		{"telemetry_set_json", []string{"telemetry", "set", "https://o.example", "-H", "X=y", "--json"}},
		{"telemetry_unset_json", []string{"telemetry", "unset", "--json"}},
		{"extend_hours_json", []string{"extend", "vm1", "--hours", "2", "--json"}},
		{"extend_by_json", []string{"extend", "vm1", "--by", "90m", "--json"}},
		{"extend_never_json", []string{"extend", "vm1", "--no-auto-stop", "--json"}},
		{"extend_none", []string{"extend", "vm1"}},
		{"limits_json", []string{"limits", "--json"}},
		{"limits_text", []string{"limits"}},
		{"snapshot_save_json", []string{"snapshot", "vm1", "web-stack", "--json"}},
		{"snapshot_rm_json", []string{"snapshot", "rm", "web-stack", "--json"}},
		{"snapshot_team_json", []string{"snapshot", "team", "--json"}},
		{"history_json", []string{"history", "vm1", "--json"}},
		{"history_logs_json", []string{"history", "vm1", "c_1", "--json"}},
		{"webhook_create_json", []string{"webhook", "create", "https://h.example", "--name", "prod", "--event", "ready", "--event", "sandbox.error", "--json"}},
		{"webhook_list_json", []string{"webhook", "list", "--json"}},
		{"webhook_rotate_json", []string{"webhook", "rotate", "wh_0123456789abcdef01234567", "--json"}},
		{"webhook_remove_json", []string{"webhook", "remove", "wh_0123456789abcdef01234567", "--json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reqs bytes.Buffer
			srv := fakeAPI(t, &reqs)
			var out, errw bytes.Buffer
			a := &app{out: &out, errw: &errw, in: strings.NewReader(""),
				getenv: func(k string) string {
					return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "cfg.json"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "boat_test"}[k]
				},
				execProg: func(argv []string) error { return fmt.Errorf("unexpected exec %v", argv) },
				open:     func(string) error { return nil }}
			code := a.run(c.args)
			got := fmt.Sprintf("$ boat %s\nexit %d\n--- stdout\n%s--- stderr\n%s--- requests\n%s",
				strings.Join(c.args, " "), code, out.String(), errw.String(), reqs.String())
			got = strings.ReplaceAll(got, home, "$HOME")
			got = regexp.MustCompile(`(?s)--- stderr\nboat - persistent.*?\n--- requests`).ReplaceAllString(got, "--- stderr\n<usage>\n--- requests")
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				os.WriteFile(path, []byte(got), 0o644)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if got != string(want) {
				t.Errorf("mismatch for %s\n--- got\n%s\n--- want\n%s", c.name, got, want)
			}
		})
	}
}

// Every --json stdout line must be a standalone JSON object (JSONL contract).
func TestJSONLContract(t *testing.T) {
	files, _ := filepath.Glob("testdata/*_json.golden")
	if len(files) == 0 {
		t.Fatal("no golden files")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		stdout := strings.SplitN(strings.SplitN(string(b), "--- stdout\n", 2)[1], "--- stderr\n", 2)[0]
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			if line == "" {
				continue
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Errorf("%s: not a JSON object line: %q", f, line)
			}
		}
	}
}

func TestLoginSavesConfig(t *testing.T) {
	var reqs bytes.Buffer
	srv := fakeAPI(t, &reqs)
	cfg := filepath.Join(t.TempDir(), "boat", "config.json")
	var out bytes.Buffer
	env := map[string]string{"BOAT_CONFIG": cfg}
	a := &app{out: &out, errw: io.Discard, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }}
	if code := a.run([]string{"login", "--url", srv.URL + "/", "--key", "boat_k", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	st, err := os.Stat(cfg)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config perms: %v %v", st, err)
	}
	c, _ := loadConfig(func(k string) string { return env[k] })
	if c.URL != srv.URL || c.Key != "boat_k" {
		t.Errorf("config %+v", c)
	}
}

func TestSSHExecArgv(t *testing.T) {
	var reqs bytes.Buffer
	srv := fakeAPI(t, &reqs)
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA k\n"), 0o600)
	var argv []string
	a := &app{out: io.Discard, errw: io.Discard, getenv: func(k string) string {
		return map[string]string{"HOME": home, "BOAT_CONFIG": "/nonexistent", "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
	}, execProg: func(v []string) error { argv = v; return nil }}
	if code := a.run([]string{"ssh", "dev", "--", "uptime"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "ssh -p 22 -o StrictHostKeyChecking=accept-new -o HostKeyAlias=boat-vm1 -o ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 boat@gw.example ubuntu@10.0.0.5 uptime"
	if strings.Join(argv, " ") != want {
		t.Errorf("argv %q", strings.Join(argv, " "))
	}
}

func TestUpgrade(t *testing.T) {
	bin := []byte("#!/bin/sh\necho new boat\n")
	sum := sha256.Sum256(bin)
	good := hex.EncodeToString(sum[:]) + "  " + assetName() + "\n"
	sums := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boat_SHA256SUMS":
			io.WriteString(w, sums)
		case "/" + assetName():
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	self := filepath.Join(dir, "boat")
	os.WriteFile(self, []byte("old"), 0o755)
	run := func() (int, string) {
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, getenv: func(k string) string {
			return map[string]string{"BOAT_RELEASE_BASE": srv.URL}[k]
		}, exe: func() (string, error) { return self, nil }}
		code := a.run([]string{"upgrade", "--json"})
		return code, out.String() + errw.String()
	}
	if code, out := run(); code != 0 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("upgrade: %d %s", code, out)
	}
	if b, _ := os.ReadFile(self); !bytes.Equal(b, bin) {
		t.Fatalf("binary not replaced: %q", b)
	}
	if st, _ := os.Stat(self); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
	os.WriteFile(self, []byte("old"), 0o755)
	sums = strings.Repeat("0", 64) + "  " + assetName() + "\n"
	if code, out := run(); code == 0 || !strings.Contains(out, "checksum mismatch") {
		t.Errorf("bad checksum accepted: %d %s", code, out)
	}
	if b, _ := os.ReadFile(self); string(b) != "old" {
		t.Error("binary replaced despite checksum mismatch")
	}
	sums = ""
	if code, _ := run(); code == 0 {
		t.Error("missing checksum accepted")
	}
}
