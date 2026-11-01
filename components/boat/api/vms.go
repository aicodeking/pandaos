package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	reVMName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reID      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reLabel   = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,63}$`)
)

type VM = map[string]any

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// publicVM drops fields that may carry secrets (boatd echoes env back).
func publicVM(vm VM) VM {
	out := make(VM, len(vm))
	for k, v := range vm {
		if k != "env" {
			out[k] = v
		}
	}
	return out
}

func vmPath(id string, rest ...string) string {
	p := "/v1/vms/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

// ownVM fetches the VM and checks it belongs to the caller's team; foreign
// VMs are reported as 404 so ids cannot be probed across teams.
func (s *server) ownVM(w http.ResponseWriter, r *http.Request, p *principal) (VM, bool) {
	id := r.PathValue("id")
	if !reID.MatchString(id) {
		writeErr(w, r, 404, "not_found", "vm not found")
		return nil, false
	}
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm); err != nil {
		writeUpstream(w, r, err)
		return nil, false
	}
	if str(vm, "team") != p.Team.ID {
		writeErr(w, r, 404, "not_found", "vm not found")
		return nil, false
	}
	return vm, true
}

// teamEnv decrypts the team secrets injected as env vars: all of them, or
// only the names in only when it is non-nil (per-VM selection).
func (s *server) teamEnv(teamID string, only []string) (map[string]string, error) {
	env := map[string]string{}
	for _, sec := range s.store.Snapshot().Secrets {
		if sec.TeamID != teamID || (only != nil && !slices.Contains(only, sec.Name)) {
			continue
		}
		v, err := s.keys.Open(sec.Sealed, secretKey(sec.TeamID, sec.Name))
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", sec.Name, err)
		}
		env[sec.Name] = string(v)
	}
	return env, nil
}

// vmSecrets is the VM's secret selection (nil = all team secrets).
func (s *server) vmSecrets(vmID string) []string {
	sel, ok := s.store.Snapshot().VMSecrets[vmID]
	if !ok {
		return nil
	}
	if sel == nil {
		return []string{}
	}
	return sel
}

func (s *server) envOrFail(w http.ResponseWriter, r *http.Request, p *principal, only []string) (map[string]string, bool) {
	env, err := s.teamEnv(p.Team.ID, only)
	if err != nil {
		log.Printf("%s: %v", info(r).id, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot decrypt team secrets")
		return nil, false
	}
	return env, true
}

func (s *server) listVMs(w http.ResponseWriter, r *http.Request, p *principal) {
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/vms?team="+url.QueryEscape(p.Team.ID), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return
	}
	out := []VM{}
	for _, vm := range list.VMs {
		if str(vm, "team") == p.Team.ID { // defence in depth if boatd ignores ?team
			out = append(out, publicVM(vm))
		}
	}
	writeJSON(w, 200, map[string]any{"vms": out})
}

func (s *server) getVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if vm, ok := s.ownVM(w, r, p); ok {
		writeJSON(w, 200, publicVM(vm))
	}
}

type createReq struct {
	Name  string `json:"name"`
	Size  string `json:"size"`
	Image string `json:"image"`
	TTLS  int64  `json:"ttl_s"`
	// IdleStopS overrides boatd's idleStopS; -1 = never.
	IdleStopS int64 `json:"idle_stop_s"`
	// Snapshot creates the VM from a snapshot of the caller's team.
	Snapshot string            `json:"snapshot"`
	Labels   map[string]string `json:"labels"`
	Env      map[string]string `json:"env"`
	// Secrets selects which team secrets the VM gets (absent = all, [] = none).
	Secrets *[]string `json:"secrets"`
}

func (s *server) validateCreate(c *createReq) string {
	if c.Name != "" && !reVMName.MatchString(c.Name) {
		return "name must be lowercase letters, digits and dashes (max 63)"
	}
	// Sizes are boatd's config; boatd rejects unknown ones, "" = its defaultSize.
	if c.Size != "" && !reVMName.MatchString(c.Size) {
		return "invalid size"
	}
	if c.Image == "" && len(s.cfg.Images) > 0 {
		c.Image = s.cfg.Images[0]
	}
	if len(s.cfg.Images) > 0 && !slices.Contains(s.cfg.Images, c.Image) {
		return "unknown image " + c.Image
	}
	if c.TTLS < 0 {
		return "ttl_s must be >= 0"
	}
	if c.IdleStopS < -1 {
		return "idle_stop_s must be >= -1"
	}
	if c.Snapshot != "" && !reID.MatchString(c.Snapshot) {
		return "invalid snapshot id"
	}
	if len(c.Labels) > 32 {
		return "at most 32 labels"
	}
	for k, v := range c.Labels {
		if !reLabel.MatchString(k) || len(v) > 256 {
			return "invalid label " + k
		}
	}
	for k := range c.Env {
		if !reEnvName.MatchString(k) {
			return "invalid env name " + k
		}
	}
	return ""
}

// checkSecretNames reports the first selected name the team does not have.
func (s *server) checkSecretNames(team string, names []string) string {
	st := s.store.Snapshot()
	for _, n := range names {
		if st.Secrets[secretKey(team, n)] == nil {
			return "unknown secret " + n
		}
	}
	return ""
}

func (s *server) createVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var c createReq
	if !decode(w, r, &c) {
		return
	}
	if msg := s.validateCreate(&c); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	var only []string
	if c.Secrets != nil {
		only = append([]string{}, *c.Secrets...)
		if msg := s.checkSecretNames(p.Team.ID, only); msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return
		}
	}
	env, ok := s.envOrFail(w, r, p, only)
	if !ok {
		return
	}
	for k, v := range c.Env { // explicit env overrides team secrets
		env[k] = v
	}
	body := map[string]any{"name": c.Name, "size": c.Size, "image": c.Image, "ttl_s": c.TTLS,
		"idle_stop_s": c.IdleStopS, "snapshot": c.Snapshot, "team": p.Team.ID, "labels": c.Labels, "env": env}
	var vm VM
	if err := s.boatd.call(r.Context(), "POST", "/v1/vms", body, &vm); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if only != nil {
		if err := s.setVMSecrets(str(vm, "id"), only); err != nil {
			// Fail loudly: without the record a later start would inject all secrets.
			log.Printf("%s: record secret selection for %s: %v; deleting the VM", info(r).id, str(vm, "id"), err)
			s.boatd.call(r.Context(), "POST", vmPath(str(vm, "id"), "delete"), nil, nil)
			writeErr(w, r, 500, "store_error", "could not record the secret selection")
			return
		}
	}
	writeJSON(w, 201, publicVM(vm))
}

func (s *server) setVMSecrets(vmID string, names []string) error {
	return s.store.Update(func(st *State) error {
		if names == nil {
			delete(st.VMSecrets, vmID)
		} else {
			st.VMSecrets[vmID] = append([]string{}, names...)
		}
		return nil
	})
}

func (s *server) startVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	env, ok := s.envOrFail(w, r, p, s.vmSecrets(r.PathValue("id")))
	if !ok {
		return
	}
	s.vmAction(w, r, "start", map[string]any{"env": env})
}

func (s *server) stopVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); ok {
		s.vmAction(w, r, "stop", nil)
	}
}

func (s *server) deleteVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); ok {
		if s.vmAction(w, r, "delete", nil) {
			s.forgetVM(r.PathValue("id"))
			s.syncJumpKeys(r.Context())
		}
	}
}

func (s *server) forkVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name != "" && !reVMName.MatchString(body.Name) {
		writeErr(w, r, 400, "invalid_request", "invalid name")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	var child VM
	parent := r.PathValue("id")
	if err := s.boatd.call(r.Context(), "POST", vmPath(parent, "fork"), map[string]any{"name": body.Name, "team": p.Team.ID}, &child); err != nil {
		writeUpstream(w, r, err)
		return
	}
	// The child inherits the parent's disk, hence its secrets, so it keeps
	// the parent's selection for later starts too.
	if sel, ok := s.store.Snapshot().VMSecrets[parent]; ok {
		if err := s.setVMSecrets(str(child, "id"), append([]string{}, sel...)); err != nil {
			log.Printf("%s: copy secret selection to fork %s: %v", info(r).id, str(child, "id"), err)
		}
	}
	writeJSON(w, 201, publicVM(child))
}

// forgetVM drops boat-api state tied to a deleted VM.
func (s *server) forgetVM(id string) {
	err := s.store.Update(func(st *State) error {
		delete(st.VMSecrets, id)
		return nil
	})
	if err != nil {
		log.Printf("forget vm %s: %v", id, err)
	}
}

// vmAction proxies a lifecycle POST and reports whether it succeeded.
func (s *server) vmAction(w http.ResponseWriter, r *http.Request, action string, body any) bool {
	var vm VM
	if err := s.boatd.call(r.Context(), "POST", vmPath(r.PathValue("id"), action), body, &vm); err != nil {
		writeUpstream(w, r, err)
		return false
	}
	if vm == nil {
		writeJSON(w, 200, map[string]any{"ok": true})
		return true
	}
	writeJSON(w, 200, publicVM(vm))
	return true
}

// ---- exec & prompt ----

type execReq struct {
	Cmd      []string          `json:"cmd"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
}

