package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// boat.dev's prompts, events, conversations, steer and interrupt on the
// native prompt runs (prompts.go). boat.dev conversation ids are UUIDs; a
// native conv_<16 hex> is shown as <8>-<4>-<4>-8000-000000000000 and read back.

var reConvUUID = regexp.MustCompile(`^([0-9a-f]{8})-([0-9a-f]{4})-([0-9a-f]{4})-8000-000000000000$`)

func convUUID(conv string) any {
	h, ok := strings.CutPrefix(conv, "conv_")
	if !ok || len(h) != 16 {
		return nil
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:] + "-8000-000000000000"
}

func convFromUUID(u string) (string, bool) {
	m := reConvUUID.FindStringSubmatch(strings.ToLower(u))
	if m == nil {
		return "", false
	}
	return "conv_" + m[1] + m[2] + m[3], true
}

// compatPromptStatus maps a native turn state onto boat.dev's PromptRun status.
func compatPromptStatus(st string) string {
	switch st {
	case "running":
		return "running"
	case "completed":
		return "finished"
	case "failed":
		return "failed"
	}
	return "interrupted" // interrupted, steered
}

func (s *server) promptRun(bx string, run *PromptRun) map[string]any {
	st := compatPromptStatus(run.State)
	return map[string]any{"id": run.ID, "promptId": run.ID, "sandboxId": bx, "status": st, "done": st != "running",
		"createdAt": run.Created, "model": nullIfEmpty(run.Model), "conversationId": convUUID(run.ConversationID)}
}

func (s *server) compatPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Provider        string `json:"provider"`
		Prompt          string `json:"prompt"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"` // accepted; the harness picks its own
		Fast            *bool  `json:"fast"`            // accepted; no fast mode here
		New             bool   `json:"new"`
		ConversationID  string `json:"conversationId"`
	}
	if !decode(w, r, &body) {
		return
	}
	in := map[string]any{"prompt": body.Prompt, "agent": compatProvider(body.Provider), "model": body.Model, "new": body.New}
	if body.ConversationID != "" {
		conv, ok := convFromUUID(body.ConversationID)
		if !ok {
			writeErr(w, r, 404, "conversation_not_found", "no conversation "+body.ConversationID)
			return
		}
		in["conversation_id"] = conv
	}
	res := s.callNative(r, p, "POST /v1/vms/{id}/prompts", s.startPrompt, r.PathValue("id"), in)
	if !res.ok() {
		res.relay(w)
		return
	}
	var run PromptRun
	json.Unmarshal(res.body.Bytes(), &run)
	bx := s.compatID(p.Team.ID, r.PathValue("id"))
	writeCompat(w, http.StatusAccepted, "prompt.queued", map[string]any{"id": bx, "promptId": run.ID,
		"promptRun": s.promptRun(bx, &run), "status": "queued", "provider": orDefault(body.Provider, run.Agent),
		"conversationId": convUUID(run.ConversationID), "model": nullIfEmpty(run.Model)})
}

// compatProvider maps boat.dev's provider names onto the native agents.
func compatProvider(p string) string {
	switch p {
	case "claude-code":
		return "claude"
	case "prime-agent":
		return "prime"
	case "kimi-code":
		return "kimi"
	}
	return p
}

func (s *server) compatPromptRun(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "GET /v1/vms/{id}/prompts/{pid}", s.getPrompt, r.PathValue("id"), nil, "pid", r.PathValue("pid"))
	if !res.ok() {
		res.relay(w)
		return
	}
	var run PromptRun
	json.Unmarshal(res.body.Bytes(), &run)
	bx := s.compatID(p.Team.ID, r.PathValue("id"))
	writeCompat(w, 200, "prompt.run", map[string]any{"id": bx, "promptRun": s.promptRun(bx, &run)})
}

