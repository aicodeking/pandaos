package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestR3SyncJumpKeysSkipsNoOpWritesAndFiltersSSEEvents(t *testing.T) {
	t.Run("no_grants_no_ssh_no_jump_file_skips_boatd_and_store", func(t *testing.T) {
		e := setup(t)
		var vmListCalls atomic.Int32
		orig := e.srv.boatd.hc.Transport
		if orig == nil {
			orig = http.DefaultTransport
		}
		e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" && r.URL.Path == "/v1/vms" {
				vmListCalls.Add(1)
			}
			return orig.RoundTrip(r)
		})

		if err := e.srv.store.Flush(); err != nil {
			t.Fatal(err)
		}
		writesBefore := e.srv.store.writes.Load()
		seqBefore := e.srv.store.Snapshot().seq

		if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
			t.Fatal(err)
		}
		if vmListCalls.Load() != 0 {
			t.Fatalf("GET /v1/vms calls = %d, want 0 when no grants and SSH disabled", vmListCalls.Load())
		}
		if e.srv.store.writes.Load() != writesBefore || e.srv.store.Snapshot().seq != seqBefore {
			t.Fatalf("unexpected store write/tx: writes %d->%d seq %d->%d",
				writesBefore, e.srv.store.writes.Load(), seqBefore, e.srv.store.Snapshot().seq)
		}
	})

	t.Run("unchanged_jump_file_and_grants_skip_fsync_and_filter_sse", func(t *testing.T) {
		e := setup(t)
		e.enableSSH()
		jf := filepath.Join(t.TempDir(), "jump_authorized_keys")
		if err := os.WriteFile(jf, []byte("ssh-ed25519 AAAA stale@host\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.srv.cfg.JumpKeysFile = jf
		e.srv.sshCleanupDebounce = 15 * time.Millisecond

		vm1 := e.createVM()
		vm2 := e.createVM()
		e.grantSSH(e.owner, vm1)

		if err := e.srv.store.Flush(); err != nil {
			t.Fatal(err)
		}
		writesBefore := e.srv.store.writes.Load()
		seqBefore := e.srv.store.Snapshot().seq

		// First sync clears the stale JumpKeysFile to 0 bytes 0640, but does not
		// write state.json because the live grant on vm1 is unchanged.
		if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(jf)
		if err != nil || len(b) != 0 {
			t.Fatalf("JumpKeysFile not cleared: len=%d err=%v", len(b), err)
		}
		fi1, err := os.Lstat(jf)
		if err != nil {
			t.Fatal(err)
		}
		if fi1.Mode().Perm() != 0o640 {
			t.Fatalf("JumpKeysFile mode = %#o, want 0640", fi1.Mode().Perm())
		}
		ino1 := fi1.Sys().(*syscall.Stat_t).Ino
		if e.srv.store.writes.Load() != writesBefore || e.srv.store.Snapshot().seq != seqBefore {
			t.Fatalf("first no-op sync mutated store: writes %d->%d seq %d->%d",
				writesBefore, e.srv.store.writes.Load(), seqBefore, e.srv.store.Snapshot().seq)
		}

		// Second sync with unchanged state must skip both writeAtomicMode (same
		// inode, 0 fsyncs) and Store.Update (0 writes) and POST /ssh-policy.
		var vmListCalls, sshPolicyCalls atomic.Int32
		orig := e.srv.boatd.hc.Transport
		if orig == nil {
			orig = http.DefaultTransport
		}
		e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" && r.URL.Path == "/v1/vms" {
				vmListCalls.Add(1)
			}
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ssh-policy") {
				sshPolicyCalls.Add(1)
			}
			return orig.RoundTrip(r)
		})

		if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
			t.Fatal(err)
		}
		fi2, err := os.Lstat(jf)
		if err != nil {
			t.Fatal(err)
		}
		ino2 := fi2.Sys().(*syscall.Stat_t).Ino
		if ino2 != ino1 || !fi2.ModTime().Equal(fi1.ModTime()) {
			t.Fatalf("JumpKeysFile was rewritten despite already being empty 0640: ino %d->%d", ino1, ino2)
		}
		if sshPolicyCalls.Load() != 0 {
			t.Fatalf("unchanged attested VMs re-attested %d times, want 0", sshPolicyCalls.Load())
		}
		if e.srv.store.writes.Load() != writesBefore || e.srv.store.Snapshot().seq != seqBefore {
			t.Fatalf("second no-op sync mutated store: writes %d->%d seq %d->%d",
				writesBefore, e.srv.store.writes.Load(), seqBefore, e.srv.store.Snapshot().seq)
		}

		// Routine SSE VM events must not trigger host-wide GET /v1/vms or
		// JumpKeysFile rewrites.
		vmListCalls.Store(0)
		team := e.teamID()
		events := []string{
			fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"creating"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"starting"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"stopping"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"forking"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.resized","team":%q,"vm_id":%q,"state":"running"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.created","team":%q,"vm_id":%q,"state":"running"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"stopped"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"error"}`, team, vm2),
			fmt.Sprintf(`{"type":"vm.deleted","team":%q,"vm_id":%q}`, team, vm2),
		}
		for _, ev := range events {
			e.srv.hooks.handleRaw(context.Background(), "", ev)
		}
		time.Sleep(80 * time.Millisecond)
		if vmListCalls.Load() != 0 {
			t.Fatalf("routine SSE VM events triggered %d GET /v1/vms scans, want 0", vmListCalls.Load())
		}
		fi3, err := os.Lstat(jf)
		if err != nil {
			t.Fatal(err)
		}
		if fi3.Sys().(*syscall.Stat_t).Ino != ino1 {
			t.Fatal("routine SSE VM events rewrote already-empty JumpKeysFile")
		}
	})
}

func TestR3SyncJumpKeysCoalescesSSEBurst(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.sshCleanupDebounce = 30 * time.Millisecond
	team := e.teamID()

	vm1 := e.createVM()
	e.grantSSH(e.owner, vm1)
	// Stop vm1 so revoking leaves pending CleanupVMs debt that clears once vm1 runs again.
	e.boatd.mu.Lock()
	e.boatd.vms[vm1]["state"] = "stopped"
	e.boatd.mu.Unlock()
	if st, _ := e.do(e.owner, "DELETE", vmPath(vm1, "ssh", url.PathEscape(sshFingerprint(r11Pub))), nil); st != 200 {
		t.Fatalf("revoke ssh: %d", st)
	}
	if len(e.srv.store.Snapshot().SSHGrants) != 1 {
		t.Fatalf("expected 1 grant with pending CleanupVMs, got %d", len(e.srv.store.Snapshot().SSHGrants))
	}

	var vmListCalls atomic.Int32
	orig := e.srv.boatd.hc.Transport
	if orig == nil {
		orig = http.DefaultTransport
	}
	e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Path == "/v1/vms" {
			vmListCalls.Add(1)
		}
		return orig.RoundTrip(r)
	})

	// Bring vm1 back to running and fire a 30-event burst.
	e.boatd.mu.Lock()
	e.boatd.vms[vm1]["state"] = "running"
	e.boatd.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ev := fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":%q,"state":"running"}`, team, vm1)
	for range 30 {
		e.srv.hooks.handleRaw(ctx, "vm.state", ev)
	}

	waitFor(t, "cleanup debt drained", func() bool {
		return len(e.srv.store.Snapshot().SSHGrants) == 0
	})
	waitFor(t, "reconciler idle", func() bool {
		e.srv.reconcileMu.Lock()
		defer e.srv.reconcileMu.Unlock()
		return !e.srv.reconcileActive[vm1]
	})
	time.Sleep(80 * time.Millisecond)

	if got := vmListCalls.Load(); got == 0 || got > 2 {
		t.Fatalf("30-event burst caused %d GET /v1/vms scans, want coalesced 1..2", got)
	}
}

