package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

const maxForgeObjectMetadataBytes = 4 << 20

// objectActionMetadataSource reads declarations with the same Forge employee
// session as list_actions. It is only used to enrich actions already present in
// that employee-visible allowlist.
type objectActionMetadataSource interface {
	ReadObjectMetadata(context.Context, string) (objectSchemaMetadata, error)
}

type forgeObjectMetadataReader struct {
	baseURL       url.URL
	authorization []byte
	client        *http.Client
	pathPrefix    string
}

type objectMetadataEnvelope struct {
	Type string               `json:"type"`
	Name string               `json:"name"`
	Item objectSchemaMetadata `json:"item"`
}

type objectSchemaMetadata struct {
	Name    string                         `json:"name"`
	Actions []actionDeclarationMetadata    `json:"actions"`
	Fields  map[string]actionFieldMetadata `json:"fields"`
}

type actionDeclarationMetadata struct {
	Name   string                       `json:"name"`
	Params []actionParameterDeclaration `json:"params"`
}

type actionParameterDeclaration struct {
	Name           string          `json:"name"`
	Field          string          `json:"field"`
	ObjectOverride string          `json:"objectOverride"`
	Type           string          `json:"type"`
	Multiple       *bool           `json:"multiple"`
	Required       *bool           `json:"required"`
	Options        json.RawMessage `json:"options"`
}

type actionFieldMetadata struct {
	Type     string          `json:"type"`
	Multiple *bool           `json:"multiple"`
	Required *bool           `json:"required"`
	Options  json.RawMessage `json:"options"`
}

type resolvedActionParameter struct {
	Type               string
	Multiple           bool
	Enum               []string
	UsesObjectOverride bool
}

func (r forgeObjectMetadataReader) ReadObjectMetadata(ctx context.Context, objectName string) (objectSchemaMetadata, error) {
	if strings.TrimSpace(objectName) == "" || objectName != strings.TrimSpace(objectName) {
		return objectSchemaMetadata{}, errors.New("Forge object metadata name is invalid")
	}
	if len(r.authorization) == 0 {
		return objectSchemaMetadata{}, errors.New("Forge object metadata authorization is missing")
	}
	endpoint := r.baseURL
	prefix := r.pathPrefix
	if prefix == "" {
		prefix = "/api/v1/meta/objects/"
	}
	endpoint.Path = prefix + objectName
	endpoint.RawPath = prefix + url.PathEscape(objectName)
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return objectSchemaMetadata{}, fmt.Errorf("build Forge object metadata request: %w", err)
	}
	request.Header.Set("Authorization", string(r.authorization))
	request.Header.Set("Accept", "application/json")
	client := r.client
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return objectSchemaMetadata{}, fmt.Errorf("read Forge object metadata for %q: %w", objectName, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return objectSchemaMetadata{}, fmt.Errorf("read Forge object metadata for %q returned HTTP %d", objectName, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxForgeObjectMetadataBytes+1))
	if err != nil {
		return objectSchemaMetadata{}, fmt.Errorf("read Forge object metadata body for %q: %w", objectName, err)
	}
	if len(body) > maxForgeObjectMetadataBytes {
		return objectSchemaMetadata{}, fmt.Errorf("Forge object metadata for %q exceeds the response limit", objectName)
	}
	var envelope objectMetadataEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return objectSchemaMetadata{}, fmt.Errorf("Forge object metadata for %q has invalid JSON", objectName)
	}
	if envelope.Type != "object" || envelope.Name != objectName || envelope.Item.Name != objectName {
		return objectSchemaMetadata{}, fmt.Errorf("Forge object metadata wrapper for %q is mismatched", objectName)
	}
	return envelope.Item, nil
}

