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
	usageAccumulatorSchema    = 2
	usageAccumulatorSchemaV1  = 1
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
	ToolCalls    int
}

// UsageCoverage preserves whether every physical attempt reported each
// budget dimension. Sources contains the sorted, distinct non-empty receipt
// sources represented by the confirmed attempts.
type UsageCoverage struct {
	HasTokens bool
	HasCost   bool
	Sources   []string
}

// UsageAttemptMetadata carries the lossless completeness/provenance fields
// that contract.Usage cannot represent.
type UsageAttemptMetadata struct {
	HasTokens bool
	HasCost   bool
	Source    string
}

type usageAttempt struct {
	Confirmed bool
	Usage     contract.Usage
	ToolCalls int
	Metadata  UsageAttemptMetadata
}

type usageCall struct {
	RunID       string
	Step        string
	CallOrdinal uint64
	Attempts    map[string]usageAttempt
}

type UsageAccumulator struct {
	nextCallOrdinals map[string]uint64
	calls            map[string]usageCall
	attemptOwners    map[string]string
}

type ConfirmedUsageReceipt struct {
	AttemptID string
	Usage     contract.Usage
	ToolCalls int
	Metadata  UsageAttemptMetadata
}

// ConfirmedReceipts preserves physical identities when a parent receives the
// same member's growing usage across recovery attempts.
func (a UsageAccumulator) ConfirmedReceipts() []ConfirmedUsageReceipt {
	var receipts []ConfirmedUsageReceipt
	for _, call := range a.calls {
		for id, attempt := range call.Attempts {
			if attempt.Confirmed {
				receipts = append(receipts, ConfirmedUsageReceipt{id, attempt.Usage, attempt.ToolCalls, attempt.Metadata})
			}
		}
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].AttemptID < receipts[j].AttemptID })
	return receipts
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
		copied.Attempts = make(map[string]usageAttempt, len(call.Attempts))
		for attemptID, attempt := range call.Attempts {
			copied.Attempts[attemptID] = attempt
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
func (a UsageAccumulator) CallOrdinal(callID string) (uint64, bool) {
	call, ok := a.calls[callID]
	return call.CallOrdinal, ok
}

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
		Attempts:    make(map[string]usageAttempt),
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
	call.Attempts[attemptID] = usageAttempt{}
	updated.calls[callID] = call
	updated.attemptOwners[attemptID] = callID
	*a = updated
	return nil
}

// ConfirmAttempt records provider-neutral usage for one physical attempt.
// The legacy contract has no completeness/provenance fields, so a successful
// response is treated as reporting both token and cost dimensions.
func (a *UsageAccumulator) ConfirmAttempt(
	callID, attemptID string,
	usage contract.Usage,
	toolCalls int,
) error {
	return a.ConfirmAttemptWithMetadata(callID, attemptID, usage, toolCalls, UsageAttemptMetadata{
		HasTokens: true,
		HasCost:   true,
	})
}

