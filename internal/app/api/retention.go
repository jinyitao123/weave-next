package api

import (
	"context"
	"log/slog"
	"time"
)

// DefaultRetentionDays is how many days to keep audit and trace data.
const DefaultRetentionDays = 90

// StartRetentionCleanup launches a background goroutine that periodically
// removes audit and trace records older than retentionDays.
func (s *Server) StartRetentionCleanup(retentionDays int) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}

	if s.StoreExt == nil {
		slog.Info("retention cleanup disabled (requires PGStore)")
		return
	}

	ext := s.StoreExt
	slog.Info("retention cleanup enabled", "retention_days", retentionDays, "interval", "1h")

	go func() {
		// Run once at startup after a short delay.
		time.Sleep(10 * time.Second)
		runCleanup(ext, retentionDays)

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			runCleanup(ext, retentionDays)
		}
	}()
}

func runCleanup(ext interface {
	CleanupOlderThan(ctx context.Context, nsPattern string, cutoff string) (int64, error)
}, retentionDays int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format(time.RFC3339)

	// Clean audit records.
	auditDeleted, err := ext.CleanupOlderThan(ctx, "audit:%", cutoff)
	if err != nil {
		slog.Warn("retention cleanup audit failed", "error", err)
	}

	// Clean trace records.
	traceDeleted, err := ext.CleanupOlderThan(ctx, "trace:%", cutoff)
	if err != nil {
		slog.Warn("retention cleanup trace failed", "error", err)
	}

	if auditDeleted > 0 || traceDeleted > 0 {
		slog.Info("retention cleanup completed",
			"audit_deleted", auditDeleted,
			"trace_deleted", traceDeleted,
			"cutoff", cutoff,
		)
	}
}
