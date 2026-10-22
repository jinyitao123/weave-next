package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RecordBatch writes observations in one statement and one transaction. Stable
// event IDs make a retry safe after a connection is lost during commit.
func (store *PGActivityStore) RecordBatch(ctx context.Context, events []ActivityEvent) error {
	if len(events) == 0 {
		return nil
	}
	if store == nil || store.Transactions == nil {
		return errors.New("activity store unavailable")
	}
	for i := range events {
		e := &events[i]
		if e.WorkspaceID == "" || e.RunID == "" || e.Kind == "" || e.MemberVersion < 0 || e.OccurredAt.IsZero() {
			return errors.New("invalid activity event")
		}
		if e.EventID == "" {
			e.EventID = uuid.NewString()
		}
		if len(e.Detail) == 0 {
			e.Detail = json.RawMessage(`{}`)
		}
		var detail map[string]json.RawMessage
		if len(e.Detail) > ActivityDetailMaxBytes || json.Unmarshal(e.Detail, &detail) != nil || detail == nil {
			return errors.New("invalid activity detail")
		}
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return err
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO weave_team_run_activity_events
		(workspace_id,run_id,event_id,kind,node_id,member_id,member_version,detail,occurred_at)
		SELECT workspace_id,run_id,event_id,kind,NULLIF(node_id,''),NULLIF(member_id,''),NULLIF(member_version,0),detail,occurred_at
		FROM jsonb_to_recordset($1::jsonb) AS event(workspace_id text,run_id text,event_id text,kind text,node_id text,
		 member_id text,member_version bigint,detail jsonb,occurred_at timestamptz)
		ON CONFLICT(workspace_id,run_id,event_id) DO NOTHING`, string(raw))
	if err != nil {
		return fmt.Errorf("record activity batch: %w", err)
	}
	return tx.Commit(ctx)
}

type activityWrite struct {
	event   *ActivityEvent
	barrier chan error
}
type BufferedActivityRecorder struct {
	gapsMu     sync.Mutex
	gaps       map[string]activityGap
	admission  sync.RWMutex
	closed     bool
	store      *PGActivityStore
	queue      chan activityWrite
	stop, done chan struct{}
	closeOnce  sync.Once
}

type activityGap struct {
	event ActivityEvent
	count int
}

func (b *BufferedActivityRecorder) noteGap(event ActivityEvent) {
	b.gapsMu.Lock()
	defer b.gapsMu.Unlock()
	if b.gaps == nil {
		b.gaps = make(map[string]activityGap)
	}
	key := event.WorkspaceID + "\x00" + event.RunID
	gap := b.gaps[key]
	if gap.count == 0 {
		gap.event = event
	}
	gap.count++
	b.gaps[key] = gap
}
func (b *BufferedActivityRecorder) takeGaps() []ActivityEvent {
	b.gapsMu.Lock()
	defer b.gapsMu.Unlock()
	var events []ActivityEvent
	for key, gap := range b.gaps {
		event := gap.event
		event.Kind = "model_observation_gap"
		event.EventID = uuid.NewString()
		event.Detail, _ = json.Marshal(map[string]any{"dropped": gap.count, "reason": "observation_queue_deadline"})
		events = append(events, event)
		delete(b.gaps, key)
	}
	return events
}

func NewBufferedActivityRecorder(store *PGActivityStore) *BufferedActivityRecorder {
	b := &BufferedActivityRecorder{store: store, queue: make(chan activityWrite, 4096), stop: make(chan struct{}), done: make(chan struct{})}
	go b.run()
	return b
}

func (b *BufferedActivityRecorder) Record(ctx context.Context, event ActivityEvent) error {
	if !strings.HasPrefix(event.Kind, "model_") {
		if err := b.Flush(ctx); err != nil {
			return err
		}
		return b.store.Record(ctx, event)
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	// Record's caller owns its detail; an asynchronous writer owns a copy.
	event.Detail = append(json.RawMessage(nil), event.Detail...)
	return b.enqueue(ctx, activityWrite{event: &event})
}

// A barrier precedes every ordinary member/terminal activity, so observations
// cannot appear after the completion that acknowledges their execution.
func (b *BufferedActivityRecorder) Flush(ctx context.Context) error {
	ack := make(chan error, 1)
	if err := b.enqueue(ctx, activityWrite{barrier: ack}); err != nil {
		return err
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return errors.New("activity buffer closed")
	}
}

func (b *BufferedActivityRecorder) Close() {
	b.closeOnce.Do(func() { b.admission.Lock(); b.closed = true; close(b.stop); b.admission.Unlock(); <-b.done })
}

func (b *BufferedActivityRecorder) enqueue(ctx context.Context, command activityWrite) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b.admission.RLock()
	defer b.admission.RUnlock()
	if b.closed {
		return errors.New("activity buffer closed")
	}
	select {
	case b.queue <- command:
		return nil
	case <-ctx.Done():
		if command.event != nil {
			b.noteGap(*command.event)
		}
		return ctx.Err()
	}
}

func (b *BufferedActivityRecorder) run() {
	defer close(b.done)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	pending := make([]ActivityEvent, 0, 128)
	flush := func() error {
		pending = append(pending, b.takeGaps()...)
		if len(pending) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.store.RecordBatch(ctx, pending); err != nil {
			slog.Error("model observation batch remains uncommitted", "events", len(pending), "error", err)
			return err
		}
		pending = pending[:0]
		return nil
	}
	for {
		input := b.queue
		if len(pending) >= 256 {
			input = nil
		}
		select {
		case <-b.stop:
			for {
				select {
				case command := <-b.queue:
					if command.event != nil {
						pending = append(pending, *command.event)
					}
					if command.barrier != nil {
						command.barrier <- flush()
					}
				default:
					_ = flush()
					return
				}
			}
		case command := <-input:
			if command.event != nil {
				pending = append(pending, *command.event)
			}
			if command.barrier != nil {
				command.barrier <- flush()
			} else if len(pending) >= 128 {
				_ = flush()
			}
		case <-ticker.C:
			_ = flush()
		}
	}
}
