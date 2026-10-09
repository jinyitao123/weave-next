package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/labstack/echo/v4"
)

// Code versions and verification evidence are written by runtime Hosts, never
// by members (see the daemon code workspace). The verdict is derived here by
// rule: a task's code is verified only when Host-run commands all succeeded on
// exactly the commit the last stage handed over.

type taskCodeFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

type taskCodeVersion struct {
	SchemaVersion int            `json:"schema_version"`
	BaseSHA       string         `json:"base_sha"`
	ParentSHA     string         `json:"parent_sha"`
	HeadSHA       string         `json:"head_sha"`
	TreeSHA       string         `json:"tree_sha"`
	NodeID        string         `json:"node_id"`
	Changed       bool           `json:"changed"`
	Files         []taskCodeFile `json:"files"`
	Patch         string         `json:"patch"`
	Push          *struct {
		Branch string `json:"branch"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	} `json:"push,omitempty"`
}

type taskCodeCommand struct {
	Command      string    `json:"command"`
	ExitCode     int       `json:"exit_code"`
	TimedOut     bool      `json:"timed_out,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int64     `json:"duration_ms"`
	OutputSHA256 string    `json:"output_sha256"`
	OutputBytes  int64     `json:"output_bytes"`
	OutputTail   string    `json:"output_tail"`
}

type taskCodeEvidence struct {
	SchemaVersion int               `json:"schema_version"`
	Commit        string            `json:"commit"`
	Tree          string            `json:"tree"`
	TreeUnchanged bool              `json:"tree_unchanged"`
	NodeID        string            `json:"node_id"`
	ReusedFrom    string            `json:"reused_from,omitempty"`
	Setup         *taskCodeCommand  `json:"setup,omitempty"`
	Commands      []taskCodeCommand `json:"commands"`
}

type taskCodeStage struct {
	NodeID      string            `json:"node_id"`
	CompletedAt time.Time         `json:"completed_at"`
	Version     taskCodeVersion   `json:"version"`
	Evidence    *taskCodeEvidence `json:"evidence,omitempty"`
	// Passes counts completed runs of this stage; a verify loop sends work
	// back to the same stage, and the latest pass is kept.
	Passes     int `json:"passes"`
	rawVersion string
}

type taskCodeVerdict struct {
	// passed | failed | unknown | not_declared | pending
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Commit string `json:"commit,omitempty"`
}

type taskCodeView struct {
	Repository     string           `json:"repository"`
	Ref            string           `json:"ref"`
	PushBranch     string           `json:"push_branch,omitempty"`
	VerifyCommands []string         `json:"verify_commands"`
	Stages         []taskCodeStage  `json:"stages"`
	Final          *taskCodeStage   `json:"final,omitempty"`
	Patch          string           `json:"patch,omitempty"`
	Verdict        taskCodeVerdict  `json:"verdict"`
	ParentRunID    string           `json:"parent_run_id,omitempty"`
	RootRunID      string           `json:"root_run_id,omitempty"`
	PullRequest    *taskPullRequest `json:"pull_request,omitempty"`
	environmentID  string
}

func (s *Server) handleGetAdminTaskCode(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_code_unavailable"})
	}
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, "", time.Now().Add(time.Minute), 1, runID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_code_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	view, found, err := loadTaskCode(c.Request().Context(), pool, getTenant(c), runID, tasks[0].Status)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_code_failed"})
	}
	if !found {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_has_no_code"})
	}
	return c.JSON(http.StatusOK, view)
}

