// Package schedule persists durable daily and one-time schedules.
package schedule

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	KindDaily = "daily"
	KindOnce  = "once"

	TargetAgent        = "agent"
	TargetTeamWorkflow = "team_workflow"

	OccurrencePending   = "pending"
	OccurrenceCommitted = "committed"
)

var (
	ErrInvalidSchedule      = errors.New("invalid schedule")
	ErrScheduleNotFound     = errors.New("schedule not found")
	ErrScheduleNotDue       = errors.New("schedule is not due")
	ErrOccurrenceNotFound   = errors.New("schedule occurrence not found")
	ErrOccurrenceNotPending = errors.New("schedule occurrence is not pending")
)

// Clock supplies timestamps for schedule persistence.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// Schedule is one durable agent or team-workflow schedule.
type Schedule struct {
	ID               string     `json:"id"`
	WorkspaceID      string     `json:"workspace_id"`
	TargetKind       string     `json:"target_kind"`
	Agent            string     `json:"agent,omitempty"`
	Message          string     `json:"message,omitempty"`
	TargetWorkflowID string     `json:"target_workflow_id,omitempty"`
	Kind             string     `json:"kind"`
	TimeOfDay        string     `json:"time_of_day"`
	RunAt            *time.Time `json:"run_at,omitempty"`
	Timezone         string     `json:"timezone"`
	Enabled          bool       `json:"enabled"`
	LastRunDate      string     `json:"last_run_date"`
	LastRunAt        *time.Time `json:"last_run_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// UpsertSchedule contains only caller-writable schedule fields. Cursor state is
// deliberately absent and can only be changed through AdvanceCursorTx.
type UpsertSchedule struct {
	ID               string
	TargetKind       string
	Agent            string
	Message          string
	TargetWorkflowID string
	Kind             string
	TimeOfDay        string
	RunAt            *time.Time
	Timezone         string
	Enabled          bool
}

// Occurrence is the immutable idempotency record for one workflow trigger.
type Occurrence struct {
	WorkspaceID      string
	OccurrenceKey    string
	ScheduleID       string
	TargetWorkflowID string
	ScheduledFor     time.Time
	Status           string
	WorkflowVersion  int
	RunSnapshotID    string
	TaskID           string
	CreatedAt        time.Time
}

// DueOccurrence identifies the logical local date and exact UTC instant that
// one sweep is allowed to admit.
type DueOccurrence struct {
	ScheduledFor time.Time
	LocalDate    string
}

// Store persists schedules in PostgreSQL.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a schedule store with an injected clock.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// Upsert creates or replaces caller-writable fields within one workspace.
func (s *Store) Upsert(
	ctx context.Context,
	workspaceID string,
	input UpsertSchedule,
) (Schedule, error) {
	normalized, err := normalizeUpsert(input)
	if err != nil {
		return Schedule{}, err
	}
	if normalized.ID == "" {
		normalized.ID = "schedule-" + uuid.NewString()
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Schedule{}, fmt.Errorf("begin upsert schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ensureWorkspace(ctx, tx, workspaceID, now); err != nil {
		return Schedule{}, err
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO weave_schedule (
			id, workspace_id, target_kind, agent, message, target_workflow_id,
			kind, time_of_day, run_at, timezone, enabled, created_at, updated_at
		) VALUES (
			$2, $1, $3, NULLIF($4, ''), $5, NULLIF($6, ''),
			$7, $8, $9, $10, $11, $12, $12
		)
		ON CONFLICT (workspace_id, id) DO UPDATE SET
			target_kind=EXCLUDED.target_kind,
			agent=EXCLUDED.agent,
			message=EXCLUDED.message,
			target_workflow_id=EXCLUDED.target_workflow_id,
			kind=EXCLUDED.kind,
			time_of_day=EXCLUDED.time_of_day,
			run_at=EXCLUDED.run_at,
			timezone=EXCLUDED.timezone,
			enabled=EXCLUDED.enabled,
			updated_at=EXCLUDED.updated_at
		RETURNING `+scheduleColumns,
		workspaceID, normalized.ID, normalized.TargetKind, normalized.Agent,
		nullableMessage(normalized), normalized.TargetWorkflowID, normalized.Kind,
		normalized.TimeOfDay, normalized.RunAt, normalized.Timezone,
		normalized.Enabled, now,
	)
	stored, err := scanSchedule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, fmt.Errorf("%w: %q", ErrScheduleNotFound, normalized.ID)
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("upsert schedule: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Schedule{}, fmt.Errorf("commit upsert schedule: %w", err)
	}
	return stored, nil
}

