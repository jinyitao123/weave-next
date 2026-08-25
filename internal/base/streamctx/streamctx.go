package streamctx

import "context"

type suppressStreamKey struct{}
type eventSenderKey struct{}

// EventSender is the minimal streaming event surface shared by runtime graph
// code without depending on the HTTP API adapter.
type EventSender interface {
	SendEvent(eventType string, data any) error
}

// SuppressStream returns a context that tells LLM adapters to bypass streaming
// for internal machine-readable steps.
func SuppressStream(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressStreamKey{}, true)
}

// IsSuppressStream reports whether streaming should be bypassed for this call.
func IsSuppressStream(ctx context.Context) bool {
	v, _ := ctx.Value(suppressStreamKey{}).(bool)
	return v
}

// WithEventSender returns a context carrying the active streaming sender.
func WithEventSender(ctx context.Context, sender EventSender) context.Context {
	return context.WithValue(ctx, eventSenderKey{}, sender)
}

// EventSenderFromContext retrieves the active streaming sender, or nil.
func EventSenderFromContext(ctx context.Context) EventSender {
	sender, _ := ctx.Value(eventSenderKey{}).(EventSender)
	return sender
}
