package api

import "github.com/labstack/echo/v4"

// registerRetiredTeamConstructionRoutes registers the team template, team
// evaluation and team-build-run endpoints. These capabilities are retired: the
// supported client (the GooeyPi desktop with Forge) authors, trials and
// publishes teams through the team development workspace and never calls them.
//
// They stay registered only while Config.RetireLegacyPlatformAPIs is false, so
// existing deployments can be switched off first and the code deleted after a
// release without callers. Handlers and role requirements are unchanged.
func (s *Server) registerRetiredTeamConstructionRoutes(auth *echo.Group, orgScope echo.MiddlewareFunc) {
	auth.POST("/teams:from-template", s.handleCreateTeamFromTemplate, RequireRole("admin"), orgScope)
	auth.POST("/teams/:id/evaluations", s.handleEvaluateTeam, RequireRole("admin"), orgScope)
	auth.GET("/team-templates/samples", s.handleListTeamTemplateSamples, orgScope)
	auth.POST("/internal/team-build-runs", s.handleCreateTeamBuildRun, RequireRole("admin"), orgScope)
	auth.GET("/internal/team-build-runs", s.handleListBuildRuns, orgScope)
	auth.PUT("/team-build-runs/:id/blueprint", s.handlePlanTeamBlueprint, RequireRole("admin"), orgScope)
	auth.PUT("/internal/team-build-runs/:id/drafts", s.handleUpdateTeamBuildRunDrafts, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/authorize", s.handleAuthorizeBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/submit", s.handleSubmitBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/execute", s.handleExecuteTeamBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/cancel", s.handleCancelBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/rollback", s.handleRollbackBuildRun, RequireRole("admin"), orgScope)
	auth.GET("/internal/team-build-runs/:id/progress", s.handleGetBuildRunProgress, orgScope)
	auth.GET("/internal/team-build-runs/:id/rounds", s.handleListBuildRunRounds, orgScope)
	auth.GET("/internal/team-build-runs/:id/rounds/:n/report", s.handleGetBuildRunRoundReport, orgScope)
	auth.GET("/internal/team-build-runs/:id/usage", s.handleGetBuildRunUsage, orgScope)
	auth.GET("/internal/team-build-runs/:id", s.handleGetBuildRun, orgScope)
	auth.POST("/internal/team-build-runs/candidate-runs", s.handleCandidateTestRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/publish", s.handleCandidatePublish, RequireRole("admin"), orgScope)
}
