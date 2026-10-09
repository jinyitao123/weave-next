package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/labstack/echo/v4"
)

// adminEnvironment is the repository a team works on and the commands that
// verify its result. Runtime Hosts fetch the repository with their own git
// credentials unless the environment holds a server-side token, which nodes
// then use only through the task-scoped git proxy and never receive.
type adminEnvironment struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	RepositoryURL  string    `json:"repository_url"`
	DefaultBranch  string    `json:"default_branch"`
	SetupScript    string    `json:"setup_script"`
	VerifyCommands []string  `json:"verify_commands"`
	PushBranches   bool      `json:"push_branches"`
	DefaultTeamID  string    `json:"default_team_id"`
	GitUsername    string    `json:"git_username"`
	GitCredential  bool      `json:"git_credential"`
	UpdatedAt      time.Time `json:"updated_at"`
	// GitToken is write-only: absent keeps the stored token, empty clears it.
	GitToken *string `json:"git_token,omitempty"`
}

const (
	maxVerifyCommands      = 20
	maxVerifyCommandLength = 1000
)

var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

func validRepositoryURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 1024 || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil && hasPassword(parsed.User) {
		return false
	}
	switch parsed.Scheme {
	case "https", "http", "ssh", "git":
		return parsed.Host != ""
	case "file":
		return parsed.Path != ""
	}
	return false
}

func hasPassword(user *url.Userinfo) bool {
	_, set := user.Password()
	return set
}

func validGitRef(ref string) bool {
	return gitRefPattern.MatchString(ref) && !strings.Contains(ref, "..") && !strings.HasPrefix(ref, "-") &&
		!strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock")
}

func (environment *adminEnvironment) normalize() error {
	environment.Name = strings.TrimSpace(environment.Name)
	environment.RepositoryURL = strings.TrimSpace(environment.RepositoryURL)
	environment.DefaultBranch = strings.TrimSpace(environment.DefaultBranch)
	environment.DefaultTeamID = strings.TrimSpace(environment.DefaultTeamID)
	environment.GitUsername = strings.TrimSpace(environment.GitUsername)
	if environment.GitToken != nil {
		trimmed := strings.TrimSpace(*environment.GitToken)
		environment.GitToken = &trimmed
	}
	switch {
	case environment.Name == "" || len([]rune(environment.Name)) > 80:
		return errors.New("请填写不超过 80 字的环境名称")
	case !validRepositoryURL(environment.RepositoryURL):
		return errors.New("仓库地址无效；请使用 https、ssh 或 git@ 地址，不要在地址里写密码")
	case !validGitRef(environment.DefaultBranch):
		return errors.New("默认分支名称无效")
	case len(environment.SetupScript) > 8000:
		return errors.New("启动脚本过长")
	case len(environment.GitUsername) > 100 || strings.ContainsAny(environment.GitUsername, ":@ \t"):
		return errors.New("git 用户名无效")
	case environment.GitToken != nil && (len(*environment.GitToken) > 4096 || strings.ContainsAny(*environment.GitToken, " \t\r\n")):
		return errors.New("访问令牌无效")
	case environment.GitToken != nil && *environment.GitToken != "" && !gitProxyRepository(environment.RepositoryURL):
		return errors.New("使用服务端访问令牌时，仓库地址须为 https 地址")
	case len(environment.VerifyCommands) > maxVerifyCommands:
		return fmt.Errorf("验证命令最多 %d 条", maxVerifyCommands)
	}
	commands := make([]string, 0, len(environment.VerifyCommands))
	for _, command := range environment.VerifyCommands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		if len(command) > maxVerifyCommandLength || strings.ContainsAny(command, "\r\n") {
			return errors.New("每条验证命令应为一行，且不超过 1000 字符")
		}
		commands = append(commands, command)
	}
	environment.VerifyCommands = commands
	return nil
}

