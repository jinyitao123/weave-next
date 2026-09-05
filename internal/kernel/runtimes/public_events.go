package runtimes

import (
	"errors"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/engine"
)

const PublicEventLimit = 512
const PublicEventBatchLimit = 16

// PublicEvent is a durable, per-physical-task sequence. Routing identities are
// intentionally absent: the server derives them from the stored engine task.
type PublicEvent struct {
	Seq        int64        `json:"seq"`
	OccurredAt time.Time    `json:"occurred_at"`
	Event      engine.Event `json:"event"`
	Truncated  bool         `json:"truncated,omitempty"`
}

func ValidatePublicEvent(event PublicEvent) error {
	if event.Seq < 1 || event.Seq > PublicEventLimit || event.OccurredAt.IsZero() {
		return errors.New("invalid public event identity")
	}
	switch event.Event.Kind {
	case "text", "tool_call", "tool_result":
		return engine.ValidateEvents([]engine.Event{event.Event})
	case "stream_end":
		if event.Event != (engine.Event{Kind: "stream_end"}) {
			return errors.New("invalid stream end")
		}
		return nil
	default:
		return errors.New("only public text and tool events are accepted")
	}
}