func TestR3NormalLifecycleSSEDoesNotTriggerFleetReconcileOrFsyncs(t *testing.T) {
	for _, withSSH := range []bool{false, true} {
		name := "no_ssh"
		if withSSH {
			name = "with_ssh"
		}
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://boat.example"
			if withSSH {
				e.enableSSH()
			}
			// Mark CopyCredentialsV1 as enabled (normal steady-state post-startup).
			if err := e.srv.store.Update(func(st *State) error {
				st.CopyCredentialsV1 = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// Pre-populate 5 background running VMs to detect any O(N) fleet scan.
			others := map[string]bool{}
			for range 5 {
				others[e.createVM()] = true
			}
			team := e.teamID()

			var (
				mu             sync.Mutex
				fleetListCalls int
				otherEnvGets   int
				otherEnvPosts  int
				otherSSHPosts  int
				reconcileDone  sync.WaitGroup
				emitRunning    atomic.Bool
			)
			orig := e.srv.boatd.hc.Transport
			if orig == nil {
				orig = http.DefaultTransport
			}
			e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && r.URL.Path == "/v1/vms" {
					mu.Lock()
					fleetListCalls++
					mu.Unlock()
				}
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				if len(parts) == 4 && parts[0] == "v1" && parts[1] == "vms" && others[parts[2]] {
					mu.Lock()
					switch {
					case r.Method == "GET" && parts[3] == "env":
						otherEnvGets++
					case r.Method == "POST" && parts[3] == "env":
						otherEnvPosts++
					case r.Method == "POST" && parts[3] == "ssh-policy":
						otherSSHPosts++
					}
					mu.Unlock()
				}

				resp, err := orig.RoundTrip(r)
				if err != nil || !emitRunning.Load() || resp.StatusCode/100 != 2 {
					return resp, err
				}
				// Reproduce boatd publishing SSE vm.state=running BEFORE the HTTP
				// response returns to createVM / startVM / forkVM.
				trigger := false
				var targetID string
				switch {
				case r.Method == "POST" && r.URL.Path == "/v1/vms":
					trigger = true
				case r.Method == "POST" && len(parts) == 4 && parts[0] == "v1" && parts[1] == "vms" && (parts[3] == "start" || parts[3] == "release"):
					trigger = true
					targetID = parts[2]
				}
				if trigger {
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					resp.Body = io.NopCloser(bytes.NewReader(body))
					if targetID == "" {
						var parsed map[string]any
						if json.Unmarshal(body, &parsed) == nil {
							targetID = str(parsed, "id")
						}
					}
					if targetID != "" {
						reconcileDone.Add(1)
						go func(id string) {
							defer reconcileDone.Done()
							e.srv.reconcileRunningVMForTeam(context.Background(), id, team)
						}(targetID)
						// Give the reconciler a moment to hit waitExisting / startLocks
						// while createVM / startVM / forkVM is still in flight.
						time.Sleep(10 * time.Millisecond)
					}
				}
				return resp, nil
			})

			assertNoFleetTouch := func(op string) {
				t.Helper()
				reconcileDone.Wait()
				mu.Lock()
				defer mu.Unlock()
				if fleetListCalls != 0 || otherEnvGets != 0 || otherEnvPosts != 0 || otherSSHPosts != 0 {
					t.Fatalf("%s + SSE vm.state=running touched fleet: GET /v1/vms=%d otherEnvGet=%d otherEnvPost=%d otherSSHPost=%d",
						op, fleetListCalls, otherEnvGets, otherEnvPosts, otherSSHPosts)
				}
			}

			emitRunning.Store(true)

			// 1. createVM
			created := e.createVM()
			if err := e.srv.store.Flush(); err != nil {
				t.Fatal(err)
			}
			writesAfterCreate := e.srv.store.writes.Load()
			assertNoFleetTouch("createVM")
			if e.srv.store.writes.Load() != writesAfterCreate {
				t.Fatalf("createVM SSE reconcile triggered spurious Store.Update fsyncs: %d -> %d", writesAfterCreate, e.srv.store.writes.Load())
			}
			if e.srv.store.Snapshot().VMTokens[created] == nil {
				t.Fatal("created VM missing VMTokens")
			}

			// 2. stop + startVM
			if st, out := e.do(e.owner, "POST", vmPath(created, "stop"), nil); st != 200 {
				t.Fatalf("stop: %d %v", st, out)
			}
			if st, out := e.do(e.owner, "POST", vmPath(created, "start"), nil); st != 200 {
				t.Fatalf("start: %d %v", st, out)
			}
			if err := e.srv.store.Flush(); err != nil {
				t.Fatal(err)
			}
			writesAfterStart := e.srv.store.writes.Load()
			assertNoFleetTouch("startVM")
			if e.srv.store.writes.Load() != writesAfterStart {
				t.Fatalf("startVM SSE reconcile triggered spurious Store.Update fsyncs: %d -> %d", writesAfterStart, e.srv.store.writes.Load())
			}

			// 3. forkVM
			st, forked := e.do(e.owner, "POST", vmPath(created, "fork"), map[string]any{"name": "child"})
			if st != 201 {
				t.Fatalf("fork: %d %v", st, forked)
			}
			childID := str(forked, "id")
			if err := e.srv.store.Flush(); err != nil {
				t.Fatal(err)
			}
			writesAfterFork := e.srv.store.writes.Load()
			assertNoFleetTouch("forkVM")
			if e.srv.store.writes.Load() != writesAfterFork {
				t.Fatalf("forkVM SSE reconcile triggered spurious Store.Update fsyncs: %d -> %d", writesAfterFork, e.srv.store.writes.Load())
			}
			if e.srv.store.Snapshot().VMTokens[childID] == nil {
				t.Fatal("forked child missing VMTokens")
			}
		})
	}
}

