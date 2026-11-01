package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Internal API on /run/boat/boatd.sock. Success bodies are the object itself;
// errors are {"error": "..."} with a 4xx/5xx status. See ../PROTOCOL.md.

const fileChunk = 1 << 20

var reMode = regexp.MustCompile(`^[0-7]{3,4}$`)

func (m *Manager) routes() *http.ServeMux {
	mux := http.NewServeMux()
	h := func(pattern string, f func(*http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			v, err := f(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			code := http.StatusOK
			if r.Method == "POST" && (strings.HasSuffix(pattern, "/v1/vms") || strings.HasSuffix(pattern, "/fork") ||
				strings.HasSuffix(pattern, "snapshots") || strings.HasSuffix(pattern, "/ports")) {
				code = http.StatusCreated
			}
			writeJSON(w, code, v)
		})
	}
	h("GET /v1/health", func(*http.Request) (any, error) { return map[string]bool{"ok": true}, nil })
	h("POST /v1/vms", func(r *http.Request) (any, error) {
		var req CreateReq
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return m.Create(r.Context(), req)
	})
	h("GET /v1/vms", func(r *http.Request) (any, error) {
		return map[string]any{"vms": m.list(r.URL.Query().Get("team"))}, nil
	})
	h("GET /v1/vms/{id}", func(r *http.Request) (any, error) { return m.get(r.PathValue("id")) })
	del := func(r *http.Request) (any, error) { return m.Delete(r.Context(), r.PathValue("id")) }
	h("DELETE /v1/vms/{id}", del)
	h("POST /v1/vms/{id}/delete", del)
	h("POST /v1/vms/{id}/stop", func(r *http.Request) (any, error) { return m.Stop(r.Context(), r.PathValue("id"), "api") })
	h("POST /v1/vms/{id}/start", func(r *http.Request) (any, error) {
		var b struct {
			Env map[string]string `json:"env"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return m.Start(r.Context(), r.PathValue("id"), b.Env)
	})
	h("POST /v1/vms/{id}/fork", func(r *http.Request) (any, error) {
		var b struct{ Name, Team string }
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return m.Fork(r.Context(), r.PathValue("id"), b.Name, b.Team)
	})
	h("POST /v1/vms/{id}/env", func(r *http.Request) (any, error) {
		var b struct {
			Vars map[string]string `json:"vars"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		if err := validEnv(b.Vars); err != nil {
			return nil, err
		}
		return m.agentPass(r, map[string]any{"op": "set_env", "vars": b.Vars})
	})
	h("POST /v1/vms/{id}/desktop", func(r *http.Request) (any, error) {
		var b struct{ Action, Token string }
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		req := map[string]any{"op": "desktop", "action": b.Action}
		if b.Token != "" {
			req["token"] = b.Token
		}
		return m.agentPass(r, req)
	})
	h("POST /v1/vms/{id}/ports", func(r *http.Request) (any, error) {
		var b struct {
			Port      int  `json:"port"`
			GuestPort int  `json:"guest_port"`
			Public    bool `json:"public"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return m.AddPort(r.PathValue("id"), max(b.Port, b.GuestPort), b.Public)
	})
	h("DELETE /v1/vms/{id}/ports", func(r *http.Request) (any, error) {
		q := r.URL.Query().Get("port")
		if q == "" {
			q = r.URL.Query().Get("guest_port")
		}
		port, err := strconv.Atoi(q)
		if err != nil {
			return nil, errf(400, "?port= required")
		}
		return map[string]bool{"ok": true}, m.RemovePort(r.PathValue("id"), port)
	})
	h("GET /v1/vms/{id}/snapshots", func(r *http.Request) (any, error) {
		if _, err := m.get(r.PathValue("id")); err != nil {
			return nil, err
		}
		return map[string]any{"snapshots": m.Snapshots(r.PathValue("id"))}, nil
	})
	h("POST /v1/vms/{id}/snapshots", func(r *http.Request) (any, error) {
		var b struct{ Name string }
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return m.CreateSnapshot(r.Context(), r.PathValue("id"), b.Name)
	})
	h("DELETE /v1/vms/{id}/snapshots/{sid}", func(r *http.Request) (any, error) {
		return map[string]bool{"ok": true}, m.DeleteSnapshot(r.PathValue("id"), r.PathValue("sid"))
	})
	h("POST /v1/snapshots", func(r *http.Request) (any, error) {
		var b struct {
			VMID string `json:"vm_id"`
			Name string `json:"name"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return m.CreateSnapshot(r.Context(), b.VMID, b.Name)
	})
	h("DELETE /v1/snapshots/{sid}", func(r *http.Request) (any, error) {
		m.mu.Lock()
		s := m.snaps[r.PathValue("sid")]
		m.mu.Unlock()
		return map[string]bool{"ok": true}, m.DeleteSnapshot(s.VMID, r.PathValue("sid"))
	})
	h("GET /v1/usage", func(r *http.Request) (any, error) { return m.usageHandler(r) })
	mux.HandleFunc("POST /v1/vms/{id}/exec", m.execHandler)
	mux.HandleFunc("GET /v1/vms/{id}/files", m.getFileHandler)
	mux.HandleFunc("PUT /v1/vms/{id}/files", m.putFileHandler)
	mux.HandleFunc("GET /v1/events", m.eventsHandler)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiErr
	var ge *agentErr
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, &ae):
		code = ae.code
	case errors.As(err, &ge) && ge.notFound():
		code = http.StatusNotFound
	case errors.As(err, &ge):
		code = http.StatusBadGateway
	default:
		log.Printf("api: %v", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// decode reads an optional JSON body (empty = zero value).
func decode(r *http.Request, v any) error {
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		return errf(400, "bad json: %v", err)
	}
	return nil
}

func (m *Manager) agentPass(r *http.Request, req any) (any, error) {
	sock, err := m.sock(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	raw, _, err := agentRaw(r.Context(), sock, req)
	return raw, err
}

func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) < 4096 && !strings.ContainsRune(p, 0)
}

type execReq struct {
	Argv     []string          `json:"argv"`
	Cmd      []string          `json:"cmd"` // alias used by boat-api
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
	TTY      bool              `json:"tty,omitempty"`
	User     string            `json:"user,omitempty"`
}

// execHandler streams the guest's exec reply lines verbatim as NDJSON.
func (m *Manager) execHandler(w http.ResponseWriter, r *http.Request) {
	var e execReq
	if err := decode(r, &e); err != nil {
		writeErr(w, err)
		return
	}
	if len(e.Argv) == 0 {
		e.Argv = e.Cmd
	}
	if e.TimeoutS <= 0 {
		e.TimeoutS = 600
	}
	if len(e.Argv) == 0 {
		writeErr(w, errf(400, "argv required"))
		return
	}
	if err := validEnv(e.Env); err != nil {
		writeErr(w, err)
		return
	}
	sock, err := m.sock(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(e.TimeoutS+30)*time.Second)
	defer cancel()
	a, err := dialAgent(ctx, sock)
	if err != nil {
		writeErr(w, errf(http.StatusBadGateway, "guest agent: %v", err))
		return
	}
	defer a.Close()
	req := map[string]any{"op": "exec", "argv": e.Argv, "env": e.Env, "cwd": e.Cwd, "stdin_b64": e.StdinB64,
		"timeout_s": e.TimeoutS, "tty": e.TTY}
	if e.User != "" {
		req["user"] = e.User
	}
	if err := a.send(req); err != nil {
		writeErr(w, errf(http.StatusBadGateway, "guest agent: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for {
		raw, rep, err := a.recv()
		if err != nil {
			b, _ := json.Marshal(map[string]string{"error": "guest agent: " + err.Error()})
			raw, rep = b, reply{Error: "x"}
		}
		if _, werr := w.Write(append(raw, '\n')); werr != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		if rep.final() {
			m.touch(r.PathValue("id"))
			return
		}
	}
}

func (m *Manager) getFileHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !validPath(path) {
		writeErr(w, errf(400, "path must be absolute"))
		return
	}
	sock, err := m.sock(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	a, err := dialAgent(r.Context(), sock)
	if err != nil {
		writeErr(w, errf(http.StatusBadGateway, "guest agent: %v", err))
		return
	}
	defer a.Close()
	if err := a.send(map[string]string{"op": "get_file", "path": path}); err != nil {
		writeErr(w, errf(http.StatusBadGateway, "guest agent: %v", err))
		return
	}
	started := false
	for {
		_, rep, err := a.recv()
		if err == nil && rep.Error != "" {
			err = &agentErr{rep.Error}
		}
		var data []byte
		if err == nil {
			data, err = base64.StdEncoding.DecodeString(rep.Data)
		}
		if err != nil {
			if !started {
				writeErr(w, err)
			} // else: truncated body; the client sees a short read
			return
		}
		if !started {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := w.Write(data); err != nil || rep.EOF {
			return
		}
	}
}

// putFileHandler streams the body to the guest as put_file chunks.
func (m *Manager) putFileHandler(w http.ResponseWriter, r *http.Request) {
	path, mode := r.URL.Query().Get("path"), r.URL.Query().Get("mode")
	if mode == "" {
		mode = "644"
	}
	if !validPath(path) || !reMode.MatchString(mode) {
		writeErr(w, errf(400, "path must be absolute, mode octal"))
		return
	}
	sock, err := m.sock(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	a, err := dialAgent(r.Context(), sock)
	if err != nil {
		writeErr(w, errf(http.StatusBadGateway, "guest agent: %v", err))
		return
	}
	defer a.Close()
	br := bufio.NewReaderSize(r.Body, fileChunk)
	buf := make([]byte, fileChunk)
	var off int64
	for {
		n, rerr := io.ReadFull(br, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			writeErr(w, errf(400, "read body: %v", rerr))
			return
		}
		_, perr := br.Peek(1)
		last := rerr != nil || perr != nil
		req := map[string]any{"op": "put_file", "path": path, "mode": mode, "offset": off,
			"data_b64": base64.StdEncoding.EncodeToString(buf[:n]), "last": last}
		if err := a.send(req); err != nil {
			writeErr(w, errf(http.StatusBadGateway, "guest agent: %v", err))
			return
		}
		_, rep, err := a.recv()
		if err == nil && rep.Error != "" {
			err = &agentErr{rep.Error}
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		off += int64(n)
		if last {
			m.touch(r.PathValue("id"))
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "bytes": off})
			return
		}
	}
}

// eventsHandler: SSE. Resume with Last-Event-ID (or ?after=); filter ?team=.
func (m *Manager) eventsHandler(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if a := r.URL.Query().Get("after"); a != "" {
		after, _ = strconv.ParseInt(a, 10, 64)
	}
	team := r.URL.Query().Get("team")
	backlog, ch, cancel := m.ev.subscribe(after)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	send := func(e Event) bool {
		if team != "" && e.Team != team {
			return true
		}
		b, _ := json.Marshal(e)
		_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, b)
		if fl != nil {
			fl.Flush()
		}
		return err == nil
	}
	for _, e := range backlog {
		if !send(e) {
			return
		}
	}
	if fl != nil {
		fl.Flush()
	}
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok || !send(e) {
				return
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func (m *Manager) usageHandler(r *http.Request) (any, error) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			since = time.Unix(n, 0).UTC()
		} else if since, err = time.Parse(time.RFC3339, s); err != nil {
			return nil, errf(400, "since: RFC 3339 or unix seconds")
		}
	}
	rep, err := m.Usage(since)
	if team := r.URL.Query().Get("team"); err == nil && team != "" {
		rep.VMs = filter(rep.VMs, func(v vmUsage) bool { return v.Team == team })
		rep.Snapshots = filter(rep.Snapshots, func(s snapUsage) bool { return s.Team == team })
		rep.Teams = filter(rep.Teams, func(t teamUsage) bool { return t.Team == team })
		rep.Storage = filter(rep.Storage, func(t teamStorage) bool { return t.Team == team })
		rep.Disk = nil // host-wide: not for tenants
	}
	return rep, err
}

func filter[T any](xs []T, keep func(T) bool) []T {
	out := []T{}
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}
