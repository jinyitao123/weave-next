package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

const workbenchContextFinalContentMax = 100_000

var errWorkbenchContextNotFound = errors.New("Workbench run context not found")

type workbenchContextResponse struct {
	Version string                 `json:"version"`
	Source  workbenchContextSource `json:"source"`
	Input   workbenchContextInput  `json:"input"`
	Run     workbenchContextRun    `json:"run"`
}

type workbenchContextSource struct {
	InputRevisionID    string `json:"input_revision_id"`
	RunID              string `json:"run_id"`
	WorkbenchSessionID string `json:"workbench_session_id"`
}

type workbenchContextInput struct {
	Task            string                       `json:"task"`
	TaskSHA256      string                       `json:"task_sha256"`
	TeamID          string                       `json:"team_id"`
	WorkflowID      string                       `json:"workflow_id"`
	WorkflowVersion int                          `json:"workflow_version"`
	Materials       []dispatchInputResource      `json:"materials"`
	SourceMessages  []dispatchInputSourceMessage `json:"source_messages"`
	BusinessRecord  *workbenchContextRecord      `json:"business_record,omitempty"`
	Parent          *workbenchContextParent      `json:"parent,omitempty"`
}

type workbenchContextRecord struct {
	ObjectName string `json:"object_name"`
	RecordID   string `json:"record_id"`
}

type workbenchContextParent struct {
	RootInputRevisionID   string `json:"root_input_revision_id"`
	ParentInputRevisionID string `json:"parent_input_revision_id,omitempty"`
	ParentRunID           string `json:"parent_run_id,omitempty"`
}

type workbenchContextRun struct {
	Status      string                            `json:"status"`
	FinalResult *workbenchContextFinalDeliverable `json:"final_result,omitempty"`
}

type workbenchContextFinalDeliverable struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
	SHA256      string `json:"sha256"`
}

type workbenchContextInputRow struct {
	InputRevisionID       string
	RunID                 string
	WorkbenchSessionID    string
	SourceMessages        []byte
	Task                  string
	TaskSHA256            string
	TeamID                string
	WorkflowID            string
	WorkflowVersion       int
	RootInputRevisionID   string
	ParentInputRevisionID string
	ParentRunID           string
	RevisionKind          string
	DelegatedResources    []byte
}

type workbenchContextDelegatedResource struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256"`
	ObjectName string `json:"object_name,omitempty"`
}

func (s *Server) handleGetWorkbenchRunContext(c echo.Context) error {
	workspaceID, userID, runID := getTenant(c), getUserID(c), strings.TrimSpace(c.Param("id"))
	if workspaceID == "" || userID == "" {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "workbench_context_identity_required"})
	}
	if runID == "" {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "workbench_context_not_found"})
	}
	if s.GetPool() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workbench_context_unavailable"})
	}

	run, owned, workbenchBound, err := s.workbenchRunAccess(c.Request().Context(), workspaceID, userID, runID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workbench_context_unavailable"})
	}
	if !workbenchBound || !owned {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "workbench_context_not_found"})
	}

	response, err := s.readWorkbenchRunContext(c.Request().Context(), workspaceID, userID, runID, run)
	if errors.Is(err, errWorkbenchContextNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "workbench_context_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workbench_context_unavailable"})
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Server) readWorkbenchRunContext(ctx context.Context, workspaceID, userID, runID string, run workbenchRunRecord) (workbenchContextResponse, error) {
	if s.GetPool() == nil || workspaceID == "" || userID == "" || runID == "" || run.RunID != runID {
		return workbenchContextResponse{}, errWorkbenchContextNotFound
	}
	rows, err := s.GetPool().Query(ctx, `SELECT input.input_revision_id,input.consumed_run_id,input.workbench_session_id,
		input.source_messages,input.task,input.task_sha256,input.team_id,input.workflow_id,input.workflow_version,
		input.root_input_revision_id,COALESCE(input.parent_input_revision_id,''),COALESCE(input.parent_run_id,''),input.revision_kind,
		delegation.resources
		FROM weave_dispatch_input_revisions AS input
		LEFT JOIN weave_task_business_delegations AS delegation
		  ON delegation.workspace_id=input.workspace_id AND delegation.user_id=input.user_id
		 AND delegation.input_revision_id=input.input_revision_id
		WHERE input.workspace_id=$1 AND input.user_id=$2 AND input.consumed_run_id=$3
		  AND input.project_id=$4
		ORDER BY input.consumed_at DESC,input.input_revision_id DESC
		LIMIT 2`, workspaceID, userID, runID, workbenchProjectID(userID))
	if err != nil {
		return workbenchContextResponse{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return workbenchContextResponse{}, err
		}
		return workbenchContextResponse{}, errWorkbenchContextNotFound
	}
	var input workbenchContextInputRow
	if err := rows.Scan(&input.InputRevisionID, &input.RunID, &input.WorkbenchSessionID, &input.SourceMessages,
		&input.Task, &input.TaskSHA256, &input.TeamID, &input.WorkflowID, &input.WorkflowVersion,
		&input.RootInputRevisionID, &input.ParentInputRevisionID, &input.ParentRunID, &input.RevisionKind,
		&input.DelegatedResources); err != nil {
		return workbenchContextResponse{}, err
	}
	if rows.Next() {
		return workbenchContextResponse{}, errWorkbenchContextNotFound
	}
	if err := rows.Err(); err != nil {
		return workbenchContextResponse{}, err
	}

	response, err := projectWorkbenchRunContext(input, run)
	if err != nil {
		return workbenchContextResponse{}, err
	}
	finalResult, err := s.readWorkbenchFinalResult(ctx, workspaceID, userID, runID)
	if err != nil {
		return workbenchContextResponse{}, err
	}
	response.Run.FinalResult = finalResult
	return response, nil
}

