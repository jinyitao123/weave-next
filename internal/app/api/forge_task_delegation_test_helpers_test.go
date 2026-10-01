package api

import "net/http"

// setTestForgeTaskDelegation attaches a Forge-issued task delegation the way
// the Workbench Host does. Grant identity and expiry come from Forge's online
// current endpoint, never from caller-controlled headers.
func setTestForgeTaskDelegation(header http.Header, token string) {
	header.Set(forgeDelegationHeader, "Bearer "+token)
}
