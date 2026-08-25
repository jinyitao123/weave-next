package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrA4AttemptLeaseHeartbeatUnsupported = errors.New(
		"A4 attempt lease heartbeat is unsupported",
	)
	ErrAttemptLeaseHeartbeatOwnerLost = errors.New(
		"attempt lease heartbeat owner lost",
	)
	ErrAttemptLeaseHeartbeatTTLExceeded = errors.New(
		"attempt lease heartbeat TTL exceeded",
	)
)

type AttemptLeaseHeartbeatStage string

const (
	AttemptLeaseHeartbeatValidate  AttemptLeaseHeartbeatStage = "validate"
	AttemptLeaseHeartbeatBegin     AttemptLeaseHeartbeatStage = "begin"
	AttemptLeaseHeartbeatRenew     AttemptLeaseHeartbeatStage = "renew"
	AttemptLeaseHeartbeatCommit    AttemptLeaseHeartbeatStage = "commit"
	AttemptLeaseHeartbeatOwnerLost AttemptLeaseHeartbeatStage = "owner_lost"
	AttemptLeaseHeartbeatExpired   AttemptLeaseHeartbeatStage = "expired"
)

type AttemptLeaseHeartbeatError struct {
	Stage       AttemptLeaseHeartbeatStage
	WorkspaceID string
	RunID       string
	Generation  int64
	AttemptID   uuid.UUID
	Err         error
}

func (err *AttemptLeaseHeartbeatError) Error() string {
	if err == nil {
		return "attempt lease heartbeat failed"
	}
	if err.Err == nil {
		return fmt.Sprintf(
			"attempt lease heartbeat failed at %s for workspace %q run %q generation %d attempt %s",
			err.Stage,
			err.WorkspaceID,
			err.RunID,
			err.Generation,
			err.AttemptID,
		)
	}
	return fmt.Sprintf(
		"attempt lease heartbeat failed at %s for workspace %q run %q generation %d attempt %s: %v",
		err.Stage,
		err.WorkspaceID,
		err.RunID,
		err.Generation,
		err.AttemptID,
		err.Err,
	)
}