func projectWorkbenchRunContext(input workbenchContextInputRow, run workbenchRunRecord) (workbenchContextResponse, error) {
	if input.RunID == "" || len(input.RunID) > 128 || input.RunID != run.RunID ||
		strings.TrimSpace(input.WorkbenchSessionID) == "" || len(input.WorkbenchSessionID) > 256 ||
		input.WorkbenchSessionID != strings.TrimSpace(input.WorkbenchSessionID) ||
		strings.TrimSpace(input.Task) == "" || len(input.Task) > 1<<20 ||
		input.TaskSHA256 != dispatchInputDigest([]byte(input.Task)) ||
		strings.TrimSpace(input.TeamID) == "" || len(input.TeamID) > 128 || input.TeamID != strings.TrimSpace(input.TeamID) ||
		strings.TrimSpace(input.WorkflowID) == "" || len(input.WorkflowID) > 128 || input.WorkflowID != strings.TrimSpace(input.WorkflowID) ||
		input.WorkflowVersion < 1 {
		return workbenchContextResponse{}, errors.New("Workbench continuation input is invalid")
	}
	if _, err := uuid.Parse(input.InputRevisionID); err != nil {
		return workbenchContextResponse{}, errors.New("Workbench continuation revision is invalid")
	}
	if err := teamrun.ValidateStatus(teamrun.Status(run.Status)); err != nil {
		return workbenchContextResponse{}, err
	}
	var sourceMessages []dispatchInputSourceMessage
	if err := json.Unmarshal(input.SourceMessages, &sourceMessages); err != nil || !validDispatchInputSourceMessages(sourceMessages) {
		return workbenchContextResponse{}, errors.New("Workbench continuation source messages are invalid")
	}

	materials, record, err := projectWorkbenchContextResources(input.DelegatedResources, input.InputRevisionID, input.TaskSHA256)
	if err != nil {
		return workbenchContextResponse{}, err
	}
	rootRevisionID, err := uuid.Parse(input.RootInputRevisionID)
	if err != nil {
		return workbenchContextResponse{}, errors.New("Workbench continuation root revision is invalid")
	}
	parent := &workbenchContextParent{RootInputRevisionID: rootRevisionID.String()}
	switch input.RevisionKind {
	case "initial":
		if input.RootInputRevisionID != input.InputRevisionID || input.ParentInputRevisionID != "" || input.ParentRunID != "" {
			return workbenchContextResponse{}, errors.New("Workbench initial input lineage is invalid")
		}
	case "revision":
		parentRevisionID, parseErr := uuid.Parse(input.ParentInputRevisionID)
		if parseErr != nil || strings.TrimSpace(input.ParentRunID) == "" || input.ParentRunID != strings.TrimSpace(input.ParentRunID) || len(input.ParentRunID) > 128 {
			return workbenchContextResponse{}, errors.New("Workbench revision input lineage is invalid")
		}
		parent.ParentInputRevisionID, parent.ParentRunID = parentRevisionID.String(), input.ParentRunID
	default:
		return workbenchContextResponse{}, errors.New("Workbench input revision kind is invalid")
	}

	return workbenchContextResponse{
		Version: "1",
		Source: workbenchContextSource{
			InputRevisionID: input.InputRevisionID, RunID: input.RunID,
			WorkbenchSessionID: input.WorkbenchSessionID,
		},
		Input: workbenchContextInput{
			Task: input.Task, TaskSHA256: input.TaskSHA256, TeamID: input.TeamID,
			WorkflowID: input.WorkflowID, WorkflowVersion: input.WorkflowVersion,
			Materials: materials, SourceMessages: sourceMessages,
			BusinessRecord: record, Parent: parent,
		},
		Run: workbenchContextRun{Status: run.Status},
	}, nil
}

