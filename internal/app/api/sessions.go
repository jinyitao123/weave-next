package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	conversationstore "github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/labstack/echo/v4"
)

// SessionSummary is a lightweight session entry for listing.
type SessionSummary struct {
	ID        string `json:"id"`
	AgentHint string `json:"agent_hint,omitempty"`
	Title     string `json:"title,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func (s *Server) handleListSessions(c echo.Context) error {
	tenant := getTenant(c)
	userID := getUserID(c)
	// New format: {tenant}:{userID}:{agent}:{sessionID}
	// Scan only this user's sessions.
	prefix := tenant + ":" + userID + ":"
	agentFilter := c.QueryParam("agent")

	keys, err := s.Store.List(c.Request().Context(), "session", prefix)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Also scan legacy keys ({tenant}:{agent}:{sessionID}) for backward compat.
	// Only include legacy sessions for the original "dev" user who created them.
	if userID == "dev" {
		legacyPrefix := tenant + ":"
		if legacyKeys, err := s.Store.List(c.Request().Context(), "session", legacyPrefix); err == nil {
			for _, k := range legacyKeys {
				// Skip keys that already match new format (contain userID segment).
				if strings.HasPrefix(k, prefix) {
					continue
				}
				keys = append(keys, k)
			}
		}
	}

	// Pre-filter keys and collect those we need titles for.
	type sessionEntry struct {
		key       string
		agent     string
		sessionID string
	}
	// Deduplicate by sessionID — prefer new-format keys over legacy.
	seen := make(map[string]int) // sessionID → index in entries
	var entries []sessionEntry
	var metaKeys []string
	for _, key := range keys {
		rest := strings.TrimPrefix(key, tenant+":")
		// Parse: new format has 3 parts (userID:agent:sessionID),
		// legacy has 2 parts (agent:sessionID).
		var agent, sessionID string
		isNewFormat := false
		parts := strings.SplitN(rest, ":", 3)
		switch len(parts) {
		case 3: // new format: userID:agent:sessionID
			agent = parts[1]
			sessionID = parts[2]
			isNewFormat = true
		case 2: // legacy: agent:sessionID
			agent = parts[0]
			sessionID = parts[1]
		default:
			sessionID = rest
		}
		if agentFilter != "" && agent != "" && agent != agentFilter {
			continue
		}
		if idx, dup := seen[sessionID]; dup {
			// If this is new-format and previous was legacy, replace it.
			if isNewFormat {
				entries[idx] = sessionEntry{key: key, agent: agent, sessionID: sessionID}
				metaKeys[idx] = key
			}
			continue
		}
		seen[sessionID] = len(entries)
		entries = append(entries, sessionEntry{key: key, agent: agent, sessionID: sessionID})
		metaKeys = append(metaKeys, key)
	}

	// Batch-load all session titles and timestamps in a single query.
	titles := make(map[string]string)
	times := make(map[string]string)
	if s.StoreExt != nil && len(metaKeys) > 0 {
		if vals, err := s.StoreExt.GetByKeys(c.Request().Context(), "session_meta", metaKeys); err == nil {
			for k, v := range vals {
				titles[k] = string(v)
			}
		}
		if vals, err := s.StoreExt.GetByKeys(c.Request().Context(), "session_time", metaKeys); err == nil {
			for k, v := range vals {
				times[k] = string(v)
			}
		}
	} else {
		// Fallback: individual queries (no StoreExt available).
		for _, key := range metaKeys {
			if raw, err := s.Store.Get(c.Request().Context(), "session_meta", key); err == nil && len(raw) > 0 {
				titles[key] = string(raw)
			}
			if raw, err := s.Store.Get(c.Request().Context(), "session_time", key); err == nil && len(raw) > 0 {
				times[key] = string(raw)
			}
		}
	}

	sessions := make([]SessionSummary, 0, len(entries))
	for _, e := range entries {
		sessions = append(sessions, SessionSummary{
			ID:        e.sessionID,
			AgentHint: e.agent,
			Title:     titles[e.key],
			CreatedAt: times[e.key],
		})
	}
	return c.JSON(http.StatusOK, sessions)
}

func (s *Server) handleGetSession(c echo.Context) error {
	tenant := getTenant(c)
	userID := getUserID(c)
	sessionID := c.Param("id")
	agentHint := c.QueryParam("agent")

	var msgs []contract.Message
	var found bool
	var foundKey string

	// Fast path: new format with user_id.
	if agentHint != "" {
		directKey := tenant + ":" + userID + ":" + agentHint + ":" + sessionID
		if loaded, err := stdlib.LoadSession(s.Store, directKey); err == nil && len(loaded) > 0 {
			msgs = loaded
			found = true
			foundKey = directKey
		}
		// Fallback: legacy format without user_id.
		if !found {
			legacyKey := tenant + ":" + agentHint + ":" + sessionID
			if loaded, err := stdlib.LoadSession(s.Store, legacyKey); err == nil && len(loaded) > 0 {
				msgs = loaded
				found = true
				foundKey = legacyKey
			}
		}
	}

	// Slow path: scan this user's session keys.
	if !found {
		prefix := tenant + ":" + userID + ":"
		keys, _ := s.Store.List(c.Request().Context(), "session", prefix)
		for _, key := range keys {
			if strings.HasSuffix(key, ":"+sessionID) {
				loaded, err := stdlib.LoadSession(s.Store, key)
				if err == nil && len(loaded) > 0 {
					msgs = loaded
					found = true
					foundKey = key
					break
				}
			}
		}
	}

	// Legacy scan fallback — only for "dev" user (legacy sessions have no user_id).
	if !found && userID == "dev" {
		keys, _ := s.Store.List(c.Request().Context(), "session", tenant+":")
		for _, key := range keys {
			if strings.HasSuffix(key, ":"+sessionID) {
				loaded, err := stdlib.LoadSession(s.Store, key)
				if err == nil && len(loaded) > 0 {
					msgs = loaded
					found = true
					foundKey = key
					break
				}
			}
		}
	}

	if !found {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
	}

	// Read-only passthrough of event messages (dispatch cards) recorded in the
	// conversation bound to this session key. Card data stays in weave_messages;
	// nothing is mirrored into the loom session. Unavailable conversation store
	// or a failed lookup degrades to an empty list, never a failed session read.
	events := make([]conversationstore.Message, 0)
	if s.Conversations != nil && foundKey != "" {
		if loaded, err := s.Conversations.ListEventMessagesBySessionKey(
			c.Request().Context(), tenant, foundKey,
		); err != nil {
			c.Logger().Warnf("session events load (key=%s): %v", foundKey, err)
		} else {
			events = loaded
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"id":       sessionID,
		"messages": msgs,
		"events":   events,
	})
}

// saveSessionTitle stores a short title for a session (derived from the first user message).
// It only writes if no title exists yet (i.e., first message in session).
func saveSessionTitle(ctx context.Context, store loom.Store, sessionKey string, userMessage string) {
	// Don't overwrite existing title.
	if raw, err := store.Get(ctx, "session_meta", sessionKey); err == nil && len(raw) > 0 {
		return
	}
	title := userMessage
	// Truncate to 50 chars.
	if len([]rune(title)) > 50 {
		title = string([]rune(title)[:50]) + "..."
	}
	// Remove newlines.
	title = strings.ReplaceAll(title, "\n", " ")
	_ = store.Put(ctx, "session_meta", sessionKey, []byte(title))
	// Store creation timestamp (ISO 8601).
	_ = store.Put(ctx, "session_time", sessionKey, []byte(time.Now().UTC().Format(time.RFC3339)))
}

func (s *Server) handleDeleteSession(c echo.Context) error {
	tenant := getTenant(c)
	userID := getUserID(c)
	sessionID := c.Param("id")

	// Try new format first: scan only this user's sessions.
	prefix := tenant + ":" + userID + ":"
	keys, _ := s.Store.List(c.Request().Context(), "session", prefix)
	for _, key := range keys {
		if strings.HasSuffix(key, ":"+sessionID) {
			_ = s.Store.Delete(c.Request().Context(), "session", key)
			_ = s.Store.Delete(c.Request().Context(), "session_meta", key)
			_ = s.Store.Delete(c.Request().Context(), "session_time", key)
			return c.NoContent(http.StatusNoContent)
		}
	}

	// Legacy fallback — only for "dev" user.
	if userID == "dev" {
		legacyKeys, _ := s.Store.List(c.Request().Context(), "session", tenant+":")
		for _, key := range legacyKeys {
			if strings.HasSuffix(key, ":"+sessionID) {
				_ = s.Store.Delete(c.Request().Context(), "session", key)
				_ = s.Store.Delete(c.Request().Context(), "session_meta", key)
				_ = s.Store.Delete(c.Request().Context(), "session_time", key)
				return c.NoContent(http.StatusNoContent)
			}
		}
	}

	return c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
}