func (s *server) execVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var e execReq
	if !decode(w, r, &e) {
		return
	}
	if len(e.Cmd) == 0 || e.Cmd[0] == "" {
		writeErr(w, r, 400, "invalid_request", "cmd is required")
		return
	}
	for k := range e.Env {
		if !reEnvName.MatchString(k) {
			writeErr(w, r, 400, "invalid_request", "invalid env name "+k)
			return
		}
	}
	if _, ok := s.ownVM(w, r, p); ok {
		s.streamExec(w, r, e)
	}
}

var agents = map[string]func(prompt, model string) []string{
	"claude": func(prompt, model string) []string {
		c := []string{"claude", "-p", "--dangerously-skip-permissions"}
		if model != "" {
			c = append(c, "--model", model)
		}
		return append(c, "--", prompt)
	},
	"codex": func(prompt, model string) []string {
		c := []string{"codex", "exec", "--full-auto", "--skip-git-repo-check"}
		if model != "" {
			c = append(c, "-m", model)
		}
		return append(c, "--", prompt)
	},
	"opencode": func(prompt, model string) []string {
		c := []string{"opencode", "run"}
		if model != "" {
			c = append(c, "-m", model)
		}
		return append(c, "--", prompt)
	},
}

var reModel = regexp.MustCompile(`^[A-Za-z0-9_./:@-]{1,128}$`)

