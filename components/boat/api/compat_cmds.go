package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// boat.dev's detached commands, command history and processes on the native
// detached commands (agentops.go). boat.dev names a detached command by an
// integer processId; boat-api gives each recorded command one per VM
// (CommandRecord.ProcessID) and its commandId is the native command id.

// compatDetached starts c in the background (POST …/commands with detached).
func (s *server) compatDetached(w http.ResponseWriter, r *http.Request, p *principal, c compatExecReq) {
	in := map[string]any{"cmd": []string{"bash", "-lc", c.Command}, "cwd": s.compatPath(c.Cwd)}
	res := s.callNative(r, p, "POST /v1/vms/{id}/commands", s.startCommand, r.PathValue("id"), in)
	if !res.ok() {
		res.relay(w)
		return
	}
	var started struct {
		ID string `json:"id"`
	}
	json.Unmarshal(res.body.Bytes(), &started)
	rec := s.commandRecord(r.PathValue("id"), started.ID)
	pid := 0
	if st, err := s.agentOp(r, r.PathValue("id"), "cmd_status", map[string]any{"cmd_id": started.ID}); err == nil {
		f, _ := st["pid"].(float64)
		pid = int(f)
	}
	out := map[string]any{"success": true, "commandId": started.ID, "processId": 0, "pid": pid, "command": c.Command,
		"startedAt": time.Now().UTC()}
	if rec != nil {
		out["processId"], out["startedAt"] = rec.ProcessID, rec.StartedAt
	}
	writeCompat(w, 200, "command.started", out)
}

func (s *server) commandRecord(vmID, cmdID string) *CommandRecord {
	for _, c := range s.store.Snapshot().Commands[vmID] {
		if c.ID == cmdID {
			return &c
		}
	}
	return nil
}

// pathProcess finds the command a {pid} path names (its processId, or its commandId).
func (s *server) pathProcess(w http.ResponseWriter, r *http.Request, name string) (*CommandRecord, bool) {
	v := r.PathValue(name)
	n, _ := strconv.Atoi(v)
	for _, c := range s.store.Snapshot().Commands[r.PathValue("id")] {
		if (n > 0 && c.ProcessID == n) || c.ID == v {
			return &c, true
		}
	}
	writeErr(w, r, 404, "not_found", "command not found")
	return nil, false
}

func commandText(rec *CommandRecord) string {
	if len(rec.Cmd) == 3 && rec.Cmd[0] == "bash" && rec.Cmd[1] == "-lc" {
		return rec.Cmd[2]
	}
	return strings.Join(rec.Cmd, " ")
}

// compatCmdStatus maps the native state onto boat.dev's record status.
func compatCmdStatus(state string, exit *int) string {
	switch state {
	case "running", "lost":
		return state
	case "signaled", "killed", "interrupted":
		return "killed"
	case "timed_out":
		return "timed_out"
	}
	if exit != nil && *exit != 0 {
		return "failed"
	}
	return "exited"
}

func (s *server) commandRecordView(bx string, rec *CommandRecord) map[string]any {
	mode := "detached"
	if rec.Kind == "prompt" || rec.Kind == "setup" {
		mode = "process"
	}
	out := map[string]any{"commandId": rec.ID, "sandboxId": bx, "command": commandText(rec), "cwd": nullIfEmpty(rec.Cwd),
		"mode": mode, "status": compatCmdStatus(rec.State, rec.Exit), "exitCode": rec.Exit, "stdoutBytes": 0, "stderrBytes": 0,
		"outputTruncated": false, "startedAt": rec.StartedAt, "finishedAt": rec.FinishedAt}
	if rec.ProcessID > 0 {
		out["processId"] = rec.ProcessID
	}
	return out
}

func (s *server) compatCommandStatus(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	rec, ok := s.pathProcess(w, r, "pid")
	if !ok {
		return
	}
	id := r.PathValue("id")
	st, err := s.agentOp(r, id, "cmd_status", map[string]any{"cmd_id": rec.ID})
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	v := commandView(st)
	running := v["state"] == "running"
	status := "exited"
	switch {
	case running:
		status = "running"
	case v["state"] == "lost":
		status = "lost"
	}
	var exit any
	success := false
	if f, ok := st["exit"].(float64); ok {
		exit, success = int(f), f == 0
	}
	var stdout string
	var cut bool
	if logs, err := s.readLogs(r, id, rec.ID, 0, compatOutCap+1); err == nil {
		b, _ := base64.StdEncoding.DecodeString(str(logs, "data_b64"))
		if len(b) > compatOutCap {
			b, cut = b[:compatOutCap], true
		}
		stdout = strings.ToValidUTF8(string(b), "�")
	}
	pid, _ := st["pid"].(float64)
	writeCompat(w, 200, "command.status", map[string]any{"success": success, "processId": rec.ProcessID, "pid": int(pid),
		"status": status, "known": true, "running": running, "exitCode": exit, "signal": v["signal"], "command": commandText(rec),
		"cwd": nullIfEmpty(rec.Cwd), "startedAt": rec.StartedAt, "finishedAt": v["finished_at"], "stdout": stdout, "stderr": "",
		"stdoutTruncated": cut, "stderrTruncated": false})
}