func (err *AttemptLeaseHeartbeatError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

type AttemptLeaseRenewal struct {
	HeartbeatAt    time.Time
	LeaseExpiresAt time.Time
}

type AttemptLeaseHeartbeat interface {
	A4AttemptLeaseHeartbeatGuaranteed() bool
	RenewAttempt(
		context.Context,
		AttemptLeaseOwner,
		time.Duration,
	) (AttemptLeaseRenewal, error)
}

const (
	defaultAttemptHeartbeatInterval = 30 * time.Second
	defaultAttemptHeartbeatTTL      = 120 * time.Second
)

type attemptHeartbeatTicker interface {
	C() <-chan time.Time
	Stop()
}

type attemptHeartbeatTimer interface {
	C() <-chan time.Time
	Stop()
	Reset(time.Duration)
}

type attemptHeartbeatClock interface {
	Now() time.Time
	NewTicker(time.Duration) attemptHeartbeatTicker
	NewTimer(time.Duration) attemptHeartbeatTimer
}

type attemptHeartbeatConfig struct {
	Interval time.Duration
	TTL      time.Duration
	Clock    attemptHeartbeatClock
}

func defaultAttemptHeartbeatConfig() attemptHeartbeatConfig {
	return attemptHeartbeatConfig{
		Interval: defaultAttemptHeartbeatInterval,
		TTL:      defaultAttemptHeartbeatTTL,
		Clock:    realAttemptHeartbeatClock{},
	}
}

func validateAttemptHeartbeatConfig(config attemptHeartbeatConfig) error {
	if config.Clock == nil || config.Interval <= 0 ||
		config.TTL != defaultAttemptHeartbeatTTL ||
		config.Interval > config.TTL/3 {
		return fmt.Errorf("heartbeat configuration is invalid")
	}
	return nil
}

type realAttemptHeartbeatClock struct{}

func (realAttemptHeartbeatClock) Now() time.Time { return time.Now() }

func (realAttemptHeartbeatClock) NewTicker(duration time.Duration) attemptHeartbeatTicker {
	return realAttemptHeartbeatTicker{Ticker: time.NewTicker(duration)}
}

func (realAttemptHeartbeatClock) NewTimer(duration time.Duration) attemptHeartbeatTimer {
	return &realAttemptHeartbeatTimer{timer: time.NewTimer(duration)}
}

type realAttemptHeartbeatTicker struct{ *time.Ticker }

func (ticker realAttemptHeartbeatTicker) C() <-chan time.Time { return ticker.Ticker.C }

type realAttemptHeartbeatTimer struct{ timer *time.Timer }

func (timer *realAttemptHeartbeatTimer) C() <-chan time.Time { return timer.timer.C }

func (timer *realAttemptHeartbeatTimer) Stop() {
	if !timer.timer.Stop() {
		select {
		case <-timer.timer.C:
		default:
		}
	}
}

func (timer *realAttemptHeartbeatTimer) Reset(duration time.Duration) {
	timer.Stop()
	timer.timer.Reset(duration)
}

type attemptHeartbeatScope struct {
	graphCtx      context.Context
	cancelGraph   context.CancelCauseFunc
	stopHeartbeat context.CancelFunc
	ready         chan struct{}
	done          chan struct{}
	stopOnce      sync.Once
	errMu         sync.Mutex
	err           error
}

func startAttemptHeartbeat(
	parent context.Context,
	heartbeat AttemptLeaseHeartbeat,
	lease RunAttemptLease,
	admissionAnchor time.Time,
	config attemptHeartbeatConfig,
) (*attemptHeartbeatScope, error) {
	owner := attemptLeaseOwner(lease)
	fail := func(err error) (*attemptHeartbeatScope, error) {
		return nil, &AttemptLeaseHeartbeatError{
			Stage:       AttemptLeaseHeartbeatValidate,
			WorkspaceID: owner.WorkspaceID,
			RunID:       owner.RunID,
			Generation:  owner.Generation,
			AttemptID:   owner.AttemptID,
			Err:         err,
		}
	}
	if parent == nil {
		return fail(fmt.Errorf("parent context must be non-nil"))
	}
	if heartbeat == nil || !heartbeat.A4AttemptLeaseHeartbeatGuaranteed() {
		return fail(ErrA4AttemptLeaseHeartbeatUnsupported)
	}
	if err := validateAttemptHeartbeatConfig(config); err != nil {
		return fail(err)
	}
	if admissionAnchor.IsZero() {
		return fail(fmt.Errorf("admission monotonic anchor must be non-zero"))
	}
	if err := ValidateRunAttemptLease(lease); err != nil {
		return fail(err)
	}
	if lease.State != AttemptLeaseActive {
		return fail(fmt.Errorf("attempt lease must be active"))
	}
	window := lease.LeaseExpiresAt.Sub(lease.HeartbeatAt)
	if window != config.TTL {
		return fail(fmt.Errorf("committed lease window must equal heartbeat TTL"))
	}
	deadline := admissionAnchor.Add(window)
	if !config.Clock.Now().Before(deadline) {
		return nil, attemptHeartbeatTTLFailure(owner, nil)
	}

	graphCtx, cancelGraph := context.WithCancelCause(parent)
	heartbeatCtx, stopHeartbeat := context.WithCancel(parent)
	scope := &attemptHeartbeatScope{
		graphCtx:      graphCtx,
		cancelGraph:   cancelGraph,
		stopHeartbeat: stopHeartbeat,
		ready:         make(chan struct{}),
		done:          make(chan struct{}),
	}
	go scope.run(heartbeatCtx, heartbeat, owner, config, deadline)
	<-scope.ready
	return scope, nil
}

func (scope *attemptHeartbeatScope) Context() context.Context {
	if scope == nil {
		return nil
	}
	return scope.graphCtx
}

func (scope *attemptHeartbeatScope) Stop() error {
	if scope == nil {
		return nil
	}
	scope.stopOnce.Do(scope.stopHeartbeat)
	<-scope.done
	scope.cancelGraph(nil)
	scope.errMu.Lock()
	defer scope.errMu.Unlock()
	return scope.err
}

func (scope *attemptHeartbeatScope) run(
	ctx context.Context,
	heartbeat AttemptLeaseHeartbeat,
	owner AttemptLeaseOwner,
	config attemptHeartbeatConfig,
	deadline time.Time,
) {
	defer close(scope.done)
	ticker := config.Clock.NewTicker(config.Interval)
	defer ticker.Stop()
	deadlineTimer := config.Clock.NewTimer(durationUntil(config.Clock.Now(), deadline))
	defer deadlineTimer.Stop()
	close(scope.ready)
	var lastRenewalErr error

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-deadlineTimer.C():
			if ctx.Err() != nil {
				return
			}
			scope.lose(attemptHeartbeatTTLFailure(owner, lastRenewalErr))
			return
		case <-ticker.C():
			callAnchor := config.Clock.Now()
			if !callAnchor.Before(deadline) {
				scope.lose(attemptHeartbeatTTLFailure(owner, lastRenewalErr))
				return
			}
			ttlFailure := attemptHeartbeatTTLFailure(owner, lastRenewalErr)
			renewal, err, stopped, expired := runAttemptHeartbeatRenewal(
				ctx,
				deadlineTimer,
				heartbeat,
				owner,
				config.TTL,
				func() { scope.lose(ttlFailure) },
			)
			if stopped {
				return
			}
			if expired {
				return
			}
			if errors.Is(err, ErrAttemptLeaseHeartbeatOwnerLost) {
				var typed *AttemptLeaseHeartbeatError
				if errors.As(err, &typed) {
					scope.lose(err)
				} else {
					scope.lose(&AttemptLeaseHeartbeatError{
						Stage:       AttemptLeaseHeartbeatOwnerLost,
						WorkspaceID: owner.WorkspaceID,
						RunID:       owner.RunID,
						Generation:  owner.Generation,
						AttemptID:   owner.AttemptID,
						Err:         ErrAttemptLeaseHeartbeatOwnerLost,
					})
				}
				return
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				lastRenewalErr = err
				continue
			}
			window := renewal.LeaseExpiresAt.Sub(renewal.HeartbeatAt)
			if renewal.HeartbeatAt.IsZero() || window != config.TTL {
				lastRenewalErr = fmt.Errorf("committed heartbeat renewal window is invalid")
				continue
			}
			deadline = callAnchor.Add(window)
			now := config.Clock.Now()
			if !now.Before(deadline) {
				scope.lose(attemptHeartbeatTTLFailure(owner, lastRenewalErr))
				return
			}
			lastRenewalErr = nil
			deadlineTimer.Reset(durationUntil(now, deadline))
		}
	}
}

