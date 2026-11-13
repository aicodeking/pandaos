package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
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
	write := s.store.Update
	if !wait {
		write = s.store.Apply
	}
	err := write(func(st *State) error {
		if st.Teams[team] == nil {
			return fmt.Errorf("VM team no longer exists")
		}
		vm := st.AIVMs[vmID]
		if vm != nil && (vm.Retired || vm.TeamID != team) {
			return fmt.Errorf("VM credentials are retired")
		}
		if vm == nil {
			vm = &AIVM{TeamID: team}
			st.AIVMs[vmID] = vm
		}
		vm.Gen++
		gen = vm.Gen
		return nil
	})
	return gen, err
}

// aiEnv returns the env vars for a VM's run: a proxy token per team credential,
// or (only when the team switched the proxy off) the raw keys. It starts a new
// token generation when bump is set.
func (s *server) aiEnv(vmID, team string, bump bool) (map[string]string, error) {
	return s.aiEnvGen(vmID, team, bump, true)
}

// First issuance must be durable too: the guest can outlive an API crash and be copied.
func (s *server) aiEnvNewVM(vmID, team string) (map[string]string, error) {
	return s.aiEnv(vmID, team, true)
}

func (s *server) aiEnvGen(vmID, team string, bump, wait bool) (map[string]string, error) {
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
	if err := s.store.Update(func(st *State) error { st.AICreds[aiCredKey(p.Team.ID, provider)] = c; return nil }); err != nil {
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
	if err := s.store.Update(func(st *State) error { delete(st.AICreds, k); return nil }); err != nil {
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
	s.aiMeter.flush(s)
	rows := []*AIUsageRow{}
	for _, u := range s.store.Snapshot().AIUsage {
		if u.TeamID == team && (vmID == "" || u.VMID == vmID) {
			rows = append(rows, u)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.VMID+a.Provider+a.Model < b.VMID+b.Provider+b.Model
	})
	return rows
}

func usageTotals(rows []*AIUsageRow) map[string]int64 {
	t := map[string]int64{"requests": 0, "input_tokens": 0, "output_tokens": 0}
	for _, u := range rows {
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

const aiMaxBody = 64 << 20

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
	cred := s.store.Snapshot().AICreds[aiCredKey(vm.TeamID, provider)]
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
	target := upstream + "/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, http.MaxBytesReader(w, r.Body, aiMaxBody))
	if err != nil {
		aiError(w, 400, "bad request")
		return
	}
	req.ContentLength = r.ContentLength
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
	if err != nil {
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
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			tap.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	in, out, model := tap.result()
	s.aiMeter.add(vm.TeamID, vmID, provider, model, in, out)
}

// usageTap reads token counts from a copy of the response. It handles a whole
// JSON body and SSE; it keeps at most 8 MiB and never affects the response.
type usageTap struct {
	buf      bytes.Buffer
	overflow bool
	in, out  int64
	model    string
}

func (t *usageTap) Write(b []byte) {
	if t.overflow {
		return
	}
	if t.buf.Len()+len(b) > 8<<20 {
		t.scan() // keep what is complete, drop the rest of the buffer
		if t.buf.Len()+len(b) > 8<<20 {
			t.overflow = true
			return
		}
	}
	t.buf.Write(b)
	if bytes.Contains(t.buf.Bytes(), []byte("\ndata:")) || bytes.HasPrefix(t.buf.Bytes(), []byte("data:")) || bytes.HasPrefix(t.buf.Bytes(), []byte("event:")) {
		t.scan()
	}
}

// scan consumes complete SSE lines.
func (t *usageTap) scan() {
	data := t.buf.Bytes()
	i := bytes.LastIndexByte(data, '\n')
	if i < 0 {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(data[:i]))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		if line, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:")); ok {
			t.parse(bytes.TrimSpace(line))
		}
	}
	rest := append([]byte{}, data[i+1:]...)
	t.buf.Reset()
	t.buf.Write(rest)
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

// aiMeter batches usage: a write per proxied request would rewrite state.json
// each time. Up to one flush interval of counts can be lost in a crash.
type aiMeter struct {
	mu   sync.Mutex
	rows map[string]*AIUsageRow
}

func newAIMeter() *aiMeter { return &aiMeter{rows: map[string]*AIUsageRow{}} }

func (m *aiMeter) add(team, vm, provider, model string, in, out int64) {
	if model == "" {
		model = "unknown"
	}
	if len(model) > 128 {
		model = model[:128]
	}
	k := team + "/" + vm + "/" + provider + "/" + model
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[k]
	if r == nil {
		r = &AIUsageRow{TeamID: team, VMID: vm, Provider: provider, Model: model}
		m.rows[k] = r
	}
	r.Requests++
	r.InputTokens += in
	r.OutputTokens += out
}

func (m *aiMeter) flush(s *server) {
	m.mu.Lock()
	pending := m.rows
	m.rows = map[string]*AIUsageRow{}
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	now := time.Now().UTC()
	err := s.store.Update(func(st *State) error {
		for k, d := range pending {
			cur := st.AIUsage[k]
			if cur == nil {
				cur = &AIUsageRow{TeamID: d.TeamID, VMID: d.VMID, Provider: d.Provider, Model: d.Model}
				st.AIUsage[k] = cur
			}
			cur.Requests += d.Requests
			cur.InputTokens += d.InputTokens
			cur.OutputTokens += d.OutputTokens
			cur.Updated = now
		}
		return nil
	})
	if err != nil {
		log.Printf("ai usage flush: %v", err)
		m.mu.Lock()
		defer m.mu.Unlock()
		for k, d := range pending { // keep the counts for the next flush
			if cur := m.rows[k]; cur != nil {
				cur.Requests += d.Requests
				cur.InputTokens += d.InputTokens
				cur.OutputTokens += d.OutputTokens
			} else {
				m.rows[k] = d
			}
		}
	}
}

func newAIClient() *http.Client {
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second, DisableCompression: true, ResponseHeaderTimeout: 10 * time.Minute}
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

// aiMeterLoop flushes usage until ctx ends.
func (s *server) aiMeterLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.aiMeter.flush(s)
			return
		case <-t.C:
			s.aiMeter.flush(s)
		}
	}
}