// ConfirmAttemptWithMetadata records one physical attempt. Replaying the same
// attempt with identical values is idempotent; distinct attempts under the
// same logical call each contribute their real spend.
func (a *UsageAccumulator) ConfirmAttemptWithMetadata(
	callID, attemptID string,
	usage contract.Usage,
	toolCalls int,
	metadata UsageAttemptMetadata,
) error {
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
	if toolCalls < 0 {
		return fmt.Errorf("usage tool_calls must be non-negative")
	}
	if !metadata.HasTokens && (usage.InputTokens != 0 || usage.OutputTokens != 0) {
		return fmt.Errorf("unreported token dimension must be zero-valued")
	}
	if !metadata.HasCost && usage.CostUSD != 0 {
		return fmt.Errorf("unreported cost dimension must be zero-valued")
	}
	current := normalizedUsageAccumulator(*a)
	call, exists := current.calls[callID]
	if !exists {
		return fmt.Errorf("unknown usage_call_id %q", callID)
	}
	if owner, exists := current.attemptOwners[attemptID]; !exists || owner != callID {
		return fmt.Errorf("attempt_id %q is not registered for usage_call_id %q", attemptID, callID)
	}
	attempt, registered := call.Attempts[attemptID]
	if !registered {
		return fmt.Errorf("attempt_id %q is not registered for usage_call_id %q", attemptID, callID)
	}
	if attempt.Confirmed {
		if attempt.Usage == usage && attempt.ToolCalls == toolCalls && attempt.Metadata == metadata {
			return nil
		}
		return fmt.Errorf("%w: attempt_id %q was already confirmed", ErrUsageConflict, attemptID)
	}
	if _, err := current.totalsWith(usage, toolCalls); err != nil {
		return err
	}

	updated := current.clone()
	call = updated.calls[callID]
	call.Attempts[attemptID] = usageAttempt{
		Confirmed: true,
		Usage:     usage,
		ToolCalls: toolCalls,
		Metadata:  metadata,
	}
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

func (a UsageAccumulator) totalsWith(extra contract.Usage, toolCalls int) (UsageTotals, error) {
	totals, err := a.validatedTotals()
	if err != nil {
		return UsageTotals{}, err
	}
	return addUsageTotals(totals, extra, toolCalls)
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
		attemptIDs := make([]string, 0, len(call.Attempts))
		for attemptID := range call.Attempts {
			attemptIDs = append(attemptIDs, attemptID)
		}
		sort.Strings(attemptIDs)
		for _, attemptID := range attemptIDs {
			attempt := call.Attempts[attemptID]
			if !attempt.Confirmed {
				continue
			}
			next, err := addUsageTotals(totals, attempt.Usage, attempt.ToolCalls)
			if err != nil {
				return UsageTotals{}, fmt.Errorf("usage_call_id %q attempt_id %q: %w", callID, attemptID, err)
			}
			totals = next
		}
	}
	return totals, nil
}

// Coverage returns dimension-level completeness across every registered
// physical attempt. An empty accumulator is complete because no usage-bearing
// request occurred; a started but unconfirmed attempt is incomplete.
func (a UsageAccumulator) Coverage() UsageCoverage {
	a = normalizedUsageAccumulator(a)
	coverage := UsageCoverage{HasTokens: true, HasCost: true}
	sources := make(map[string]struct{})
	for _, call := range a.calls {
		for _, attempt := range call.Attempts {
			if !attempt.Confirmed {
				coverage.HasTokens = false
				coverage.HasCost = false
				continue
			}
			coverage.HasTokens = coverage.HasTokens && attempt.Metadata.HasTokens
			coverage.HasCost = coverage.HasCost && attempt.Metadata.HasCost
			if attempt.Metadata.Source != "" {
				sources[attempt.Metadata.Source] = struct{}{}
			}
		}
	}
	coverage.Sources = make([]string, 0, len(sources))
	for source := range sources {
		coverage.Sources = append(coverage.Sources, source)
	}
	sort.Strings(coverage.Sources)
	return coverage
}

func addUsageTotals(current UsageTotals, usage contract.Usage, toolCalls int) (UsageTotals, error) {
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
	tools, ok := safeAddInt(current.ToolCalls, toolCalls)
	if !ok || toolCalls < 0 {
		return UsageTotals{}, fmt.Errorf("tool_calls total is invalid")
	}
	return UsageTotals{InputTokens: input, OutputTokens: output, CostUSD: cost, ToolCalls: tools}, nil
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
	UsageCallID string                   `json:"usage_call_id"`
	RunID       string                   `json:"run_id"`
	Step        string                   `json:"step"`
	CallOrdinal uint64                   `json:"call_ordinal"`
	Attempts    []usageAttemptCheckpoint `json:"attempts"`
}

