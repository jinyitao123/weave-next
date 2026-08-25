package fanout

import "github.com/jinyitao123/weave/internal/base/taskqueue"

type TaskLegMapping struct {
	State    LegDecisionState
	Terminal *LegTerminal
	Reason   string
}

func AdaptTaskStatus(status string) (TaskLegMapping, error) {
	state := LegDecisionState("")
	var terminal LegTerminal
	result := TaskLegMapping{}
	switch status {
	case taskqueue.StatusQueued, taskqueue.StatusDispatched:
		state = LegDecisionQueued
	case taskqueue.StatusRunning:
		state = LegDecisionRunning
	case taskqueue.StatusCancelRequested:
		state = LegDecisionCancelRequested
	case taskqueue.StatusCompleted:
		state, terminal = LegDecisionSucceeded, LegSucceeded
	case taskqueue.StatusFailed:
		state, terminal = LegDecisionFailed, LegFailed
	case taskqueue.StatusTimedOut:
		state, terminal = LegDecisionTimeout, LegTimeout
	case taskqueue.StatusCut:
		state, terminal = LegDecisionCut, LegCut
	case taskqueue.StatusCancelled:
		state, terminal = LegDecisionCancelled, LegCancelled
	case taskqueue.StatusSuperseded:
		state, terminal, result.Reason = LegDecisionCancelled, LegCancelled, "superseded"
	default:
		return TaskLegMapping{}, workflowError(ErrorUnknownLegStatus, "unknown taskqueue status %q", status)
	}
	result.State = state
	if terminal != "" {
		result.Terminal = &terminal
	}
	return result, nil
}
