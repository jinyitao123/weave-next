package businessaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

const (
	frozenMaterialReadToolName = "read_frozen_material"
	frozenMaterialReadDefault  = 16 << 10
	frozenMaterialReadMax      = 32 << 10
	frozenExtractionMaxBytes   = 700_000
)

var frozenMaterialIDPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

type frozenMaterialReadScope struct {
	WorkspaceID     string
	UserID          string
	TaskID          string
	InputRevisionID string
	InputTaskSHA256 string
	RunID           string
	RunSnapshotID   string
	WorkflowID      string
	WorkflowVersion int
	ResourceDigest  string
	ExpiresAt       time.Time
}

type frozenMaterialReadFile struct {
	MaterialID string
	FileID     string
	Name       string
	MediaType  string
	Bytes      int64
	SHA256     string
	Extraction materialExtractionSnapshot
	Available  bool
	Reason     string
}

// FrozenMaterialResource is the request-bound Forge file metadata used to
// project a Workbench material task into a small Loom input.
type FrozenMaterialResource struct {
	Type       string
	MaterialID string
	FileID     string
	Name       string
	MediaType  string
	Bytes      int64
	SHA256     string
}

type taskMaterialEnvelope struct {
	Goal                 string                 `json:"goal"`
	MaterialHandling     string                 `json:"materialHandling"`
	BusinessDataHandling string                 `json:"businessDataHandling,omitempty"`
	BusinessSnapshot     json.RawMessage        `json:"businessSnapshot,omitempty"`
	Materials            []taskMaterialSnapshot `json:"materials"`
}

type taskMaterialSnapshot struct {
	MaterialID string                      `json:"materialId"`
	Name       string                      `json:"name"`
	MediaType  string                      `json:"mediaType"`
	Bytes      *int64                      `json:"bytes"`
	Size       *int64                      `json:"size"`
	SHA256     string                      `json:"sha256"`
	Extraction *materialExtractionSnapshot `json:"extraction"`
}

type materialExtractionSnapshot struct {
	Status       string          `json:"status"`
	MediaType    string          `json:"mediaType"`
	Bytes        *int64          `json:"bytes"`
	Size         *int64          `json:"size"`
	SHA256       string          `json:"sha256"`
	SourceSHA256 string          `json:"sourceSha256"`
	Content      string          `json:"content"`
	Extractor    string          `json:"extractor"`
	Coverage     json.RawMessage `json:"coverage"`
	Limitations  []string        `json:"limitations"`
}

type executionTaskMaterial struct {
	MaterialID string                      `json:"materialId,omitempty"`
	FileID     string                      `json:"fileId,omitempty"`
	Name       string                      `json:"name"`
	MediaType  string                      `json:"mediaType"`
	Bytes      *int64                      `json:"bytes,omitempty"`
	Size       *int64                      `json:"size,omitempty"`
	SHA256     string                      `json:"sha256"`
	Extraction executionMaterialExtraction `json:"extraction"`
}

type executionMaterialExtraction struct {
	Status       string          `json:"status"`
	MediaType    string          `json:"mediaType"`
	Bytes        *int64          `json:"bytes,omitempty"`
	Size         *int64          `json:"size,omitempty"`
	SHA256       string          `json:"sha256"`
	SourceSHA256 string          `json:"sourceSha256"`
	Extractor    string          `json:"extractor"`
	Coverage     json.RawMessage `json:"coverage"`
	Limitations  []string        `json:"limitations"`
}

type executionMaterialTask struct {
	Goal                     string                  `json:"goal"`
	MaterialHandling         string                  `json:"materialHandling,omitempty"`
	MaterialReadInstructions string                  `json:"materialReadInstructions"`
	BusinessDataHandling     string                  `json:"businessDataHandling,omitempty"`
	BusinessSnapshot         json.RawMessage         `json:"businessSnapshot,omitempty"`
	Materials                []executionTaskMaterial `json:"materials"`
}

type materialReadArguments struct {
	MaterialID string `json:"materialId"`
	FileID     string `json:"fileId"`
	SHA256     string `json:"sha256"`
	Offset     *int   `json:"offset,omitempty"`
	MaxBytes   *int   `json:"maxBytes,omitempty"`
}

