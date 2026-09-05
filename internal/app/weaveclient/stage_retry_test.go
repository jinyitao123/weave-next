package weaveclient

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestStageRetryClientPreservesDurableRequestIdentity(t *testing.T) {
	calls := 0
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/runs/run-1/stages/research%2Fone/retry" {
			t.Errorf("wrong exact-stage route: %s %s", r.Method, r.URL.EscapedPath())
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["idempotency_key"] != "persisted-click" || len(body) != 1 {
			t.Errorf("unstable retry identity: %+v %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"run_id":"run-1","status":"queued","replayed":true}`))
	}))
	for range 2 {
		if _, err := client.TeamRunRetryStage(context.Background(), "run-1", "research/one", "persisted-click"); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 257)} {
		if _, err := client.TeamRunRetryStage(context.Background(), "run-1", "research/one", key); err == nil {
			t.Fatal("retry without durable identity sent")
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected transport retries: %d", calls)
	}
}
