package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jinyitao123/loom"
	importskills "github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/labstack/echo/v4"
)

// Skill is a reusable prompt module that can be attached to any agent.
type Skill struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Body         string    `json:"body"`                    // prompt text injected into system prompt
	AlwaysActive bool      `json:"always_active,omitempty"` // if true, body always injected (no matching)
	Category     string    `json:"category,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const skillNamespace = "skill"

func skillKey(tenant, id string) (string, string) {
	return skillNamespace + ":" + tenant, id
}

func (s *Server) handleListSkills(c echo.Context) error {
	tenant := getTenant(c)
	ns := skillNamespace + ":" + tenant
	keys, err := s.Store.List(c.Request().Context(), ns, "")
	if err != nil {
		return c.JSON(http.StatusOK, []Skill{})
	}
	var skills []Skill
	for _, key := range keys {
		data, err := s.Store.Get(c.Request().Context(), ns, key)
		if err != nil {
			continue
		}
		var sk Skill
		if json.Unmarshal(data, &sk) == nil {
			skills = append(skills, sk)
		}
	}
	if skills == nil {
		skills = []Skill{}
	}
	return c.JSON(http.StatusOK, skills)
}

func (s *Server) handleGetSkill(c echo.Context) error {
	tenant := getTenant(c)
	id := c.Param("id")
	ns, key := skillKey(tenant, id)
	data, err := s.Store.Get(c.Request().Context(), ns, key)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "skill not found"})
	}
	var sk Skill
	if err := json.Unmarshal(data, &sk); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "corrupt skill data"})
	}
	return c.JSON(http.StatusOK, sk)
}

func (s *Server) handleCreateSkill(c echo.Context) error {
	tenant := getTenant(c)
	var sk Skill
	if err := c.Bind(&sk); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if sk.ID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
	}
	if sk.Body == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "body is required"})
	}
	sk.CreatedAt = time.Now()
	sk.UpdatedAt = sk.CreatedAt

	if err := putSkill(c.Request().Context(), s.Store, tenant, &sk); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, sk)
}

func (s *Server) handleUpdateSkill(c echo.Context) error {
	tenant := getTenant(c)
	id := c.Param("id")
	var sk Skill
	if err := c.Bind(&sk); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	sk.ID = id
	sk.UpdatedAt = time.Now()

	// Preserve created_at from existing
	ns, key := skillKey(tenant, id)
	if data, err := s.Store.Get(c.Request().Context(), ns, key); err == nil {
		var existing Skill
		if json.Unmarshal(data, &existing) == nil {
			sk.CreatedAt = existing.CreatedAt
		}
	}
	if sk.CreatedAt.IsZero() {
		sk.CreatedAt = sk.UpdatedAt
	}

	if err := putSkill(c.Request().Context(), s.Store, tenant, &sk); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, sk)
}

func (s *Server) handleDeleteSkill(c echo.Context) error {
	tenant := getTenant(c)
	id := c.Param("id")
	ns, key := skillKey(tenant, id)
	if err := s.Store.Delete(c.Request().Context(), ns, key); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "skill not found"})
	}
	return c.JSON(http.StatusNoContent, nil)
}

func (s *Server) handleImportLegacySkill(c echo.Context) error {
	if s.SkillImporter == nil {
		return skillImportError(c, http.StatusServiceUnavailable, importskills.CodeSkillImportUnavailable)
	}
	var request importskills.ImportRequest
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return skillImportError(c, http.StatusBadRequest, importskills.CodeSkillImportInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return skillImportError(c, http.StatusBadRequest, importskills.CodeSkillImportInvalid)
	}
	response, err := s.SkillImporter.Import(
		c.Request().Context(), getTenant(c), c.Param("id"), getUserID(c), request,
	)
	if err == nil {
		return c.JSON(http.StatusOK, response)
	}
	code := importskills.ImportCode(err)
	status := http.StatusInternalServerError
	switch code {
	case importskills.CodeSkillImportLegacyNotFound:
		status = http.StatusNotFound
	case importskills.CodeSkillImportInvalid:
		status = http.StatusBadRequest
	case importskills.CodeSkillImportSourceChanged,
		importskills.CodeSkillImportIdempotencyConflict:
		status = http.StatusConflict
	case importskills.CodeSkillImportUnavailable:
		status = http.StatusServiceUnavailable
	}
	return skillImportError(c, status, code)
}

func skillImportError(c echo.Context, status int, code string) error {
	return c.JSON(status, map[string]string{"code": code, "error": code})
}

func putSkill(ctx context.Context, store loom.Store, tenant string, sk *Skill) error {
	data, err := json.Marshal(sk)
	if err != nil {
		return fmt.Errorf("marshal skill: %w", err)
	}
	ns, key := skillKey(tenant, sk.ID)
	return store.Put(ctx, ns, key, data)
}
