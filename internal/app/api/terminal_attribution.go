package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/weave/internal/app/kernelbindings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

type runAssociationsEvidence struct {
	SchemaVersion    json.RawMessage `json:"schema_version"`
	ParentRunID      json.RawMessage `json:"parent_run_id"`
	SourceSnapshotID json.RawMessage `json:"source_snapshot_id"`
	TaskGroupID      json.RawMessage `json:"task_group_id"`
}

type loomTerminalRecordStore struct {
	store loom.Store
}

func (adapter loomTerminalRecordStore) ReadValue(
	ctx context.Context,
	namespace string,
	key string,
) ([]byte, bool, error) {
	return readLoomStoreValue(ctx, adapter.store, namespace, key)
}

func (adapter loomTerminalRecordStore) ListKeys(
	ctx context.Context,
	namespace string,
) ([]string, error) {
	if adapter.store == nil {
		return nil, fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.List(ctx, namespace, "")
}

func (adapter loomTerminalRecordStore) MutateValue(
	ctx context.Context,
	namespace string,
	key string,
	mutate func(current []byte, present bool) (next []byte, err error),
) error {
	if adapter.store == nil {
		return fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.Tx(ctx, func(tx loom.Store) error {
		current, present, err := readLoomStoreValue(ctx, tx, namespace, key)
		if err != nil {
			return err
		}
		next, err := mutate(current, present)
		if err != nil {
			return err
		}
		if next == nil {
			return tx.Delete(ctx, namespace, key)
		}
		return tx.Put(ctx, namespace, key, next)
	})
}

func readLoomStoreValue(
	ctx context.Context,
	store loom.Store,
	namespace string,
	key string,
) ([]byte, bool, error) {
	if store == nil {
		return nil, false, fmt.Errorf("terminal store is unavailable")
	}
	value, err := store.Get(ctx, namespace, key)
	if err == nil {
		return bytes.Clone(value), true, nil
	}
	keys, listErr := store.List(ctx, namespace, key)
	if listErr != nil {
		return nil, false, fmt.Errorf(
			"read terminal %q/%q: %v (verify absence: %w)",
			namespace,
			key,
			err,
			listErr,
		)
	}
	for _, listedKey := range keys {
		if listedKey == key {
			return nil, false, fmt.Errorf(
				"read existing terminal %q/%q: %w",
				namespace,
				key,
				err,
			)
		}
	}
	return nil, false, nil
}

func (s *Server) rootTerminalSink() (loomruntime.TerminalSink, error) {
	if s.StoreExt != nil {
		return loomruntime.NewLineageTerminalSink(kernelbindings.WithTerminalActivity(s.StoreExt))
	}
	return loomruntime.NewLineageTerminalSink(
		loomTerminalRecordStore{store: s.Store},
	)
}