func (s *server) compatStopCommand(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	rec, ok := s.pathProcess(w, r, "pid")
	if !ok {
		return
	}
	res := s.callNative(r, p, "DELETE /v1/vms/{id}/commands/{cid}", s.stopCommand, r.PathValue("id"), nil, "cid", rec.ID)
	if !res.ok() {
		res.relay(w)
		return
	}
	if cur := s.commandRecord(r.PathValue("id"), rec.ID); cur != nil {
		rec = cur
	}
	writeCompat(w, 200, "command.stopped", map[string]any{"processId": rec.ProcessID, "running": false, "signaled": true,
		"command": s.commandRecordView(s.compatID(p.Team.ID, r.PathValue("id")), rec)})
}

func (s *server) compatCommandHistory(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "GET /v1/vms/{id}/command-history", s.commandHistory, r.PathValue("id"), nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	var list struct {
		Commands []CommandRecord `json:"commands"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	bx := s.compatID(p.Team.ID, r.PathValue("id"))
	out := []map[string]any{}
	for i := len(list.Commands) - 1; i >= 0; i-- { // newest first
		out = append(out, s.commandRecordView(bx, &list.Commands[i]))
	}
	writeCompat(w, 200, "command.list", map[string]any{"commands": out, "nextCursor": nil})
}

func (s *server) compatCommandInfo(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	rec, ok := s.pathProcess(w, r, "cid")
	if ok {
		writeCompat(w, 200, "command.info", map[string]any{"command": s.commandRecordView(s.compatID(p.Team.ID, r.PathValue("id")), rec)})
	}
}

func (s *server) compatCommandLogs(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	rec, ok := s.pathProcess(w, r, "cid")
	if !ok {
		return
	}
	q := r.URL.Query()
	offset, err := strconv.ParseInt(orDefault(q.Get("offset"), "0"), 10, 64)
	limit, lerr := strconv.Atoi(orDefault(q.Get("limit"), strconv.Itoa(compatOutCap)))
	enc := orDefault(q.Get("encoding"), "utf8")
	if err != nil || lerr != nil || offset < 0 || limit < 1 || limit > 8<<20 || !validEncoding(enc) {
		writeErr(w, r, 400, "invalid_query", "offset >= 0, limit 1-8388608, encoding utf8 or base64")
		return
	}
	stream := orDefault(q.Get("stream"), "stdout")
	if stream == "stderr" { // one combined log here, read as stdout
		writeCompat(w, 200, "command.logs", map[string]any{"commandId": rec.ID, "stream": "stderr", "offset": offset,
			"nextOffset": offset, "size": 0, "encoding": enc, "data": "", "done": rec.State != "running"})
		return
	}
	out, err := s.readLogs(r, r.PathValue("id"), rec.ID, offset, limit)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	b, _ := base64.StdEncoding.DecodeString(str(out, "data_b64"))
	data := strings.ToValidUTF8(string(b), "�")
	if enc == "base64" {
		data = base64.StdEncoding.EncodeToString(b)
	}
	next, _ := out["next_offset"].(float64)
	size, _ := out["size"].(float64)
	writeCompat(w, 200, "command.logs", map[string]any{"commandId": rec.ID, "stream": "stdout", "offset": offset,
		"nextOffset": max(int64(next), offset+int64(len(b))), "size": max(int64(size), offset+int64(len(b))), "encoding": enc,
		"data": data, "done": out["running"] != true})
}

func (s *server) compatProcesses(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "GET /v1/vms/{id}/processes", s.listProcesses, r.PathValue("id"), nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	var list struct {
		Processes []struct {
			PID     int    `json:"pid"`
			PPID    int    `json:"ppid"`
			User    string `json:"user"`
			Command string `json:"command"`
		} `json:"processes"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	procs := []map[string]any{}
	for _, x := range list.Processes {
		procs = append(procs, map[string]any{"pid": x.PID, "parentPid": x.PPID, "user": x.User, "command": x.Command})
	}
	writeCompat(w, 200, "process.list", map[string]any{"live": true, "sampledAt": time.Now().UTC(), "total": len(procs), "processes": procs})
}
