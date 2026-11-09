package main

import (
	"bytes"
	"errors"
	"log"
	"net/http"
)

// PATCH /v1/vms/{id}: rename, set the auto-stop TTL, or push the auto-stop
// deadline out (boat extend). boatd keeps the deadline (expires_at) and does
// the auto-stop, so this is its PATCH /v1/vms/{id}. A boatd without that
// route answers 404/405: then this is 501 not_supported.

const maxTTL = 2592000 // 30 days, as boat.dev

type patchVMReq struct {
	Name    *string `json:"name"`
	TTLS    *int64  `json:"ttl_s"`
	ExtendS *int64  `json:"extend_s"`
}

func (s *server) patchVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body patchVMReq
	if !decode(w, r, &body) {
		return
	}
	msg := ""
	switch {
	case body.Name == nil && body.TTLS == nil && body.ExtendS == nil:
		msg = "set name, ttl_s or extend_s"
	case body.Name != nil && !reVMName.MatchString(*body.Name):
		msg = "invalid name"
	case body.TTLS != nil && (*body.TTLS < 0 || *body.TTLS > maxTTL):
		msg = "ttl_s must be 0 (no auto-stop) to 2592000"
	case body.ExtendS != nil && (*body.ExtendS < 1 || *body.ExtendS > maxTTL):
		msg = "extend_s must be 1 to 2592000"
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	vm, err := s.boatdPatch(r, r.PathValue("id"), body)
	if err != nil {
		if errors.Is(err, errPatchUnsupported) {
			writeErr(w, r, 501, "not_supported", "this boatd cannot rename a VM or move its auto-stop; upgrade boatd")
			return
		}
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, publicVM(vm))
}

var errPatchUnsupported = errors.New("boatd has no PATCH /v1/vms/{id}")

func (s *server) boatdPatch(r *http.Request, id string, body patchVMReq) (VM, error) {
	var vm VM
	err := s.boatd.call(r.Context(), "PATCH", vmPath(id), body, &vm)
	var be *boatdError
	if errors.As(err, &be) && (be.Status == http.StatusMethodNotAllowed || (be.Status == http.StatusNotFound && !bytes.HasPrefix(bytes.TrimSpace(be.Body), []byte("{")))) {
		return nil, errPatchUnsupported
	}
	return vm, err
}

// refreshTTL gives a VM whose agent starts working a fresh full TTL, as
// boat.dev does for a prompt. Best effort: a boatd without PATCH keeps the old deadline.
func (s *server) refreshTTL(r *http.Request, vm VM) {
	ttl, _ := vm["ttl_s"].(float64)
	if ttl <= 0 {
		return
	}
	t := int64(ttl)
	if _, err := s.boatdPatch(r, str(vm, "id"), patchVMReq{TTLS: &t}); err != nil && !errors.Is(err, errPatchUnsupported) {
		log.Printf("%s: refresh the auto-stop of %s: %v", info(r).id, str(vm, "id"), err)
	}
}
