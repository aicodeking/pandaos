package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ---- A: boat snapshot pull security ----

func TestPullSnapshotPermissionsAndCleanupOnError(t *testing.T) {
	t.Run("writes_0600_and_tightens_existing_file", func(t *testing.T) {
		var srv *httptest.Server
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v1/snapshots/snap_ok/download", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(obj{
				"chunks": []obj{{"signedUrl": srv.URL + "/blob/snap_ok?sig=abc"}},
			})
		})
		mux.HandleFunc("GET /blob/snap_ok", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("signed download leaked Authorization header: %q", r.Header.Get("Authorization"))
			}
			w.Write([]byte("SECRET-ARCHIVE-DATA"))
		})
		srv = httptest.NewServer(mux)
		defer srv.Close()

		outDir := t.TempDir()
		dst := filepath.Join(outDir, "snap_ok.tar.zst")
		// Pre-create with permissive 0644 to prove permissions are tightened to 0600.
		if err := os.WriteFile(dst, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}

		a, out, errw := upgradeDeleteApp(t, srv.URL)
		if code := a.run([]string{"snapshot", "pull", "snap_ok", "-o", outDir}); code != 0 {
			t.Fatalf("pull exit %d: out=%q err=%q", code, out, errw)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != "SECRET-ARCHIVE-DATA" {
			t.Fatalf("downloaded file = %q err=%v", got, err)
		}
		if runtime.GOOS != "windows" {
			st, err := os.Stat(dst)
			if err != nil {
				t.Fatal(err)
			}
			if perm := st.Mode().Perm(); perm != 0o600 {
				t.Fatalf("archive permissions = %o, want 0600", perm)
			}
		}
	})

	t.Run("removes_partial_file_on_stream_error", func(t *testing.T) {
		var srv *httptest.Server
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v1/snapshots/snap_trunc/download", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(obj{
				"chunks": []obj{{"signedUrl": srv.URL + "/blob/trunc"}},
			})
		})
		mux.HandleFunc("GET /blob/trunc", func(w http.ResponseWriter, r *http.Request) {
			// Advertise 4096 bytes, write a few bytes, then hijack and close abruptly.
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("partial-secret-bytes"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
		})
		srv = httptest.NewServer(mux)
		defer srv.Close()

		outDir := t.TempDir()
		a, out, errw := upgradeDeleteApp(t, srv.URL)
		if code := a.run([]string{"snapshot", "pull", "snap_trunc", "-o", outDir}); code == 0 {
			t.Fatalf("truncated pull succeeded: out=%q err=%q", out, errw)
		}
		dst := filepath.Join(outDir, "snap_trunc.tar.zst")
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("partial archive left behind at %s (stat err=%v)", dst, err)
		}
	})

	t.Run("allows_https_signed_url_on_different_origin", func(t *testing.T) {
		blobSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("TLS-ARCHIVE"))
		}))
		defer blobSrv.Close()

		apiMux := http.NewServeMux()
		apiMux.HandleFunc("GET /api/v1/snapshots/snap_tls/download", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(obj{
				"chunks": []obj{{"signedUrl": blobSrv.URL + "/chunk.tar.zst"}},
			})
		})
		apiSrv := httptest.NewServer(apiMux)
		defer apiSrv.Close()

		outDir := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{
			out:  &out,
			errw: &errw,
			cfg:  Config{URL: apiSrv.URL, Key: "boat_k"},
			c:    &client{base: apiSrv.URL, key: "boat_k", hc: blobSrv.Client()},
		}
		if err := a.pullSnapshot("snap_tls", outDir); err != nil {
			t.Fatalf("pullSnapshot over https failed: %v", err)
		}
		dst := filepath.Join(outDir, "snap_tls.tar.zst")
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != "TLS-ARCHIVE" {
			t.Fatalf("got=%q err=%v", got, err)
		}
		if runtime.GOOS != "windows" {
			st, _ := os.Stat(dst)
			if st.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%o want 0600", st.Mode().Perm())
			}
		}
	})
}

