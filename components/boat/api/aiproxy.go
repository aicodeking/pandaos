package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AI credentials proxy. A team's provider keys stay on the host, sealed like
// secrets. A VM gets <PROVIDER>_BASE_URL = the proxy and <PROVIDER>_API_KEY =
// a per-VM token, so nothing run in the VM (exec included) can read the real
// key. The proxy swaps the token for the key, sends the request only to that
// provider's fixed host, and meters tokens per VM.
//
// A token is boatvm_<vmid>_<hex(HMAC(aiKey, "copy-v1|<vmid>.<gen>"))>. Gen changes at
// every start, stop and source copy, so a copied token dies before its copy is released.

type aiProvider struct {
	Upstream string // fixed target; never taken from the request
	Header   string // x-api-key, or Authorization with Bearer
	EnvKey   string
	EnvBase  string
	BasePath string // appended to the proxy URL for the base-URL env var
}

var aiProviders = map[string]aiProvider{
	"anthropic":  {"https://api.anthropic.com", "x-api-key", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", ""},
	"openai":     {"https://api.openai.com", "Authorization", "OPENAI_API_KEY", "OPENAI_BASE_URL", "/v1"},
	"openrouter": {"https://openrouter.ai", "Authorization", "OPENROUTER_API_KEY", "OPENROUTER_BASE_URL", "/api/v1"},
	"deepseek":   {"https://api.deepseek.com", "Authorization", "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL", ""},
	"mistral":    {"https://api.mistral.ai", "Authorization", "MISTRAL_API_KEY", "MISTRAL_BASE_URL", "/v1"},
	"moonshot":   {"https://api.moonshot.ai", "Authorization", "MOONSHOT_API_KEY", "MOONSHOT_BASE_URL", "/v1"},
	"xai":        {"https://api.x.ai", "Authorization", "XAI_API_KEY", "XAI_BASE_URL", "/v1"},
}

func aiCredKey(team, provider string) string { return team + "/" + provider }

func (s *server) aiToken(vmID string, gen int) string {
	message := fmt.Sprintf("%s.%d", vmID, gen)
	if s.store.Snapshot().CopyCredentialsV1 {
		message = "copy-v1|" + message
	}
	return "boatvm_" + vmID + "_" + signHex(s.keys.aiKey, message)
}

var reAIToken = regexp.MustCompile(`^boatvm_([A-Za-z0-9_.-]{1,64})_([0-9a-f]{64})$`)

// aiCheckToken returns the VM's identity if tok is its current token.
func (s *server) aiCheckToken(tok string) (string, *AIVM, bool) {
	m := reAIToken.FindStringSubmatch(tok)
	if m == nil {
		return "", nil, false
	}
	st := s.store.Snapshot()
	vm := st.AIVMs[m[1]]
	if vm == nil || vm.Retired || st.Teams[vm.TeamID] == nil || !hmac.Equal([]byte(s.aiToken(m[1], vm.Gen)), []byte(tok)) {
		return "", nil, false
	}
	return m[1], vm, true
}

// aiBumpVM registers a VM (or moves it to a new token generation) and returns gen.
func (s *server) aiBumpVM(vmID, team string, wait bool) (int, error) {
	gen := 0
	err := s.store.UpdateOrApply(func(st *State) (bool, error) {
		if st.Teams[team] == nil {
			return false, fmt.Errorf("VM team no longer exists")
		}
		vm := st.AIVMs[vmID]
		if vm != nil && (vm.Retired || vm.TeamID != team) {
			return false, fmt.Errorf("VM credentials are retired")
		}
		syncWrite := wait && vm != nil
		if vm == nil {
			vm = &AIVM{TeamID: team, Blocked: st.ForeignVMs[vmID]}
			st.AIVMs[vmID] = vm
		} else {
			vm = st.cowAIVM(vmID)
		}
		vm.Gen++
		gen = vm.Gen
		return syncWrite, nil
	})
	return gen, err
}

// aiEnv returns the env vars for a VM's run: a proxy token per team credential,
// or (only when the team switched the proxy off) the raw keys. It starts a new
// token generation when bump is set.
func (s *server) aiEnv(vmID, team string, bump bool) (map[string]string, error) {
	return s.aiEnvGen(vmID, team, bump, false, false)
}

// aiEnvRun is aiEnv for a run that may be no_env: such a run gets no raw
// provider key (a proxy token is revocable per VM; a raw key is not).
func (s *server) aiEnvRun(vmID, team string, bump, noEnv bool) (map[string]string, error) {
	return s.aiEnvGen(vmID, team, bump, false, noEnv)
}

// First issuance for a brand-new VM commits via Store.Apply; reconcileCopyCredentials
// recovers generation 1 from the live guest env if a crash precedes the flush.
func (s *server) aiEnvNewVM(vmID, team string) (map[string]string, error) {
	return s.aiEnvGen(vmID, team, true, false, false)
}

func (s *server) aiEnvGen(vmID, team string, bump, wait, noEnv bool) (map[string]string, error) {
	st := s.store.Snapshot()
	if st.Teams[team] == nil {
		return nil, fmt.Errorf("VM team no longer exists")
	}
	if vm := st.AIVMs[vmID]; vm != nil && (vm.Retired || vm.TeamID != team) {
		return nil, fmt.Errorf("VM credentials are retired")
	}
	var creds []*AICredential
	for _, c := range st.AICreds {
		if c.TeamID == team {
			creds = append(creds, c)
		}
	}
	if len(creds) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	if st.AIProxyOff[team] {
		if noEnv || st.ForeignVMs[vmID] {
			return nil, nil // raw keys are irrevocable; proxy authorization cannot change disk provenance
		}
		if vm := st.AIVMs[vmID]; vm != nil && vm.Blocked {
			return nil, nil // raw keys cannot be blocked later, so they never go in
		}
		for _, c := range creds {
			key, err := s.keys.Open(c.Sealed, "ai/"+aiCredKey(c.TeamID, c.Provider))
			if err != nil {
				return nil, fmt.Errorf("decrypt %s credential: %w", c.Provider, err)
			}
			env[aiProviders[c.Provider].EnvKey] = string(key)
		}
		return env, nil
	}
	if s.cfg.AIProxyURL == "" {
		log.Printf("vm %s: team %s has AI credentials but -ai-proxy-url is not set; none injected", vmID, team)
		return nil, nil
	}
	gen := 0
	if vm := st.AIVMs[vmID]; vm != nil {
		gen = vm.Gen
	}
	if bump || gen == 0 {
		var err error
		if gen, err = s.aiBumpVM(vmID, team, wait); err != nil {
			return nil, err
		}
	}
	base := strings.TrimRight(s.cfg.AIProxyURL, "/")
	tok := s.aiToken(vmID, gen)
	for _, c := range creds {
		pr := aiProviders[c.Provider]
		env[pr.EnvKey] = tok
		env[pr.EnvBase] = base + "/" + c.Provider + pr.BasePath
	}
	return env, nil
}

// ---- management API ----

func (s *server) listAICreds(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []map[string]any{}
	for _, c := range s.store.Snapshot().AICreds {
		if c.TeamID == p.Team.ID {
			out = append(out, map[string]any{"provider": c.Provider, "last4": c.Last4, "updated": c.Updated})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["provider"].(string) < out[j]["provider"].(string) })
	writeJSON(w, 200, map[string]any{"credentials": out})
}

func (s *server) putAICred(w http.ResponseWriter, r *http.Request, p *principal) {
	provider := r.PathValue("provider")
	var body struct {
		Value string `json:"value"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, ok := aiProviders[provider]; !ok {
		writeErr(w, r, 400, "invalid_request", "unknown provider "+provider)
		return
	}
	if len(body.Value) < 8 || len(body.Value) > 4096 || strings.ContainsAny(body.Value, "\r\n\x00") {
		writeErr(w, r, 400, "invalid_request", "value must be an API key (one line, 8..4096 chars)")
		return
	}
	sealed, err := s.keys.Seal([]byte(body.Value), "ai/"+aiCredKey(p.Team.ID, provider))
	if err != nil {
		writeErr(w, r, 500, "crypto_error", "could not encrypt")
		return
	}
	c := &AICredential{TeamID: p.Team.ID, Provider: provider, Sealed: sealed, Last4: body.Value[len(body.Value)-4:], Updated: time.Now().UTC()}
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		st.AICreds[aiCredKey(p.Team.ID, provider)] = c
		return nil
	}); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"provider": provider, "last4": c.Last4, "updated": c.Updated})
}

func (s *server) deleteAICred(w http.ResponseWriter, r *http.Request, p *principal) {
	k := aiCredKey(p.Team.ID, r.PathValue("provider"))
	if s.store.Snapshot().AICreds[k] == nil {
		writeErr(w, r, 404, "not_found", "credential not found")
		return
	}
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		delete(st.AICreds, k)
		return nil
	}); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) aiProxyView(team string) map[string]any {
	return map[string]any{"enabled": !s.store.Snapshot().AIProxyOff[team], "available": s.cfg.AIProxyURL != ""}
}

func (s *server) getAIProxy(w http.ResponseWriter, r *http.Request, p *principal) {
	writeJSON(w, 200, s.aiProxyView(p.Team.ID))
}

// putAIProxy: enabled=false injects the raw keys into VMs at their next start.
func (s *server) putAIProxy(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, r, 400, "invalid_request", "enabled is required")
		return
	}
	err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		if *body.Enabled {
			delete(st.AIProxyOff, p.Team.ID)
		} else {
			st.AIProxyOff[p.Team.ID] = true
		}
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, s.aiProxyView(p.Team.ID))
}

func (s *server) aiUsageRows(team, vmID string) []*AIUsageRow {
	st, pending := s.aiMeter.view(s.store, team, vmID)
	rows := []*AIUsageRow{}
	rows = append(rows, teamAIUsageRows(st, pending, team, vmID, false)...)
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.VMID+a.Provider+a.Model < b.VMID+b.Provider+b.Model
	})
	return rows
}

func usageTotals(rows []*AIUsageRow) map[string]int64 {
	t := map[string]int64{"requests": 0, "input_tokens": 0, "output_tokens": 0, "unmetered": 0}
	for _, u := range rows {
		t["unmetered"] += u.Unmetered
		t["requests"] += u.Requests
		t["input_tokens"] += u.InputTokens
		t["output_tokens"] += u.OutputTokens
	}
	return t
}

func (s *server) teamAIUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	rows := s.aiUsageRows(p.Team.ID, "")
	writeJSON(w, 200, map[string]any{"usage": rows, "totals": usageTotals(rows)})
}

func (s *server) vmAIUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	rows := s.aiUsageRows(p.Team.ID, r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"usage": rows, "totals": usageTotals(rows)})
}

// setAIAccess blocks or unblocks a VM at the proxy, effective at once.
func (s *server) setAIAccess(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, r, 400, "invalid_request", "enabled is required")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		if st.Teams[p.Team.ID] == nil {
			return fmt.Errorf("VM credentials are unavailable")
		}
		vm := st.AIVMs[id]
		if vm != nil && vm.Retired {
			return fmt.Errorf("VM credentials are unavailable")
		}
		if vm == nil {
			vm = &AIVM{TeamID: p.Team.ID}
			st.AIVMs[id] = vm
		} else {
			vm = st.cowAIVM(id)
		}
		vm.Blocked = !*body.Enabled
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": *body.Enabled})
}

// ---- the proxy itself ----

// aiRequestHeaders are never forwarded: credentials, hop-by-hop, and anything
// that names the local network. Accept-Encoding goes so usage can be read.
var aiDropRequest = map[string]bool{"Authorization": true, "X-Api-Key": true, "Cookie": true, "Host": true,
	"Connection": true, "Proxy-Connection": true, "Proxy-Authorization": true, "Keep-Alive": true, "Te": true,
	"Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Forwarded": true, "X-Forwarded-For": true,
	"X-Forwarded-Host": true, "X-Forwarded-Proto": true, "X-Real-Ip": true, "Accept-Encoding": true, "Content-Length": true}

var aiDropResponse = map[string]bool{"Set-Cookie": true, "Connection": true, "Keep-Alive": true,
	"Proxy-Authenticate": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Content-Length": true}

func aiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": "boat_proxy_error", "message": msg}})
}

// aiHandler serves only the proxy: <listen>/<provider>/<upstream path>.
func (s *server) aiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{provider}/{path...}", s.aiProxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	return mux
}

func (s *server) aiProxy(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	pr, ok := aiProviders[provider]
	if !ok {
		aiError(w, 404, "unknown provider")
		return
	}
	if !aiPathAllowed(provider, r, r.PathValue("path")) {
		aiError(w, 403, "not an inference endpoint: the AI proxy forwards only the paths docs/boat.md lists for "+provider)
		return
	}
	tok := r.Header.Get("X-Api-Key")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	vmID, vm, ok := s.aiCheckToken(strings.TrimSpace(tok))
	if !ok {
		aiError(w, 401, "invalid or expired VM token (tokens change at every VM start and stop)")
		return
	}
	if vm.Blocked {
		aiError(w, 403, "AI access is blocked for this VM")
		return
	}
	if s.store.Snapshot().AIProxyOff[vm.TeamID] {
		aiError(w, 403, "the team switched the AI proxy off")
		return
	}
	// A stream can outlive its token: re-resolve the VM's AI registration while the
	// request is open and cancel the upstream call (and so this response) once the
	// token generation moved on or the VM was retired, blocked or lost its team.
	// It also ends when the team's credential for this provider is deleted or
	// replaced (the stream was authorized by the one it started with) or the team
	// switched the proxy off.
	gen := vm.Gen
	cred := s.store.Snapshot().AICreds[aiCredKey(vm.TeamID, provider)]
	// The upstream call outlives a client that hangs up (see aiDrainMax), so its
	// context is cut off from the client's and ends on revocation, on the drain
	// bound, or when the handler returns.
	clientGone := r.Context().Done()
	r, stopWatch := s.watchAlive(r.WithContext(context.WithoutCancel(r.Context())), func(st *State, _ time.Time) bool {
		cur := st.AIVMs[vmID]
		if cur == nil || cur.Gen != gen || cur.Retired || cur.Blocked || st.Teams[cur.TeamID] == nil || st.AIProxyOff[cur.TeamID] {
			return false
		}
		if cred == nil {
			return true // answered 404 below; nothing to revoke
		}
		now := st.AICreds[aiCredKey(vm.TeamID, provider)]
		return now != nil && now.Sealed == cred.Sealed && now.Updated.Equal(cred.Updated)
	})
	defer stopWatch()
	if ok, wait := s.aiLim.allow(vmID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		aiError(w, 429, "AI proxy rate limit for this VM")
		return
	}
	release, ok := s.aiInflight.acquire(vmID)
	if !ok {
		w.Header().Set("Retry-After", "5")
		aiError(w, 429, "too many AI proxy requests in flight for this VM")
		return
	}
	defer release()
	if cred == nil {
		aiError(w, 404, "the team has no "+provider+" credential")
		return
	}
	key, err := s.keys.Open(cred.Sealed, "ai/"+aiCredKey(cred.TeamID, cred.Provider))
	if err != nil {
		log.Printf("ai proxy: decrypt %s for team %s: %v", provider, vm.TeamID, err)
		aiError(w, 500, "cannot read the credential")
		return
	}
	upstream := pr.Upstream
	if u := s.cfg.AIUpstreams[provider]; u != "" {
		upstream = u
	}
	target := upstream + "/" + aiEscapePath(r.PathValue("path"))
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, aiMaxBody)
	contentLength := r.ContentLength
	res := s.aiInflight.reserveTeam(vmID, vm.TeamID, aiBudgetPerVM, aiBudgetPerTeam, aiBudgetGlobal)
	defer res.release() // also on every early return
	var held *aiHeldBody
	var streamGuard *aiProgressReader
	if aiMayInjectUsage(provider, r) {
		// Whatever the framing (sized, chunked, any length up to aiMaxBody) the body is
		// read whole and checked, so the guest cannot pick a shape that is not metered.
		raw, herr := aiReadBody(w, r, res)
		if herr == nil {
			raw, herr = aiPrepareBody(raw)
		}
		if herr != nil {
			writeAIBodyError(w, herr)
			return
		}
		held = newAIHeldBody(raw, res)
		defer held.drop()
		body, contentLength = held.reader(), int64(len(raw))
	} else {
		if contentLength > aiMaxBody {
			writeAIBodyError(w, &aiBodyError{status: 413, typ: "boat_proxy_error", msg: "the request body is too large"})
			return
		}
		if r.Body != nil && r.Body != http.NoBody && contentLength != 0 {
			streamGuard = newAIProgressReader(w, http.MaxBytesReader(w, r.Body, aiMaxBody))
			defer streamGuard.done()
			body = streamGuard
		}
	}
	upCtx, upCancel := context.WithCancel(r.Context())
	defer upCancel()
	drainMax, drainBytes := aiDrainMax, aiDrainBytes // read once: tests change them
	go aiDrainTimer(upCtx, upCancel, clientGone, drainMax)
	reqCtx := upCtx
	if held != nil {
		// The buffer and its budget go back as soon as the body is on the wire, not
		// when the provider (which may think for minutes) answers.
		reqCtx = httptrace.WithClientTrace(upCtx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				held.drop()
			}
		}})
	}
	req, err := http.NewRequestWithContext(reqCtx, r.Method, target, body)
	if err != nil {
		aiError(w, 400, "bad request")
		return
	}
	if held != nil {
		req.GetBody = held.getBody
	}
	req.ContentLength = contentLength
	for k, v := range r.Header {
		if !aiDropRequest[k] {
			req.Header[k] = v
		}
	}
	if pr.Header == "x-api-key" {
		req.Header.Set("X-Api-Key", string(key))
	} else {
		req.Header.Set("Authorization", "Bearer "+string(key))
	}
	resp, err := s.aiClient.Do(req)
	if held != nil {
		held.drop() // no-op when the trace already did
	}
	if streamGuard != nil {
		streamGuard.done()
	}
	if err != nil {
		if streamGuard != nil {
			if berr := streamGuard.bodyError(); berr != nil {
				writeAIBodyError(w, berr)
				return
			}
		}
		// err may name the upstream URL, never the key; keep it out of the VM anyway.
		log.Printf("ai proxy: vm %s %s: %v", vmID, provider, err)
		aiError(w, 502, "cannot reach the provider")
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		if !aiDropResponse[k] {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	tap := &usageTap{}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	gone, drained := false, 0
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			tap.Write(buf[:n])
			if gone {
				// The client left: keep reading the provider into the tap so the final
				// usage chunk is metered, up to aiDrainBytes (and aiDrainMax in time).
				if drained += n; drained > drainBytes {
					break
				}
			} else if _, werr := w.Write(buf[:n]); werr != nil {
				gone = true
			} else if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	in, out, model := tap.result()
	unmetered := in == 0 && out == 0 && aiShouldMeter(r, resp.StatusCode)
	if unmetered {
		// The request is counted (requests) with zero tokens and flagged in the usage
		// rows (unmetered); this line says why.
		log.Printf("ai proxy: unmetered response: vm %s %s %s status %d (no usage object seen%s)", vmID, provider, r.PathValue("path"), resp.StatusCode, map[bool]string{true: "; client left", false: ""}[gone])
	}
	s.aiMeter.record(vm.TeamID, vmID, provider, model, in, out, unmetered)
}

// After the client hangs up the proxy keeps draining the provider's response into
// the usage tap for at most this long and this many bytes. Variables for tests.
var (
	aiDrainMax   = 30 * time.Second
	aiDrainBytes = 8 << 20
)

// aiDrainTimer cancels the upstream call aiDrainMax after the client went away
// (clientGone closes), or stops with the call.
func aiDrainTimer(upCtx context.Context, cancel context.CancelFunc, clientGone <-chan struct{}, window time.Duration) {
	select {
	case <-upCtx.Done():
	case <-clientGone:
		t := time.NewTimer(window)
		defer t.Stop()
		select {
		case <-upCtx.Done():
		case <-t.C:
			cancel()
		}
	}
}

// aiInjectUsage forces "stream_options": {"include_usage": true} on a streaming
// completions body, whatever the caller sent (false, a string, another
// type), so the final chunk carries the token counts the proxy meters. The
// result is the canonical re-encoding of the object (duplicate keys collapse, so
// the provider cannot read another value than the proxy did). A body that is not
// a JSON object, is not streaming, or has a stream_options that is not an
// object is unchanged (false); the provider answers those itself.
func aiInjectUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body, false
	}
	var stream bool
	if json.Unmarshal(m["stream"], &stream) != nil || !stream {
		return body, false
	}
	opts, ok := aiStreamOptions(m)
	if !ok {
		return body, false
	}
	out, err := aiMarshalWithUsage(m, opts)
	if err != nil {
		return body, false
	}
	return out, true
}

// aiShouldMeter: a successful inference POST whose answer should carry usage.
func aiShouldMeter(r *http.Request, status int) bool {
	if r.Method != http.MethodPost || status < 200 || status > 299 {
		return false
	}
	p := r.PathValue("path")
	last := p[strings.LastIndexByte(p, '/')+1:]
	switch last {
	case "completions", "messages", "responses", "embeddings", "complete":
		return true
	}
	return false
}

// usageTap reads token counts from a copy of the response. It handles a whole
// JSON body and SSE; it keeps at most 8 MiB and never affects the response.
type usageTap struct {
	buf      bytes.Buffer
	overflow bool
	// A JSON body over the cap keeps no head (only its model) but its last
	// tapTailBytes: the usage object closes the body.
	tail     []byte
	kind     tapKind
	searched int  // bytes of buf already searched for an SSE marker
	skipping bool // SSE: inside a line too long to keep; only its tail is kept
	in, out  int64
	model    string
}

// tapKind is what the tap has decided the body is; it is decided once.
type tapKind int

const (
	tapUnknown tapKind = iota // not enough seen to tell
	tapSSE                    // event stream: scan complete lines as they arrive
	tapJSON                   // a whole JSON body: parsed at the end
)

// sseMarker starts a data line after the first one.
var sseMarker = []byte("\ndata:")

func (t *usageTap) Write(b []byte) {
	for len(b) > 0 {
		if t.skipping {
			b = t.skipLine(b)
			continue
		}
		if t.overflow {
			if t.kind == tapJSON {
				t.addTail(b)
			}
			return
		}
		if t.buf.Len()+len(b) > 8<<20 && t.kind == tapJSON {
			// A whole JSON body is never line-scanned: take its model from the head, then
			// keep only the tail.
			if mod := jsonModelRe.FindSubmatch(t.buf.Bytes()); mod != nil {
				t.model = string(mod[1])
			}
			t.overflow = true
			t.buf = bytes.Buffer{}
			t.addTail(b)
			return
		}
		if t.buf.Len()+len(b) > 8<<20 {
			t.scan() // keep what is complete, drop the rest of the buffer
			t.searched = 0
			if t.buf.Len()+len(b) > 8<<20 {
				if t.kind != tapSSE {
					t.overflow = true
					return
				}
				// One SSE line over the cap: keep its tail until it ends, so a usage line
				// after it is still read.
				t.tail = t.tail[:0]
				t.addTail(t.buf.Bytes())
				t.buf.Reset()
				t.skipping = true
				continue
			}
		}
		t.buf.Write(b)
		t.classify()
		if t.kind == tapSSE && bytes.IndexByte(b, '\n') >= 0 {
			t.scan() // a line can only have completed if this chunk had a newline
			t.searched = 0
		}
		return
	}
}

// skipLine consumes b up to the end of an oversized SSE line (reading usage from
// the line's tail) and returns what follows it, or nil while the line goes on.
func (t *usageTap) skipLine(b []byte) []byte {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		t.addTail(b)
		return nil
	}
	t.addTail(b[:i])
	t.parseTail()
	t.tail, t.skipping = nil, false
	return b[i+1:]
}

// tapTailBytes is how much of the end of an oversized JSON body the tap keeps.
const tapTailBytes = 1 << 20

var (
	jsonModelRe = regexp.MustCompile(`"model"\s*:\s*"([^"\\]{1,128})"`)
	jsonUsageRe = regexp.MustCompile(`"usage"\s*:\s*\{`)
)