// readActionCatalog treats list_actions as the capability allowlist. Native
// object metadata supplies declared parameter types only for actions requested
// by this frozen member and already visible to the current Forge employee.
func readActionCatalog(
	ctx context.Context,
	host contract.ToolDispatcher,
	requested []string,
	metadataSource objectActionMetadataSource,
) (map[string]actionMetadata, error) {
	result, err := host.Dispatch(ctx, contract.ToolCall{ID: "forge-action-catalog", Name: "list_actions", Args: `{}`})
	if err != nil {
		return nil, fmt.Errorf("%w: Forge action catalog unavailable: %v", mcphost.ErrFailClosed, err)
	}
	if result == nil || result.IsError {
		message := "empty response"
		if result != nil && strings.TrimSpace(result.Content) != "" {
			message = strings.TrimSpace(result.Content)
		}
		return nil, fmt.Errorf("%w: Forge action catalog unavailable: %s", mcphost.ErrFailClosed, message)
	}
	var payload struct {
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil || payload.Actions == nil {
		return nil, fmt.Errorf("%w: Forge action catalog is invalid", mcphost.ErrFailClosed)
	}
	visible := make(map[string]actionMetadata, len(payload.Actions))
	for _, raw := range payload.Actions {
		var item actionMetadata
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("%w: Forge action catalog contains an invalid action", mcphost.ErrFailClosed)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("%w: Forge action catalog contains an invalid action", mcphost.ErrFailClosed)
		}
		if err := validateActionSummaryParameters(fields["params"], item.Params); err != nil {
			return nil, fmt.Errorf("%w: Forge action catalog parameters are incomplete: %v", mcphost.ErrFailClosed, err)
		}
		if required, exists := fields["requiresRecord"]; !exists {
			// Unknown metadata must never broaden a business action's record scope.
			item.RequiresRecord = true
		} else if string(required) != "true" && string(required) != "false" {
			return nil, fmt.Errorf("%w: Forge action record requirement is invalid", mcphost.ErrFailClosed)
		}
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.ObjectName) == "" ||
			item.Name != strings.TrimSpace(item.Name) || item.ObjectName != strings.TrimSpace(item.ObjectName) {
			return nil, fmt.Errorf("%w: Forge action catalog contains an invalid action identity", mcphost.ErrFailClosed)
		}
		key := item.ObjectName + "." + item.Name
		if _, exists := visible[key]; exists {
			return nil, fmt.Errorf("%w: Forge action catalog contains a duplicate action", mcphost.ErrFailClosed)
		}
		visible[key] = item
	}

	requestedByKey := make(map[string]action, len(requested))
	objects := make(map[string]struct{})
	for _, capabilityID := range requested {
		requestedAction, err := parseAction(capabilityID)
		if err != nil {
			return nil, fmt.Errorf("%w: requested Forge action is invalid: %v", mcphost.ErrFailClosed, err)
		}
		key := requestedAction.objectName + "." + requestedAction.actionName
		if _, duplicate := requestedByKey[key]; duplicate {
			return nil, fmt.Errorf("%w: requested Forge action is duplicated", mcphost.ErrFailClosed)
		}
		if _, ok := visible[key]; !ok {
			return nil, fmt.Errorf("%w: published Forge action %q is unavailable to the current employee", mcphost.ErrFailClosed, capabilityID)
		}
		requestedByKey[key] = requestedAction
		objects[requestedAction.objectName] = struct{}{}
	}
	if len(requestedByKey) == 0 {
		return map[string]actionMetadata{}, nil
	}
	if metadataSource == nil {
		return nil, fmt.Errorf("%w: Forge declared action metadata is unavailable", mcphost.ErrFailClosed)
	}

	objectNames := make([]string, 0, len(objects))
	for objectName := range objects {
		objectNames = append(objectNames, objectName)
	}
	sort.Strings(objectNames)
	declarations := make(map[string]objectSchemaMetadata, len(objectNames))
	for _, objectName := range objectNames {
		object, err := metadataSource.ReadObjectMetadata(ctx, objectName)
		if err != nil {
			return nil, fmt.Errorf("%w: Forge declared metadata for %q is unavailable: %v", mcphost.ErrFailClosed, objectName, err)
		}
		declarations[objectName] = object
	}

	selectedDeclarations := make(map[string]actionDeclarationMetadata, len(requestedByKey))
	overrides := make(map[string]struct{})
	for key, requestedAction := range requestedByKey {
		object := declarations[requestedAction.objectName]
		var declaration *actionDeclarationMetadata
		for index := range object.Actions {
			if object.Actions[index].Name != requestedAction.actionName {
				continue
			}
			if declaration != nil {
				return nil, fmt.Errorf("%w: Forge declared action %q is ambiguous", mcphost.ErrFailClosed, key)
			}
			declaration = &object.Actions[index]
		}
		if declaration == nil {
			return nil, fmt.Errorf("%w: Forge declared action %q is missing", mcphost.ErrFailClosed, key)
		}
		selectedDeclarations[key] = *declaration
		for _, parameter := range declaration.Params {
			if parameter.Field != "" && parameter.ObjectOverride != "" {
				overrides[parameter.ObjectOverride] = struct{}{}
			}
		}
	}
	overrideNames := make([]string, 0, len(overrides))
	for objectName := range overrides {
		if _, alreadyRead := declarations[objectName]; !alreadyRead {
			overrideNames = append(overrideNames, objectName)
		}
	}
	sort.Strings(overrideNames)
	for _, objectName := range overrideNames {
		object, err := metadataSource.ReadObjectMetadata(ctx, objectName)
		if err != nil {
			return nil, fmt.Errorf("%w: Forge objectOverride metadata for %q is unavailable: %v", mcphost.ErrFailClosed, objectName, err)
		}
		declarations[objectName] = object
	}

	catalog := make(map[string]actionMetadata, len(requestedByKey))
	for key := range requestedByKey {
		merged, err := mergeDeclaredActionParameters(visible[key], selectedDeclarations[key], declarations)
		if err != nil {
			return nil, fmt.Errorf("%w: Forge declared parameters for %q are invalid: %v", mcphost.ErrFailClosed, key, err)
		}
		catalog[key] = merged
	}
	return catalog, nil
}