func TestPullSnapshotRejectsUnsafeSID(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	badSIDs := []string{
		"../evil",
		"sub/snap",
		"..",
		".",
		"snap 1",
		"snap\n1",
		"snap;rm",
		`a\b`,
	}
	outDir := t.TempDir()
	for _, sid := range badSIDs {
		a, out, errw := upgradeDeleteApp(t, srv.URL)
		code := a.run([]string{"snapshot", "pull", sid, "-o", outDir})
		if code == 0 {
			t.Errorf("sid=%q accepted: out=%q", sid, out)
		}
		if !strings.Contains(errw.String(), "invalid snapshot id") {
			t.Errorf("sid=%q unexpected stderr: %q", sid, errw)
		}
	}
	// Also test leading-dash sid directly (flag parser catches it on CLI before pullSnapshot).
	a, _, _ := upgradeDeleteApp(t, srv.URL)
	a.c = &client{base: srv.URL, key: "k", hc: &http.Client{}}
	if err := a.pullSnapshot("-snap", outDir); err == nil || !strings.Contains(err.Error(), "invalid snapshot id") {
		t.Errorf("pullSnapshot(-snap) err=%v, want invalid snapshot id", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("unsafe sid sent %d HTTP requests, want 0", n)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Fatalf("unsafe sid created files: %v", entries)
	}
}

func TestPullSnapshotURLSchemeAndOriginCheck(t *testing.T) {
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		w.Write([]byte("SHOULD-NOT-BE-FETCHED"))
	}))
	defer foreign.Close()

	var signedURL string
	var apiSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snapshots/snap_1/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(obj{"chunks": []obj{{"signedUrl": signedURL}}})
	})
	mux.HandleFunc("GET /redir-to-foreign", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/secret", http.StatusFound)
	})
	apiSrv = httptest.NewServer(mux)
	defer apiSrv.Close()

	badURLs := []struct {
		name string
		url  string
	}{
		{"foreign_http", foreign.URL + "/chunk"},
		{"file_scheme", "file:///etc/passwd"},
		{"ftp_scheme", "ftp://example.com/snap.tar.zst"},
		{"javascript_scheme", "javascript:alert(1)"},
		{"missing_host", "https:///no-host"},
		{"redirect_to_foreign_http", apiSrv.URL + "/redir-to-foreign"},
	}

	outDir := t.TempDir()
	for _, tc := range badURLs {
		t.Run(tc.name, func(t *testing.T) {
			signedURL = tc.url
			a, out, errw := upgradeDeleteApp(t, apiSrv.URL)
			code := a.run([]string{"snapshot", "pull", "snap_1", "-o", outDir})
			if code == 0 {
				t.Fatalf("url=%q should fail; out=%q", tc.url, out)
			}
			if !strings.Contains(errw.String(), "refusing") {
				t.Errorf("url=%q expected clear refusal error, got %q", tc.url, errw)
			}
		})
	}
	if n := foreignHits.Load(); n != 0 {
		t.Fatalf("disallowed signed URL sent %d requests to foreign server, want 0", n)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Fatalf("rejected pull created files: %v", entries)
	}

	// Unit-check HTTPS BOAT_URL never allows an http:// signedUrl, even on the same host.
	if err := validateSnapshotDownloadURL("http://boat.example/chunk", "https://boat.example"); err == nil {
		t.Error("expected http downgrade from https BOAT_URL to be rejected")
	}
	// Default port normalization for same-origin local http dev.
	if err := validateSnapshotDownloadURL("http://localhost:80/chunk", "http://localhost"); err != nil {
		t.Errorf("same-origin default port rejected: %v", err)
	}
	if err := validateSnapshotDownloadURL("http://localhost:8080/chunk", "http://localhost:8081"); err == nil {
		t.Error("different port on same host should be rejected")
	}
	if err := validateSnapshotDownloadURL("https://s3.example.com/bucket/snap", "https://boat.example"); err != nil {
		t.Errorf("https signed url rejected: %v", err)
	}
}

// ---- B: SSH jump host (IPv4 / IPv6 / hostname) and config id ----

