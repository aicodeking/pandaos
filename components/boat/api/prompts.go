package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Async prompts run the agent CLI as a detached command. A conversation maps
// to the CLI's native session, so resuming keeps its memory.
//
//   claude    we choose the session id: --session-id U, then --resume U.
//   codex     the CLI chooses it; read from its first --json event (thread_id).
//   opencode  the CLI chooses it; read from its --format json events (sessionID).
//
// Steer is interrupt + resume: these CLIs have no way to take input during a
// headless turn. The running turn gets SIGINT, then a new turn in the same
// session carries the new message. Files and session memory stay; a tool call
// that was in flight is cut off, unlike boat.dev's in-turn steer.

const (
	maxPromptsPerVM = 200
	previewLen      = 200
)

var reConvID = regexp.MustCompile(`^conv_[0-9a-f]{16}$`)

func preview(s string) string {
	if len(s) > previewLen {
		return strings.ToValidUTF8(s[:previewLen], "")
	}
	return s
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// agentArgv builds a detached turn. first says the session does not exist yet.
func agentArgv(agent, prompt, model, session string, first bool) []string {
	var c []string
	switch agent {
	case "claude":
		c = []string{"claude", "-p", "--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose"}
		if first {
			c = append(c, "--session-id", session)
		} else {
			c = append(c, "--resume", session)
		}
		if model != "" {
			c = append(c, "--model", model)
		}
	case "codex":
		c = []string{"codex", "exec"}
		if session != "" {
			c = append(c, "resume", session)
		}
		c = append(c, "--json", "--full-auto", "--skip-git-repo-check")
		if model != "" {
			c = append(c, "-m", model)
		}
	default: // opencode
		c = []string{"opencode", "run", "--format", "json"}
		if session != "" {
			c = append(c, "--session", session)
		}
		if model != "" {
			c = append(c, "-m", model)
		}
	}
	return append(c, "--", prompt)
}

var reNativeSession = []*regexp.Regexp{
	regexp.MustCompile(`"thread_id"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
	regexp.MustCompile(`"sessionID"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
	regexp.MustCompile(`"session_id"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
}

// nativeSession finds the session id in a turn's first log output.
func nativeSession(log string) string {
	for _, re := range reNativeSession {
		if m := re.FindStringSubmatch(log); m != nil {
			return m[1]
		}
	}
	return ""
}

func findPrompt(st *State, vmID, id string) *PromptRun {
	for _, p := range st.Prompts[vmID] {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func findConv(st *State, vmID, id string) *Conversation {
	for _, c := range st.Conversations[vmID] {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func latestConv(st *State, vmID string) *Conversation {
	var best *Conversation
	for _, c := range st.Conversations[vmID] {
		if best == nil || c.Updated.After(best.Updated) {
			best = c
		}
	}
	return best
}

func runningTurn(st *State, vmID, conv string) *PromptRun {
	for _, p := range st.Prompts[vmID] {
		if p.ConversationID == conv && p.State == "running" {
			return p
		}
	}
	return nil
}

type promptReq struct {
	Prompt         string `json:"prompt"`
	Agent          string `json:"agent"`
	Model          string `json:"model"`
	Cwd            string `json:"cwd"`
	ConversationID string `json:"conversation_id"`
	New            bool   `json:"new"`
}

// startPrompt is POST /prompts: continue the latest conversation, a named one,
// or (new, or no conversation yet) a fresh one.
func (s *server) startPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	var body promptReq
	if !decode(w, r, &body) {
		return
	}
	msg := ""
	switch {
	case strings.TrimSpace(body.Prompt) == "":
		msg = "prompt is required"
	case body.Agent != "" && agents[body.Agent] == nil:
		msg = "agent must be claude, codex or opencode"
	case body.Model != "" && !reModel.MatchString(body.Model):
		msg = "invalid model"
	case !validCwd(body.Cwd):
		msg = "cwd must be absolute"
	case body.ConversationID != "" && !reConvID.MatchString(body.ConversationID):
		msg = "invalid conversation_id"
	case body.ConversationID != "" && body.New:
		msg = "conversation_id and new conflict"
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	run, code, msg := s.runTurn(r, p, r.PathValue("id"), body, "")
	if run == nil {
		writeErr(w, r, code, "invalid_request", msg)
		return
	}
	writeJSON(w, 202, run)
}

// runTurn starts one turn. On failure it returns nil, an HTTP status and a message.
func (s *server) runTurn(r *http.Request, p *principal, vmID string, body promptReq, steers string) (*PromptRun, int, string) {
	s.refreshPrompts(r, vmID)
	st := s.store.Snapshot()
	var conv *Conversation
	switch {
	case body.ConversationID != "":
		if conv = findConv(st, vmID, body.ConversationID); conv == nil {
			return nil, 404, "conversation not found"
		}
	case !body.New:
		conv = latestConv(st, vmID)
		if conv != nil && body.Agent != "" && body.Agent != conv.Agent {
			conv = nil // another agent cannot resume this session
		}
	}
	first := conv == nil
	agent, model := body.Agent, body.Model
	session := ""
	convID := "conv_" + randHex(8)
	if conv != nil {
		if body.Agent != "" && body.Agent != conv.Agent {
			return nil, 409, "conversation belongs to agent " + conv.Agent
		}
		if run := runningTurn(st, vmID, conv.ID); run != nil {
			return nil, 409, "a turn is running in this conversation (" + run.ID + "); steer or interrupt it, or use new"
		}
		agent, session, convID = conv.Agent, conv.Session, conv.ID
		if model == "" {
			model = conv.Model
		}
		if session == "" && agent != "claude" {
			first = true // the first turn ended before it reported a session
		}
	}
	if agent == "" {
		agent = "claude"
	}
	if agent == "claude" && session == "" {
		session, first = newUUID(), true
	}
	env, err := s.teamEnv(p.Team.ID, s.vmSecrets(vmID))
	if err != nil {
		log.Printf("%s: %v", info(r).id, err)
		return nil, 500, "cannot decrypt team secrets"
	}
	ai, err := s.aiEnv(vmID, p.Team.ID, false)
	if err != nil {
		log.Printf("%s: %v", info(r).id, err)
		return nil, 500, "cannot prepare AI credentials"
	}
	for k, v := range ai {
		env[k] = v
	}
	argv := agentArgv(agent, body.Prompt, model, session, first)
	cid, err := s.startDetached(r, p, vmID, "prompt", argv, env, body.Cwd)
	if err != nil {
		log.Printf("%s: start prompt: %v", info(r).id, err)
		if be, ok := err.(*boatdError); ok && be.Status/100 == 4 {
			return nil, be.Status, be.message()
		}
		return nil, 502, "cannot start the agent in the VM"
	}
	now := time.Now().UTC()
	run := &PromptRun{ID: "prm_" + randHex(8), VMID: vmID, ConversationID: convID, Agent: agent, Model: model,
		Prompt: preview(body.Prompt), CmdID: cid, State: "running", Created: now}
	err = s.store.Update(func(st *State) error {
		c := findConv(st, vmID, convID)
		if c == nil {
			c = &Conversation{ID: convID, VMID: vmID, Agent: agent, Created: now}
			st.Conversations[vmID] = append(st.Conversations[vmID], c)
		}
		c.Model, c.Session, c.Updated, c.LastPrompt = model, session, now, run.Prompt
		c.PromptCount++
		if steers != "" {
			if old := findPrompt(st, vmID, steers); old != nil {
				old.State, old.SteeredBy = "steered", run.ID
			}
		}
		list := append(st.Prompts[vmID], run)
		if len(list) > maxPromptsPerVM {
			list = list[len(list)-maxPromptsPerVM:]
		}
		st.Prompts[vmID] = list
		return nil
	})
	if err != nil {
		log.Printf("%s: record prompt: %v", info(r).id, err)
		return nil, 500, "could not record the prompt"
	}
	return run, 202, ""
}

// refreshPrompts updates running turns from the guest and learns native sessions.
func (s *server) refreshPrompts(r *http.Request, vmID string) {
	st := s.store.Snapshot()
	type upd struct {
		state   string
		exit    *int
		session string
		conv    string
	}
	updates := map[string]upd{}
	for _, run := range st.Prompts[vmID] {
		if run.State != "running" {
			continue
		}
		c := findConv(st, vmID, run.ConversationID)
		if c != nil && c.Session == "" {
			if out, err := s.readLogs(r, vmID, run.CmdID, 0, 64<<10); err == nil {
				if data, derr := base64.StdEncoding.DecodeString(str(out, "data_b64")); derr == nil {
					if id := nativeSession(string(data)); id != "" {
						updates[run.ID] = upd{state: "running", session: id, conv: c.ID}
					}
				}
			}
		}
		g, err := s.agentOp(r, vmID, "cmd_status", map[string]any{"cmd_id": run.CmdID})
		if err != nil {
			continue // VM stopped or agent away: keep the last known state
		}
		if g["running"] == true {
			continue
		}
		u := updates[run.ID]
		u.state, u.conv = "completed", run.ConversationID
		if f, ok := g["exit"].(float64); ok {
			n := int(f)
			u.exit = &n
			if n != 0 {
				u.state = "failed"
			}
		}
		if sig, _ := g["signal"].(string); sig != "" {
			u.state = "interrupted"
		}
		updates[run.ID] = u
	}
	if len(updates) == 0 {
		return
	}
	err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		for id, u := range updates {
			run := findPrompt(st, vmID, id)
			if run == nil {
				continue
			}
			if run.State == "running" {
				run.State, run.Exit = u.state, u.exit
				if u.state != "running" {
					run.Finished = &now
				}
			}
			if c := findConv(st, vmID, u.conv); c != nil && u.session != "" && c.Session == "" {
				c.Session = u.session
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("refresh prompts: %v", err)
	}
}

func (s *server) listPrompts(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	s.refreshPrompts(r, id)
	list := s.store.Snapshot().Prompts[id]
	if list == nil {
		list = []*PromptRun{}
	}
	writeJSON(w, 200, map[string]any{"prompts": list})
}

// pathPrompt checks VM ownership and finds the prompt in the path.
func (s *server) pathPrompt(w http.ResponseWriter, r *http.Request, p *principal) (*PromptRun, bool) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return nil, false
	}
	id := r.PathValue("id")
	s.refreshPrompts(r, id)
	run := findPrompt(s.store.Snapshot(), id, r.PathValue("pid"))
	if run == nil {
		writeErr(w, r, 404, "not_found", "prompt not found")
		return nil, false
	}
	return run, true
}

func (s *server) getPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	if run, ok := s.pathPrompt(w, r, p); ok {
		writeJSON(w, 200, run)
	}
}

func (s *server) promptLogs(w http.ResponseWriter, r *http.Request, p *principal) {
	run, ok := s.pathPrompt(w, r, p)
	if !ok {
		return
	}
	r.SetPathValue("cid", run.CmdID)
	s.commandLogs(w, r, p)
}

// interruptPrompt sends SIGINT. The session keeps its memory; changed files stay.
func (s *server) interruptPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	run, ok := s.pathPrompt(w, r, p)
	if !ok {
		return
	}
	if run.State != "running" {
		writeErr(w, r, 409, "not_running", "this turn is "+run.State)
		return
	}
	if _, err := s.agentOp(r, r.PathValue("id"), "cmd_signal", map[string]any{"cmd_id": run.CmdID, "signal": "INT"}); err != nil {
		writeUpstream(w, r, err)
		return
	}
	id := r.PathValue("id")
	var out PromptRun
	err := s.store.Update(func(st *State) error {
		cur := findPrompt(st, id, run.ID)
		if cur == nil {
			return errConflict
		}
		now := time.Now().UTC()
		cur.State, cur.Finished = "interrupted", &now
		out = *cur
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

// steerPrompt interrupts the running turn and resumes its session with the new
// message (see the top of this file for how this differs from in-turn steering).
func (s *server) steerPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeErr(w, r, 400, "invalid_request", "prompt is required")
		return
	}
	run, ok := s.pathPrompt(w, r, p)
	if !ok {
		return
	}
	if run.State != "running" {
		writeErr(w, r, 409, "not_running", "this turn is "+run.State+"; send a new prompt instead")
		return
	}
	vmID := r.PathValue("id")
	if run.Agent != "claude" {
		// These learn their session from the turn's output; give it a moment.
		for i := 0; i < 10 && findConv(s.store.Snapshot(), vmID, run.ConversationID).Session == ""; i++ {
			s.refreshPrompts(r, vmID)
			time.Sleep(300 * time.Millisecond)
		}
		if findConv(s.store.Snapshot(), vmID, run.ConversationID).Session == "" {
			writeErr(w, r, 409, "session_unknown", "the agent has not reported a session yet; try again")
			return
		}
	}
	if _, err := s.agentOp(r, vmID, "cmd_signal", map[string]any{"cmd_id": run.CmdID, "signal": "INT"}); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if err := s.waitCommandEnd(r, vmID, run.CmdID, 10*time.Second); err != nil {
		writeErr(w, r, 409, "still_running", err.Error())
		return
	}
	err := s.store.Update(func(st *State) error {
		if cur := findPrompt(st, vmID, run.ID); cur != nil {
			now := time.Now().UTC()
			cur.State, cur.Finished = "interrupted", &now
		}
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	next, code, msg := s.runTurn(r, p, vmID, promptReq{Prompt: body.Prompt, ConversationID: run.ConversationID}, run.ID)
	if next == nil {
		writeErr(w, r, code, "invalid_request", msg)
		return
	}
	writeJSON(w, 202, next)
}

// waitCommandEnd escalates to SIGKILL after half the wait.
func (s *server) waitCommandEnd(r *http.Request, vmID, cid string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	killed := false
	for time.Now().Before(deadline) {
		g, err := s.agentOp(r, vmID, "cmd_status", map[string]any{"cmd_id": cid})
		if err != nil {
			return err
		}
		if g["running"] != true {
			return nil
		}
		if !killed && time.Until(deadline) < wait/2 {
			killed = true
			s.agentOp(r, vmID, "cmd_signal", map[string]any{"cmd_id": cid, "signal": "KILL"})
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the running turn did not stop within %s", wait)
}

func (s *server) listConversations(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	s.refreshPrompts(r, id)
	st := s.store.Snapshot()
	out := []map[string]any{}
	for i := len(st.Conversations[id]) - 1; i >= 0; i-- {
		c := st.Conversations[id][i]
		out = append(out, map[string]any{"id": c.ID, "vm_id": c.VMID, "agent": c.Agent, "model": c.Model, "prompt_count": c.PromptCount,
			"last_prompt": c.LastPrompt, "running": runningTurn(st, id, c.ID) != nil, "created": c.Created, "updated": c.Updated})
	}
	writeJSON(w, 200, map[string]any{"conversations": out})
}
