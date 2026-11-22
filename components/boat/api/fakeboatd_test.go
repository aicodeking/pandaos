package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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
	envFail  bool // env op answers 502 (guest agent unreachable)
	// startEnvFail: a cold start's boot-time set_env fails like boatd's: the VM
	// is running but agent_ready=false with an error, and the call still answers 200.
	startEnvFail bool
	// releaseEnvFail: a fork/clone release's set_env fails like boatd's: the child is
	// running but agent_ready=false with an error, and the call still answers 200.
	releaseEnvFail bool
	// usageFn supplies cumulative aggregate seconds; usageWindowFn supplies exact VM intervals.
	usageFn           func(since string) float64
	usageWindowFn     func(url.Values) map[string]any
	usageQueries      []string // raw queries of GET /v1/usage
	snaps             map[string]map[string]any
	cmds              map[string]*fakeCmd
	events            chan string
	n                 int
	creates           []map[string]any // POST /v1/vms bodies
	patches           []map[string]any // PATCH /v1/vms/{id} bodies
	noPatch           bool             // answer PATCH 405, like a boatd from before it
	noDefaultSize     bool             // refuse size "default", like a host with other size names
	noImages          bool             // answer GET /v1/images 404, like a boatd without size discovery
	stops             []map[string]any
	legacyLogs        bool // cmd_logs omits exit/signal, like a guest agent from before they were added
	legacyCmdStart    bool // cmd_start omits pid, like an older guest agent
	execErr           map[string]any
	downloadProtected []bool
	forks             []map[string]any
	deskErr           string // non-empty: the guest agent refuses desktop with this (boatd answers 502)
	resizeTimeout     bool   // the guest's resize2fs times out (boatd answers 504 resize_timeout)
	refuse            string // non-empty: lifecycle POSTs answer 503 with this code (host full)
	// discardPuts counts file uploads instead of keeping them (memory tests).
	discardPuts         bool
	putBytes            int64
	afterCopy           func() // inject a failure after boatd copied a disk
	cleanupErr          bool   // refuse VM/snapshot deletion
	noPrepare           bool
	beforeRelease       func(kind, id string)
	copyEnvs            map[string]map[string]any
	sshPolicyCalls      int
	sshPolicyBatchCalls int
	noSSHPolicyBatch    bool
	snapPrepares        []map[string]any
	// envHook/startHook run (without the lock) before the env op / a start is
	// handled; envHook answering true makes the env op fail with 502.
	envHook   func(vmID string) (fail bool)
	startHook func(vmID string)
	// logsHook runs (without the lock) before a cmd_logs reply: timing and
	// concurrency tests of the guest's log reads.
	logsHook func()
	// beforeExec runs (without the lock) before an exec is handled, with the VM id:
	// concurrency tests of the exec path.
	beforeExec func(vmID string)
	// snapListHook runs (without the lock) before a GET /v1/vms/{id}/snapshots reply;
	// set it under mu, since background goroutines may be serving requests.
	snapListHook func(vmID string)
	// failStarts: the next this-many starts answer 500 (boatd could not boot the VM).
	failStarts int
	// failStartsAfterBoot: the next this-many starts boot the VM, then answer 500
	// (boatd failed after the VM was running, or its answer was lost).
	failStartsAfterBoot int
	// failStop: stops answer 500 and leave the VM as it is.
	failStop bool
	// freshBoots: the VMs a start asked to cold-boot with "fresh_boot": true; like
	// boatd, such a start ignores and discards the stopped VM's memory snapshot.
	freshBoots []string
	// releaseBodies: every release body, in order; freshReleases: the children a
	// release asked to cold-boot with "fresh_boot": true (no parent memory).
	releaseBodies []map[string]any
	freshReleases []string
	// failRelease: the next this-many releases answer 500 and leave the child pending;
	// failReleaseAfterBoot: they boot the child first, then answer 500.
	failRelease          int
	failReleaseAfterBoot int
	// freshCreates: the VMs a create made with "fresh_boot": true (the snapshot's
	// disk only, a cold boot, never its memory).
	freshCreates []string
	// agentOpHook overrides or injects a guest agent reply (called under f.mu).
	agentOpHook func(op string, in map[string]any) (int, map[string]any, bool)
	// vmGets counts GET /v1/vms/{id} calls per VM id.
	vmGets map[string]int
}

