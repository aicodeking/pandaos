package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeBoatd implements the boatd internal contract in memory.
type fakeBoatd struct {
	mu       sync.Mutex
	vms      map[string]VM
	files    map[string][]byte // vmid + ":" + path
	execs    []map[string]any
	starts   []map[string]any
	desk     []map[string]any
	portReqs []map[string]any
	resizes  []map[string]any
	envOps   []map[string]any
	snaps    map[string]map[string]any
	cmds     map[string]*fakeCmd
	events   chan string
	n        int
	// discardPuts counts file uploads instead of keeping them (memory tests).
	discardPuts bool
	putBytes    int64
}

func newFakeBoatd(t *testing.T) (*fakeBoatd, *httptest.Server) {
	f := &fakeBoatd{vms: map[string]VM{}, files: map[string][]byte{}, snaps: map[string]map[string]any{}, cmds: map[string]*fakeCmd{}, events: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		switch in["size"] {
		case "", nil:
			in["size"] = "default" // boatd's defaultSize
		case "small", "default", "large", "xlarge":
		default:
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("unknown size %q", in["size"])})
			return
		}
		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("vm%d", f.n)
		vm := VM{"id": id, "name": in["name"], "size": in["size"], "image": in["image"], "team": in["team"],
			"state": "running", "env": in["env"], "ip": "10.0.0.5", "ports": []any{}, "from_snapshot": in["snapshot"], "labels": in["labels"]}
		if sid, _ := in["snapshot"].(string); sid != "" {
			if sn := f.snaps[sid]; sn == nil || sn["team"] != in["team"] {
				f.mu.Unlock()
				writeJSON(w, 404, map[string]string{"error": "no such snapshot"})
				return
			}
		}
		f.vms[id] = vm
		f.mu.Unlock()
		writeJSON(w, 201, vm)
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []VM{}
		for _, vm := range f.vms {
			if t := r.URL.Query().Get("team"); t == "" || vm["team"] == t {
				out = append(out, vm)
			}
		}
		writeJSON(w, 200, map[string]any{"vms": out})
	})
	get := func(w http.ResponseWriter, r *http.Request) VM {
		f.mu.Lock()
		defer f.mu.Unlock()
		vm := f.vms[r.PathValue("id")]
		if vm == nil {
			writeJSON(w, 404, map[string]string{"error": "no such vm"})
		}
		return vm
	}
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		if vm := get(w, r); vm != nil {
			writeJSON(w, 200, vm)
		}
	})
	mux.HandleFunc("POST /v1/vms/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		vm := get(w, r)
		if vm == nil {
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.PathValue("action") {
		case "start":
			f.starts = append(f.starts, in)
			vm["state"] = "running"
		case "stop":
			vm["state"] = "stopped"
			vm["has_snapshot"] = true
		case "delete":
			delete(f.vms, vm["id"].(string))
			for k, sn := range f.snaps { // like boatd: a VM's snapshots go with it
				if sn["vm_id"] == vm["id"] {
					delete(f.snaps, k)
				}
			}
			vm["state"] = "deleted"
		case "fork":
			f.n++
			id := fmt.Sprintf("vm%d", f.n)
			f.vms[id] = VM{"id": id, "name": in["name"], "team": in["team"], "state": "running", "labels": vm["labels"]} // like boatd: labels are copied
			writeJSON(w, 201, f.vms[id])
			return
		case "exec":
			f.execs = append(f.execs, in)
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			out, exit := "hello\n", 0
			if cmd, _ := in["cmd"].([]any); len(cmd) > 0 {
				joined := fmt.Sprint(cmd...)
				switch { // behave like these commands for the handlers and live scripts
				case cmd[0] == "echo":
					parts := make([]string, len(cmd)-1)
					for i, c := range cmd[1:] {
						parts[i] = fmt.Sprint(c)
					}
					out = strings.Join(parts, " ") + "\n"
				case cmd[0] == "cat" && fmt.Sprint(cmd[1]) == "/home/dev/env-ok":
					out = "built\n"
				case cmd[0] == "stat":
					out = "regular file|5|644|100\n"
				case cmd[0] == "ps":
					out = "1 0 root 100 /sbin/init splash\n"
				case strings.Contains(joined, "exit 3"):
					exit = 3
				}
			}
			enc.Encode(map[string]string{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte(out))})
			enc.Encode(map[string]int{"exit": exit})
			return
		case "resize":
			if in["size"] == "" { // disk growth works in any state
				writeJSON(w, 200, vm)
				return
			}
			if vm["state"] != "stopped" {
				writeJSON(w, 409, map[string]string{"error": "vm must be stopped to resize"})
				return
			}
			if in["discard_memory"] != true && vm["has_snapshot"] == true {
				writeJSON(w, 409, map[string]string{"error": "stop snapshot cannot change shape; set discard_memory"})
				return
			}
			vm["size"] = in["size"]
			f.resizes = append(f.resizes, in)
		case "agent":
			f.agentOp(w, vm, in)
			return
		case "env":
			f.envOps = append(f.envOps, in)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		case "desktop":
			f.desk = append(f.desk, in)
			writeJSON(w, 200, map[string]any{"ok": true, "port": 6080, "token": in["token"]})
			return
		case "ports":
			port := in["port"]
			if port == nil {
				port = in["guest_port"]
			}
			p := map[string]any{"guest_port": port, "hostname": fmt.Sprintf("%v-%s", port, vm["id"])}
			if in["public"] == true {
				p["host_port"] = 20000
			}
			f.portReqs = append(f.portReqs, in)
			ports, _ := vm["ports"].([]any)
			vm["ports"] = append(ports, p)
			writeJSON(w, 201, p)
			return
		case "snapshots":
			f.n++
			sid := fmt.Sprintf("snap%d", f.n)
			f.snaps[sid] = map[string]any{"id": sid, "name": in["name"], "vm_id": vm["id"], "team": vm["team"]}
			writeJSON(w, 201, f.snaps[sid])
			return
		}
		writeJSON(w, 200, vm)
	})
	mux.HandleFunc("GET /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		if get(w, r) == nil {
			return
		}
		f.mu.Lock()
		b, ok := f.files[r.PathValue("id")+":"+r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "no such file"})
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("PUT /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		if get(w, r) == nil {
			return
		}
		if f.discardPuts {
			f.putBytes, _ = io.Copy(io.Discard, r.Body)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.files[r.PathValue("id")+":"+r.URL.Query().Get("path")] = b
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "path": r.URL.Query().Get("path"), "bytes": len(b)})
	})
	mux.HandleFunc("DELETE /v1/vms/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
		vm := get(w, r)
		if vm == nil {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		ports, _ := vm["ports"].([]any)
		var kept []any
		for _, p := range ports {
			if fmt.Sprint(p.(map[string]any)["guest_port"]) != r.URL.Query().Get("port") {
				kept = append(kept, p)
			}
		}
		vm["ports"] = kept
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /v1/vms/{id}/snapshots/{sid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.snaps, r.PathValue("sid"))
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/vms/{id}/snapshots/{sid}/download", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if sn := f.snaps[r.PathValue("sid")]; sn != nil && sn["vm_id"] == r.PathValue("id") {
			fmt.Fprintf(w, "ARCHIVE:%s", sn["id"])
			return
		}
		writeJSON(w, 404, map[string]string{"error": "no such snapshot"})
	})
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for _, sn := range f.snaps {
			if t := r.URL.Query().Get("team"); t == "" || sn["team"] == t {
				out = append(out, sn)
			}
		}
		writeJSON(w, 200, map[string]any{"snapshots": out})
	})
	mux.HandleFunc("DELETE /v1/snapshots/{sid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.snaps, r.PathValue("sid"))
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/snapshots/import", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(string(b), "ARCHIVE:") {
			writeJSON(w, 400, map[string]string{"error": "not a snapshot archive"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.n++
		id := fmt.Sprintf("snap%d", f.n)
		f.snaps[id] = map[string]any{"id": id, "name": r.URL.Query().Get("name"), "team": r.URL.Query().Get("team"), "vm_id": ""}
		writeJSON(w, 201, f.snaps[id])
	})
	mux.HandleFunc("GET /v1/vms/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for _, sn := range f.snaps {
			if sn["vm_id"] == r.PathValue("id") {
				out = append(out, sn)
			}
		}
		writeJSON(w, 200, map[string]any{"snapshots": out})
	})
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("from") || r.URL.Query().Has("to") {
			writeJSON(w, 400, map[string]string{"error": "boatd takes ?since="})
			return
		}
		team := r.URL.Query().Get("team")
		writeJSON(w, 200, map[string]any{"since": r.URL.Query().Get("since"), "until": "now",
			"vms":   []any{map[string]any{"id": "vm1", "team": team, "size": "default", "vm_seconds": 42}},
			"teams": []any{map[string]any{"team": team, "vm_seconds": 42, "vm_seconds_by_size": map[string]int{"default": 42}}}})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-f.events:
				fmt.Fprintf(w, "id: 1\nevent: vm.state\ndata: %s\n\n", ev)
				w.(http.Flusher).Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