func TestSSHJumpArgv(t *testing.T) {
	cases := []struct {
		name      string
		jump      string
		wantJump  string
		wantProxy string
	}{
		{
			name:      "ipv4_with_port",
			jump:      "jump@192.0.2.10:2222",
			wantJump:  "-p 2222 -- jump@192.0.2.10",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 -- jump@192.0.2.10",
		},
		{
			name:      "ipv4_default_port",
			jump:      "jump@192.0.2.10",
			wantJump:  "-p 22 -- jump@192.0.2.10",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 22 -- jump@192.0.2.10",
		},
		{
			name:      "bracketed_ipv6_with_port",
			jump:      "jump@[2001:db8::1]:2222",
			wantJump:  "-p 2222 -- jump@2001:db8::1",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 -- jump@2001:db8::1",
		},
		{
			name:      "bracketed_ipv6_without_port",
			jump:      "jump@[2001:db8::1]",
			wantJump:  "-p 22 -- jump@2001:db8::1",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 22 -- jump@2001:db8::1",
		},
		{
			name:      "unbracketed_ipv6",
			jump:      "jump@2001:db8::1",
			wantJump:  "-p 22 -- jump@2001:db8::1",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 22 -- jump@2001:db8::1",
		},
		{
			name:      "hostname_with_port",
			jump:      "boat@gw.example:2200",
			wantJump:  "-p 2200 -- boat@gw.example",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2200 -- boat@gw.example",
		},
		{
			name:      "hostname_default_port",
			jump:      "boat@gw.example",
			wantJump:  "-p 22 -- boat@gw.example",
			wantProxy: "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 22 -- boat@gw.example",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !validSSHJump(tc.jump) {
				t.Fatalf("validSSHJump(%q) = false, want true", tc.jump)
			}
			gotJump, err := sshJumpArgs(tc.jump)
			if err != nil {
				t.Fatalf("sshJumpArgs(%q) err: %v", tc.jump, err)
			}
			if gotJump != tc.wantJump {
				t.Fatalf("sshJumpArgs(%q) = %q, want %q", tc.jump, gotJump, tc.wantJump)
			}
			if strings.ContainsAny(gotJump, "[]*?{}()") {
				t.Fatalf("sshJumpArgs(%q) leaked shell glob characters: %q", tc.jump, gotJump)
			}

			info := obj{"user": "ubuntu", "host": "10.0.0.5", "port": "22", "jump": tc.jump}
			argv, err := sshArgv("vm_1", info, "-t")
			if err != nil {
				t.Fatalf("sshArgv err: %v", err)
			}
			wantArgv := []string{
				"ssh", "-p", "22",
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "HostKeyAlias=boat-vm_1",
				"-o", tc.wantProxy,
				"-t",
				"ubuntu@10.0.0.5",
			}
			if !slices.Equal(argv, wantArgv) {
				t.Fatalf("argv = %#v\nwant   %#v", argv, wantArgv)
			}
		})
	}

	// Reject malformed bracketed/IPv6 jumps.
	for _, bad := range []string{
		"jump@[192.0.2.1]:22",
		"jump@[2001:db8::1]:",
		"jump@[2001:db8::1]extra",
		"jump@[not-an-ip]:22",
		"jump@2001:db8::1:zzzz",
	} {
		if validSSHJump(bad) {
			t.Errorf("validSSHJump(%q) = true, want false", bad)
		}
		if _, err := sshJumpArgs(bad); err == nil {
			t.Errorf("sshJumpArgs(%q) succeeded, want error", bad)
		}
	}
}

