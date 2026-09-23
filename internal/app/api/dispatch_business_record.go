package api

import (
	"encoding/json"
	"regexp"
	"strings"
)

// This is selected by the trusted employee Host, never inferred from task prose.
// Forge remains responsible for record permissions and current business state.
type dispatchBusinessRecord struct {
	ObjectName string `json:"object_name"`
	RecordID   string `json:"record_id"`
}

var businessObjectName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,127}$`)

func validDispatchBusinessRecord(record *dispatchBusinessRecord) bool {
	return record == nil || (businessObjectName.MatchString(record.ObjectName) &&
		!strings.HasPrefix(record.ObjectName, "sys_") && record.RecordID != "" &&
		len(record.RecordID) <= 128 && record.RecordID == strings.TrimSpace(record.RecordID) &&
		!strings.ContainsAny(record.RecordID, "\x00\r\n"))
}

func (record dispatchBusinessRecord) resource() map[string]any {
	identity := map[string]string{"object_name": record.ObjectName, "id": record.RecordID}
	raw, _ := json.Marshal(identity)
	return map[string]any{"type": "forge-record", "id": record.RecordID, "object_name": record.ObjectName, "sha256": dispatchInputDigest(raw)}
}