// addTail keeps the last tapTailBytes of the body. The buffer compacts only when
// it reaches twice that, so the copying stays linear in the body's size.
func (t *usageTap) addTail(b []byte) {
	t.tail = append(t.tail, b...)
	if len(t.tail) > 2*tapTailBytes {
		n := copy(t.tail, t.tail[len(t.tail)-tapTailBytes:])
		t.tail = t.tail[:n]
	}
}

// parseTail reads usage (and a model the head did not show) from the kept tail
// of an oversized JSON body: the last "usage" object that decodes.
func (t *usageTap) parseTail() {
	tail := t.tail
	if len(tail) > tapTailBytes {
		tail = tail[len(tail)-tapTailBytes:]
	}
	if t.model == "" {
		all := jsonModelRe.FindAllSubmatch(tail, -1)
		if n := len(all); n > 0 {
			t.model = string(all[n-1][1])
		}
	}
	locs := jsonUsageRe.FindAllIndex(tail, -1)
	for tries, i := 0, len(locs)-1; i >= 0 && tries < 8; i, tries = i-1, tries+1 {
		var u json.RawMessage
		if err := json.NewDecoder(bytes.NewReader(tail[locs[i][1]-1:])).Decode(&u); err != nil {
			continue
		}
		t.parse(append(append([]byte(`{"usage":`), u...), '}'))
		if t.in > 0 || t.out > 0 {
			return
		}
	}
}