// loadTaskCode reads a run's frozen code context and the Host-written version,
// patch and evidence of each completed stage.
func loadTaskCode(ctx context.Context, pool *pgxpool.Pool, workspaceID, runID, runStatus string) (taskCodeView, bool, error) {
	var view taskCodeView
	var commands []byte
	err := pool.QueryRow(ctx, `SELECT repository_url, ref, push_branch, verify_commands, environment_id, parent_run_id, root_run_id
		FROM weave_run_code_contexts WHERE workspace_id=$1 AND run_id=$2`, workspaceID, runID).
		Scan(&view.Repository, &view.Ref, &view.PushBranch, &commands, &view.environmentID, &view.ParentRunID, &view.RootRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, false, nil
	}
	if err != nil {
		return view, false, err
	}
	if err := json.Unmarshal(commands, &view.VerifyCommands); err != nil {
		return view, false, err
	}
	rows, err := pool.Query(ctx, `SELECT COALESCE(payload->>'node_id',''), COALESCE(result->'artifacts','[]'::jsonb), completed_at
		FROM weave_task_queue
		WHERE workspace_id=$1 AND run_snapshot_id=$2 AND kind='engine_exec' AND status='completed' AND completed_at IS NOT NULL
		ORDER BY completed_at, id`, workspaceID, runID)
	if err != nil {
		return view, false, err
	}
	defer rows.Close()
	latest := map[string]int{}
	patches := map[string]string{}
	finalNode := ""
	view.Stages = []taskCodeStage{}
	for rows.Next() {
		var nodeID string
		var raw []byte
		var stage taskCodeStage
		if err := rows.Scan(&nodeID, &raw, &stage.CompletedAt); err != nil {
			return view, false, err
		}
		var artifacts []engine.Artifact
		if json.Unmarshal(raw, &artifacts) != nil {
			continue
		}
		found := false
		for _, artifact := range artifacts {
			switch artifact.Path {
			case "code/version.json":
				found = json.Unmarshal([]byte(artifact.Content), &stage.Version) == nil && stage.Version.SchemaVersion == 1
				stage.rawVersion = artifact.Content
			case "code/evidence.json":
				var evidence taskCodeEvidence
				if json.Unmarshal([]byte(artifact.Content), &evidence) == nil && evidence.SchemaVersion == 1 {
					stage.Evidence = &evidence
				}
			case "code/changes.patch":
				patches[nodeID] = artifact.Content
			}
		}
		if !found {
			continue
		}
		stage.NodeID = nodeID
		stage.Passes = 1
		finalNode = nodeID
		// A later pass of the same stage replaces the earlier one.
		if index, exists := latest[nodeID]; exists {
			stage.Passes = view.Stages[index].Passes + 1
			view.Stages[index] = stage
			continue
		}
		latest[nodeID] = len(view.Stages)
		view.Stages = append(view.Stages, stage)
	}
	if err := rows.Err(); err != nil {
		return view, false, err
	}
	if len(view.Stages) > 0 {
		// Stages are read in completion order, so the final version is the
		// one completed last, wherever its stage sits in the list.
		final := view.Stages[latest[finalNode]]
		view.Final = &final
		if final.Version.Patch == "complete" {
			view.Patch = patches[final.NodeID]
		}
	}
	view.Verdict = codeVerdict(runStatus, view)
	if view.PullRequest, err = readPullRequest(ctx, pool, workspaceID, view.environmentID, view.PushBranch); err != nil {
		return view, false, err
	}
	return view, true, nil
}

func codeVerdict(runStatus string, view taskCodeView) taskCodeVerdict {
	switch runStatus {
	case "queued", "running", "parked", "cancel_requested":
		return taskCodeVerdict{Status: "pending"}
	}
	if view.Final == nil {
		return taskCodeVerdict{Status: "unknown", Reason: "no_code_version"}
	}
	head := view.Final.Version.HeadSHA
	if len(view.VerifyCommands) == 0 {
		return taskCodeVerdict{Status: "not_declared", Reason: "no_verify_commands", Commit: head}
	}
	evidence := view.Final.Evidence
	if evidence == nil {
		return taskCodeVerdict{Status: "unknown", Reason: "no_evidence", Commit: head}
	}
	if evidence.Commit != head {
		return taskCodeVerdict{Status: "unknown", Reason: "evidence_commit_mismatch", Commit: head}
	}
	if evidence.Tree == "" || evidence.Tree != view.Final.Version.TreeSHA || !evidence.TreeUnchanged {
		return taskCodeVerdict{Status: "unknown", Reason: "evidence_tree_mismatch", Commit: head}
	}
	if len(evidence.Commands) != len(view.VerifyCommands) {
		return taskCodeVerdict{Status: "unknown", Reason: "evidence_incomplete", Commit: head}
	}
	for index, command := range evidence.Commands {
		if command.Command != view.VerifyCommands[index] {
			return taskCodeVerdict{Status: "unknown", Reason: "evidence_commands_mismatch", Commit: head}
		}
		if command.ExitCode != 0 || command.TimedOut {
			return taskCodeVerdict{Status: "failed", Reason: "command_failed", Commit: head}
		}
	}
	return taskCodeVerdict{Status: "passed", Commit: head}
}
