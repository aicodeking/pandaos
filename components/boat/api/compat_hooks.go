package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Deliveries to webhooks registered through /api/v1 use boat.dev's events,
// payload and headers (docs.boat.dev/webhooks): sandbox.ready when a VM
// becomes usable, sandbox.archived when it stops, sandbox.error when it
// fails; data.sandbox.id is its bx_ id; signed with X-Ascii-Signature
// v1=hex(HMAC-SHA256(secret, delivery_id + "." + timestamp + "." + body)).
// hydrated, degraded and recovered have no source here and are never sent.

// boatDevEvent turns a native vm.state event into boat.dev's, or ok=false.
func (d *dispatcher) boatDevEvent(ctx context.Context, ev Event) (Event, bool) {
	var raw map[string]any
	if ev.Type != "vm.state" || json.Unmarshal(ev.Data, &raw) != nil {
		return Event{}, false
	}
	state, prev := str(raw, "state"), str(raw, "prev")
	typ := ""
	switch {
	case state == "running" && prev != "running":
		typ = "sandbox.ready"
	case state == "stopped" && prev != "stopped":
		typ = "sandbox.archived"
	case state == "error" && prev != "error":
		typ = "sandbox.error"
	default:
		return Event{}, false
	}
	vmID := str(raw, "vm_id")
	name := ""
	if vm, ok := raw["vm"].(map[string]any); ok {
		vmID, name = orDefault(vmID, str(vm, "id")), str(vm, "name")
	}
	if name == "" && vmID != "" {
		var vm VM
		if d.s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm) == nil {
			name = str(vm, "name")
		}
	}
	data := map[string]any{"sandbox": map[string]any{"id": d.s.compatID(ev.TeamID, vmID), "name": name},
		"previousState": compatState(prev, false), "state": compatState(state, false)}
	b, _ := json.Marshal(data)
	return Event{ID: "evt_" + randHex(16), Type: typ, Created: ev.Created, TeamID: ev.TeamID, Data: b}, true
}

// boatDevBody is the payload boat.dev sends.
func boatDevBody(ev Event) []byte {
	b, _ := json.Marshal(map[string]any{"id": ev.ID, "type": ev.Type, "createdAt": ev.Created.UTC().Format("2006-01-02T15:04:05.000Z"),
		"data": ev.Data})
	return b
}

// signBoatDev sets boat.dev's delivery headers.
func signBoatDev(req *http.Request, secret []byte, ev Event, body []byte, attempt int) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s.%s.", ev.ID, ts)
	m.Write(body)
	req.Header.Set("X-Ascii-Event", ev.Type)
	req.Header.Set("X-Ascii-Delivery", ev.ID)
	req.Header.Set("X-Ascii-Timestamp", ts)
	req.Header.Set("X-Ascii-Signature", "v1="+hex.EncodeToString(m.Sum(nil)))
	req.Header.Set("X-Ascii-Attempt", strconv.Itoa(attempt))
}