// classify decides the body's kind once, looking only at bytes it has not
// searched (plus the marker's length, for one split across two chunks).
func (t *usageTap) classify() {
	if t.kind != tapUnknown {
		return
	}
	data := t.buf.Bytes()
	if bytes.HasPrefix(data, []byte("data:")) || bytes.HasPrefix(data, []byte("event:")) {
		t.kind = tapSSE
		return
	}
	if first := bytes.TrimLeft(data, " \t\r\n"); len(first) > 0 && (first[0] == '{' || first[0] == '[') {
		t.kind = tapJSON
		return
	}
	from := t.searched - (len(sseMarker) - 1)
	if from < 0 {
		from = 0
	}
	if bytes.Contains(data[from:], sseMarker) {
		t.kind = tapSSE
		return
	}
	t.searched = len(data)
}

// tapLineMax is the longest SSE data line decoded whole; a longer one is read
// from its head (model) and tail (usage) like an oversized JSON body.
const tapLineMax = 1 << 20

// scan consumes complete SSE lines in place: no scanner, no buffer per call, and
// no limit on a line that could swallow the lines after it.
func (t *usageTap) scan() {
	data := t.buf.Bytes()
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return
	}
	for rest := data[:end+1]; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if line, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:")); ok {
			if len(line) > tapLineMax {
				t.parseLong(line)
			} else {
				t.parse(bytes.TrimSpace(line))
			}
		}
	}
	t.buf.Next(end + 1)
}

