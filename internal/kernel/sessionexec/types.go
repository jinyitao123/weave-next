package sessionexec

import (
	"encoding/json"
	"fmt"
	"time"
)

type LeaseState string

const (
	LeaseStateActive LeaseState = "active"
	LeaseStateParked LeaseState = "parked"
	LeaseStateClosed LeaseState = "closed"
)

type ControllerKind string

const (
	ControllerLead   ControllerKind = "lead_avatar"
	ControllerWorker ControllerKind = "worker"
)

type YieldKind string

const (
	YieldNone  YieldKind = "none"
	YieldInput YieldKind = "await_input"
)

type CloseReason string

const (
	CloseFinalCommitted CloseReason = "final_committed"
	CloseExpired        CloseReason = "expired"
	CloseFailed         CloseReason = "failed"
)

type AcquireDisposition string

const (
	AcquireGranted       AcquireDisposition = "granted"
	AcquireReplay        AcquireDisposition = "replay"
	AcquireRouteToParked AcquireDisposition = "route_to_parked"
	AcquireBusy          AcquireDisposition = "busy"
)

type DeliveryState string

const (
	DeliveryPending    DeliveryState = "pending"
	DeliveryDelivering DeliveryState = "delivering"
	DeliveryDelivered  DeliveryState = "delivered"
)

type SessionKey struct {
	WorkspaceID  string
	UserID       string
	LeadAvatarID string
	SessionID    string
}

type SessionExecutionLease struct {
	Key             SessionKey
	ProjectID       string
	LeaseEpoch      int64
	State           LeaseState
	ActiveRunID     string
	RunSnapshotID   string
	ControllerKind  ControllerKind
	ControllerID    string
	YieldKind       YieldKind
	ResumeTokenHash []byte
	YieldGeneration int64
	InputSchema     json.RawMessage
	ExpiresAt       time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ClosedAt        *time.Time
	CloseReason     *CloseReason
}

type AcquireRequest struct {
	Key            SessionKey
	ProjectID      string
	EventID        string
	ActiveRunID    string
	RunSnapshotID  string
	ControllerKind ControllerKind
	ControllerID   string
	TTL            time.Duration
}

type AcquireResult struct {
	Disposition AcquireDisposition
	Lease       SessionExecutionLease
}

type ParkRequest struct {
	Key             SessionKey
	LeaseEpoch      int64
	ActiveRunID     string
	YieldKind       YieldKind
	ResumeTokenHash []byte
	YieldGeneration int64
	InputSchema     json.RawMessage
	ExpiresAt       time.Time
}

type ResumeRequest struct {
	Key             SessionKey
	LeaseEpoch      int64
	ActiveRunID     string
	YieldKind       YieldKind
	ResumeToken     string
	YieldGeneration int64
	Input           json.RawMessage
}

type ResumeResult struct {
	Lease          SessionExecutionLease
	ValidatedInput json.RawMessage
}

type TransferControllerRequest struct {
	Key                    SessionKey
	LeaseEpoch             int64
	ActiveRunID            string
	ExpectedControllerKind ControllerKind
	ExpectedControllerID   string
	NewControllerKind      ControllerKind
	NewControllerID        string
	ExpiresAt              time.Time
}

type RenewRequest struct {
	Key         SessionKey
	LeaseEpoch  int64
	ActiveRunID string
	ExpiresAt   time.Time
}

type FailRequest struct {
	Key           SessionKey
	LeaseEpoch    int64
	ActiveRunID   string
	RunSnapshotID string
}

type FinalOutboxMessage struct {
	EventID       string
	Key           SessionKey
	LeaseEpoch    int64
	ActiveRunID   string
	RunSnapshotID string
	Role          string
	Content       string
	Metadata      json.RawMessage
}

type ExpireResult struct {
	Key         SessionKey
	OldEpoch    int64
	FencedEpoch int64
	ActiveRunID string
	Changed     bool
}

type OutboxRecord struct {
	Key              SessionKey
	ProjectID        string
	EventID          string
	LeaseEpoch       int64
	ActiveRunID      string
	RunSnapshotID    string
	Role             string
	Content          string
	Metadata         json.RawMessage
	DeliveryState    DeliveryState
	DeliveryAttempts int32
	ClaimOwner       *string
	ClaimExpiresAt   *time.Time
	CreatedAt        time.Time
	DeliveredAt      *time.Time
	LastError        *string
}

type SessionAuditEvent struct {
	ID              string
	Key             SessionKey
	ObservedEpoch   int64
	CurrentEpoch    int64
	ActiveRunID     string
	EventKind       string
	IsolationReason *string
	PayloadDigest   []byte
	Detail          json.RawMessage
	CreatedAt       time.Time
}

type IsolatedSessionAuditEvent struct {
	ID              string
	Key             SessionKey
	ObservedEpoch   int64
	CurrentEpoch    int64
	ActiveRunID     string
	EventKind       string
	IsolationReason string
	PayloadDigest   []byte
	Detail          json.RawMessage
	CreatedAt       time.Time
}

func ValidateSessionKey(key SessionKey) error {
	for field, value := range map[string]string{
		"workspace_id":   key.WorkspaceID,
		"user_id":        key.UserID,
		"lead_avatar_id": key.LeadAvatarID,
		"session_id":     key.SessionID,
	} {
		if value == "" {
			return fmt.Errorf("%s must be non-empty", field)
		}
	}
	return nil
}

func ValidateSessionExecutionLease(lease SessionExecutionLease) error {
	if err := ValidateSessionKey(lease.Key); err != nil {
		return err
	}
	if lease.LeaseEpoch < 1 {
		return fmt.Errorf("lease_epoch must be positive")
	}
	if err := validateEnum("state", lease.State, LeaseStateActive, LeaseStateParked, LeaseStateClosed); err != nil {
		return err
	}
	if lease.ActiveRunID == "" || lease.RunSnapshotID == "" {
		return fmt.Errorf("run identity must be non-empty")
	}
	if err := validateEnum("controller_kind", lease.ControllerKind, ControllerLead, ControllerWorker); err != nil {
		return err
	}
	if lease.ControllerID == "" {
		return fmt.Errorf("controller_id must be non-empty")
	}
	if err := validateEnum("yield_kind", lease.YieldKind, YieldNone, YieldInput); err != nil {
		return err
	}
	parkedShape := lease.YieldKind != YieldNone && len(lease.ResumeTokenHash) > 0 && lease.YieldGeneration > 0 && len(lease.InputSchema) > 0 && json.Valid(lease.InputSchema)
	if (lease.State == LeaseStateParked) != parkedShape {
		return fmt.Errorf("parked lease shape is invalid")
	}
	closedShape := lease.ClosedAt != nil && lease.CloseReason != nil
	if (lease.State == LeaseStateClosed) != closedShape {
		return fmt.Errorf("closed lease shape is invalid")
	}
	if lease.CloseReason != nil {
		if err := validateEnum("close_reason", *lease.CloseReason, CloseFinalCommitted, CloseExpired, CloseFailed); err != nil {
			return err
		}
	}
	if lease.ExpiresAt.IsZero() || lease.CreatedAt.IsZero() || lease.UpdatedAt.IsZero() {
		return fmt.Errorf("lease timestamps must be non-zero")
	}
	return nil
}

func validateEnum[T ~string](field string, value T, allowed ...T) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("%s %q is invalid", field, value)
}

func validateJSON(field string, value json.RawMessage) error {
	if len(value) == 0 || !json.Valid(value) {
		return fmt.Errorf("%s must be valid JSON", field)
	}
	return nil
}
