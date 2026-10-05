package loomruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// Preserve already observed failure facts even when the original model context
// was cancelled. This bounded best-effort write replaces ONLY the reserved
// diagnostic sample in the existing journal slot. It cannot change an input,
// response, usage, attempt identity, checkpoint or business/run state.
func (member *memberExecution) persistProtocolProbe(ctx context.Context, namespace, key, inputHash string, index int, sample ModelProtocolProbeSample) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	tx, err := member.runner.store.BeginTx(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if tx.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2 FOR UPDATE`, namespace, key).Scan(&raw) != nil {
		return
	}
	var op memberOperation
	if json.Unmarshal(raw, &op) != nil || op.Kind != "model" || op.InputHash != inputHash || index < 0 || index >= len(op.ProtocolProbe) {
		return
	}
	reservation := op.ProtocolProbe[index]
	if reservation.PolicySHA256 != sample.PolicySHA256 || reservation.AttemptGeneration != sample.AttemptGeneration || reservation.Attempt != sample.Attempt {
		return
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		return
	}
	// Replace one exact JSON value without re-encoding even a byte of the
	// original input/response/usage or other observations.
	updated, err := replaceProtocolProbeSample(raw, index, encoded)
	if err != nil {
		return
	}
	if err = member.runner.store.PutValueTx(ctx, tx, namespace, key, updated); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

// A current execution must not replace another generation's late diagnostic
// update with its stale local copy when writing the ordinary journal receipt.
func (member *memberExecution) mergeProtocolProbeTx(ctx context.Context, tx pgx.Tx, namespace, key string, op *memberOperation) error {
	if len(op.ProtocolProbe) == 0 {
		return nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2 FOR UPDATE`, namespace, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var current memberOperation
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	op.ProtocolProbe = mergeProtocolProbeSamples(current.ProtocolProbe, op.ProtocolProbe, op.AttemptGeneration, op.Attempts)
	return nil
}

func mergeProtocolProbeSamples(current, local []ModelProtocolProbeSample, generation, attempt int64) []ModelProtocolProbeSample {
	merged := append([]ModelProtocolProbeSample(nil), current...)
	for _, sample := range local {
		found := false
		for index, existing := range merged {
			if existing.PolicySHA256 == sample.PolicySHA256 && existing.AttemptGeneration == sample.AttemptGeneration && existing.Attempt == sample.Attempt {
				if sample.AttemptGeneration == generation && sample.Attempt == attempt {
					merged[index] = sample
				}
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, sample)
		}
	}
	return merged
}

func replaceProtocolProbeSample(raw []byte, index int, replacement []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid journal object")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if key != "protocol_probe" {
			continue
		}
		fieldStart := int(decoder.InputOffset()) - len(value)
		array := json.NewDecoder(bytes.NewReader(value))
		if token, err := array.Token(); err != nil || token != json.Delim('[') {
			return nil, errors.New("invalid probe array")
		}
		for current := 0; array.More(); current++ {
			var entry json.RawMessage
			if err := array.Decode(&entry); err != nil {
				return nil, err
			}
			if current != index {
				continue
			}
			end := fieldStart + int(array.InputOffset())
			start := end - len(entry)
			result := make([]byte, 0, len(raw)-len(entry)+len(replacement))
			result = append(result, raw[:start]...)
			result = append(result, replacement...)
			return append(result, raw[end:]...), nil
		}
	}
	return nil, errors.New("probe reservation not found")
}