func (s *server) compatConversations(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	s.refreshPrompts(r, id)
	st := s.store.Snapshot()
	convs := append([]*Conversation(nil), st.Conversations[id]...)
	sort.SliceStable(convs, func(i, j int) bool { return convs[i].Updated.After(convs[j].Updated) })
	out := []map[string]any{}
	for i, c := range convs {
		out = append(out, map[string]any{"id": convUUID(c.ID), "createdAt": c.Created, "lastPromptAt": c.Updated,
			"prompts": c.PromptCount, "running": runningTurn(st, id, c.ID) != nil, "lastHarness": nullIfEmpty(c.Agent),
			"lastModel": nullIfEmpty(c.Model), "lastPromptPreview": nullIfEmpty(c.LastPrompt), "current": i == 0})
	}
	writeCompat(w, 200, "conversation.list", map[string]any{"id": s.compatID(p.Team.ID, id), "conversations": out})
}

// activeTurn is the running turn of the named conversation (a UUID), or of the
// most recently updated one.
func (s *server) activeTurn(w http.ResponseWriter, r *http.Request, vmID, convUUIDArg string) (*PromptRun, bool) {
	s.refreshPrompts(r, vmID)
	st := s.store.Snapshot()
	conv := ""
	if convUUIDArg != "" {
		c, ok := convFromUUID(convUUIDArg)
		if !ok || findConv(st, vmID, c) == nil {
			writeErr(w, r, 404, "conversation_not_found", "no conversation "+convUUIDArg)
			return nil, false
		}
		conv = c
	} else {
		var last *Conversation
		for _, c := range st.Conversations[vmID] {
			if last == nil || c.Updated.After(last.Updated) {
				last = c
			}
		}
		if last != nil {
			conv = last.ID
		}
	}
	run := runningTurn(st, vmID, conv)
	if run == nil {
		writeErr(w, r, 409, "not_running", "no turn is running in this conversation; send a prompt instead")
		return nil, false
	}
	return run, true
}

