package main

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"
)

// PATCH /v1/vms/{id}: rename, set the auto-stop TTL, push the auto-stop
// deadline out (boat extend), replace labels, or set idle_stop_s. boatd keeps
// the deadline (expires_at) and does the auto-stop, so this is its PATCH
// /v1/vms/{id}. A boatd without that route answers 404/405: then this is 501
// not_supported. resume_on_boot is the host's (boatd sets it on a clean shutdown
// and restarts such VMs with no API gates): a tenant PATCH refuses it.

const maxTTL = 2592000 // 30 days, as boat.dev

// defaultForkTTLS is the lifetime of a fork or clone that names none (boat.dev: 1 hour).
const defaultForkTTLS = 3600

type patchVMReq struct {
	Name         *string           `json:"name"`
	TTLS         *int64            `json:"ttl_s"`
	ExtendS      *int64            `json:"extend_s"`
	Labels       map[string]string `json:"labels"`
	IdleStopS    *int64            `json:"idle_stop_s"`
	ResumeOnBoot *bool             `json:"resume_on_boot"`
}

func (s *server) patchVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body patchVMReq
	if !decode(w, r, &body) {
		return
	}
	if (body.TTLS != nil || body.ExtendS != nil || body.IdleStopS != nil) && s.refuseSelfLifetime(w, r, p) {
		return
	}
	msg := ""
	switch {
	case body.ResumeOnBoot != nil:
		msg = "resume_on_boot is managed by the host"
	case body.Name == nil && body.TTLS == nil && body.ExtendS == nil && body.Labels == nil && body.IdleStopS == nil:
		msg = "set name, ttl_s, extend_s, labels or idle_stop_s"
	case body.Name != nil && !validNamePtr(body.Name, &msg):
	case body.TTLS != nil && (*body.TTLS < 0 || *body.TTLS > maxTTL):
		msg = "ttl_s must be 0 (no auto-stop) to 2592000"
	case body.ExtendS != nil && (*body.ExtendS < 1 || *body.ExtendS > maxTTL):
		msg = "extend_s must be 1 to 2592000"
	case body.IdleStopS != nil && (*body.IdleStopS < -1 || *body.IdleStopS > maxTTL):
		msg = "idle_stop_s must be -1 (never) to 2592000"
	case body.Labels != nil:
		msg = validLabels(body.Labels)
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	curVM, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		return
	}
	if body.Labels != nil {
		merged := make(map[string]string, len(body.Labels)+1)
		for k, v := range body.Labels {
			merged[k] = v
		}
		if curLabels, ok := curVM["labels"].(map[string]any); ok {
			for k, v := range curLabels {
				if sv, ok := v.(string); ok && strings.HasPrefix(k, reservedLabels) {
					merged[k] = sv
				}
			}
		}
		body.Labels = merged
	}
	display := ""
	if body.Name != nil {
		display = *body.Name // free text; boatd gets its DNS slug
	}
	vm, err := s.patchNamed(r, r.PathValue("id"), body)
	if err != nil {
		if errors.Is(err, errPatchUnsupported) {
			writeErr(w, r, 501, "not_supported", "this boatd cannot rename a VM or move its auto-stop; upgrade boatd")
			return
		}
		writeUpstream(w, r, err)
		return
	}
	if display != "" {
		if err := s.recordDisplayName(r.PathValue("id"), p.Team.ID, display); err != nil {
			s.storeErr(w, r, err)
			return
		}
	}
	writeJSON(w, 200, s.withDisplay(vm))
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
