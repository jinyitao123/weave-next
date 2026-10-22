package execution

import "context"

// SchedulingHints carry already authorized dispatch metadata. They never grant
// identity, permission or the right to preempt an executing request.
type SchedulingHints struct {
	Priority    int    `json:"priority"`
	FairnessKey string `json:"fairness_key,omitempty"`
}
type schedulingKey struct{}

func WithScheduling(ctx context.Context, hints SchedulingHints) context.Context {
	return context.WithValue(ctx, schedulingKey{}, hints)
}
func SchedulingFromContext(ctx context.Context) SchedulingHints {
	hints, _ := ctx.Value(schedulingKey{}).(SchedulingHints)
	return hints
}
