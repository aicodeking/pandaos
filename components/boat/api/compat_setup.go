package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"
)

// boat.dev's create `files` and `setupScript` (docs.boat.dev create-sandbox):
// files are written before the sandbox is ready (here: before create answers);
// relative paths resolve against the user's home, and only the home and /tmp
// take files. The script then runs in the background as the sandbox user with
// the sandbox env. A failed write or a non-zero exit sets setupStatus "failed"
// and setupError (the exit code and a stderr tail); create itself succeeds.

const (
	maxCompatFiles       = 32
	maxCompatSetupScript = 64 << 10
	maxSetupErrTail      = 2000
)

type compatFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // utf8 (default) | base64
}

// CompatSetup: the state of a create's files + setupScript, per VM.
type CompatSetup struct {
	Status string    `json:"status"` // pending | running | done | failed
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

type preparedFile struct {
	path string
	data []byte
}

func (s *server) homeDir() string {
	if s.cfg.SSHUser == "root" {
		return "/root"
	}
	return "/home/" + s.cfg.SSHUser
}

// prepareCompatFiles validates files; "" on success, else the 400 message.
func (s *server) prepareCompatFiles(files []compatFile) ([]preparedFile, string) {
	if len(files) > maxCompatFiles {
		return nil, fmt.Sprintf("at most %d files", maxCompatFiles)
	}
	home := s.homeDir()
	seen := map[string]bool{}
	var out []preparedFile
	for _, f := range files {
		p := f.Path
		switch {
		case p == "" || strings.ContainsRune(p, 0) || len(p) > 4096:
			return nil, "each file needs a path"
		case p == "/home/user" || strings.HasPrefix(p, "/home/user/"): // boat.dev's home
			p = home + strings.TrimPrefix(p, "/home/user")
		case !strings.HasPrefix(p, "/"):
			p = home + "/" + p
		}
		p = path.Clean(p)
		if !strings.HasPrefix(p, home+"/") && !strings.HasPrefix(p, "/tmp/") {
			return nil, "file " + f.Path + ": paths must be under " + home + " (or /home/user) or /tmp"
		}
		if seen[p] {
			return nil, "file " + f.Path + " is listed twice"
		}
		seen[p] = true
		data := []byte(f.Content)
		switch f.Encoding {
		case "", "utf8":
		case "base64":
			b, err := base64.StdEncoding.DecodeString(f.Content)
			if err != nil {
				return nil, "file " + f.Path + ": content is not base64"
			}
			data = b
		default:
			return nil, "file " + f.Path + ": encoding must be utf8 or base64"
		}
		if int64(len(data)) > s.cfg.MaxFileBytes {
			return nil, "file " + f.Path + " is too large"
		}
		out = append(out, preparedFile{path: p, data: data})
	}
	return out, ""
}

func (s *server) setCompatSetup(vmID string, cs CompatSetup) {
	cs.At = time.Now().UTC()
	if err := s.store.Update(func(st *State) error {
		if st.CompatSetups == nil {
			st.CompatSetups = map[string]*CompatSetup{}
		}
		st.CompatSetups[vmID] = &cs
		return nil
	}); err != nil {
		log.Printf("vm %s: record setup status: %v", vmID, err)
	}
}

// compatSetupVM writes the files now and starts the script in the background.
func (s *server) compatSetupVM(r *http.Request, vmID string, files []preparedFile, script string) {
	if len(files) == 0 && script == "" {
		return
	}
	for _, f := range files {
		// Parents first: boatd's file write needs the directory.
		if dir := path.Dir(f.path); dir != "/tmp" && dir != s.homeDir() {
			s.execCapture(r, vmID, []string{"mkdir", "-p", "--", dir}, 30)
		}
		if err := s.putVMFile(r, vmID, f.path, strings.NewReader(string(f.data))); err != nil {
			s.setCompatSetup(vmID, CompatSetup{Status: "failed", Error: "write " + f.path + ": " + err.Error()})
			return
		}
	}
	if script == "" {
		s.setCompatSetup(vmID, CompatSetup{Status: "done"})
		return
	}
	s.setCompatSetup(vmID, CompatSetup{Status: "running"})
	go s.runCompatSetup(vmID, script)
}

func (s *server) runCompatSetup(vmID, script string) {
	ctx, cancel := context.WithTimeout(s.baseCtx(), buildExecS*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://boat-api/internal/compat-setup", nil)
	_, stderr, exit, err := s.execCapture(req, vmID, []string{"bash", "-lc", script}, buildExecS)
	switch {
	case err != nil:
		s.setCompatSetup(vmID, CompatSetup{Status: "failed", Error: "setup script lost: " + err.Error()})
	case exit != 0:
		if len(stderr) > maxSetupErrTail {
			stderr = stderr[len(stderr)-maxSetupErrTail:]
		}
		s.setCompatSetup(vmID, CompatSetup{Status: "failed", Error: fmt.Sprintf("exit %d: %s", exit, strings.TrimSpace(stderr))})
	default:
		s.setCompatSetup(vmID, CompatSetup{Status: "done"})
	}
}
