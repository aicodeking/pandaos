package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// boat.dev's host (public port URLs), desktop and sshkey on the native port
// routes (ports.go), desktop links and ssh (vms.go).
//
//   - host: POST /v1/vms/{id}/ports, then GET …/ports/{port}/url. A private
//     port's URL carries this server's boat_exp/boat_sig (not boat.dev's
//     _token) and lasts 7 days; calling host again mints a fresh one.
//   - desktop: GET /v1/vms/{id}/desktop, mode "vnc". The link is the signed
//     desktop link; publicAccess is not offered.
//   - sshkey: POST /v1/vms/{id}/ssh. machineIp is the VM's address; with a
//     jump host (-ssh-jump), sshEndpoint names it.

type compatHostReq struct {
	Port      int    `json:"port"`
	Public    bool   `json:"public"`
	Localhost bool   `json:"localhost"`
	Title     string `json:"title"`
}

// hostPortRoutes are the native routes hostPort runs.
var hostPortRoutes = []string{"POST /v1/vms/{id}/ports", "GET /v1/vms/{id}/ports/{port}/url"}

// hostPort exposes one port of vmID and returns boat.dev's entry for it, or
// the native answer that refused it.
func (s *server) hostPort(r *http.Request, p *principal, vmID string, c compatHostReq) (map[string]any, *nativeAnswer) {
	auth := "private"
	if c.Public {
		auth = "public"
	}
	add := s.callNative(r, p, "POST /v1/vms/{id}/ports", s.addPort, vmID, map[string]any{"port": c.Port, "auth": auth})
	if !add.ok() && add.status != http.StatusConflict { // 409: already exposed, as host is idempotent
		return nil, add
	}
	q := "ttl_s=" + strconv.Itoa(int(maxPortURLTTL.Seconds()))
	res := s.callNative(r, p, "GET /v1/vms/{id}/ports/{port}/url", s.portURL, vmID, nil, "port", strconv.Itoa(c.Port), "?", q)
	if !res.ok() {
		return nil, res
	}
	var u struct {
		URL  string `json:"url"`
		Auth string `json:"auth"`
	}
	json.Unmarshal(res.body.Bytes(), &u)
	access := orDefault(u.Auth, auth)
	return map[string]any{"success": true, "port": c.Port, "url": u.URL, "isProtected": access == "private",
		"access": access, "localhost": false}, nil
}

func validHostReq(c compatHostReq) string {
	switch {
	case c.Port < 1 || c.Port > 65535:
		return "port must be 1-65535"
	case c.Localhost:
		return "localhost is not supported by this server: the port route reaches the VM's own address"
	}
	return ""
}

func (s *server) compatHost(w http.ResponseWriter, r *http.Request, p *principal) {
	var c compatHostReq
	if !decode(w, r, &c) {
		return
	}
	if msg := validHostReq(c); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	out, fail := s.hostPort(r, p, r.PathValue("id"), c)
	if fail != nil {
		fail.relay(w)
		return
	}
	writeCompat(w, 200, "port.hosted", out)
}

// compatHostedPorts hosts create's hostedPorts on vmID: one entry per port,
// in request order, an error entry for a port that could not be hosted.
func (s *server) compatHostedPorts(r *http.Request, p *principal, vmID string, ports []compatHostReq) []any {
	out := make([]any, 0, len(ports))
	for _, c := range ports {
		entry, fail := s.hostPort(r, p, vmID, c)
		if fail != nil {
			var e struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(fail.body.Bytes(), &e)
			entry = map[string]any{"port": c.Port, "error": orDefault(e.Error.Code, "host_failed"), "message": e.Error.Message}
		}
		out = append(out, entry)
	}
	return out
}

// hostedPortsMsg validates create's hostedPorts like boat.dev (400 messages).
func hostedPortsMsg(ports []compatHostReq, count int, failFast bool) string {
	if len(ports) == 0 {
		return ""
	}
	seen := map[int]bool{}
	for _, c := range ports {
		if msg := validHostReq(c); msg != "" || seen[c.Port] {
			return "invalid_hosted_ports: " + orDefault(msg, fmt.Sprintf("port %d twice", c.Port))
		}
		seen[c.Port] = true
	}
	switch {
	case len(ports) > 50:
		return "invalid_hosted_ports: at most 50"
	case count > 1:
		return "count_with_hosted_ports: hostedPorts cannot be combined with count"
	case failFast:
		return "fail_fast_with_hosted_ports: hostedPorts cannot be combined with failFast"
	}
	return ""
}

func (s *server) compatDesktop(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		PublicAccess bool `json:"publicAccess"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	if body.PublicAccess {
		writeErr(w, r, 400, "invalid_request", "publicAccess is not supported by this server: desktop links are signed and short-lived")
		return
	}
	if v := r.URL.Query().Get("vnc"); v != "" && v != "1" {
		writeErr(w, r, 400, "invalid_query", "vnc must be 1")
		return
	}
	res := s.callNative(r, p, "GET /v1/vms/{id}/desktop", s.desktopVM, r.PathValue("id"), nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	var d struct {
		URL string `json:"url"`
	}
	json.Unmarshal(res.body.Bytes(), &d)
	vm, _ := s.vmOf(r, r.PathValue("id"))
	writeCompat(w, 200, "desktop.url", map[string]any{"success": true, "desktopUrl": d.URL, "ip": nullIfEmpty(str(vm, "ip")),
		"mode": "vnc", "provisioning": false})
}

// vmOf reads a VM from boatd (nil on error; callers have checked ownership).
func (s *server) vmOf(r *http.Request, id string) (VM, error) {
	var vm VM
	err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm)
	return vm, err
}

func (s *server) compatSSHKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Key) == "" {
		writeErr(w, r, 400, "invalid_request", "key is required: one OpenSSH public key line")
		return
	}
	res := s.callNative(r, p, "POST /v1/vms/{id}/ssh", s.sshVM, r.PathValue("id"), map[string]any{"public_key": body.Key})
	if !res.ok() {
		res.relay(w)
		return
	}
	var ssh struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		User string `json:"user"`
		Jump string `json:"jump"`
	}
	json.Unmarshal(res.body.Bytes(), &ssh)
	var endpoint any
	if ssh.Jump != "" {
		endpoint = fmt.Sprintf("%s:%d via %s", ssh.Host, ssh.Port, ssh.Jump)
	} else if ssh.Port != 0 && ssh.Port != 22 {
		endpoint = fmt.Sprintf("%s:%d", ssh.Host, ssh.Port)
	}
	writeCompat(w, 200, "ssh_key.configured", map[string]any{"success": true, "machineIp": nullIfEmpty(ssh.Host),
		"sshUser": ssh.User, "sshEndpoint": endpoint})
}
