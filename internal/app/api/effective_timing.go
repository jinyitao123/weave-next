package api

import (
	"os"
	"strings"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// effectiveTimingValue is one time bound in force, where it came from and
// what it bounds, so a deployment can see which settings it is running on
// rather than infer them (#107).
type effectiveTimingValue struct {
	Seconds float64 `json:"seconds"`
	// Source is env (set for this deployment), default, or fixed (not
	// configurable).
	Source string `json:"source"`
	Scope  string `json:"scope"`
	// Overrides lists, by workflow ID, the seconds that replace Seconds for
	// single workflows; only the slow-run alert has them.
	Overrides map[string]float64 `json:"overrides,omitempty"`
}

func timingSource(env string) string {
	if strings.TrimSpace(os.Getenv(env)) != "" {
		return "env"
	}
	return "default"
}

// effectiveTiming lists the deployment-wide bounds. A member's own stage and
// call budgets are frozen with its published version and shown with it.
func (s *Server) effectiveTiming() map[string]effectiveTimingValue {
	timing := map[string]effectiveTimingValue{
		"member_step_limit": {Seconds: float64(frozen.MemberNodeExecutionLimitMS) / 1000, Source: "fixed",
			Scope: "one member step of a workflow, every model call in it included; a member wait budget must be shorter"},
		"provider_attempt_default": {Seconds: llmrouter.DefaultAttemptTimeoutSeconds, Source: "default",
			Scope: "one provider request when the provider sets no attempt_timeout_seconds"},
		"shutdown_drain": {Seconds: teamRunDrainTimeout().Seconds(), Source: timingSource("WEAVE_TEAMRUN_DRAIN_TIMEOUT"),
			Scope: "how long a stopping pooled executor waits for work in flight; 0 does not wait"},
	}
	if s.Config != nil {
		alert := effectiveTimingValue{Seconds: float64(s.Config.HealthSlowRunSeconds), Source: timingSource("WEAVE_HEALTH_SLOW_RUN_SECONDS"),
			Scope: "when workflow health counts a run as slow; observation only, it never ends a run; WEAVE_HEALTH_SLOW_RUN_OVERRIDES sets it for single workflows"}
		if len(s.Config.HealthSlowRunOverrides) > 0 {
			alert.Overrides = make(map[string]float64, len(s.Config.HealthSlowRunOverrides))
			for workflowID, threshold := range s.Config.HealthSlowRunOverrides {
				alert.Overrides[workflowID] = threshold.Seconds()
			}
		}
		timing["slow_run_alert"] = alert
	}
	return timing
}
