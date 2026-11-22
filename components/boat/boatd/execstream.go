package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Exec across vsock resets
//
// Firecracker resets the guest's vsock transport on every snapshot, so a fork,
// snapshot or warm capture of a running VM drops each connection in the guest
// while the host side stays open and silent: an exec running then lost its
// output and exit, and its caller waited out timeout_s+30 (boat-e2e 42985309146).
// An exec now carries a session id; the agent keeps its reply lines, and after
// each snapshot of the running VM boatd reconnects and sends exec_attach with
// the number of lines already relayed, so nothing is lost or repeated.

// resetHub hands out, per VM, a channel closed at its next vsock reset.
type resetHub struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func (h *resetHub) watch(id string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ch == nil {
		h.ch = map[string]chan struct{}{}
	}
	c, ok := h.ch[id]
	if !ok {
		c = make(chan struct{})
		h.ch[id] = c
	}
	return c
}

// fire: id's vsock transport was reset (a snapshot of it running, now resumed).
func (h *resetHub) fire(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.ch[id]; ok {
		close(c)
		delete(h.ch, id)
	}
}

type agentLine struct {
	raw []byte
	rep reply
	err error
}

// readLines feeds a's lines to the returned channel until an error, stop, or the
// final line. It reads nothing past the final line, so once that line is handed
// over the conn has no reader and may go back to the pool (finishStream).
func readLines(a *agentConn, stop <-chan struct{}) <-chan agentLine {
	out := make(chan agentLine, 16)
	go func() {
		for {
			raw, rep, err := a.recv()
			select {
			case out <- agentLine{raw, rep, err}:
			case <-stop:
				return
			}
			if err != nil || rep.final() {
				return
			}
		}
	}()
	return out
}

// attachAckTimeout bounds the wait for exec_attach's first line, which the
// agent sends at once: another reset may have eaten it.
var attachAckTimeout = 10 * time.Second

// attachExec reconnects to exec session sid after a reset; seen lines were
// relayed. With none seen it hands the agent the exec request too: if the reset
// swallowed it before the agent took it, the agent starts it then (session ids
// are single use, so it runs once). Returns the connection, or the agent's
// error line as pending (old agents: "unknown op").
func attachExec(ctx context.Context, sock, sid string, seen int, req map[string]any) (*agentConn, *agentLine, error) {
	attach := map[string]any{"op": "exec_attach", "session": sid, "seen": seen}
	if seen == 0 {
		ex := map[string]any{}
		for k, v := range req {
			if k != "op" && k != "session" {
				ex[k] = v
			}
		}
		attach["exec"] = ex
	}
	var err error
	for try := 0; try < 3 && ctx.Err() == nil; try++ {
		var a *agentConn
		if a, err = dialAgent(ctx, sock); err != nil {
			continue
		}
		dl, _ := ctx.Deadline()
		if ack := time.Now().Add(attachAckTimeout); dl.IsZero() || ack.Before(dl) {
			a.c.SetReadDeadline(ack)
		}
		var raw []byte
		var rep reply
		if err = a.send(attach); err == nil {
			raw, rep, err = a.recv()
		}
		if err != nil {
			a.Close()
			continue
		}
		a.c.SetReadDeadline(dl) // zero = none
		if rep.Attached == sid {
			return a, nil, nil
		}
		return a, &agentLine{raw: raw, rep: rep}, nil
	}
	if err == nil {
		err = ctx.Err()
	}
	return nil, nil, err
}

// relayExec writes the exec's reply lines to emit until its final line,
// reattaching after each vsock reset of VM id. a carries req, already sent;
// reset is m.resets.watch(id) from before a was dialed.
func (m *Manager) relayExec(ctx context.Context, id, sock string, a *agentConn, reset <-chan struct{},
	req map[string]any, emit func(raw []byte, final bool) error) {
	sid, _ := req["session"].(string)
	seen := 0
	var pending *agentLine // an attach's first line, read before its reader started
	for {
		stop := make(chan struct{})
		lines := readLines(a, stop)
		done, reattach := false, false
		var last *reply // the final line, when it arrived intact
		for !done && !reattach {
			var l agentLine
			if pending != nil {
				l, pending = *pending, nil
			} else {
				select {
				case l = <-lines:
				case <-reset:
					reattach = true
					continue
				}
			}
			if l.err != nil {
				emitErr(emit, "guest agent: "+l.err.Error())
				done = true
				continue
			}
			seen++
			final := l.rep.final()
			if final {
				last = &l.rep
			}
			done = emit(l.raw, final) != nil || final
		}
		close(stop)
		if last != nil {
			// A finished exec leaves its conn idle (the guest loops on readline and
			// sends nothing after the final line), so it can serve the next request.
			// finishStream still closes it when unfit: an error line, a spent ctx
			// deadline, bytes left over, or a conn that was reattached (no pool
			// identity). Any other end (read error, client gone, reset) closes it:
			// the guest may still be writing to it.
			a.finishStream(sock, *last)
		} else {
			a.Close()
		}
		if done {
			return
		}
		reset = m.resets.watch(id)
		na, first, err := attachExec(ctx, sock, sid, seen, req)
		if err != nil {
			emitErr(emit, "guest agent: reattach after a vsock reset: "+err.Error())
			return
		}
		a, pending = na, first
	}
}

func emitErr(emit func([]byte, bool) error, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	emit(b, true)
}