func TestR3UnmanagedRunningVMReconcilesSingleVMFailClosed(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.cfg.VMAPIURL = "https://boat.example"
	if err := e.srv.store.Update(func(st *State) error {
		st.CopyCredentialsV1 = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	team := e.teamID()

	// Pre-populate 5 normal running VMs that must remain untouched.
	others := map[string]bool{}
	for range 5 {
		others[e.createVM()] = true
	}

	// Add an externally-started / unmanaged running VM with foreign + restricted labels.
	e.boatd.mu.Lock()
	e.boatd.vms["vm-ext-foreign"] = VM{
		"id": "vm-ext-foreign", "name": "ext-foreign", "size": "default", "image": "ubuntu-24.04",
		"team": team, "state": "running", "ip": "10.0.0.88", "ports": []any{},
		"ssh_boot_epoch": "epoch-ext-1", "ssh_policy_ready": false,
		"labels": map[string]any{foreignLabel: "1", restrictedLabel: "1"},
	}
	// Add an externally-started VM with a crashed cold credential transition fence.
	e.boatd.vms["vm-ext-transition"] = VM{
		"id": "vm-ext-transition", "name": "ext-trans", "size": "default", "image": "ubuntu-24.04",
		"team": team, "state": "running", "ip": "10.0.0.89", "ports": []any{},
		"ssh_boot_epoch": "epoch-tr-1", "ssh_policy_ready": false,
	}
	e.boatd.mu.Unlock()
	if err := e.srv.beginCredentialTransition("vm-ext-transition", &CredentialTransition{NoEnv: true, Selection: []string{}}); err != nil {
		t.Fatal(err)
	}

	var (
		mu             sync.Mutex
		fleetListCalls int
		otherTouched   []string
	)
	orig := e.srv.boatd.hc.Transport
	if orig == nil {
		orig = http.DefaultTransport
	}
	e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		if r.Method == "GET" && r.URL.Path == "/v1/vms" {
			fleetListCalls++
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "vms" && others[parts[2]] {
			otherTouched = append(otherTouched, r.Method+" "+r.URL.Path)
		}
		mu.Unlock()
		return orig.RoundTrip(r)
	})

	// Trigger SSE vm.state=running for both unmanaged VMs.
	e.srv.hooks.handleRaw(context.Background(), "vm.state",
		fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":"vm-ext-foreign","state":"running"}`, team))
	e.srv.hooks.handleRaw(context.Background(), "vm.state",
		fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":"vm-ext-transition","state":"running"}`, team))

	waitFor(t, "vm-ext-foreign reconciled and attested", func() bool {
		st := e.srv.store.Snapshot()
		e.boatd.mu.Lock()
		ready := e.boatd.vms["vm-ext-foreign"]["ssh_policy_ready"] == true
		e.boatd.mu.Unlock()
		return ready && st.VMTokens["vm-ext-foreign"] != nil
	})
	waitFor(t, "vm-ext-transition force-stopped fail-closed", func() bool {
		e.boatd.mu.Lock()
		defer e.boatd.mu.Unlock()
		return e.boatd.vms["vm-ext-transition"]["state"] == "stopped"
	})

	st := e.srv.store.Snapshot()
	if !st.ForeignVMs["vm-ext-foreign"] {
		t.Fatal("vm-ext-foreign missing ForeignVMs mark")
	}
	if ai := st.AIVMs["vm-ext-foreign"]; ai == nil || !ai.Blocked {
		t.Fatalf("vm-ext-foreign AI credentials not blocked fail-closed: %+v", ai)
	}
	tok := st.VMTokens["vm-ext-foreign"]
	if tok == nil || !tok.NoSelfAPI || !tok.RestrictSelf || tok.NarrowGen == 0 {
		t.Fatalf("vm-ext-foreign VMToken missing fail-closed restrictions: %+v", tok)
	}

	mu.Lock()
	defer mu.Unlock()
	if fleetListCalls != 0 {
		t.Fatalf("single-VM reconcile performed %d host-wide GET /v1/vms scans, want 0", fleetListCalls)
	}
	if len(otherTouched) != 0 {
		t.Fatalf("single-VM reconcile touched unrelated running VMs: %v", otherTouched)
	}
}