// parseLong reads usage from the end of a data line too long to decode whole.
func (t *usageTap) parseLong(line []byte) {
	if t.model == "" {
		if mod := jsonModelRe.FindSubmatch(line[:min(len(line), 64<<10)]); mod != nil {
			t.model = string(mod[1])
		}
	}
	saved := t.tail
	t.tail = line[len(line)-min(len(line), tapTailBytes):]
	t.parseTail()
	t.tail = saved
}

func (t *usageTap) parse(b []byte) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	nested := func(k string) map[string]any {
		v, _ := m[k].(map[string]any)
		return v
	}
	for _, src := range []map[string]any{m, nested("message"), nested("response")} {
		if src == nil {
			continue
		}
		if mod, _ := src["model"].(string); mod != "" {
			t.model = mod
		}
		u, _ := src["usage"].(map[string]any)
		if u == nil {
			continue
		}
		// Streams report cumulative counts, so the largest value seen is the total.
		for _, k := range []string{"input_tokens", "prompt_tokens"} {
			if f, ok := u[k].(float64); ok && int64(f) > t.in {
				t.in = int64(f)
			}
		}
		for _, k := range []string{"output_tokens", "completion_tokens"} {
			if f, ok := u[k].(float64); ok && int64(f) > t.out {
				t.out = int64(f)
			}
		}
	}
}

