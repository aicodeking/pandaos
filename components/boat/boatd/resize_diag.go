package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A disk grow on a running VM ends in the guest's online resize2fs. boat-e2e
// 43313462575 (attempt 2) saw it never answer: no reply within boatd's 60 s, so
// resize2fs ignored SIGKILL (uninterruptible I/O on /dev/vda after the PATCH
// /drives/rootfs). Cause unproven (PROTOCOL.md "Known issues"); these make the next
// one provable and the API answer 504 resize_timeout instead of a 502 after 60 s.

const (
	codeResizeTimeout = "resize_timeout"
	guestExecTimeout  = 50 * time.Second
	guestExecGrace    = 10 * time.Second // the agent's SIGTERM -> SIGKILL -> reap, and the reply
	resizeDiagTimeout = 15 * time.Second
	diagFCLogLines    = 30
)

var errGuestTimeout = errors.New("timed out in the guest")

// guestTimeout: the guest exec ran out of time, as the agent reported it or as
// boatd's own deadline on the vsock saw it.
func guestTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, errGuestTimeout) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// resizeDiag logs, one line each (the e2e prints boatd.log's tail): the failure, the
// guest's kernel log after sysrq w (blocked tasks with their stacks), and the end of
// firecracker.log. A second exec: the agent serves each connection on its own thread.
func (m *Manager) resizeDiag(v VM, cause error) {
	log.Printf("%s: resize: guest resize2fs failed: %v", v.ID, cause)
	ctx, cancel := context.WithTimeout(context.Background(), resizeDiagTimeout+guestExecGrace)
	defer cancel()
	out, err := execRootOut(ctx, filepath.Join(m.root(v.ID), vsockUDS),
		[]string{"sh", "-c", "echo w >/proc/sysrq-trigger; dmesg | tail -n 80"}, resizeDiagTimeout)
	if err != nil {
		out = append(out, fmt.Sprintf(" (diag exec: %v)", err)...)
	}
	log.Printf("%s: resize diag: guest dmesg after sysrq w: %s", v.ID, oneLine(out))
	log.Printf("%s: resize diag: firecracker.log: %s", v.ID, oneLine(tailLines(filepath.Join(m.vmDir(v.ID), "firecracker.log"), diagFCLogLines)))
}

func oneLine(b []byte) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " | ")), " ")
}

// tailLines: the last n lines of path, read from its last 16 KiB.
func tailLines(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return []byte(err.Error())
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 16<<10 {
		f.Seek(st.Size()-16<<10, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return []byte(strings.Join(lines[max(0, len(lines)-n):], "\n"))
}