type attemptHeartbeatRenewalResult struct {
	renewal AttemptLeaseRenewal
	err     error
}

func runAttemptHeartbeatRenewal(
	ctx context.Context,
	deadlineTimer attemptHeartbeatTimer,
	heartbeat AttemptLeaseHeartbeat,
	owner AttemptLeaseOwner,
	ttl time.Duration,
	onExpired func(),
) (AttemptLeaseRenewal, error, bool, bool) {
	renewCtx, cancelRenew := context.WithCancel(ctx)
	resultCh := make(chan attemptHeartbeatRenewalResult, 1)
	go func() {
		renewal, err := heartbeat.RenewAttempt(renewCtx, owner, ttl)
		resultCh <- attemptHeartbeatRenewalResult{renewal: renewal, err: err}
	}()
	select {
	case <-ctx.Done():
		cancelRenew()
		<-resultCh
		return AttemptLeaseRenewal{}, nil, true, false
	case <-deadlineTimer.C():
		cancelRenew()
		if onExpired != nil {
			onExpired()
		}
		<-resultCh
		if ctx.Err() != nil {
			return AttemptLeaseRenewal{}, nil, true, false
		}
		return AttemptLeaseRenewal{}, nil, false, true
	case result := <-resultCh:
		cancelRenew()
		return result.renewal, result.err, false, false
	}
}

func (scope *attemptHeartbeatScope) lose(err error) {
	scope.cancelGraph(err)
	cause := attemptHeartbeatLossCause(scope.graphCtx)
	if cause == nil {
		return
	}
	scope.errMu.Lock()
	if scope.err == nil {
		scope.err = cause
	}
	scope.errMu.Unlock()
}

func attemptHeartbeatTTLFailure(owner AttemptLeaseOwner, lastErr error) error {
	cause := error(ErrAttemptLeaseHeartbeatTTLExceeded)
	if lastErr != nil {
		cause = errors.Join(cause, lastErr)
	}
	return &AttemptLeaseHeartbeatError{
		Stage:       AttemptLeaseHeartbeatExpired,
		WorkspaceID: owner.WorkspaceID,
		RunID:       owner.RunID,
		Generation:  owner.Generation,
		AttemptID:   owner.AttemptID,
		Err:         cause,
	}
}

func durationUntil(now, deadline time.Time) time.Duration {
	duration := deadline.Sub(now)
	if duration < 0 {
		return 0
	}
	return duration
}

func attemptHeartbeatLossCause(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrAttemptLeaseHeartbeatOwnerLost) ||
		errors.Is(cause, ErrAttemptLeaseHeartbeatTTLExceeded) {
		return cause
	}
	return nil
}