type materialReadResult struct {
	Status           string          `json:"status"`
	Source           string          `json:"source,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	BinaryReadStatus string          `json:"binaryReadStatus"`
	BinaryReadCode   string          `json:"binaryReadCode"`
	MaterialID       string          `json:"materialId,omitempty"`
	SHA256           string          `json:"sha256,omitempty"`
	MediaType        string          `json:"mediaType,omitempty"`
	ExtractionStatus string          `json:"extractionStatus,omitempty"`
	ExtractionSHA256 string          `json:"extractionSha256,omitempty"`
	Extractor        string          `json:"extractor,omitempty"`
	Coverage         json.RawMessage `json:"coverage,omitempty"`
	Limitations      []string        `json:"limitations,omitempty"`
	Offset           int             `json:"offset,omitempty"`
	NextOffset       *int            `json:"nextOffset,omitempty"`
	HasMore          bool            `json:"hasMore,omitempty"`
	Content          string          `json:"content,omitempty"`
}

type materialReadDispatcher struct {
	store *Store
	scope frozenMaterialReadScope
	files map[string]frozenMaterialReadFile
	bound *mcphost.ToolContract
	tools []contract.ToolDef
}

const frozenMaterialReadInstructions = "材料正文不放在初始运行输入中。按 materials 中的 materialId 和 sha256 调用 read_frozen_material 分段读取；仅当文本材料没有 materialId 时，使用同条材料中的 fileId。status=partial、unsupported 或 unavailable 时如实说明缺口。工具仅返回与原件 sha256 绑定的桌面提取文本；binaryReadStatus=unavailable 表示 Weave 未读取 Forge 原始文件字节。工具片段按 UTF-8 字节偏移定位，不得编造页码或视觉内容。"

// PrepareExecutionTask keeps the frozen input unchanged for hashing/replay but
// removes extraction bodies from the Loom prompt. The scoped tool reads them
// from that exact input revision after verifying its run-bound Forge manifest.
func PrepareExecutionTask(task string, resources []FrozenMaterialResource) (string, bool, error) {
	var envelope taskMaterialEnvelope
	if err := json.Unmarshal([]byte(task), &envelope); err != nil || len(envelope.Materials) == 0 {
		return task, false, nil
	}
	recognized := false
	for _, material := range envelope.Materials {
		if material.Extraction != nil || isBinaryMaterialType(material.MediaType) {
			recognized = true
			break
		}
	}
	if !recognized {
		return task, false, nil
	}
	if strings.TrimSpace(envelope.Goal) == "" || len(resources) == 0 || len(resources) != len(envelope.Materials) {
		return "", true, errors.New("frozen material task does not match its Forge resource manifest")
	}
	delegated := make([]delegatedResource, 0, len(resources))
	for _, resource := range resources {
		if resource.Type != "forge-file" {
			return "", true, errors.New("frozen material resource type is unsupported")
		}
		delegated = append(delegated, delegatedResource{
			Type: resource.Type, MaterialID: resource.MaterialID, ID: resource.FileID,
			Name: resource.Name, MediaType: resource.MediaType, Bytes: resource.Bytes, SHA256: resource.SHA256,
		})
	}
	files := bindTaskMaterialExtractions(task, delegated, dispatchInputDigestBytes([]byte(task)))
	projection := executionMaterialTask{
		Goal: envelope.Goal, MaterialHandling: envelope.MaterialHandling,
		MaterialReadInstructions: frozenMaterialReadInstructions,
		BusinessDataHandling:     envelope.BusinessDataHandling, BusinessSnapshot: envelope.BusinessSnapshot,
		Materials: make([]executionTaskMaterial, 0, len(envelope.Materials)),
	}
	for _, resource := range resources {
		file, ok := files[resource.FileID]
		if !ok || !file.Available {
			return "", true, errors.New("frozen material extraction does not match its Forge file digest")
		}
		projection.Materials = append(projection.Materials, executionTaskMaterial{
			MaterialID: file.MaterialID, FileID: fallbackMaterialFileID(file.MaterialID, resource.FileID),
			Name: file.Name, MediaType: file.MediaType, Bytes: int64Pointer(resource.Bytes),
			SHA256: file.SHA256,
			Extraction: executionMaterialExtraction{
				Status: file.Extraction.Status, MediaType: file.Extraction.MediaType,
				Bytes: file.Extraction.Bytes, Size: file.Extraction.Size,
				SHA256: file.Extraction.SHA256, SourceSHA256: file.Extraction.SourceSHA256,
				Extractor: file.Extraction.Extractor, Coverage: file.Extraction.Coverage,
				Limitations: file.Extraction.Limitations,
			},
		})
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", true, err
	}
	return string(encoded), true, nil
}

func int64Pointer(value int64) *int64 { return &value }

func fallbackMaterialFileID(materialID, fileID string) string {
	if materialID != "" {
		return ""
	}
	return fileID
}

func supportedMaterialMediaType(value string) bool {
	switch value {
	case "text/plain", "text/markdown", "text/csv", "application/json", "application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return true
	default:
		return false
	}
}

func isBinaryMaterialType(value string) bool {
	return value == "application/pdf" || value == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}

// MaterialReadDispatcher exposes only extracted text already bound to the
// active Workbench input and TeamRun. Forge binary bytes are not fetched here.
func (s *Store) MaterialReadDispatcher(ctx context.Context) (contract.ToolDispatcher, error) {
	scope, resources, task, found, err := s.loadFrozenMaterialReadScope(ctx)
	if err != nil || !found {
		return nil, err
	}
	if len(resources) == 0 {
		return nil, nil
	}
	files := bindTaskMaterialExtractions(task, resources, scope.InputTaskSHA256)
	dispatcher, err := newMaterialReadDispatcher(s, scope, files)
	if err != nil {
		return nil, err
	}
	return dispatcher, nil
}

func (s *Store) loadFrozenMaterialReadScope(ctx context.Context) (frozenMaterialReadScope, []delegatedResource, string, bool, error) {
	if s == nil || s.pool == nil || s.tasks == nil {
		return frozenMaterialReadScope{}, nil, "", false, nil
	}
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok || current.Subject.UserID == "" {
		return frozenMaterialReadScope{}, nil, "", false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return frozenMaterialReadScope{}, nil, "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.tasks.ValidateCurrentTaskTx(ctx, tx); err != nil {
		return frozenMaterialReadScope{}, nil, "", false, err
	}
	var scope frozenMaterialReadScope
	var task, resourcesJSON []byte
	var inputRevisionID, inputWorkflowID, inputTeamID, inputConsumedRunID string
	var delegationWorkflowID, deliveryInputRevisionID, deliveryRunID, deliverySnapshotID, deliveryWorkflowID string
	var runID, runSnapshotID, runWorkflowID, runTeamID, runStatus, queueRunSnapshotID string
	var inputWorkflowVersion, delegationWorkflowVersion, deliveryWorkflowVersion, runWorkflowVersion int
	err = tx.QueryRow(ctx, `SELECT input.input_revision_id,input.task,input.task_sha256,input.workflow_id,input.workflow_version,
		input.team_id,input.consumed_run_id,delegation.resources,delegation.expires_at,delegation.workflow_id,
		delegation.workflow_version,delivery.input_revision_id,delivery.run_id,delivery.run_snapshot_id,
		delivery.workflow_id,delivery.workflow_version,run.run_id,run.run_snapshot_id,run.workflow_id,
		run.workflow_version,run.team_id,run.status,q.run_snapshot_id
		FROM weave_task_queue AS q
		JOIN weave_team_runs AS run
		  ON run.workspace_id=q.workspace_id AND run.run_snapshot_id=q.run_snapshot_id
		JOIN weave_run_delivery_state AS delivery
		  ON delivery.workspace_id=run.workspace_id AND delivery.run_snapshot_id=run.run_snapshot_id
		 AND delivery.run_id=run.run_id
		JOIN weave_dispatch_input_revisions AS input
		  ON input.workspace_id=delivery.workspace_id AND input.input_revision_id=delivery.input_revision_id
		 AND input.consumed_run_id=run.run_id
		JOIN weave_task_business_delegations AS delegation
		  ON delegation.workspace_id=input.workspace_id AND delegation.input_revision_id=input.input_revision_id
		WHERE q.workspace_id=$1 AND q.id=$2 AND delegation.user_id=$3 AND delegation.revoked_at IS NULL`,
		current.WorkspaceID, current.ID, current.Subject.UserID).Scan(
		&inputRevisionID, &task, &scope.InputTaskSHA256, &inputWorkflowID, &inputWorkflowVersion,
		&inputTeamID, &inputConsumedRunID, &resourcesJSON, &scope.ExpiresAt, &delegationWorkflowID,
		&delegationWorkflowVersion, &deliveryInputRevisionID, &deliveryRunID, &deliverySnapshotID,
		&deliveryWorkflowID, &deliveryWorkflowVersion, &runID, &runSnapshotID, &runWorkflowID,
		&runWorkflowVersion, &runTeamID, &runStatus, &queueRunSnapshotID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozenMaterialReadScope{}, nil, "", false, nil
	}
	if err != nil {
		return frozenMaterialReadScope{}, nil, "", false, err
	}
	if inputRevisionID == "" || inputRevisionID != deliveryInputRevisionID ||
		inputConsumedRunID != runID || deliveryRunID != runID || deliverySnapshotID != runSnapshotID ||
		queueRunSnapshotID != runSnapshotID || inputTeamID != runTeamID ||
		inputWorkflowID != runWorkflowID || delegationWorkflowID != runWorkflowID || deliveryWorkflowID != runWorkflowID ||
		inputWorkflowVersion != runWorkflowVersion || deliveryWorkflowVersion != runWorkflowVersion ||
		delegationWorkflowVersion != runWorkflowVersion || runStatus != "running" || !scope.ExpiresAt.After(s.now().UTC()) ||
		runID == "" || runSnapshotID == "" {
		return frozenMaterialReadScope{}, nil, "", false, nil
	}
	scope.InputRevisionID, scope.RunID, scope.RunSnapshotID = inputRevisionID, runID, runSnapshotID
	scope.WorkflowID, scope.WorkflowVersion = runWorkflowID, runWorkflowVersion
	scope.WorkspaceID, scope.UserID, scope.TaskID = current.WorkspaceID, current.Subject.UserID, current.ID
	scope.ResourceDigest = digestBytes(resourcesJSON)
	if dispatchInputDigestBytes(task) != scope.InputTaskSHA256 {
		// Keep a fail-closed reader so the caller gets a specific unavailable
		// state instead of receiving content whose owning task hash differs.
		task = nil
	}
	resources, decodeErr := decodeDelegatedResources(resourcesJSON, scope.InputRevisionID)
	if decodeErr != nil {
		return frozenMaterialReadScope{}, nil, "", false, fmt.Errorf("frozen material resources are invalid: %w", decodeErr)
	}
	if !delegatedInputMatches(resourcesJSON, scope.InputRevisionID, scope.InputTaskSHA256) {
		task = nil
	}
	if err := tx.Commit(ctx); err != nil {
		return frozenMaterialReadScope{}, nil, "", false, err
	}
	files := make([]delegatedResource, 0, len(resources))
	for _, resource := range resources {
		if resource.Type == "forge-file" {
			files = append(files, resource)
		}
	}
	return scope, files, string(task), len(files) > 0, nil
}

func (s *Store) frozenMaterialReadScopeActive(ctx context.Context, expected frozenMaterialReadScope) bool {
	scope, _, _, found, err := s.loadFrozenMaterialReadScope(ctx)
	return err == nil && found && sameFrozenMaterialReadScope(scope, expected)
}

func sameFrozenMaterialReadScope(left, right frozenMaterialReadScope) bool {
	return left.WorkspaceID == right.WorkspaceID && left.UserID == right.UserID && left.TaskID == right.TaskID &&
		left.InputRevisionID == right.InputRevisionID && left.InputTaskSHA256 == right.InputTaskSHA256 &&
		left.RunID == right.RunID && left.RunSnapshotID == right.RunSnapshotID &&
		left.WorkflowID == right.WorkflowID && left.WorkflowVersion == right.WorkflowVersion &&
		left.ResourceDigest == right.ResourceDigest && left.ExpiresAt.Equal(right.ExpiresAt)
}

func newMaterialReadDispatcher(store *Store, scope frozenMaterialReadScope, files map[string]frozenMaterialReadFile) (*materialReadDispatcher, error) {
	tool := contract.ToolDef{
		Name:        frozenMaterialReadToolName,
		Description: "按当前冻结运行中的 materialId 与原件 SHA-256 读取已授权提取文本；只有文本材料没有 materialId 时才使用 fileId。该工具不读取 Forge 二进制原件；返回的 PDF/DOCX 内容来自桌面 Host 的提取结果。offset/maxBytes 是 UTF-8 字节范围，不是页码；未提供页级文本时不得编造页码引文。",
		ReadOnly:    true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"materialId":{"type":"string","pattern":"^[0-9a-f]{24}$"},"fileId":{"type":"string","minLength":1,"maxLength":128},"sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},"offset":{"type":"integer","minimum":0},"maxBytes":{"type":"integer","minimum":1,"maximum":32768}},"required":["sha256"],"oneOf":[{"required":["materialId"]},{"required":["fileId"]}],"additionalProperties":false}`),
	}
	bound, err := mcphost.NewToolContract([]contract.ToolDef{tool})
	if err != nil {
		return nil, err
	}
	return &materialReadDispatcher{store: store, scope: scope, files: files, bound: bound, tools: []contract.ToolDef{tool}}, nil
}

