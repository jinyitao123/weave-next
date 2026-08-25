package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/labstack/echo/v4"
)

const mcpPermissionsImportNote = "本导入格式不设置 MCP 与权限，保存后为空/默认值"

type importIdentityPreview struct {
	Source           string   `json:"source"`
	PreviewLines     []string `json:"preview_lines"`
	Content          string   `json:"content"`
	ContentTruncated bool     `json:"content_truncated"`
	Length           int      `json:"length"`
	SHA256           string   `json:"sha256"`
}

type importSkillPreview struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	AlwaysActive    bool   `json:"always_active"`
	BodyLength      int    `json:"body_length"`
	BodySHA256      string `json:"body_sha256"`
	ScriptsCount    int    `json:"scripts_count"`
	ReferencesCount int    `json:"references_count"`
}

type importConflictPreview struct {
	Name           string `json:"name"`
	CurrentVersion int    `json:"current_version"`
	AfterVersion   int    `json:"after_version"`
}

type importPreviewResponse struct {
	PreviewToken          string                         `json:"preview_token"`
	ExpiresAt             time.Time                      `json:"expires_at"`
	Name                  string                         `json:"name"`
	NameSource            string                         `json:"name_source"`
	Model                 string                         `json:"model"`
	ModelSource           string                         `json:"model_source"`
	NameValid             bool                           `json:"name_valid"`
	NameError             string                         `json:"name_error,omitempty"`
	Identity              *importIdentityPreview         `json:"identity,omitempty"`
	ShadowedIdentityFiles []string                       `json:"shadowed_identity_files,omitempty"`
	Skills                []importSkillPreview           `json:"skills"`
	Profiles              map[string]stdlib.ProfileEntry `json:"profiles,omitempty"`
	HealthCheck           *stdlib.HealthCheckDef         `json:"health_check,omitempty"`
	SubAgents             []stdlib.SubAgentRef           `json:"sub_agents,omitempty"`
	GraphType             string                         `json:"graph_type,omitempty"`
	MCPPermissionsNote    string                         `json:"mcp_permissions_note"`
	Conflict              *importConflictPreview         `json:"conflict,omitempty"`
	Files                 []importedFile                 `json:"files"`
	SkippedEntries        []skippedArchiveEntry          `json:"skipped_entries,omitempty"`
	RootHint              string                         `json:"root_hint,omitempty"`
	Warnings              []string                       `json:"warnings,omitempty"`
	ArchiveSHA256         string                         `json:"archive_sha256"`
}

func (s *Server) handleImportPreview(c echo.Context) error {
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is required"})
	}
	pkg, cleanup, importErr := importAgentPackage(file)
	if importErr != nil {
		return c.JSON(importErr.Status, map[string]string{"error": importErr.Message})
	}
	defer cleanup()

	name, nameSource := c.FormValue("name"), "form"
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(file.Filename), filepath.Ext(file.Filename))
		nameSource = "filename"
	}
	model, modelSource := c.FormValue("model"), "form"
	if model == "" {
		model, modelSource = "deepseek-v4-flash", "default"
	}
	response := importPreviewResponse{
		Name: name, NameSource: nameSource, Model: model, ModelSource: modelSource,
		NameValid: isValidAgentName(name), Skills: make([]importSkillPreview, 0),
		Profiles: pkg.Spec.Profiles, HealthCheck: pkg.Spec.HealthCheck,
		SubAgents: pkg.Spec.SubAgents, GraphType: pkg.Spec.GraphType,
		MCPPermissionsNote: mcpPermissionsImportNote, Files: pkg.Files,
		SkippedEntries: pkg.Skipped, ArchiveSHA256: pkg.ArchiveSHA256,
	}
	if !response.NameValid {
		response.NameError = "name must be URL-safe: lowercase letters, digits, underscores or hyphens; maximum 64 characters"
	}
	response.Identity, response.ShadowedIdentityFiles = inspectImportedIdentity(pkg.Root, pkg.Spec)
	for _, skill := range pkg.Spec.Skills {
		response.Skills = append(response.Skills, importSkillPreview{
			Name: skill.Name, Description: skill.Description, AlwaysActive: skill.AlwaysActive,
			BodyLength: len(skill.Body), BodySHA256: sha256Text(skill.Body),
			ScriptsCount: len(skill.Scripts), ReferencesCount: len(skill.References),
		})
	}
	response.RootHint = nestedRootHint(pkg.Files, pkg.Spec)
	response.Warnings = importWarnings(pkg.Files)
	if s.Registry != nil {
		if current, err := s.Registry.Get(c.Request().Context(), getTenant(c), name); err == nil {
			response.Conflict = &importConflictPreview{Name: name, CurrentVersion: current.Version, AfterVersion: current.Version + 1}
		}
	}
	previewToken, expiresAt, err := s.issueImportPreviewToken(c.Request().Context(), getTenant(c), pkg.ArchiveSHA256, name, model, response.Conflict)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	response.PreviewToken, response.ExpiresAt = previewToken, expiresAt
	return c.JSON(http.StatusOK, response)
}

