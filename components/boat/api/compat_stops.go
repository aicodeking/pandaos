package main

import (
	"log"
	"strings"
	"time"
)

// stopOps are boat.dev's stop operations: the last stop asked of each VM
// (Sandbox.stop) and every stop by id (GET /sandboxes/{id}/stops/{stopId}).
// A stop through this server finishes before it answers, so an operation is
// completed, failing (the native stop failed) or superseded (a resume came).
// They live in the store (State.StopOps), so a restart keeps them.
type stopOps struct{ store *Store }

const (
	maxStopOpsPerTeam = 1024
	maxStopOps        = 32768
)

func evictStopOp(st *State, id string) {
	if op := st.StopOps[id]; op != nil {
		if vm, _ := op["_vm"].(string); vm != "" && st.StopLast[vm] == id {
			delete(st.StopLast, vm)
		}
	}
	delete(st.StopOps, id)
}

func (o *stopOps) begin(team, vm string) map[string]any {
	now := time.Now().UTC()
	op := map[string]any{"id": "stop_" + randHex(8), "status": "pending", "requestedAt": now, "lastAttemptAt": nil,
		"error": nil, "endedAt": nil, "_team": team, "_vm": vm}
	o.stage(func(st *State) {
		// The run-end order is by sequence, not by clock (r37_seq.go): taken here,
		// in the mutation that records the stop.
		op["_seq"] = int64(st.nextSeq())
		teamCount := 0
		for _, cur := range st.StopOps {
			if cur["_team"] == team && !isMarkRecord(cur) {
				teamCount++
			}
		}
		for teamCount >= maxStopOpsPerTeam {
			evict := oldestTeamOpID(st.StopOps, team)
			if evict == "" {
				break
			}
			evictStopOp(st, evict)
			teamCount--
		}
		for len(st.StopOps) >= maxStopOps {
			evict := oldestTeamOpID(st.StopOps, "")
			if evict == "" {
				break
			}
			evictStopOp(st, evict)
		}
		st.StopOps[op["id"].(string)] = op
		st.StopLast[vm] = op["id"].(string)
	})
	return public(op)
}

// update writes a durable stop change. A store error is logged: the stop itself
// happened, only its record is behind.
func (o *stopOps) update(f func(st *State)) {
	if err := o.store.Update(func(st *State) error { f(st); return nil }); err != nil {
		log.Printf("stop operations: %v", err)
	}
}

func (o *stopOps) stage(f func(st *State)) {
	if err := o.store.Stage(func(st *State) error { f(st); return nil }); err != nil {
		log.Printf("stop operations: %v", err)
	}
}

func (o *stopOps) apply(f func(st *State)) {
	if err := o.store.Apply(func(st *State) error { f(st); return nil }); err != nil {
		log.Printf("stop operations: %v", err)
	}
}

func (o *stopOps) set(stopID string, f func(op map[string]any)) map[string]any {
	var out map[string]any
	if o.store.Snapshot().StopOps[stopID] == nil {
		return nil
	}
	o.update(func(st *State) {
		if op := st.StopOps[stopID]; op != nil {
			cp := cloneAnyMap(op)
			f(cp)
			st.StopOps[stopID] = cp
			out = public(cp)
		}
	})
	return out
}

func (o *stopOps) setAsync(stopID string, f func(op map[string]any)) map[string]any {
	var out map[string]any
	if o.store.Snapshot().StopOps[stopID] == nil {
		return nil
	}
	o.apply(func(st *State) {
		if op := st.StopOps[stopID]; op != nil {
			cp := cloneAnyMap(op)
			f(cp)
			st.StopOps[stopID] = cp
			out = public(cp)
		}
	})
	return out
}

func (o *stopOps) complete(vm string, op map[string]any) map[string]any {
	return o.set(op["id"].(string), func(op map[string]any) {
		now := time.Now().UTC()
		op["status"], op["lastAttemptAt"], op["endedAt"] = "completed", now, now
	})
}

func (o *stopOps) completeAsync(vm string, op map[string]any) map[string]any {
	return o.setAsync(op["id"].(string), func(op map[string]any) {
		now := time.Now().UTC()
		op["status"], op["lastAttemptAt"], op["endedAt"] = "completed", now, now
	})
}

func (o *stopOps) fail(vm string, op map[string]any, why string) {
	o.set(op["id"].(string), func(op map[string]any) {
		op["status"], op["lastAttemptAt"], op["error"] = "failing", time.Now().UTC(), strings.TrimSpace(preview(why))
	})
}

func (o *stopOps) failAsync(vm string, op map[string]any, why string) {
	o.setAsync(op["id"].(string), func(op map[string]any) {
		op["status"], op["lastAttemptAt"], op["error"] = "failing", time.Now().UTC(), strings.TrimSpace(preview(why))
	})
}

// supersede ends a prior stop operation when the VM is started or resumed.
func (o *stopOps) supersede(vm string) {
	st := o.store.Snapshot()
	stopID := st.StopLast[vm]
	op := st.StopOps[stopID]
	if op == nil || (op["status"] != "pending" && op["status"] != "failing" && op["status"] != "completed") {
		return
	}
	o.apply(func(st *State) {
		if op := st.StopOps[stopID]; op != nil {
			if s := op["status"]; s == "pending" || s == "failing" || s == "completed" {
				cp := cloneAnyMap(op)
				cp["status"], cp["endedAt"] = "superseded", time.Now().UTC()
				st.StopOps[stopID] = cp
			}
		}
	})
}

// last is the VM's last stop, or nil.
func (o *stopOps) last(vm string) any {
	st := o.store.Snapshot()
	if op := st.StopOps[st.StopLast[vm]]; op != nil {
		return public(op)
	}
	return nil
}

func (o *stopOps) get(team, vm, stopID string) map[string]any {
	op := o.store.Snapshot().StopOps[stopID]
	if op == nil || op["_team"] != team || op["_vm"] != vm {
		return nil
	}
	return public(op)
}

// forgetStops drops a deleted VM's stop operations (inside Store.Update).
func forgetStops(st *State, vm string) {
	delete(st.StopLast, vm)
	for id, op := range st.StopOps {
		if op["_vm"] == vm {
			delete(st.StopOps, id)
		}
	}
}

// public copies op without its private "_" keys.
func public(op map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range op {
		if !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}
