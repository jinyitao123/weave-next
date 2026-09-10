package execution

import "errors"

// ErrMemberOutcomeUnknown marks a dispatched tool without a confirmed receipt.
var ErrMemberOutcomeUnknown = errors.New("member tool outcome requires reconciliation")
