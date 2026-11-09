package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"time"
)

// boat.dev's saved snapshots on the team snapshots (snapshots.go): a saved
// snapshot is a team snapshot whose name fits boat.dev's name pattern.
// Saving here is synchronous: the answer's snapshot is already ready.

var reSavedName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *server) teamSnapshots(r *http.Request, p *principal) ([]map[string]any, *nativeAnswer) {
	res := s.callNative(r, p, "GET /v1/snapshots", s.listTeamSnapshots, "", nil)
	if !res.ok() {
		return nil, res
	}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	return list.Snapshots, nil
}

func (s *server) savedSnapshot(team string, sn map[string]any) map[string]any {
	size, _ := sn["size_bytes"].(float64)
	src := str(sn, "vm_id")
	if bx, ok := s.store.Snapshot().CompatByVM[src]; ok {
		src = bx
	}
	created := sn["created_at"]
	if created == nil {
		created = time.Now().UTC()
	}
	out := map[string]any{"name": str(sn, "name"), "status": "ready", "sourceSandboxId": src,
		"snapshotId": str(sn, "id"), "createdAt": created}
	if compatTypes[str(sn, "size")] {
		out["type"] = str(sn, "size")
	}
	if size > 0 {
		out["sizeBytes"] = int64(size)
	}
	return out
}

// newestNamed is the newest team snapshot called name, or nil.
func newestNamed(list []map[string]any, name string) map[string]any {
	var best map[string]any
	for _, sn := range list {
		if str(sn, "name") == name && (best == nil || str(sn, "created_at") > str(best, "created_at")) {
			best = sn
		}
	}
	return best
}