func (t *usageTap) result() (in, out int64, model string) {
	if (t.overflow && t.kind == tapJSON) || t.skipping {
		t.parseTail()
		t.skipping = false
	}
	if t.buf.Len() > 0 && !t.overflow {
		if bytes.HasPrefix(bytes.TrimSpace(t.buf.Bytes()), []byte("{")) {
			t.parse(t.buf.Bytes())
		} else {
			t.buf.WriteByte('\n')
			t.scan()
		}
	}
	return t.in, t.out, t.model
}

// aiMeter batches usage in memory. flush folds the batch into the store with a
// lazy (coalesced, fsync-free) write, and reads overlay the batch on the store
// (view, overlayAIUsage) instead of flushing it, so counting and reading usage
// never wait for a state.json fsync.
//
// Crash loss is bounded: a kill -9 loses at most the batch since the last flush
// (aiMeterInterval) plus the lazy write delay (aiMeterLazy); any durable store
// write in between, and a clean shutdown (flushFinal), persists everything.
type aiMeter struct {
	mu   sync.Mutex
	rows map[string]*AIUsageRow
	// fmu orders flush against view: flush holds it while a batch moves from rows
	// to the store, readers hold it while they read both, so no count is missed or
	// doubled in between.
	fmu sync.RWMutex
}