func (s *server) promptVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Prompt   string `json:"prompt"`
		Agent    string `json:"agent"`
		Model    string `json:"model"`
		Cwd      string `json:"cwd"`
		TimeoutS int    `json:"timeout_s"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Agent == "" {
		body.Agent = "claude"
	}
	build, ok := agents[body.Agent]
	switch {
	case !ok:
		writeErr(w, r, 400, "invalid_request", "agent must be claude, codex or opencode")
		return
	case strings.TrimSpace(body.Prompt) == "":
		writeErr(w, r, 400, "invalid_request", "prompt is required")
		return
	case body.Model != "" && !reModel.MatchString(body.Model):
		writeErr(w, r, 400, "invalid_request", "invalid model")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	env, ok := s.envOrFail(w, r, p, s.vmSecrets(r.PathValue("id")))
	if !ok {
		return
	}
	s.streamExec(w, r, execReq{Cmd: build(body.Prompt, body.Model), Env: env, Cwd: body.Cwd, TimeoutS: body.TimeoutS})
}

// streamExec relays boatd's JSON-lines exec stream, flushing per chunk.
func (s *server) streamExec(w http.ResponseWriter, r *http.Request, e execReq) {
	b, _ := json.Marshal(e)
	resp, err := s.boatd.do(r.Context(), "POST", vmPath(r.PathValue("id"), "exec"), bytes.NewReader(b), "application/json")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ---- files ----

func validFilePath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) < 4096 && !strings.ContainsRune(p, 0)
}

func (s *server) getFile(w http.ResponseWriter, r *http.Request, p *principal) {
	path := r.URL.Query().Get("path")
	if !validFilePath(path) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(r.PathValue("id"), "files")+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == 404 {
			writeErr(w, r, 404, "not_found", "file not found")
			return
		}
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	io.Copy(w, resp.Body)
}

func (s *server) putFile(w http.ResponseWriter, r *http.Request, p *principal) {
	path := r.URL.Query().Get("path")
	if !validFilePath(path) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxFileBytes))
	if err != nil {
		writeErr(w, r, http.StatusRequestEntityTooLarge, "too_large", "file exceeds limit")
		return
	}
	if err := s.putVMFile(r, r.PathValue("id"), path, data); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": path, "bytes": len(data)})
}

func (s *server) putVMFile(r *http.Request, id, path string, data []byte) error {
	resp, err := s.boatd.do(r.Context(), "PUT", vmPath(id, "files")+"?path="+url.QueryEscape(path), bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return &boatdError{Status: resp.StatusCode, Body: body}
	}
	return nil
}

// ---- snapshots ----

// proxySnapshots forwards GET/POST /snapshots and DELETE /snapshots/{sid}.
func (s *server) proxySnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	path := vmPath(r.PathValue("id"), "snapshots")
	if sid := r.PathValue("sid"); sid != "" {
		if !reID.MatchString(sid) {
			writeErr(w, r, 404, "not_found", "snapshot not found")
			return
		}
		path += "/" + url.PathEscape(sid)
	}
	var in any
	if r.Method == "POST" {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		in = body
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	var out any
	if err := s.boatd.call(r.Context(), r.Method, path, in, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	status := 200
	if r.Method == "POST" {
		status = 201
	}
	if out == nil {
		out = map[string]any{"ok": true}
	}
	writeJSON(w, status, out)
}

// ---- ssh & desktop ----

var reSSHKey = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]{16,}( [^\r\n]{0,256})?$`)

