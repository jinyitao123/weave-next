package businessaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// An expiry that surfaces during a call reaches the member as a plain tool
// result. It must say the authorization expired and that nothing was executed,
// not the generic business-failure text, and the receipt must record a failure.
func TestForgeActionExpiryDuringACallIsExplainedToTheMember(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{err: fmt.Errorf("%w: MCP dispatch authority expired: %w", mcphost.ErrFailClosed, ErrDelegationExpired)}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	result, err := dispatcher.Dispatch(outcomeTestContext(&events, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
		return ActionOutcomeReplay{}, nil
	}, nil), call)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Content, "授权已过期") || !strings.Contains(result.Content, "没有执行") ||
		strings.Contains(result.Content, "返回失败") {
		t.Fatalf("the member was not told the authorization expired: %q", result.Content)
	}
	if len(events) != 2 || events[1].Phase != "result" || events[1].Status != ActionOutcomeStatusFailed {
		t.Fatalf("receipts = %+v, want a start and a failed result", events)
	}

	// Any other refusal keeps the generic wording.
	other := &outcomeTestHost{err: fmt.Errorf("%w: MCP dispatch authority expired: %w", mcphost.ErrFailClosed, errors.New("scope changed"))}
	otherDispatcher, otherCall := newTrackedOutcomeDispatcher(t, other)
	generic, _ := otherDispatcher.Dispatch(outcomeTestContext(&[]ActionOutcomeEvent{}, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
		return ActionOutcomeReplay{}, nil
	}, nil), otherCall)
	if generic == nil || strings.Contains(generic.Content, "授权已过期") {
		t.Fatalf("a non-expiry refusal was reported as an expiry: %+v", generic)
	}
}
