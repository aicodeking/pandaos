package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Async prompts run the agent CLI as a detached command. A conversation maps
// to the CLI's native session, so resuming keeps its memory.
//
//   claude    we choose the session id: --session-id U, then --resume U.
//   codex     the CLI chooses it; read from its first --json event (thread_id).
//   opencode  the CLI chooses it; read from its --format json events (sessionID).
//   pi        we choose it: --session-id U on every turn.
//   kimi      the CLI chooses it; read from its stream-json events, then -S.
//   mistral   (vibe) the CLI chooses it; read from its streaming events, then --resume.
//   prime     no session flag: every turn starts fresh, steer is refused.
//
// pi, prime, kimi and mistral were written from their --help and never run
// here: their output format is not known, so the session is looked for with
// the same loose patterns as the others and a turn without one still works.
//
// Steer is interrupt + resume: these CLIs have no way to take input during a
// headless turn. The running turn gets SIGINT, then a new turn in the same
// session carries the new message. Files and session memory stay; a tool call
// that was in flight is cut off, unlike boat.dev's in-turn steer.

const (
	maxPromptsPerVM = 200
	// maxConvsPerVM caps the conversations kept per VM, like prompts: past it the
	// least recently updated one is dropped.
	maxConvsPerVM = 200
	previewLen    = 200
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
	case "pi":
		c = withModel([]string{"pi", "-p", "--mode", "json", "--session-id", session}, "--model", model)
	case "prime":
		return append(withModel([]string{"prime-agent", "-p", "--mode", "json"}, "--model", model), positional(prompt))
	case "kimi":
		c = withModel([]string{"kimi", "-p", prompt, "--yolo", "--output-format", "stream-json"}, "-m", model)
		if session != "" {
			c = append(c, "-S", session)
		}
		return c
	case "mistral":
		c = withModel([]string{"vibe", "-p", prompt, "--auto-approve", "--output", "streaming"}, "--model", model)
		if session != "" {
			c = append(c, "--resume", session)
		}
		return c
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

// chosenSession: agents whose session id boat-api picks before the first turn.
func chosenSession(agent string) bool { return agent == "claude" || agent == "pi" }

// sessionless: agents with no way to resume a session.
func sessionless(agent string) bool { return agent == "prime" }

var reNativeSession = []*regexp.Regexp{
	regexp.MustCompile(`"thread_id"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
	regexp.MustCompile(`"sessionID"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
	regexp.MustCompile(`"session_id"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
	regexp.MustCompile(`"sessionId"\s*:\s*"([A-Za-z0-9_-]{1,128})"`),
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
	body.Agent = canonicalAgent(body.Agent)
	msg := ""
	switch {
	case strings.TrimSpace(body.Prompt) == "":
		msg = "prompt is required"
	case body.Agent != "" && agents[body.Agent] == nil:
		msg = "agent must be " + agentNames
	case body.Model != "" && !reModel.MatchString(body.Model):
		msg = "invalid model"
	case !validCwd(body.Cwd):
		msg = "invalid cwd"
	case body.ConversationID != "" && !reConvID.MatchString(body.ConversationID):
		msg = "invalid conversation_id"
	case body.ConversationID != "" && body.New:
		msg = "conversation_id and new conflict"
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok || s.refuseNotReady(w, r, vm) {
		return // like compat: no agent turn (it carries team secrets) before the environment is applied
	}
	run, code, msg := s.runTurn(r, p, r.PathValue("id"), body, "")
	if run == nil {
		writeTurnErr(w, r, code, msg)
		return
	}
	s.refreshTTL(r, vm) // an agent at work gets a fresh full TTL, as on boat.dev
	writeJSON(w, 202, run)
}

// msgNotReady is the 409 sandbox_not_ready message of a command whose secrets
// raced a start (see secretsRaced).
const msgNotReady = "the sandbox is still provisioning: its environment is being applied; retry shortly"

// promptExecHook runs just before a prompt's secret check ahead of the exec
// (tests); nil in production.
var promptExecHook func(s *server, vmID string)

// secretsRaced reports whether a command about to run with team secrets in env
// must be refused: a start of the VM took the not-ready gate, or the run was
// marked no_env, after the request passed refuseNotReady and chose its secrets.
func (s *server) secretsRaced(vmID string, teamEnv map[string]string) bool {
	if promptExecHook != nil {
		promptExecHook(s, vmID)
	}
	if len(teamEnv) == 0 {
		return false
	}
	return s.envPushPending(vmID) || s.noEnvRun(vmID)
}

// writeTurnErr answers a failed runTurn: a command that raced a start is the
// 409 sandbox_not_ready of refuseNotReady.
func writeTurnErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if code == http.StatusConflict && msg == msgNotReady {
		writeErr(w, r, code, "sandbox_not_ready", msg)
		return
	}
	writeErr(w, r, code, "invalid_request", msg)
}

// runTurn starts one turn. On failure it returns nil, an HTTP status and a message.
func (s *server) runTurn(r *http.Request, p *principal, vmID string, body promptReq, steers string) (*PromptRun, int, string) {
	s.refreshPromptsNow(r, vmID) // exact: a turn that just ended must not block this one
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
		if session == "" && !chosenSession(agent) {
			first = true // the first turn ended before it reported a session
		}
	}
	if agent == "" {
		agent = "claude"
	}
	if chosenSession(agent) && session == "" {
		session, first = newUUID(), true
	}
	sel := s.selectionFor(r, vmID) // empty-non-nil for a VM created or forked with no_env
	if s.noEnvRun(vmID) {
		sel = []string{} // resumed with no_env: this run holds no team secrets
	}
	env, err := s.teamEnv(p.Team.ID, sel)
	if err != nil {
		log.Printf("%s: %v", info(r).id, err)
		return nil, 500, "cannot decrypt team secrets"
	}
	// The gate and the run's mark may have changed since refuseNotReady and the
	// selection above: re-check right before the exec so secrets selected for a
	// run a no_env cold start is now marking never reach it.
	if s.secretsRaced(vmID, env) {
		return nil, http.StatusConflict, msgNotReady
	}
	unlock := startLocks.rlock(vmID)
	defer unlock()
	if s.envPushPending(vmID) || (len(env) > 0 && s.noEnvRun(vmID)) || !slices.Equal(sel, s.runSecretsOf(vmID, info(r).vm)) {
		return nil, http.StatusConflict, msgNotReady
	}
	if err := s.recordPossibleRunNames(p, "exec", vmID, envNames(env)); err != nil {
		return nil, http.StatusForbidden, "cannot authorize or record prompt credentials"
	}
	ai, err := s.aiEnvRun(vmID, p.Team.ID, false, s.noEnvRun(vmID))
	if err != nil {
		log.Printf("%s: %v", info(r).id, err)
		return nil, 500, "cannot prepare AI credentials"
	}
	for k, v := range ai {
		env[k] = v
	}
	argv := agentArgv(agent, body.Prompt, model, session, first)
	cid, _, err := s.startDetached(r, p, vmID, "prompt", argv, env, body.Cwd)
	if err != nil {
		log.Printf("%s: start prompt: %v", info(r).id, err)
		if be, ok := err.(*boatdError); ok && be.Status/100 == 4 {
			return nil, be.Status, strings.TrimPrefix(be.message(), guestPrefix)
		}
		return nil, 502, "cannot start the agent in the VM"
	}
	now := time.Now().UTC()
	run := &PromptRun{ID: "prm_" + randHex(8), VMID: vmID, ConversationID: convID, Agent: agent, Model: model,
		Prompt: preview(body.Prompt), CmdID: cid, State: "running", Created: now}
	// Lazy like recordCommand: the turn already runs on the guest, the record is
	// display bookkeeping, so readers see it at once and writes inside the
	// idemPersistDelay window share one disk write instead of the POST waiting
	// on a deep clone and two fsyncs under the store mutex. Crash tradeoff: a
	// crash inside that window loses the newest prompt row / conversation bump
	// (the agent still ran; its command is listed by id on the guest).
	err = s.store.ApplyLazy(func(st *State) error {
		c := findConv(st, vmID, convID)
		if c == nil {
			c = &Conversation{ID: convID, VMID: vmID, Agent: agent, Created: now}
			st.Conversations[vmID] = append(st.Conversations[vmID], c)
		}
		c.Model, c.Session, c.Updated, c.LastPrompt = model, session, now, run.Prompt
		st.Conversations[vmID] = capConversations(st.Conversations[vmID], maxConvsPerVM)
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
	}, idemPersistDelay)
	if err != nil {
		log.Printf("%s: record prompt: %v", info(r).id, err)
		return nil, 500, "could not record the prompt"
	}
	promptRefreshes.forget(s, vmID) // the next read looks at the new turn at once
	return run, 202, ""
}

// refreshPrompts updates running turns from the guest and learns native sessions.
// Each running turn costs guest round trips, so concurrent callers share one
// refresh (singleflight per VM) but a caller that arrives alone always sees the
// guest's current state. Compat reads use this: sandbox state follows it at once.
func (s *server) refreshPrompts(r *http.Request, vmID string) {
	promptRefreshes.run(s, r, vmID, refreshJoin, s.refreshPromptsOnce)
}

// refreshPromptsCached is refreshPrompts for display-only reads (native prompt
// and conversation GETs): a refresh finished within promptRefreshTTL is reused.
func (s *server) refreshPromptsCached(r *http.Request, vmID string) {
	promptRefreshes.run(s, r, vmID, refreshTTL, s.refreshPromptsOnce)
}

// refreshPromptsNow is for callers that decide on the state (start a turn, learn
// a session): it waits for a refresh already in flight, which may predate the
// need, then does its own.
func (s *server) refreshPromptsNow(r *http.Request, vmID string) {
	promptRefreshes.run(s, r, vmID, refreshForce, s.refreshPromptsOnce)
}

// promptUpd is what one running turn's guest check learned.
type promptUpd struct {
	state   string
	exit    *int
	session string
	conv    string
}

// refreshParallel bounds the guest round trips one refresh makes at once.
const refreshParallel = listRefreshParallel

// refreshPromptsOnce checks the VM's running turns on the guest, at most
// refreshParallel at a time; the results are applied in the turns' order.
func (s *server) refreshPromptsOnce(r *http.Request, vmID string) {
	st := s.store.Snapshot()
	var running []*PromptRun
	for _, run := range st.Prompts[vmID] {
		if run.State == "running" {
			running = append(running, run)
		}
	}
	results := make([]*promptUpd, len(running)) // index = turn order: deterministic whatever finishes first
	var wg sync.WaitGroup
	sem := make(chan struct{}, refreshParallel)
	for i, run := range running {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.refreshOneTurn(r, st, vmID, run)
		}()
	}
	wg.Wait()
	updates := map[string]promptUpd{}
	for i, u := range results {
		if u != nil {
			updates[running[i].ID] = *u
		}
	}
	if len(updates) == 0 {
		return
	}
	// Lazy: the state is re-derived from the guest on the next read, so a crash
	// inside the window only repeats this refresh.
	err := s.store.ApplyLazy(func(st *State) error {
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
	}, idemPersistDelay)
	if err != nil {
		log.Printf("refresh prompts: %v", err)
	}
}

// refreshOneTurn is one running turn's guest check (nil = nothing to record: the
// turn still runs and its session is known, or the guest did not answer).
func (s *server) refreshOneTurn(r *http.Request, st *State, vmID string, run *PromptRun) *promptUpd {
	var u promptUpd
	learned := false
	if c := findConv(st, vmID, run.ConversationID); c != nil && c.Session == "" {
		if out, err := s.readLogs(r, vmID, run.CmdID, 0, 64<<10); err == nil {
			if data, derr := base64.StdEncoding.DecodeString(str(out, "data_b64")); derr == nil {
				if id := nativeSession(string(data)); id != "" {
					u = promptUpd{state: "running", session: id, conv: c.ID}
					learned = true
				}
			}
		}
	}
	g, err := s.agentOp(r, vmID, "cmd_status", map[string]any{"cmd_id": run.CmdID})
	if err != nil || g["running"] == true {
		// VM stopped or agent away: keep the last known state
		if learned {
			return &u
		}
		return nil
	}
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
	return &u
}

func (s *server) listPrompts(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	s.refreshPromptsCached(r, id)
	list := s.store.Snapshot().Prompts[id]
	if list == nil {
		list = []*PromptRun{}
	}
	writeJSON(w, 200, map[string]any{"prompts": list})
}

// pathPrompt checks VM ownership and finds the prompt in the path.
// exact: the caller acts on the turn's state (interrupt, steer) instead of showing it.
func (s *server) pathPrompt(w http.ResponseWriter, r *http.Request, p *principal, exact bool) (*PromptRun, bool) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return nil, false
	}
	id := r.PathValue("id")
	if exact {
		s.refreshPromptsNow(r, id)
	} else {
		s.refreshPromptsCached(r, id)
	}
	run := findPrompt(s.store.Snapshot(), id, r.PathValue("pid"))
	if run == nil {
		writeErr(w, r, 404, "not_found", "prompt not found")
		return nil, false
	}
	return run, true
}

func (s *server) getPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	if run, ok := s.pathPrompt(w, r, p, false); ok {
		writeJSON(w, 200, run)
	}
}

func (s *server) promptLogs(w http.ResponseWriter, r *http.Request, p *principal) {
	run, ok := s.pathPrompt(w, r, p, false)
	if !ok {
		return
	}
	r.SetPathValue("cid", run.CmdID)
	s.commandLogs(w, r, p)
}

// interruptPrompt sends SIGINT. The session keeps its memory; changed files stay.
func (s *server) interruptPrompt(w http.ResponseWriter, r *http.Request, p *principal) {
	run, ok := s.pathPrompt(w, r, p, true)
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
	// Lazy like refreshPrompts: the turn is already signalled, and a crash inside
	// the window only leaves the row to be re-derived from the guest on a read.
	err := s.store.ApplyLazy(func(st *State) error {
		cur := findPrompt(st, id, run.ID)
		if cur == nil {
			return errConflict
		}
		now := time.Now().UTC()
		cur.State, cur.Finished = "interrupted", &now
		out = *cur
		return nil
	}, idemPersistDelay)
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
	run, ok := s.pathPrompt(w, r, p, true)
	if !ok {
		return
	}
	if run.State != "running" {
		writeErr(w, r, 409, "not_running", "this turn is "+run.State+"; send a new prompt instead")
		return
	}
	if vm, ok := s.ownVM(w, r, p); !ok || s.refuseNotReady(w, r, vm) {
		return // the resumed turn would carry team secrets: refuse before interrupting the old one
	}
	vmID := r.PathValue("id")
	if sessionless(run.Agent) {
		writeErr(w, r, 409, "session_unknown", run.Agent+" cannot resume a session, so it cannot be steered; interrupt it and send a new prompt")
		return
	}
	if !chosenSession(run.Agent) {
		// These learn their session from the turn's output; give it a moment.
		bo := newBackoff(pollMin, pollMax, sessionLearnWait)
		for findConv(s.store.Snapshot(), vmID, run.ConversationID).Session == "" {
			s.refreshPrompts(r, vmID)
			if !bo.sleep(r.Context()) {
				break
			}
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
	err := s.store.ApplyLazy(func(st *State) error {
		if cur := findPrompt(st, vmID, run.ID); cur != nil {
			now := time.Now().UTC()
			cur.State, cur.Finished = "interrupted", &now
		}
		return nil
	}, idemPersistDelay) // lazy: the next turn's record write persists it too
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	next, code, msg := s.runTurn(r, p, vmID, promptReq{Prompt: body.Prompt, ConversationID: run.ConversationID}, run.ID)
	if next == nil {
		writeTurnErr(w, r, code, msg)
		return
	}
	writeJSON(w, 202, next)
}

// waitCommandEnd escalates to SIGKILL after half the wait.
func (s *server) waitCommandEnd(r *http.Request, vmID, cid string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	bo := newBackoff(pollMin, pollMax, wait)
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
		if !bo.sleep(r.Context()) {
			break
		}
	}
	return fmt.Errorf("the running turn did not stop within %s", wait)
}

func (s *server) listConversations(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	s.refreshPromptsCached(r, id)
	st := s.store.Snapshot()
	out := []map[string]any{}
	for i := len(st.Conversations[id]) - 1; i >= 0; i-- {
		c := st.Conversations[id][i]
		out = append(out, map[string]any{"id": c.ID, "vm_id": c.VMID, "agent": c.Agent, "model": c.Model, "prompt_count": c.PromptCount,
			"last_prompt": c.LastPrompt, "running": runningTurn(st, id, c.ID) != nil, "created": c.Created, "updated": c.Updated})
	}
	writeJSON(w, 200, map[string]any{"conversations": out})
}

// no_env is sticky (boat.dev): a sandbox started, created or forked with no_env
// never holds team secrets again. The mark is persisted in State.NoEnvRuns (it
// survives a boat-api restart and a stop, a plain start, a type change and a
// recover) and ends only with the VM (forgetVM) or when the start that set it
// failed and was rolled back (restoreRunMark). Its time is when the latest cold
// start marked it: a no_env self-token reaches only commands that began after it.
// markNoEnvRun(vmID, false) never clears a mark; it only resets the token's
// no_env generation of a VM that is not no_env.
func (s *server) markNoEnvRun(vmID string, on bool) error {
	_, was := s.store.Snapshot().NoEnvRuns[vmID]
	if on {
		return s.markNoEnvRunAt(vmID, time.Now().UTC())
	}
	if was || !s.tokenNoEnvGen(vmID) {
		return nil
	}
	return s.store.Update(func(st *State) error {
		if _, sticky := st.NoEnvRuns[vmID]; sticky {
			return nil
		}
		if t := st.cowVMToken(vmID); t != nil {
			t.NoEnvGen = 0 // a normal run: its token reads what any key of the team reads
		}
		return nil
	})
}

// markTokenNoEnv binds the VM's current token generation to a no_env run (see
// VMToken.NoEnvGen).
func markTokenNoEnv(st *State, vmID string) {
	if t := st.VMTokens[vmID]; t != nil && !t.Retired {
		st.cowVMToken(vmID).NoEnvGen = t.Gen
	}
}

// tokenNoEnvGen: whether the VM's current self-token generation was minted for a
// no_env run.
func (s *server) tokenNoEnvGen(vmID string) bool {
	t := s.store.Snapshot().VMTokens[vmID]
	return t != nil && t.NoEnvGen != 0 && t.NoEnvGen == t.Gen
}

// markNoEnvRunAt marks vmID no_env (sticky) as of at.
//
// This helper waits for disk only for a previously unmarked VM. Refreshes are
// asynchronous. Create and fork set their marks separately in a batched write;
// their sticky no_env policy is durable in boatd's labels before they return.
func (s *server) markNoEnvRunAt(vmID string, at time.Time) error {
	return s.store.UpdateOrApply(func(st *State) (bool, error) {
		_, had := st.NoEnvRuns[vmID]
		st.NoEnvRuns[vmID] = at
		delete(st.StopOps, markRecordKey(vmID)) // a record of the former per-run marks
		markTokenNoEnv(st, vmID)
		return !had, nil
	})
}

// noEnvRun reports whether vmID is no_env (sticky: set by a no_env create, start
// or fork, or inherited from a no_env source; it never ends with a stop).
func (s *server) noEnvRun(vmID string) bool {
	st := s.store.Snapshot()
	if pending := st.VMTransitions[vmID]; pending != nil && pending.NoEnv {
		return true
	}
	_, ok := st.NoEnvRuns[vmID]
	if ok {
		return true
	}
	// A create/fork's batched API write may be lost; boatd's labels survive it.
	ctx, cancel := context.WithTimeout(s.baseCtx(), 5*time.Second)
	defer cancel()
	var vm VM
	if err := s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm); err != nil {
		return true // unknown policy cannot authorize releasing team secrets
	}
	return labeledNoEnv(vm)
}

// convSession is the conversation's native session id ("" = unknown or gone).
func convSession(st *State, vmID, conv string) string {
	if c := findConv(st, vmID, conv); c != nil {
		return c.Session
	}
	return ""
}

const (
	// promptRefreshTTL: reads within it of a finished refresh reuse its result.
	promptRefreshTTL = 750 * time.Millisecond
	pollMin          = 50 * time.Millisecond
	pollMax          = 500 * time.Millisecond
	sessionLearnWait = 3 * time.Second
	promptKeepMax    = 512 // refresh records kept before expired ones are dropped
)

// backoff paces a poll loop: delays double from min to max and the sum never
// passes the total, so the loop keeps its time bound.
type backoff struct {
	next, max time.Duration
	deadline  time.Time
}

func newBackoff(lo, hi, total time.Duration) *backoff {
	return &backoff{next: lo, max: hi, deadline: time.Now().Add(total)}
}

// sleep waits one step; false = the total is spent or ctx is done.
func (b *backoff) sleep(ctx context.Context) bool {
	left := time.Until(b.deadline)
	if left <= 0 {
		return false
	}
	d := min(b.next, left)
	b.next = min(b.next*2, b.max)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type promptRefreshKey struct {
	s  *server
	id string
}

type promptRefresh struct {
	last     time.Time     // when the last refresh finished (zero = none / forgotten)
	inflight chan struct{} // closed when the refresh in progress ends
}

type promptRefreshSet struct {
	mu sync.Mutex
	m  map[promptRefreshKey]*promptRefresh
}

var promptRefreshes = &promptRefreshSet{m: map[promptRefreshKey]*promptRefresh{}}

// forget drops the TTL window of vmID so the next read refreshes.
func (ps *promptRefreshSet) forget(s *server, vmID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if e := ps.m[promptRefreshKey{s, vmID}]; e != nil {
		e.last = time.Time{}
	}
}

type refreshMode int

const (
	refreshJoin  refreshMode = iota // share a refresh in flight, else run
	refreshTTL                      // also reuse one finished within promptRefreshTTL
	refreshForce                    // wait for one in flight, then run its own
)

// run does fn for vmID as mode says. Callers arriving during a refresh wait for
// it (refreshForce then still runs its own).
func (ps *promptRefreshSet) run(s *server, r *http.Request, vmID string, mode refreshMode, fn func(*http.Request, string)) {
	key := promptRefreshKey{s, vmID}
	for {
		ps.mu.Lock()
		e := ps.m[key]
		if e == nil {
			e = &promptRefresh{}
			ps.m[key] = e
			ps.prune(key)
		}
		if ch := e.inflight; ch != nil {
			ps.mu.Unlock()
			select {
			case <-ch:
			case <-r.Context().Done():
				return
			}
			if mode == refreshForce {
				continue
			}
			return
		}
		if mode == refreshTTL && !e.last.IsZero() && time.Since(e.last) < promptRefreshTTL {
			ps.mu.Unlock()
			return
		}
		ch := make(chan struct{})
		e.inflight = ch
		ps.mu.Unlock()
		func() {
			defer func() {
				ps.mu.Lock()
				e.inflight = nil
				if r.Context().Err() == nil { // a cancelled refresh may be partial
					e.last = time.Now()
				}
				ps.mu.Unlock()
				close(ch)
			}()
			fn(r, vmID)
		}()
		return
	}
}

// prune drops expired idle records once the set is large (ps.mu held).
func (ps *promptRefreshSet) prune(keep promptRefreshKey) {
	if len(ps.m) <= promptKeepMax {
		return
	}
	for k, e := range ps.m {
		if k != keep && e.inflight == nil && time.Since(e.last) >= promptRefreshTTL {
			delete(ps.m, k)
		}
	}
}

// capConversations returns list without its least recently updated entries
// (the earliest of equal ones) until at most limit remain; list itself is not
// modified, and the order of the rest is kept.
func capConversations(list []*Conversation, limit int) []*Conversation {
	if len(list) <= limit {
		return list
	}
	out := append([]*Conversation(nil), list...)
	for len(out) > limit {
		oldest := 0
		for i, c := range out {
			if c.Updated.Before(out[oldest].Updated) {
				oldest = i
			}
		}
		out = append(out[:oldest], out[oldest+1:]...)
	}
	return out
}
