package api

import (
	"context"
	"errors"
	"log/slog"
)

// RunExecutor starts the same durable workflow workers as the API process,
// without listening on HTTP or starting unrelated product workers.
func (s *Server) RunExecutor(ctx context.Context) error {
	if s.teamRunWorkers == nil {
		return errors.New("workflow executor dependencies are unavailable")
	}
	if s.modelActivityBuffer != nil {
		defer s.modelActivityBuffer.Close()
	}
	s.teamRunWorkers.Start()
	defer s.teamRunWorkers.Stop()
	slog.Info("workflow executor ready")
	if s.workflowFanoutReconciler != nil {
		s.workflowFanoutReconciler.Start()
		defer s.workflowFanoutReconciler.Stop()
	}
	<-ctx.Done()
	return nil
}
