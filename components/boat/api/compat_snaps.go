package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// boat.dev's snapshot listings, named snapshots and saved-snapshot shares on
// the team snapshots (snapshots.go).
//
// boat.dev's snapshot ids are UUIDs; a native snapshot id maps onto one with
// snapshotUUID (a hash, stable, no state). A sandbox's captures are its
// unnamed snapshots; named ones are saved (named) snapshots, which
// DELETE /sandboxes/{id}/snapshots keeps, as boat.dev does. Every snapshot here
// is a full one: kind, generation and chainId are null, fileCount unknown.

// snapshotUUID is the UUID boat.dev clients see for a native snapshot id.
func snapshotUUID(team, sid string) string {
	h := sha256.Sum256([]byte("boat snapshot\x00" + team + "\x00" + sid))
	h[6] = h[6]&0x0f | 0x80 // version 8 (custom)
	h[8] = h[8]&0x3f | 0x80 // RFC 4122 variant
	x := hex.EncodeToString(h[:16])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

func (s *server) snapshotSummary(team string, sn map[string]any) map[string]any {
	created := sn["created_at"]
	if created == nil || created == "" {
		created = time.Now().UTC()
	}
	var size any
	if f, ok := sn["size_bytes"].(float64); ok {
		size = int64(f)
	}
	sandbox := str(sn, "vm_id") // a deleted VM's snapshot keeps its native id: no bx_ id is minted for it
	if bx, ok := s.store.Snapshot().CompatByVM[sandbox]; ok {
		sandbox = bx
	}
	return map[string]any{"id": snapshotUUID(team, str(sn, "id")), "sandboxId": sandbox, "status": "completed",
		"kind": nil, "generation": nil, "chainId": nil, "createdAt": created, "completedAt": created,
		"sizeBytes": size, "fileCount": nil, "origin": "captured"}
}

// snapshotPage sorts (sort=asc|desc, default desc), applies limit (1-200,
// default 100) and answers boat.dev's snapshot.list with a scoped continuation.
func (s *server) snapshotPage(w http.ResponseWriter, r *http.Request, team string, list []map[string]any) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(orDefault(q.Get("limit"), "100"))
	order := orDefault(q.Get("sort"), "desc")
	if err != nil || limit < 1 || limit > 200 || (order != "asc" && order != "desc") {
		writeErr(w, r, 400, "invalid_query", "limit 1-200, sort asc or desc")
		return
	}
	// Scope the opaque cursor to the team, listing route and order. The last
	// (timestamp, native id) pair makes ties stable even if boatd changes order.
	type cursorKey struct {
		Team    string `json:"team"`
		Path    string `json:"path"`
		Order   string `json:"order"`
		Created string `json:"created"`
		ID      string `json:"id"`
	}
	key := func(sn map[string]any) string { return str(sn, "created_at") + "\x00" + str(sn, "id") }
	after := ""
	if raw := q.Get("cursor"); raw != "" {
		var c cursorKey
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(raw) > 8192 || json.Unmarshal(data, &c) != nil || c.Team != team || c.Path != r.URL.Path || c.Order != order || c.ID == "" {
			writeErr(w, r, 400, "invalid_query", "invalid or mismatched snapshot cursor")
			return
		}
		after = c.Created + "\x00" + c.ID
	}
	sort.Slice(list, func(i, j int) bool {
		if order == "asc" {
			return key(list[i]) < key(list[j])
		}
		return key(list[i]) > key(list[j])
	})
	start := 0
	if after != "" {
		start = sort.Search(len(list), func(i int) bool {
			if order == "asc" {
				return key(list[i]) > after
			}
			return key(list[i]) < after
		})
	}
	end := min(start+limit, len(list))
	more := end < len(list)
	list = list[start:end]
	var next any
	if more {
		last := list[len(list)-1]
		data, _ := json.Marshal(cursorKey{team, r.URL.Path, order, str(last, "created_at"), str(last, "id")})
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	out := make([]map[string]any, len(list))
	for i, sn := range list {
		out[i] = s.snapshotSummary(team, sn)
	}
	writeCompat(w, 200, "snapshot.list", map[string]any{"snapshots": out,
		"pageInfo": map[string]any{"nextCursor": next, "hasMore": more, "limit": limit}})
}

func (s *server) vmSnapshots(r *http.Request, p *principal, vmID string) ([]map[string]any, *nativeAnswer) {
	res := s.callNative(r, p, "GET /v1/vms/{id}/snapshots", s.proxySnapshots, vmID, nil)
	if !res.ok() {
		return nil, res
	}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	return list.Snapshots, nil
}

func (s *server) compatTeamSnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	s.snapshotPage(w, r, p.Team.ID, list)
}

