package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Guest agent client: Firecracker hybrid vsock. See ../PROTOCOL.md.
const (
	agentPort    = 1024
	maxAgentLine = 8 << 20 // the guest is untrusted: cap what one line may cost us
)

// agentCallTimeout bounds one request/reply op. A guest may answer health and hang
// on the next op, and boatd often waits for it holding the VM's lock.
var agentCallTimeout = 30 * time.Second

// agentHandshakeTimeout bounds the vsock CONNECT: the guest kernel accepts it
// at once unless the guest is not running (a VM left paused), and an exec must
// not wait its whole timeout_s for that.
var agentHandshakeTimeout = 15 * time.Second

type reply struct {
	Stream string   `json:"stream,omitempty"`
	Data   string   `json:"data_b64,omitempty"`
	Exit   *int     `json:"exit,omitempty"`
	OK     bool     `json:"ok,omitempty"`
	EOF    bool     `json:"eof,omitempty"`
	Error  string   `json:"error,omitempty"`
	IdleS  *float64 `json:"idle_s,omitempty"`
	// Attached: exec_attach took the session over (execstream.go).
	Attached string `json:"attached,omitempty"`
}

// final: the last line of a reply (exec ends on exit, get_file on eof).
func (r reply) final() bool { return r.Exit != nil || r.OK || r.EOF || r.Error != "" }

type agentConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialAgent(ctx context.Context, sock string) (*agentConn, error) {
	c, err := dialJail(ctx, sock)
	if err != nil {
		return nil, err
	}
	dl, hasDL := ctx.Deadline()
	hs := time.Now().Add(agentHandshakeTimeout)
	if hasDL && dl.Before(hs) {
		hs = dl
	}
	c.SetDeadline(hs)
	a := &agentConn{c: c, r: bufio.NewReaderSize(c, 64<<10)}
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) }) // unblocks reads on cancel
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", agentPort); err != nil {
		stop()
		c.Close()
		return nil, err
	}
	line, err := a.line()
	if err != nil || !bytes.HasPrefix(line, []byte("OK ")) {
		stop()
		c.Close()
		return nil, fmt.Errorf("vsock handshake: %q %v", line, err)
	}
	if !stop() { // ctx ended meanwhile: its deadline stays
		c.Close()
		return nil, ctx.Err()
	}
	if hasDL {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Time{})
	}
	context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	return a, nil
}

func (a *agentConn) Close() error { return a.c.Close() }

func (a *agentConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = a.c.Write(append(b, '\n'))
	return err
}

// line reads one \n-terminated line of at most maxAgentLine bytes.
func (a *agentConn) line() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := a.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxAgentLine {
			return nil, errors.New("agent line too long")
		}
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// recv returns the raw line and its decoded form.
func (a *agentConn) recv() ([]byte, reply, error) {
	var r reply
	for {
		b, err := a.line()
		if err != nil {
			return nil, r, err
		}
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, r, fmt.Errorf("agent sent bad json: %w", err)
		}
		return b, r, nil
	}
}

// agentErr is a well-formed {"error": ...} from the guest.
type agentErr struct{ msg string }

func (e *agentErr) Error() string { return "guest agent: " + e.msg }

func (e *agentErr) notFound() bool {
	m := strings.ToLower(e.msg)
	return strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// agentRaw sends one request and returns its single final reply line.
func agentRaw(ctx context.Context, sock string, req any) (json.RawMessage, reply, error) {
	ctx, cancel := context.WithTimeout(ctx, agentCallTimeout)
	defer cancel()
	a, err := dialAgent(ctx, sock)
	if err != nil {
		return nil, reply{}, err
	}
	defer a.Close()
	if err := a.send(req); err != nil {
		return nil, reply{}, err
	}
	for {
		raw, r, err := a.recv()
		if err != nil {
			return nil, r, err
		}
		if r.Error != "" {
			return raw, r, &agentErr{r.Error}
		}
		if r.final() {
			return raw, r, nil
		}
	}
}

func agentCall(ctx context.Context, sock string, req any) (reply, error) {
	_, r, err := agentRaw(ctx, sock, req)
	return r, err
}

// waitAgent polls health until the agent answers or d passes.
func waitAgent(ctx context.Context, sock string, d time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		c, cc := context.WithTimeout(ctx, 2*time.Second)
		_, err := agentCall(c, sock, map[string]string{"op": "health"})
		cc()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent not ready after %s: %w", d, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