func (d *materialReadDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return append([]contract.ToolDef(nil), d.tools...), nil
}

func (d *materialReadDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != frozenMaterialReadToolName {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "材料读取工具不可用", IsError: true}, nil
	}
	if rejected := d.bound.Validate(call); rejected != nil {
		rejected.ToolName = call.Name
		return rejected, nil
	}
	var args materialReadArguments
	decoder := json.NewDecoder(strings.NewReader(call.Args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil || !frozenSHA256.MatchString(args.SHA256) ||
		(args.MaterialID == "") == (args.FileID == "") ||
		args.MaterialID != "" && !frozenMaterialIDPattern.MatchString(args.MaterialID) ||
		args.FileID != "" && (len(args.FileID) > 128 || args.FileID != strings.TrimSpace(args.FileID)) {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "材料读取参数无效", IsError: true}, nil
	}
	result := materialReadResult{Status: "unavailable", BinaryReadStatus: "unavailable", BinaryReadCode: "forge_binary_read_not_registered"}
	if d.store == nil || !d.store.frozenMaterialReadScopeActive(ctx, d.scope) {
		result.Reason = "execution_scope_unavailable"
		return encodeMaterialReadResult(call.ID, result)
	}
	result, rangeErr := readFrozenMaterial(d.files, args)
	if rangeErr != nil {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "材料片段范围无效", IsError: true}, nil
	}
	return encodeMaterialReadResult(call.ID, result)
}

