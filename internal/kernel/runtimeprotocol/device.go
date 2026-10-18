package runtimeprotocol

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
)

// A device runtime is the person's own machine (weave-workbench decision
// 009). It claims control sessions instead of engine work, takes the
// session's operations one at a time and hands back their results.

// DeviceFacts is what a device reports about itself in hello.
type DeviceFacts struct {
	Platform  string `json:"platform"`
	OSVersion string `json:"os_version,omitempty"`
	// Driver is the cua-driver command line; Available is false with a reason
	// when it cannot run, lacks a system permission or the screen is locked.
	DriverPath        string   `json:"driver_path,omitempty"`
	DriverVersion     string   `json:"driver_version,omitempty"`
	Available         bool     `json:"available"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
	Levels            []string `json:"levels"`
	FileRoots         []string `json:"file_roots,omitempty"`
}

// DeviceSessionClaim is handed to a device in place of an ExecutionClaim.
type DeviceSessionClaim struct {
	TaskID         string            `json:"task_id"`
	ClaimEpoch     int64             `json:"claim_epoch"`
	Subject        execution.Subject `json:"subject"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at"`
	Reason         string            `json:"reason"`
	OpenedBy       string            `json:"opened_by"`
}

// DeviceNextRequest asks for the session's next operation, waiting up to
// WaitSeconds when there is none.
type DeviceNextRequest struct {
	Versioned
	ClaimEpoch  int64 `json:"claim_epoch"`
	WaitSeconds int   `json:"wait_seconds"`
}

// DeviceOperation is one operation for the device to perform.
type DeviceOperation struct {
	OpID   string          `json:"op_id"`
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params"`
}

// DeviceNextResponse carries the next operation, or none; Ended tells the
// device the session is over and it should stop.
type DeviceNextResponse struct {
	Versioned
	Operation *DeviceOperation `json:"operation,omitempty"`
	Ended     bool             `json:"ended,omitempty"`
}

// DeviceOperationResult hands one operation's outcome back.
type DeviceOperationResult struct {
	Versioned
	ClaimEpoch int64           `json:"claim_epoch"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	// Audit holds content-free facts about the outcome: an exit code and
	// output lengths, a file's digest, never the content itself.
	Audit json.RawMessage `json:"audit,omitempty"`
}

// DeviceSessionEnd ends a session from the device side.
type DeviceSessionEnd struct {
	Versioned
	ClaimEpoch int64  `json:"claim_epoch"`
	Reason     string `json:"reason"`
}

var ErrDeviceResultStatus = errors.New("device result status must be succeeded or failed")

func (r DeviceOperationResult) Validate() error {
	if err := r.Versioned.Validate(); err != nil {
		return err
	}
	if r.Status != "succeeded" && r.Status != "failed" {
		return ErrDeviceResultStatus
	}
	return nil
}
