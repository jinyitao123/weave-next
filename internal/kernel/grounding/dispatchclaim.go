package grounding

import (
	"regexp"
	"strings"
)

var dispatchArrangementPattern = regexp.MustCompile(`(已|已经)安排.*?去(查|做|跑|核|执行)`)

var dispatchClaimMarkers = []string{
	"已安排",
	"已经安排",
	"已派",
	"已经派",
	"派好了",
	"安排好了",
	"已交给",
	"已经交给",
	"已提交给",
}

// ClaimsDispatch reports whether reply claims completed dispatch to a named worker.
func ClaimsDispatch(reply string, workerNames []string) bool {
	hasWorker := false
	for _, name := range workerNames {
		if name != "" && strings.Contains(reply, name) {
			hasWorker = true
			break
		}
	}
	if !hasWorker {
		return false
	}

	for _, marker := range dispatchClaimMarkers {
		if strings.Contains(reply, marker) {
			return true
		}
	}
	return dispatchArrangementPattern.MatchString(reply)
}
