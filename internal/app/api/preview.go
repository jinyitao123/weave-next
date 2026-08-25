package api

import (
	"context"
	"net/http"
	"unicode"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/labstack/echo/v4"
)

// EstimateTokens provides a more accurate token estimate for mixed CJK/English text.
func EstimateTokens(text string) int {
	cjk := 0
	ascii := 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			ascii++
		}
	}
	tokens := int(float64(cjk)*1.5) + ascii/4
	if tokens == 0 && len(text) > 0 {
		tokens = 1
	}
	return tokens
}

// PreviewPromptRequest is the input to the preview-prompt endpoint.
type PreviewPromptRequest struct {
	UserMessage string         `json:"user_message"`
	Profile     string         `json:"profile,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
}

// PreviewPromptResponse returns the assembled system prompt.
type PreviewPromptResponse struct {
	Prompt         string   `json:"prompt"`
	Tokens         int      `json:"tokens"`
	ActiveSkills   []string `json:"active_skills"`
	ProfileApplied string   `json:"profile_applied,omitempty"`
	ContextKeys    []string `json:"context_keys,omitempty"`
}

func (s *Server) handlePreviewPrompt(c echo.Context) error {
	tenant := getTenant(c)
	name := c.Param("name")

	rec, err := s.Registry.Get(c.Request().Context(), tenant, name)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	var req PreviewPromptRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.UserMessage == "" {
		req.UserMessage = "Hello"
	}

	// Resolve skills through the same shared live path CompileAgent uses, so
	// the preview shows exactly what a live standard/declarative compile would
	// inject, including exact registry_version SkillRefs. Work on a copy of
	// the fetched record.
	recCopy := *rec
	resolvedSkills, resolveErr := compiler.ResolveLiveSkills(
		c.Request().Context(), tenant, &recCopy, s.Store, s.Skills,
	)
	if resolveErr != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": resolveErr.Error()})
	}
	recCopy.Spec.Skills = resolvedSkills

	// Shared prompt preparation: identity fallback + grounding,
	// always-active/builtin injection, matchable subset, matcher selection.
	var embedder contract.Embedder
	if memSvc := s.memoryFor(c.Request().Context(), tenant); memSvc != nil {
		embedder = memSvc.Embedder()
	}
	prepared := compiler.PreparePrompt(&recCopy, embedder)

	// Run the PromptAssembleStep to get the compiled prompt.
	step := stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
		Identity:              prepared.Identity,
		Skills:                prepared.MatchableSkills,
		Profile:               req.Profile,
		ProfilesMap:           recCopy.Spec.Profiles,
		Context:               req.Context,
		MaxSystemPromptTokens: 8000,
		SkillMatcher:          prepared.Matcher,
	})

	result, err := step(context.Background(), loom.State{
		"last_user_message": req.UserMessage,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	prompt, _ := result["__system_prompt"].(string)
	activeSkills, _ := result["__active_skills"].([]string)
	if activeSkills == nil {
		activeSkills = []string{}
	}

	// Build response with verification feedback.
	resp := PreviewPromptResponse{
		Prompt:       prompt,
		Tokens:       EstimateTokens(prompt),
		ActiveSkills: activeSkills,
	}

	// Confirm profile was applied.
	if req.Profile != "" {
		if _, ok := rec.Spec.Profiles[req.Profile]; ok {
			resp.ProfileApplied = req.Profile
		}
	}

	// Confirm context keys were injected.
	if len(req.Context) > 0 {
		keys := make([]string, 0, len(req.Context))
		for k := range req.Context {
			keys = append(keys, k)
		}
		resp.ContextKeys = keys
	}

	return c.JSON(http.StatusOK, resp)
}
