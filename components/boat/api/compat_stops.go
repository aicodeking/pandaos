package main

import (
	"strings"
	"sync"
	"time"
)

// stopOps are boat.dev's stop operations: the last stop asked of each VM
// (Sandbox.stop) and every stop by id (GET /sandboxes/{id}/stops/{stopId}).
// A stop through this server finishes before it answers, so an operation is
// completed, failing (the native stop failed) or superseded (a resume came).
//
// ponytail: in memory, so a restart forgets them (Sandbox.stop is then null).
type stopOps struct {
	mu     sync.Mutex
	byID   map[string]map[string]any // stop id -> op (with "_team", "_vm")
	lastOf map[string]string         // vm id -> its last stop id
}

const maxStopOps = 10000

func (o *stopOps) begin(team, vm string) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byID == nil || len(o.byID) >= maxStopOps {
		o.byID, o.lastOf = map[string]map[string]any{}, map[string]string{} // ponytail: forgets all at the cap
	}
	now := time.Now().UTC()
	op := map[string]any{"id": "stop_" + randHex(8), "status": "pending", "requestedAt": now, "lastAttemptAt": nil,
		"error": nil, "endedAt": nil, "_team": team, "_vm": vm}
	o.byID[op["id"].(string)] = op
	o.lastOf[vm] = op["id"].(string)
	return public(op)
}

func (o *stopOps) set(stopID string, f func(op map[string]any)) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	op := o.byID[stopID]
	if op == nil {
		return nil
	}
	f(op)
	return public(op)
}

func (o *stopOps) complete(vm string, op map[string]any) map[string]any {
	return o.set(op["id"].(string), func(op map[string]any) {
		now := time.Now().UTC()
		op["status"], op["lastAttemptAt"], op["endedAt"] = "completed", now, now
	})
}

func (o *stopOps) fail(vm string, op map[string]any, why string) {
	o.set(op["id"].(string), func(op map[string]any) {
		op["status"], op["lastAttemptAt"], op["error"] = "failing", time.Now().UTC(), strings.TrimSpace(preview(why))
	})
}

// supersede ends a pending or failing stop when the VM is resumed.
func (o *stopOps) supersede(vm string) {
	o.mu.Lock()
	id := o.lastOf[vm]
	o.mu.Unlock()
	o.set(id, func(op map[string]any) {
		if st := op["status"]; st == "pending" || st == "failing" {
			op["status"], op["endedAt"] = "superseded", time.Now().UTC()
		}
	})
}

// last is the VM's last stop, or nil.
func (o *stopOps) last(vm string) any {
	o.mu.Lock()
	id := o.lastOf[vm]
	o.mu.Unlock()
	if op := o.set(id, func(map[string]any) {}); op != nil {
		return op
	}
	return nil
}

func (o *stopOps) get(team, vm, stopID string) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	op := o.byID[stopID]
	if op == nil || op["_team"] != team || op["_vm"] != vm {
		return nil
	}
	return public(op)
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
