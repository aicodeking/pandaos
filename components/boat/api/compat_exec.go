package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// boat.dev's commands and files on the native exec and file calls.

const (
	compatDefaultTimeout = 30
	compatMaxTimeout     = 600
	compatOutCap         = 1 << 20 // per stream in a synchronous answer; more is cut, flagged *Truncated
	compatMaxFile        = 5 << 20 // boat.dev's inline file limit
)

// compatPath maps boat.dev paths (relative to /home/user) onto the guest's home.
func (s *server) compatPath(p string) string {
	home := "/home/" + orDefault(s.cfg.SSHUser, "dev")
	switch {
	case p == "":
		return home
	case p == "/home/user" || strings.HasPrefix(p, "/home/user/"):
		return home + strings.TrimPrefix(p, "/home/user")
	case strings.HasPrefix(p, "/"):
		return path.Clean(p)
	}
	return path.Join(home, p)
}

type compatExecReq struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds *int   `json:"timeoutSeconds"`
	Detached       bool   `json:"detached"`
	Stream         bool   `json:"stream"`
}

// execFrame is one native exec NDJSON line.
type execFrame struct {
	Stream   string `json:"stream"`
	DataB64  string `json:"data_b64"`
	Exit     *int   `json:"exit"`
	Signal   string `json:"signal"`
	TimedOut bool   `json:"timed_out"`
	Error    string `json:"error"`
}

func (s *server) compatExec(w http.ResponseWriter, r *http.Request, p *principal) {
	var c compatExecReq
	if !decode(w, r, &c) {
		return
	}
	timeout := compatDefaultTimeout
	if c.TimeoutSeconds != nil {
		timeout = *c.TimeoutSeconds
	}
	switch {
	case strings.TrimSpace(c.Command) == "":
		writeErr(w, r, 400, "invalid_request", "command is required")
		return
	case timeout < 1 || timeout > compatMaxTimeout:
		writeErr(w, r, 400, "invalid_timeout", "timeoutSeconds must be 1-600")
		return
	case c.Detached:
		writeErr(w, r, 400, "invalid_request", "detached commands are not mapped here; use POST /v1/vms/{id}/commands")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	cwd := s.compatPath(c.Cwd)
	b, _ := json.Marshal(execReq{Cmd: []string{"bash", "-lc", c.Command}, Cwd: cwd, TimeoutS: timeout})
	resp, err := s.boatd.do(r.Context(), "POST", vmPath(r.PathValue("id"), "exec"), bytes.NewReader(b), "application/json")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	cid := "cmd_" + randHex(8)
	if c.Stream {
		s.compatStreamExec(w, resp.Body, cid)
		return
	}
	s.compatSyncExec(w, resp.Body, cid, cwd)
}

// readFrames calls f for each native frame until the stream ends.
func readFrames(body io.Reader, f func(execFrame)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var fr execFrame
		if len(bytes.TrimSpace(sc.Bytes())) == 0 || json.Unmarshal(sc.Bytes(), &fr) != nil {
			continue
		}
		f(fr)
	}
	return sc.Err()
}

func (s *server) compatSyncExec(w http.ResponseWriter, body io.Reader, cid, cwd string) {
	started := time.Now().UTC()
	var out, errb bytes.Buffer
	var outCut, errCut bool
	var last execFrame
	add := func(buf *bytes.Buffer, cut *bool, data []byte) {
		if room := compatOutCap - buf.Len(); len(data) > room {
			data, *cut = data[:max(room, 0)], true
		}
		buf.Write(data)
	}
	err := readFrames(body, func(fr execFrame) {
		data, _ := base64.StdEncoding.DecodeString(fr.DataB64)
		switch {
		case fr.Stream == "stdout":
			add(&out, &outCut, data)
		case fr.Stream == "stderr":
			add(&errb, &errCut, data)
		case fr.Exit != nil || fr.Error != "":
			last = fr
		}
	})
	if err != nil && last.Exit == nil {
		last.Error = "output stream broke off"
	}
	var exit any
	success := false
	if last.Exit != nil {
		exit, success = *last.Exit, *last.Exit == 0 && !last.TimedOut
	}
	fields := map[string]any{"success": success, "exitCode": exit, "signal": nullIfEmpty(last.Signal), "oomKilled": false,
		"stdout": strings.ToValidUTF8(out.String(), "�"), "stderr": strings.ToValidUTF8(errb.String(), "�"),
		"stdoutTruncated": outCut, "stderrTruncated": errCut, "timedOut": last.TimedOut, "cwd": cwd,
		"startedAt": started, "finishedAt": time.Now().UTC(), "commandId": cid}
	if last.Error != "" {
		fields["error"] = last.Error
	}
	writeCompat(w, 200, "command.finished", fields)
}

