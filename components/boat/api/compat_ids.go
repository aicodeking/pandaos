package main

import (
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"strings"
)

// boat.dev sandbox ids: the official SDKs check every Sandbox id against
// ^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$, and native ids are vm-<hex>.
// /api/v1 gives each VM a bx_ id on first sight, stable for the VM's life:
// persisted in state.json (CompatIDs / CompatByVM), drawn at random and
// checked against every id in use, and forgotten with the VM.

const (
	compatIDPrefix   = "bx_"
	compatIDAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"
	compatIDLen      = 8
)

// CompatID is one bx_ id: the VM it names and that VM's team.
type CompatID struct {
	VM   string `json:"vm"`
	Team string `json:"team"`
}

func newCompatID() string {
	b := make([]byte, compatIDLen)
	rand.Read(b)
	for i := range b {
		b[i] = compatIDAlphabet[int(b[i])%len(compatIDAlphabet)] // 256 % 31: a slight bias is harmless here
	}
	return compatIDPrefix + string(b)
}

func validCompatID(id string) bool {
	rest, ok := strings.CutPrefix(id, compatIDPrefix)
	if !ok || len(rest) != compatIDLen {
		return false
	}
	for _, c := range rest {
		if !strings.ContainsRune(compatIDAlphabet, c) {
			return false
		}
	}
	return true
}

// assignCompatIDs gives every vm id that has none a fresh bx_ id. Caller
// passes a state it may change (inside Store.Update).
func assignCompatIDs(st *State, team string, vms []string, draw func() string) error {
	for _, vm := range vms {
		if _, ok := st.CompatByVM[vm]; ok || vm == "" {
			continue
		}
		var id string
		for tries := 0; ; tries++ {
			if tries == 64 {
				return errors.New("no free sandbox id")
			}
			if id = draw(); st.CompatIDs[id] == nil {
				break
			}
		}
		st.CompatIDs[id] = &CompatID{VM: vm, Team: team}
		st.CompatByVM[vm] = id
	}
	return nil
}

// compatIDs returns the bx_ id of each VM, assigning missing ones in one write.
func (s *server) compatIDs(team string, vms []string) map[string]string {
	out := map[string]string{}
	var missing []string
	st := s.store.Snapshot()
	for _, vm := range vms {
		if id, ok := st.CompatByVM[vm]; ok {
			out[vm] = id
		} else {
			missing = append(missing, vm)
		}
	}
	if len(missing) == 0 {
		return out
	}
	draw := s.drawCompatID
	if draw == nil {
		draw = newCompatID
	}
	if err := s.store.Update(func(st *State) error { return assignCompatIDs(st, team, missing, draw) }); err != nil {
		log.Printf("compat ids: %v", err) // the VM shows its native id; the SDK then refuses it
	}
	st = s.store.Snapshot()
	for _, vm := range missing {
		out[vm] = orDefault(st.CompatByVM[vm], vm)
	}
	return out
}

func (s *server) compatID(team, vm string) string { return s.compatIDs(team, []string{vm})[vm] }

// compatResolve turns the bx_ id in an /api/v1 path into the VM id the native
// handlers use. Another team's id and an unknown one are both 404. A native
// vm- id passes through, so ids from /v1 work here too.
func (s *server) compatResolve(w http.ResponseWriter, r *http.Request, p *principal) bool {
	id := r.PathValue("id")
	if !strings.HasPrefix(id, compatIDPrefix) {
		return true
	}
	c := s.store.Snapshot().CompatIDs[id]
	if c == nil || c.Team != p.Team.ID {
		writeErr(w, r, http.StatusNotFound, "not_found", "sandbox not found")
		return false
	}
	info(r).compatID = id
	r.SetPathValue("id", c.VM)
	return true
}

// forgetCompatID drops a deleted VM's bx_ id (inside Store.Update).
func forgetCompatID(st *State, vm string) {
	if id, ok := st.CompatByVM[vm]; ok {
		delete(st.CompatIDs, id)
		delete(st.CompatByVM, vm)
	}
}