func readFrozenMaterial(files map[string]frozenMaterialReadFile, args materialReadArguments) (materialReadResult, error) {
	result := materialReadResult{Status: "unavailable", BinaryReadStatus: "unavailable", BinaryReadCode: "forge_binary_read_not_registered"}
	var file frozenMaterialReadFile
	if args.MaterialID != "" {
		matches := 0
		for _, candidate := range files {
			if candidate.MaterialID == args.MaterialID {
				file, matches = candidate, matches+1
			}
		}
		if matches != 1 {
			file = frozenMaterialReadFile{}
		}
	} else {
		file = files[args.FileID]
	}
	if file.FileID == "" || file.SHA256 != args.SHA256 {
		result.Reason = "material_not_in_current_run"
		return result, nil
	}
	result.MaterialID, result.SHA256, result.MediaType = file.MaterialID, file.SHA256, file.MediaType
	result.ExtractionStatus, result.ExtractionSHA256 = file.Extraction.Status, file.Extraction.SHA256
	result.Extractor, result.Coverage, result.Limitations = file.Extraction.Extractor, file.Extraction.Coverage, file.Extraction.Limitations
	if !file.Available {
		result.Reason = file.Reason
		return result, nil
	}
	if file.Extraction.Status != "complete" && file.Extraction.Status != "partial" {
		result.Reason = "frozen_extraction_unavailable"
		return result, nil
	}
	limit := frozenMaterialReadDefault
	if args.MaxBytes != nil {
		limit = *args.MaxBytes
	}
	offset := 0
	if args.Offset != nil {
		offset = *args.Offset
	}
	if offset < 0 || limit < 1 || limit > frozenMaterialReadMax {
		return materialReadResult{}, errors.New("invalid material text range")
	}
	chunk, next, more, err := materialTextRange(file.Extraction.Content, offset, limit)
	if err != nil {
		return materialReadResult{}, err
	}
	result.Status, result.Source, result.Offset = file.Extraction.Status, "workbench-extraction", offset
	result.Content, result.HasMore = chunk, more
	if more {
		result.NextOffset = &next
	}
	return result, nil
}