func TestSSHArgvAndSetCurrentRejectInvalidID(t *testing.T) {
	info := obj{"user": "ubuntu", "host": "10.0.0.5", "port": "22", "jump": "jump@[2001:db8::1]:2222"}
	badIDs := []string{
		"vm 1",
		"vm1\nHost *",
		"../evil",
		"*",
		"-flag",
		"",
	}

	for _, id := range badIDs {
		t.Run("sshArgv_"+id, func(t *testing.T) {
			if argv, err := sshArgv(id, info); err == nil {
				t.Fatalf("sshArgv(%q) succeeded with argv=%v, want error", id, argv)
			}
		})
		t.Run("setCurrent_"+id, func(t *testing.T) {
			home := t.TempDir()
			var errw bytes.Buffer
			a := &app{
				errw:    &errw,
				shellID: 4242,
				getenv: func(k string) string {
					if k == "BOAT_CONFIG" {
						return filepath.Join(home, "boat", "config.json")
					}
					return ""
				},
			}
			a.setCurrent(id)
			if _, err := os.Stat(a.currentFile()); !os.IsNotExist(err) {
				t.Fatalf("setCurrent(%q) wrote current file (stat err=%v)", id, err)
			}
			if !strings.Contains(errw.String(), "invalid VM id") {
				t.Errorf("setCurrent(%q) stderr = %q", id, errw.String())
			}
		})
	}

	// Valid ID writes the current file with 0600 permissions.
	t.Run("setCurrent_valid", func(t *testing.T) {
		home := t.TempDir()
		var errw bytes.Buffer
		a := &app{
			errw:    &errw,
			shellID: 4243,
			getenv: func(k string) string {
				if k == "BOAT_CONFIG" {
					return filepath.Join(home, "boat", "config.json")
				}
				return ""
			},
		}
		a.setCurrent("vm_ok-1")
		b, err := os.ReadFile(a.currentFile())
		if err != nil || strings.TrimSpace(string(b)) != "vm_ok-1" {
			t.Fatalf("current file = %q err=%v", b, err)
		}
		if errw.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errw.String())
		}
	})

	// End-to-end: boat ssh and boat forward reject a server-returned VM id with a space or newline.
	for _, badID := range []string{"vm 1", "vm1\nProxyCommand sh -c evil"} {
		t.Run("e2e_"+badID, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj{"id": badID, "name": "dev", "state": "running"})
			})
			mux.HandleFunc("POST /v1/vms/{id}/ssh", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"user":"ubuntu","host":"10.0.0.5","port":22}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			home := t.TempDir()
			os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
			os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA k\n"), 0o600)
			env := map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "cfg.json"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}

			// boat ssh
			var out, errw bytes.Buffer
			ran := false
			a := &app{
				out: &out, errw: &errw, in: strings.NewReader(""),
				getenv:   func(k string) string { return env[k] },
				execProg: func([]string) error { ran = true; return nil },
				runProg:  func([]string) error { ran = true; return nil },
			}
			if code := a.run([]string{"ssh", "target"}); code == 0 || ran {
				t.Fatalf("boat ssh badID=%q: code=%d ran=%v err=%q", badID, code, ran, errw)
			}
			// boat forward --json
			out.Reset()
			errw.Reset()
			a.json = false
			if code := a.run([]string{"forward", "target", "--remote", "8080", "--json"}); code == 0 || ran {
				t.Fatalf("boat forward badID=%q: code=%d ran=%v err=%q", badID, code, ran, errw)
			}
		})
	}
}

// ---- C: scp tilde expansion ----

