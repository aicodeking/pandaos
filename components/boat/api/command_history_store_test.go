package main

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestCommandHistoryScopedSnapshots(t *testing.T) {
	st := &State{}
	st.init()
	st.AIUsage["other/vm/provider/model"] = &AIUsageRow{TeamID: "other", Requests: 3}
	st.AIUsageDays["other/vm/provider/model/day"] = &AIUsageRow{TeamID: "other", Requests: 3}
	exit, finished := 0, time.Unix(1, 0).UTC()
	// Spare capacity must never let an append write into a published snapshot.
	rows := make([]CommandRecord, 1, maxCommandRecords+1)
	rows[0] = CommandRecord{ID: "old", Cmd: []string{"true"}, State: "exited", Exit: &exit, FinishedAt: &finished, ProcessID: 1}
	st.Commands["vm"] = rows
	st.CommandSeq["vm"] = 1
	st.Commands["other"] = []CommandRecord{{ID: "unrelated", Cmd: []string{"echo", "other"}}}
	store := &Store{kick: make(chan struct{}, 1)}
	store.cur.Store(st)
	// Suppress timers in this in-memory fixture; persistence is tested separately.
	store.lazyPending.Store(true)
	srv := &server{store: store}
	argv := []string{"echo", "hello"}
	if err := srv.recordCommand("vm", CommandRecord{ID: "new", Cmd: argv, State: "running"}); err != nil {
		t.Fatal(err)
	}
	appended := store.Snapshot()
	if appended.AIUsage["other/vm/provider/model"] != st.AIUsage["other/vm/provider/model"] || appended.AIUsageDays["other/vm/provider/model/day"] != st.AIUsageDays["other/vm/provider/model/day"] {
		t.Fatal("command append cloned unrelated AI usage rows")
	}
	argv[0] = "changed by caller"
	if appended.Commands["vm"][1].Cmd[0] != "echo" {
		t.Fatal("command append retained caller-owned argv")
	}
	if len(st.Commands["vm"]) != 1 || rows[:cap(rows)][1].ID != "" || st.CommandSeq["vm"] != 1 {
		t.Fatal("append mutated a previous snapshot or its spare capacity")
	}
	if appended.Commands["vm"][1].ProcessID != 2 || appended.seq != st.seq+1 {
		t.Fatal("append lost process or store sequence")
	}
	srv.refreshHistory("vm", map[string]any{"cmds": []any{map[string]any{"cmd_id": "new", "state": "exited", "exit": float64(7)}}})
	refreshed := store.Snapshot()
	rec := refreshed.Commands["vm"][1]
	if rec.State != "exited" || rec.Exit == nil || *rec.Exit != 7 || rec.FinishedAt == nil || rec.FinishedAt.Location() != time.UTC {
		t.Fatalf("refresh = %+v", rec)
	}
	if appended.Commands["vm"][1].State != "running" || appended.Commands["vm"][1].Exit != nil || appended.Commands["vm"][1].FinishedAt != nil {
		t.Fatal("refresh mutated a previous snapshot")
	}
	if refreshed.CommandSeq["vm"] != 2 || refreshed.seq != appended.seq+1 || !refreshed.Commands["vm"][0].FinishedAt.Equal(finished) {
		t.Fatal("refresh changed process sequence or an existing finish timestamp")
	}
	srv.refreshHistory("vm", map[string]any{"cmds": []any{map[string]any{"cmd_id": "new", "state": "exited", "exit": float64(7)}}})
	if store.Snapshot() != refreshed {
		t.Fatal("unchanged refresh published another state")
	}
	// Even in-place writes through old record pointers must be isolated on error.
	wantErr := errors.New("abort")
	err := store.applyCommandHistoryLazy("vm", func(next *State) error {
		next.Commands["vm"][0].Cmd[0] = "changed"
		*next.Commands["vm"][0].Exit = 99
		*next.Commands["vm"][0].FinishedAt = time.Time{}
		next.CommandSeq["vm"]++
		return wantErr
	}, time.Hour)
	if !errors.Is(err, wantErr) || store.Snapshot() != refreshed || refreshed.Commands["vm"][0].Cmd[0] != "true" || *refreshed.Commands["vm"][0].Exit != 0 || !refreshed.Commands["vm"][0].FinishedAt.Equal(finished) || refreshed.CommandSeq["vm"] != 2 {
		t.Fatal("failed callback changed a published snapshot")
	}
	for i := 0; i < maxCommandRecords; i++ {
		if err := srv.recordCommand("vm", CommandRecord{ID: "capped", State: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	final := store.Snapshot()
	if len(final.Commands["vm"]) != maxCommandRecords || final.Commands["vm"][0].ProcessID != 3 || final.CommandSeq["vm"] != maxCommandRecords+2 || len(final.Commands["other"]) != 1 {
		t.Fatal("capping lost sequence or changed another VM")
	}
}

func TestCommandHistoryScopedStateFields(t *testing.T) {
	// Give every persisted field a nonzero value so newly added fields cannot
	// silently disappear from the explicit shallow copy.
	st := &State{}
	v := reflect.ValueOf(st).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Uint64:
			f.SetUint(17)
		case reflect.Struct:
			if f.Type() != reflect.TypeOf(time.Time{}) {
				t.Fatalf("add a nonzero fixture for State.%s", v.Type().Field(i).Name)
			}
			f.Set(reflect.ValueOf(time.Unix(17, 0).UTC()))
		default:
			t.Fatalf("add a nonzero fixture for State.%s", v.Type().Field(i).Name)
		}
	}
	store := &Store{kick: make(chan struct{}, 1)}
	store.cur.Store(st)
	store.lazyPending.Store(true)
	st.aiUsageIdx()
	st.sshGrantsByPub()
	if err := store.applyCommandHistoryLazy("vm", func(*State) error { return nil }, time.Hour); err != nil {
		t.Fatal(err)
	}
	next := store.Snapshot()
	nv := reflect.ValueOf(next).Elem()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).CanInterface() && !reflect.DeepEqual(v.Field(i).Interface(), nv.Field(i).Interface()) {
			t.Fatalf("scoped update lost State.%s", v.Type().Field(i).Name)
		}
	}
	if next.seq != st.seq+1 {
		t.Fatal("scoped update lost store sequence")
	}
	if next.sharedAIUsageIndex() != st.sharedAIUsageIndex() {
		t.Fatal("scoped update discarded the immutable AI index")
	}
}