func (s *server) compatSandboxSnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.vmSnapshots(r, p, r.PathValue("id"))
	if fail != nil {
		fail.relay(w)
		return
	}
	s.snapshotPage(w, r, p.Team.ID, list)
}

func (s *server) compatLatestSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.vmSnapshots(r, p, r.PathValue("id"))
	if fail != nil {
		fail.relay(w)
		return
	}
	var latest any
	var best map[string]any
	for _, sn := range list {
		if best == nil || str(sn, "created_at") > str(best, "created_at") {
			best = sn
		}
	}
	if best != nil {
		latest = s.snapshotSummary(p.Team.ID, best)
	}
	writeCompat(w, 200, "snapshot.latest", map[string]any{"snapshot": latest})
}

func newDeletionOp(kind, target string) map[string]any {
	b := make([]byte, 16)
	rand.Read(b)
	now := time.Now().UTC()
	return map[string]any{"id": "bdop_" + hex.EncodeToString(b), "kind": kind, "targetId": target, "reason": "explicit",
		"status": "completed", "stage": "completed", "expectedBy": nil, "attemptCount": 1, "requestedAt": now, "completedAt": now}
}

// compatDeleteSandboxSnapshots deletes the sandbox's captures (its unnamed
// snapshots); its saved snapshots stay.
func (s *server) compatDeleteSandboxSnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	asked := orDefault(info(r).compatID, id)
	if c := r.Header.Get("X-Ascii-Confirm-Delete"); c != asked && c != id {
		writeErr(w, r, 409, "confirmation_required", "set X-Ascii-Confirm-Delete to the sandbox id")
		return
	}
	list, fail := s.vmSnapshots(r, p, id)
	if fail != nil {
		fail.relay(w)
		return
	}
	ops := []map[string]any{}
	for _, sn := range list {
		if str(sn, "name") != "" {
			continue
		}
		res := s.callNative(r, p, "DELETE /v1/snapshots/{sid}", s.deleteTeamSnapshot, "", nil, "sid", str(sn, "id"))
		if !res.ok() {
			res.relay(w)
			return
		}
		op := newDeletionOp("snapshot", snapshotUUID(p.Team.ID, str(sn, "id")))
		s.deletions.add(p.Team.ID, op)
		ops = append(ops, op)
	}
	writeCompat(w, http.StatusAccepted, "snapshot.deleting", map[string]any{"operations": ops})
}

// compatDeleteSnapshot is DELETE /snapshots/{snapshotId} (a UUID from the lists).
func (s *server) compatDeleteSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	want := r.PathValue("sid")
	if r.Header.Get("X-Ascii-Confirm-Delete") != want {
		writeErr(w, r, 409, "confirmation_required", "set X-Ascii-Confirm-Delete to the snapshot id")
		return
	}
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	for _, sn := range list {
		if snapshotUUID(p.Team.ID, str(sn, "id")) != want {
			continue
		}
		res := s.callNative(r, p, "DELETE /v1/snapshots/{sid}", s.deleteTeamSnapshot, "", nil, "sid", str(sn, "id"))
		if !res.ok() {
			res.relay(w)
			return
		}
		op := newDeletionOp("snapshot", want)
		s.deletions.add(p.Team.ID, op)
		writeCompat(w, http.StatusAccepted, "snapshot.deleting", map[string]any{"operation": op})
		return
	}
	writeErr(w, r, 404, "snapshot_not_found", "no snapshot "+want)
}

// ---- named snapshots: boat.dev's other name for saved snapshots ----