func scanEnvironment(row pgx.Row) (adminEnvironment, error) {
	var environment adminEnvironment
	var commands []byte
	err := row.Scan(&environment.ID, &environment.Name, &environment.RepositoryURL, &environment.DefaultBranch,
		&environment.SetupScript, &commands, &environment.PushBranches, &environment.DefaultTeamID, &environment.GitUsername, &environment.GitCredential, &environment.UpdatedAt)
	if err != nil {
		return environment, err
	}
	if err := json.Unmarshal(commands, &environment.VerifyCommands); err != nil {
		return environment, err
	}
	return environment, nil
}

const environmentColumns = `id, name, repository_url, default_branch, setup_script, verify_commands, push_branches, COALESCE(default_team_id,''), git_username, git_token_sealed IS NOT NULL, updated_at`

func (s *Server) handleListEnvironments(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "environment_store_unavailable"})
	}
	rows, err := pool.Query(c.Request().Context(), `SELECT `+environmentColumns+` FROM weave_environments WHERE workspace_id=$1 AND archived_at IS NULL ORDER BY name`, getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "environment_list_failed"})
	}
	defer rows.Close()
	environments := []adminEnvironment{}
	for rows.Next() {
		environment, err := scanEnvironment(rows)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "environment_list_failed"})
		}
		environments = append(environments, environment)
	}
	if rows.Err() != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "environment_list_failed"})
	}
	return c.JSON(http.StatusOK, map[string]any{"environments": environments})
}

func (s *Server) handleSaveEnvironment(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "environment_store_unavailable"})
	}
	var environment adminEnvironment
	if err := c.Bind(&environment); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	if err := environment.normalize(); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	}
	commands, _ := json.Marshal(environment.VerifyCommands)
	var team any
	if environment.DefaultTeamID != "" {
		team = environment.DefaultTeamID
	}
	// tokenChange: 0 keeps the stored token, 1 replaces it, 2 clears it.
	tokenChange, sealed := 0, ""
	if environment.GitToken != nil {
		tokenChange = 2
		if *environment.GitToken != "" {
			key, err := secret.KeyFromEnv()
			if err != nil {
				return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "服务端未配置凭据加密密钥，不能保存访问令牌"})
			}
			if sealed, err = secret.Seal(key, []byte(*environment.GitToken)); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "environment_save_failed"})
			}
			tokenChange = 1
		}
	}
	ctx := c.Request().Context()
	var row pgx.Row
	if id := c.Param("id"); id == "" {
		var token any
		if tokenChange == 1 {
			token = sealed
		}
		row = pool.QueryRow(ctx, `INSERT INTO weave_environments(workspace_id,id,name,repository_url,default_branch,setup_script,verify_commands,push_branches,default_team_id,created_by,git_username,git_token_sealed,git_token_set_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::text,CASE WHEN $12::text IS NULL THEN NULL ELSE NOW() END) RETURNING `+environmentColumns,
			getTenant(c), uuid.NewString(), environment.Name, environment.RepositoryURL, environment.DefaultBranch,
			environment.SetupScript, string(commands), environment.PushBranches, team, getUserID(c), environment.GitUsername, token)
	} else {
		// A token stays bound to the address it was saved for: changing the
		// repository address without a new token clears it.
		row = pool.QueryRow(ctx, `UPDATE weave_environments SET name=$3,repository_url=$4,default_branch=$5,setup_script=$6,verify_commands=$7,push_branches=$8,default_team_id=$9,git_username=$10,
				git_token_sealed=CASE WHEN $11=1 THEN $12 WHEN $11=2 OR repository_url<>$4 THEN NULL ELSE git_token_sealed END,
				git_token_set_at=CASE WHEN $11=1 THEN NOW() WHEN $11=2 OR repository_url<>$4 THEN NULL ELSE git_token_set_at END,
				updated_at=NOW()
			WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL RETURNING `+environmentColumns,
			getTenant(c), id, environment.Name, environment.RepositoryURL, environment.DefaultBranch,
			environment.SetupScript, string(commands), environment.PushBranches, team, environment.GitUsername, tokenChange, sealed)
	}
	saved, err := scanEnvironment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "environment_not_found"})
	}
	if err != nil {
		if strings.Contains(err.Error(), "weave_environments_active_name") {
			return c.JSON(http.StatusConflict, map[string]string{"error": "已有同名环境"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "environment_save_failed"})
	}
	return c.JSON(http.StatusOK, saved)
}

