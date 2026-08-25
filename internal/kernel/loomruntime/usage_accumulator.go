package loomruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

const (
	usageAccumulatorStateKey  = "__usage_accumulator"
	usageAccumulatorSchema    = 1
	maxUsageCheckpointInteger = uint64(1<<53 - 1)
	usageCallIDPrefix         = "uc1_"
	usageCallIdentityDomain   = "weave-usage-call-v1"
)

// ErrUsageConflict reports a non-idempotent attempt to change confirmed usage
// or reuse an identifier for a different logical owner.
var ErrUsageConflict = errors.New("usage accumulator conflict")

// UsageTotals is the sum of confirmed logical-call usage in an accumulator.
type UsageTotals struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

type usageCall struct {
	RunID       string
	Step        string
	CallOrdinal uint64
	AttemptIDs  map[string]struct{}
	Confirmed   bool
	Usage       contract.Usage
}

type UsageAccumulator struct {
	nextCallOrdinals map[string]uint64
	calls            map[string]usageCall
	attemptOwners    map[string]string
}

// NewUsageAccumulator returns an empty run-level usage accumulator.
func NewUsageAccumulator() UsageAccumulator {
	return UsageAccumulator{
		nextCallOrdinals: make(map[string]uint64),
		calls:            make(map[string]usageCall),
		attemptOwners:    make(map[string]string),
	}
}

func (a UsageAccumulator) clone() UsageAccumulator {
	cloned := NewUsageAccumulator()
	for step, ordinal := range a.nextCallOrdinals {
		cloned.nextCallOrdinals[step] = ordinal
	}
	for callID, call := range a.calls {
		copied := call
		copied.AttemptIDs = make(map[string]struct{}, len(call.AttemptIDs))
		for attemptID := range call.AttemptIDs {
			copied.AttemptIDs[attemptID] = struct{}{}
		}
		cloned.calls[callID] = copied
	}
	for attemptID, callID := range a.attemptOwners {
		cloned.attemptOwners[attemptID] = callID
	}
	return cloned
}

func normalizedUsageAccumulator(a UsageAccumulator) UsageAccumulator {
	if a.nextCallOrdinals == nil && a.calls == nil && a.attemptOwners == nil {
		return NewUsageAccumulator()
	}
	return a
}

func (a UsageAccumulator) ownedRunID() string {
	for _, call := range a.calls {
		return call.RunID
	}
	return ""
}

// OwnedRunID returns the run id owning every logical call, or "" for an empty
// accumulator. Callers use it to verify a restored accumulator belongs to the
// same run before replay.
func (a UsageAccumulator) OwnedRunID() string {
	return a.ownedRunID()
}

func usageCallID(runID, step string, ordinal uint64) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(usageCallIdentityDomain))
	writeUsageIdentityPart(hasher, []byte(runID))
	writeUsageIdentityPart(hasher, []byte(step))
	var encodedOrdinal [8]byte
	binary.BigEndian.PutUint64(encodedOrdinal[:], ordinal)
	_, _ = hasher.Write(encodedOrdinal[:])
	return usageCallIDPrefix + hex.EncodeToString(hasher.Sum(nil))
}

type usageIdentityWriter interface {
	Write([]byte) (int, error)
}

func writeUsageIdentityPart(writer usageIdentityWriter, value []byte) {
	var encodedLength [8]byte
	binary.BigEndian.PutUint64(encodedLength[:], uint64(len(value)))
	_, _ = writer.Write(encodedLength[:])
	_, _ = writer.Write(value)
}

// NextCall allocates the next deterministic logical-call ID for a run step.
func (a *UsageAccumulator) NextCall(runID, step string) (string, error) {
	if a == nil {
		return "", fmt.Errorf("usage accumulator is nil")
	}
	if runID == "" {
		return "", fmt.Errorf("usage run_id is required")
	}
	if step == "" {
		return "", fmt.Errorf("usage step is required")
	}

	current := normalizedUsageAccumulator(*a)
	if ownedRunID := current.ownedRunID(); ownedRunID != "" && ownedRunID != runID {
		return "", fmt.Errorf(
			"%w: accumulator belongs to run_id %q, not %q",
			ErrUsageConflict,
			ownedRunID,
			runID,
		)
	}
	next := current.nextCallOrdinals[step]
	if next >= maxUsageCheckpointInteger {
		return "", fmt.Errorf("usage call ordinal for step %q exceeds checkpoint integer range", step)
	}
	callID := usageCallID(runID, step, next)
	if _, exists := current.calls[callID]; exists {
		return "", fmt.Errorf("%w: usage_call_id %q already exists", ErrUsageConflict, callID)
	}

	updated := current.clone()
	updated.calls[callID] = usageCall{
		RunID:       runID,
		Step:        step,
		CallOrdinal: next,
		AttemptIDs:  make(map[string]struct{}),
	}
	updated.nextCallOrdinals[step] = next + 1
	*a = updated
	return callID, nil
}