// compatStreamExec relays the native frames as boat.dev's stream: started,
// stdout/stderr {data}, then one exit or error.
func (s *server) compatStreamExec(w http.ResponseWriter, body io.Reader, cid string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(v map[string]any) {
		enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"type": "started", "commandId": cid})
	ended := false
	err := readFrames(body, func(fr execFrame) {
		switch {
		case fr.Stream == "stdout" || fr.Stream == "stderr":
			data, _ := base64.StdEncoding.DecodeString(fr.DataB64)
			send(map[string]any{"type": fr.Stream, "data": strings.ToValidUTF8(string(data), "�")})
		case fr.Exit != nil:
			ended = true
			send(map[string]any{"type": "exit", "exitCode": *fr.Exit, "success": *fr.Exit == 0 && !fr.TimedOut, "timedOut": fr.TimedOut})
		case fr.Error != "":
			ended = true
			send(map[string]any{"type": "error", "error": "command_failed", "message": fr.Error, "retryable": false})
		}
	})
	if !ended {
		msg := "the command's output ended without an exit status"
		if err != nil {
			msg = err.Error()
		}
		send(map[string]any{"type": "error", "error": "stream_broken", "message": msg, "retryable": true})
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- files ----

type compatFileReq struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func validEncoding(e string) bool { return e == "" || e == "utf8" || e == "base64" }

func (s *server) compatWriteFile(w http.ResponseWriter, r *http.Request, p *principal) {
	var c compatFileReq
	// base64 of 5 MiB plus the JSON around it: more than decode's 1 MiB.
	r.Body = http.MaxBytesReader(w, r.Body, compatMaxFile*4/3+(64<<10))
	if decodeHook != nil {
		decodeHook(r, &c)
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErrDetails(w, r, 413, "file_too_large", "file is over 5 MiB; use the native PUT /v1/vms/{id}/files", map[string]any{"maxBytes": compatMaxFile})
			return
		}
		writeErr(w, r, 400, "invalid_json", err.Error())
		return
	}
	if c.Path == "" || !validEncoding(c.Encoding) {
		writeErr(w, r, 400, "invalid_path", "path is required and encoding must be utf8 or base64")
		return
	}
	data := []byte(c.Content)
	if c.Encoding == "base64" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(c.Content); err != nil {
			writeErr(w, r, 400, "invalid_request", "content is not valid base64")
			return
		}
	}
	if len(data) > compatMaxFile {
		writeErrDetails(w, r, 413, "file_too_large", "file is over 5 MiB", map[string]any{"maxBytes": compatMaxFile, "sizeBytes": len(data)})
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	dst := s.compatPath(c.Path)
	if err := s.putVMFile(r, r.PathValue("id"), dst, bytes.NewReader(data)); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeCompat(w, 200, "file.written", map[string]any{"success": true, "path": dst,
		"encoding": orDefault(c.Encoding, "utf8"), "size": len(data)})
}

func (s *server) compatReadFile(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	enc := q.Get("encoding")
	if q.Get("path") == "" || !validEncoding(enc) {
		writeErr(w, r, 400, "invalid_path", "path is required and encoding must be utf8 or base64")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	src := s.compatPath(q.Get("path"))
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(r.PathValue("id"), "files")+"?path="+url.QueryEscape(src), nil, "")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == 404 {
			writeErr(w, r, 404, "not_found", "file not found")
			return
		}
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, compatMaxFile+1))
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	if len(data) > compatMaxFile {
		writeErrDetails(w, r, 413, "file_too_large", "file is over 5 MiB; use the native GET /v1/vms/{id}/files", map[string]any{"maxBytes": compatMaxFile})
		return
	}
	content := string(data)
	if enc == "base64" {
		content = base64.StdEncoding.EncodeToString(data)
	}
	writeCompat(w, 200, "file.read", map[string]any{"success": true, "path": src, "encoding": orDefault(enc, "utf8"),
		"size": len(data), "content": content})
}