func (s *server) compatSavedList(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	q := r.URL.Query()
	seen := map[string]bool{}
	out := []map[string]any{}
	sort.Slice(list, func(i, j int) bool { return str(list[i], "created_at") > str(list[j], "created_at") })
	for _, sn := range list {
		name := str(sn, "name")
		if !reSavedName.MatchString(name) || seen[name] || (q.Get("name") != "" && name != q.Get("name")) ||
			(q.Get("namePrefix") != "" && !hasPrefix(name, q.Get("namePrefix"))) {
			continue
		}
		seen[name] = true
		out = append(out, s.savedSnapshot(p.Team.ID, sn))
	}
	writeCompat(w, 200, "snapshot.saved.list", map[string]any{"snapshots": out})
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func (s *server) compatSavedSave(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		SandboxID string `json:"sandboxId"`
		Name      string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !reSavedName.MatchString(body.Name) {
		writeErr(w, r, 400, "invalid_request", "name must match ^[a-z0-9][a-z0-9-]{0,62}$")
		return
	}
	vmID := body.SandboxID
	if c := s.store.Snapshot().CompatIDs[vmID]; c != nil && c.Team == p.Team.ID {
		vmID = c.VM
	}
	before, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	res := s.callNative(r, p, "POST /v1/vms/{id}/snapshots", s.proxySnapshots, vmID, map[string]any{"name": body.Name})
	if !res.ok() {
		res.relay(w)
		return
	}
	var sn map[string]any
	json.Unmarshal(res.body.Bytes(), &sn)
	// Reusing a name replaces that snapshot, as boat.dev does.
	for _, old := range before {
		if str(old, "name") == body.Name && str(old, "id") != str(sn, "id") {
			s.callNative(r, p, "DELETE /v1/snapshots/{sid}", s.deleteTeamSnapshot, "", nil, "sid", str(old, "id"))
		}
	}
	if sn["team"] == nil {
		sn["team"] = p.Team.ID
	}
	writeCompat(w, http.StatusAccepted, "snapshot.saved.saving", map[string]any{"status": "saving", "snapshot": s.savedSnapshot(p.Team.ID, sn)})
}

func (s *server) compatSavedGet(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	sn := newestNamed(list, r.PathValue("name"))
	if sn == nil {
		writeErr(w, r, 404, "snapshot_not_found", "no saved snapshot "+r.PathValue("name"))
		return
	}
	writeCompat(w, 200, "snapshot.saved.info", map[string]any{"snapshot": s.savedSnapshot(p.Team.ID, sn)})
}

func (s *server) compatSavedDelete(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	name, found := r.PathValue("name"), false
	for _, sn := range list {
		if str(sn, "name") != name {
			continue
		}
		found = true
		if res := s.callNative(r, p, "DELETE /v1/snapshots/{sid}", s.deleteTeamSnapshot, "", nil, "sid", str(sn, "id")); !res.ok() {
			res.relay(w)
			return
		}
	}
	if !found {
		writeErr(w, r, 404, "snapshot_not_found", "no saved snapshot "+name)
		return
	}
	writeCompat(w, 200, "snapshot.saved.deleted", map[string]any{"name": name, "status": "deleted"})
}

// ---- webhooks with boat.dev's events and payloads (deliveries: compat_hooks.go) ----

const hookFormatBoatDev = "boatdev"

var compatHookEvents = map[string]bool{"sandbox.ready": true, "sandbox.hydrated": true, "sandbox.error": true,
	"sandbox.archived": true, "sandbox.degraded": true, "sandbox.recovered": true}

func compatHookView(h *Webhook) map[string]any {
	ev := h.Events
	if ev == nil {
		ev = []string{}
	}
	upd := h.Updated
	if upd.IsZero() {
		upd = h.Created
	}
	return map[string]any{"id": h.ID, "name": nullIfEmpty(h.Name), "url": h.URL, "events": ev, "createdAt": h.Created, "updatedAt": upd}
}

type compatHookReq struct {
	Name   *string   `json:"name"`
	URL    *string   `json:"url"`
	Events *[]string `json:"events"`
}

// check validates a create (all = true) or an update; "" = fine.
func (s *server) checkHookReq(b compatHookReq, all bool) string {
	switch {
	case all && (b.URL == nil || b.Events == nil):
		return "url and events are required"
	case b.URL != nil && !s.validHookURL(*b.URL):
		return "url must be an absolute https URL"
	case b.Name != nil && len(*b.Name) > 100:
		return "name is at most 100 characters"
	case b.Events != nil && len(*b.Events) == 0:
		return "give at least one event"
	}
	if b.Events != nil {
		for _, e := range *b.Events {
			if !compatHookEvents[e] {
				return "unknown event " + e + "; use sandbox.ready, sandbox.hydrated, sandbox.error, sandbox.archived, sandbox.degraded or sandbox.recovered"
			}
		}
	}
	return ""
}

func (s *server) compatHook(w http.ResponseWriter, r *http.Request, p *principal) (*Webhook, bool) {
	h := s.store.Snapshot().Webhooks[r.PathValue("id")]
	if h == nil || h.TeamID != p.Team.ID || h.Format != hookFormatBoatDev {
		writeErr(w, r, 404, "not_found", "webhook not found")
		return nil, false
	}
	return h, true
}

func (s *server) compatHookList(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []map[string]any{}
	var hooks []*Webhook
	for _, h := range s.store.Snapshot().Webhooks {
		if h.TeamID == p.Team.ID && h.Format == hookFormatBoatDev {
			hooks = append(hooks, h)
		}
	}
	sort.Slice(hooks, func(i, j int) bool { return hooks[i].Created.Before(hooks[j].Created) })
	for _, h := range hooks {
		out = append(out, compatHookView(h))
	}
	writeCompat(w, 200, "webhook.list", map[string]any{"webhooks": out})
}

func (s *server) compatHookCreate(w http.ResponseWriter, r *http.Request, p *principal) {
	var body compatHookReq
	if !decode(w, r, &body) {
		return
	}
	if msg := s.checkHookReq(body, true); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	now := time.Now().UTC()
	h := &Webhook{ID: "wh_" + randHex(12), TeamID: p.Team.ID, URL: *body.URL, Events: *body.Events, Created: now, Updated: now,
		Format: hookFormatBoatDev}
	if body.Name != nil {
		h.Name = *body.Name
	}
	secret := "whsec_" + randHex(32)
	if !s.saveNewHook(w, r, p, h, secret) {
		return
	}
	writeCompat(w, 201, "webhook.created", map[string]any{"webhook": compatHookView(h), "secret": secret})
}

func (s *server) compatHookGet(w http.ResponseWriter, r *http.Request, p *principal) {
	if h, ok := s.compatHook(w, r, p); ok {
		writeCompat(w, 200, "webhook.info", map[string]any{"webhook": compatHookView(h)})
	}
}

func (s *server) compatHookUpdate(w http.ResponseWriter, r *http.Request, p *principal) {
	var body compatHookReq
	if !decode(w, r, &body) {
		return
	}
	if msg := s.checkHookReq(body, false); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	h, ok := s.compatHook(w, r, p)
	if !ok {
		return
	}
	var out Webhook
	err := s.store.Update(func(st *State) error {
		cur := st.Webhooks[h.ID]
		if cur == nil {
			return errConflict
		}
		if body.Name != nil {
			cur.Name = *body.Name
		}
		if body.URL != nil {
			cur.URL = *body.URL
		}
		if body.Events != nil {
			cur.Events = *body.Events
		}
		cur.Updated = time.Now().UTC()
		out = *cur
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeCompat(w, 200, "webhook.updated", map[string]any{"webhook": compatHookView(&out)})
}

func (s *server) compatHookDelete(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.compatHook(w, r, p); !ok {
		return
	}
	res := s.callNative(r, p, "DELETE /v1/webhooks/{id}", s.deleteHook, r.PathValue("id"), nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "webhook.deleted", map[string]any{"id": r.PathValue("id")})
}

// compatHookRotate replaces the secret at once: boat.dev's signature header
// carries one signature, so there is no overlap with the old secret.
func (s *server) compatHookRotate(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.compatHook(w, r, p); !ok {
		return
	}
	secret := "whsec_" + randHex(32)
	sealed, err := s.keys.Seal([]byte(secret), "webhook/"+r.PathValue("id"))
	if err != nil {
		writeErr(w, r, 500, "crypto_error", "could not encrypt")
		return
	}
	var out Webhook
	err = s.store.Update(func(st *State) error {
		cur := st.Webhooks[r.PathValue("id")]
		if cur == nil {
			return errConflict
		}
		cur.SealedSecret, cur.PrevSealedSecret, cur.PrevUntil, cur.Updated = sealed, "", nil, time.Now().UTC()
		out = *cur
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeCompat(w, 200, "webhook.rotated", map[string]any{"webhook": compatHookView(&out), "secret": secret})
}