// ResolveScheduleTx locks and returns one exact workspace schedule. The caller
// owns the transaction and is responsible for commit or rollback.
func (s *Store) ResolveScheduleTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, scheduleID string,
) (Schedule, error) {
	item, err := scanSchedule(tx.QueryRow(ctx, `
		SELECT `+scheduleColumns+`
		FROM weave_schedule
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, scheduleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, fmt.Errorf("%w: %q", ErrScheduleNotFound, scheduleID)
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("resolve schedule: %w", err)
	}
	return item, nil
}

func nullableMessage(input UpsertSchedule) any {
	if input.TargetKind == TargetTeamWorkflow {
		return nil
	}
	return input.Message
}

func normalizeUpsert(input UpsertSchedule) (UpsertSchedule, error) {
	if input.TargetKind == "" {
		input.TargetKind = TargetAgent
	}
	switch input.TargetKind {
	case TargetAgent:
		if input.Agent == "" || input.TargetWorkflowID != "" {
			return UpsertSchedule{}, invalidSchedulef("agent target requires agent and forbids target_workflow_id")
		}
	case TargetTeamWorkflow:
		if input.TargetWorkflowID == "" || input.Agent != "" || input.Message != "" {
			return UpsertSchedule{}, invalidSchedulef("team_workflow target requires only target_workflow_id")
		}
	default:
		return UpsertSchedule{}, invalidSchedulef("target_kind must be agent or team_workflow")
	}
	switch input.Kind {
	case KindDaily:
		if !validTimeOfDay(input.TimeOfDay) {
			return UpsertSchedule{}, invalidSchedulef("time_of_day must be HH:MM (00:00-23:59)")
		}
		if input.RunAt != nil {
			return UpsertSchedule{}, invalidSchedulef("daily schedule forbids run_at")
		}
		if input.Timezone == "" {
			return UpsertSchedule{}, invalidSchedulef("timezone is required for daily schedules")
		}
	case KindOnce:
		if input.RunAt == nil {
			return UpsertSchedule{}, invalidSchedulef("run_at is required for once schedules")
		}
		input.TimeOfDay = ""
		if input.Timezone == "" {
			input.Timezone = "UTC"
		}
	default:
		return UpsertSchedule{}, invalidSchedulef("kind must be daily or once")
	}
	if _, err := loadTimezone(input.Timezone); err != nil {
		return UpsertSchedule{}, err
	}
	return input, nil
}

func validTimeOfDay(value string) bool {
	if len(value) != len("00:00") {
		return false
	}
	_, err := time.Parse("15:04", value)
	return err == nil
}

func loadTimezone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, invalidSchedulef("timezone must be a valid IANA timezone")
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, invalidSchedulef("timezone must be a valid IANA timezone: %v", err)
	}
	return location, nil
}

func invalidSchedulef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSchedule, fmt.Sprintf(format, args...))
}

// List returns schedules owned by one workspace.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+scheduleColumns+`
		FROM weave_schedule
		WHERE workspace_id=$1
		ORDER BY created_at, id
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	return collectSchedules(rows)
}

// Delete removes an exact workspace schedule.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM weave_schedule
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q", ErrScheduleNotFound, id)
	}
	return nil
}

// DueSchedules scans enabled schedules and evaluates each in its persisted timezone.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+scheduleColumns+`
		FROM weave_schedule
		WHERE enabled
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list schedules for due evaluation: %w", err)
	}
	defer rows.Close()
	items, err := collectSchedules(rows)
	if err != nil {
		return nil, err
	}
	due := make([]Schedule, 0, len(items))
	for _, item := range items {
		isDue, err := scheduleDue(item, now)
		if err != nil {
			return nil, fmt.Errorf("evaluate schedule %q: %w", item.ID, err)
		}
		if isDue {
			due = append(due, item)
		}
	}
	return due, nil
}

