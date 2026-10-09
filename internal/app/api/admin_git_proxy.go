package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

// The git proxy lets a node fetch and push a run's repository with the
// environment's server-held credential. It serves only git smart HTTP for the
// repository frozen into the run, only while this runtime holds the task, and
// a push may update only the run's own result branch.

const (
	gitProxyDefaultUsername = "x-access-token"
	gitProxyMaxRequestBytes = 1 << 30
	gitProxyMaxCommandBytes = 1 << 20
)

var gitProxyClient = &http.Client{
	// Credentials must never follow a redirect to another location.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	},
}

type gitProxyTarget struct {
	repository string
	pushBranch string
	username   string
	token      string
}

// gitProxyRepository reports whether a repository URL can be reached through
// the proxy.
func gitProxyRepository(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil
}

// runGitProxyTarget resolves the credential for a run's frozen repository. It
// is nil when the environment holds no credential or now points elsewhere.
func (s *Server) runGitProxyTarget(ctx context.Context, workspaceID, runID string) (*gitProxyTarget, error) {
	pool := s.GetPool()
	if pool == nil || runID == "" {
		return nil, nil
	}
	var target gitProxyTarget
	var sealed string
	err := pool.QueryRow(ctx, `SELECT c.repository_url, c.push_branch, e.git_username, e.git_token_sealed
		FROM weave_run_code_contexts c
		JOIN weave_environments e ON e.workspace_id=c.workspace_id AND e.id=c.environment_id
		WHERE c.workspace_id=$1 AND c.run_id=$2 AND e.archived_at IS NULL AND e.git_token_sealed IS NOT NULL AND e.repository_url=c.repository_url`,
		workspaceID, runID).Scan(&target.repository, &target.pushBranch, &target.username, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !gitProxyRepository(target.repository) {
		return nil, nil
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		return nil, err
	}
	token, err := secret.Open(key, sealed)
	if err != nil {
		return nil, err
	}
	target.token = string(token)
	if target.username == "" {
		target.username = gitProxyDefaultUsername
	}
	return &target, nil
}

func (s *Server) handleRuntimeGitProxy(c echo.Context) error {
	_, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return err
	}
	if task.Status != taskqueue.StatusRunning {
		return echo.NewHTTPError(http.StatusConflict, "task is not running")
	}
	ctx := c.Request().Context()
	target, err := s.runGitProxyTarget(ctx, task.WorkspaceID, task.RunSnapshotID)
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "repository credential unavailable")
	}
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, "repository not proxied for this task")
	}
	request := c.Request()
	path := c.Param("*")
	service := ""
	switch {
	case request.Method == http.MethodGet && path == "info/refs":
		service = request.URL.Query().Get("service")
	case request.Method == http.MethodPost && (path == "git-upload-pack" || path == "git-receive-pack"):
		service = path
	default:
		return echo.NewHTTPError(http.StatusNotFound, "unsupported git request")
	}
	switch service {
	case "git-upload-pack":
	case "git-receive-pack":
		if target.pushBranch == "" {
			return echo.NewHTTPError(http.StatusForbidden, "this run does not push")
		}
	default:
		return echo.NewHTTPError(http.StatusNotFound, "unsupported git service")
	}

	body := io.Reader(http.MaxBytesReader(c.Response(), request.Body, gitProxyMaxRequestBytes))
	encoding := request.Header.Get("Content-Encoding")
	if request.Method == http.MethodPost && path == "git-receive-pack" {
		if encoding == "gzip" {
			unzipped, err := gzip.NewReader(body)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "invalid push body")
			}
			defer unzipped.Close()
			body, encoding = unzipped, ""
		} else if encoding != "" {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "unsupported push encoding")
		}
		checked, err := allowOnlyRefUpdate(body, "refs/heads/"+target.pushBranch)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		body = checked
	}

	upstream := strings.TrimRight(target.repository, "/") + "/" + path
	if request.URL.RawQuery != "" {
		upstream += "?" + url.Values{"service": {service}}.Encode()
	}
	outbound, err := http.NewRequestWithContext(ctx, request.Method, upstream, body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "repository unreachable")
	}
	for _, header := range []string{"Content-Type", "Accept", "Git-Protocol", "User-Agent"} {
		if value := request.Header.Get(header); value != "" {
			outbound.Header.Set(header, value)
		}
	}
	if encoding != "" {
		outbound.Header.Set("Content-Encoding", encoding)
	}
	// The command check forwards the same bytes, so the length still holds
	// unless the body was decompressed.
	if encoding == request.Header.Get("Content-Encoding") {
		outbound.ContentLength = request.ContentLength
	}
	outbound.SetBasicAuth(target.username, target.token)
	response, err := gitProxyClient.Do(outbound)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "repository unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return echo.NewHTTPError(http.StatusBadGateway, "repository redirected; update the environment address")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		// The node must not be prompted for credentials it does not hold.
		return echo.NewHTTPError(http.StatusBadGateway, "repository rejected the environment credential")
	}
	for _, header := range []string{"Content-Type", "Content-Encoding", "Cache-Control", "Expires", "Pragma"} {
		if value := response.Header.Get(header); value != "" {
			c.Response().Header().Set(header, value)
		}
	}
	c.Response().WriteHeader(response.StatusCode)
	_, err = io.Copy(flushingWriter{c.Response()}, response.Body)
	return err
}

type flushingWriter struct{ response *echo.Response }

func (writer flushingWriter) Write(data []byte) (int, error) {
	count, err := writer.response.Write(data)
	writer.response.Flush()
	return count, err
}

// allowOnlyRefUpdate reads the receive-pack command list and refuses any
// update other than the allowed ref. It returns the full body for forwarding.
func allowOnlyRefUpdate(body io.Reader, allowed string) (io.Reader, error) {
	reader := bufio.NewReader(body)
	var consumed bytes.Buffer
	updates := 0
	for {
		length := make([]byte, 4)
		if _, err := io.ReadFull(reader, length); err != nil {
			return nil, errors.New("invalid push request")
		}
		consumed.Write(length)
		size, err := strconv.ParseUint(string(length), 16, 16)
		if err != nil {
			return nil, errors.New("invalid push request")
		}
		if size == 0 {
			break
		}
		if size < 4 || consumed.Len()+int(size) > gitProxyMaxCommandBytes {
			return nil, errors.New("invalid push request")
		}
		line := make([]byte, size-4)
		if _, err := io.ReadFull(reader, line); err != nil {
			return nil, errors.New("invalid push request")
		}
		consumed.Write(line)
		text := strings.TrimSuffix(string(line), "\n")
		if strings.HasPrefix(text, "shallow ") {
			continue
		}
		command, _, _ := strings.Cut(text, "\x00")
		fields := strings.Fields(command)
		if len(fields) != 3 {
			return nil, errors.New("invalid push request")
		}
		if fields[2] != allowed {
			return nil, fmt.Errorf("push may only update %s", strings.TrimPrefix(allowed, "refs/heads/"))
		}
		updates++
	}
	if updates == 0 {
		return nil, errors.New("invalid push request")
	}
	return io.MultiReader(&consumed, reader), nil
}