const (
	aiMeterInterval = 30 * time.Second
	aiMeterLazy     = 5 * time.Second
)

func newAIMeter() *aiMeter { return &aiMeter{rows: map[string]*AIUsageRow{}} }

// add counts one metered request now.
func (m *aiMeter) add(team, vm, provider, model string, in, out int64) {
	m.recordAt(time.Now(), team, vm, provider, model, in, out, false)
}

// addAt counts one metered request on the UTC day of at.
func (m *aiMeter) addAt(at time.Time, team, vm, provider, model string, in, out int64) {
	m.recordAt(at, team, vm, provider, model, in, out, false)
}

// record counts one request now; unmetered marks an answer with no usage object.
func (m *aiMeter) record(team, vm, provider, model string, in, out int64, unmetered bool) {
	m.recordAt(time.Now(), team, vm, provider, model, in, out, unmetered)
}

// recordAt counts one request on the UTC day of at.
func (m *aiMeter) recordAt(at time.Time, team, vm, provider, model string, in, out int64, unmetered bool) {
	if model == "" {
		model = "unknown"
	}
	if len(model) > 128 {
		model = model[:128]
	}
	day := at.UTC().Format(time.DateOnly)
	k := aiUsageKey(team, vm, provider, model) + "/" + day
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[k]
	if r == nil {
		r = &AIUsageRow{TeamID: team, VMID: vm, Provider: provider, Model: model, Day: day}
		m.rows[k] = r
	}
	r.Requests++
	r.InputTokens += in
	r.OutputTokens += out
	if unmetered {
		r.Unmetered++
	}
	r.Updated = at.UTC()
}