func (s *Server) handleArchiveEnvironment(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "environment_store_unavailable"})
	}
	tag, err := pool.Exec(c.Request().Context(), `UPDATE weave_environments SET archived_at=NOW(), updated_at=NOW() WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL`, getTenant(c), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "environment_archive_failed"})
	}
	if tag.RowsAffected() == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "environment_not_found"})
	}
	return c.NoContent(http.StatusNoContent)
}

type adminTaskSubmission struct {
	TeamID          string `json:"team_id"`
	EnvironmentID   string `json:"environment_id"`
	Ref             string `json:"ref"`
	Task            string `json:"task"`
	ClientRequestID string `json:"client_request_id"`
}

// handleSubmitAdminTask freezes the run's code context under the run identity
// the dispatch will use, then admits the task through the ordinary team
// dispatch. A replay with the same request finds the same context.
func (s *Server) handleSubmitAdminTask(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_submission_unavailable"})
	}
	var submission adminTaskSubmission
	if err := c.Bind(&submission); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	submission.TeamID, submission.EnvironmentID, submission.Ref = strings.TrimSpace(submission.TeamID), strings.TrimSpace(submission.EnvironmentID), strings.TrimSpace(submission.Ref)
	if submission.TeamID == "" || strings.TrimSpace(submission.Task) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "team_and_task_required"})
	}
	if _, err := uuid.Parse(submission.ClientRequestID); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_request_id"})
	}
	ctx := c.Request().Context()
	runID, _ := deterministicWorkflowDispatchIDs(getTenant(c), getUserID(c), submission.ClientRequestID)
	code, err := s.loadAdminCodeContext(ctx, getTenant(c), runID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return workflowStoreFailure(c, err)
	}
	if err == nil {
		if code.EnvironmentID != submission.EnvironmentID || code.RequestedRef != submission.Ref || code.ParentRunID != "" {
			return c.JSON(http.StatusConflict, map[string]string{"error": "client_request_id_reused"})
		}
	} else {
		code = &adminRunCodeContext{EnvironmentID: submission.EnvironmentID, RequestedRef: submission.Ref}
		if submission.EnvironmentID == "" && submission.Ref != "" {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "ref_requires_environment"})
		}
		if submission.EnvironmentID != "" {
			environment, err := scanEnvironment(pool.QueryRow(ctx, `SELECT `+environmentColumns+` FROM weave_environments WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL`, getTenant(c), submission.EnvironmentID))
			if errors.Is(err, pgx.ErrNoRows) {
				return c.JSON(http.StatusNotFound, map[string]string{"error": "environment_not_found"})
			}
			if err != nil {
				return workflowStoreFailure(c, err)
			}
			code.Ref = submission.Ref
			if code.Ref == "" {
				code.Ref = environment.DefaultBranch
			}
			if !validGitRef(code.Ref) {
				return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "分支名称无效"})
			}
			code.Repository, code.SetupScript, code.VerifyCommands = environment.RepositoryURL, environment.SetupScript, environment.VerifyCommands
			if environment.PushBranches {
				code.PushBranch = readableWeaveBranch(time.Now())
			}
		}
	}
	c.SetParamNames("id")
	c.SetParamValues(submission.TeamID)
	if ok, err := s.ensureTeamAvailable(c, getTenant(c), submission.TeamID); !ok {
		return err
	}
	return s.dispatchAdmittedTeam(c, teamDispatchRequest{Task: submission.Task, ClientRequestID: submission.ClientRequestID, codeContext: code})
}

// adminRunCodeContext is server-frozen code input. Even an empty context marks
// an explicit console submission without an environment in the request digest.
type adminRunCodeContext struct {
	EnvironmentID  string
	RequestedRef   string
	Repository     string
	Ref            string
	SetupScript    string
	VerifyCommands []string
	PushBranch     string `json:"-"`
	ParentRunID    string
	RootRunID      string
	SeedVersion    string
	SeedPatch      string
}

