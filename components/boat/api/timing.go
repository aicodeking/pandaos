package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// phases times the steps of a lifecycle request and logs one line per request,
// like boatd's "create alloc=… materialize=…", so a slow step shows up in
// api.log next to boatd's own breakdown.
type phases struct {
	start, last time.Time
	b           strings.Builder
}

func newPhases() *phases {
	now := time.Now()
	return &phases{start: now, last: now}
}

// mark ends the step called name.
func (p *phases) mark(name string) {
	now := time.Now()
	fmt.Fprintf(&p.b, " %s=%dms", name, now.Sub(p.last).Milliseconds())
	p.last = now
}

// done logs "<vm>: api <op> step=…ms … total=…ms".
func (p *phases) done(op, vmID string) {
	log.Printf("%s: api %s%s total=%dms", vmID, op, p.b.String(), time.Since(p.start).Milliseconds())
}