func encodeMaterialReadResult(callID string, value materialReadResult) (*contract.ToolResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &contract.ToolResult{CallID: callID, ToolName: frozenMaterialReadToolName, Content: string(encoded)}, nil
}

func materialTextRange(content string, offset, maxBytes int) (string, int, bool, error) {
	raw := []byte(content)
	if offset > len(raw) || offset < len(raw) && !utf8.RuneStart(raw[offset]) {
		return "", 0, false, errors.New("offset is not a UTF-8 boundary")
	}
	end := min(len(raw), offset+maxBytes)
	for end > offset && end < len(raw) && !utf8.RuneStart(raw[end]) {
		end--
	}
	if end == offset && offset < len(raw) {
		return "", 0, false, errors.New("range does not contain a UTF-8 character")
	}
	return string(raw[offset:end]), end, end < len(raw), nil
}

func bindTaskMaterialExtractions(task string, resources []delegatedResource, expectedTaskSHA string) map[string]frozenMaterialReadFile {
	files := make(map[string]frozenMaterialReadFile, len(resources))
	for _, resource := range resources {
		if resource.Type == "forge-file" {
			files[resource.ID] = frozenMaterialReadFile{
				MaterialID: resource.MaterialID, FileID: resource.ID, Name: resource.Name,
				MediaType: resource.MediaType, Bytes: resource.Bytes, SHA256: resource.SHA256,
				Reason: "frozen_extraction_unavailable",
			}
		}
	}
	if task == "" || dispatchInputDigestBytes([]byte(task)) != expectedTaskSHA {
		for key, file := range files {
			file.Reason = "frozen_input_digest_mismatch"
			files[key] = file
		}
		return files
	}
	var envelope taskMaterialEnvelope
	if err := json.Unmarshal([]byte(task), &envelope); err != nil || len(envelope.Materials) == 0 || len(envelope.Materials) > 8 {
		for key, file := range files {
			file.Reason = "frozen_extraction_manifest_unavailable"
			files[key] = file
		}
		return files
	}
	seenMaterialIDs := make(map[string]struct{}, len(envelope.Materials))
	invalid := false
	for _, material := range envelope.Materials {
		if !supportedMaterialMediaType(material.MediaType) || !frozenSHA256.MatchString(material.SHA256) ||
			materialByteSize(material) < 1 || material.Extraction == nil ||
			isBinaryMaterialType(material.MediaType) && !frozenMaterialIDPattern.MatchString(material.MaterialID) ||
			material.MaterialID != "" && !frozenMaterialIDPattern.MatchString(material.MaterialID) {
			invalid = true
		}
		if material.MaterialID != "" {
			if _, exists := seenMaterialIDs[material.MaterialID]; exists {
				invalid = true
			}
			seenMaterialIDs[material.MaterialID] = struct{}{}
		}
	}
	if invalid {
		for key, file := range files {
			file.Reason = "frozen_extraction_manifest_invalid"
			files[key] = file
		}
		return files
	}
	for key, file := range files {
		var material *taskMaterialSnapshot
		matches := 0
		for index := range envelope.Materials {
			candidate := &envelope.Materials[index]
			if file.MaterialID != "" && candidate.MaterialID != "" && file.MaterialID != candidate.MaterialID ||
				candidate.Name != file.Name || file.MediaType != "" && candidate.MediaType != file.MediaType ||
				materialSHA256(*candidate) != file.SHA256 || materialByteSize(*candidate) != file.Bytes {
				continue
			}
			material, matches = candidate, matches+1
		}
		if matches != 1 || material == nil || material.Extraction == nil {
			file.Reason = "frozen_material_manifest_mismatch"
			files[key] = file
			continue
		}
		if file.MaterialID == "" {
			file.MaterialID = material.MaterialID
		}
		if file.MediaType == "" {
			file.MediaType = material.MediaType
		}
		extraction := *material.Extraction
		file.Extraction = extraction
		if !validMaterialExtraction(extraction, file) {
			file.Reason = "frozen_extraction_digest_mismatch"
			files[key] = file
			continue
		}
		file.Available = true
		file.Reason = ""
		files[key] = file
	}
	return files
}