func inspectImportedIdentity(root string, spec *stdlib.AgentSpec) (*importIdentityPreview, []string) {
	present := make(map[string]bool)
	content := make(map[string]string)
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "SOUL.md", "IDENTITY.md"} {
		if data, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			present[name] = true
			content[name] = string(data)
		}
	}
	var source string
	var effective []string
	switch {
	case content["CLAUDE.md"] != "":
		source, effective = "CLAUDE.md", []string{"CLAUDE.md"}
	case content["AGENTS.md"] != "":
		source, effective = "AGENTS.md", []string{"AGENTS.md"}
	case content["SOUL.md"] != "" || content["IDENTITY.md"] != "":
		source = "SOUL.md + IDENTITY.md"
		if content["SOUL.md"] != "" {
			effective = append(effective, "SOUL.md")
		}
		if content["IDENTITY.md"] != "" {
			effective = append(effective, "IDENTITY.md")
		}
	}
	effectiveSet := make(map[string]bool)
	for _, name := range effective {
		effectiveSet[name] = true
	}
	var shadowed []string
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "SOUL.md", "IDENTITY.md"} {
		if present[name] && !effectiveSet[name] {
			shadowed = append(shadowed, name)
		}
	}
	if source == "" || spec.Identity.Raw == "" {
		return nil, shadowed
	}
	lines := strings.Split(spec.Identity.Raw, "\n")
	if len(lines) > 10 {
		lines = lines[:10]
	}
	identityContent, truncated := truncateUTF8(spec.Identity.Raw, 64<<10)
	return &importIdentityPreview{
		Source: source, PreviewLines: lines, Content: identityContent, ContentTruncated: truncated,
		Length: len(spec.Identity.Raw), SHA256: sha256Text(spec.Identity.Raw),
	}, shadowed
}

func truncateUTF8(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && value[end]&0xc0 == 0x80 {
		end--
	}
	return value[:end], true
}

func sha256Text(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

func nestedRootHint(files []importedFile, spec *stdlib.AgentSpec) string {
	if spec.SystemPrompt != "" || len(spec.Skills) != 0 || len(spec.Profiles) != 0 || spec.HealthCheck != nil || len(files) == 0 {
		return ""
	}
	root := ""
	for _, file := range files {
		parts := strings.Split(file.Path, "/")
		if len(parts) < 2 {
			return ""
		}
		if root == "" {
			root = parts[0]
		} else if root != parts[0] {
			return ""
		}
	}
	return fmt.Sprintf("ZIP 内容位于单层嵌套目录 %q；loader 只从解压根目录查找，当前解析为空 spec，请将该目录内容移到 ZIP 根目录", root)
}

func importWarnings(files []importedFile) []string {
	if len(files) == 0 {
		return []string{"ZIP 包为空，没有可导入文件"}
	}
	allowed := map[string]bool{".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true, ".sh": true, ".py": true, ".js": true, ".ts": true}
	var warnings []string
	for _, file := range files {
		if file.Size > 10<<20 {
			warnings = append(warnings, fmt.Sprintf("文件 %q 较大（%d bytes），请人工确认", file.Path, file.Size))
		}
		ext := strings.ToLower(filepath.Ext(file.Path))
		if ext != "" && !allowed[ext] {
			warnings = append(warnings, fmt.Sprintf("文件 %q 类型异常，请人工确认", file.Path))
		}
	}
	sort.Strings(warnings)
	return warnings
}
