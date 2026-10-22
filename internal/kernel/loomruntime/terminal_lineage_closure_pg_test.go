package loomruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

// fullScanTerminalStore hides the lineage index so a scan reads the whole
// namespace, as it did before migration 0201.
type fullScanTerminalStore struct{ TerminalRecordStore }

// TestTerminalLineageClosureMatchesFullScanRealPG assembles random audit
// namespaces both ways: the full namespace scan and the indexed closure read.
// The namespaces mix valid lineages with cycles, dangling and cross-tenant
// parents, stale child breakdowns, duplicate and mis-keyed records, and values
// the index can only over-approximate: case-variant field names the Go decoder
// accepts, invalid UTF-8, NUL escapes, non-JSON and pre-attribution records.
// Every target must assemble to the same entry or the same error, for both a
// rebuild and a self-write replacement; rewrites and deletes between rounds
// exercise the index trigger.
func TestTerminalLineageClosureMatchesFullScanRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := t.Context()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	indexed := storeext.New(pool)
	full := fullScanTerminalStore{indexed}
	if _, ok := TerminalRecordStore(full).(terminalLineageRelatedReader); ok {
		t.Fatal("full-scan store still exposes the lineage index")
	}
	const tenant = "lineage-ws"
	namespace := "audit:" + tenant
	rng := rand.New(rand.NewSource(20261022))
	put := func(key string, value []byte) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at) VALUES($1,$2,$3,now())
		 ON CONFLICT(namespace,key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, namespace, key, value); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *string { return &s }
	entry := func(runID string, input int) TerminalEntryV3 {
		self := TerminalUsage{InputTokens: input, OutputTokens: 1}
		return TerminalEntryV3{SchemaVersion: 3, RunID: runID, Agent: "agent", Tenant: tenant,
			AttributionScope: TerminalAttributionLegacyUnattributed, Status: "succeeded", StopReason: "done",
			StartedAt: "2026-10-22T00:00:00Z", EndedAt: "2026-10-22T00:00:01Z", DurationMs: 1000,
			TokensIn: input, TokensOut: 1, SelfExclusive: self, ChildBreakdown: []TerminalChildBreakdownV3{}, SubtreeTotal: self}
	}
	var compared, assembled, failed int
	for round := 0; round < 150; round++ {
		if _, err := pool.Exec(ctx, `DELETE FROM loom_store WHERE namespace=$1`, namespace); err != nil {
			t.Fatal(err)
		}
		runs := make([]string, 4+rng.Intn(10))
		for i := range runs {
			runs[i] = fmt.Sprintf("r%d", i)
		}
		pick := func() string {
			if rng.Intn(12) == 0 {
				return "ghost"
			}
			return runs[rng.Intn(len(runs))]
		}
		for i, runID := range runs {
			e := entry(runID, 1+i)
			if i > 0 && rng.Intn(5) != 0 {
				// Mostly a forest over earlier runs; sometimes any run, so cycles form.
				parent := runs[rng.Intn(i)]
				if rng.Intn(6) == 0 {
					parent = pick()
				}
				if parent != runID {
					seq := int64(rng.Intn(3))
					e.ParentRunID, e.ParentSeq = str(parent), &seq
					if rng.Intn(5) != 0 {
						e.AggregationParentRunID = str(parent)
					}
				}
			}
			if rng.Intn(3) == 0 {
				for range 1 + rng.Intn(3) {
					child := pick()
					if child == runID || child == "ghost" && rng.Intn(2) == 0 {
						continue
					}
					duplicate := false
					for _, existing := range e.ChildBreakdown {
						duplicate = duplicate || existing.RunID == child
					}
					if duplicate {
						continue
					}
					usage := TerminalUsage{InputTokens: 1 + rng.Intn(3), OutputTokens: 1}
					e.ChildBreakdown = append(e.ChildBreakdown, TerminalChildBreakdownV3{RunID: child, ParentRunID: runID, ParentSeq: int64(rng.Intn(3)), Agent: "agent", SelfExclusive: usage})
					e.SubtreeTotal, _ = addTerminalUsage(e.SubtreeTotal, usage)
				}
			}
			if rng.Intn(15) == 0 {
				e.Tenant = "other-tenant"
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			key := runID
			switch rng.Intn(14) {
			case 0:
				key = "dup-" + runID // a second record claiming this run
				put(runID, raw)
			case 1:
				key = pick() // a record stored under another run's key
			case 2:
				raw = bytes.Replace(raw, []byte(`"parent_run_id"`), []byte(`"Parent_Run_ID"`), 1)
			case 3:
				raw = []byte(`{"schema_version":3,"run_id":"` + runID + `"`) // truncated
			case 4:
				raw = append([]byte{0xff, 0xfe}, raw...)
			case 5:
				raw = bytes.Replace(raw, []byte(`"agent":"agent"`), []byte(`"agent":"a\u0000b"`), 1)
			case 6:
				raw = []byte(`{"schema_version":2,"run_id":"` + runID + `"}`)
			}
			put(key, raw)
		}
		if rng.Intn(3) == 0 {
			// Rewrite and delete records so the trigger's update and delete paths run.
			victim := runs[rng.Intn(len(runs))]
			raw, _ := json.Marshal(entry(victim, 99))
			put(victim, raw)
			if _, err := pool.Exec(ctx, `DELETE FROM loom_store WHERE namespace=$1 AND key=$2`, namespace, runs[rng.Intn(len(runs))]); err != nil {
				t.Fatal(err)
			}
		}
		for _, target := range append(runs, "ghost") {
			wantEntry, wantErr := scanAndAssembleTerminalLineage(ctx, full, tenant, target)
			gotEntry, gotErr := scanAndAssembleTerminalLineage(ctx, indexed, tenant, target)
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(gotEntry, wantEntry) {
				t.Fatalf("round %d target %s: closure assembled %+v, %v; full scan %+v, %v", round, target, gotEntry, gotErr, wantEntry, wantErr)
			}
			compared++
			if wantErr == nil {
				assembled++
			} else {
				failed++
			}
			value, present, err := indexed.ReadValue(ctx, namespace, target)
			if err != nil {
				t.Fatal(err)
			}
			if inspection := InspectTerminalRecord(present, value); inspection.Entry != nil {
				candidate := *inspection.Entry
				candidate.SelfExclusive.InputTokens++
				candidate.TokensIn++
				wantEntry, wantErr = scanAndAssembleTerminalLineageReplacement(ctx, full, candidate)
				gotEntry, gotErr = scanAndAssembleTerminalLineageReplacement(ctx, indexed, candidate)
				if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(gotEntry, wantEntry) {
					t.Fatalf("round %d replacement %s: closure %+v, %v; full scan %+v, %v", round, target, gotEntry, gotErr, wantEntry, wantErr)
				}
				compared++
			}
		}
	}
	// Both outcomes must be common, or the comparison proves little.
	if assembled < compared/10 || failed < compared/10 {
		t.Fatalf("random namespaces too uniform: %d comparisons, %d assembled, %d failed", compared, assembled, failed)
	}
	t.Logf("%d comparisons: %d assembled, %d failed", compared, assembled, failed)
}