func TestCommandHistoryScopedConcurrentPersistence(t *testing.T) {
	for _, barrier := range []string{"Flush", "Update", "Close", "Delay"} {
		t.Run(barrier, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			srv := &server{store: store}
			var wg sync.WaitGroup
			for n := 0; n < 4; n++ {
				wg.Add(1)
				go func(n int) {
					defer wg.Done()
					for i := 0; i < 8; i++ {
						if err := srv.recordCommand("vm", CommandRecord{ID: fmt.Sprintf("%d/%d", n, i), State: "running"}); err != nil {
							t.Error(err)
						}
					}
				}(n)
			}
			// Exercise ordinary deep-copy writers and the snapshot caches alongside
			// command appends; old snapshots must remain safe for concurrent reads.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					if err := store.Stage(func(st *State) error {
						st.AIUsage["other"] = &AIUsageRow{TeamID: "other", Requests: int64(i)}
						st.dropAIUsageIndex()
						return nil
					}); err != nil {
						t.Error(err)
					}
				}
			}()
			for i := 0; i < 32; i++ {
				st := store.Snapshot()
				st.aiUsageIdx()
				st.sshGrantsByPub()
				for _, rec := range st.Commands["vm"] {
					if rec.ProcessID <= 0 {
						t.Error("observed unassigned process ID")
					}
				}
			}
			wg.Wait()
			before := store.Snapshot()
			if len(before.Commands["vm"]) != 32 || before.CommandSeq["vm"] != 32 || before.seq != 40 {
				t.Fatal("concurrent writer lost a command or sequence")
			}
			for i, rec := range before.Commands["vm"] {
				if rec.ProcessID != i+1 {
					t.Fatal("concurrent process IDs are not ordered")
				}
			}
			switch barrier {
			case "Flush":
				err = store.Flush()
			case "Update":
				err = store.Update(func(st *State) error { st.Seq++; return nil })
			case "Close":
				err = store.Close()
			case "Delay":
				deadline := time.Now().Add(2 * idemPersistDelay)
				for store.writes.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if store.writes.Load() == 0 {
					t.Fatal("lazy command update never flushed")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.persist(before); barrier == "Close" {
				if !errors.Is(err, errStoreClosed) {
					t.Fatal("closed store accepted another write")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !reflect.DeepEqual(before.Commands, reopened.Snapshot().Commands) || reopened.Snapshot().CommandSeq["vm"] != 32 {
				t.Fatal("persistence lost command history")
			}
			if barrier == "Update" && reopened.Snapshot().Seq != 1 {
				t.Fatal("an older snapshot overwrote the durable update")
			}
		})
	}
}

func TestCommandHistoryScopedEnvErrorCoverage(t *testing.T) {
	st := &State{}
	st.init()
	base := time.Now().UTC().Add(-time.Hour)
	st.EnvErrorCutoffs["vm"] = base
	st.EnvErrorLegacyBefore = base.Add(-time.Hour)
	store := &Store{kick: make(chan struct{}, 1)}
	store.cur.Store(st)
	store.lazyPending.Store(true)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 32; i++ {
			if err := store.applyCommandHistoryLazy("vm", func(next *State) error {
				next.CommandSeq["vm"]++
				return nil
			}, time.Hour); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 32; i++ {
			if err := store.Stage(func(next *State) error {
				recordEnvErrorCutoff(next, "vm", base.Add(time.Duration(i+1)*time.Second))
				return nil
			}); err != nil {
				t.Error(err)
			}
		}
	}()
	for i := 0; i < 64; i++ {
		snapshot := store.Snapshot()
		if snapshot.EnvErrorCutoffs["vm"].Before(base) || !snapshot.EnvErrorLegacyBefore.Equal(st.EnvErrorLegacyBefore) {
			t.Error("scoped clone lost coverage")
		}
	}
	wg.Wait()
	if !st.EnvErrorCutoffs["vm"].Equal(base) || !store.Snapshot().EnvErrorCutoffs["vm"].Equal(base.Add(32*time.Second)) {
		t.Fatal("ordinary/scoped writers changed an old snapshot or lost the latest cutoff")
	}
}
