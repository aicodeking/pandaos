package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Status is what /run/panda-agentd/status.json holds. The desktop dock reads it.
type Status struct {
	Paired      bool      `json:"paired"`
	PairingCode string    `json:"pairingCode,omitempty"` // only while pairing
	Relay       string    `json:"relay"`
	Slots       int       `json:"slots"`
	Running     int       `json:"running"`
	Completed   int       `json:"completed"`
	Failed      int       `json:"failed"`
	Version     string    `json:"version"`
	Since       time.Time `json:"since"`
}

// writeFileAtomic writes via a temp file plus rename, so readers never see a
// partial file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
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
	return writeFileAtomic(path, append(b, '\n'), 0o644)
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
	if !st.Paired {
		pairing = "no"
		if st.PairingCode != "" {
			pairing = "no, pairing code " + st.PairingCode
		}
	}
	_, err = fmt.Fprintf(out, "panda-agentd %s, up since %s\nrelay:   %s\npaired:  %s\njobs:    %d/%d running, %d completed, %d failed\n",
		st.Version, st.Since.Local().Format(time.RFC3339), st.Relay, pairing, st.Running, st.Slots, st.Completed, st.Failed)
	return err
}
