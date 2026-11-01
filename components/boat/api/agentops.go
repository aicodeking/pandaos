package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Detached commands and file operations, passed through boatd's
// POST /v1/vms/{id}/agent to the guest agent.

const maxCommandRecords = 200

var (
	reCmdID   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	signals   = []string{"TERM", "INT", "KILL", "HUP"}
	followGap = 500 * time.Millisecond
)

// agentOp runs one guest op through boatd and returns its final reply.
func (s *server) agentOp(r *http.Request, vmID, op string, args map[string]any) (map[string]any, error) {
	in := map[string]any{"op": op}
	for k, v := range args {
		in[k] = v
	}
	out := map[string]any{}
	if err := s.boatd.call(r.Context(), "POST", vmPath(vmID, "agent"), in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// agentReply drops the agent's own "ok" so public shapes stay ours.
func agentReply(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != "ok" {
			out[k] = v
		}
	}
	return out
}

// commandView maps the guest's status object to the public shape.
func commandView(g map[string]any) map[string]any {
	state := str(g, "state")
	switch {
	case g["running"] == true:
		state = "running"
	case state != "lost" && g["signal"] != nil:
		state = "signaled"
	case state == "":
		state = "exited"
	}
	id := str(g, "cmd_id")
	cmd := g["argv"]
	if cmd == nil {
		cmd = []any{}
	}
	return map[string]any{"id": id, "cmd": cmd, "state": state, "exit": g["exit"], "signal": g["signal"],
		"pid": g["pid"], "started_at": epochTime(g["started"]), "finished_at": epochTime(g["ended"])}
}

// epochTime turns the guest's epoch seconds into RFC 3339, or null.
func epochTime(v any) any {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
}

func validEnv(env map[string]string) string {
	for k := range env {
		if !reEnvName.MatchString(k) {
			return "invalid env name " + k
		}
	}
	return ""
}

func validCwd(c string) bool { return c == "" || validFilePath(c) }

type startCmd struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env"`
	Cwd string            `json:"cwd"`
}

// startDetached starts argv on the VM and records it in the history.
func (s *server) startDetached(r *http.Request, p *principal, vmID, kind string, cmd []string, env map[string]string, cwd string) (string, error) {
	args := map[string]any{"argv": cmd}
	if len(env) > 0 {
		args["env"] = env
	}
	if cwd != "" {
		args["cwd"] = cwd
	}
	out, err := s.agentOp(r, vmID, "cmd_start", args)
	if err != nil {
		return "", err
	}
	id := str(out, "cmd_id")
	if id == "" {
		return "", fmt.Errorf("agent cmd_start returned no cmd_id")
	}
	// Prompts hold secrets in argv only as the user's text; env is never stored.
	rec := CommandRecord{ID: id, Cmd: cmd, Cwd: cwd, Kind: kind, StartedAt: time.Now().UTC(), State: "running"}
	if p != nil {
		rec.KeyID = p.Key.ID
	}
	if err := s.recordCommand(vmID, rec); err != nil {
		log.Printf("record command %s: %v", id, err)
	}
	return id, nil
}

func (s *server) recordCommand(vmID string, rec CommandRecord) error {
	return s.store.Update(func(st *State) error {
		list := append(st.Commands[vmID], rec)
		if len(list) > maxCommandRecords {
			list = list[len(list)-maxCommandRecords:]
		}
		st.Commands[vmID] = list
		return nil
	})
}

func (s *server) startCommand(w http.ResponseWriter, r *http.Request, p *principal) {
	var c startCmd
	if !decode(w, r, &c) {
		return
	}
	msg := validEnv(c.Env)
	switch {
	case len(c.Cmd) == 0 || c.Cmd[0] == "":
		msg = "cmd is required"
	case !validCwd(c.Cwd):
		msg = "cwd must be absolute"
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id, err := s.startDetached(r, p, r.PathValue("id"), "command", c.Cmd, c.Env, c.Cwd)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "cmd": c.Cmd, "state": "running"})
}

