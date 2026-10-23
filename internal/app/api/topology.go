package api

import (
	"net/http"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleTopology(c echo.Context) error {
	tenant := getTenant(c)
	name := c.Param("name")

	rec, err := s.Registry.Get(c.Request().Context(), tenant, name)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	// Compile the graph to get the real topology (the snapshot is never nil,
	// satisfying CompileAgent's non-nil LLM requirement).
	llm, err := s.llmFor(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	tools := s.buildAuditedMCPDispatcher(rec, tenant, "")
	compileOpts := compiler.CompileOpts{
		Store:                s.Store,
		SubAgentStepResolver: s.resolveSubAgent,
		AgentRunner:          s.compilerAgentRunner(tenant, getUserID(c), llm, nil),
	}

	prepared, compileErr := loomruntime.Prepare(loomruntime.RunRequest{
		Tenant: tenant,
		Agent:  rec,
		Dependencies: loomruntime.Dependencies{
			LLM:                llm,
			Tools:              tools,
			Store:              s.Store,
			SkillVersionReader: s.Skills,
			CompileOpts:        compileOpts,
		},
	})
	if compileErr != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": compileErr.Error()})
	}

	topo := prepared.Topology()
	if topo == nil {
		topo = []loom.StepInfo{}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"agent": name,
		"entry": prepared.Name(),
		"steps": topo,
	})
}
