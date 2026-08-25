package loomruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const a4AdmissionReceiptSchemaV1 = 1

var ErrA4AdmissionReceiptConflict = errors.New("a4 admission receipt conflict")

type A4AdmissionReceiptV1 struct {
	SchemaVersion            int       `json:"schema_version"`
	WorkspaceID              string    `json:"workspace_id"`
	RunID                    string    `json:"run_id"`
	ClaimKey                 string    `json:"claim_key"`
	DomainKey                string    `json:"domain_key"`
	CanonicalClaimSHA256     string    `json:"canonical_claim_sha256"`
	InitialAttemptGeneration int64     `json:"initial_attempt_generation"`
	InitialAttemptID         uuid.UUID `json:"initial_attempt_id"`
	InitialGraphName         string    `json:"initial_graph_name"`
	InitialRunStartedAt      string    `json:"initial_run_started_at"`
}

func a4AdmissionReceiptNamespace(workspaceID string) string {
	return "runadm:" + workspaceID
}

func a4AdmissionReceiptKey(runID string) string {
	return expectedRunClaimKey(runID)
}

func newA4AdmissionReceiptV1(
	claim ExpectedRunRecordV1,
	canonicalClaim []byte,
	domainKey string,
	lease RunAttemptLease,
) (A4AdmissionReceiptV1, error) {
	digest := sha256.Sum256(canonicalClaim)
	receipt := A4AdmissionReceiptV1{
		SchemaVersion:            a4AdmissionReceiptSchemaV1,
		WorkspaceID:              claim.WorkspaceID,
		RunID:                    claim.RunID,
		ClaimKey:                 expectedRunClaimKey(claim.RunID),
		DomainKey:                domainKey,
		CanonicalClaimSHA256:     hex.EncodeToString(digest[:]),
		InitialAttemptGeneration: 1,
		InitialAttemptID:         lease.AttemptID,
		InitialGraphName:         lease.GraphName,
		InitialRunStartedAt:      lease.RunStartedAt,
	}
	if err := validateA4AdmissionReceiptBinding(
		receipt,
		a4AdmissionReceiptNamespace(claim.WorkspaceID),
		a4AdmissionReceiptKey(claim.RunID),
		claim,
		canonicalClaim,
		domainKey,
		lease,
	); err != nil {
		return A4AdmissionReceiptV1{}, err
	}
	return receipt, nil
}

func encodeA4AdmissionReceiptV1(receipt A4AdmissionReceiptV1) ([]byte, error) {
	if err := validateA4AdmissionReceiptV1(receipt); err != nil {
		return nil, err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("marshal a4 admission receipt: %w", err)
	}
	return data, nil
}

