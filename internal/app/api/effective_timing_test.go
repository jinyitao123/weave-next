package api

import (
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/config"
)

// The slow-run alert is read back with the workflows that set their own
// threshold, so a deployment can see which one each workflow is judged by.
func TestEffectiveTimingListsSlowRunOverrides(t *testing.T) {
	s := &Server{Config: &config.Config{HealthSlowRunSeconds: 600}}
	if alert := s.effectiveTiming()["slow_run_alert"]; alert.Seconds != 600 || alert.Overrides != nil {
		t.Fatalf("without overrides = %+v", alert)
	}
	s.Config.HealthSlowRunOverrides = map[string]time.Duration{"chat": 2 * time.Minute}
	if alert := s.effectiveTiming()["slow_run_alert"]; alert.Seconds != 600 || len(alert.Overrides) != 1 || alert.Overrides["chat"] != 120 {
		t.Fatalf("with overrides = %+v", alert)
	}
}
