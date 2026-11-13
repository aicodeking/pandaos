package main

import (
	"path/filepath"
	"testing"
	"time"
)

// The instance-wide cap holds across teams and survives a restart.
func TestCertQuotaGlobalPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.json")
	now := time.Now()
	q, err := newCertQuota(10, 24*time.Hour, 3, 7*24*time.Hour, path)
	if err != nil {
		t.Fatal(err)
	}
	for i, team := range []string{"t1", "t2", "t3"} {
		if !q.allow(team, team+".host", now) {
			t.Fatalf("approval %d refused", i)
		}
	}
	if q.allow("t4", "t4.host", now) {
		t.Error("global cap not applied across teams")
	}
	q2, err := newCertQuota(10, 24*time.Hour, 3, 7*24*time.Hour, path) // restart
	if err != nil {
		t.Fatal(err)
	}
	if q2.allow("t5", "t5.host", now.Add(time.Hour)) {
		t.Error("restart reset the global window")
	}
	if !q2.allow("t1", "t1.host", now.Add(time.Hour)) {
		t.Error("renewal of an approved host refused after restart")
	}
	if !q2.allow("t5", "t5.host", now.Add(8*24*time.Hour)) {
		t.Error("window never reopened")
	}
}
