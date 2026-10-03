package businessaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

var boundObjectName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,127}$`)

// CompletionRecordBinding reuses the execution boundary's frozen-resource
// validation. Completion readers must not invent a second record wire format.
func CompletionRecordBinding(raw []byte, inputRevisionID, taskSHA256 string) (*TaskBusinessRecord, error) {
	resources, err := decodeDelegatedResources(raw, inputRevisionID)
	if err != nil {
		return nil, err
	}
	var stored []TaskDelegationResource
	if json.Unmarshal(raw, &stored) != nil {
		return nil, errors.New("task resources are invalid")
	}
	for _, item := range stored {
		if item.Type == "dispatch-input" && item.SHA256 != taskSHA256 {
			return nil, errors.New("task input digest differs")
		}
	}
	var record *TaskBusinessRecord
	for _, item := range resources {
		if item.Type == "forge-record" {
			if record != nil {
				return nil, errors.New("task has multiple record bindings")
			}
			record = &TaskBusinessRecord{ObjectName: item.ObjectName, RecordID: item.ID}
		}
	}
	return record, nil
}

func validateRecordResource(item delegatedResource) error {
	if !boundObjectName.MatchString(item.ObjectName) || strings.HasPrefix(item.ObjectName, "sys_") ||
		item.ID == "" || len(item.ID) > 128 || item.ID != strings.TrimSpace(item.ID) || strings.ContainsAny(item.ID, "\x00\r\n") {
		return errors.New("task business record is invalid")
	}
	raw, _ := json.Marshal(map[string]string{"object_name": item.ObjectName, "id": item.ID})
	digest := sha256.Sum256(raw)
	if item.SHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("task business record digest differs")
	}
	return nil
}

// A record is an exact task input. The model may omit it; if it asserts a value,
// the tool schema must reject substitution before any upstream side effect.
func (d *dispatcher) bindRecords(catalog map[string]actionMetadata, resources []delegatedResource) error {
	var record *delegatedResource
	for _, resource := range resources {
		if resource.Type != "forge-record" {
			continue
		}
		if record != nil {
			return errors.New("task contains multiple business record bindings")
		}
		if err := validateRecordResource(resource); err != nil {
			return err
		}
		copy := resource
		record = &copy
	}
	d.records = make(map[string]string, len(d.tools))
	d.recordHashes = make(map[string]string, len(d.tools))
	for i := range d.tools {
		tool := &d.tools[i]
		action := d.byTool[tool.Name]
		metadata := catalog[action.objectName+"."+action.actionName]
		recordID := ""
		if metadata.RequiresRecord && record != nil && record.ObjectName == action.objectName {
			recordID = record.ID
			d.recordHashes[tool.Name] = record.SHA256
		}
		if metadata.RequiresRecord && recordID == "" {
			return fmt.Errorf("业务动作 %s 缺少本次员工指定的业务记录", action.actionName)
		}
		d.records[tool.Name] = recordID
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			return err
		}
		properties := schema["properties"].(map[string]any)
		delete(properties, "recordId")
		if recordID != "" {
			properties["recordId"] = map[string]any{"type": "string", "enum": []string{recordID}, "description": "已绑定员工本次指定的业务记录，可省略；不能更换。"}
		}
		required := []any{}
		if existing, ok := schema["required"].([]any); ok {
			for _, field := range existing {
				if field != "recordId" {
					required = append(required, field)
				}
			}
		}
		delete(schema, "required")
		if len(required) > 0 {
			schema["required"] = required
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			return err
		}
		tool.InputSchema = raw
	}
	bound, err := mcphost.NewToolContract(d.tools)
	if err != nil {
		return err
	}
	d.bound = bound
	return nil
}
