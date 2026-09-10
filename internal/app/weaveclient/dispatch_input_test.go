package weaveclient

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestTeamDispatchBoundInputNeverInventsTextOrRequestKey(t *testing.T) {
	const revision = "00000000-0000-4000-8000-000000000021"
	const fixedRequest = "00000000-0000-4000-8000-000000000022"
	for _, taskProvided := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "explicit_empty"}[taskProvided], func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				_, hasTask := body["task"]
				if body["input_revision_id"] != revision || hasTask != taskProvided {
					t.Fatalf("bound input assertion changed: %#v", body)
				}
				if _, hasNonce := body["client_request_id"]; hasNonce {
					t.Fatal("client invented a nonce for registered input")
				}
				writeJSON(response, http.StatusOK, `{"input_revision_id":"`+revision+`","client_request_id":"`+fixedRequest+`","run_id":"existing"}`)
			}))
			id, _, err := client.TeamDispatch(context.Background(), DispatchRequest{TeamID: "team", InputRevisionID: revision, TaskProvided: taskProvided})
			if err != nil || id != fixedRequest {
				t.Fatalf("fixed receipt key=%q err=%v", id, err)
			}
		})
	}
}

func TestTeamDispatchBoundInputRejectsMismatchedReceipt(t *testing.T) {
	const revision = "00000000-0000-4000-8000-000000000021"
	const fixedRequest = "00000000-0000-4000-8000-000000000022"
	client := newTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeJSON(response, http.StatusOK, `{"input_revision_id":"`+revision+`","client_request_id":"00000000-0000-4000-8000-000000000023","run_id":"wrong"}`)
	}))
	_, _, err := client.TeamDispatch(context.Background(), DispatchRequest{TeamID: "team", InputRevisionID: revision, ClientRequestID: fixedRequest})
	assertClientError(t, err, "dispatch_input_receipt_invalid", 0)
}
