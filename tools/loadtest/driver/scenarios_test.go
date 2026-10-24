package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdmissionErrorsKeepUnknownOutcomesSeparateFromRejections(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		outcome string
	}{
		{"quota rejection", http.StatusTooManyRequests, outcomeRejected},
		{"server error after possible admission", http.StatusInternalServerError, outcomeUnsettled},
		{"unreadable acceptance", http.StatusAccepted, outcomeUnsettled},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("unreadable response"))
			}))
			defer server.Close()
			env := &scenarioEnv{caller: newAPIClient(server.URL, 1), prov: &provisioned{
				Capabilities: map[string]string{flowDecide: "flow"}, Revisions: map[string]int64{flowDecide: 1},
			}}
			measurement, err := env.invoke(context.Background(), flowDecide, "lane")
			if err == nil || measurement.outcome != test.outcome {
				t.Fatalf("admission result: outcome=%s err=%v, want %s", measurement.outcome, err, test.outcome)
			}
		})
	}
}

func TestLadderDoesNotAdvanceWhileOnePercentHasNotSettled(t *testing.T) {
	env := &scenarioEnv{o: options{minSuccess: .99, p95Target: 30 * time.Second}}
	result := &latencyResult{Outcomes: outcomeCounts{Submitted: 100, Completed: 99, Unsettled: 1}, SuccessRate: .99, AllP95MS: 1000}
	if verdict := env.verdict(result); verdict.Passed {
		t.Fatal("advanced with an invocation still unsettled, although success rate and P95 passed")
	}
	result.Outcomes = outcomeCounts{Submitted: 100, Completed: 99, Failed: 1}
	if verdict := env.verdict(result); !verdict.Passed {
		t.Fatalf("all invocations settled and both thresholds passed: %s", verdict.Reason)
	}
}