func validateActionSummaryParameters(raw json.RawMessage, params []actionParam) error {
	if len(raw) == 0 {
		if len(params) == 0 {
			return nil
		}
		return errors.New("parameter summaries are missing")
	}
	var summaries []json.RawMessage
	if err := json.Unmarshal(raw, &summaries); err != nil || summaries == nil {
		return errors.New("parameter summaries are not an array")
	}
	if len(summaries) != len(params) {
		return errors.New("parameter summary count is inconsistent")
	}
	for index, rawSummary := range summaries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawSummary, &fields); err != nil {
			return errors.New("parameter summary is invalid")
		}
		var name, parameterType string
		var required bool
		if err := json.Unmarshal(fields["name"], &name); err != nil || strings.TrimSpace(name) == "" ||
			json.Unmarshal(fields["type"], &parameterType) != nil || strings.TrimSpace(parameterType) == "" ||
			json.Unmarshal(fields["required"], &required) != nil {
			return errors.New("parameter summary is missing name, type, or required state")
		}
		if name != params[index].Name || parameterType != params[index].Type || required != params[index].Required {
			return fmt.Errorf("parameter summary %q does not match its decoded declaration", name)
		}
	}
	return nil
}

func mergeDeclaredActionParameters(summary actionMetadata, declaration actionDeclarationMetadata, objects map[string]objectSchemaMetadata) (actionMetadata, error) {
	declared := make(map[string]resolvedActionParameter, len(declaration.Params))
	for _, param := range declaration.Params {
		name := param.Name
		if name == "" {
			name = param.Field
		}
		// Match ObjectStack's declaration projection: unnamed parameters do not
		// become LLM-visible summary parameters.
		if name == "" {
			continue
		}
		if _, duplicate := declared[name]; duplicate {
			return actionMetadata{}, fmt.Errorf("declared parameter %q is duplicated", name)
		}
		parameterType := strings.TrimSpace(param.Type)
		fieldObject := objects[summary.ObjectName]
		var field actionFieldMetadata
		fieldFound := false
		if param.Field != "" {
			if param.ObjectOverride != "" {
				var overrideFound bool
				fieldObject, overrideFound = objects[param.ObjectOverride]
				if !overrideFound || fieldObject.Name != param.ObjectOverride {
					return actionMetadata{}, fmt.Errorf("objectOverride %q for parameter %q is unavailable to this employee", param.ObjectOverride, name)
				}
			}
			field, fieldFound = fieldObject.Fields[param.Field]
			if !fieldFound {
				return actionMetadata{}, fmt.Errorf("field-backed parameter %q cannot be resolved for this employee", name)
			}
			if parameterType == "" {
				parameterType = strings.TrimSpace(field.Type)
			}
		}
		if parameterType == "" {
			return actionMetadata{}, fmt.Errorf("declared parameter %q has no type", name)
		}
		multiple := false
		if param.Multiple != nil {
			multiple = *param.Multiple
		} else if param.Field != "" {
			if field.Multiple != nil {
				multiple = *field.Multiple
			}
		}
		enumOptions := param.Options
		if (len(enumOptions) == 0 || strings.TrimSpace(string(enumOptions)) == "null") && fieldFound {
			enumOptions = field.Options
		}
		enumValues, err := declaredEnumValues(enumOptions)
		if err != nil {
			return actionMetadata{}, fmt.Errorf("declared parameter %q has invalid options: %w", name, err)
		}
		declared[name] = resolvedActionParameter{
			Type: parameterType, Multiple: multiple, Enum: enumValues,
			UsesObjectOverride: param.Field != "" && param.ObjectOverride != "",
		}
	}
	if len(declared) != len(summary.Params) {
		return actionMetadata{}, fmt.Errorf("MCP summary declares %d parameters but native metadata declares %d", len(summary.Params), len(declared))
	}
	params := append([]actionParam(nil), summary.Params...)
	seen := make(map[string]struct{}, len(params))
	for index := range params {
		name := params[index].Name
		if name == "" {
			name = params[index].Field
		}
		if name == "" {
			return actionMetadata{}, errors.New("MCP summary contains an unnamed parameter")
		}
		if _, duplicate := seen[name]; duplicate {
			return actionMetadata{}, fmt.Errorf("MCP summary parameter %q is duplicated", name)
		}
		seen[name] = struct{}{}
		declaredParam, ok := declared[name]
		if !ok {
			return actionMetadata{}, fmt.Errorf("native declaration for parameter %q is missing", name)
		}
		expectedSummaryType := objectStackSummaryType(declaredParam.Type)
		if params[index].Type != expectedSummaryType && !declaredParam.UsesObjectOverride {
			return actionMetadata{}, fmt.Errorf("MCP summary type %q does not match declared type %q for parameter %q", params[index].Type, declaredParam.Type, name)
		}
		params[index].Type = expectedSummaryType
		if declaredParam.Type == "file" {
			params[index].Type = "file"
		}
		params[index].Multiple = declaredParam.Multiple
		if len(declaredParam.Enum) > 0 {
			params[index].Enum = declaredParam.Enum
		}
	}
	summary.Params = params
	return summary, nil
}

func declaredEnumValues(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var options []any
	if err := json.Unmarshal(raw, &options); err != nil {
		// The native MCP projection only treats an array as an enum source.
		return nil, nil
	}
	values := make([]string, 0, len(options))
	for _, option := range options {
		switch typed := option.(type) {
		case string:
			values = append(values, typed)
		case map[string]any:
			if value, ok := typed["value"].(string); ok {
				values = append(values, value)
			}
		}
	}
	return values, nil
}

func objectStackSummaryType(declaredType string) string {
	switch declaredType {
	case "number", "currency", "percent", "rating", "slider", "autonumber":
		return "number"
	case "boolean", "toggle":
		return "boolean"
	case "multiselect", "checkboxes", "tags":
		return "array"
	default:
		return "string"
	}
}
