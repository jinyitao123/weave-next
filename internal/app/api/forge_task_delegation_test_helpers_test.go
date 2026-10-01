package api

import (
	"net/http"
	"time"
)

// setTestForgeTaskDelegation attaches a Forge-issued task delegation the way
// the Workbench Host does (decision 002): credential, Forge delegation id and
// Forge expiry. Forge keeps the delegation id when it rotates a credential.
func setTestForgeTaskDelegation(header http.Header, token string) {
	header.Set(forgeDelegationHeader, "Bearer "+token)
	header.Set(forgeDelegationIDHeader, "forge-delegation-test")
	header.Set(forgeDelegationExpiresHeader, time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339Nano))
}
