package businessaction

import (
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

func TestDelegationLiveNamesExpiryAndStaysFailClosed(t *testing.T) {
	now := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	if err := delegationLive(now.Add(time.Second), now); err != nil {
		t.Fatalf("a delegation valid for another second was rejected: %v", err)
	}
	for name, expiresAt := range map[string]time.Time{
		"already past":          now.Add(-time.Minute),
		"exactly now (strict)":  now,
		"non-UTC clock offset":  now.In(time.FixedZone("UTC+8", 8*3600)),
		"long expired original": now.Add(-24 * time.Hour),
	} {
		err := delegationLive(expiresAt, now)
		if !errors.Is(err, ErrDelegationExpired) || !errors.Is(err, mcphost.ErrFailClosed) {
			t.Errorf("%s: err=%v, want ErrDelegationExpired that also fails closed", name, err)
		}
	}
	// A clock zone must not move the expiry: 09:00 UTC is 17:00 at UTC+8.
	if err := delegationLive(now.Add(time.Minute).In(time.FixedZone("UTC+8", 8*3600)), now); err != nil {
		t.Fatalf("a still-valid delegation expressed in another zone was rejected: %v", err)
	}
}
