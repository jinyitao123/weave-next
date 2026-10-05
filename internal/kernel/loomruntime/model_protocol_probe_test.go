package loomruntime

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

func TestProbeSampleReplacementPreservesOriginalJournalBytes(t *testing.T) {
	raw := []byte(`{ "kind":"model", "input": { "protocol_probe":["nested"], "request":"original" }, "input_hash":"same", "response" : { "z":1, "a":2 }, "protocol_probe": [ {"state":"first"}, {"state":"second"} ], "usage": {"z":1} }`)
	updated, err := replaceProtocolProbeSample(raw, 1, []byte(`{"state":"observed"}`))
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal(raw, &before) != nil || json.Unmarshal(updated, &after) != nil {
		t.Fatal("journal JSON changed shape")
	}
	for _, key := range []string{"kind", "input", "input_hash", "response", "usage"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Fatalf("probe rewrote original %s bytes", key)
		}
	}
	if !bytes.Contains(updated, []byte(`[ {"state":"first"}, {"state":"observed"} ]`)) {
		t.Fatal("replacement touched another observation")
	}
}

func TestProtocolProbeUnknownExpiryAndPhysicalCaps(t *testing.T) {
	now := time.Now()
	policy := &ModelProtocolProbePolicy{ExpiresAt: now.Add(time.Minute), Now: func() time.Time { return now }}
	collector := &modelProtocolProbeCollector{policy: policy, sample: ModelProtocolProbeSample{Observed: []llmrouter.ModelProtocolObservation{}}}
	if collector.result().State != "unavailable" {
		t.Fatal("missing observation was called zero")
	}
	count := 0
	collector.normalized(llmrouter.NormalizedModelProtocol{Count: &count})
	for index := 0; index < 5; index++ {
		collector.observe(llmrouter.ModelProtocolObservation{Complete: true})
	}
	result := collector.result()
	if result.State != "incomplete" || len(result.Observed) != 4 || result.DroppedObservations != 1 {
		t.Fatal("physical attempt observation cap failed")
	}
	now = now.Add(time.Hour)
	lateCount := 10
	collector.normalized(llmrouter.NormalizedModelProtocol{Count: &lateCount, Error: true})
	collector.observe(llmrouter.ModelProtocolObservation{Complete: true})
	if collector.result().State != "incomplete" || len(collector.result().Observed) != 4 || *collector.result().NormalizedToolCount != 0 || collector.result().NormalizedError {
		t.Fatal("expired observation was captured")
	}
}

func TestProtocolProbeMergeKeepsLateEarlierGeneration(t *testing.T) {
	earlier := ModelProtocolProbeSample{PolicySHA256: "policy", AttemptGeneration: 1, Attempt: 1, State: "incomplete"}
	current := ModelProtocolProbeSample{PolicySHA256: "policy", AttemptGeneration: 2, Attempt: 2, State: "observed"}
	local := []ModelProtocolProbeSample{earlier, current}
	earlier.State = "observed"
	merged := mergeProtocolProbeSamples([]ModelProtocolProbeSample{earlier, current}, local, 2, 2)
	if len(merged) != 2 || merged[0].State != "observed" || merged[1].State != "observed" {
		t.Fatal("current receipt erased a late earlier-attempt observation")
	}
}