func (s *Server) loadAdminCodeContext(ctx context.Context, workspace, run string) (*adminRunCodeContext, error) {
	code := &adminRunCodeContext{}
	var commands []byte
	err := s.GetPool().QueryRow(ctx, `SELECT environment_id,requested_ref,repository_url,ref,setup_script,verify_commands,push_branch,parent_run_id,root_run_id,seed_version,seed_patch
 FROM weave_run_code_contexts WHERE workspace_id=$1 AND run_id=$2`, workspace, run).Scan(&code.EnvironmentID, &code.RequestedRef, &code.Repository, &code.Ref, &code.SetupScript, &commands, &code.PushBranch, &code.ParentRunID, &code.RootRunID, &code.SeedVersion, &code.SeedPatch)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(commands, &code.VerifyCommands); err != nil {
		return nil, err
	}
	return code, nil
}

// Freeze code in the same transaction as the existing workflow admission.
// Replays/conflicts are decided before this insert, so they cannot attach code
// to a plain request or leave orphan contexts after admission is refused.
func freezeAdminCodeContext(ctx context.Context, tx pgx.Tx, workspace, user, run string, code *adminRunCodeContext) error {
	if code == nil || code.EnvironmentID == "" {
		return nil
	}
	commands, err := json.Marshal(code.VerifyCommands)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_run_code_contexts(workspace_id,run_id,environment_id,requested_ref,repository_url,ref,setup_script,verify_commands,push_branch,created_by,parent_run_id,root_run_id,seed_version,seed_patch)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,$13,$14) ON CONFLICT (workspace_id,run_id) DO NOTHING`, workspace, run, code.EnvironmentID, code.RequestedRef, code.Repository, code.Ref, code.SetupScript, string(commands), code.PushBranch, user, code.ParentRunID, code.RootRunID, code.SeedVersion, code.SeedPatch)
	return err
}

// readableWeaveBranch names the branch a run's result is pushed to without
// exposing internal identifiers.
func readableWeaveBranch(now time.Time) string {
	const letters = "abcdefghjkmnpqrstuvwxyz23456789"
	suffix := make([]byte, 4)
	for index := range suffix {
		suffix[index] = letters[rand.IntN(len(letters))]
	}
	return "weave/" + now.UTC().Format("20060102-1504") + "-" + string(suffix)
}

// runCodeWorkspace returns the frozen code context of a run, or nil.
func (s *Server) runCodeWorkspace(ctx context.Context, workspaceID, runID string) (*runtimeprotocol.CodeWorkspace, error) {
	pool := s.GetPool()
	if pool == nil || runID == "" {
		return nil, nil
	}
	var code runtimeprotocol.CodeWorkspace
	var commands []byte
	var seedVersion, seedPatch string
	err := pool.QueryRow(ctx, `SELECT c.repository_url, c.ref, c.setup_script, c.verify_commands, c.push_branch, c.seed_version, c.seed_patch,
			COALESCE(e.archived_at IS NULL AND e.git_token_sealed IS NOT NULL AND e.repository_url=c.repository_url, false)
		FROM weave_run_code_contexts c LEFT JOIN weave_environments e ON e.workspace_id=c.workspace_id AND e.id=c.environment_id
		WHERE c.workspace_id=$1 AND c.run_id=$2`, workspaceID, runID).
		Scan(&code.Repository, &code.Ref, &code.SetupScript, &commands, &code.PushBranch, &seedVersion, &seedPatch, &code.Proxy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(commands, &code.VerifyCommands); err != nil {
		return nil, err
	}
	code.Proxy = code.Proxy && gitProxyRepository(code.Repository)
	if seedVersion != "" {
		code.Seed = &runtimeprotocol.CodeSeed{Version: seedVersion, Patch: seedPatch}
	}
	return &code, nil
}