type usageAttemptCheckpoint struct {
	AttemptID    string  `json:"attempt_id"`
	Confirmed    bool    `json:"confirmed"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	ToolCalls    int     `json:"tool_calls,omitempty"`
	HasTokens    bool    `json:"has_tokens"`
	HasCost      bool    `json:"has_cost"`
	Source       string  `json:"source,omitempty"`
}

type usageAccumulatorCheckpointInput struct {
	SchemaVersion    *int                        `json:"schema_version"`
	NextCallOrdinals *map[string]uint64          `json:"next_call_ordinals"`
	Calls            *[]usageCallCheckpointInput `json:"calls"`
}

type usageCallCheckpointInput struct {
	UsageCallID *string                        `json:"usage_call_id"`
	RunID       *string                        `json:"run_id"`
	Step        *string                        `json:"step"`
	CallOrdinal *uint64                        `json:"call_ordinal"`
	Attempts    *[]usageAttemptCheckpointInput `json:"attempts"`
}

type usageAttemptCheckpointInput struct {
	AttemptID    *string  `json:"attempt_id"`
	Confirmed    *bool    `json:"confirmed"`
	InputTokens  *int     `json:"input_tokens"`
	OutputTokens *int     `json:"output_tokens"`
	CostUSD      *float64 `json:"cost_usd"`
	ToolCalls    *int     `json:"tool_calls,omitempty"`
	HasTokens    *bool    `json:"has_tokens"`
	HasCost      *bool    `json:"has_cost"`
	Source       *string  `json:"source,omitempty"`
}

type usageAccumulatorCheckpointV1Input struct {
	SchemaVersion    *int                          `json:"schema_version"`
	NextCallOrdinals *map[string]uint64            `json:"next_call_ordinals"`
	Calls            *[]usageCallCheckpointV1Input `json:"calls"`
}

type usageCallCheckpointV1Input struct {
	UsageCallID  *string   `json:"usage_call_id"`
	RunID        *string   `json:"run_id"`
	Step         *string   `json:"step"`
	CallOrdinal  *uint64   `json:"call_ordinal"`
	AttemptIDs   *[]string `json:"attempt_ids"`
	Confirmed    *bool     `json:"confirmed"`
	InputTokens  *int      `json:"input_tokens"`
	OutputTokens *int      `json:"output_tokens"`
	CostUSD      *float64  `json:"cost_usd"`
	ToolCalls    *int      `json:"tool_calls,omitempty"`
}

func optionalInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
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
		attemptIDs := make([]string, 0, len(call.Attempts))
		for attemptID := range call.Attempts {
			attemptIDs = append(attemptIDs, attemptID)
		}
		sort.Strings(attemptIDs)
		encoded := usageCallCheckpoint{
			UsageCallID: callID, RunID: call.RunID, Step: call.Step,
			CallOrdinal: call.CallOrdinal,
			Attempts:    make([]usageAttemptCheckpoint, 0, len(attemptIDs)),
		}
		for _, attemptID := range attemptIDs {
			attempt := call.Attempts[attemptID]
			encoded.Attempts = append(encoded.Attempts, usageAttemptCheckpoint{
				AttemptID: attemptID, Confirmed: attempt.Confirmed,
				InputTokens: attempt.Usage.InputTokens, OutputTokens: attempt.Usage.OutputTokens,
				CostUSD: attempt.Usage.CostUSD, ToolCalls: attempt.ToolCalls,
				HasTokens: attempt.Metadata.HasTokens, HasCost: attempt.Metadata.HasCost,
				Source: attempt.Metadata.Source,
			})
		}
		checkpoint.Calls = append(checkpoint.Calls, encoded)
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
	var header struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return UsageAccumulator{}, fmt.Errorf("decode usage accumulator checkpoint: %w", err)
	}
	if header.SchemaVersion == nil {
		return UsageAccumulator{}, fmt.Errorf("usage accumulator schema_version is required")
	}
	switch *header.SchemaVersion {
	case usageAccumulatorSchema:
		return unmarshalUsageAccumulatorV2(data)
	case usageAccumulatorSchemaV1:
		return unmarshalUsageAccumulatorV1(data)
	default:
		return UsageAccumulator{}, fmt.Errorf("unsupported usage accumulator schema_version %d", *header.SchemaVersion)
	}
}

func decodeUsageAccumulatorCheckpoint(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode usage accumulator checkpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode usage accumulator checkpoint: trailing JSON content")
	}
	return nil
}

func newCheckpointAccumulator(next *map[string]uint64, callsPresent bool) (UsageAccumulator, error) {
	if next == nil {
		return UsageAccumulator{}, fmt.Errorf("usage accumulator next_call_ordinals are required")
	}
	if !callsPresent {
		return UsageAccumulator{}, fmt.Errorf("usage accumulator calls are required")
	}
	accumulator := NewUsageAccumulator()
	for step, ordinal := range *next {
		if step == "" || ordinal > maxUsageCheckpointInteger {
			return UsageAccumulator{}, fmt.Errorf("invalid next_call_ordinals[%q]", step)
		}
		accumulator.nextCallOrdinals[step] = ordinal
	}
	return accumulator, nil
}

func unmarshalUsageAccumulatorV2(data []byte) (UsageAccumulator, error) {
	var input usageAccumulatorCheckpointInput
	if err := decodeUsageAccumulatorCheckpoint(data, &input); err != nil {
		return UsageAccumulator{}, err
	}
	accumulator, err := newCheckpointAccumulator(input.NextCallOrdinals, input.Calls != nil)
	if err != nil {
		return UsageAccumulator{}, err
	}
	for index, rawCall := range *input.Calls {
		if rawCall.UsageCallID == nil || rawCall.RunID == nil || rawCall.Step == nil || rawCall.CallOrdinal == nil || rawCall.Attempts == nil {
			return UsageAccumulator{}, fmt.Errorf("usage accumulator calls[%d] has missing required fields", index)
		}
		callID := *rawCall.UsageCallID
		if _, duplicate := accumulator.calls[callID]; duplicate {
			return UsageAccumulator{}, fmt.Errorf("duplicate usage_call_id %q", callID)
		}
		call := usageCall{RunID: *rawCall.RunID, Step: *rawCall.Step, CallOrdinal: *rawCall.CallOrdinal, Attempts: make(map[string]usageAttempt)}
		for attemptIndex, rawAttempt := range *rawCall.Attempts {
			if rawAttempt.AttemptID == nil || rawAttempt.Confirmed == nil || rawAttempt.InputTokens == nil || rawAttempt.OutputTokens == nil || rawAttempt.CostUSD == nil || rawAttempt.HasTokens == nil || rawAttempt.HasCost == nil {
				return UsageAccumulator{}, fmt.Errorf("usage accumulator calls[%d].attempts[%d] has missing required fields", index, attemptIndex)
			}
			attemptID := *rawAttempt.AttemptID
			if _, duplicate := call.Attempts[attemptID]; duplicate || attemptID == "" {
				return UsageAccumulator{}, fmt.Errorf("duplicate or empty attempt_id %q", attemptID)
			}
			if owner, duplicate := accumulator.attemptOwners[attemptID]; duplicate {
				return UsageAccumulator{}, fmt.Errorf("%w: attempt_id %q belongs to %q", ErrUsageConflict, attemptID, owner)
			}
			call.Attempts[attemptID] = usageAttempt{
				Confirmed: *rawAttempt.Confirmed,
				Usage:     contract.Usage{InputTokens: *rawAttempt.InputTokens, OutputTokens: *rawAttempt.OutputTokens, CostUSD: *rawAttempt.CostUSD},
				ToolCalls: optionalInt(rawAttempt.ToolCalls),
				Metadata:  UsageAttemptMetadata{HasTokens: *rawAttempt.HasTokens, HasCost: *rawAttempt.HasCost, Source: optionalString(rawAttempt.Source)},
			}
			accumulator.attemptOwners[attemptID] = callID
		}
		accumulator.calls[callID] = call
	}
	if err := accumulator.validate(); err != nil {
		return UsageAccumulator{}, err
	}
	return accumulator.clone(), nil
}

func unmarshalUsageAccumulatorV1(data []byte) (UsageAccumulator, error) {
	var input usageAccumulatorCheckpointV1Input
	if err := decodeUsageAccumulatorCheckpoint(data, &input); err != nil {
		return UsageAccumulator{}, err
	}
	accumulator, err := newCheckpointAccumulator(input.NextCallOrdinals, input.Calls != nil)
	if err != nil {
		return UsageAccumulator{}, err
	}
	for index, raw := range *input.Calls {
		if raw.UsageCallID == nil || raw.RunID == nil || raw.Step == nil || raw.CallOrdinal == nil || raw.AttemptIDs == nil || raw.Confirmed == nil || raw.InputTokens == nil || raw.OutputTokens == nil || raw.CostUSD == nil {
			return UsageAccumulator{}, fmt.Errorf("usage accumulator calls[%d] has missing required fields", index)
		}
		callID := *raw.UsageCallID
		call := usageCall{RunID: *raw.RunID, Step: *raw.Step, CallOrdinal: *raw.CallOrdinal, Attempts: make(map[string]usageAttempt)}
		attemptIDs := append([]string(nil), (*raw.AttemptIDs)...)
		sort.Strings(attemptIDs)
		for attemptIndex, attemptID := range attemptIDs {
			if attemptID == "" {
				return UsageAccumulator{}, fmt.Errorf("empty attempt_id for %q", callID)
			}
			if _, duplicate := call.Attempts[attemptID]; duplicate {
				return UsageAccumulator{}, fmt.Errorf("duplicate attempt_id %q", attemptID)
			}
			attempt := usageAttempt{}
			if *raw.Confirmed && attemptIndex == 0 {
				attempt = usageAttempt{
					Confirmed: true,
					Usage:     contract.Usage{InputTokens: *raw.InputTokens, OutputTokens: *raw.OutputTokens, CostUSD: *raw.CostUSD},
					ToolCalls: optionalInt(raw.ToolCalls),
					Metadata:  UsageAttemptMetadata{HasTokens: true, HasCost: true},
				}
			}
			call.Attempts[attemptID] = attempt
			if owner, duplicate := accumulator.attemptOwners[attemptID]; duplicate {
				return UsageAccumulator{}, fmt.Errorf("%w: attempt_id %q belongs to %q", ErrUsageConflict, attemptID, owner)
			}
			accumulator.attemptOwners[attemptID] = callID
		}
		if _, duplicate := accumulator.calls[callID]; duplicate {
			return UsageAccumulator{}, fmt.Errorf("duplicate usage_call_id %q", callID)
		}
		accumulator.calls[callID] = call
	}
	if err := accumulator.validate(); err != nil {
		return UsageAccumulator{}, err
	}
	return accumulator.clone(), nil
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
		if call.Attempts == nil {
			return fmt.Errorf("usage_call_id %q attempts are nil", callID)
		}
		for attemptID, attempt := range call.Attempts {
			if attemptID == "" {
				return fmt.Errorf("usage_call_id %q has empty attempt_id", callID)
			}
			if owner := a.attemptOwners[attemptID]; owner != callID {
				return fmt.Errorf("attempt_id %q owner mismatch", attemptID)
			}
			if !attempt.Confirmed && (attempt.Usage != (contract.Usage{}) || attempt.ToolCalls != 0 || attempt.Metadata != (UsageAttemptMetadata{})) {
				return fmt.Errorf("unconfirmed attempt_id %q carries usage metadata", attemptID)
			}
			if err := validateUsage(attempt.Usage); err != nil {
				return fmt.Errorf("attempt_id %q: %w", attemptID, err)
			}
			if attempt.ToolCalls < 0 {
				return fmt.Errorf("attempt_id %q has negative tool_calls", attemptID)
			}
			if !attempt.Metadata.HasTokens && (attempt.Usage.InputTokens != 0 || attempt.Usage.OutputTokens != 0) {
				return fmt.Errorf("attempt_id %q carries unreported token usage", attemptID)
			}
			if !attempt.Metadata.HasCost && attempt.Usage.CostUSD != 0 {
				return fmt.Errorf("attempt_id %q carries unreported cost usage", attemptID)
			}
		}
	}
	for attemptID, callID := range a.attemptOwners {
		call, exists := a.calls[callID]
		if attemptID == "" || !exists {
			return fmt.Errorf("attempt_id %q has invalid owner %q", attemptID, callID)
		}
		if _, exists := call.Attempts[attemptID]; !exists {
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