func TestSCPTildeExpansion(t *testing.T) {
	// Unit check homeRel mappings.
	relCases := map[string]string{
		"~":            "",
		"~/":           "",
		"~/a":          "a",
		"~/sub/a":      "sub/a",
		"~//a":         "a",
		"~/dir/":       "dir/",
		"/home/user":   "",
		"/home/user/":  "",
		"/home/user/a": "a",
		"/tmp/a":       "/tmp/a",
		"rel/a":        "rel/a",
	}
	for in, want := range relCases {
		if got := homeRel(in); got != want {
			t.Errorf("homeRel(%q) = %q, want %q", in, got, want)
		}
	}

	var mu sync.Mutex
	var putPaths []string
	var getPaths []string
	var execCmds [][]string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cmd []string `json:"cmd"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		execCmds = append(execCmds, body.Cmd)
		mu.Unlock()
		// Report a VM home distinct from local HOME.
		vmHome := base64.StdEncoding.EncodeToString([]byte("/home/vmuser"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"stream":"stdout","data_b64":"`+vmHome+`"}`+"\n"+`{"exit":0}`+"\n")
	})
	mux.HandleFunc("PUT /v1/vms/vm1/files", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		putPaths = append(putPaths, r.URL.Query().Get("path"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"bytes":5}`)
	})
	mux.HandleFunc("GET /v1/vms/vm1/files", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getPaths = append(getPaths, r.URL.Query().Get("path"))
		mu.Unlock()
		io.WriteString(w, "hello")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	localFile := filepath.Join(tmp, "a")
	os.WriteFile(localFile, []byte("hello"), 0o600)
	localDir := filepath.Join(tmp, "mydir")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "f.txt"), []byte("f"), 0o600)

	runSCP := func(args ...string) {
		t.Helper()
		a, out, errw := upgradeDeleteApp(t, srv.URL)
		if code := a.run(append([]string{"scp"}, args...)); code != 0 {
			t.Fatalf("boat scp %v failed (exit %d): out=%q err=%q", args, code, out, errw)
		}
	}

	// File upload cases
	uploadCases := []struct {
		remote   string
		wantPath string
		wantExec bool
	}{
		{"vm1:~/a", "/home/vmuser/a", true},
		{"vm1:~", "/home/vmuser/a", true},
		{"vm1:~/", "/home/vmuser/a", true},
		{"vm1:~/sub/a", "/home/vmuser/sub/a", true},
		{"vm1:~/sub/", "/home/vmuser/sub/a", true},
		{"vm1:~//a", "/home/vmuser/a", true},
		{"vm1:/home/user/a", "/home/vmuser/a", true},
		{"vm1:/home/user", "/home/vmuser/a", true},
		{"vm1:/tmp/a", "/tmp/a", false},
	}
	for _, tc := range uploadCases {
		t.Run("up_"+tc.remote, func(t *testing.T) {
			mu.Lock()
			putPaths, execCmds = nil, nil
			mu.Unlock()
			runSCP(localFile, tc.remote)
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(putPaths, []string{tc.wantPath}) {
				t.Fatalf("remote=%q PUT paths=%v, want [%s]", tc.remote, putPaths, tc.wantPath)
			}
			if tc.wantExec && len(execCmds) == 0 {
				t.Errorf("remote=%q expected VM $HOME lookup", tc.remote)
			}
			if !tc.wantExec && len(execCmds) != 0 {
				t.Errorf("remote=%q should not query VM $HOME, got %v", tc.remote, execCmds)
			}
		})
	}

	// File download cases
	t.Run("down_tilde", func(t *testing.T) {
		mu.Lock()
		getPaths = nil
		mu.Unlock()
		dst := filepath.Join(tmp, "dl.txt")
		runSCP("vm1:~/a", dst)
		mu.Lock()
		got := slices.Clone(getPaths)
		mu.Unlock()
		if !slices.Equal(got, []string{"/home/vmuser/a"}) {
			t.Fatalf("GET paths=%v, want [/home/vmuser/a]", got)
		}
	})

	// Recursive upload to vm1:~ and vm1:~/proj passes home-relative target to unpackScript (no leading ~)
	t.Run("dir_up_tilde", func(t *testing.T) {
		for _, tc := range []struct {
			remote string
			wantTo string
		}{
			{"vm1:~", "mydir"},
			{"vm1:~/", "mydir"},
			{"vm1:~/proj", "proj"},
		} {
			mu.Lock()
			execCmds = nil
			mu.Unlock()
			runSCP("-r", localDir, tc.remote)
			mu.Lock()
			cmds := slices.Clone(execCmds)
			mu.Unlock()
			if len(cmds) != 1 || len(cmds[0]) < 4 || cmds[0][3] != tc.wantTo {
				t.Fatalf("remote=%q unpack cmd=%v, want target %q", tc.remote, cmds, tc.wantTo)
			}
		}
	})
}

// ---- D: exec flag conflicts and extra positional args ----

func TestExecFlagConflictsAndExtraArgs(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/vms/vm1":
			io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
		case r.Method == "GET" && r.URL.Path == "/v1/vms/vm1/commands/c1":
			io.WriteString(w, `{"id":"c1","state":"running"}`)
		case r.Method == "GET" && r.URL.Path == "/v1/vms/vm1/commands/c1/logs":
			if r.URL.Query().Get("follow") == "1" {
				io.WriteString(w, `{"exit":0}`+"\n")
				return
			}
			io.WriteString(w, `{"data_b64":"","next_offset":0,"running":false,"exit":0}`)
		case r.Method == "DELETE" && r.URL.Path == "/v1/vms/vm1/commands/c1":
			io.WriteString(w, `{"ok":true,"id":"c1","signal":"TERM"}`)
		default:
			w.WriteHeader(404)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	badCases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"status_and_attach", []string{"exec", "vm1", "--status", "c1", "--attach", "c2"}, "cannot be combined"},
		{"status_and_kill", []string{"exec", "vm1", "--status", "c1", "--kill", "c2"}, "cannot be combined"},
		{"attach_and_kill", []string{"exec", "vm1", "--attach", "c1", "--kill", "c2"}, "cannot be combined"},
		{"all_three", []string{"exec", "vm1", "--status", "c1", "--attach", "c2", "--kill", "c3"}, "cannot be combined"},
		{"detach_and_status", []string{"exec", "vm1", "--detach", "--status", "c1"}, "cannot be combined with --detach"},
		{"detach_and_attach", []string{"exec", "vm1", "--detach", "--attach", "c1"}, "cannot be combined with --detach"},
		{"detach_and_kill", []string{"exec", "vm1", "--detach", "--kill", "c1"}, "cannot be combined with --detach"},
		{"empty_status", []string{"exec", "vm1", "--status", ""}, "--status requires a command id"},
		{"empty_attach", []string{"exec", "vm1", "--attach", ""}, "--attach requires a command id"},
		{"empty_kill", []string{"exec", "vm1", "--kill", ""}, "--kill requires a command id"},
		{"extra_pos_with_attach", []string{"exec", "vm1", "extra", "--attach", "c1"}, "unexpected arguments"},
		{"extra_pos_with_status", []string{"exec", "vm1", "extra", "--status", "c1"}, "unexpected arguments"},
		{"extra_pos_with_kill", []string{"exec", "vm1", "extra", "--kill", "c1"}, "unexpected arguments"},
		{"cmd_after_dashdash_with_attach", []string{"exec", "vm1", "--attach", "c1", "--", "ls"}, "unexpected command"},
		{"cmd_after_dashdash_with_status", []string{"exec", "vm1", "--status", "c1", "--", "ls"}, "unexpected command"},
		{"cmd_after_dashdash_with_kill", []string{"exec", "vm1", "--kill", "c1", "--", "ls"}, "unexpected command"},
		{"extra_pos_without_dashdash", []string{"exec", "vm1", "echo", "hi"}, "unexpected arguments"},
		{"extra_pos_with_dashdash", []string{"exec", "vm1", "extra", "--", "ls"}, "expected <vm>"},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			a, out, errw := upgradeDeleteApp(t, srv.URL)
			code := a.run(tc.args)
			if code != 2 {
				t.Fatalf("args=%v exit=%d, want 2; out=%q err=%q", tc.args, code, out, errw)
			}
			if !strings.Contains(errw.String(), tc.wantErr) {
				t.Errorf("args=%v stderr=%q missing %q", tc.args, errw, tc.wantErr)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("args=%v made %d API calls, want 0", tc.args, n)
			}
		})
	}

	// Valid invocations still succeed.
	for _, args := range [][]string{
		{"exec", "vm1", "--status", "c1"},
		{"exec", "vm1", "--attach", "c1"},
		{"exec", "vm1", "--kill", "c1"},
	} {
		a, out, errw := upgradeDeleteApp(t, srv.URL)
		if code := a.run(args); code != 0 {
			t.Errorf("valid args=%v failed (exit %d): out=%q err=%q", args, code, out, errw)
		}
	}
}

// ---- E: --cwd passthrough on exec and prompt ----

func TestExecAndPromptCwdPassthrough(t *testing.T) {
	var mu sync.Mutex
	var lastCwd string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	record := func(resp string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body obj
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			lastCwd = s(body, "cwd")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, resp)
		}
	}
	mux.HandleFunc("POST /v1/vms/vm1/exec", record(`{"exit":0}`+"\n"))
	mux.HandleFunc("POST /v1/vms/vm1/commands", record(`{"id":"c1","state":"running","cmd":["pwd"]}`))
	mux.HandleFunc("POST /v1/vms/vm1/prompt", record(`{"exit":0}`+"\n"))
	mux.HandleFunc("POST /v1/vms/vm1/prompts", record(`{"id":"p1","state":"running","conversation_id":"conv_1"}`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cwds := []string{"/var/tmp", "~", "~/work", "sub/dir"}
	for _, cwd := range cwds {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"exec_sync", []string{"exec", "vm1", "--cwd", cwd, "--", "pwd"}},
			{"exec_detach", []string{"exec", "vm1", "--detach", "--cwd", cwd, "--", "pwd"}},
			{"prompt_sync", []string{"prompt", "vm1", "--cwd", cwd, "hi"}},
			{"prompt_async", []string{"prompt", "vm1", "--async", "--cwd", cwd, "hi"}},
		} {
			t.Run(tc.name+"_"+cwd, func(t *testing.T) {
				mu.Lock()
				lastCwd = "<unset>"
				mu.Unlock()
				a, out, errw := upgradeDeleteApp(t, srv.URL)
				if code := a.run(tc.args); code != 0 {
					t.Fatalf("args=%v exit=%d out=%q err=%q", tc.args, code, out, errw)
				}
				mu.Lock()
				got := lastCwd
				mu.Unlock()
				if got != cwd {
					t.Fatalf("args=%v sent cwd=%q, want unchanged %q", tc.args, got, cwd)
				}
			})
		}
	}
}
