package main

import (
	"context"
	"reflect"
	"testing"
)

// A fork that omits labels inherits only the parent's boat.* labels; a labels map
// in the request is used exactly as given.
func TestR48ForkLabels(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	p, err := m.Create(ctx, CreateReq{Team: "t1", Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	lbl := map[string]string{"env": "prod", "boat.secrets": "a,b"}
	if _, err = m.Patch(p.ID, PatchReq{Labels: lbl}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		labels map[string]string
		want   map[string]string
	}{
		{"omitted", nil, map[string]string{"boat.secrets": "a,b"}},
		{"provided", map[string]string{"x": "1"}, map[string]string{"x": "1"}},
		{"empty", map[string]string{}, nil},
	}
	for _, c := range cases {
		_, base, err := m.forkParent(p.ID, "t1", c.name, c.labels, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(base.Labels, c.want) {
			t.Fatalf("%s: labels %v, want %v", c.name, base.Labels, c.want)
		}
	}
	if got, _ := m.get(p.ID); !reflect.DeepEqual(got.Labels, lbl) {
		t.Fatalf("parent labels mutated: %v", got.Labels)
	}
}