// StartAttempt associates one physical request attempt with a logical call.
func (a *UsageAccumulator) StartAttempt(callID, attemptID string) error {
	if a == nil {
		return fmt.Errorf("usage accumulator is nil")
	}
	if callID == "" {
		return fmt.Errorf("usage_call_id is required")
	}
	if attemptID == "" {
		return fmt.Errorf("attempt_id is required")
	}
	current := normalizedUsageAccumulator(*a)
	if _, exists := current.calls[callID]; !exists {
		return fmt.Errorf("unknown usage_call_id %q", callID)
	}
	if owner, exists := current.attemptOwners[attemptID]; exists {
		if owner == callID {
			return nil
		}
		return fmt.Errorf("%w: attempt_id %q belongs to %q", ErrUsageConflict, attemptID, owner)
	}

	updated := current.clone()
	call := updated.calls[callID]
	call.AttemptIDs[attemptID] = struct{}{}
	updated.calls[callID] = call
	updated.attemptOwners[attemptID] = callID
	*a = updated
	return nil
}

// ConfirmAttempt records the first confirmed response for a logical call.
func (a *UsageAccumulator) ConfirmAttempt(callID, attemptID string, usage contract.Usage) error {
	if a == nil {
		return fmt.Errorf("usage accumulator is nil")
	}
	if callID == "" {
		return fmt.Errorf("usage_call_id is required")
	}
	if attemptID == "" {
		return fmt.Errorf("attempt_id is required")
	}
	if err := validateUsage(usage); err != nil {
		return err
	}
	current := normalizedUsageAccumulator(*a)
	call, exists := current.calls[callID]
	if !exists {
		return fmt.Errorf("unknown usage_call_id %q", callID)
	}
	if owner, exists := current.attemptOwners[attemptID]; !exists || owner != callID {
		return fmt.Errorf("attempt_id %q is not registered for usage_call_id %q", attemptID, callID)
	}
	if call.Confirmed {
		if call.Usage == usage {
			return nil
		}
		return fmt.Errorf("%w: usage_call_id %q was already confirmed", ErrUsageConflict, callID)
	}
	if _, err := current.totalsWith(usage); err != nil {
		return err
	}

	updated := current.clone()
	call = updated.calls[callID]
	call.Confirmed = true
	call.Usage = usage
	updated.calls[callID] = call
	*a = updated
	return nil
}

func validateUsage(usage contract.Usage) error {
	if usage.InputTokens < 0 {
		return fmt.Errorf("usage input_tokens must be non-negative")
	}
	if usage.OutputTokens < 0 {
		return fmt.Errorf("usage output_tokens must be non-negative")
	}
	if usage.CostUSD < 0 || math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) {
		return fmt.Errorf("usage cost_usd must be finite and non-negative")
	}
	return nil
}

func (a UsageAccumulator) totalsWith(extra contract.Usage) (UsageTotals, error) {
	totals, err := a.validatedTotals()
	if err != nil {
		return UsageTotals{}, err
	}
	return addUsageTotals(totals, extra)
}

func safeAddInt(left, right int) (int, bool) {
	if right > 0 && left > int(^uint(0)>>1)-right {
		return 0, false
	}
	return left + right, true
}

func (a UsageAccumulator) validatedTotals() (UsageTotals, error) {
	a = normalizedUsageAccumulator(a)
	callIDs := make([]string, 0, len(a.calls))
	for callID := range a.calls {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	var totals UsageTotals
	for _, callID := range callIDs {
		call := a.calls[callID]
		if !call.Confirmed {
			continue
		}
		next, err := addUsageTotals(totals, call.Usage)
		if err != nil {
			return UsageTotals{}, fmt.Errorf("usage_call_id %q: %w", callID, err)
		}
		totals = next
	}
	return totals, nil
}

func addUsageTotals(current UsageTotals, usage contract.Usage) (UsageTotals, error) {
	if err := validateUsage(usage); err != nil {
		return UsageTotals{}, err
	}
	input, ok := safeAddInt(current.InputTokens, usage.InputTokens)
	if !ok {
		return UsageTotals{}, fmt.Errorf("input_tokens total overflows int")
	}
	output, ok := safeAddInt(current.OutputTokens, usage.OutputTokens)
	if !ok {
		return UsageTotals{}, fmt.Errorf("output_tokens total overflows int")
	}
	cost := current.CostUSD + usage.CostUSD
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return UsageTotals{}, fmt.Errorf("cost_usd total is not finite")
	}
	return UsageTotals{InputTokens: input, OutputTokens: output, CostUSD: cost}, nil
}

