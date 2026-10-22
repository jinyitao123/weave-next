package api

import (
	"log/slog"
	"os"
	"time"
)

func teamRunDrainTimeout() time.Duration {
	raw := os.Getenv("WEAVE_TEAMRUN_DRAIN_TIMEOUT")
	if raw == "" {
		return 0
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		slog.Warn("invalid WEAVE_TEAMRUN_DRAIN_TIMEOUT", "value", raw)
		return 0
	}
	return value
}
