package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	events   chan string
	n        int
}

func newFakeBoatd(t *testing.T) (*fakeBoatd, *httptest.Server) {
	f := &fakeBoatd{vms: map[string]VM{}, files: map[string][]byte{}, events: make(chan string, 16)}
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
			"state": "running", "env": in["env"], "ip": "10.0.0.5", "ports": []any{}}
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
		case "delete":
			delete(f.vms, vm["id"].(string))
			vm["state"] = "deleted"
		case "fork":
			f.n++
			id := fmt.Sprintf("vm%d", f.n)
			f.vms[id] = VM{"id": id, "name": in["name"], "team": in["team"], "state": "running"}
			writeJSON(w, 201, f.vms[id])
			return
		case "exec":
			f.execs = append(f.execs, in)
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			enc.Encode(map[string]string{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("hello\n"))})
			enc.Encode(map[string]int{"exit": 0})
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
			writeJSON(w, 201, map[string]any{"id": "snap1", "name": in["name"]})
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
	mux.HandleFunc("GET /v1/vms/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"snapshots": []any{}})
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
