package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Exercise issuance policy in memory, without a guest, provider or real key.
func TestR50AICredentialDeliveryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		foreign, blocked, noEnv bool
		proxy, missingIdentity  bool
		wantRaw, wantProxy      bool
	}{
		{name: "local raw", wantRaw: true},
		{name: "local no env", noEnv: true},
		{name: "foreign allowed", foreign: true},
		{name: "foreign blocked", foreign: true, blocked: true},
		{name: "foreign without proxy identity", foreign: true, missingIdentity: true},
		{name: "local blocked", blocked: true},
		{name: "foreign proxy allowed", foreign: true, proxy: true, wantProxy: true},
		{name: "foreign proxy blocked", foreign: true, blocked: true, proxy: true, wantProxy: true},
		{name: "no env proxy", noEnv: true, proxy: true, wantProxy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kr, err := NewKeyring(bytes.Repeat([]byte{7}, 32))
			if err != nil {
				t.Fatal(err)
			}
			const team, vm, value = "team", "vm", "synthetic-provider-value"
			sealed, err := kr.Seal([]byte(value), "ai/"+aiCredKey(team, "openai"))
			if err != nil {
				t.Fatal(err)
			}
			st := newUsageState()
			st.Teams[team] = &Team{ID: team}
			st.ForeignVMs[vm] = tc.foreign
			st.AIProxyOff[team] = !tc.proxy
			st.AICreds[aiCredKey(team, "openai")] = &AICredential{TeamID: team, Provider: "openai", Sealed: sealed}
			if !tc.missingIdentity {
				st.AIVMs[vm] = &AIVM{TeamID: team, Gen: 1, Blocked: tc.blocked}
			}
			store := &Store{}
			store.cur.Store(st)
			s := &server{store: store, keys: kr, cfg: Config{AIProxyURL: "https://ai.example"}}
			env, err := s.aiEnvRun(vm, team, false, tc.noEnv)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.wantRaw:
				if env["OPENAI_API_KEY"] != value || len(env) != 1 {
					t.Fatal("local raw credential policy changed")
				}
			case tc.wantProxy:
				if env["OPENAI_API_KEY"] != s.aiToken(vm, 1) || env["OPENAI_BASE_URL"] != "https://ai.example/openai/v1" {
					t.Fatal("per-VM proxy credential policy changed")
				}
			default:
				if len(env) != 0 {
					t.Fatal("raw credential policy must omit credentials")
				}
			}
		})
	}
}

func TestR50AIMeterScopedSnapshot(t *testing.T) {
	st := newUsageState()
	store := &Store{}
	store.cur.Store(st)
	m := newAIMeter()
	at := time.Now().UTC()
	m.addAt(at, "mine", "a", "openai", "m", 2, 3)
	m.addAt(at, "mine", "b", "openai", "m", 4, 5)
	m.addAt(at, "other", "a", "openai", "m", 6, 7)
	for _, tc := range []struct {
		team, vm string
		want     int
	}{{"mine", "", 2}, {"mine", "a", 1}, {"other", "a", 1}, {"missing", "", 0}} {
		gotState, pending := m.view(store, tc.team, tc.vm)
		if gotState != st || len(pending) != tc.want {
			t.Fatalf("scope %s/%s: %d rows, want %d", tc.team, tc.vm, len(pending), tc.want)
		}
		for k, row := range pending {
			if row.TeamID != tc.team || (tc.vm != "" && row.VMID != tc.vm) || row == m.rows[k] {
				t.Fatal("snapshot must contain independent copies of matching rows")
			}
			row.Requests = 99
			if m.rows[k].Requests != 1 {
				t.Fatal("read changed the pending batch")
			}
		}
	}
}

func TestR50AIUsageConcurrentFlush(t *testing.T) {
	e := setup(t)
	team := e.teamID()
	const n = 40
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			e.srv.aiMeter.record(team, "a", "openai", "m", 2, 3, true)
			e.srv.aiMeter.add(team, "b", "openai", "m", 4, 5)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			e.srv.aiMeter.flush(e.srv)
		}
	}()
	for i := 0; i < n; i++ {
		for _, perDay := range []bool{false, true} {
			st, pending := e.srv.aiMeter.view(e.srv.store, team, "a")
			tot := usageTotals(teamAIUsageRows(st, pending, team, "a", perDay))
			if tot["requests"] > n || tot["input_tokens"] != 2*tot["requests"] || tot["output_tokens"] != 3*tot["requests"] || tot["unmetered"] != tot["requests"] {
				t.Errorf("inconsistent read during flush: %v", tot)
			}
		}
	}
	wg.Wait()
	e.srv.aiMeter.flush(e.srv)
	e.srv.aiMeter.flush(e.srv)
	for _, rows := range [][]*AIUsageRow{e.srv.aiUsageRows(team, "a"), e.srv.aiUsageDays(team, "a", time.Now().AddDate(0, 0, -1))} {
		tot := usageTotals(rows)
		if tot["requests"] != n || tot["input_tokens"] != 2*n || tot["output_tokens"] != 3*n || tot["unmetered"] != n {
			t.Fatalf("flush changed counters: %v", tot)
		}
	}
}

func BenchmarkR50AIMeterScopedView(b *testing.B) {
	for _, foreignRows := range []int{0, 1000, 20000} {
		b.Run(fmt.Sprint(foreignRows), func(b *testing.B) {
			store := &Store{}
			store.cur.Store(newUsageState())
			m := newAIMeter()
			m.add("mine", "a", "openai", "m", 2, 3)
			m.add("mine", "b", "openai", "m", 4, 5)
			for i := 0; i < foreignRows; i++ {
				m.add("other", fmt.Sprint(i), "openai", "m", 6, 7)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, pending := m.view(store, "mine", "a")
				if len(pending) != 1 {
					b.Fatal("unexpected scope")
				}
			}
		})
	}
}
