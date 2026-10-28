package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Status is what /run/panda-agentd/status.json (0644) holds. The desktop dock
// reads it, so it must never contain secrets: the pairing code lives only in
// pairing-code (0640, group wheel).
type Status struct {
	Paired      bool       `json:"paired"`
	Pairing     bool       `json:"pairing"` // a pairing code is waiting for approval
	Relay       string     `json:"relay"`
	Slots       int        `json:"slots"`
	Running     int        `json:"running"`
	Completed   int        `json:"completed"`
	Failed      int        `json:"failed"`
	Version     string     `json:"version"`
	Since       time.Time  `json:"since"`
	LastError   string     `json:"lastError"`             // "" when the last relay call worked
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"` // set with LastError
}

// writeFileAtomic writes via a temp file plus rename, so readers never see a
// partial file. gid >= 0 sets the file's group before it becomes visible.
func writeFileAtomic(path string, data []byte, mode os.FileMode, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if gid >= 0 {
		if err := f.Chown(-1, gid); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func writeStatus(path string, st Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o644, -1)
}

// statusCmd implements `panda-agentd status [-json] [-run-dir DIR]`.
func statusCmd(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("panda-agentd status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw status.json")
	runDir := fs.String("run-dir", envOr(getenv, "PANDA_AGENTD_RUN_DIR", "/run/panda-agentd"), "runtime files (env PANDA_AGENTD_RUN_DIR)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(*runDir, "status.json"))
	if err != nil {
		return fmt.Errorf("panda-agentd is not running? %w", err)
	}
	if *asJSON {
		_, err := out.Write(b)
		return err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("bad status.json: %w", err)
	}
	pairing := "yes"
	switch {
	case st.Paired:
	case !st.Pairing:
		pairing = "no"
	default:
		pairing = "no, waiting for approval (run as root or wheel to see the code)"
		if code, err := os.ReadFile(filepath.Join(*runDir, "pairing-code")); err == nil {
			pairing = "no, pairing code " + strings.TrimSpace(string(code))
		}
	}
	lastErr := "none"
	if st.LastError != "" {
		lastErr = st.LastError
		if st.LastErrorAt != nil {
			lastErr += " (at " + st.LastErrorAt.Local().Format(time.RFC3339) + ")"
		}
	}
	_, err = fmt.Fprintf(out, "panda-agentd %s, up since %s\nrelay:   %s\npaired:  %s\njobs:    %d/%d running, %d completed, %d failed\nerror:   %s\n",
		st.Version, st.Since.Local().Format(time.RFC3339), st.Relay, pairing, st.Running, st.Slots, st.Completed, st.Failed, lastErr)
	return err
}