// Totals returns the sum of all confirmed logical calls.
func (a UsageAccumulator) Totals() UsageTotals {
	totals, err := a.validatedTotals()
	if err != nil {
		panic("loomruntime: invalid UsageAccumulator invariant: " + err.Error())
	}
	return totals
}

type usageAccumulatorCheckpoint struct {
	SchemaVersion    int                   `json:"schema_version"`
	NextCallOrdinals map[string]uint64     `json:"next_call_ordinals"`
	Calls            []usageCallCheckpoint `json:"calls"`
}

type usageCallCheckpoint struct {
	UsageCallID  string   `json:"usage_call_id"`
	RunID        string   `json:"run_id"`
	Step         string   `json:"step"`
	CallOrdinal  uint64   `json:"call_ordinal"`
	AttemptIDs   []string `json:"attempt_ids"`
	Confirmed    bool     `json:"confirmed"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	CostUSD      float64  `json:"cost_usd"`
}

type usageAccumulatorCheckpointInput struct {
	SchemaVersion    *int                        `json:"schema_version"`
	NextCallOrdinals *map[string]uint64          `json:"next_call_ordinals"`
	Calls            *[]usageCallCheckpointInput `json:"calls"`
}

type usageCallCheckpointInput struct {
	UsageCallID  *string   `json:"usage_call_id"`
	RunID        *string   `json:"run_id"`
	Step         *string   `json:"step"`
	CallOrdinal  *uint64   `json:"call_ordinal"`
	AttemptIDs   *[]string `json:"attempt_ids"`
	Confirmed    *bool     `json:"confirmed"`
	InputTokens  *int      `json:"input_tokens"`
	OutputTokens *int      `json:"output_tokens"`
	CostUSD      *float64  `json:"cost_usd"`
}

func (input usageCallCheckpointInput) value(index int) (usageCallCheckpoint, error) {
	missing := func(field string) (usageCallCheckpoint, error) {
		return usageCallCheckpoint{}, fmt.Errorf("usage accumulator calls[%d].%s is required", index, field)
	}
	if input.UsageCallID == nil {
		return missing("usage_call_id")
	}
	if input.RunID == nil {
		return missing("run_id")
	}
	if input.Step == nil {
		return missing("step")
	}
	if input.CallOrdinal == nil {
		return missing("call_ordinal")
	}
	if input.AttemptIDs == nil {
		return missing("attempt_ids")
	}
	if input.Confirmed == nil {
		return missing("confirmed")
	}
	if input.InputTokens == nil {
		return missing("input_tokens")
	}
	if input.OutputTokens == nil {
		return missing("output_tokens")
	}
	if input.CostUSD == nil {
		return missing("cost_usd")
	}
	return usageCallCheckpoint{
		UsageCallID:  *input.UsageCallID,
		RunID:        *input.RunID,
		Step:         *input.Step,
		CallOrdinal:  *input.CallOrdinal,
		AttemptIDs:   *input.AttemptIDs,
		Confirmed:    *input.Confirmed,
		InputTokens:  *input.InputTokens,
		OutputTokens: *input.OutputTokens,
		CostUSD:      *input.CostUSD,
	}, nil
}

func (a UsageAccumulator) checkpoint() (usageAccumulatorCheckpoint, error) {
	a = normalizedUsageAccumulator(a)
	if err := a.validate(); err != nil {
		return usageAccumulatorCheckpoint{}, err
	}
	checkpoint := usageAccumulatorCheckpoint{
		SchemaVersion:    usageAccumulatorSchema,
		NextCallOrdinals: make(map[string]uint64, len(a.nextCallOrdinals)),
		Calls:            make([]usageCallCheckpoint, 0, len(a.calls)),
	}
	for step, ordinal := range a.nextCallOrdinals {
		checkpoint.NextCallOrdinals[step] = ordinal
	}
	callIDs := make([]string, 0, len(a.calls))
	for callID := range a.calls {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	for _, callID := range callIDs {
		call := a.calls[callID]
		attemptIDs := make([]string, 0, len(call.AttemptIDs))
		for attemptID := range call.AttemptIDs {
			attemptIDs = append(attemptIDs, attemptID)
		}
		sort.Strings(attemptIDs)
		checkpoint.Calls = append(checkpoint.Calls, usageCallCheckpoint{
			UsageCallID:  callID,
			RunID:        call.RunID,
			Step:         call.Step,
			CallOrdinal:  call.CallOrdinal,
			AttemptIDs:   attemptIDs,
			Confirmed:    call.Confirmed,
			InputTokens:  call.Usage.InputTokens,
			OutputTokens: call.Usage.OutputTokens,
			CostUSD:      call.Usage.CostUSD,
		})
	}
	return checkpoint, nil
}

// MarshalCheckpoint encodes the accumulator using its versioned checkpoint format.
func (a UsageAccumulator) MarshalCheckpoint() ([]byte, error) {
	checkpoint, err := a.checkpoint()
	if err != nil {
		return nil, err
	}
	return json.Marshal(checkpoint)
}

// UnmarshalUsageAccumulator decodes and validates a checkpointed accumulator.
func UnmarshalUsageAccumulator(data []byte) (UsageAccumulator, error) {
	var input usageAccumulatorCheckpointInput
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return UsageAccumulator{}, fmt.Errorf("decode usage accumulator checkpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return UsageAccumulator{}, fmt.Errorf("decode usage accumulator checkpoint: trailing JSON content")
	}
	if input.SchemaVersion == nil {
		return UsageAccumulator{}, fmt.Errorf("usage accumulator schema_version is required")
	}
	if *input.SchemaVersion != usageAccumulatorSchema {
		return UsageAccumulator{}, fmt.Errorf(
			"unsupported usage accumulator schema_version %d",
			*input.SchemaVersion,
		)
	}
	if input.NextCallOrdinals == nil {
		return UsageAccumulator{}, fmt.Errorf("usage accumulator next_call_ordinals are required")
	}
	if input.Calls == nil {
		return UsageAccumulator{}, fmt.Errorf("usage accumulator calls are required")
	}

	accumulator := NewUsageAccumulator()
	for step, ordinal := range *input.NextCallOrdinals {
		if step == "" || ordinal > maxUsageCheckpointInteger {
			return UsageAccumulator{}, fmt.Errorf("invalid next_call_ordinals[%q]", step)
		}
		accumulator.nextCallOrdinals[step] = ordinal
	}
	for index, rawCall := range *input.Calls {
		encoded, err := rawCall.value(index)
		if err != nil {
			return UsageAccumulator{}, err
		}
		if _, duplicate := accumulator.calls[encoded.UsageCallID]; duplicate {
			return UsageAccumulator{}, fmt.Errorf("duplicate usage_call_id %q", encoded.UsageCallID)
		}
		attempts := make(map[string]struct{}, len(encoded.AttemptIDs))
		for _, attemptID := range encoded.AttemptIDs {
			if attemptID == "" {
				return UsageAccumulator{}, fmt.Errorf("empty attempt_id for %q", encoded.UsageCallID)
			}
			if _, duplicate := attempts[attemptID]; duplicate {
				return UsageAccumulator{}, fmt.Errorf("duplicate attempt_id %q", attemptID)
			}
			if owner, duplicate := accumulator.attemptOwners[attemptID]; duplicate {
				return UsageAccumulator{}, fmt.Errorf(
					"%w: attempt_id %q belongs to %q",
					ErrUsageConflict,
					attemptID,
					owner,
				)
			}
			attempts[attemptID] = struct{}{}
			accumulator.attemptOwners[attemptID] = encoded.UsageCallID
		}
		usage := contract.Usage{
			InputTokens:  encoded.InputTokens,
			OutputTokens: encoded.OutputTokens,
			CostUSD:      encoded.CostUSD,
		}
		accumulator.calls[encoded.UsageCallID] = usageCall{
			RunID:       encoded.RunID,
			Step:        encoded.Step,
			CallOrdinal: encoded.CallOrdinal,
			AttemptIDs:  attempts,
			Confirmed:   encoded.Confirmed,
			Usage:       usage,
		}
	}
	if err := accumulator.validate(); err != nil {
		return UsageAccumulator{}, err
	}
	return accumulator.clone(), nil
}

func (a UsageAccumulator) validate() error {
	if a.nextCallOrdinals == nil || a.calls == nil || a.attemptOwners == nil {
		return fmt.Errorf("usage accumulator maps are required")
	}
	for step, next := range a.nextCallOrdinals {
		if step == "" {
			return fmt.Errorf("next_call_ordinals contains an empty step")
		}
		if next > maxUsageCheckpointInteger {
			return fmt.Errorf("next_call_ordinals[%q] exceeds checkpoint range", step)
		}
	}
	runID := ""
	for callID, call := range a.calls {
		if call.RunID == "" || call.Step == "" {
			return fmt.Errorf("usage_call_id %q has empty identity", callID)
		}
		if runID == "" {
			runID = call.RunID
		} else if call.RunID != runID {
			return fmt.Errorf(
				"%w: usage_call_id %q belongs to run_id %q, not %q",
				ErrUsageConflict,
				callID,
				call.RunID,
				runID,
			)
		}
		if call.CallOrdinal >= maxUsageCheckpointInteger {
			return fmt.Errorf("usage_call_id %q ordinal exceeds checkpoint range", callID)
		}
		if want := usageCallID(call.RunID, call.Step, call.CallOrdinal); callID != want {
			return fmt.Errorf("usage_call_id %q does not match logical identity", callID)
		}
		next, ok := a.nextCallOrdinals[call.Step]
		if !ok || next <= call.CallOrdinal {
			return fmt.Errorf(
				"next_call_ordinals[%q] does not advance past call %q",
				call.Step,
				callID,
			)
		}
		if !call.Confirmed && call.Usage != (contract.Usage{}) {
			return fmt.Errorf("unconfirmed usage_call_id %q carries usage", callID)
		}
		if call.Confirmed && len(call.AttemptIDs) == 0 {
			return fmt.Errorf("confirmed usage_call_id %q has no attempt", callID)
		}
		if err := validateUsage(call.Usage); err != nil {
			return fmt.Errorf("usage_call_id %q: %w", callID, err)
		}
		for attemptID := range call.AttemptIDs {
			if attemptID == "" {
				return fmt.Errorf("usage_call_id %q has empty attempt_id", callID)
			}
			if owner := a.attemptOwners[attemptID]; owner != callID {
				return fmt.Errorf("attempt_id %q owner mismatch", attemptID)
			}
		}
	}
	for attemptID, callID := range a.attemptOwners {
		call, exists := a.calls[callID]
		if attemptID == "" || !exists {
			return fmt.Errorf("attempt_id %q has invalid owner %q", attemptID, callID)
		}
		if _, exists := call.AttemptIDs[attemptID]; !exists {
			return fmt.Errorf("attempt_id %q is absent from owner %q", attemptID, callID)
		}
	}
	_, err := a.validatedTotals()
	return err
}

// StoreUsageAccumulator deep-copies an accumulator into Loom state.
func StoreUsageAccumulator(state loom.State, accumulator UsageAccumulator) error {
	if state == nil {
		return fmt.Errorf("usage accumulator state is nil")
	}
	data, err := accumulator.MarshalCheckpoint()
	if err != nil {
		return err
	}
	var stored any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&stored); err != nil {
		return fmt.Errorf("copy usage accumulator into state: %w", err)
	}
	state[usageAccumulatorStateKey] = stored
	return nil
}

// LoadUsageAccumulator loads a deep copy from Loom state.
func LoadUsageAccumulator(state loom.State) (UsageAccumulator, error) {
	if state == nil {
		return NewUsageAccumulator(), nil
	}
	raw, exists := state[usageAccumulatorStateKey]
	if !exists || raw == nil {
		return NewUsageAccumulator(), nil
	}
	if typed, ok := raw.(UsageAccumulator); ok {
		data, err := typed.MarshalCheckpoint()
		if err != nil {
			return UsageAccumulator{}, err
		}
		return UnmarshalUsageAccumulator(data)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return UsageAccumulator{}, fmt.Errorf("encode usage accumulator state: %w", err)
	}
	return UnmarshalUsageAccumulator(data)
}

func initializeUsageAccumulator(state loom.State) {
	if err := StoreUsageAccumulator(state, NewUsageAccumulator()); err != nil {
		panic("loomruntime: initialize usage accumulator: " + err.Error())
	}
}

func clearUsageAccumulator(state loom.State) {
	delete(state, usageAccumulatorStateKey)
}
