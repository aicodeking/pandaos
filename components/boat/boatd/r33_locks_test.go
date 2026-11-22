package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestR33LockMapPruned(t *testing.T) {
	m := &Manager{locks: map[string]*sync.Mutex{}}
	for i := 0; i < 1000; i++ {
		m.lock(fmt.Sprintf("vm-%d", i))()
	}
	if n := len(m.locks); n != 0 {
		t.Fatalf("locks map has %d entries, want 0", n)
	}
}

func TestR33LockSerializesSameID(t *testing.T) {
	m := &Manager{locks: map[string]*sync.Mutex{}}
	var wg sync.WaitGroup
	counter := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				u := m.lock("same")
				counter++
				u()
			}
		}()
	}
	wg.Wait()
	if counter != 5000 {
		t.Fatalf("counter=%d", counter)
	}
	if len(m.locks) != 0 {
		t.Fatalf("map not empty: %d", len(m.locks))
	}
}

func TestR33LockBusyAndDoubleUnlock(t *testing.T) {
	m := &Manager{locks: map[string]*sync.Mutex{}}
	u := m.lock("a")
	if !m.lockBusy("a") {
		t.Fatal("want busy")
	}
	u()
	u()
	if m.lockBusy("a") || len(m.locks) != 0 {
		t.Fatal("want idle and empty")
	}
}