func decodeA4AdmissionReceiptV1(data []byte) (A4AdmissionReceiptV1, error) {
	fields, err := decodeA4AdmissionReceiptObject(data)
	if err != nil {
		return A4AdmissionReceiptV1{}, err
	}

	var receipt A4AdmissionReceiptV1
	if receipt.SchemaVersion, err = decodeA4AdmissionReceiptInt(fields["schema_version"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode schema_version: %w", err)
	}
	if receipt.WorkspaceID, err = decodeA4AdmissionReceiptString(fields["workspace_id"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode workspace_id: %w", err)
	}
	if receipt.RunID, err = decodeA4AdmissionReceiptString(fields["run_id"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode run_id: %w", err)
	}
	if receipt.ClaimKey, err = decodeA4AdmissionReceiptString(fields["claim_key"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode claim_key: %w", err)
	}
	if receipt.DomainKey, err = decodeA4AdmissionReceiptString(fields["domain_key"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode domain_key: %w", err)
	}
	if receipt.CanonicalClaimSHA256, err = decodeA4AdmissionReceiptString(fields["canonical_claim_sha256"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode canonical_claim_sha256: %w", err)
	}
	if receipt.InitialAttemptGeneration, err = decodeA4AdmissionReceiptInt64(fields["initial_attempt_generation"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode initial_attempt_generation: %w", err)
	}
	attemptID, err := decodeA4AdmissionReceiptString(fields["initial_attempt_id"])
	if err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode initial_attempt_id: %w", err)
	}
	receipt.InitialAttemptID, err = uuid.Parse(attemptID)
	if err != nil || receipt.InitialAttemptID.String() != attemptID {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode initial_attempt_id: non-canonical UUID")
	}
	if receipt.InitialGraphName, err = decodeA4AdmissionReceiptString(fields["initial_graph_name"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode initial_graph_name: %w", err)
	}
	if receipt.InitialRunStartedAt, err = decodeA4AdmissionReceiptString(fields["initial_run_started_at"]); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("decode initial_run_started_at: %w", err)
	}

	if err := validateA4AdmissionReceiptV1(receipt); err != nil {
		return A4AdmissionReceiptV1{}, fmt.Errorf("validate a4 admission receipt: %w", err)
	}
	return receipt, nil
}

func validateA4AdmissionReceiptBinding(
	receipt A4AdmissionReceiptV1,
	namespace string,
	key string,
	claim ExpectedRunRecordV1,
	canonicalClaim []byte,
	domainKey string,
	lease RunAttemptLease,
) error {
	if err := validateA4AdmissionReceiptV1(receipt); err != nil {
		return a4AdmissionReceiptConflict("invalid embedded receipt: %v", err)
	}
	encodedClaim, err := encodeExpectedRunRecordV1(claim)
	if err != nil {
		return a4AdmissionReceiptConflict("invalid expected run claim: %v", err)
	}
	if !bytes.Equal(canonicalClaim, encodedClaim) {
		return a4AdmissionReceiptConflict("canonical claim bytes do not match expected run claim")
	}
	if namespace != a4AdmissionReceiptNamespace(claim.WorkspaceID) {
		return a4AdmissionReceiptConflict("physical namespace mismatch")
	}
	if key != a4AdmissionReceiptKey(claim.RunID) {
		return a4AdmissionReceiptConflict("physical key mismatch")
	}
	if receipt.WorkspaceID != claim.WorkspaceID {
		return a4AdmissionReceiptConflict("embedded workspace_id mismatch")
	}
	if receipt.RunID != claim.RunID {
		return a4AdmissionReceiptConflict("embedded run_id mismatch")
	}
	if receipt.ClaimKey != expectedRunClaimKey(claim.RunID) {
		return a4AdmissionReceiptConflict("embedded claim_key mismatch")
	}
	wantDomainKey := expectedRunDomainKey(claim)
	if domainKey != wantDomainKey {
		return a4AdmissionReceiptConflict("physical domain_key mismatch")
	}
	if receipt.DomainKey != wantDomainKey {
		return a4AdmissionReceiptConflict("embedded domain_key mismatch")
	}
	digest := sha256.Sum256(canonicalClaim)
	if receipt.CanonicalClaimSHA256 != hex.EncodeToString(digest[:]) {
		return a4AdmissionReceiptConflict("canonical claim digest mismatch")
	}
	if lease.WorkspaceID != claim.WorkspaceID {
		return a4AdmissionReceiptConflict("lease workspace_id mismatch")
	}
	if lease.RunID != claim.RunID {
		return a4AdmissionReceiptConflict("lease run_id mismatch")
	}
	if lease.AttemptGeneration != 1 ||
		receipt.InitialAttemptGeneration != lease.AttemptGeneration {
		return a4AdmissionReceiptConflict("lease attempt_generation mismatch")
	}
	if receipt.InitialAttemptID != lease.AttemptID {
		return a4AdmissionReceiptConflict("lease attempt_id mismatch")
	}
	if receipt.InitialGraphName != lease.GraphName {
		return a4AdmissionReceiptConflict("lease graph_name mismatch")
	}
	if receipt.InitialRunStartedAt != lease.RunStartedAt {
		return a4AdmissionReceiptConflict("lease run_started_at mismatch")
	}
	if lease.State != AttemptLeaseActive {
		return a4AdmissionReceiptConflict("lease state is not initial active state")
	}
	if lease.AttemptStartedAt.IsZero() || lease.HeartbeatAt.IsZero() ||
		!lease.AttemptStartedAt.Equal(lease.HeartbeatAt) {
		return a4AdmissionReceiptConflict("lease initial attempt_started_at/heartbeat_at mismatch")
	}
	if !lease.LeaseExpiresAt.After(lease.HeartbeatAt) {
		return a4AdmissionReceiptConflict("lease initial expiry must be after heartbeat")
	}
	if lease.ClaimID != nil || lease.ClaimExpiresAt != nil {
		return a4AdmissionReceiptConflict("lease initial claim fields must be nil")
	}
	if lease.LastErrorCode != nil {
		return a4AdmissionReceiptConflict("lease initial last_error_code must be nil")
	}
	if lease.RetryCount != 0 {
		return a4AdmissionReceiptConflict("lease initial retry_count must be zero")
	}
	return nil
}

func validateA4AdmissionReceiptV1(receipt A4AdmissionReceiptV1) error {
	if receipt.SchemaVersion != a4AdmissionReceiptSchemaV1 {
		return fmt.Errorf("schema_version must be %d", a4AdmissionReceiptSchemaV1)
	}
	if receipt.WorkspaceID == "" {
		return fmt.Errorf("workspace_id must be non-empty")
	}
	if receipt.RunID == "" {
		return fmt.Errorf("run_id must be non-empty")
	}
	if receipt.ClaimKey != expectedRunClaimKey(receipt.RunID) {
		return fmt.Errorf("claim_key must equal canonical run claim key")
	}
	if err := validateA4AdmissionReceiptDomainKey(receipt.DomainKey, receipt.RunID); err != nil {
		return err
	}
	if len(receipt.CanonicalClaimSHA256) != sha256.Size*2 {
		return fmt.Errorf("canonical_claim_sha256 must be 64 lowercase hexadecimal characters")
	}
	for _, char := range []byte(receipt.CanonicalClaimSHA256) {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return fmt.Errorf("canonical_claim_sha256 must be 64 lowercase hexadecimal characters")
		}
	}
	if receipt.InitialAttemptGeneration != 1 {
		return fmt.Errorf("initial_attempt_generation must be 1")
	}
	if receipt.InitialAttemptID == uuid.Nil {
		return fmt.Errorf("initial_attempt_id must be non-zero")
	}
	if receipt.InitialGraphName == "" {
		return fmt.Errorf("initial_graph_name must be non-empty")
	}
	if err := validateA4AdmissionReceiptTime(receipt.InitialRunStartedAt); err != nil {
		return err
	}
	return nil
}

func validateA4AdmissionReceiptDomainKey(domainKey string, runID string) error {
	runComponent := expectedRunEncodeKeyComponent(runID)
	if domainKey == "v1/legacy_unattributed/run/"+runComponent {
		return nil
	}
	const snapshotPrefix = "v1/snapshot/"
	if !strings.HasPrefix(domainKey, snapshotPrefix) {
		return fmt.Errorf("domain_key must be a canonical expected run domain key")
	}
	rest := strings.TrimPrefix(domainKey, snapshotPrefix)
	parts := strings.Split(rest, "/run/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != runComponent {
		return fmt.Errorf("domain_key must be a canonical expected run domain key")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) ||
		base64.RawURLEncoding.EncodeToString(decoded) != parts[0] {
		return fmt.Errorf("domain_key must be a canonical expected run domain key")
	}
	return nil
}

func validateA4AdmissionReceiptTime(raw string) error {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !strings.HasSuffix(raw, "Z") || parsed.Location() != time.UTC ||
		parsed.Format(time.RFC3339Nano) != raw {
		return fmt.Errorf("initial_run_started_at must be canonical UTC RFC3339Nano")
	}
	return nil
}

func decodeA4AdmissionReceiptObject(data []byte) (map[string]json.RawMessage, error) {
	allowed := map[string]struct{}{
		"schema_version": {}, "workspace_id": {}, "run_id": {},
		"claim_key": {}, "domain_key": {}, "canonical_claim_sha256": {},
		"initial_attempt_generation": {}, "initial_attempt_id": {},
		"initial_graph_name": {}, "initial_run_started_at": {},
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("a4 admission receipt must be one JSON object")
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		rawName, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode a4 admission receipt field name: %w", err)
		}
		name, ok := rawName.(string)
		if !ok {
			return nil, fmt.Errorf("a4 admission receipt field name is not a string")
		}
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("unknown a4 admission receipt field %q", name)
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate a4 admission receipt field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode a4 admission receipt field %q: %w", name, err)
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("close a4 admission receipt object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("a4 admission receipt has trailing JSON content")
	}
	if len(fields) != len(allowed) {
		missing := make([]string, 0)
		for name := range allowed {
			if _, ok := fields[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("a4 admission receipt missing fields: %s", strings.Join(missing, ", "))
	}
	return fields, nil
}

func decodeA4AdmissionReceiptString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func decodeA4AdmissionReceiptInt(raw json.RawMessage) (int, error) {
	parsed, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected exact integer: %w", err)
	}
	value := int(parsed)
	if int64(value) != parsed {
		return 0, fmt.Errorf("integer overflows int")
	}
	return value, nil
}

func decodeA4AdmissionReceiptInt64(raw json.RawMessage) (int64, error) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected exact integer: %w", err)
	}
	return value, nil
}

func a4AdmissionReceiptConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrA4AdmissionReceiptConflict, fmt.Sprintf(format, args...))
}
