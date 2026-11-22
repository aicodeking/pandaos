package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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

// validCwd accepts empty (unset), absolute ("/x"), home-relative ("~" or "~/x"),
// or plain relative ("subdir") paths; the guest resolves non-absolute forms
// against the target user's home. Reject NUL, control characters, and overly
// long or invalid UTF-8 strings.
func validCwd(c string) bool {
	if c == "" {
		return true
	}
	if len(c) >= 4096 || !utf8.ValidString(c) {
		return false
	}
	for _, r := range c {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type startCmd struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env"`
	Cwd string            `json:"cwd"`
}

// startDetached starts argv on the VM, records it in the history, and returns
// the command id and guest pid (0 if an older guest omitted pid).
func (s *server) startDetached(r *http.Request, p *principal, vmID, kind string, cmd []string, env map[string]string, cwd string) (string, int, error) {
	args := map[string]any{"argv": cmd}
	if len(env) > 0 {
		args["env"] = env
	}
	if cwd != "" {
		args["cwd"] = cwd
	}
	out, err := s.agentOp(r, vmID, "cmd_start", args)
	if err != nil {
		return "", 0, err
	}
	id := str(out, "cmd_id")
	if id == "" {
		return "", 0, fmt.Errorf("agent cmd_start returned no cmd_id")
	}
	pid := 0
	if f, ok := out["pid"].(float64); ok && f > 0 {
		pid = int(f)
	}
	// Prompts hold secrets in argv only as the user's text; env is never stored.
	rec := CommandRecord{ID: id, Cmd: capArgv(cmd), Cwd: preview(cwd), Kind: kind, StartedAt: time.Now().UTC(), State: "running"}
	if p != nil {
		rec.KeyID = p.Key.ID
	}
	if err := s.recordCommand(vmID, rec); err != nil {
		log.Printf("record command %s: %v", id, err)
	}
	return id, pid, nil
}

// recordCommand appends rec to the VM's capped command history. The history is
// display-only bookkeeping (the command itself already runs on the guest), so
// it is written lazily like idempotency records: readers see it at once, and
// writes in the idemPersistDelay window share one disk write instead of each
// command waiting on a seal and two fsyncs. Only this VM's history is copied
// under the store's writer mutex. Crash tradeoff: a crash inside that window
// loses the newest history rows (the commands still ran; the guest agent still
// lists them by id).
// Graceful shutdown and any durable Update in between persist them.
func (s *server) recordCommand(vmID string, rec CommandRecord) error {
	rec = cloneCommandRecord(rec)
	return s.store.applyCommandHistoryLazy(vmID, func(st *State) error {
		st.CommandSeq[vmID]++
		rec.ProcessID = st.CommandSeq[vmID]
		list := append(st.Commands[vmID], rec)
		if len(list) > maxCommandRecords {
			list = list[len(list)-maxCommandRecords:]
		}
		st.Commands[vmID] = list
		return nil
	}, idemPersistDelay)
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
		msg = "invalid cwd"
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id, pid, err := s.startDetached(r, p, r.PathValue("id"), "command", c.Cmd, c.Env, c.Cwd)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	resp := map[string]any{"id": id, "cmd": c.Cmd, "state": "running"}
	if pid > 0 {
		resp["pid"] = pid
	}
	writeJSON(w, 201, resp)
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
	// Lazy: the states are re-derived from the guest on the next history read.
	err := s.store.applyCommandHistoryLazy(vmID, func(st *State) error {
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
	}, idemPersistDelay)
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
	if out["running"] == true {
		out["exit"] = nil
		return out, nil
	}
	// Current guests answer cmd_logs with exit and signal (null when not
	// applicable). An older guest omits the keys; only then ask cmd_status.
	if _, ok := out["exit"]; !ok {
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
		if next, ok := out["next_offset"].(float64); ok && next >= 0 {
			offset = int64(next)
		}
		data := str(out, "data_b64")
		if data != "" {
			stream := str(out, "stream")
			if stream == "" {
				stream = "stdout"
			}
			if !emit(map[string]any{"stream": stream, "data_b64": data, "next_offset": offset}) {
				return
			}
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
	typ := statFileType(f[0])
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
	if s.refuseSelfLifetime(w, r, p) {
		return
	}
	if _, ok := s.ownVM(w, r, p); ok {
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
			return
		}
		s.vmAction(w, r, "resize", map[string]any{"size": body.Size, "disk_gib": body.DiskGiB, "discard_memory": body.DiscardMemory})
	}
}

// parseUsageBound reads a usage bound: RFC 3339 or unix seconds.
func parseUsageBound(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > 253402300799 {
		return time.Time{}, errors.New("bad time")
	}
	return time.Unix(n, 0).UTC(), nil
}

// vmUsage reads one exact team+VM interval snapshot instead of subtracting two
// rounded fleet reports. Each interval retains the size used during that run.
func (s *server) vmUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var since, until time.Time
	for _, b := range []struct {
		name string
		dst  *time.Time
	}{{"since", &since}, {"until", &until}} {
		if v := r.URL.Query().Get(b.name); v != "" {
			t, err := parseUsageBound(v)
			if err != nil {
				writeErr(w, r, 400, "invalid_request", b.name+" must be RFC 3339 or unix seconds")
				return
			}
			*b.dst = t
		}
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		writeErr(w, r, 400, "invalid_request", "until must be after since")
		return
	}
	q := url.Values{"team": {p.Team.ID}, "vm_id": {id}}
	if !since.IsZero() {
		q.Set("since", since.Format(time.RFC3339Nano))
	}
	if !until.IsZero() {
		q.Set("until", until.Format(time.RFC3339Nano))
	}
	var rep struct {
		Exact     bool      `json:"exact"`
		Since     time.Time `json:"since"`
		Until     time.Time `json:"until"`
		Intervals []struct {
			ID    string    `json:"id"`
			Team  string    `json:"team"`
			Size  string    `json:"size"`
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"intervals"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/usage?"+q.Encode(), nil, &rep); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if !rep.Exact {
		writeErr(w, r, 501, "not_supported", "exact per-VM usage requires a boatd with usage-window support")
		return
	}
	state := s.store.Snapshot()
	cutoff := state.EnvErrorCutoffs[id]
	if cutoff.Before(state.EnvErrorLegacyBefore) {
		cutoff = state.EnvErrorLegacyBefore
	}
	spans := append([]EnvErrorSpan(nil), state.EnvErrorSpans[id]...)
	look := func(_ context.Context, _ string) (VM, bool) { return vm, true }
	for i := range spans {
		if spans[i].End.IsZero() {
			spans[i].End = s.openSpanEnd(r.Context(), look, id, spans[i].Start, rep.Until)
		}
	}
	seconds := 0.0
	bySize := map[string]float64{}
	for _, interval := range rep.Intervals {
		if interval.ID != id || interval.Team != p.Team.ID {
			continue
		}
		start, end := interval.Start, interval.End
		if start.Before(since) {
			start = since
		}
		if end.After(rep.Until) {
			end = rep.Until
		}
		if !end.After(start) {
			continue
		}
		if start.Before(cutoff) {
			writeErr(w, r, 501, "not_supported", "exact per-VM usage is unavailable for incomplete environment-error history")
			return
		}
		billable := end.Sub(start).Seconds()
		for _, span := range spans {
			lo, hi := span.Start, span.End
			if lo.Before(start) {
				lo = start
			}
			if hi.After(end) {
				hi = end
			}
			if hi.After(lo) {
				billable -= hi.Sub(lo).Seconds()
			}
		}
		billable = math.Max(0, billable)
		seconds += billable
		bySize[interval.Size] += billable
	}
	out := map[string]any{"id": id, "vm_seconds": seconds, "vm_seconds_by_size": bySize, "until": rep.Until}
	if !since.IsZero() {
		out["since"] = since
	}

	writeJSON(w, 200, out)
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

// statFileType maps `stat -c %F` ("regular file", "directory", "symbolic link")
// onto the FileEntry type enum (file, dir, symlink).
func statFileType(f string) string {
	switch {
	case strings.Contains(f, "directory"):
		return "dir"
	case strings.Contains(f, "link"):
		return "symlink"
	}
	return "file"
}
