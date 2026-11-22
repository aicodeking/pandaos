package main

import (
	"fmt"
	"testing"
	"time"
)

// Reproduce with:
// go test -run '^$' -bench '^BenchmarkCommandHistoryPopulated$' -benchmem -benchtime=50x -count=3 -cpu=4
// Both paths run the same mutation under the real store writer mutex, starting
// with 64 VMs x 200 history records and the stated number of unrelated lifetime
// AND daily AI rows. RunParallel exposes writer contention. Suppressing timers
// equally excludes JSON/sealing/fsync and measures only in-memory publication.
func BenchmarkCommandHistoryPopulated(b *testing.B) {
	for _, aiRows := range []int{1000, 10000} {
		for _, operation := range []string{"Append", "Refresh"} {
			for _, scoped := range []bool{false, true} {
				path := "ApplyLazy"
				if scoped {
					path = "Scoped"
				}
				b.Run(fmt.Sprintf("AI%d/%s/%s", aiRows, operation, path), func(b *testing.B) {
					st := &State{}
					st.init()
					for i := 0; i < aiRows; i++ {
						key := fmt.Sprintf("other-%d/vm/provider/model", i)
						st.AIUsage[key] = &AIUsageRow{TeamID: fmt.Sprintf("other-%d", i), Requests: 5}
						st.AIUsageDays[key+"/2026-01-01"] = &AIUsageRow{TeamID: fmt.Sprintf("other-%d", i), Day: "2026-01-01", Requests: 5}
					}
					for i := 0; i < 64; i++ {
						id := fmt.Sprintf("vm-%d", i)
						for n := 0; n < maxCommandRecords; n++ {
							exit, finished := 0, time.Unix(1, 0).UTC()
							rec := CommandRecord{ID: fmt.Sprintf("cmd-%d", n), Cmd: []string{"echo", "hello"}, State: "exited", Exit: &exit, FinishedAt: &finished, ProcessID: n + 1}
							if i == 0 && operation == "Append" {
								// Match the appended records so allocations do not depend
								// on how many iterations replaced the starting history.
								rec.State, rec.Exit, rec.FinishedAt = "running", nil, nil
							}
							st.Commands[id] = append(st.Commands[id], rec)
						}
						st.CommandSeq[id] = maxCommandRecords
					}
					store := &Store{kick: make(chan struct{}, 1)}
					store.cur.Store(st)
					store.lazyPending.Store(true)
					mutate := func(st *State) error {
						if operation == "Append" {
							st.CommandSeq["vm-0"]++
							list := append(st.Commands["vm-0"], cloneCommandRecord(CommandRecord{ID: "new", Cmd: []string{"echo", "hello"}, State: "running", ProcessID: st.CommandSeq["vm-0"]}))
							st.Commands["vm-0"] = list[len(list)-maxCommandRecords:]
						} else {
							rec := &st.Commands["vm-0"][0]
							if rec.State == "running" {
								rec.State = "exited"
							} else {
								rec.State = "running"
							}
							exit, finished := 0, time.Unix(2, 0).UTC()
							rec.Exit, rec.FinishedAt = &exit, &finished
						}
						return nil
					}
					apply := func() error { return store.ApplyLazy(mutate, time.Hour) }
					if scoped {
						apply = func() error { return store.applyCommandHistoryLazy("vm-0", mutate, time.Hour) }
					}
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if err := apply(); err != nil {
								b.Error(err)
							}
						}
					})
				})
			}
		}
	}
}
