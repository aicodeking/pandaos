package main

import (
	"maps"
	"slices"
	"time"
)

// applyCommandHistoryLazy is ApplyLazy scoped to one VM's Commands and
// CommandSeq. fn may mutate only those two entries; every other field is shared
// with read-only snapshots. Ordinary store writers still use the full clone.
// The touched history is deeply copied, including nested pointers and spare
// append capacity, so a failed callback cannot change a published snapshot.
func (s *Store) applyCommandHistoryLazy(vmID string, fn func(*State) error, delay time.Duration) error {
	s.mu.Lock()
	cur := s.cur.Load()
	// Spell out the shallow copy: copying *cur would race with its lazy SSH
	// cache's sync.Once and AI index. Leave SSH caches fresh, and share the
	// immutable AI index through its existing synchronized accessor.
	next := &State{
		Users:                cur.Users,
		Teams:                cur.Teams,
		Keys:                 cur.Keys,
		Secrets:              cur.Secrets,
		Webhooks:             cur.Webhooks,
		Deliveries:           cur.Deliveries,
		SSHGrants:            cur.SSHGrants,
		VMSecrets:            cur.VMSecrets,
		Commands:             maps.Clone(cur.Commands),
		Prompts:              cur.Prompts,
		Conversations:        cur.Conversations,
		Environments:         cur.Environments,
		VMEnvs:               cur.VMEnvs,
		VMEnvPins:            cur.VMEnvPins,
		VMTransitions:        cur.VMTransitions,
		VMNames:              cur.VMNames,
		VMExtraEnv:           cur.VMExtraEnv,
		VMEnvErrors:          cur.VMEnvErrors,
		NoEnvRuns:            cur.NoEnvRuns,
		SnapShares:           cur.SnapShares,
		AICreds:              cur.AICreds,
		AIVMs:                cur.AIVMs,
		AIProxyOff:           cur.AIProxyOff,
		AIUsage:              cur.AIUsage,
		AIUsageDays:          cur.AIUsageDays,
		AIUsageDaysV1:        cur.AIUsageDaysV1,
		aiIdx:                cur.sharedAIUsageIndex(),
		Seq:                  cur.Seq,
		EnvErrorSpans:        cur.EnvErrorSpans,
		EnvErrorCutoffs:      cur.EnvErrorCutoffs,
		EnvErrorLegacyBefore: cur.EnvErrorLegacyBefore,
		Telemetry:            cur.Telemetry,
		ForeignVMs:           cur.ForeignVMs,
		RouteGens:            cur.RouteGens,
		PortMinters:          cur.PortMinters,
		PortMinterGens:       cur.PortMinterGens,
		DesktopVMs:           cur.DesktopVMs,
		StopOps:              cur.StopOps,
		StopLast:             cur.StopLast,
		DeletionOps:          cur.DeletionOps,
		Idempotency:          cur.Idempotency,
		idemIdx:              cur.idemIdx,
		VMTokens:             cur.VMTokens,
		CompatIDs:            cur.CompatIDs,
		CompatByVM:           cur.CompatByVM,
		CompatSetups:         cur.CompatSetups,
		CommandSeq:           maps.Clone(cur.CommandSeq),
		CopyCredentialsV1:    cur.CopyCredentialsV1,
		seq:                  cur.seq + 1,
	}
	if next.Commands == nil {
		next.Commands = map[string][]CommandRecord{}
	}
	if next.CommandSeq == nil {
		next.CommandSeq = map[string]int{}
	}
	if records := cur.Commands[vmID]; records != nil {
		// Reserve the single append without reusing published spare capacity.
		copied := make([]CommandRecord, len(records), len(records)+1)
		for i, rec := range records {
			copied[i] = cloneCommandRecord(rec)
		}
		next.Commands[vmID] = copied
	}
	if err := fn(next); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cur.Store(next)
	s.mu.Unlock()
	// Match ApplyLazy's coalescing window and shutdown/Flush behavior.
	if s.lazyPending.CompareAndSwap(false, true) {
		time.AfterFunc(delay, func() {
			s.lazyPending.Store(false)
			s.Kick()
		})
	}
	return nil
}

func cloneCommandRecord(rec CommandRecord) CommandRecord {
	rec.Cmd = slices.Clone(rec.Cmd)
	rec.Exit = cloneIntPtr(rec.Exit)
	rec.FinishedAt = cloneTimePtr(rec.FinishedAt)
	return rec
}
