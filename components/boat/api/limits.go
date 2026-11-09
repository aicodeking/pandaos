package main

import "sync"

// Per-team limits on what lives in state.json: it is rewritten on every change,
// so one team's bulk would slow every team's writes.
const (
	maxSecretsPerTeam     = 200
	maxSecretBytesPerTeam = 2 << 20 // sealed bytes
	maxSharesPerTeam      = 100
	maxEnvBytesPerTeam    = 4 << 20
	maxGrantsPerTeam      = 200
	maxRecordedArgvBytes  = 8 << 10
)

// In-flight defaults: the rate limits bound how fast these start, not how many
// stay open. A slot is held for the whole request.
const (
	defaultTeamStreams  = 64
	defaultTeamUploads  = 8
	defaultTeamInflight = 128 // all routes: streams (64) and uploads (8) fit under it
	defaultAIVMInflight = 16
)

// inflight counts requests per key; keys vanish at zero, so it cannot grow.
type inflight struct {
	mu  sync.Mutex
	max int // 0 = unlimited
	n   map[string]int
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
