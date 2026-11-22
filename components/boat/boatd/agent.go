package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	Code   string   `json:"code,omitempty"`
	IdleS  *float64 `json:"idle_s,omitempty"`
	// TimedOut: an exec ran past its timeout_s and was killed.
	TimedOut bool `json:"timed_out,omitempty"`
	// Attached: exec_attach took the session over (execstream.go).
	Attached string `json:"attached,omitempty"`
}

// final: the last line of a reply (exec ends on exit, get_file on eof).
func (r reply) final() bool { return r.Exit != nil || r.OK || r.EOF || r.Error != "" }

type agentConn struct {
	c    net.Conn
	r    *bufio.Reader
	dead bool // set when the conn must not return to the pool
	// ident: the socket file this conn was dialed through, to spot a restarted VM.
	ident os.FileInfo
	// gen: the pool's generation for the socket when this conn was taken; put drops
	// the conn if closeSock ran since (a snapshot reset the guest's conns meanwhile).
	gen uint64
	// unbind stops the ctx deadline hook of a dialStream conn; false means ctx ended.
	unbind func() bool
}

func dialAgent(ctx context.Context, sock string) (*agentConn, error) {
	a, err := dialAgentHS(ctx, sock)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		a.c.SetDeadline(dl)
	} else {
		a.c.SetDeadline(time.Time{})
	}
	c := a.c
	context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	return a, nil
}

// dialStream returns a conn dedicated to one streamed exec: an idle pooled conn
// when one is available (skipping the dial + CONNECT), else a fresh dial. reused
// reports which. The conn is bound to ctx like dialAgent's. When the stream ends
// on its final line, finishStream may return it to the pool; on any other end the
// caller just Closes it.
//
// Reuse after a finished exec is safe for the same reason agentRaw pools exec
// conns: the guest's handle_conn loops on readline, and op_exec sends nothing
// after its final line (a session's replay log lives in the guest, not on the
// conn). A conn that was reattached after a vsock reset (execstream.go) is a
// different one, dialed unpooled, and is never returned.
func dialStream(ctx context.Context, sock string) (a *agentConn, reused bool, err error) {
	a, reused, err = agentPool.take(ctx, sock, "exec")
	if err != nil {
		return nil, false, err
	}
	if dl, ok := ctx.Deadline(); ok {
		a.c.SetDeadline(dl)
	} else {
		a.c.SetDeadline(time.Time{})
	}
	c := a.c
	a.unbind = context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	return a, reused, nil
}

// sendExec takes the stream conn (dialStream) and writes the exec request line. A
// pooled conn whose write fails before the whole line went out is replaced by one
// fresh dial: the guest acts on complete lines only, so the exec cannot have run.
// The caller owns the returned conn: relayExec ends it with finishStream or Close.
func sendExec(ctx context.Context, sock string, req map[string]any) (*agentConn, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	line := append(b, '\n')
	for attempt := 0; ; attempt++ {
		a, reused, err := dialStream(ctx, sock)
		if err != nil {
			return nil, err
		}
		n, err := a.c.Write(line)
		if err == nil {
			return a, nil
		}
		a.Close()
		if reused && attempt == 0 && n < len(line) && ctx.Err() == nil {
			continue
		}
		return nil, err
	}
}

// finishStream retires a conn whose exec ended with final line r, which no other
// goroutine reads any more: back to the pool when clean (see connPool.put), else
// closed. Conns not from dialStream have no socket identity, so put closes them.
func (a *agentConn) finishStream(sock string, r reply) {
	if a.unbind != nil && !a.unbind() { // ctx ended mid-stream: the deadline is spent
		a.dead = true
	}
	agentPool.put(sock, "exec", a, r)
}

// dialAgentHS dials and completes the vsock CONNECT handshake. The conn comes back
// with the handshake deadline still set and nothing bound to ctx: the caller owns
// deadlines (dialAgent for dedicated conns, the pool per op).
func dialAgentHS(ctx context.Context, sock string) (*agentConn, error) {
	agentDials.Add(1)
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

// agentErr is a well-formed {"error": ..., "code": ...} from the guest.
type agentErr struct {
	msg  string
	code string
}

func (e *agentErr) Error() string { return "guest agent: " + e.msg }

func (e *agentErr) isNoSuchUser() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(e.msg)), "no such user")
}

