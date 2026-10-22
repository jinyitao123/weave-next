package teamrun

import (
	"errors"
	"testing"
	"time"
)

func TestReconcileRenewalKeepsClaimsThroughTransientFailure(t *testing.T) {
	now := time.Now()
	ttl := 60 * time.Second
	active := map[string]activeWorkflowClaim{
		"fresh":  {ttl: ttl, leaseUntil: now.Add(50 * time.Second)},
		"ending": {ttl: ttl, leaseUntil: now.Add(8 * time.Second)},
	}
	lost := reconcileRenewal(active, nil, errors.New("connection reset"), now, now.Add(10*time.Second))
	if _, stopped := lost["fresh"]; stopped {
		t.Fatal("a failed renewal statement must not stop a claim whose lease outlives the next attempt")
	}
	cause, stopped := lost["ending"]
	if !stopped || !errors.Is(cause, errTaskLeaseLost) {
		t.Fatalf("a claim that would expire before the next attempt must stop as lease lost: %v", cause)
	}
	if !active["fresh"].leaseUntil.Equal(now.Add(50 * time.Second)) {
		t.Fatal("a failed renewal must not extend the local lease bound")
	}
}

func TestReconcileRenewalExtendsRenewedAndStopsMissing(t *testing.T) {
	now := time.Now()
	ttl := 60 * time.Second
	active := map[string]activeWorkflowClaim{
		"kept":      {ttl: ttl, leaseUntil: now.Add(5 * time.Second)},
		"cancelled": {ttl: ttl, leaseUntil: now.Add(55 * time.Second)},
	}
	renewStart := now.Add(-time.Second)
	lost := reconcileRenewal(active, map[string]bool{"kept": true}, nil, renewStart, now.Add(10*time.Second))
	if len(lost) != 1 || !errors.Is(lost["cancelled"], errTaskLeaseLost) {
		t.Fatalf("a row missing from a successful renewal proves loss: %v", lost)
	}
	if got := active["kept"].leaseUntil; !got.Equal(renewStart.Add(ttl)) {
		t.Fatalf("renewed lease bound = %v, want measured from before the statement %v", got, renewStart.Add(ttl))
	}
}
