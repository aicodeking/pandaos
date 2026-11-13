package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// boat.dev's PATCH /sandboxes/{id}, /me and /sandboxes/{id}/usage.

func (s *server) compatUpdate(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name       *string         `json:"name"`
		TTLSeconds json.RawMessage `json:"ttlSeconds"`
		Subdomain  *string         `json:"subdomain"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Subdomain != nil {
		writeErr(w, r, http.StatusNotImplemented, "not_supported", "subdomain is not supported on self-hosted boat: "+
			"port URLs are <port>-<vm id>.<vm domain>, and the gateway's route tokens and TLS certificates are bound to that name; "+
			"nothing was changed")
		return
	}
	in := map[string]any{}
	if body.Name != nil {
		in["name"] = *body.Name
	}
	if len(body.TTLSeconds) > 0 {
		ttl, ok := compatTTL(body.TTLSeconds)
		if !ok {
			writeErr(w, r, 400, "invalid_request", "ttlSeconds must be 1-2592000, null or \"infinite\"")
			return
		}
		in["ttl_s"] = ttl
	}
	res := s.callNative(r, p, "PATCH /v1/vms/{id}", s.patchVM, r.PathValue("id"), in)
	if !res.ok() {
		res.relay(w)
		return
	}
	var vm VM
	json.Unmarshal(res.body.Bytes(), &vm)
	writeCompat(w, 200, "sandbox.updated", map[string]any{"sandbox": s.sandbox(vm)})
}

func (s *server) compatMe(w http.ResponseWriter, r *http.Request, p *principal) {
	writeCompat(w, 200, "user.info", map[string]any{"user": map[string]any{"id": p.User.ID, "login": p.User.Name,
		"displayName": p.User.Name, "serviceAccount": false, "accountStatus": "active",
		"suspended": false, "closed": false, "accountPlan": "self_hosted"}})
}

// compatMultiplier is boat.dev's billing multiplier of a machine type.
var compatMultiplier = map[string]float64{"small": 0.5, "default": 1, "large": 2, "xlarge": 50.0 / 9}

func (s *server) compatUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	res := s.callNative(r, p, "GET /v1/vms/{id}/usage", s.vmUsage, r.PathValue("id"), nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	var u struct {
		Seconds float64 `json:"vm_seconds"`
	}
	json.Unmarshal(res.body.Bytes(), &u)
	typ := str(vm, "size")
	if !compatTypes[typ] {
		typ = "default"
	}
	since, _ := time.Parse(time.RFC3339, str(vm, "created_at"))
	writeCompat(w, 200, "sandbox.usage", map[string]any{"sandboxId": s.sandbox(vm)["id"], "sandboxType": typ,
		"billingMultiplier": compatMultiplier[typ], "since": since.UTC(), "until": time.Now().UTC(),
		"seconds": int64(u.Seconds), "dollars": 0, "secondsPerDollar": 0, "running": str(vm, "state") == "running"})
}