func (s *server) listCommands(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	out, err := s.agentOp(r, r.PathValue("id"), "cmd_list", nil)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	list := []map[string]any{}
	raw, _ := out["cmds"].([]any)
	for _, x := range raw {
		if g, ok := x.(map[string]any); ok {
			list = append(list, commandView(g))
		}
	}
	writeJSON(w, 200, map[string]any{"commands": list})
}

func pathCmd(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("cid")
	if !reCmdID.MatchString(id) {
		writeErr(w, r, 404, "not_found", "command not found")
		return "", false
	}
	return id, true
}

func (s *server) commandStatus(w http.ResponseWriter, r *http.Request, p *principal) {
	cid, ok := pathCmd(w, r)
	if !ok {
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	out, err := s.agentOp(r, r.PathValue("id"), "cmd_status", map[string]any{"cmd_id": cid})
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, commandView(out))
}

// commandHistory is boat-api's own record, so it survives a stopped VM. When the
// VM answers, finished states are refreshed from the guest.
func (s *server) commandHistory(w http.ResponseWriter, r *http.Request, p *principal) {
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if str(vm, "state") == "running" {
		if out, err := s.agentOp(r, id, "cmd_list", nil); err == nil {
			s.refreshHistory(id, out)
		}
	}
	recs := s.store.Snapshot().Commands[id]
	if recs == nil {
		recs = []CommandRecord{}
	}
	writeJSON(w, 200, map[string]any{"commands": recs})
}