func materialSHA256(value taskMaterialSnapshot) string { return strings.TrimSpace(value.SHA256) }

func materialByteSize(value taskMaterialSnapshot) int64 {
	if value.Bytes != nil {
		if value.Size != nil && *value.Size != *value.Bytes {
			return -1
		}
		return *value.Bytes
	}
	if value.Size != nil {
		return *value.Size
	}
	return -1
}

func extractionByteSize(value materialExtractionSnapshot) int64 {
	if value.Bytes != nil {
		if value.Size != nil && *value.Size != *value.Bytes {
			return -1
		}
		return *value.Bytes
	}
	if value.Size != nil {
		return *value.Size
	}
	return -1
}

func validMaterialExtraction(value materialExtractionSnapshot, file frozenMaterialReadFile) bool {
	if value.MediaType != "text/plain; charset=utf-8" || value.SourceSHA256 != file.SHA256 ||
		!frozenSHA256.MatchString(value.SHA256) || extractionByteSize(value) != int64(len([]byte(value.Content))) ||
		extractionByteSize(value) < 0 || extractionByteSize(value) > frozenExtractionMaxBytes ||
		digestBytes([]byte(value.Content)) != value.SHA256 || len(value.Coverage) == 0 || !json.Valid(value.Coverage) ||
		value.Limitations == nil || value.Status == "complete" && value.Content == "" {
		return false
	}
	if value.Status != "complete" && value.Status != "partial" && value.Status != "unsupported" && value.Status != "pending" {
		return false
	}
	if value.Extractor != "utf8" && value.Extractor != "pdfjs-dist" && value.Extractor != "mammoth" {
		return false
	}
	if value.Status == "complete" && len(value.Limitations) != 0 {
		return false
	}
	for _, limitation := range value.Limitations {
		if limitation != "page-without-text" && limitation != "embedded-image" && limitation != "unsupported-document-content" {
			return false
		}
	}
	if file.MediaType == "application/pdf" && value.Extractor != "pdfjs-dist" ||
		file.MediaType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" && value.Extractor != "mammoth" ||
		!isBinaryMaterialType(file.MediaType) && value.Extractor != "utf8" {
		return false
	}
	return true
}

func dispatchInputDigestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func digestBytes(value []byte) string { return dispatchInputDigestBytes(value) }

func delegatedInputMatches(raw []byte, inputRevisionID, taskSHA256 string) bool {
	var resources []delegatedResource
	if err := json.Unmarshal(raw, &resources); err != nil {
		return false
	}
	for _, item := range resources {
		if item.Type == "dispatch-input" {
			return item.ID == inputRevisionID && item.SHA256 == taskSHA256
		}
	}
	return false
}
