package main

import (
	"log"
	"time"
)

// no_env is sticky since r46: a stop never ends it, so the mark needs no ordering
// against stops. Earlier versions kept a "mark:<vm>" record next to the stop
// operations to order the two by a sequence number; such records may still be in
// a loaded state. They are skipped when stops are listed (isMarkRecord) and
// dropped when the VM is marked again.

const markRecordPrefix = "mark:"

func markRecordKey(vmID string) string { return markRecordPrefix + vmID }

// isMarkRecord reports whether a StopOps entry is a (legacy) mark record, not a stop.
func isMarkRecord(op map[string]any) bool {
	v, _ := op["_mark"].(bool)
	return v
}

// runMark is a VM's no_env mark as it was, to put back (restoreRunMark).
type runMark struct {
	had      bool
	at       time.Time
	tokNoEnv int // the token's NoEnvGen at the time
}

func (s *server) readRunMark(vmID string) runMark {
	st := s.store.Snapshot()
	at, had := st.NoEnvRuns[vmID]
	m := runMark{had: had, at: at}
	if t := st.VMTokens[vmID]; t != nil {
		m.tokNoEnv = t.NoEnvGen
	}
	return m
}

// restoreRunMark puts the VM's no_env mark back as it was: a start that did not
// take effect gives back the mark of the VM it found (none: it is not no_env).
func (s *server) restoreRunMark(vmID string, m runMark) {
	if cur := s.readRunMark(vmID); cur.tokNoEnv == m.tokNoEnv && cur.had == m.had && cur.at.Equal(m.at) {
		return
	}
	err := s.store.Update(func(st *State) error {
		delete(st.NoEnvRuns, vmID)
		delete(st.StopOps, markRecordKey(vmID))
		if m.had {
			st.NoEnvRuns[vmID] = m.at
		}
		if t := st.cowVMToken(vmID); t != nil {
			t.NoEnvGen = m.tokNoEnv
		}
		return nil
	})
	if err != nil {
		log.Printf("restore no_env mark of %s: %v", vmID, err) // stays as it is: fail closed
	}
}
