package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

// A finished task whose result branch was pushed can open one pull request
// with the environment's server-held token. Follow-ups share the branch, so
// they share the pull request too. Only GitHub and GitHub Enterprise are
// supported.

type taskPullRequest struct {
	URL    string `json:"url"`
	Number int64  `json:"number"`
}

var (
	githubAPIBase = func(host string) string {
		if host == "github.com" {
			return "https://api.github.com"
		}
		return "https://" + host + "/api/v3"
	}
	githubClient = &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

func githubRepository(raw string) (host, owner, repository string, ok bool) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || strings.TrimSuffix(parts[1], ".git") == "" {
		return "", "", "", false
	}
	return parsed.Host, parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

func readPullRequest(ctx context.Context, pool *pgxpool.Pool, workspaceID, environmentID, branch string) (*taskPullRequest, error) {
	if environmentID == "" || branch == "" {
		return nil, nil
	}
	var pull taskPullRequest
	err := pool.QueryRow(ctx, `SELECT url, number FROM weave_environment_pull_requests WHERE workspace_id=$1 AND environment_id=$2 AND branch=$3`,
		workspaceID, environmentID, branch).Scan(&pull.URL, &pull.Number)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pull, nil
}

var verdictCopy = map[string]string{"passed": "验证通过", "failed": "验证未通过", "unknown": "无法判定", "not_declared": "未声明验证命令"}

func pullRequestBody(view taskCodeView) string {
	var body strings.Builder
	body.WriteString("由 Weave 团队任务生成。\n\n")
	if view.Final != nil {
		fmt.Fprintf(&body, "- 交付提交：`%s`\n", view.Final.Version.HeadSHA)
	}
	label := verdictCopy[view.Verdict.Status]
	if label == "" {
		label = "无法判定"
	}
	fmt.Fprintf(&body, "- 验证结论：%s\n", label)
	if view.Final != nil && view.Final.Evidence != nil && len(view.Final.Evidence.Commands) > 0 {
		body.WriteString("- 验证命令（由运行节点执行）：\n")
		for _, command := range view.Final.Evidence.Commands {
			outcome := fmt.Sprintf("退出码 %d", command.ExitCode)
			if command.TimedOut {
				outcome = "超时"
			}
			fmt.Fprintf(&body, "  - `%s` → %s\n", strings.ReplaceAll(command.Command, "`", "'"), outcome)
		}
	}
	return body.String()
}

func (s *Server) handleCreatePullRequest(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "pull_request_unavailable"})
	}
	ctx := c.Request().Context()
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: runID, Write: true})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pull_request_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	if !runTerminal(tasks[0].Status) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "任务结束后才能创建合并请求"})
	}
	view, found, err := loadTaskCode(ctx, pool, getTenant(c), runID, tasks[0].Status)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pull_request_failed"})
	}
	pushed := false
	for _, stage := range view.Stages {
		pushed = pushed || stage.Version.Push != nil && stage.Version.Push.Status == "pushed"
	}
	if !found || view.PushBranch == "" || view.Final == nil || !pushed || view.Final.Version.Push != nil && view.Final.Version.Push.Status == "failed" {
		return c.JSON(http.StatusConflict, map[string]string{"error": "这个任务没有推送到结果分支的代码"})
	}
	if existing, err := readPullRequest(ctx, pool, getTenant(c), view.environmentID, view.PushBranch); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pull_request_failed"})
	} else if existing != nil {
		return c.JSON(http.StatusOK, existing)
	}
	target, err := s.runGitProxyTarget(ctx, getTenant(c), runID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "pull_request_failed"})
	}
	if target == nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "环境没有保存服务端访问令牌，不能创建合并请求"})
	}
	host, owner, repository, ok := githubRepository(target.repository)
	if !ok {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "目前只支持 GitHub 仓库创建合并请求"})
	}
	base := view.Ref
	if view.RootRunID != "" {
		if err := pool.QueryRow(ctx, `SELECT ref FROM weave_run_code_contexts WHERE workspace_id=$1 AND run_id=$2`, getTenant(c), view.RootRunID).Scan(&base); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pull_request_failed"})
		}
	}
	title := strings.TrimSpace(tasks[0].Title)
	if title == "" {
		title = "Weave 团队任务结果"
	}
	api := githubAPIBase(host) + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/pulls"
	pull, message, err := githubCreatePull(ctx, api, target.token, owner, map[string]any{"title": title, "head": view.PushBranch, "base": base, "body": pullRequestBody(view)})
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": "无法连接 GitHub"})
	}
	if pull == nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": message})
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_environment_pull_requests(workspace_id,environment_id,branch,url,number,created_by) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT DO NOTHING`, getTenant(c), view.environmentID, view.PushBranch, pull.URL, pull.Number, getUserID(c)); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pull_request_failed"})
	}
	stored, err := readPullRequest(ctx, pool, getTenant(c), view.environmentID, view.PushBranch)
	if err != nil || stored == nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pull_request_failed"})
	}
	return c.JSON(http.StatusCreated, stored)
}

// githubCreatePull opens the pull request, or finds the open one GitHub
// already has for the branch. A nil pull comes with a message for people.
func githubCreatePull(ctx context.Context, api, token, owner string, request map[string]any) (*taskPullRequest, string, error) {
	call := func(method, target string, body any) (int, []byte, error) {
		var reader io.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		}
		outbound, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return 0, nil, err
		}
		outbound.Header.Set("Authorization", "Bearer "+token)
		outbound.Header.Set("Accept", "application/vnd.github+json")
		outbound.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if body != nil {
			outbound.Header.Set("Content-Type", "application/json")
		}
		response, err := githubClient.Do(outbound)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return response.StatusCode, raw, err
	}
	type githubPull struct {
		HTMLURL string `json:"html_url"`
		Number  int64  `json:"number"`
	}
	status, raw, err := call(http.MethodPost, api, request)
	if err != nil {
		return nil, "", err
	}
	switch status {
	case http.StatusCreated:
		var created githubPull
		if json.Unmarshal(raw, &created) != nil || created.HTMLURL == "" || created.Number < 1 {
			return nil, "GitHub 返回了无法识别的结果", nil
		}
		return &taskPullRequest{URL: created.HTMLURL, Number: created.Number}, "", nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "GitHub 拒绝了环境的访问令牌", nil
	case http.StatusNotFound:
		return nil, "找不到仓库，或访问令牌无权访问", nil
	case http.StatusUnprocessableEntity:
		query := url.Values{"head": {owner + ":" + fmt.Sprint(request["head"])}, "state": {"open"}}
		if listed, rawList, err := call(http.MethodGet, api+"?"+query.Encode(), nil); err == nil && listed == http.StatusOK {
			var open []githubPull
			if json.Unmarshal(rawList, &open) == nil && len(open) > 0 && open[0].HTMLURL != "" {
				return &taskPullRequest{URL: open[0].HTMLURL, Number: open[0].Number}, "", nil
			}
		}
		var failure struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(raw, &failure)
		detail := failure.Message
		if len(failure.Errors) > 0 && failure.Errors[0].Message != "" {
			detail = failure.Errors[0].Message
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return nil, strings.TrimSpace("GitHub 拒绝创建合并请求：" + detail), nil
	}
	return nil, fmt.Sprintf("GitHub 返回 HTTP %d", status), nil
}
