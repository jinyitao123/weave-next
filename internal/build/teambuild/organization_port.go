package teambuild

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/orgspec"
)

// OrganizationBaselineReader supplies product-owned team facts inside the
// caller's existing baseline transaction. It grants no organization mutation.
type OrganizationBaselineReader interface {
	GetTeam(context.Context, string, string) (orgspec.Team, error)
	GetTeamTx(context.Context, pgx.Tx, string, string) (orgspec.Team, error)
	GetTeamDispatchRulesTx(context.Context, pgx.Tx, string, string) (orgspec.TeamDispatchRules, error)
}

func organizationBaselineReaderAvailable(read OrganizationBaselineReader) bool {
	if read == nil {
		return false
	}
	value := reflect.ValueOf(read)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}