func projectWorkbenchContextResources(raw []byte, inputRevisionID, taskSHA256 string) ([]dispatchInputResource, *workbenchContextRecord, error) {
	materials := []dispatchInputResource{}
	if len(raw) == 0 {
		return materials, nil, nil
	}
	var resources []workbenchContextDelegatedResource
	if err := json.Unmarshal(raw, &resources); err != nil || resources == nil {
		return nil, nil, errors.New("Workbench delegated resources are invalid")
	}
	inputSeen := false
	var record *workbenchContextRecord
	for _, resource := range resources {
		switch resource.Type {
		case "dispatch-input":
			if inputSeen || resource.ID != inputRevisionID || resource.SHA256 != taskSHA256 ||
				resource.Name != "" || resource.Bytes != 0 || resource.ObjectName != "" {
				return nil, nil, errors.New("Workbench dispatch input resource does not match the run")
			}
			inputSeen = true
		case "forge-file":
			if resource.ID != strings.TrimSpace(resource.ID) || resource.Name != strings.TrimSpace(resource.Name) || resource.ObjectName != "" {
				return nil, nil, errors.New("Workbench Forge file resource is invalid")
			}
			materials = append(materials, dispatchInputResource{
				Type: resource.Type, ID: resource.ID, Name: resource.Name,
				Bytes: resource.Bytes, SHA256: resource.SHA256,
			})
		case "forge-record":
			if record != nil || resource.ID != strings.TrimSpace(resource.ID) ||
				resource.Name != "" || resource.Bytes != 0 ||
				!validDispatchBusinessRecord(&dispatchBusinessRecord{ObjectName: resource.ObjectName, RecordID: resource.ID}) {
				return nil, nil, errors.New("Workbench Forge record resource is invalid")
			}
			identity, _ := json.Marshal(map[string]string{"object_name": resource.ObjectName, "id": resource.ID})
			if resource.SHA256 != dispatchInputDigest(identity) {
				return nil, nil, errors.New("Workbench Forge record resource digest does not match")
			}
			record = &workbenchContextRecord{ObjectName: resource.ObjectName, RecordID: resource.ID}
		default:
			return nil, nil, errors.New("Workbench delegated resource type is unsupported")
		}
	}
	if !inputSeen || !validDispatchInputResources(materials) {
		return nil, nil, errors.New("Workbench delegated resources are incomplete")
	}
	return materials, record, nil
}

func (s *Server) readWorkbenchFinalResult(ctx context.Context, workspaceID, userID, runID string) (*workbenchContextFinalDeliverable, error) {
	var item workbenchContextFinalDeliverable
	var idRunes, titleRunes, contentTypeRunes, contentRunes int
	err := s.GetPool().QueryRow(ctx, `SELECT left(id,129),char_length(id),left(title,301),char_length(title),
		left(content_type,161),char_length(content_type),
		CASE WHEN char_length(content)<=$4 THEN content ELSE '' END,char_length(content)
		FROM weave_final_deliverables
		WHERE workspace_id=$1 AND user_id=$2 AND run_id=$3
		  AND COALESCE(metadata->>'artifact_kind','final')='final'
		  AND (btrim(content)<>'' OR COALESCE(metadata->>'filename','')<>'')
		ORDER BY created_at DESC,id DESC LIMIT 1`,
		workspaceID, userID, runID, workbenchContextFinalContentMax).Scan(
		&item.ID, &idRunes, &item.Title, &titleRunes, &item.ContentType, &contentTypeRunes, &item.Content, &contentRunes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if item.ID == "" || idRunes > 128 || item.Title == "" || titleRunes > 300 ||
		item.ContentType == "" || contentTypeRunes > 160 || contentRunes > workbenchContextFinalContentMax ||
		utf8.RuneCountInString(item.Content) != contentRunes {
		return nil, errors.New("Workbench final result is outside the continuation contract")
	}
	item.SHA256 = dispatchInputDigest([]byte(item.Content))
	return &item, nil
}
