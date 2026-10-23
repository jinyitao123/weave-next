package api

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"
)

// A draft starts from what was live when it was opened. When the live team,
// one of its members or one of its flows changes afterwards (a profile edit,
// a roster change, another surface), the draft can no longer be trialled or
// published until it takes those changes in. Rebasing does that with a
// three-way merge against the document the draft last published from.

type developmentRebaseChange struct {
	// Kind is team, member or workflow; Field names the team field.
	Kind  string `json:"kind"`
	ID    string `json:"id,omitempty"`
	Field string `json:"field,omitempty"`
	Name  string `json:"name"`
	// Action: taken (the live change replaced an unchanged draft part),
	// added (new live), removed (removed live), kept (the draft's own edit
	// was kept over a live change, on request).
	Action string `json:"action"`
}

type developmentRebaseConflict struct {
	Kind  string `json:"kind"`
	ID    string `json:"id,omitempty"`
	Field string `json:"field,omitempty"`
	Name  string `json:"name"`
	// LiveRemoved marks a part deleted live that the draft still edits; it
	// cannot be kept, only removed from the draft.
	LiveRemoved bool `json:"live_removed,omitempty"`
}

const developmentRebaseChangesKey = "development_rebase_changes"

func developmentBaselineChanged(message string) error {
	return echo.NewHTTPError(http.StatusConflict, map[string]string{"error": "development_baseline_changed", "message": message})
}

// canonicalDevelopmentValue compares parts by content, not by how their JSON
// happens to be spelled.
func canonicalDevelopmentValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return string(raw)
	}
	canonical, _ := json.Marshal(decoded)
	return string(canonical)
}

type developmentMergeOutcome int

const (
	mergeKeepDraft developmentMergeOutcome = iota // unchanged live, or both changed alike
	mergeTakeLive
	mergeConflict
)

func threeWay(base, draft, live string) developmentMergeOutcome {
	switch {
	case live == base || live == draft:
		return mergeKeepDraft
	case draft == base:
		return mergeTakeLive
	default:
		return mergeConflict
	}
}

// mergeDevelopmentDocuments merges live changes into the draft. base is the
// document the draft started from. keepDraft resolves conflicts in favour of
// the draft's own edits, which publishing would apply over live anyway.
func mergeDevelopmentDocuments(base, draft, live developmentDocument, keepDraft bool) (developmentDocument, []developmentRebaseChange, []developmentRebaseConflict) {
	merged := draft
	changes := []developmentRebaseChange{}
	conflicts := []developmentRebaseConflict{}
	resolve := func(kind, id, field, name string, outcome developmentMergeOutcome, take func()) {
		switch outcome {
		case mergeTakeLive:
			take()
			changes = append(changes, developmentRebaseChange{Kind: kind, ID: id, Field: field, Name: name, Action: "taken"})
		case mergeConflict:
			if keepDraft {
				changes = append(changes, developmentRebaseChange{Kind: kind, ID: id, Field: field, Name: name, Action: "kept"})
			} else {
				conflicts = append(conflicts, developmentRebaseConflict{Kind: kind, ID: id, Field: field, Name: name})
			}
		}
	}
	resolve("team", "", "name", live.Name, threeWay(base.Name, draft.Name, live.Name), func() { merged.Name = live.Name })
	resolve("team", "", "objective", draft.Name, threeWay(base.Objective, draft.Objective, live.Objective), func() { merged.Objective = live.Objective })
	resolve("team", "", "audience", draft.Name, threeWay(canonicalDevelopmentValue(base.Audience), canonicalDevelopmentValue(draft.Audience), canonicalDevelopmentValue(live.Audience)),
		func() { merged.Audience = append([]string{}, live.Audience...) })

	baseMembers, liveMembers := map[string]developmentMember{}, map[string]developmentMember{}
	for _, member := range base.Members {
		baseMembers[member.ID] = member
	}
	for _, member := range live.Members {
		liveMembers[member.ID] = member
	}
	draftMembers := map[string]bool{}
	merged.Members = []developmentMember{}
	for _, member := range draft.Members {
		draftMembers[member.ID] = true
		name := member.Configuration.DisplayName
		baseMember, inBase := baseMembers[member.ID]
		liveMember, inLive := liveMembers[member.ID]
		switch {
		case inLive:
			current := member
			baseValue := canonicalDevelopmentValue(baseMember)
			if !inBase {
				baseValue = ""
			}
			resolve("member", member.ID, "", name, threeWay(baseValue, canonicalDevelopmentValue(member), canonicalDevelopmentValue(liveMember)), func() { current = liveMember })
			merged.Members = append(merged.Members, current)
		case inBase && canonicalDevelopmentValue(member) == canonicalDevelopmentValue(baseMember):
			changes = append(changes, developmentRebaseChange{Kind: "member", ID: member.ID, Name: name, Action: "removed"})
		case inBase:
			conflicts = append(conflicts, developmentRebaseConflict{Kind: "member", ID: member.ID, Name: name, LiveRemoved: true})
			merged.Members = append(merged.Members, member)
		default:
			merged.Members = append(merged.Members, member) // added in the draft
		}
	}
	for _, member := range live.Members {
		if draftMembers[member.ID] {
			continue
		}
		baseMember, inBase := baseMembers[member.ID]
		switch {
		case !inBase:
			merged.Members = append(merged.Members, member)
			changes = append(changes, developmentRebaseChange{Kind: "member", ID: member.ID, Name: member.Configuration.DisplayName, Action: "added"})
		case canonicalDevelopmentValue(member) != canonicalDevelopmentValue(baseMember) && !keepDraft:
			// The draft removed a member that has since changed live.
			conflicts = append(conflicts, developmentRebaseConflict{Kind: "member", ID: member.ID, Name: member.Configuration.DisplayName})
		}
	}

	baseFlows, liveFlows := map[string]developmentWorkflow{}, map[string]developmentWorkflow{}
	for _, flow := range base.Workflows {
		baseFlows[flow.ID] = flow
	}
	for _, flow := range live.Workflows {
		liveFlows[flow.ID] = flow
	}
	draftFlows := map[string]bool{}
	merged.Workflows = []developmentWorkflow{}
	for _, flow := range draft.Workflows {
		draftFlows[flow.ID] = true
		baseFlow, inBase := baseFlows[flow.ID]
		liveFlow, inLive := liveFlows[flow.ID]
		switch {
		case inLive:
			current := flow
			baseValue := canonicalDevelopmentValue(baseFlow)
			if !inBase {
				baseValue = ""
			}
			resolve("workflow", flow.ID, "", flow.Name, threeWay(baseValue, canonicalDevelopmentValue(flow), canonicalDevelopmentValue(liveFlow)), func() { current = liveFlow })
			merged.Workflows = append(merged.Workflows, current)
		case inBase && canonicalDevelopmentValue(flow) == canonicalDevelopmentValue(baseFlow):
			changes = append(changes, developmentRebaseChange{Kind: "workflow", ID: flow.ID, Name: flow.Name, Action: "removed"})
		case inBase:
			conflicts = append(conflicts, developmentRebaseConflict{Kind: "workflow", ID: flow.ID, Name: flow.Name, LiveRemoved: true})
			merged.Workflows = append(merged.Workflows, flow)
		default:
			merged.Workflows = append(merged.Workflows, flow)
		}
	}
	for _, flow := range live.Workflows {
		if draftFlows[flow.ID] {
			continue
		}
		baseFlow, inBase := baseFlows[flow.ID]
		switch {
		case !inBase:
			merged.Workflows = append(merged.Workflows, flow)
			changes = append(changes, developmentRebaseChange{Kind: "workflow", ID: flow.ID, Name: flow.Name, Action: "added"})
		case canonicalDevelopmentValue(flow) != canonicalDevelopmentValue(baseFlow) && !keepDraft:
			conflicts = append(conflicts, developmentRebaseConflict{Kind: "workflow", ID: flow.ID, Name: flow.Name})
		}
	}
	return merged, changes, conflicts
}