func (s *server) sshVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		PublicKey string `json:"public_key"`
	}
	if !decode(w, r, &body) {
		return
	}
	pub := strings.TrimSpace(body.PublicKey)
	if len(pub) > 16<<10 {
		pub = "x" // fails the regexp below
	}
	if pub != "" && !reSSHKey.MatchString(pub) {
		writeErr(w, r, 400, "invalid_request", "public_key must be a single OpenSSH public key line")
		return
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	if pub != "" {
		if err := s.authorizeKey(r, r.PathValue("id"), pub); err != nil {
			writeUpstream(w, r, err)
			return
		}
		if err := s.grantJump(r, p.Team.ID, r.PathValue("id"), pub); err != nil {
			log.Printf("%s: jump keys: %v", info(r).id, err)
			writeErr(w, r, 500, "jump_sync_failed", "could not update ssh jump host")
			return
		}
	}
	host := str(vm, "ssh_host")
	if host == "" {
		host = str(vm, "ip")
	}
	port := 22
	if f, ok := vm["ssh_port"].(float64); ok && f > 0 {
		port = int(f)
	}
	out := map[string]any{"host": host, "port": port, "user": s.cfg.SSHUser}
	if s.cfg.SSHJump != "" {
		out["jump"] = s.cfg.SSHJump
	}
	writeJSON(w, 200, out)
}

// authorizeKey appends pub to the VM user's authorized_keys if absent.
func (s *server) authorizeKey(r *http.Request, id, pub string) error {
	path := "/home/" + s.cfg.SSHUser + "/.ssh/authorized_keys"
	if s.cfg.SSHUser == "root" {
		path = "/root/.ssh/authorized_keys"
	}
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(id, "files")+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return err
	}
	cur, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode == 404:
		cur = nil
	case resp.StatusCode/100 != 2:
		return &boatdError{Status: resp.StatusCode, Body: cur}
	}
	fields := strings.Fields(pub)
	for _, line := range strings.Split(string(cur), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == fields[0] && f[1] == fields[1] {
			return nil
		}
	}
	next := string(cur)
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	return s.putVMFile(r, id, path, []byte(next+pub+"\n"))
}

func (s *server) desktopVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.cfg.DesktopURL == "" || len(s.cfg.DesktopKey) == 0 {
		writeErr(w, r, 501, "not_configured", "desktop gateway is not configured")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	// Start the guest desktop proxy with the token the gateway presents as
	// Cookie boat_desktop=<token> (see SignGuestDesktop).
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "desktop"), map[string]string{"action": "start", "token": SignGuestDesktop(s.cfg.DesktopKey, id)}, nil); err != nil {
		writeUpstream(w, r, err)
		return
	}
	exp := time.Now().Add(s.cfg.DesktopTTL).Unix()
	u := fmt.Sprintf("%s/%s?exp=%d&sig=%s", strings.TrimRight(s.cfg.DesktopURL, "/"), url.PathEscape(id), exp, SignDesktop(s.cfg.DesktopKey, id, exp))
	writeJSON(w, 200, map[string]any{"url": u, "expires_at": time.Unix(exp, 0).UTC()})
}
