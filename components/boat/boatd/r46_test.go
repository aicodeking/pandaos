package main

import (
	"context"
	"testing"
)

// A fork that omits ttl_s gets the default hour, even from a parent with auto-stop
// off; an explicit ttl_s (including 0) wins.
func TestR46ForkTTLDefaultsToOneHour(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	p, err := m.Create(ctx, CreateReq{Team: "t1", Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = m.Patch(p.ID, PatchReq{TTLS: ptrInt(0)}); err != nil || p.TTLS != 0 {
		t.Fatalf("patch: %+v %v", p, err)
	}
	cases := []struct {
		name string
		ttl  *int
		want int
	}{{"omitted", nil, 3600}, {"explicit", ptrInt(120), 120}, {"zero", ptrInt(0), 0}}
	for _, c := range cases {
		_, base, err := m.forkParent(p.ID, "t1", c.name, nil, c.ttl, false)
		if err != nil {
			t.Fatal(err)
		}
		if base.TTLS != c.want {
			t.Fatalf("%s: child ttl %d, want %d", c.name, base.TTLS, c.want)
		}
	}
}

func ptrInt(i int) *int { return &i }
