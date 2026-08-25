package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// UsageReport aggregates cost and token usage across all runs.
type UsageReport struct {
	TotalRuns      int          `json:"total_runs"`
	TotalCostUSD   float64      `json:"total_cost_usd"`
	TotalTokensIn  int          `json:"total_tokens_in"`
	TotalTokensOut int          `json:"total_tokens_out"`
	ByAgent        []AgentUsage `json:"by_agent"`
}

// AgentUsage is per-agent usage breakdown.
type AgentUsage struct {
	Agent     string  `json:"agent"`
	Runs      int     `json:"runs"`
	CostUSD   float64 `json:"cost_usd"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
}

func (s *Server) handleGetUsage(c echo.Context) error {
	tenant := getTenant(c)
	projectID := strings.TrimSpace(c.QueryParam("project_id"))
	selector, teamAware, selectorErr := parseTeamSelector(c.QueryParams(), "", true)
	if selectorErr != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_team_selector"})
	}
	if teamAware {
		return s.handleTeamAwareUsage(c, selector)
	}
	if projectID != "" {
		report, err := s.projectUsage(c.Request().Context(), tenant, projectID)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "project_usage_unavailable"})
		}
		return c.JSON(http.StatusOK, report)
	}
	ns := "audit:" + tenant

	// Use server-side aggregation if PGExt is available (avoids loading all rows).
	if s.StoreExt != nil {
		aggs, err := s.StoreExt.AggregateUsage(c.Request().Context(), ns)
		if err != nil {
			return c.JSON(http.StatusOK, UsageReport{ByAgent: []AgentUsage{}})
		}
		report := UsageReport{ByAgent: make([]AgentUsage, 0, len(aggs))}
		for _, a := range aggs {
			report.TotalRuns += a.Runs
			report.TotalCostUSD += a.CostUSD
			report.TotalTokensIn += a.TokensIn
			report.TotalTokensOut += a.TokensOut
			report.ByAgent = append(report.ByAgent, AgentUsage{
				Agent:     a.Agent,
				Runs:      a.Runs,
				CostUSD:   a.CostUSD,
				TokensIn:  a.TokensIn,
				TokensOut: a.TokensOut,
			})
		}
		return c.JSON(http.StatusOK, report)
	}

	// Fallback: in-memory aggregation (non-PG stores).
	keys, err := s.Store.List(c.Request().Context(), ns, "")
	if err != nil {
		return c.JSON(http.StatusOK, UsageReport{ByAgent: []AgentUsage{}})
	}

	agentMap := make(map[string]*AgentUsage)
	report := UsageReport{}

	for _, key := range keys {
		data, err := s.Store.Get(c.Request().Context(), ns, key)
		if err != nil {
			continue
		}
		var entry struct {
			Agent     string  `json:"agent"`
			TokensIn  int     `json:"tokens_in"`
			TokensOut int     `json:"tokens_out"`
			CostUSD   float64 `json:"cost_usd"`
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}

		report.TotalRuns++
		report.TotalCostUSD += entry.CostUSD
		report.TotalTokensIn += entry.TokensIn
		report.TotalTokensOut += entry.TokensOut

		agent := entry.Agent
		if agent == "" {
			agent = "unknown"
		}
		au, ok := agentMap[agent]
		if !ok {
			au = &AgentUsage{Agent: agent}
			agentMap[agent] = au
		}
		au.Runs++
		au.CostUSD += entry.CostUSD
		au.TokensIn += entry.TokensIn
		au.TokensOut += entry.TokensOut
	}

	for _, au := range agentMap {
		report.ByAgent = append(report.ByAgent, *au)
	}
	if report.ByAgent == nil {
		report.ByAgent = []AgentUsage{}
	}

	return c.JSON(http.StatusOK, report)
}

func (s *Server) projectUsage(
	ctx context.Context,
	workspaceID, projectID string,
) (UsageReport, error) {
	pool := s.GetPool()
	if pool == nil {
		return UsageReport{}, errTeamReadStoreUnavailable
	}
	rows, err := pool.Query(ctx, `
		SELECT
			COALESCE(stored.value->>'agent', 'unknown') AS agent,
			COUNT(*)::int AS runs,
			COALESCE(SUM((stored.value->>'tokens_in')::int), 0)::int AS tokens_in,
			COALESCE(SUM((stored.value->>'tokens_out')::int), 0)::int AS tokens_out,
			COALESCE(SUM((stored.value->>'cost_usd')::numeric), 0)::float8 AS cost_usd
		FROM (
			SELECT key, namespace, convert_from(value, 'UTF8')::jsonb AS value
			FROM loom_store
		) AS stored
		JOIN weave_run_terminal_markers AS marker
		  ON marker.workspace_id=$1 AND marker.run_id=stored.key
		WHERE stored.namespace=$2 AND marker.project_id=$3
		GROUP BY COALESCE(stored.value->>'agent', 'unknown')
		ORDER BY agent
	`, workspaceID, "audit:"+workspaceID, projectID)
	if err != nil {
		return UsageReport{}, err
	}
	defer rows.Close()
	report := UsageReport{ByAgent: []AgentUsage{}}
	for rows.Next() {
		var usage AgentUsage
		if err := rows.Scan(
			&usage.Agent, &usage.Runs, &usage.TokensIn, &usage.TokensOut, &usage.CostUSD,
		); err != nil {
			return UsageReport{}, err
		}
		report.TotalRuns += usage.Runs
		report.TotalTokensIn += usage.TokensIn
		report.TotalTokensOut += usage.TokensOut
		report.TotalCostUSD += usage.CostUSD
		report.ByAgent = append(report.ByAgent, usage)
	}
	return report, rows.Err()
}