func (s *server) refreshHistory(vmID string, list map[string]any) {
	raw, _ := list["cmds"].([]any)
	live := map[string]map[string]any{}
	for _, x := range raw {
		if g, ok := x.(map[string]any); ok {
			v := commandView(g)
			live[str(v, "id")] = v
		}
	}
	changed := false
	for _, rec := range s.store.Snapshot().Commands[vmID] {
		if v := live[rec.ID]; v != nil && str(v, "state") != rec.State {
			changed = true
		}
	}
	if !changed {
		return
	}
	err := s.store.Update(func(st *State) error {
		for i := range st.Commands[vmID] {
			rec := &st.Commands[vmID][i]
			v := live[rec.ID]
			if v == nil {
				continue
			}
			rec.State = str(v, "state")
			if f, ok := v["exit"].(float64); ok {
				n := int(f)
				rec.Exit = &n
			}
			if rec.State != "running" && rec.FinishedAt == nil {
				now := time.Now().UTC()
				rec.FinishedAt = &now
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("refresh command history: %v", err)
	}
}

// commandLogs returns one chunk, or with ?follow=1 streams NDJSON until the
// command ends: {"stream","data_b64","next_offset"}… then {"exit",...}.
func (s *server) commandLogs(w http.ResponseWriter, r *http.Request, p *principal) {
	cid, ok := pathCmd(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(orDefault(r.URL.Query().Get("offset"), "0"), 10, 64)
	limit, lerr := strconv.Atoi(orDefault(r.URL.Query().Get("limit"), "0"))
	if err != nil || offset < 0 || lerr != nil || limit < 0 || limit > 8<<20 {
		writeErr(w, r, 400, "invalid_request", "offset and limit must be non-negative integers (limit max 8 MiB)")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	if r.URL.Query().Get("follow") == "1" {
		s.followLogs(w, r, id, cid, offset)
		return
	}
	out, err := s.readLogs(r, id, cid, offset, limit)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *server) readLogs(r *http.Request, vmID, cid string, offset int64, limit int) (map[string]any, error) {
	args := map[string]any{"cmd_id": cid, "offset": offset}
	if limit > 0 {
		args["limit"] = limit
	}
	out, err := s.agentOp(r, vmID, "cmd_logs", args)
	if err != nil {
		return nil, err
	}
	out = agentReply(out)
	out["exit"] = nil
	if out["running"] != true { // cmd_logs has no exit status; cmd_status does
		st, err := s.agentOp(r, vmID, "cmd_status", map[string]any{"cmd_id": cid})
		if err != nil {
			return nil, err
		}
		out["exit"], out["signal"] = st["exit"], st["signal"]
	}
	return out, nil
}

// followLogs polls cmd_logs until the command ends or the client leaves. A
// {"reconnect":true,"next_offset":N} line ends the stream on a transient
// agent error (a VM stop, for example), so a client can resume with no gap.
func (s *server) followLogs(w http.ResponseWriter, r *http.Request, vmID, cid string, offset int64) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	emit := func(v any) bool {
		if enc.Encode(v) != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for {
		out, err := s.readLogs(r, vmID, cid, offset, 0)
		if err != nil {
			if r.Context().Err() == nil {
				log.Printf("%s: follow %s/%s: %v", info(r).id, vmID, cid, err)
				emit(map[string]any{"reconnect": true, "next_offset": offset})
			}
			return
		}
		next, _ := out["next_offset"].(float64)
		data := str(out, "data_b64")
		if data != "" {
			stream := str(out, "stream")
			if stream == "" {
				stream = "stdout"
			}
			if !emit(map[string]any{"stream": stream, "data_b64": data, "next_offset": int64(next)}) {
				return
			}
		}
		if next > 0 {
			offset = int64(next)
		}
		if out["running"] != true && data == "" {
			last := map[string]any{"exit": out["exit"]}
			if sig := out["signal"]; sig != nil {
				last["signal"] = sig
			}
			emit(last)
			return
		}
		if data != "" {
			continue // more may be waiting
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(followGap):
		}
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (s *server) signalCommand(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Signal string `json:"signal"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.sendSignal(w, r, p, strings.TrimPrefix(strings.ToUpper(orDefault(body.Signal, "TERM")), "SIG"))
}

// stopCommand is DELETE on a command: SIGTERM.
func (s *server) stopCommand(w http.ResponseWriter, r *http.Request, p *principal) {
	s.sendSignal(w, r, p, "TERM")
}

func (s *server) sendSignal(w http.ResponseWriter, r *http.Request, p *principal, sig string) {
	cid, ok := pathCmd(w, r)
	if !ok {
		return
	}
	if !slices.Contains(signals, sig) {
		writeErr(w, r, 400, "invalid_request", "signal must be one of "+strings.Join(signals, ", "))
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	if _, err := s.agentOp(r, r.PathValue("id"), "cmd_signal", map[string]any{"cmd_id": cid, "signal": sig}); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": cid, "signal": sig})
}

// ---- files ----

// filePathArg reads and checks ?path=.
func filePathArg(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if !validFilePath(path) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute")
		return "", false
	}
	return path, true
}

func (s *server) listFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	path, ok := filePathArg(w, r)
	if !ok {
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	out, err := s.agentOp(r, r.PathValue("id"), "file_list", map[string]any{"path": path})
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	entries := out["entries"]
	if entries == nil {
		entries = []any{}
	}
	writeJSON(w, 200, map[string]any{"path": path, "entries": entries, "truncated": out["truncated"] == true})
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request, p *principal) {
	path, ok := filePathArg(w, r)
	if !ok {
		return
	}
	if strings.Trim(path, "/") == "" {
		writeErr(w, r, 400, "invalid_request", "refusing to delete /")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	args := map[string]any{"path": path, "recursive": r.URL.Query().Get("recursive") == "1"}
	if _, err := s.agentOp(r, r.PathValue("id"), "file_delete", args); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": path})
}

func (s *server) moveFile(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validFilePath(body.From) || !validFilePath(body.To) {
		writeErr(w, r, 400, "invalid_request", "from and to must be absolute paths")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	if _, err := s.agentOp(r, r.PathValue("id"), "file_move", map[string]any{"from": body.From, "to": body.To, "overwrite": body.Overwrite}); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "from": body.From, "to": body.To})
}

func (s *server) searchFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	path, ok := filePathArg(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	query := q.Get("q")
	limit, err := strconv.Atoi(orDefault(q.Get("limit"), "100"))
	if query == "" || len(query) > 1024 || err != nil || limit < 1 || limit > 10000 {
		writeErr(w, r, 400, "invalid_request", "q is required (max 1024 chars); limit must be 1..10000")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	out, aerr := s.agentOp(r, r.PathValue("id"), "file_search",
		map[string]any{"path": path, "query": query, "limit": limit, "content": q.Get("content") == "1", "regex": q.Get("regex") == "1"})
	if aerr != nil {
		writeUpstream(w, r, aerr)
		return
	}
	matches := out["matches"]
	if matches == nil {
		matches = []any{}
	}
	writeJSON(w, 200, map[string]any{"matches": matches, "truncated": out["truncated"] == true})
}

// execCapture runs argv through boatd's exec and collects its output.
func (s *server) execCapture(r *http.Request, vmID string, argv []string, timeoutS int) (stdout, stderr string, exit int, err error) {
	b, _ := json.Marshal(execReq{Cmd: argv, TimeoutS: timeoutS})
	resp, err := s.boatd.do(r.Context(), "POST", vmPath(vmID, "exec"), bytes.NewReader(b), "application/json")
	if err != nil {
		return "", "", -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return "", "", -1, &boatdError{Status: resp.StatusCode, Body: body}
	}
	return readExecStream(resp.Body)
}

// readExecStream collects an exec NDJSON stream (64 MiB of output at most).
func readExecStream(rd io.Reader) (stdout, stderr string, exit int, err error) {
	var out, errb strings.Builder
	exit = -1
	dec := json.NewDecoder(io.LimitReader(rd, 64<<20))
	for {
		var ev struct {
			Stream  string  `json:"stream"`
			DataB64 string  `json:"data_b64"`
			Exit    *int    `json:"exit"`
			Error   *string `json:"error"`
		}
		if derr := dec.Decode(&ev); derr != nil {
			if derr == io.EOF {
				break
			}
			return out.String(), errb.String(), exit, derr
		}
		if ev.Error != nil {
			return out.String(), errb.String(), exit, fmt.Errorf("guest: %s", *ev.Error)
		}
		data, _ := base64.StdEncoding.DecodeString(ev.DataB64)
		if ev.Stream == "stderr" {
			errb.Write(data)
		} else {
			out.Write(data)
		}
		if ev.Exit != nil {
			exit = *ev.Exit
		}
	}
	return out.String(), errb.String(), exit, nil
}

// runSimple answers with {ok,path} when argv exits 0, or 400 with its stderr.
func (s *server) runSimple(w http.ResponseWriter, r *http.Request, argv []string, ok map[string]any) {
	_, stderr, exit, err := s.execCapture(r, r.PathValue("id"), argv, 60)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	if exit != 0 {
		writeErr(w, r, 400, "command_failed", strings.TrimSpace(stderr))
		return
	}
	writeJSON(w, 200, ok)
}

// makeFolder is mkdir -p in the VM.
func (s *server) makeFolder(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validFilePath(body.Path) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute")
		return
	}
	if _, ok := s.ownVM(w, r, p); ok {
		s.runSimple(w, r, []string{"mkdir", "-p", "--", body.Path}, map[string]any{"ok": true, "path": body.Path})
	}
}

var reFileMode = regexp.MustCompile(`^[0-7]{3,4}$`)

// chmodFile is chmod in the VM.
func (s *server) chmodFile(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validFilePath(body.Path) || !reFileMode.MatchString(body.Mode) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute and mode octal, e.g. 644")
		return
	}
	if _, ok := s.ownVM(w, r, p); ok {
		s.runSimple(w, r, []string{"chmod", "--", body.Mode, body.Path}, map[string]any{"ok": true, "path": body.Path, "mode": body.Mode})
	}
}

// fileInfo is stat in the VM.
func (s *server) fileInfo(w http.ResponseWriter, r *http.Request, p *principal) {
	path, ok := filePathArg(w, r)
	if !ok {
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	stdout, _, exit, err := s.execCapture(r, r.PathValue("id"), []string{"stat", "-c", "%F|%s|%a|%Y", "--", path}, 30)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	f := strings.Split(strings.TrimSpace(stdout), "|")
	if exit != 0 || len(f) != 4 {
		writeErr(w, r, 404, "not_found", "file not found")
		return
	}
	typ := "file"
	switch {
	case strings.Contains(f[0], "directory"):
		typ = "dir"
	case strings.Contains(f[0], "link"):
		typ = "link"
	}
	size, _ := strconv.ParseInt(f[1], 10, 64)
	mtime, _ := strconv.ParseInt(f[3], 10, 64)
	writeJSON(w, 200, map[string]any{"path": path, "type": typ, "size": size, "mode": f[2], "mtime": mtime})
}

// listProcesses is ps in the VM.
func (s *server) listProcesses(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	stdout, stderr, exit, err := s.execCapture(r, r.PathValue("id"),
		[]string{"ps", "-eo", "pid=,ppid=,user=,etimes=,args="}, 30)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	if exit != 0 {
		writeErr(w, r, 502, "command_failed", strings.TrimSpace(stderr))
		return
	}
	writeJSON(w, 200, map[string]any{"processes": parsePS(stdout)})
}

func parsePS(out string) []map[string]any {
	list := []map[string]any{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		secs, _ := strconv.Atoi(f[3])
		list = append(list, map[string]any{"pid": pid, "ppid": ppid, "user": f[2], "elapsed_s": secs, "command": strings.Join(f[4:], " ")})
	}
	return list
}

// resizeVM changes size or grows the disk. A vCPU/RAM change needs a stopped VM,
// and discard_memory if it was stopped with a memory snapshot (which cannot
// change shape; the next start is then a cold boot).
func (s *server) resizeVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Size          string `json:"size"`
		DiskGiB       int    `json:"disk_gib"`
		DiscardMemory bool   `json:"discard_memory"`
	}
	if !decode(w, r, &body) {
		return
	}
	if (body.Size != "" && !reVMName.MatchString(body.Size)) || body.DiskGiB < 0 || (body.Size == "" && body.DiskGiB == 0) {
		writeErr(w, r, 400, "invalid_request", "give a size and/or disk_gib")
		return
	}
	if _, ok := s.ownVM(w, r, p); ok {
		s.vmAction(w, r, "resize", map[string]any{"size": body.Size, "disk_gib": body.DiskGiB, "discard_memory": body.DiscardMemory})
	}
}

// vmUsage narrows boatd's usage report to one VM.
func (s *server) vmUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	var rep struct {
		VMs []map[string]any `json:"vms"`
	}
	q := url.Values{"team": {p.Team.ID}}
	if err := s.boatd.call(r.Context(), "GET", "/v1/usage?"+q.Encode(), nil, &rep); err != nil {
		writeUpstream(w, r, err)
		return
	}
	var seconds float64
	for _, u := range rep.VMs {
		if str(u, "id") == id {
			f, _ := u["vm_seconds"].(float64)
			seconds += f
		}
	}
	writeJSON(w, 200, map[string]any{"id": id, "vm_seconds": seconds})
}

// streamEvents relays boatd's SSE lifecycle events, filtered to the caller's team.
func (s *server) streamEvents(w http.ResponseWriter, r *http.Request, p *principal) {
	q := url.Values{"team": {p.Team.ID}}
	if a := orDefault(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")); a != "" {
		if _, err := strconv.ParseInt(a, 10, 64); err != nil {
			writeErr(w, r, 400, "invalid_request", "after must be an event sequence number")
			return
		}
		q.Set("after", a)
	}
	resp, err := s.boatd.do(r.Context(), "GET", "/v1/events?"+q.Encode(), nil, "")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush() // events may be minutes away; the client should see the stream open
	}
	buf := make([]byte, 16<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