func newFakeBoatd(t *testing.T) (*fakeBoatd, *httptest.Server) {
	f := &fakeBoatd{copyEnvs: map[string]map[string]any{}, vms: map[string]VM{}, files: map[string][]byte{}, snaps: map[string]map[string]any{}, cmds: map[string]*fakeCmd{}, vmGets: map[string]int{}, events: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		noImg, noDef := f.noImages, f.noDefaultSize
		f.mu.Unlock()
		if noImg {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		sizes := map[string]any{
			"small":  map[string]any{"vcpu": 2, "mem_mib": 4096, "disk_gib": 51},
			"large":  map[string]any{"vcpu": 8, "mem_mib": 16384, "disk_gib": 111},
			"xlarge": map[string]any{"vcpu": 16, "mem_mib": 32768, "disk_gib": 251},
		}
		if !noDef {
			sizes["default"] = map[string]any{"vcpu": 4, "mem_mib": 8192, "disk_gib": 51}
		}
		writeJSON(w, 200, map[string]any{"sizes": sizes})
	})
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.creates = append(f.creates, in)
		refuse := f.refuse
		f.mu.Unlock()
		if refuse != "" {
			writeJSON(w, 503, map[string]string{"error": "host is full", "code": refuse})
			return
		}
		if n, _ := in["count"].(float64); n > 1 { // like boatd: all or none, names x-1 … x-N
			out := []VM{}
			f.mu.Lock()
			for i := 1; i <= int(n); i++ {
				f.n++
				id := fmt.Sprintf("vm%d", f.n)
				name := ""
				if s, _ := in["name"].(string); s != "" {
					name = fmt.Sprintf("%s-%d", s, i)
				}
				vm := VM{"id": id, "name": name, "size": "default", "image": in["image"], "team": in["team"], "state": "running",
					"env": in["env"], "ip": "10.0.0.5", "ports": []any{}, "labels": in["labels"], "ssh_boot_epoch": "fake-boot-" + id}
				if ttl, _ := in["ttl_s"].(float64); ttl > 0 {
					vm["ttl_s"] = ttl
					vm["expires_at"] = time.Now().UTC().Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
				}
				f.vms[id] = vm
				if in["fresh_boot"] == true {
					f.freshCreates = append(f.freshCreates, id)
				}
				out = append(out, fakeVMMetadata(vm))
			}
			f.mu.Unlock()
			writeJSON(w, 201, map[string]any{"vms": out})
			return
		}
		switch in["size"] {
		case "", nil:
			in["size"] = "default" // boatd's defaultSize
		case "default":
			if f.noDefaultSize { // a host whose sizes have other names
				writeJSON(w, 400, map[string]string{"error": `unknown size "default"`})
				return
			}
		case "small", "large", "xlarge":
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
			if sn := f.snaps[sid]; sn == nil || sn["pending_copy"] == true || sn["team"] != in["team"] {
				f.mu.Unlock()
				writeJSON(w, 404, map[string]string{"error": "no such snapshot"})
				return
			}
		}
		if ttl, _ := in["ttl_s"].(float64); ttl > 0 { // like boatd: the deadline starts at boot
			vm["ttl_s"] = ttl
			vm["expires_at"] = time.Now().UTC().Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
		}
		f.vms[id] = vm
		if in["fresh_boot"] == true {
			f.freshCreates = append(f.freshCreates, id)
		}
		f.mu.Unlock()
		writeJSON(w, 201, vm)
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []VM{}
		for _, vm := range f.vms {
			if t := r.URL.Query().Get("team"); (t == "" || vm["team"] == t) && (vm["pending_copy"] != true || r.URL.Query().Get("include_pending") == "true") {
				out = append(out, fakeVMMetadata(vm))
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
		f.mu.Lock()
		f.vmGets[r.PathValue("id")]++
		f.mu.Unlock()
		if vm := get(w, r); vm != nil {
			f.mu.Lock()
			cp := fakeVMMetadata(vm)
			f.mu.Unlock()
			if cp["pending_copy"] == true {
				writeJSON(w, 404, map[string]string{"error": "pending copy"})
				return
			}
			writeJSON(w, 200, cp)
		}
	})
	mux.HandleFunc("GET /v1/vms/{id}/env", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		vm := f.vms[r.PathValue("id")]
		if vm == nil || vm["pending_copy"] == true || vm["state"] != "running" {
			writeJSON(w, 409, map[string]string{"error": "VM is not running"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "vars": fakeCopyEnv(vm)})
	})
	// PATCH like boatd's: rename, ttl_s (deadline now+ttl), extend_s (deadline +extend).
	mux.HandleFunc("PATCH /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.noPatch {
			http.Error(w, "method not allowed", 405) // an older boatd
			return
		}
		vm := get(w, r)
		if vm == nil {
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.patches = append(f.patches, in)
		now := time.Now().UTC()
		if n, ok := in["name"].(string); ok {
			for id, o := range f.vms { // like boatd: a team's hostnames are unique
				if id != vm["id"] && o["team"] == vm["team"] && o["name"] == n && n != vm["name"] {
					writeJSON(w, 409, map[string]string{"error": "team already has a VM named " + n})
					return
				}
			}
			vm["name"] = n
		}
		if t, ok := in["ttl_s"].(float64); ok {
			vm["ttl_s"] = t
			vm["expires_at"] = now.Add(time.Duration(t) * time.Second).Format(time.RFC3339)
			if t == 0 {
				delete(vm, "expires_at")
			}
		}
		if x, ok := in["extend_s"].(float64); ok {
			base := now
			if e, err := time.Parse(time.RFC3339, fmt.Sprint(vm["expires_at"])); err == nil && e.After(now) {
				base = e
			}
			vm["expires_at"] = base.Add(time.Duration(x) * time.Second).Format(time.RFC3339)
		}
		if l, ok := in["labels"].(map[string]any); ok {
			vm["labels"] = l
		}
		if idle, ok := in["idle_stop_s"].(float64); ok {
			vm["idle_stop_s"] = idle
		}
		if rob, ok := in["resume_on_boot"].(bool); ok {
			vm["resume_on_boot"] = rob
		}
		writeJSON(w, 200, vm)
	})
	mux.HandleFunc("POST /v1/vms/ssh-policy-batch", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.noSSHPolicyBatch {
			http.Error(w, "not found", 404)
			return
		}
		f.sshPolicyBatchCalls++
		var req struct {
			Items []map[string]any `json:"items"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		results := map[string]any{}
		errs := map[string]string{}
		for _, item := range req.Items {
			id := fmt.Sprint(item["id"])
			vm := f.vms[id]
			if vm == nil {
				errs[id] = "no such vm"
				continue
			}
			config, _ := item["config"].(map[string]any)
			epoch := vm["ssh_boot_epoch"]
			if epoch == nil {
				epoch = "fake-boot-" + id
				vm["ssh_boot_epoch"] = epoch
			}
			if item["version"] != float64(1) || epoch == nil || item["boot_epoch"] != epoch || config["boot_epoch"] != epoch || config["vm_id"] != vm["id"] || config["team_id"] != vm["team"] || item["helper_b64"] == "" {
				errs[id] = "SSH identity mismatch"
				continue
			}
			b, _ := json.Marshal(config)
			sum := sha256.Sum256(b)
			digest := hex.EncodeToString(sum[:])
			vm["ssh_policy_ready"], vm["ssh_policy_digest"] = true, digest
			f.files[id+":/etc/boat/ssh-auth.json"] = b
			results[id] = map[string]any{
				"version":           1,
				"boot_epoch":        epoch,
				"config_digest":     digest,
				"readiness":         true,
				"vm_id":             id,
				"team":              vm["team"],
				"state":             vm["state"],
				"pending_copy":      vm["pending_copy"] == true,
				"ssh_policy_ready":  true,
				"ssh_policy_digest": digest,
			}
		}
		writeJSON(w, 200, map[string]any{"results": results, "errors": errs})
	})
	action := func(w http.ResponseWriter, r *http.Request) {
		vm := get(w, r)
		if vm == nil {
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refuse != "" && (r.PathValue("action") == "start" || r.PathValue("action") == "fork") {
			writeJSON(w, 503, map[string]string{"error": "host is full", "code": f.refuse})
			return
		}
		if vm["pending_copy"] == true && r.PathValue("action") != "delete" && r.PathValue("action") != "release" {
			writeJSON(w, 409, map[string]string{"error": "pending copy"})
			return
		}
		switch r.PathValue("action") {
		case "start":
			if hk := f.startHook; hk != nil {
				f.mu.Unlock()
				hk(vm["id"].(string))
				f.mu.Lock()
			}
			if f.failStarts > 0 {
				f.failStarts--
				writeJSON(w, 500, map[string]string{"error": "boot failed"})
				return
			}
			f.starts = append(f.starts, in)
			if vm["state"] != "running" {
				if in["fresh_boot"] == true {
					f.freshBoots = append(f.freshBoots, vm["id"].(string))
					vm["has_snapshot"] = false
				}
				vm["ssh_boot_epoch"] = randHex(16)
				vm["ssh_policy_ready"] = false
				delete(vm, "ssh_policy_digest")
				vm["state"] = "running"
				if vars, ok := in["env"]; ok {
					vm["env"] = vars
				}
				if _, hasEnv := in["env"]; hasEnv && f.startEnvFail {
					vm["agent_ready"], vm["error"] = false, "set_env: guest agent unreachable"
				} else if hasEnv {
					vm["agent_ready"], vm["error"] = true, ""
				}
			}
			if t, ok := in["ttl_s"].(float64); ok {
				vm["ttl_s"] = t
				if t == 0 {
					delete(vm, "expires_at")
				} else {
					vm["expires_at"] = time.Now().UTC().Add(time.Duration(t) * time.Second).Format(time.RFC3339)
				}
			}
			if f.failStartsAfterBoot > 0 {
				f.failStartsAfterBoot--
				writeJSON(w, 500, map[string]string{"error": "boatd failed after the boot"})
				return
			}
		case "stop":
			if f.failStop {
				writeJSON(w, 500, map[string]string{"error": "stop failed"})
				return
			}
			f.stops = append(f.stops, in)
			vm["state"] = "stopped"
			vm["has_snapshot"] = in["force"] != true
			delete(vm, "expires_at") // like boatd: a stopped VM has no deadline
			delete(vm, "error")
			vm["stopped_by"] = "boatd" // only a read of the VM after the stop shows this
		case "delete":
			if f.cleanupErr {
				writeJSON(w, 500, map[string]string{"error": "deletion refused"})
				return
			}
			delete(f.vms, vm["id"].(string))
			for k, sn := range f.snaps { // like boatd: a VM's snapshots go with it
				if sn["vm_id"] == vm["id"] {
					delete(f.snaps, k)
				}
			}
			vm["state"] = "deleted"
		case "release":
			if vm["pending_copy"] == true {
				if f.beforeRelease != nil {
					f.beforeRelease("fork", vm["id"].(string))
				}
				f.releaseBodies = append(f.releaseBodies, in)
				if f.failRelease > 0 {
					f.failRelease--
					writeJSON(w, 500, map[string]string{"error": "release failed"})
					return
				}
				if in["fresh_boot"] == true {
					f.freshReleases = append(f.freshReleases, vm["id"].(string))
					vm["has_snapshot"] = false
				}
				delete(vm, "pending_copy")
				vm["state"] = "running"
				if f.failReleaseAfterBoot > 0 {
					f.failReleaseAfterBoot--
					writeJSON(w, 500, map[string]string{"error": "release failed after the boot"})
					return
				}
				if vars, ok := in["env"]; ok {
					vm["env"] = vars
					f.envOps = append(f.envOps, map[string]any{"vm": vm["id"], "vars": vars})
					if f.releaseEnvFail {
						vm["agent_ready"], vm["error"] = false, "set_env: guest agent unreachable"
					}
				}
			}
		case "fork":
			if strings.HasSuffix(r.URL.Path, "/prepare") && f.noPrepare {
				writeJSON(w, 404, map[string]string{"error": "no prepare route"})
				return
			}
			f.forks = append(f.forks, in)
			f.n++
			id := fmt.Sprintf("vm%d", f.n)
			labels := vm["labels"] // like boatd: labels are copied unless given
			if l, ok := in["labels"]; ok {
				labels = l
			}
			f.vms[id] = VM{"id": id, "name": in["name"], "team": in["team"], "state": "running", "labels": labels, "env": vm["env"]}
			if ttl, ok := vm["ttl_s"]; ok { // like boatd: forks inherit the parent's TTL
				f.vms[id]["ttl_s"] = ttl
			}
			if t, ok := in["ttl_s"].(float64); ok {
				f.vms[id]["ttl_s"] = t
				if t == 0 {
					delete(f.vms[id], "expires_at")
				} else {
					f.vms[id]["expires_at"] = time.Now().UTC().Add(time.Duration(t) * time.Second).Format(time.RFC3339)
				}
			}
			f.copyEnvs[id] = fakeCopyEnv(vm)
			if strings.HasSuffix(r.URL.Path, "/prepare") {
				f.vms[id]["pending_copy"], f.vms[id]["state"] = true, "forking"
			}
			if f.afterCopy != nil {
				after := f.afterCopy
				f.mu.Unlock()
				after()
				f.mu.Lock()
			}
			writeJSON(w, 201, f.vms[id])
			return
		case "exec":
			if hk := f.beforeExec; hk != nil {
				f.mu.Unlock()
				hk(vm["id"].(string))
				f.mu.Lock()
			}
			f.execs = append(f.execs, in)
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			if f.execErr != nil {
				enc.Encode(f.execErr)
				return
			}
			if cwd, _ := in["cwd"].(string); strings.HasPrefix(cwd, "/no-such-") || cwd == "no-such-cwd" {
				enc.Encode(map[string]any{"error": "guest agent: cwd does not exist: " + cwd, "code": "spawn_failed"})
				return
			}
			out, exit := "hello\n", 0
			if cmd, _ := in["cmd"].([]any); len(cmd) > 0 {
				joined := fmt.Sprint(cmd...)
				switch { // behave like these commands for the handlers and live scripts
				case cmd[0] == "no-such-bin" || strings.Contains(joined, "no-such-bin"):
					enc.Encode(map[string]any{"error": "guest agent: exec failed: no such file or directory", "code": "spawn_failed"})
					return
				case cmd[0] == "perm-denied" || strings.Contains(joined, "perm-denied"):
					enc.Encode(map[string]any{"error": "guest agent: permission denied", "code": "permission_denied"})
					return
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
			if f.resizeTimeout {
				writeJSON(w, 504, map[string]string{"error": "resize: the disk is 9 GiB on the host, but the guest's resize2fs did not finish", "code": "resize_timeout"})
				return
			}
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
			switch in["size"] {
			case "default", "small", "large", "xlarge":
			default:
				writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("unknown size %q", in["size"])})
				return
			}
			vm["size"] = in["size"]
			f.resizes = append(f.resizes, in)
		case "agent":
			f.agentOp(w, vm, in)
			return
		case "ssh-policy":
			f.sshPolicyCalls++
			config, _ := in["config"].(map[string]any)
			epoch := vm["ssh_boot_epoch"]
			if epoch == nil {
				epoch = "fake-boot-" + fmt.Sprint(vm["id"])
				vm["ssh_boot_epoch"] = epoch
			}
			if in["version"] != float64(1) || epoch == nil || in["boot_epoch"] != epoch || config["boot_epoch"] != epoch || config["vm_id"] != vm["id"] || config["team_id"] != vm["team"] || in["helper_b64"] == "" {
				writeJSON(w, 409, map[string]string{"error": "SSH identity mismatch"})
				return
			}
			b, _ := json.Marshal(config)
			sum := sha256.Sum256(b)
			digest := hex.EncodeToString(sum[:])
			vm["ssh_policy_ready"], vm["ssh_policy_digest"] = true, digest
			f.files[fmt.Sprint(vm["id"])+":/etc/boat/ssh-auth.json"] = b
			writeJSON(w, 200, map[string]any{
				"version":           1,
				"boot_epoch":        epoch,
				"config_digest":     digest,
				"readiness":         true,
				"vm_id":             vm["id"],
				"team":              vm["team"],
				"state":             vm["state"],
				"pending_copy":      vm["pending_copy"] == true,
				"ssh_policy_ready":  true,
				"ssh_policy_digest": digest,
			})
			return
		case "env":
			if hk := f.envHook; hk != nil {
				f.mu.Unlock()
				fail := hk(vm["id"].(string))
				f.mu.Lock()
				if fail {
					writeJSON(w, 502, map[string]string{"error": "guest agent: unreachable"})
					return
				}
			}
			if f.envFail {
				writeJSON(w, 502, map[string]string{"error": "guest agent: unreachable"})
				return
			}
			in["vm"] = vm["id"]
			vm["env"] = in["vars"]
			f.envOps = append(f.envOps, in)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		case "desktop":
			if f.deskErr != "" {
				lower := strings.ToLower(f.deskErr)
				switch {
				case strings.Contains(lower, "not installed in this image"):
					writeJSON(w, 422, map[string]string{"error": "guest agent: " + f.deskErr, "code": "unsupported"})
				case strings.Contains(lower, "must be"):
					writeJSON(w, 400, map[string]string{"error": "guest agent: " + f.deskErr, "code": "invalid_argument"})
				default:
					writeJSON(w, 502, map[string]string{"error": "guest agent: " + f.deskErr})
				}
				return
			}
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
			replaced := false
			for i, existing := range ports {
				if em, ok := existing.(map[string]any); ok && fmt.Sprint(em["guest_port"]) == fmt.Sprint(port) {
					ports[i] = p
					replaced = true
					break
				}
			}
			if !replaced {
				ports = append(ports, p)
			}
			vm["ports"] = ports
			writeJSON(w, 201, p)
			return
		case "snapshots":
			if strings.HasSuffix(r.URL.Path, "/prepare") {
				f.snapPrepares = append(f.snapPrepares, in)
				if f.noPrepare {
					writeJSON(w, 404, map[string]string{"error": "no prepare route"})
					return
				}
			}
			f.n++
			sid := fmt.Sprintf("snap%d", f.n)
			f.snaps[sid] = map[string]any{"id": sid, "name": in["name"], "vm_id": vm["id"], "team": vm["team"]}
			f.copyEnvs[sid] = fakeCopyEnv(vm)
			if strings.HasSuffix(r.URL.Path, "/prepare") {
				f.snaps[sid]["pending_copy"] = true
			}
			if f.afterCopy != nil {
				after := f.afterCopy
				f.mu.Unlock()
				after()
				f.mu.Lock()
			}
			writeJSON(w, 201, f.snaps[sid])
			return
		}
		writeJSON(w, 200, vm)
	}
	mux.HandleFunc("POST /v1/vms/{id}/{action}", action)
	mux.HandleFunc("POST /v1/vms/{id}/fork/prepare", func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("action", "fork"); action(w, r) })
	mux.HandleFunc("POST /v1/vms/{id}/snapshots/prepare", func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("action", "snapshots"); action(w, r) })
	mux.HandleFunc("POST /v1/vms/{id}/snapshots/{sid}/release", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		sn := f.snaps[r.PathValue("sid")]
		if sn == nil || sn["vm_id"] != r.PathValue("id") {
			writeJSON(w, 404, map[string]string{"error": "snapshot not found"})
			return
		}
		if sn["pending_copy"] == true && f.beforeRelease != nil {
			f.beforeRelease("snapshots", r.PathValue("sid"))
		}
		delete(sn, "pending_copy")
		writeJSON(w, 200, sn)
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
		if f.cleanupErr {
			writeJSON(w, 500, map[string]string{"error": "deletion refused"})
			return
		}
		delete(f.snaps, r.PathValue("sid"))
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/vms/{id}/snapshots/{sid}/download", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if sn := f.snaps[r.PathValue("sid")]; sn != nil && sn["pending_copy"] != true && sn["vm_id"] == r.PathValue("id") {
			prot := r.URL.Query().Get("protected") == "1"
			f.downloadProtected = append(f.downloadProtected, prot)
			if prot {
				fmt.Fprintf(w, "ARCHIVE:%s:protected", sn["id"])
			} else {
				fmt.Fprintf(w, "ARCHIVE:%s", sn["id"])
			}
			return
		}
		writeJSON(w, 404, map[string]string{"error": "no such snapshot"})
	})
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for _, sn := range f.snaps {
			if t := r.URL.Query().Get("team"); (t == "" || sn["team"] == t) && (sn["pending_copy"] != true || r.URL.Query().Get("include_pending") == "true") {
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
		sn := map[string]any{"id": id, "name": r.URL.Query().Get("name"), "team": r.URL.Query().Get("team"), "vm_id": ""}
		src := strings.TrimPrefix(string(b), "ARCHIVE:")
		if strings.HasSuffix(src, ":protected") {
			sn["no_env"] = true
			sn["protected"] = true
		} else if src == "no_env" || strings.HasSuffix(src, ":no_env") {
			sn["no_env"] = true
		} else if strings.HasSuffix(src, ":label_no_env") {
			sn["labels"] = map[string]any{noEnvLabel: "1"}
		} else if orig := f.snaps[src]; orig != nil {
			if b, _ := orig["no_env"].(bool); b {
				sn["no_env"] = true
			} else if origVM := f.vms[fmt.Sprint(orig["vm_id"])]; origVM != nil && labeledNoEnv(origVM) {
				sn["no_env"] = true
			}
		}
		f.snaps[id] = sn
		writeJSON(w, 201, f.snaps[id])
	})
	mux.HandleFunc("GET /v1/vms/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		hook := f.snapListHook
		f.mu.Unlock()
		if hook != nil {
			hook(r.PathValue("id"))
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for _, sn := range f.snaps {
			if sn["vm_id"] == r.PathValue("id") && (sn["pending_copy"] != true || r.URL.Query().Get("include_pending") == "true") {
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
		f.mu.Lock()
		f.usageQueries = append(f.usageQueries, r.URL.RawQuery)
		secs := float64(42)
		if f.usageFn != nil {
			secs = f.usageFn(r.URL.Query().Get("since"))
		}
		if id := r.URL.Query().Get("vm_id"); id != "" {
			if f.usageWindowFn != nil {
				out := f.usageWindowFn(r.URL.Query())
				f.mu.Unlock()
				writeJSON(w, 200, out)
				return
			}
			now := time.Now().UTC()
			since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
			until := now
			if raw := r.URL.Query().Get("until"); raw != "" {
				until, _ = time.Parse(time.RFC3339Nano, raw)
				if until.After(now) {
					until = now
				}
				if f.usageFn != nil {
					secs -= f.usageFn(raw)
				}
			}
			start := until.Add(-time.Duration(secs * float64(time.Second)))
			if start.Before(since) {
				start = since
			}
			size := str(f.vms[id], "size")
			f.mu.Unlock()
			writeJSON(w, 200, map[string]any{"exact": true, "since": since, "until": until, "intervals": []any{
				map[string]any{"kind": "vm", "id": id, "team": team, "size": size, "start": start, "end": until},
			}})
			return
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"since": r.URL.Query().Get("since"), "until": "now",
			"vms":   []any{map[string]any{"id": "vm1", "team": team, "size": "default", "vm_seconds": secs}},
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
	if f.agentOpHook != nil {
		if status, body, handled := f.agentOpHook(fmt.Sprint(in["op"]), in); handled {
			writeJSON(w, status, body)
			return
		}
	}
	id, _ := in["cmd_id"].(string)
	c := f.cmds[id]
	switch in["op"] {
	case "cmd_start":
		if u, _ := in["user"].(string); u != "" && u != "dev" && u != "user" && u != "root" {
			writeJSON(w, 400, map[string]string{"error": "guest agent: no such user: " + u, "code": "invalid_user"})
			return
		}
		if cwd, _ := in["cwd"].(string); strings.HasPrefix(cwd, "/no-such-") || cwd == "no-such-cwd" {
			writeJSON(w, 400, map[string]string{"error": "guest agent: cwd does not exist: " + cwd, "code": "spawn_failed"})
			return
		}
		argv, _ := in["argv"].([]any)
		if len(argv) > 0 {
			bin := fmt.Sprint(argv[0])
			if bin == "no-such-bin" || strings.HasPrefix(bin, "/no-such-") {
				writeJSON(w, 400, map[string]string{"error": "guest agent: exec failed: no such file or directory: " + bin, "code": "spawn_failed"})
				return
			}
			if bin == "permission-denied" {
				writeJSON(w, 403, map[string]string{"error": "guest agent: permission denied: " + bin, "code": "permission_denied"})
				return
			}
		}
		f.n++
		c = &fakeCmd{id: fmt.Sprintf("c_%d", f.n), vm: vm["id"], running: false, started: "2026-11-01T00:00:00Z"}
		c.argv = argv
		c.env, _ = in["env"].(map[string]any)
		first := fmt.Sprint(c.argv[0])
		joined := fmt.Sprint(c.argv...)
		isLongSleep := first == "sleep" || strings.Contains(joined, "sleep-forever")
		if !isLongSleep && first == "bash" && len(c.argv) == 3 && fmt.Sprint(c.argv[1]) == "-lc" {
			script := strings.TrimSpace(fmt.Sprint(c.argv[2]))
			if strings.HasPrefix(script, "sleep ") && !strings.ContainsAny(script, ";|&") {
				isLongSleep = true
			}
		}
		switch {
		case isLongSleep:
			c.running, c.exit = true, nil
			c.log = "started\n"
		case first == "codex":
			c.exit, c.log = 0, `{"type":"thread.started","thread_id":"th_abc"}`+"\n"
		case first == "opencode":
			c.exit, c.log = 0, `{"type":"step","sessionID":"ses_abc"}`+"\n"
		case first == "kimi":
			c.exit, c.log = 0, `{"role":"assistant","session_id":"kimi_s1"}`+"\n"
		case first == "vibe":
			c.exit, c.log = 0, `{"type":"init","sessionId":"vibe_s1"}`+"\n"
		case first == "false":
			c.exit, c.log = 1, "boom\n"
		default:
			c.exit, c.log = 0, "ran: "+joined+"\n"
		}
		f.cmds[c.id] = c
		reply := map[string]any{"ok": true, "cmd_id": c.id}
		if !f.legacyCmdStart {
			reply["pid"] = 42
		}
		writeJSON(w, 200, reply)
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
			writeJSON(w, 404, map[string]string{"error": "guest agent: no such command: " + id, "code": "not_found"})
			return
		}
		v := c.view()
		v["ok"] = true
		writeJSON(w, 200, v)
	case "cmd_logs":
		if hk := f.logsHook; hk != nil {
			f.mu.Unlock()
			hk()
			f.mu.Lock()
		}
		if c == nil || c.vm != vm["id"] {
			writeJSON(w, 404, map[string]string{"error": "guest agent: no such command: " + id, "code": "not_found"})
			return
		}
		off, _ := in["offset"].(float64)
		data := ""
		if int(off) < len(c.log) {
			data = c.log[int(off):]
		}
		reply := map[string]any{"ok": true, "data_b64": base64.StdEncoding.EncodeToString([]byte(data)), "offset": off,
			"next_offset": len(c.log), "size": len(c.log), "truncated": false, "running": c.running}
		if !f.legacyLogs { // current guests also report exit/signal (null when not applicable)
			if c.running {
				reply["exit"], reply["signal"] = nil, nil
			} else {
				reply["exit"], reply["signal"] = c.exit, c.signal
			}
		}
		writeJSON(w, 200, reply)
	case "cmd_signal":
		if c == nil || c.vm != vm["id"] {
			writeJSON(w, 404, map[string]string{"error": "guest agent: no such command: " + id, "code": "not_found"})
			return
		}
		if !c.running {
			writeJSON(w, 409, map[string]string{"error": "guest agent: command is not running: exited", "code": "not_running"})
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
		path := fmt.Sprint(in["path"])
		if path == "/" || path == "/etc" || path == "/usr" {
			writeJSON(w, 400, map[string]string{"error": "guest agent: refusing to delete " + path, "code": "invalid_argument"})
			return
		}
		if strings.HasPrefix(path, "/root/denied") {
			writeJSON(w, 403, map[string]string{"error": "guest agent: permission denied: " + path, "code": "permission_denied"})
			return
		}
		k := fmt.Sprint(vm["id"]) + ":"
		dirPrefix := strings.TrimRight(path, "/") + "/"
		hasChildren := false
		for fk := range f.files {
			if strings.HasPrefix(fk, k+dirPrefix) {
				hasChildren = true
				break
			}
		}
		if hasChildren {
			if in["recursive"] != true {
				writeJSON(w, 409, map[string]string{"error": "guest agent: " + path + " is a non-empty directory; set recursive", "code": "not_empty"})
				return
			}
			for fk := range f.files {
				if strings.HasPrefix(fk, k+dirPrefix) {
					delete(f.files, fk)
				}
			}
		}
		delete(f.files, k+path)
		writeJSON(w, 200, map[string]any{"ok": true})
	case "file_move":
		k := fmt.Sprint(vm["id"]) + ":"
		from, to := fmt.Sprint(in["from"]), fmt.Sprint(in["to"])
		b, ok := f.files[k+from]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "guest agent: no such file: " + from, "code": "not_found"})
			return
		}
		toDirPrefix := strings.TrimRight(to, "/") + "/"
		toIsDir := to == "/etc"
		if !toIsDir {
			for fk := range f.files {
				if strings.HasPrefix(fk, k+toDirPrefix) {
					toIsDir = true
					break
				}
			}
		}
		if toIsDir {
			writeJSON(w, 409, map[string]string{"error": "guest agent: " + to + " is a directory", "code": "is_a_directory"})
			return
		}
		if _, exists := f.files[k+to]; exists && in["overwrite"] != true {
			writeJSON(w, 409, map[string]string{"error": "guest agent: " + to + " exists; set overwrite", "code": "already_exists"})
			return
		}
		delete(f.files, k+from)
		f.files[k+to] = b
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
		writeJSON(w, 400, map[string]string{"error": "guest agent: unknown op: " + fmt.Sprint(in["op"]), "code": "invalid_argument"})
	}
}

func fakeCopyEnv(vm VM) map[string]any {
	out := map[string]any{}
	if vars, ok := vm["env"].(map[string]any); ok {
		for k, v := range vars {
			out[k] = v
		}
	}
	return out
}

func fakeVMMetadata(vm VM) VM {
	out := VM{}
	for k, v := range vm {
		if k != "env" {
			out[k] = v
		}
	}
	if out["ssh_boot_epoch"] == nil {
		out["ssh_boot_epoch"] = "fake-boot-" + fmt.Sprint(vm["id"])
	}
	return out
}
