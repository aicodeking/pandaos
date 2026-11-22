package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// r48: display names. A VM's name is free text (boat.dev: 1-120 characters,
// "api worker"). boatd keeps only a DNS slug (its hostname, also the kernel
// ip= hostname), derived here; the display name is State.VMNames[id], reported
// as `name` by the native and compat views and matched by name filters.

const (
	maxDisplayName = 120
	maxHostname    = 63
	// batchSuffixRoom keeps boatd's "-N" batch suffix (N up to 16) inside 63.
	batchSuffixRoom = 3
)

// validDisplayName trims name and checks it: 1-120 characters, no control
// characters, valid UTF-8. "" (omitted) is the caller's to allow.
func validDisplayName(name string) (string, string) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", "name must be 1-120 characters"
	case !utf8.ValidString(name):
		return "", "name must be valid UTF-8"
	case utf8.RuneCountInString(name) > maxDisplayName:
		return "", "name must be 1-120 characters"
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", "name must not contain control characters"
		}
	}
	return name, ""
}

// vmSlug is the DNS hostname for a display name: lowercase letters, digits
// and dashes, other characters become "-" (runs collapse), trimmed, at most
// max long. "" when nothing usable is left (boatd then names the VM by its id).
func vmSlug(display string, max int) string {
	if len(display) <= max && reVMName.MatchString(display) {
		return display // already a slug: unchanged
	}
	var b strings.Builder
	dash := true // no leading dash
	for _, r := range strings.ToLower(display) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := b.String()
	if len(s) > max {
		s = s[:max]
	}
	return strings.Trim(s, "-")
}

// hostnameOf is the slug sent to boatd for a create, fork or rename: the name
// itself when it already is one.
func hostnameOf(display string) string {
	return vmSlug(display, maxHostname)
}

// batchHostname is the base slug of a count > 1 create, short enough for boatd's "-N".
func batchHostname(display string) string {
	return vmSlug(display, maxHostname-batchSuffixRoom)
}

// batchDisplayName is the display name of the i-th (0-based) VM of a batch,
// numbered like boatd's hostnames and within 120 characters.
func batchDisplayName(display string, i int) string {
	suffix := fmt.Sprintf("-%d", i+1)
	rs := []rune(display)
	if len(rs)+len(suffix) > maxDisplayName {
		rs = rs[:maxDisplayName-len(suffix)]
	}
	return strings.TrimSpace(string(rs)) + suffix
}

// displayName is the name a VM is shown with: its stored display name, else
// the one boatd keeps.
func displayName(st *State, vm VM) string {
	if n := st.VMNames[str(vm, "id")]; n != "" {
		return n
	}
	return str(vm, "name")
}

// withDisplay is publicVM plus what boat-api stores per VM: the display name
// and the environment version the VM is pinned to.
func (s *server) withDisplay(vm VM) VM {
	out := publicVM(vm)
	s.applyDisplay(out)
	return out
}

// applyDisplay sets name and environment_version on out (a copy of a VM).
func (s *server) applyDisplay(out VM) {
	st := s.store.Snapshot()
	id := str(out, "id")
	if n := st.VMNames[id]; n != "" {
		out["name"] = n
	}
	if pin := st.VMEnvPins[id]; pin != nil {
		out["environment_version"] = pin.Version
	}
}

// createDisplayNames maps the new VMs' ids to their display names: the request's
// name, or "name-N" in boatd's order for a batch. nil when no name was given.
func createDisplayNames(name string, ids []string, batch bool) map[string]string {
	if name == "" {
		return nil
	}
	out := make(map[string]string, len(ids))
	for i, id := range ids {
		if batch {
			out[id] = batchDisplayName(name, i)
		} else {
			out[id] = name
		}
	}
	return out
}

// validNamePtr trims *name in place (the request's own copy) and reports whether
// it is a valid display name; if not, *msg says why.
func validNamePtr(name *string, msg *string) bool {
	n, m := validDisplayName(*name)
	if m != "" {
		*msg = m
		return false
	}
	*name = n
	return true
}

// patchNamed is boatdPatch for a request that may rename: boatd gets the DNS slug
// of the new name (the VM id when nothing usable is left). boatd refuses a slug
// another VM of the team has (409); a free-text name that only collides as a slug
// is retried once with the id's tail appended, so two display names like
// "api worker" and "api-worker" can coexist.
func (s *server) patchNamed(r *http.Request, id string, body patchVMReq) (VM, error) {
	if body.Name == nil {
		return s.boatdPatch(r, id, body)
	}
	display := *body.Name
	host := hostnameOf(display)
	if host == "" {
		host = id
	}
	body.Name = &host
	vm, err := s.boatdPatch(r, id, body)
	var be *boatdError
	if errors.As(err, &be) && be.Status == http.StatusConflict && host != display && host != id {
		alt := vmSlug(host, maxHostname-9) + "-" + idTail(id)
		body.Name = &alt
		return s.boatdPatch(r, id, body)
	}
	return vm, err
}

// idTail is up to 8 slug characters of a VM id, for a hostname that must be unique.
func idTail(id string) string {
	t := vmSlug(strings.TrimPrefix(id, "vm-"), 8)
	if t == "" {
		return "vm"
	}
	return t
}

// recordDisplayName stores the display name of VM id.
func (s *server) recordDisplayName(id, team, name string) error {
	return s.store.Apply(func(st *State) error {
		if st.Teams[team] == nil {
			return errors.New("VM team no longer exists")
		}
		st.VMNames[id] = name
		return nil
	})
}
