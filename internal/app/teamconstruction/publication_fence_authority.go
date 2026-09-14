package teamconstruction

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/app/accesschange"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
)

func (a *PublicationAuthority) AuthorizeFence(ctx context.Context, command admissionfence.Command) error {
	if a == nil || a.pool == nil {
		return errors.New("permission change authority unavailable")
	}
	return accesschange.NewStore(a.pool).AuthorizeFence(ctx, command)
}
