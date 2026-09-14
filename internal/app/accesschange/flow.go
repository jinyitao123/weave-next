package accesschange

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"

	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
)

type Flow struct {
	Store  *Store
	Kernel admissionfence.Service
	// Validate runs only when reserving a new intent, inside its product
	// transaction and before any Kernel fence call. It must not call another
	// storage domain or produce external side effects.
	Validate Validator
}

// Apply executes only product operations inside a product transaction. Every
// Kernel call has its own durable operation ID and is outside that transaction.
func (f Flow) Apply(ctx context.Context, intent Intent, mutation Mutation) (Result, error) {
	return f.run(ctx, intent, func(ctx context.Context, r record) (record, error) { return f.Store.apply(ctx, r, mutation) })
}

// ApplyReceiptMutation is for a mutation owned by another storage domain.
// The callback must return an immutable receipt using this same operation ID;
// an unknown response is recovered by replaying that domain's own operation.
func (f Flow) ApplyReceiptMutation(ctx context.Context, intent Intent, mutation func(context.Context) (json.RawMessage, error)) (Result, error) {
	return f.run(ctx, intent, func(ctx context.Context, r record) (record, error) {
		value, err := mutation(ctx)
		if err != nil {
			return r, err
		}
		return f.Store.apply(ctx, r, func(context.Context, pgx.Tx) (json.RawMessage, error) { return value, nil })
	})
}

func (f Flow) run(ctx context.Context, intent Intent, apply func(context.Context, record) (record, error)) (Result, error) {
	if f.Store == nil || f.Kernel == nil {
		return Result{}, errors.New("permission change service unavailable")
	}
	r, err := f.Store.reserve(ctx, intent, f.Validate)
	if err != nil {
		return Result{}, err
	}
	if r.Result.State == "completed" {
		return r.Result, nil
	}
	if len(r.Intent.Block) > 0 && r.Result.BlockReceipt == nil {
		receipt, err := f.Kernel.TransitionFence(ctx, fenceCommand(r.Intent, admissionfence.Block))
		if err != nil {
			return r.Result, err
		}
		r, err = f.Store.saveFence(ctx, r, receipt)
		if err != nil {
			return r.Result, err
		}
	}
	if r.Result.State != "applied" && r.Result.State != "completed" {
		r, err = apply(ctx, r)
		if err != nil {
			return r.Result, err
		}
	}
	if len(r.Intent.Grant) > 0 && r.Result.GrantReceipt == nil {
		receipt, err := f.Kernel.TransitionFence(ctx, fenceCommand(r.Intent, admissionfence.Regrant))
		if err != nil {
			return r.Result, err
		}
		r, err = f.Store.saveFence(ctx, r, receipt)
		if err != nil {
			return r.Result, err
		}
	}
	return r.Result, nil
}