// asNamed runs a saved-snapshot handler and answers with boat.dev's
// snapshot.named.* types instead of snapshot.saved.*.
func (s *server) asNamed(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		rec := &nativeAnswer{h: http.Header{}}
		h(rec, r, p)
		var out map[string]any
		if !rec.ok() || json.Unmarshal(rec.body.Bytes(), &out) != nil || out["ok"] != true {
			rec.relay(w)
			return
		}
		typ, _ := out["type"].(string)
		delete(out, "ok")
		delete(out, "type")
		writeCompat(w, rec.status, strings.Replace(typ, "snapshot.saved.", "snapshot.named.", 1), out)
	}
}

// ---- shares of saved snapshots (POST /v1/vms/{id}/snapshots/{sid}/shares) ----

// savedSnapshotRef is the newest team snapshot called name, with its VM.
func (s *server) savedSnapshotRef(w http.ResponseWriter, r *http.Request, p *principal) (vmID, sid string, ok bool) {
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return "", "", false
	}
	sn := newestNamed(list, r.PathValue("name"))
	if sn == nil || !reSavedName.MatchString(r.PathValue("name")) {
		writeErr(w, r, 404, "snapshot_not_found", "no saved snapshot "+r.PathValue("name"))
		return "", "", false
	}
	if str(sn, "vm_id") == "" {
		writeErr(w, r, 409, "not_shareable", "an imported snapshot cannot be shared again")
		return "", "", false
	}
	return str(sn, "vm_id"), str(sn, "id"), true
}

func compatShareView(v shareView) map[string]any {
	return map[string]any{"id": "shr_" + v.ID, "codePrefix": "bsh_" + v.ID, "status": "active", "useCount": v.Uses,
		"lastUsedAt": nil, "revokedAt": nil, "createdAt": v.Created}
}

func (s *server) compatCreateShare(w http.ResponseWriter, r *http.Request, p *principal) {
	vmID, sid, ok := s.savedSnapshotRef(w, r, p)
	if !ok {
		return
	}
	// A share code: a link for any account, for the longest time links live.
	res := s.callNative(r, p, "POST /v1/vms/{id}/snapshots/{sid}/shares", s.createShare, vmID,
		map[string]any{"ttl_s": int64(maxLinkTTL.Seconds())}, "sid", sid)
	if !res.ok() {
		res.relay(w)
		return
	}
	var v shareView
	json.Unmarshal(res.body.Bytes(), &v)
	writeCompat(w, http.StatusCreated, "snapshot.share.created", map[string]any{"name": r.PathValue("name"),
		"shareCode": v.Code, "share": compatShareView(v)})
}

func (s *server) compatListShares(w http.ResponseWriter, r *http.Request, p *principal) {
	vmID, sid, ok := s.savedSnapshotRef(w, r, p)
	if !ok {
		return
	}
	res := s.callNative(r, p, "GET /v1/vms/{id}/snapshots/{sid}/shares", s.listShares, vmID, nil, "sid", sid)
	if !res.ok() {
		res.relay(w)
		return
	}
	var list struct {
		Shares []shareView `json:"shares"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	out := []map[string]any{}
	for _, v := range list.Shares {
		if v.Grantee == "" { // codes only; team grants are the native API's
			out = append(out, compatShareView(v))
		}
	}
	writeCompat(w, 200, "snapshot.share.list", map[string]any{"name": r.PathValue("name"), "shares": out})
}

func (s *server) compatRevokeShare(w http.ResponseWriter, r *http.Request, p *principal) {
	vmID, sid, ok := s.savedSnapshotRef(w, r, p)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.PathValue("share"), "shr_")
	sh := s.store.Snapshot().SnapShares[id]
	if sh == nil || sh.SnapshotID != sid || sh.TeamID != p.Team.ID {
		writeErr(w, r, 404, "not_found", fmt.Sprintf("share %s not found", r.PathValue("share")))
		return
	}
	v := compatShareView(viewShare(sh))
	res := s.callNative(r, p, "DELETE /v1/vms/{id}/snapshots/{sid}/shares/{share}", s.revokeShare, vmID, nil, "sid", sid, "share", id)
	if !res.ok() {
		res.relay(w)
		return
	}
	v["status"], v["revokedAt"] = "revoked", time.Now().UTC()
	writeCompat(w, 200, "snapshot.share.revoked", map[string]any{"name": r.PathValue("name"), "share": v})
}