// errCode returns the structured error code to expose in HTTP error responses,
// normalizing legacy guest images that reported "no such user..." with code "not_found"
// (or empty code) to "invalid_user".
func (e *agentErr) errCode() string {
	c := strings.TrimSpace(e.code)
	low := strings.ToLower(c)
	if (low == "" || low == "not_found") && e.isNoSuchUser() {
		return "invalid_user"
	}
	return c
}

func agentCodeStatus(code string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "not_found", "no_such_file", "no_such_command", "enoent":
		return 404, true
	case "conflict", "not_running", "command_not_running", "already_exists", "exists", "directory_not_empty", "not_empty", "is_a_directory", "eexist", "enotempty", "eisdir":
		return 409, true
	case "invalid_user", "spawn_failed", "invalid_request", "bad_request", "invalid_argument", "invalid_path", "refused", "forbidden_path", "protected_path", "not_a_directory", "enotdir", "einval":
		return 400, true
	case "unsupported", "not_supported":
		return 422, true
	case "permission_denied", "eacces", "eperm":
		return 403, true
	default:
		return 0, false
	}
}

func (e *agentErr) notFound() bool {
	if status, known := agentCodeStatus(e.errCode()); known {
		return status == 404
	}
	if e.isNoSuchUser() {
		return false
	}
	m := strings.ToLower(e.msg)
	return strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// httpStatus maps structured guest agent error codes authoritatively when known,
// falling back to message heuristics only when the code is empty or unknown, and
// reserving 502 for unexpected guest failures.
func (e *agentErr) httpStatus() int {
	if status, known := agentCodeStatus(e.errCode()); known {
		return status
	}
	m := strings.ToLower(e.msg)
	if strings.HasPrefix(strings.TrimSpace(m), "no such user") {
		return 400
	}
	if strings.Contains(m, "no such") || strings.Contains(m, "not found") {
		return 404
	}
	for _, needle := range []string{
		"command is not running",
		"directory not empty",
		"non-empty directory",
		"already exists",
		"exists; set overwrite",
		"file exists",
		"session exists",
	} {
		if strings.Contains(m, needle) {
			return 409
		}
	}
	for _, needle := range []string{
		"refusing to delete",
		"not a directory",
		"is a directory",
		"path must be absolute",
		"must be an absolute path",
		"must not be negative",
		"must be a non-empty",
		"must be a non-negative",
		"must be an object",
		"bad signal",
		"bad base64",
		"bad env key",
		"bad session id",
	} {
		if strings.Contains(m, needle) {
			return 400
		}
	}
	return 502
}

// agentRaw sends one request and returns its single final reply line. Requests
// ride a small per-VM pool of idle persistent conns (agentpool.go); a conn that
// breaks is replaced transparently only when that cannot run an op twice.
func agentRaw(ctx context.Context, sock string, req any) (json.RawMessage, reply, error) {
	ctx, cancel := context.WithTimeout(ctx, agentCallTimeout)
	defer cancel()
	b, err := json.Marshal(req)
	if err != nil {
		return nil, reply{}, err
	}
	line := append(b, '\n')
	op := agentOpName(b)
	for attempt := 0; ; attempt++ {
		a, reused, err := agentPool.take(ctx, sock, op)
		if err != nil {
			return nil, reply{}, err
		}
		raw, r, sent, err := a.roundTrip(ctx, line)
		var ae *agentErr
		if err == nil || errors.As(err, &ae) {
			agentPool.put(sock, op, a, r)
			return raw, r, err
		}
		a.Close()
		if reused && attempt == 0 && ctx.Err() == nil && (!sent || agentIdempotent(op)) {
			continue // a stale pooled conn: the op did not run, or running it twice is harmless
		}
		return nil, r, err
	}
}

// roundTrip writes one request line and reads to its final reply. sent reports
// whether the whole line reached the conn (a shorter write cannot have run: the
// guest acts on a complete line only).
func (a *agentConn) roundTrip(ctx context.Context, line []byte) (raw json.RawMessage, r reply, sent bool, err error) {
	if dl, ok := ctx.Deadline(); ok {
		a.c.SetDeadline(dl)
	}
	c := a.c
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer func() {
		if !stop() { // ctx ended mid-op: the conn's deadline is spent, never reuse it
			a.dead = true
		}
	}()
	n, err := a.c.Write(line)
	if err != nil {
		return nil, r, n == len(line), err
	}
	for {
		raw, r, err := a.recv()
		if err != nil {
			return nil, r, true, err
		}
		if r.Error != "" {
			return raw, r, true, &agentErr{msg: r.Error, code: r.Code}
		}
		if r.final() {
			return raw, r, true, nil
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
	bo := newPollBackoff(time.Millisecond, 20*time.Millisecond)
	for {
		c, cc := context.WithTimeout(ctx, 2*time.Second)
		_, err := agentCall(c, sock, map[string]string{"op": "health"})
		cc()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(bo.next())
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("guest agent not ready after %s: %w", d, err)
		case <-timer.C:
		}
	}
}

// Per-VM pool of idle persistent agent conns. The guest agent serves many
// request/reply ops per conn in sequence (handle_conn loops on readline), so a
// finished op's conn can carry the next one and skip the unix dial + CONNECT round
// trip into the guest. Only agentRaw uses it; anything that hijacks or streams
// (exec_attach, put/get streaming, desktop) dials its own conn with dialAgent.
const agentPoolMaxIdle = 4

// agentPoolIdleTimeout closes a conn nobody reused.
var agentPoolIdleTimeout = 30 * time.Second

// agentDials counts vsock dials (pooled or not); tests and metrics read it.
var agentDials atomic.Int64

var agentPool = &connPool{idle: map[string][]*idleConn{}}

type idleConn struct {
	a     *agentConn
	timer *time.Timer
}

type connPool struct {
	mu   sync.Mutex
	idle map[string][]*idleConn
	// Per-socket generation: closeSock stamps the socket with a new value from seq, and
	// put discards a conn taken under an older one. gens is pruned when it grows large:
	// floor then stands in for every dropped entry, so older conns are still discarded.
	gens  map[string]uint64
	seq   uint64
	floor uint64
}

const agentPoolMaxGens = 4096

// genLocked is the current generation of sock.
func (p *connPool) genLocked(sock string) uint64 { return max(p.gens[sock], p.floor) }

func (p *connPool) generation(sock string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.genLocked(sock)
}

// agentNoPool: ops whose conn must not be reused. shutdown ends the guest;
// exec_attach and desktop take the stream over. sync is the snapshot barrier: the
// guest syncs right before a VM is paused and snapshotted, and a snapshot resets the
// guest's vsock conns (the host end may stay open and then never answers), so every
// idle conn to that VM is dropped before it and none is kept after it.
var agentNoPool = map[string]bool{"shutdown": true, "exec_attach": true, "desktop": true, "sync": true}

// agentBarrierOps also drop the VM's idle conns before they dial.
var agentBarrierOps = map[string]bool{"sync": true, "shutdown": true}

// agentIdempotentOps are pure reads: running one twice is harmless, so a request
// that died on a stale pooled conn may be re-sent. Everything else (exec, start,
// writes, signals) is never replayed once its line was written.
var agentIdempotentOps = map[string]bool{
	"health": true, "cmd_status": true, "cmd_logs": true, "cmd_list": true,
	"file_list": true, "file_search": true, "get_file": true, "get_env": true,
}

func agentIdempotent(op string) bool { return agentIdempotentOps[op] }

func agentOpName(b []byte) string {
	var h struct {
		Op string `json:"op"`
	}
	json.Unmarshal(b, &h)
	return h.Op
}

// take returns a live idle conn for sock, or dials. reused reports which.
func (p *connPool) take(ctx context.Context, sock, op string) (a *agentConn, reused bool, err error) {
	if agentBarrierOps[op] {
		p.closeSock(sock)
	}
	gen := p.generation(sock) // before any conn is obtained: a later closeSock must outdate it
	if !agentNoPool[op] {
		if a = p.pop(sock); a != nil {
			a.gen = gen
			return a, true, nil
		}
	}
	// Identity before the dial: a socket replaced in between only costs a spurious
	// drop later, never a stale conn kept.
	st, _ := os.Lstat(sock)
	if a, err = dialAgentHS(ctx, sock); err != nil {
		return nil, false, err
	}
	a.ident, a.gen = st, gen
	return a, false, nil
}

func (p *connPool) pop(sock string) *agentConn {
	for {
		p.mu.Lock()
		l := p.idle[sock]
		if len(l) == 0 {
			p.mu.Unlock()
			return nil
		}
		e := l[len(l)-1]
		p.setLocked(sock, l[:len(l)-1])
		p.mu.Unlock()
		e.timer.Stop()
		st, err := os.Lstat(sock)
		if err != nil || e.a.ident == nil || !os.SameFile(st, e.a.ident) || !st.ModTime().Equal(e.a.ident.ModTime()) {
			// The socket file is gone or replaced: the VM restarted, so every idle
			// conn for this path belongs to the previous generation.
			e.a.Close()
			p.closeSock(sock)
			return nil
		}
		if e.a.alive() {
			return e.a
		}
		e.a.Close()
	}
}

func (p *connPool) setLocked(sock string, l []*idleConn) {
	if len(l) == 0 {
		delete(p.idle, sock)
		return
	}
	p.idle[sock] = l
}

// put returns a after a finished op if it is clean enough to carry another.
func (p *connPool) put(sock, op string, a *agentConn, r reply) {
	if a.dead || a.ident == nil || agentNoPool[op] || r.TimedOut ||
		(r.Error != "" && r.Exit != nil) || a.r.Buffered() > 0 || agentPoolIdleTimeout <= 0 {
		a.Close()
		return
	}
	a.c.SetDeadline(time.Time{})
	e := &idleConn{a: a}
	p.mu.Lock()
	l := p.idle[sock]
	if a.gen < p.genLocked(sock) || len(l) >= agentPoolMaxIdle {
		p.mu.Unlock()
		a.Close()
		return
	}
	e.timer = time.AfterFunc(agentPoolIdleTimeout, func() { p.expire(sock, e) })
	p.idle[sock] = append(l, e)
	p.mu.Unlock()
}

func (p *connPool) expire(sock string, e *idleConn) {
	p.mu.Lock()
	l := p.idle[sock]
	for i, x := range l {
		if x == e {
			nl := append(append([]*idleConn(nil), l[:i]...), l[i+1:]...)
			p.setLocked(sock, nl)
			p.mu.Unlock()
			e.a.Close()
			return
		}
	}
	p.mu.Unlock()
}

func (p *connPool) closeSock(sock string) {
	p.mu.Lock()
	l := p.idle[sock]
	delete(p.idle, sock)
	p.seq++
	if p.gens == nil {
		p.gens = map[string]uint64{}
	}
	p.gens[sock] = p.seq
	if len(p.gens) > agentPoolMaxGens {
		p.floor, p.gens = p.seq, map[string]uint64{}
	}
	p.mu.Unlock()
	for _, e := range l {
		e.timer.Stop()
		e.a.Close()
	}
}

// closeAgentConns drops every idle pooled conn to the VM behind sock. Call it when
// the VM stops or is removed; pooled conns also self-heal (idle timeout, socket
// identity change, peer EOF) if nobody does.
func closeAgentConns(sock string) { agentPool.closeSock(sock) }

// alive reports whether an idle conn is still usable: the peer has not closed it
// and nothing unsolicited arrived (a stray byte would be mistaken for the next reply).
func (a *agentConn) alive() bool {
	sc, ok := a.c.(syscall.Conn)
	if !ok || a.r.Buffered() > 0 {
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	a.c.SetDeadline(time.Time{}) // an expired deadline would fail rc.Read before the peek
	alive := false
	var buf [1]byte
	err = rc.Read(func(fd uintptr) bool {
		n, _, e := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		alive = n < 0 && (errors.Is(e, syscall.EAGAIN) || errors.Is(e, syscall.EWOULDBLOCK))
		return true
	})
	return err == nil && alive
}