func (s *Server) handleRebaseTeamDevelopment(c echo.Context) error {
	if s.Pool == nil {
		return echo.NewHTTPError(503, "开发服务暂不可用")
	}
	var request struct {
		Revision  int64 `json:"expected_revision"`
		KeepDraft bool  `json:"keep_draft"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	if err := s.ensureDevelopmentDraft(c); err != nil {
		return err
	}
	live, liveBase, err := s.seedDevelopment(c)
	if err != nil {
		return err
	}
	ctx, ws, id := c.Request().Context(), getTenant(c), c.Param("id")
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, id, true)
	if err != nil {
		return err
	}
	if d.Revision != request.Revision || d.PublishingRevision != 0 {
		return developmentError("草稿已变化或正在发布，请重新读取；未完成的发布可重试")
	}
	if checkDevelopmentBaseline(ctx, tx, ws, id, d.Baseline) == nil {
		// Nothing changed live since the draft was opened.
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		c.Set(developmentRebaseChangesKey, []developmentRebaseChange{})
		return s.handleGetTeamDevelopment(c)
	}
	// The live state read above must still be current, under the same locks
	// trials and publishing take.
	if err := checkDevelopmentBaseline(ctx, tx, ws, id, liveBase); err != nil {
		return developmentBaselineChanged("线上团队刚刚又有变化，请再试一次")
	}
	merged, changes, conflicts := mergeDevelopmentDocuments(d.PublishedDocument, d.Document, live, request.KeepDraft)
	if len(conflicts) > 0 {
		return c.JSON(http.StatusConflict, map[string]any{
			"error":     "development_rebase_conflict",
			"message":   "草稿和线上都改了同一处，请选择保留草稿的修改，或先在草稿中撤销这些修改",
			"conflicts": conflicts,
		})
	}
	if err := validateDevelopmentDocument(merged); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$3,published_document=$4,baseline=$5,revision=revision+1,updated_by=$6,updated_at=now()
		WHERE workspace_id=$1 AND team_id=$2`, ws, id, encodeDevelopment(merged), encodeDevelopment(live), encodeDevelopment(liveBase), getUserID(c)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	c.Set(developmentRebaseChangesKey, changes)
	return s.handleGetTeamDevelopment(c)
}