// dropTeam forgets the unflushed counts of a deleted team.
func (m *aiMeter) dropTeam(team string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.rows {
		if r.TeamID == team {
			delete(m.rows, k)
		}
	}
}

// view returns the store's current state and copies only the requested team's
// unflushed rows (one VM when set), consistent with each other. It never writes.
func (m *aiMeter) view(store *Store, team, vmID string) (*State, map[string]*AIUsageRow) {
	m.fmu.RLock()
	defer m.fmu.RUnlock()
	st := store.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := map[string]*AIUsageRow{}
	// shortcut: scan the pending batch; index it if traversal under mu becomes costly.
	for k, r := range m.rows {
		if r.TeamID != team || (vmID != "" && r.VMID != vmID) {
			continue
		}
		cp := *r
		pending[k] = &cp
	}
	return st, pending
}

// flush moves the batch into the store's memory; the disk write is lazy.
func (m *aiMeter) flush(s *server) { m.flushWith(s, false) }

// flushFinal is flush for shutdown: it waits for the disk. Call it before
// Store.Close.
func (m *aiMeter) flushFinal(s *server) { m.flushWith(s, true) }

func (m *aiMeter) flushWith(s *server, durable bool) {
	m.fmu.Lock()
	defer m.fmu.Unlock()
	m.mu.Lock()
	pending := m.rows
	m.rows = map[string]*AIUsageRow{}
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	now := time.Now().UTC()
	apply := func(st *State) error {
		var life, days []*AIUsageRow // the rows touched
		newKey := false
		for k0, d := range pending {
			if st.Teams[d.TeamID] == nil { // the team was deleted while this batch was detached
				delete(pending, k0)
				continue
			}
			k := aiUsageKey(d.TeamID, d.VMID, d.Provider, d.Model)
			lr, c1 := addAIUsage(st.AIUsage, k, d, "", now)
			dr, c2 := addAIUsage(st.AIUsageDays, k+"/"+d.Day, d, d.Day, now)
			life, days, newKey = append(life, lr), append(days, dr), newKey || c1 || c2
		}
		// A request that finished after its VM was deleted counts for the team's
		// aggregate, not for a row of the gone VM.
		folded := map[string]bool{}
		for _, d := range pending {
			if ai := st.AIVMs[d.VMID]; ai != nil && ai.Retired && !folded[d.VMID] {
				folded[d.VMID] = true
				foldDeletedVMAIUsage(st, d.VMID)
			}
		}
		pruneAIUsageDays(st, now) // drops the index when it removes a key
		newKey = capAIUsageLife(st) || newKey
		if newKey {
			st.dropAIUsageIndex()
		} else {
			st.patchAIUsageIndex(life, days) // counters only: keep the index, swap the rows
		}
		return nil
	}
	var err error
	if durable {
		err = s.store.Update(apply)
	} else {
		err = s.store.ApplyLazy(apply, aiMeterLazy)
	}
	if err != nil {
		log.Printf("ai usage flush: %v", err)
		m.mu.Lock()
		defer m.mu.Unlock()
		for k, d := range pending { // keep the counts for the next flush
			if cur := m.rows[k]; cur != nil {
				cur.Requests += d.Requests
				cur.InputTokens += d.InputTokens
				cur.OutputTokens += d.OutputTokens
				cur.Unmetered += d.Unmetered
			} else {
				m.rows[k] = d
			}
		}
	}
}

