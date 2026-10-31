package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// atspiHelper is one long-lived `panda-cu-atspi` process, spoken to in
// JSON lines. Starting Python and loading the typelibs costs ~1 s, so it is
// paid once per MCP session instead of once per call. A request that times
// out kills the process; the next call starts a fresh one, whose ids carry
// a new prefix, so ids from the old one answer "unknown".
type atspiHelper struct {
	mu    sync.Mutex
	env   []string
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan []byte
	seq   int
	gen   int // helpers started so far
}

func (h *atspiHelper) start() error {
	h.gen++
	cmd := exec.Command("panda-cu-atspi")
	cmd.Env = h.env
	if h.gen > 1 { // ids of a restarted helper never collide with the old ones
		cmd.Env = append(slices.Clone(h.env), fmt.Sprintf("PANDA_CU_ID_PREFIX=g%de", h.gen))
	}
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("panda-cu-atspi: %w", err)
	}
	lines := make(chan []byte, 4)
	go func() {
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			l, err := r.ReadBytes('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- l
		}
	}()
	go cmd.Wait() //nolint:errcheck // reap; a dead helper shows up as a closed channel
	h.cmd, h.in, h.lines = cmd, in, lines
	return nil
}

func (h *atspiHelper) stop() {
	if h.cmd != nil {
		_ = h.in.Close()
		_ = h.cmd.Process.Kill()
		h.cmd = nil
	}
}

// call sends req (cmd plus fields) and decodes the reply into v. Replies
// with "error" become errors.
func (h *atspiHelper) call(timeout time.Duration, req map[string]any, v any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil {
		if err := h.start(); err != nil {
			return err
		}
	}
	h.seq++
	req["seq"] = h.seq
	b, _ := json.Marshal(req)
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.stop()
		return fmt.Errorf("accessibility helper died: %w", err)
	}
	deadline := time.After(timeout)
	for {
		select {
		case l, ok := <-h.lines:
			if !ok {
				h.stop()
				return errors.New("accessibility helper exited (see journalctl); it restarts on the next call")
			}
			var hdr struct {
				Seq   int    `json:"seq"`
				Error string `json:"error"`
			}
			if json.Unmarshal(l, &hdr) != nil || hdr.Seq != h.seq {
				continue // a late reply to a request that timed out
			}
			if hdr.Error != "" {
				return errors.New(hdr.Error)
			}
			return json.Unmarshal(l, v)
		case <-deadline:
			h.stop()
			return fmt.Errorf("accessibility helper did not answer within %s (an app is not responding?)", timeout)
		}
	}
}