func (s *server) compatSteer(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Message      string  `json:"message"`
		Conversation *string `json:"conversation"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeErr(w, r, 400, "invalid_request", "message is required")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	conv := ""
	if body.Conversation != nil {
		conv = *body.Conversation
	}
	run, ok := s.activeTurn(w, r, r.PathValue("id"), conv)
	if !ok {
		return
	}
	res := s.callNative(r, p, "POST /v1/vms/{id}/prompts/{pid}/steer", s.steerPrompt, r.PathValue("id"),
		map[string]any{"prompt": body.Message}, "pid", run.ID)
	if !res.ok() {
		res.relay(w)
		return
	}
	var next PromptRun
	json.Unmarshal(res.body.Bytes(), &next)
	writeCompat(w, 200, "prompt.steered", map[string]any{"id": s.compatID(p.Team.ID, r.PathValue("id")),
		"conversationId": convUUID(run.ConversationID), "promptId": next.ID, "native": false, "mode": "fallback", "status": "steered"})
}

func (s *server) compatInterrupt(w http.ResponseWriter, r *http.Request, p *principal) {
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	run, ok := s.activeTurn(w, r, r.PathValue("id"), r.URL.Query().Get("conversation"))
	if !ok {
		return
	}
	res := s.callNative(r, p, "POST /v1/vms/{id}/prompts/{pid}/interrupt", s.interruptPrompt, r.PathValue("id"), nil, "pid", run.ID)
	if !res.ok() {
		res.relay(w)
		return
	}
	sb := s.sandbox(publicVM(vm))
	writeCompat(w, 200, "sandbox.interrupted", map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
		"conversationId": convUUID(run.ConversationID)})
}

// ---- events ----

// compatEvents lists the sandbox's work as boat.dev events: a "prompt" event
// per turn and a "response" event when it ends, with the agent's output
// (up to compatEventText bytes) while the VM runs. Cursor: base64url of
// "<timestamp>:<id>" of the last event read, as the official SDKs build it.
func (s *server) compatEvents(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, r, 400, "invalid_query", "limit must be 1-200")
			return
		}
		limit = n
	}
	desc := q.Get("sort") == "desc"
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	id := r.PathValue("id")
	s.refreshPrompts(r, id)
	types := splitList(q["type"])
	convs := map[string]bool{}
	for _, c := range splitList(q["conversation"]) {
		if conv, ok := convFromUUID(c); ok {
			convs[conv] = true
		} else {
			convs["\x00"] = true // matches nothing
		}
	}
	var evs []map[string]any
	for _, run := range s.store.Snapshot().Prompts[id] {
		if len(convs) > 0 && !convs[run.ConversationID] {
			continue
		}
		base := func(suffix, typ string, at time.Time, data map[string]any) map[string]any {
			return map[string]any{"id": run.ID + suffix, "type": typ, "timestamp": at.UnixMilli(), "taskId": run.ID,
				"conversationId": convUUID(run.ConversationID), "data": data}
		}
		evs = append(evs, base(".prompt", "prompt", run.Created, map[string]any{"prompt": run.Prompt, "provider": run.Agent,
			"model": nullIfEmpty(run.Model), "status": compatPromptStatus(run.State)}))
		if run.Finished != nil {
			evs = append(evs, base(".response", "response", *run.Finished, map[string]any{"status": compatPromptStatus(run.State),
				"is_streaming": false, "_cmd": run.CmdID}))
		}
	}
	if len(types) > 0 {
		kept := evs[:0]
		for _, e := range evs {
			if containsStr(types, e["type"].(string)) {
				kept = append(kept, e)
			}
		}
		evs = kept
	}
	key := func(e map[string]any) string {
		return strconv.FormatInt(e["timestamp"].(int64), 10) + ":" + e["id"].(string)
	}
	less := func(a, b map[string]any) bool {
		ta, tb := a["timestamp"].(int64), b["timestamp"].(int64)
		if ta != tb {
			return ta < tb
		}
		return a["id"].(string) < b["id"].(string)
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if desc {
			return less(evs[j], evs[i])
		}
		return less(evs[i], evs[j])
	})
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(c, "="))
		if err != nil {
			writeErr(w, r, 400, "invalid_query", "bad cursor")
			return
		}
		after := string(raw)
		start := len(evs)
		for i, e := range evs {
			if key(e) == after {
				start = i + 1
				break
			}
			if cmpEventKey(key(e), after, desc) {
				start = i
				break
			}
		}
		evs = evs[start:]
	}
	more := len(evs) > limit
	if more {
		evs = evs[:limit]
	}
	running := str(vm, "state") == "running"
	for _, e := range evs {
		d := e["data"].(map[string]any)
		if cid, ok := d["_cmd"].(string); ok {
			delete(d, "_cmd")
			if running {
				if out, err := s.readLogs(r, id, cid, 0, compatEventText); err == nil {
					b, _ := base64.StdEncoding.DecodeString(str(out, "data_b64"))
					d["text"] = strings.ToValidUTF8(string(b), "�")
				}
			}
		}
	}
	var next any
	if more && len(evs) > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(key(evs[len(evs)-1])))
	}
	if evs == nil {
		evs = []map[string]any{}
	}
	writeCompat(w, 200, "events.list", map[string]any{"id": s.compatID(p.Team.ID, id), "events": evs,
		"pageInfo": map[string]any{"nextCursor": next, "hasMore": more, "limit": limit}})
}

const compatEventText = 64 << 10

// cmpEventKey: does event key k come after the cursor key in this order?
// Keys are "<ms>:<id>"; ms compare numerically.
func cmpEventKey(k, cursor string, desc bool) bool {
	kt, kid, _ := strings.Cut(k, ":")
	ct, cid, _ := strings.Cut(cursor, ":")
	a, _ := strconv.ParseInt(kt, 10, 64)
	b, _ := strconv.ParseInt(ct, 10, 64)
	after := a > b || (a == b && kid > cid)
	if desc {
		return !after && k != cursor
	}
	return after
}

// splitList reads a repeated and/or comma-separated query parameter.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
	}
	return out
}