// fakeCmd is a detached command in the fake guest. "sleep" runs until signalled;
// anything else finishes at once with output. Stub agents report sessions the
// way the real CLIs do.
type fakeCmd struct {
	id      string
	vm      any
	argv    []any
	env     map[string]any
	running bool
	exit    any
	signal  any
	log     string
	started string
}

func (c *fakeCmd) view() map[string]any {
	state := "exited"
	if c.running {
		state = "running"
	}
	v := map[string]any{"cmd_id": c.id, "argv": c.argv, "state": state, "running": c.running, "pid": 42, "started": 1793000000}
	if !c.running { // absent until they apply, like the real agent
		if c.exit != nil {
			v["exit"] = c.exit
		}
		if c.signal != nil {
			v["signal"] = c.signal
		}
		v["ended"] = 1793000060
	}
	return v
}

// agentOp plays the guest agent; the caller holds f.mu.
func (f *fakeBoatd) agentOp(w http.ResponseWriter, vm VM, in map[string]any) {
	id, _ := in["cmd_id"].(string)
	c := f.cmds[id]
	switch in["op"] {
	case "cmd_start":
		f.n++
		c = &fakeCmd{id: fmt.Sprintf("c_%d", f.n), vm: vm["id"], running: false, started: "2026-11-01T00:00:00Z"}
		c.argv, _ = in["argv"].([]any)
		c.env, _ = in["env"].(map[string]any)
		first := fmt.Sprint(c.argv[0])
		joined := fmt.Sprint(c.argv...)
		switch {
		case first == "sleep" || strings.Contains(joined, "sleep-forever"):
			c.running, c.exit = true, nil
			c.log = "started\n"
		case first == "codex":
			c.exit, c.log = 0, `{"type":"thread.started","thread_id":"th_abc"}`+"\n"
		case first == "opencode":
			c.exit, c.log = 0, `{"type":"step","sessionID":"ses_abc"}`+"\n"
		case first == "false":
			c.exit, c.log = 1, "boom\n"
		default:
			c.exit, c.log = 0, "ran: "+joined+"\n"
		}
		f.cmds[c.id] = c
		writeJSON(w, 200, map[string]any{"ok": true, "cmd_id": c.id})
	case "cmd_list":
		out := []any{}
		for _, x := range f.cmds {
			if x.vm == vm["id"] {
				out = append(out, x.view())
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "cmds": out})
	case "cmd_status":
		if c == nil || c.vm != vm["id"] {
			writeJSON(w, 404, map[string]string{"error": "no such command"})
			return
		}
		v := c.view()
		v["ok"] = true
		writeJSON(w, 200, v)
	case "cmd_logs":
		if c == nil || c.vm != vm["id"] {
			writeJSON(w, 404, map[string]string{"error": "no such command"})
			return
		}
		off, _ := in["offset"].(float64)
		data := ""
		if int(off) < len(c.log) {
			data = c.log[int(off):]
		}
		writeJSON(w, 200, map[string]any{"ok": true, "data_b64": base64.StdEncoding.EncodeToString([]byte(data)), "offset": off,
			"next_offset": len(c.log), "size": len(c.log), "truncated": false, "running": c.running})
	case "cmd_signal":
		if c == nil || c.vm != vm["id"] {
			writeJSON(w, 404, map[string]string{"error": "no such command"})
			return
		}
		c.running, c.signal = false, "SIG"+fmt.Sprint(in["signal"])
		writeJSON(w, 200, map[string]any{"ok": true})
	case "file_list":
		out := []any{}
		prefix := strings.TrimRight(fmt.Sprint(in["path"]), "/") + "/"
		for k, b := range f.files {
			if p := strings.TrimPrefix(k, fmt.Sprint(vm["id"])+":"); p != k && strings.HasPrefix(p, prefix) {
				out = append(out, map[string]any{"name": strings.TrimPrefix(p, prefix), "type": "file", "size": len(b), "mode": "644", "mtime": 1})
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "entries": out, "truncated": false})
	case "file_delete":
		delete(f.files, fmt.Sprint(vm["id"])+":"+fmt.Sprint(in["path"]))
		writeJSON(w, 200, map[string]any{"ok": true})
	case "file_move":
		k := fmt.Sprint(vm["id"]) + ":"
		b, ok := f.files[k+fmt.Sprint(in["from"])]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "No such file"})
			return
		}
		delete(f.files, k+fmt.Sprint(in["from"]))
		f.files[k+fmt.Sprint(in["to"])] = b
		writeJSON(w, 200, map[string]any{"ok": true})
	case "file_search":
		out := []any{}
		for k := range f.files {
			if p := strings.TrimPrefix(k, fmt.Sprint(vm["id"])+":"); p != k && strings.Contains(p, fmt.Sprint(in["query"])) {
				out = append(out, map[string]any{"path": p})
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "matches": out, "truncated": false})
	default:
		writeJSON(w, 400, map[string]string{"error": "unknown op"})
	}
}
