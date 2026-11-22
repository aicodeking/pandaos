package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialSnapshotStickyWithUncertainRun(t *testing.T) {
	for _, uncertain := range []string{"unknown", "nonempty", "never-landed"} {
		t.Run(uncertain, func(t *testing.T) {
			e := setup(t)
			id := e.createVMWith(map[string]any{"no_env": true})
			if err := e.srv.store.Update(func(st *State) error {
				switch uncertain {
				case "unknown":
					delete(st.VMSecrets, runNamesKey(id))
				case "nonempty":
					setRunNames(st, id, []string{"OLD"}, false)
				case "never-landed":
					st.VMSecrets[envNeverKey(id)] = []string{"1"}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			published := false
			e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/release") {
					published = true
					back, err := OpenStore(e.dir)
					if err != nil {
						t.Error(err)
					} else {
						defer back.Close()
						parts := strings.Split(r.URL.Path, "/")
						sid := parts[len(parts)-2]
						if !snapshotSourceNoEnvIn(back.Snapshot(), sid) {
							t.Error("policy must be durable before release dispatch")
						}
					}
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			sid := e.r39Snapshot(id, "uncertain")
			if !published {
				t.Fatal("fixture did not reach snapshot publication")
			}
			if !e.srv.snapshotSourceNoEnv(sid) {
				t.Fatal("uncertain inventory must retain sticky disk policy")
			}
			if e.srv.snapshotSourceProtected(sid) {
				t.Fatal("uncertain inventory must not certify copied RAM")
			}
			back, err := OpenStore(e.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer back.Close()
			srv := &server{store: back}
			if !srv.snapshotSourceNoEnv(sid) {
				t.Fatal("sticky policy must be durable before snapshot publication")
			}
			code, out := e.createFrom(sid, nil)
			if code != 201 {
				t.Fatalf("clone = %d %v", code, out)
			}
			child := out["id"].(string)
			if !e.srv.noEnvRun(child) || e.freshCreatesOf(child) != 1 {
				t.Fatal("uncertified protected disk must cold-boot and stay no_env")
			}
		})
	}
}

func TestCredentialPossiblyDeliveredNamesBeforeDispatch(t *testing.T) {
	for _, path := range []string{"prompt", "prompts", "start"} {
		t.Run(path, func(t *testing.T) {
			e := setup(t)
			id := e.createVM()
			if code, out := e.do(e.owner, "PUT", "/v1/secrets/LATE", map[string]any{"value": "synthetic", "inject": true}); code != 200 {
				t.Fatalf("secret = %d %v", code, out)
			}
			seen := false
			e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" && (r.URL.Path == vmPath(id, "env") || r.URL.Path == vmPath(id, "exec") || strings.HasPrefix(r.URL.Path, vmPath(id, "agent"))) {
					seen = true
					names, known := runNamesOf(e.srv.store.Snapshot(), id)
					if !known || !slices.Contains(names, "LATE") {
						t.Error("possible delivery must be recorded before dispatch")
					}
					back, err := OpenStore(e.dir)
					if err != nil {
						t.Error(err)
					} else {
						persisted, known := runNamesOf(back.Snapshot(), id)
						if !known || !slices.Contains(persisted, "LATE") {
							t.Error("new possibly delivered key must be durable")
						}
						back.Close()
					}
					return nil, errors.New("synthetic ambiguous delivery")
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			body := map[string]any{"prompt": "fixture"}
			if path == "start" {
				body = map[string]any{"env": map[string]string{"CUSTOM": "fixture"}}
			}
			e.raw(e.owner, "POST", vmPath(id, path), body)
			if !seen {
				t.Fatal("fixture did not reach dispatch")
			}
			names, known := runNamesOf(e.srv.store.Snapshot(), id)
			if !known || !slices.Contains(names, "LATE") {
				t.Fatal("ambiguous result must retain the conservative union")
			}
		})
	}
}

func TestCredentialUnknownRunResumeDiscardsMemory(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, runNamesKey(id)); return nil }); err != nil {
		t.Fatal(err)
	}
	code, out := e.do(e.owner, "POST", vmPath(id, "start"), nil)
	if code != 200 || e.freshBootsOf(id) != 1 {
		t.Fatalf("unknown run must cold-boot: %d %v", code, out)
	}
}

func TestCredentialReconcileRechecksMissingInventory(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	id := e.createVM()
	// An ordinary stale inventory response can omit a still-existing VM.
	e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Path == "/v1/vms" {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"vms":[]}`)), Request: r}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tok := e.srv.store.Snapshot().VMTokens[id]; tok == nil || tok.Retired {
		t.Fatal("a stale inventory must not retire a currently existing VM")
	}
}

func TestCredentialForkKeepsPreparedParentMetadata(t *testing.T) {
	e := setup(t)
	for _, name := range []string{"OLD", "NEW"} {
		if code, out := e.do(e.owner, "PUT", "/v1/secrets/"+name, map[string]any{"value": "synthetic", "inject": true}); code != 200 {
			t.Fatalf("secret = %d %v", code, out)
		}
	}
	oldEnv := e.saveEnv("old", map[string]string{"PINNED": "old"}, []string{"OLD"})
	e.saveEnv("new", map[string]string{"PINNED": "new"}, []string{"NEW"})
	id := e.createVMWith(map[string]any{"environment": "old", "env": map[string]string{"EXTRA": "old"}})
	forkPreparedHook = func(_ *server, parent string) {
		if parent != id {
			return
		}
		e.stopStart(id, map[string]any{"environment": "new", "env": map[string]string{"EXTRA": "new"}})
	}
	t.Cleanup(func() { forkPreparedHook = nil })
	code, out := e.do(e.owner, "POST", vmPath(id, "fork"), nil)
	if code != 201 {
		t.Fatalf("fork = %d %v", code, out)
	}
	child := out["id"].(string)
	st := e.srv.store.Snapshot()
	if !slices.Equal(st.VMSecrets[child], []string{"OLD"}) || st.VMEnvs[child] != oldEnv || st.VMEnvPins[child].Vars["PINNED"] != "old" || st.VMExtraEnv[child]["EXTRA"] != "old" {
		t.Fatal("child policy, pin and extras must describe the parent at prepare")
	}
	names, known := runNamesOf(st, child)
	if !known || !slices.Equal(names, []string{"OLD"}) {
		t.Fatalf("prepared names = %v/%v", names, known)
	}
	if e.guestVar(child, "PINNED") != "old" || e.guestVar(child, "EXTRA") != "old" || e.guestVar(child, "NEW") != nil {
		t.Fatal("child must receive the captured parent environment")
	}
}

func TestCredentialStickyShareWithUnknownInventory(t *testing.T) {
	e := setup(t)
	id := e.createVMWith(map[string]any{"no_env": true})
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, runNamesKey(id)); return nil }); err != nil {
		t.Fatal(err)
	}
	sid := e.r39Snapshot(id, "uncertain")
	_, other := e.newTeam("receiver")
	code, share := e.do(e.owner, "POST", vmPath(id, "snapshots", sid, "shares"), nil)
	if code != 201 {
		t.Fatalf("share = %d %v", code, share)
	}
	code, imp := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"code": share["code"]})
	if code != 201 {
		t.Fatalf("import = %d %v", code, imp)
	}
	isid := imp["id"].(string)
	if !e.srv.snapshotSourceNoEnv(isid) || e.srv.snapshotSourceProtected(isid) {
		t.Fatal("share must preserve policy without certifying unknown RAM")
	}
	code, out := e.do(other, "POST", "/v1/vms", map[string]any{"snapshot": isid})
	if code != 409 {
		t.Fatalf("uncertified foreign protected copy = %d %v", code, out)
	}
}

func TestCredentialPendingStartRetainsPinnedIntent(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/OLD", map[string]any{"value": "synthetic", "inject": true})
	e.saveEnv("web", map[string]string{"PINNED": "original"}, []string{})
	id := e.createVM()
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	e.failNextStarts(1)
	code, out := e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"environment": "web", "env": map[string]string{"EXTRA": "original"}})
	if code != 502 {
		t.Fatalf("failed start = %d %v", code, out)
	}
	e.putEnvVar("web", "PINNED", "edited")
	code, out = e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"environment": "web"})
	if code != 200 {
		t.Fatalf("retry = %d %v", code, out)
	}
	if e.guestVar(id, "PINNED") != "original" || e.guestVar(id, "EXTRA") != "original" {
		t.Fatal("retry must use the pinned transition and extras")
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	st := back.Snapshot()
	if st.VMTransitions[id] != nil || st.VMEnvPins[id].Vars["PINNED"] != "original" || st.VMExtraEnv[id]["EXTRA"] != "original" {
		t.Fatal("completed transition and effective policy must persist atomically")
	}
}

func TestCredentialStartRevalidatesBeforeMutations(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	tok := e.newKey("vms:read", "vms:write", "exec")
	kid := keyID(e, tok)
	unlock := startLocks.lock(id)
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()
	done := make(chan int, 1)
	go func() {
		code, _ := e.do(tok, "POST", vmPath(id, "start"), map[string]any{"no_env": true})
		done <- code
	}()
	waitR50LifecycleWaiter(t, id)
	if err := e.srv.store.Update(func(st *State) error { st.Keys[kid].Scopes = []string{"vms:read"}; return nil }); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case code := <-done:
		if code != 403 {
			t.Fatalf("revoked start = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start stalled")
	}
	st := e.srv.store.Snapshot()
	if st.VMTransitions[id] != nil || e.srv.noEnvRun(id) || e.nativeVM(id)["state"] != "stopped" {
		t.Fatal("revoked caller must not change lifecycle or credential policy")
	}
}
