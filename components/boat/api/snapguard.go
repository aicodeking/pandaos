package main

import (
	"errors"
	"net/http"
	"net/url"
)

// refuseBuilderSnapshot extends the environment-builder gate (refuseBuilder) to
// the team-level snapshot routes, which resolve a snapshot by team only: a key
// without scope admin may not delete (or overwrite by name) a snapshot taken
// from an environment builder VM, nor an environment's build snapshot. It
// reports whether it answered (the builder 403, or the upstream failure: the
// builder lookup fails closed).
func (s *server) refuseBuilderSnapshot(w http.ResponseWriter, r *http.Request, p *principal, sn map[string]any) bool {
	if p.has("admin") {
		return false
	}
	sid := str(sn, "id")
	if sid != "" {
		for _, env := range s.store.Snapshot().Environments {
			if env != nil && env.TeamID == p.Team.ID && env.Build != nil && env.Build.SnapshotID == sid {
				writeErr(w, r, http.StatusForbidden, "forbidden", builderSnapshotMsg)
				return true
			}
		}
	}
	src := str(sn, "vm_id")
	if src == "" {
		return false
	}
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(src), nil, &vm); err != nil {
		var be *boatdError
		if errors.As(err, &be) && be.Status == http.StatusNotFound {
			return false // the source VM is gone: nothing of a builder is left
		}
		writeUpstream(w, r, err)
		return true
	}
	return s.refuseBuilder(w, r, p, vm)
}

const builderSnapshotMsg = "this snapshot belongs to an environment builder VM, which holds the environment's secrets: only an admin key may delete or replace it"

// snapshotsNamed is the snapshots of list named name.
func snapshotsNamed(list []map[string]any, name string) []map[string]any {
	out := []map[string]any{}
	for _, sn := range list {
		if str(sn, "name") == name {
			out = append(out, sn)
		}
	}
	return out
}

// refuseAnyBuilderSnapshot runs refuseBuilderSnapshot over snaps before anything
// is deleted, so a refusal never leaves a partial delete.
func (s *server) refuseAnyBuilderSnapshot(w http.ResponseWriter, r *http.Request, p *principal, snaps []map[string]any) bool {
	for _, sn := range snaps {
		if s.refuseBuilderSnapshot(w, r, p, sn) {
			return true
		}
	}
	return false
}

// refuseCreateFromBuilder applies the builder gate to a create whose snapshot
// the caller named (an unknown or foreign id is left to the create itself). It
// reports whether it answered.
func (s *server) refuseCreateFromBuilder(w http.ResponseWriter, r *http.Request, p *principal, sid string) bool {
	if p.has("admin") {
		return false
	}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(p.Team.ID), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return true
	}
	for _, sn := range list.Snapshots {
		if str(sn, "id") == sid && str(sn, "team") == p.Team.ID {
			return s.refuseBuilderSnapshot(w, r, p, sn)
		}
	}
	return false
}
