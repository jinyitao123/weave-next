package teamorch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

type roundDriver interface {
	Run(ctx context.Context, workspaceID, buildRunID string) (Result, error)
}

type passedCandidatePublisher interface {
	PublishStep(ctx context.Context, workspaceID, buildRunID string) error
}

// Service is the platform-facing execution boundary for one authorized team
// build run. The controller owns build/evaluate state transitions; when it
// reaches publishing, Service releases the exact evaluated candidate and
// completes the control record in the same request.
type Service struct {
	driver    roundDriver
	publisher passedCandidatePublisher
}

func NewService(driver roundDriver, publisher passedCandidatePublisher) *Service {
	return &Service{driver: driver, publisher: publisher}
}

func (s *Service) Execute(
	ctx context.Context,
	workspaceID, buildRunID string,
) (Result, error) {
	if s == nil || s.driver == nil || s.publisher == nil {
		return Result{}, errors.New("team build orchestration service unavailable")
	}
	result, err := s.driver.Run(ctx, workspaceID, buildRunID)
	if err != nil {
		return Result{}, fmt.Errorf("execute team build run: %w", err)
	}
	if result.Status != teambuild.StatusPublishing {
		return result, nil
	}
	if err := s.publisher.PublishStep(ctx, workspaceID, buildRunID); err != nil {
		if errors.Is(err, teambuild.ErrEvaluationBaselineChanged) ||
			errors.Is(err, teambuild.ErrEvaluationPublishCAS) {
			result.Status = teambuild.StatusBlocked
			result.StopReason = "evaluation_baseline_cas_failed"
			return result, nil
		}
		return Result{}, fmt.Errorf("publish evaluated team build candidate: %w", err)
	}
	result.Status = teambuild.StatusPassed
	return result, nil
}