// LockDueTx locks one exact schedule and returns ErrScheduleNotDue when its
// persisted rule is not due at now.
func (s *Store) LockDueTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, scheduleID string,
	now time.Time,
) (*Schedule, error) {
	item, err := scanSchedule(tx.QueryRow(ctx, `
		SELECT `+scheduleColumns+`
		FROM weave_schedule
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, scheduleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("schedule %q not found", scheduleID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock schedule: %w", err)
	}
	due, err := scheduleDue(item, now)
	if err != nil {
		return nil, err
	}
	if !due {
		return nil, ErrScheduleNotDue
	}
	return &item, nil
}

func scheduleDue(item Schedule, now time.Time) (bool, error) {
	due, err := DueScheduledFor(item, now)
	return due != nil, err
}

// DueScheduledFor resolves the one logical occurrence due at sweepInstant.
// local-date gaps may map only the immediately preceding logical date onto the
// current local date; ordinary missed days are never backfilled here.
func DueScheduledFor(item Schedule, sweepInstant time.Time) (*DueOccurrence, error) {
	return dueScheduledForWithProbe(item, sweepInstant, probeTimezone)
}

type timezoneProbe func(time.Time, *time.Location) time.Time

func probeTimezone(instant time.Time, location *time.Location) time.Time {
	return instant.In(location)
}

func dueScheduledForWithProbe(
	item Schedule,
	sweepInstant time.Time,
	probe timezoneProbe,
) (*DueOccurrence, error) {
	if !item.Enabled {
		return nil, nil
	}
	switch item.Kind {
	case KindDaily:
		location, err := loadTimezone(item.Timezone)
		if err != nil {
			return nil, err
		}
		localNow := probe(sweepInstant, location)
		year, month, day := localNow.Date()
		logicalDate := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
		localDate := logicalDate.Format("2006-01-02")
		current, err := scheduledForWithProbe(item, logicalDate, probe)
		if err != nil {
			return nil, err
		}
		if !sweepInstant.Before(current) {
			if item.LastRunDate == localDate {
				return nil, nil
			}
			return &DueOccurrence{ScheduledFor: current, LocalDate: localDate}, nil
		}
		previousDate := logicalDate.AddDate(0, 0, -1)
		previousLocalDate := previousDate.Format("2006-01-02")
		previous, err := scheduledForWithProbe(item, previousDate, probe)
		if err != nil {
			return nil, err
		}
		if !sweepInstant.Before(previous) &&
			probe(previous, location).Format("2006-01-02") == localDate &&
			item.LastRunDate != previousLocalDate {
			return &DueOccurrence{
				ScheduledFor: previous,
				LocalDate:    previousLocalDate,
			}, nil
		}
		return nil, nil
	case KindOnce:
		if item.RunAt == nil {
			return nil, fmt.Errorf("once schedule requires run_at")
		}
		if sweepInstant.Before(*item.RunAt) || item.LastRunAt != nil {
			return nil, nil
		}
		return &DueOccurrence{ScheduledFor: item.RunAt.UTC()}, nil
	default:
		return nil, fmt.Errorf("unsupported schedule kind %q", item.Kind)
	}
}

// MarkRan is the compatibility wrapper for agent schedules. It locks the due
// row and advances its cursor in one self-managed transaction.
func (s *Store) MarkRan(ctx context.Context, workspaceID, id string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark schedule ran: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	item, err := s.LockDueTx(ctx, tx, workspaceID, id, now)
	if err != nil {
		return err
	}
	due, err := DueScheduledFor(*item, now)
	if err != nil {
		return err
	}
	if due == nil {
		return ErrScheduleNotDue
	}
	if err := s.AdvanceCursorTx(ctx, tx, *item, due.ScheduledFor); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark schedule ran: %w", err)
	}
	return nil
}

// AdvanceCursorTx records the logical occurrence instant while the caller owns
// the row lock and transaction.
func (s *Store) AdvanceCursorTx(
	ctx context.Context,
	tx pgx.Tx,
	item Schedule,
	scheduledFor time.Time,
) error {
	canonical, err := scanSchedule(tx.QueryRow(ctx, `
		SELECT `+scheduleColumns+`
		FROM weave_schedule
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, item.WorkspaceID, item.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("schedule %q not found", item.ID)
	}
	if err != nil {
		return fmt.Errorf("lock schedule for cursor advance: %w", err)
	}
	if !sameLockedSchedule(canonical, item) {
		return fmt.Errorf("schedule %q changed since it was locked", item.ID)
	}
	dueOccurrence, err := DueScheduledFor(canonical, scheduledFor)
	if err != nil {
		return err
	}
	if dueOccurrence == nil || !dueOccurrence.ScheduledFor.Equal(scheduledFor) {
		return fmt.Errorf("scheduled_for does not match locked schedule")
	}
	var lastRunDate string
	enabled := canonical.Enabled
	switch canonical.Kind {
	case KindDaily:
		lastRunDate = dueOccurrence.LocalDate
	case KindOnce:
		enabled = false
	default:
		return fmt.Errorf("unsupported schedule kind %q", canonical.Kind)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weave_schedule
		SET last_run_date=$3, last_run_at=$4, enabled=$5, updated_at=$6
		WHERE workspace_id=$1 AND id=$2
	`, canonical.WorkspaceID, canonical.ID, lastRunDate, scheduledFor.UTC(), enabled, s.clock.Now())
	if err != nil {
		return fmt.Errorf("advance schedule cursor: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("schedule %q not found", item.ID)
	}
	return nil
}

func sameLockedSchedule(left, right Schedule) bool {
	return left.ID == right.ID &&
		left.WorkspaceID == right.WorkspaceID &&
		left.TargetKind == right.TargetKind &&
		left.Agent == right.Agent &&
		left.Message == right.Message &&
		left.TargetWorkflowID == right.TargetWorkflowID &&
		left.Kind == right.Kind &&
		left.TimeOfDay == right.TimeOfDay &&
		equalOptionalTime(left.RunAt, right.RunAt) &&
		left.Timezone == right.Timezone &&
		left.Enabled == right.Enabled &&
		left.LastRunDate == right.LastRunDate &&
		equalOptionalTime(left.LastRunAt, right.LastRunAt) &&
		left.CreatedAt.Equal(right.CreatedAt) &&
		left.UpdatedAt.Equal(right.UpdatedAt)
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

// ScheduledFor returns a schedule's logical UTC occurrence for one local date.
func ScheduledFor(item Schedule, localDate time.Time) (time.Time, error) {
	return scheduledForWithProbe(item, localDate, probeTimezone)
}

func scheduledForWithProbe(
	item Schedule,
	localDate time.Time,
	probe timezoneProbe,
) (time.Time, error) {
	if item.Kind == KindOnce {
		if item.RunAt == nil {
			return time.Time{}, fmt.Errorf("once schedule requires run_at")
		}
		return item.RunAt.UTC(), nil
	}
	if item.Kind != KindDaily || !validTimeOfDay(item.TimeOfDay) {
		return time.Time{}, fmt.Errorf("daily schedule requires valid time_of_day")
	}
	location, err := loadTimezone(item.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	hour := int((item.TimeOfDay[0]-'0')*10 + item.TimeOfDay[1] - '0')
	minute := int((item.TimeOfDay[3]-'0')*10 + item.TimeOfDay[4] - '0')
	year, month, day := localDate.Date()
	wallUTC := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	offsets := make(map[int]struct{})
	for delta := -48 * time.Hour; delta <= 48*time.Hour; delta += 6 * time.Hour {
		_, offset := probe(wallUTC.Add(delta), location).Zone()
		offsets[offset] = struct{}{}
	}
	var exact []time.Time
	wantWallKey := wallTimeKey(year, month, day, hour, minute)
	var windowStart, windowEnd time.Time
	for offset := range offsets {
		candidate := wallUTC.Add(-time.Duration(offset) * time.Second)
		if windowStart.IsZero() || candidate.Before(windowStart) {
			windowStart = candidate
		}
		if windowEnd.IsZero() || candidate.After(windowEnd) {
			windowEnd = candidate
		}
		local := probe(candidate, location)
		ly, lm, ld := local.Date()
		wallKey := wallTimeKey(ly, lm, ld, local.Hour(), local.Minute())
		if wallKey == wantWallKey && local.Second() == 0 {
			exact = append(exact, candidate.UTC())
		}
	}
	if len(exact) > 0 {
		earliest := exact[0]
		for _, candidate := range exact[1:] {
			if candidate.Before(earliest) {
				earliest = candidate
			}
		}
		return earliest, nil
	}
	var gapCandidate time.Time
	var gapWallKey int64
	for instant := windowStart; !instant.After(windowEnd); instant = instant.Add(time.Minute) {
		local := probe(instant, location)
		ly, lm, ld := local.Date()
		if local.Second() != 0 {
			continue
		}
		wallKey := wallTimeKey(ly, lm, ld, local.Hour(), local.Minute())
		if wallKey > wantWallKey && (gapCandidate.IsZero() || wallKey < gapWallKey ||
			(wallKey == gapWallKey && instant.Before(gapCandidate))) {
			gapWallKey = wallKey
			gapCandidate = instant.UTC()
		}
	}
	if !gapCandidate.IsZero() {
		return gapCandidate, nil
	}
	return time.Time{}, fmt.Errorf("cannot resolve local schedule instant")
}

func wallTimeKey(year int, month time.Month, day, hour, minute int) int64 {
	key := int64(year)
	key = key*13 + int64(month)
	key = key*32 + int64(day)
	key = key*24 + int64(hour)
	return key*60 + int64(minute)
}

// OccurrenceKey hashes the fixed-nine UTC logical occurrence identity.
func OccurrenceKey(item Schedule, scheduledFor time.Time) string {
	fixedUTC := scheduledFor.UTC().Format("2006-01-02T15:04:05.000000000Z")
	material := strings.Join([]string{
		item.WorkspaceID,
		item.ID,
		fixedUTC,
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("%x", sum)
}

// InsertOccurrenceTx inserts a pending occurrence or returns the existing row
// after an idempotency conflict. The caller owns the transaction.
func (s *Store) InsertOccurrenceTx(
	ctx context.Context,
	tx pgx.Tx,
	occurrence *Occurrence,
) (*Occurrence, bool, error) {
	if occurrence == nil {
		return nil, false, fmt.Errorf("occurrence is required")
	}
	status := occurrence.Status
	if status == "" {
		status = OccurrencePending
	}
	if status != OccurrencePending || occurrence.WorkflowVersion != 0 ||
		occurrence.RunSnapshotID != "" || occurrence.TaskID != "" {
		return nil, false, fmt.Errorf("occurrence must be inserted pending")
	}
	existing, err := readExistingOccurrenceTx(ctx, tx, occurrence)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrOccurrenceNotFound) {
		return nil, false, err
	}
	canonical, err := scanSchedule(tx.QueryRow(ctx, `
		SELECT `+scheduleColumns+`
		FROM weave_schedule
		WHERE workspace_id=$1 AND id=$2
		FOR KEY SHARE
	`, occurrence.WorkspaceID, occurrence.ScheduleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("schedule %q not found", occurrence.ScheduleID)
	}
	if err != nil {
		return nil, false, fmt.Errorf("read occurrence schedule: %w", err)
	}
	if canonical.TargetKind != TargetTeamWorkflow ||
		canonical.TargetWorkflowID != occurrence.TargetWorkflowID {
		return nil, false, fmt.Errorf("occurrence target does not match workflow schedule")
	}
	if want := OccurrenceKey(canonical, occurrence.ScheduledFor); occurrence.OccurrenceKey != want {
		return nil, false, fmt.Errorf("occurrence_key does not match schedule and scheduled_for")
	}
	dueOccurrence, err := DueScheduledFor(canonical, occurrence.ScheduledFor)
	if err != nil {
		return nil, false, err
	}
	if dueOccurrence == nil || !dueOccurrence.ScheduledFor.Equal(occurrence.ScheduledFor) {
		existing, readErr := readExistingOccurrenceTx(ctx, tx, occurrence)
		if readErr == nil {
			return existing, false, nil
		}
		if !errors.Is(readErr, ErrOccurrenceNotFound) {
			return nil, false, readErr
		}
		return nil, false, fmt.Errorf("occurrence scheduled_for does not match schedule")
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO weave_schedule_occurrences (
			workspace_id, occurrence_key, schedule_id, target_workflow_id,
			scheduled_for, status
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (workspace_id, occurrence_key) DO NOTHING
		RETURNING `+occurrenceColumns,
		occurrence.WorkspaceID, occurrence.OccurrenceKey, occurrence.ScheduleID,
		occurrence.TargetWorkflowID, occurrence.ScheduledFor.UTC(), status,
	)
	inserted, err := scanOccurrence(row)
	if err == nil {
		return &inserted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("insert schedule occurrence: %w", err)
	}
	existing, err = readExistingOccurrenceTx(ctx, tx, occurrence)
	return existing, false, err
}

func readExistingOccurrenceTx(
	ctx context.Context,
	tx pgx.Tx,
	occurrence *Occurrence,
) (*Occurrence, error) {
	existing, err := scanOccurrence(tx.QueryRow(ctx, `
		SELECT `+occurrenceColumns+`
		FROM weave_schedule_occurrences
		WHERE workspace_id=$1 AND occurrence_key=$2
	`, occurrence.WorkspaceID, occurrence.OccurrenceKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOccurrenceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read existing schedule occurrence: %w", err)
	}
	if existing.WorkspaceID != occurrence.WorkspaceID ||
		existing.OccurrenceKey != occurrence.OccurrenceKey ||
		existing.ScheduleID != occurrence.ScheduleID ||
		existing.TargetWorkflowID != occurrence.TargetWorkflowID ||
		!existing.ScheduledFor.Equal(occurrence.ScheduledFor) {
		return nil, fmt.Errorf("existing occurrence frozen fields do not match")
	}
	return &existing, nil
}

// CommitOccurrenceTx performs the only permitted pending-to-committed update.
func (s *Store) CommitOccurrenceTx(
	ctx context.Context,
	tx pgx.Tx,
	occurrence Occurrence,
) error {
	if occurrence.WorkflowVersion < 1 || occurrence.RunSnapshotID == "" || occurrence.TaskID == "" {
		return fmt.Errorf("committed occurrence requires workflow version, snapshot, and task")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weave_schedule_occurrences
		SET status='committed', workflow_version=$3, run_snapshot_id=$4, task_id=$5
		WHERE workspace_id=$1 AND occurrence_key=$2 AND status='pending'
	`, occurrence.WorkspaceID, occurrence.OccurrenceKey, occurrence.WorkflowVersion,
		occurrence.RunSnapshotID, occurrence.TaskID)
	if err != nil {
		return fmt.Errorf("commit schedule occurrence: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOccurrenceNotPending
	}
	return nil
}

func ensureWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name, created_at)
		VALUES ($1, $1, $1, $2)
		ON CONFLICT DO NOTHING
	`, workspaceID, now); err != nil {
		return fmt.Errorf("ensure schedule workspace: %w", err)
	}
	return nil
}

const scheduleColumns = `
	id, workspace_id, target_kind, agent, message, target_workflow_id,
	kind, time_of_day, run_at, timezone, enabled,
	last_run_date, last_run_at, created_at, updated_at`

const occurrenceColumns = `
	workspace_id, occurrence_key, schedule_id, target_workflow_id,
	scheduled_for, status, workflow_version, run_snapshot_id, task_id, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSchedule(row rowScanner) (Schedule, error) {
	var item Schedule
	var agent, message, targetWorkflowID *string
	err := row.Scan(
		&item.ID, &item.WorkspaceID, &item.TargetKind, &agent, &message,
		&targetWorkflowID, &item.Kind, &item.TimeOfDay, &item.RunAt,
		&item.Timezone, &item.Enabled, &item.LastRunDate,
		&item.LastRunAt, &item.CreatedAt, &item.UpdatedAt,
	)
	if agent != nil {
		item.Agent = *agent
	}
	if message != nil {
		item.Message = *message
	}
	if targetWorkflowID != nil {
		item.TargetWorkflowID = *targetWorkflowID
	}
	return item, err
}

func scanOccurrence(row rowScanner) (Occurrence, error) {
	var item Occurrence
	var workflowVersion *int
	var runSnapshotID, taskID *string
	err := row.Scan(
		&item.WorkspaceID, &item.OccurrenceKey, &item.ScheduleID,
		&item.TargetWorkflowID, &item.ScheduledFor, &item.Status,
		&workflowVersion, &runSnapshotID, &taskID, &item.CreatedAt,
	)
	if workflowVersion != nil {
		item.WorkflowVersion = *workflowVersion
	}
	if runSnapshotID != nil {
		item.RunSnapshotID = *runSnapshotID
	}
	if taskID != nil {
		item.TaskID = *taskID
	}
	return item, err
}

func collectSchedules(rows pgx.Rows) ([]Schedule, error) {
	items := make([]Schedule, 0)
	for rows.Next() {
		item, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedules: %w", err)
	}
	return items, nil
}