// aiEscapePath re-encodes a decoded upstream path segment by segment, so a
// "?" or "#" the guest sent encoded (%3F, %23) stays part of the path and cannot
// become the upstream's query or fragment. The path was checked first (no "." or
// ".." segment, no encoded slash).
func aiEscapePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// newAIClient is the proxy's upstream client. The pool is sized for many VMs
// calling one provider host at once (the default keeps two idle connections per
// host, so every burst redialed and re-handshook TLS) and HTTP/2 is on (a custom
// dialer turns it off unless forced).
func newAIClient() *http.Client {
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second, DisableCompression: true, ResponseHeaderTimeout: 10 * time.Minute,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: true}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// serveAIProxy runs the VM-facing listener, retrying the bind because the
// bridge address appears only when boatd has created it.
func (s *server) serveAIProxy(ctx context.Context, addr string) {
	var ln net.Listener
	for ctx.Err() == nil {
		var err error
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		log.Printf("ai proxy: listen %s: %v; retrying", addr, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	if ln == nil {
		return
	}
	srv := &http.Server{Handler: s.aiHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("ai proxy on %s", addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("ai proxy: %v", err)
	}
}

// flushAIUsageFinal writes the unflushed AI usage to disk. Shutdown calls it
// before Store.Close.
func (s *server) flushAIUsageFinal() { s.aiMeter.flushFinal(s) }

// aiMeterLoop flushes usage until ctx ends.
func (s *server) aiMeterLoop(ctx context.Context) {
	t := time.NewTicker(aiMeterInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.aiMeter.flushFinal(s)
			return
		case <-t.C:
			s.aiMeter.flush(s)
		}
	}
}
