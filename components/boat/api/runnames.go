package main

import (
	"log"
	"slices"
	"sort"
	"time"
)

// The names of the team secrets a VM's run was started with, recorded where the
// env is pushed (create, fork release, cold start, environment build) in
// State.VMSecrets under runNamesKey(vmID): the concrete secrets that went in,
// not the selection (@default, @all) that picked them, which can mean something
// else by the time a snapshot is taken. captureSnapshot records exactly these
// names for the snapshot; a VM with none recorded (a run from before this was
// kept, a lost write) yields a snapshot of unknown source, which fails towards a
// fresh boot. Absent = unknown; an empty list = the run held no team secret.

const runNamesPrefix = "run:"

func runNamesKey(vmID string) string { return runNamesPrefix + vmID }

// runNamesOf: the recorded names of vmID's run.
func runNamesOf(st *State, vmID string) ([]string, bool) {
	v, ok := st.VMSecrets[runNamesKey(vmID)]
	return v, ok
}

// envNames: the sorted names of env (never nil).
func envNames(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergeNames: the sorted union of a and b (never nil).
func mergeNames(a, b []string) []string {
	out := append(append([]string{}, a...), b...)
	sort.Strings(out)
	return slices.Compact(out)
}

// setRunNames records the run of vmID inside a store write. union keeps what the
// VM's run already held (a start that joins a running VM, a fork child that
// resumes its parent's memory) and records nothing when that is unknown.
func setRunNames(st *State, vmID string, names []string, union bool) {
	if union {
		prev, ok := runNamesOf(st, vmID)
		if !ok {
			return
		}
		names = mergeNames(prev, names)
	}
	st.VMSecrets[runNamesKey(vmID)] = append([]string{}, names...)
}

// recordRunNames is setRunNames as its own store write. A write that fails is
// logged only: the snapshot is then of unknown source.
func (s *server) recordRunNames(vmID string, names []string, union bool) {
	if err := s.store.Apply(func(st *State) error { // lazy: a start never waits for the disk
		setRunNames(st, vmID, names, union)
		return nil
	}); err != nil {
		log.Printf("record the secrets of the run of %s: %v", vmID, err)
	}
}

// recordPossibleRunNames precedes dispatch under the lifecycle lock. An
// unanswered command/update may have delivered every key; only new names need
// a disk barrier. Unknown inventory remains unknown until a proven fresh boot.
func (s *server) recordPossibleRunNames(p *principal, scope, id string, names []string) error {
	st := s.store.Snapshot()
	if _, err := requirePrincipalIn(st, p, scope, time.Now()); err != nil {
		return err
	}
	prev, known := runNamesOf(st, id)
	if !known || slices.Equal(mergeNames(prev, names), prev) {
		return nil
	}
	return s.store.UpdateOrApply(func(cur *State) (bool, error) {
		if _, err := requirePrincipalIn(cur, p, scope, time.Now()); err != nil {
			return false, err
		}
		prev, known := runNamesOf(cur, id)
		if !known {
			return false, nil
		}
		merged := mergeNames(prev, names)
		changed := !slices.Equal(prev, merged)
		if changed {
			setRunNames(cur, id, merged, false)
		}
		return changed, nil
	})
}
