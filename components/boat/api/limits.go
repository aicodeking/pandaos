package main

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// Per-team limits on what lives in state.json: it is rewritten on every change,
// so one team's bulk would slow every team's writes.
const (
	maxSecretsPerTeam     = 200
	maxSecretBytesPerTeam = 2 << 20 // sealed bytes
	maxSharesPerTeam      = 100
	maxEnvBytesPerTeam    = 4 << 20
	maxGrantsPerTeam      = 200
	maxRecordedArgvBytes  = 8 << 10
	maxRateBuckets        = 16384
)

func parseIPToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.Unmap().WithZone("").String()
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil {
			return addr.Unmap().WithZone("").String()
		}
	}
	return ""
}

// trustedClientIP extracts the rate-limit client IP: X-Forwarded-For (rightmost
// valid hop appended by local Caddy) and X-Real-Ip are consulted only when
// RemoteAddr is loopback; non-loopback callers cannot spoof proxy headers.
func trustedClientIP(r *http.Request) string {
	raw := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	remote, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	remote = remote.Unmap().WithZone("")
	if !remote.IsLoopback() {
		return remote.String()
	}
	vals := r.Header.Values("X-Forwarded-For")
	for i := len(vals) - 1; i >= 0; i-- {
		parts := strings.Split(vals[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			if ip := parseIPToken(parts[j]); ip != "" {
				return ip
			}
		}
	}
	if ip := parseIPToken(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	return remote.String()
}

// In-flight defaults: the rate limits bound how fast these start, not how many
// stay open. A slot is held for the whole request.
const (
	defaultTeamStreams  = 64
	defaultTeamUploads  = 8
	defaultTeamInflight = 128 // all routes: streams (64) and uploads (8) fit under it
	defaultAIVMInflight = 16
)

// inflight counts requests per key; keys vanish at zero, so it cannot grow.
// It also keeps a byte budget per key and over all keys (reserve).
type inflight struct {
	mu  sync.Mutex
	max int // 0 = unlimited
	n   map[string]int
	// Buffered request bytes now held, per key and in total (see reserve).
	held      map[string]int64
	heldTeam  map[string]int64
	heldTotal int64
}

func newInflight(max int) *inflight { return &inflight{max: max, n: map[string]int{}} }

// acquire takes a slot; ok=false means the key is at its limit.
func (l *inflight) acquire(key string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.n[key] >= l.max {
		return nil, false
	}
	l.n[key]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.n[key]--; l.n[key] <= 0 {
			delete(l.n, key)
		}
	}, true
}

// byteRes is a growing reservation of buffered bytes under one key (a VM) and,
// when team is set, one team.
type byteRes struct {
	l                            *inflight
	key, team                    string
	perKey, perTeam, global, got int64
}

// errByteBudget says which limit a reservation hit.
type errByteBudget struct{ perKey, perTeam bool }

func (e *errByteBudget) Error() string {
	switch {
	case e.perKey:
		return "the VM's buffered request bytes are at their limit"
	case e.perTeam:
		return "the team's buffered request bytes are at their limit"
	}
	return "the AI proxy's buffered request bytes are at their limit"
}

// reserve starts a reservation of buffered bytes for key, limited to perKey for
// the key and global for all keys together (0 = unlimited). Call add as bytes
// are buffered and release when the buffer is no longer needed.
func (l *inflight) reserve(key string, perKey, global int64) *byteRes {
	return l.reserveTeam(key, "", perKey, 0, global)
}

// reserveTeam is reserve with a third limit: the bytes all keys of one team hold
// together (perTeam, 0 = unlimited; team "" = none), so one team's VMs cannot
// take the whole global budget from the others.
func (l *inflight) reserveTeam(key, team string, perKey, perTeam, global int64) *byteRes {
	return &byteRes{l: l, key: key, team: team, perKey: perKey, perTeam: perTeam, global: global}
}

// add takes n more bytes; it fails (and takes nothing) when a limit would pass.
func (r *byteRes) add(n int64) error {
	if n <= 0 {
		return nil
	}
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.perKey > 0 && l.held[r.key]+n > r.perKey {
		return &errByteBudget{perKey: true}
	}
	if r.team != "" && r.perTeam > 0 && l.heldTeam[r.team]+n > r.perTeam {
		return &errByteBudget{perTeam: true}
	}
	if r.global > 0 && l.heldTotal+n > r.global {
		return &errByteBudget{}
	}
	if l.held == nil {
		l.held = map[string]int64{}
	}
	l.held[r.key] += n
	if r.team != "" {
		if l.heldTeam == nil {
			l.heldTeam = map[string]int64{}
		}
		l.heldTeam[r.team] += n
	}
	l.heldTotal += n
	r.got += n
	return nil
}

// release gives everything back; it is safe to call twice.
func (r *byteRes) release() {
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.got == 0 {
		return
	}
	if l.held[r.key] -= r.got; l.held[r.key] <= 0 {
		delete(l.held, r.key)
	}
	if r.team != "" {
		if l.heldTeam[r.team] -= r.got; l.heldTeam[r.team] <= 0 {
			delete(l.heldTeam, r.team)
		}
	}
	l.heldTotal -= r.got
	r.got = 0
}

// capArgv is what the command history keeps of an argv: state.json holds up to
// maxCommandRecords per VM, and an argv can be as large as a request body.
func capArgv(argv []string) []string {
	out := make([]string, 0, min(len(argv), 64))
	left := maxRecordedArgvBytes
	for _, a := range argv {
		if left <= 0 {
			return append(out, "…")
		}
		if len(a) > left {
			a = a[:left]
		}
		left -= len(a) + 1
		out = append(out, a)
	}
	return out
}
