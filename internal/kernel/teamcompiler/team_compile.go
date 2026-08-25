package teamcompiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

const teamCatalogCheckpointSchemaVersion = 1

type teamCompiler struct {
	assembler TeamInteractionAssembler
}

type teamCatalogCheckpoint struct {
	SchemaVersion int                 `json:"schema_version"`
	Manifest      TeamCompileManifest `json:"manifest"`
}

func NewTeamCompiler(assembler TeamInteractionAssembler) interface {
	TeamCompiler
	TeamResumer
} {
	if assembler == nil {
		assembler = NewTeamInteractionAssembler()
	}
	return &teamCompiler{assembler: assembler}
}

func (c *teamCompiler) CompileTeam(
	ctx context.Context,
	in TeamCompileInput,
) (*loom.Graph, TeamCompileManifest, error) {
	if err := validateCompileInput(in); err != nil {
		return nil, TeamCompileManifest{}, err
	}
	catalog, err := c.assembler.Assemble(ctx, TeamInteractionInput{
		WorkspaceID:   in.WorkspaceID,
		TeamID:        in.TeamID,
		RunID:         in.RunID,
		RunSnapshotID: in.RunSnapshotID,
		Lead:          in.Lead,
		Workers:       in.Workers,
	})
	if err != nil {
		return nil, TeamCompileManifest{}, err
	}
	hash, err := CatalogContentHash(catalog)
	if err != nil {
		return nil, TeamCompileManifest{}, err
	}
	manifest := TeamCompileManifest{
		Mode: in.Mode, Catalog: catalog, CatalogContentHash: hash,
	}
	graph, err := buildTeamGraph(teamGraphInput{
		Context: ctx, Mode: in.Mode, Workflow: in.Workflow,
		RequestContext: in.RequestContext, WorkerRunner: in.WorkerRunner,
		LLM: in.LLM, Tools: in.Tools, ToolHooks: in.ToolHooks,
		BeforeStepHooks: in.BeforeStepHooks, AfterStepHooks: in.AfterStepHooks,
		ContextEnrichmentStep: in.ContextEnrichmentStep,
		CheckpointStore:       in.CheckpointStore,
		Manifest:              manifest, Assembler: c.assembler,
	})
	if err != nil {
		return nil, TeamCompileManifest{}, err
	}
	if err := persistTeamCatalog(ctx, in.CheckpointStore, manifest); err != nil {
		return nil, TeamCompileManifest{}, err
	}
	return graph, manifest, nil
}

func (c *teamCompiler) ResumeTeam(
	ctx context.Context,
	in TeamResumeInput,
) (*loom.Graph, TeamCompileManifest, error) {
	if err := validateResumeInput(in); err != nil {
		return nil, TeamCompileManifest{}, err
	}
	manifest, err := restoreTeamCatalog(
		ctx, in.CheckpointStore, in.WorkspaceID, in.RunSnapshotID,
	)
	if err != nil {
		return nil, TeamCompileManifest{}, err
	}
	if manifest.Mode != in.Mode ||
		manifest.Catalog.WorkspaceID != in.WorkspaceID ||
		manifest.Catalog.TeamID != in.TeamID ||
		manifest.Catalog.RunID != in.RunID ||
		manifest.Catalog.RunSnapshotID != in.RunSnapshotID {
		return nil, TeamCompileManifest{}, ErrTeamInteractionCatalogMismatch
	}
	graph, err := buildTeamGraph(teamGraphInput{
		Context: ctx, Mode: in.Mode, Workflow: in.Workflow,
		RequestContext: in.RequestContext, WorkerRunner: in.WorkerRunner,
		LLM: in.LLM, Tools: in.Tools, ToolHooks: in.ToolHooks,
		BeforeStepHooks: in.BeforeStepHooks, AfterStepHooks: in.AfterStepHooks,
		ContextEnrichmentStep: in.ContextEnrichmentStep,
		CheckpointStore:       in.CheckpointStore,
		Manifest:              manifest, Assembler: c.assembler,
	})
	if err != nil {
		return nil, TeamCompileManifest{}, err
	}
	return graph, manifest, nil
}

type teamGraphInput struct {
	Context               context.Context
	Mode                  TeamRunMode
	Workflow              *FrozenWorkflowGraph
	RequestContext        map[string]any
	WorkerRunner          LockedWorkerRunner
	LLM                   contract.LLM
	Tools                 contract.ToolDispatcher
	ToolHooks             []contract.ToolHook
	BeforeStepHooks       []loom.StepHook
	AfterStepHooks        []loom.StepHook
	ContextEnrichmentStep loom.Step
	CheckpointStore       loom.Store
	Manifest              TeamCompileManifest
	Assembler             TeamInteractionAssembler
}

func buildTeamGraph(in teamGraphInput) (*loom.Graph, error) {
	switch in.Mode {
	case TeamRunModeFixedWorkflow:
		if len(in.Manifest.Catalog.Handoff) != 0 {
			return nil, ErrTeamHandoffUnavailable
		}
		return buildFixedTeamGraph(in)
	case TeamRunModeFreeCollaboration:
		return buildFreeTeamGraph(in)
	default:
		return nil, ErrTeamCompileInputInvalid
	}
}

func buildFixedTeamGraph(in teamGraphInput) (*loom.Graph, error) {
	if in.Workflow == nil || in.Workflow.Entry == "" ||
		len(in.Workflow.Steps) == 0 || isNilValue(in.WorkerRunner) {
		return nil, ErrTeamCompileInputInvalid
	}
	steps := make(map[string]FrozenWorkflowStep, len(in.Workflow.Steps))
	for _, step := range in.Workflow.Steps {
		if step.ID == "" || step.WorkerAgentID == "" || step.WorkerAgentVersion < 1 {
			return nil, ErrTeamCompileInputInvalid
		}
		if _, duplicate := steps[step.ID]; duplicate {
			return nil, ErrTeamCompileInputInvalid
		}
		if !catalogContainsExactWorker(
			in.Manifest.Catalog, step.WorkerAgentID, step.WorkerAgentVersion,
		) {
			return nil, ErrTeamWorkerVersionUnavailable
		}
		steps[step.ID] = step
	}
	if _, ok := steps[in.Workflow.Entry]; !ok {
		return nil, ErrTeamCompileInputInvalid
	}
	for _, step := range steps {
		if step.Next != "" {
			if _, ok := steps[step.Next]; !ok {
				return nil, ErrTeamCompileInputInvalid
			}
		}
	}

	entry := in.Workflow.Entry
	if in.ContextEnrichmentStep != nil {
		entry = "context_enrichment"
	}
	graph := loom.NewGraph(
		teamGraphName(in.Manifest.Catalog), entry,
		loom.WithCheckpointPolicy(loom.CheckpointRequired),
		loom.WithCheckpointHistory(50),
		loom.WithMergeConfig(loom.DefaultMergeConfig()),
	)
	if len(in.BeforeStepHooks) > 0 || len(in.AfterStepHooks) > 0 {
		graph.SetHooks(loom.HookPoints{Before: in.BeforeStepHooks, After: in.AfterStepHooks})
	}
	if in.ContextEnrichmentStep != nil {
		graph.AddStep("context_enrichment", in.ContextEnrichmentStep, loom.Always(in.Workflow.Entry))
	}
	topology := make([]loom.StepInfo, 0, len(in.Workflow.Steps))
	if in.ContextEnrichmentStep != nil {
		topology = append(topology, loom.StepInfo{
			Name: "context_enrichment", Edges: []loom.Edge{{To: in.Workflow.Entry}},
		})
	}
	for _, definition := range in.Workflow.Steps {
		step, err := in.WorkerRunner.StepFor(in.Context, LockedWorkerRef{
			WorkspaceID:        in.Manifest.Catalog.WorkspaceID,
			RunSnapshotID:      in.Manifest.Catalog.RunSnapshotID,
			WorkerAgentID:      definition.WorkerAgentID,
			WorkerAgentVersion: definition.WorkerAgentVersion,
		})
		if err != nil {
			return nil, err
		}
		if step == nil {
			return nil, ErrTeamWorkerVersionUnavailable
		}
		graph.AddStep(
			definition.ID, fixedWorkerStep(step), loom.Always(definition.Next),
		)
		topology = append(topology, loom.StepInfo{
			Name:   definition.ID,
			Detail: definition.WorkerAgentID,
			Edges:  []loom.Edge{{To: definition.Next}},
		})
	}
	graph.SetTopology(topology)
	return graph, nil
}

func fixedWorkerStep(inner loom.Step) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		update, err := inner(ctx, state)
		if err != nil {
			return nil, err
		}
		if _, attemptedHandoff := update["__delegate_to"]; attemptedHandoff {
			return nil, ErrTeamWorkerGraphIncompatible
		}
		return update, nil
	}
}

func buildFreeTeamGraph(in teamGraphInput) (*loom.Graph, error) {
	if isNilValue(in.LLM) {
		return nil, ErrTeamCompileInputInvalid
	}
	dispatcher := newTeamInteractionDispatcher(
		in.Tools, in.Assembler, in.Manifest.Catalog,
	)
	graph := loom.NewGraph(
		teamGraphName(in.Manifest.Catalog), "team_context",
		loom.WithCheckpointPolicy(loom.CheckpointRequired),
		loom.WithCheckpointHistory(50),
		loom.WithMergeConfig(loom.DefaultMergeConfig()),
	)
	if len(in.BeforeStepHooks) > 0 || len(in.AfterStepHooks) > 0 {
		graph.SetHooks(loom.HookPoints{Before: in.BeforeStepHooks, After: in.AfterStepHooks})
	}
	teamContextTarget := "chat"
	if in.ContextEnrichmentStep != nil {
		teamContextTarget = "context_enrichment"
	}
	graph.AddStep("team_context", teamContextStep(in), loom.Always(teamContextTarget))
	if in.ContextEnrichmentStep != nil {
		graph.AddStep("context_enrichment", in.ContextEnrichmentStep, loom.Always("chat"))
	}
	graph.AddStep("chat", stdlib.NewToolLoopStep(
		in.LLM,
		dispatcher,
		stdlib.ToolLoopOpts{
			SystemPrompt:     teamSystemPrompt(in.Manifest.Catalog),
			MaxIterations:    20,
			StatePatchPolicy: teamHandoffPatchPolicy(in.Manifest.Catalog),
			ToolHooks:        in.ToolHooks,
		},
	), teamHandoffRouter(in.Assembler, in.Manifest.Catalog))

	routes := sortedHandoffRoutes(in.Manifest.Catalog)
	topology := []loom.StepInfo{
		{Name: "team_context", Edges: []loom.Edge{{To: teamContextTarget}}},
		{Name: "chat", Edges: make([]loom.Edge, 0, len(routes)+1)},
	}
	if in.ContextEnrichmentStep != nil {
		topology = append(topology[:1], append([]loom.StepInfo{{
			Name: "context_enrichment", Edges: []loom.Edge{{To: "chat"}},
		}}, topology[1:]...)...)
	}
	for _, route := range routes {
		if isNilValue(in.WorkerRunner) {
			return nil, ErrTeamHandoffUnavailable
		}
		step, err := in.WorkerRunner.StepFor(in.Context, LockedWorkerRef{
			WorkspaceID:        in.Manifest.Catalog.WorkspaceID,
			RunSnapshotID:      in.Manifest.Catalog.RunSnapshotID,
			WorkerAgentID:      route.WorkerAgentID,
			WorkerAgentVersion: route.WorkerAgentVersion,
		})
		if err != nil {
			return nil, codedError(CodeTeamHandoffUnavailable, err)
		}
		if step == nil {
			return nil, ErrTeamHandoffUnavailable
		}
		stepName := handoffStepName(route.RouteKey)
		graph.AddStep(
			stepName, handoffWorkerStep(in.Assembler, in.Manifest.Catalog, route, step), handoffTerminalRouter,
		)
		topology[1].Edges = append(topology[1].Edges, loom.Edge{
			To: stepName, Label: route.RouteKey,
		})
		topology = append(topology, loom.StepInfo{
			Name:   stepName,
			Detail: route.WorkerAgentID,
			Edges:  []loom.Edge{{To: ""}},
		})
	}
	topology[1].Edges = append(
		topology[1].Edges, loom.Edge{To: "", Label: "no handoff"},
	)
	graph.SetTopology(topology)
	return graph, nil
}

func teamContextStep(in teamGraphInput) loom.Step {
	return func(_ context.Context, _ loom.State) (loom.State, error) {
		contextCopy := make(map[string]any, len(in.RequestContext)+4)
		for key, value := range in.RequestContext {
			contextCopy[key] = value
		}
		catalog := in.Manifest.Catalog
		contextCopy["team_id"] = catalog.TeamID
		contextCopy["run_id"] = catalog.RunID
		contextCopy["run_snapshot_id"] = catalog.RunSnapshotID
		contextCopy["mode"] = string(in.Mode)
		return loom.State{
			"context":                         contextCopy,
			"__team_interaction_catalog":      catalog,
			"__team_interaction_catalog_hash": in.Manifest.CatalogContentHash,
		}, nil
	}
}

func teamSystemPrompt(catalog TeamInteractionCatalog) string {
	workers := make(map[string]AuthorizedWorker, len(catalog.Consult)+len(catalog.Dispatch))
	for id, worker := range catalog.Consult {
		workers[id] = worker
	}
	for id, worker := range catalog.Dispatch {
		workers[id] = worker
	}
	ids := make([]string, 0, len(workers))
	for id := range workers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := "You are the frozen lead for this team run. Use only the listed stable worker IDs and route keys." +
		" Delegate required worker duties through the team tools; never perform a worker's duty yourself or claim a worker/verifier result without a successful tool result." +
		" If a required worker is unavailable, report the run as blocked instead of substituting for that worker." +
		" Keep the final response concise: state the outcome, artifact or evidence references, and verifier conclusion without reproducing full worker artifacts or input."
	for _, id := range ids {
		worker := workers[id]
		result += fmt.Sprintf(
			"\n- %s: %s; use when: %s; result: %s",
			id, worker.Duty, worker.WhenToUse, worker.ResultRequirement,
		)
	}
	for _, route := range sortedHandoffRoutes(catalog) {
		result += fmt.Sprintf(
			"\n- handoff %s -> %s", route.RouteKey, route.WorkerAgentID,
		)
	}
	return result
}

func handoffWorkerStep(
	assembler TeamInteractionAssembler,
	catalog TeamInteractionCatalog,
	route HandoffRoute,
	inner loom.Step,
) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		if completed, _ := state["__team_handoff_completed"].(string); completed == route.RouteKey {
			return deleteStateKey(loom.State{}, "__delegate_to"), nil
		}
		update, err := inner(ctx, state)
		if err != nil {
			if recorder, ok := assembler.(TeamInteractionRecorder); ok {
				_ = recorder.RecordInteractionFailure(ctx, catalog, InteractionAuthorizationRequest{
					WorkspaceID:   catalog.WorkspaceID,
					TeamID:        catalog.TeamID,
					RunID:         catalog.RunID,
					RunSnapshotID: catalog.RunSnapshotID,
					WorkerAgentID: route.WorkerAgentID,
					RouteKey:      route.RouteKey,
					Kind:          InteractionHandoff,
				}, InteractionFailureRecord{
					FailureClass: classifyInteractionFailure(err),
					FailureCause: err.Error(),
				})
			}
			return loom.State{
				"__delegate_failed":        route.RouteKey,
				"__delegate_error":         CodeTeamHandoffUnavailable,
				"__delegate_failure_class": classifyInteractionFailure(err),
				"__delegate_cause":         err.Error(),
				"__deleted_keys":           []string{"__delegate_to"},
			}, nil
		}
		if yielded, _ := update["__yield"].(bool); yielded {
			return update, nil
		}
		completed := deleteStateKey(update, "__delegate_to")
		completed["__team_handoff_completed"] = route.RouteKey
		return completed, nil
	}
}

func classifyInteractionFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	cause := strings.ToLower(err.Error())
	if strings.Contains(cause, "timeout") || strings.Contains(cause, "deadline exceeded") {
		return "timeout"
	}
	if strings.Contains(cause, "business") || strings.Contains(cause, "quality") {
		return "business"
	}
	return "infra"
}

func handoffTerminalRouter(_ context.Context, state loom.State) (string, error) {
	if code, failed := state["__delegate_error"].(string); failed && code != "" {
		return "", codedError(code, errors.New(stringValue(state["__delegate_cause"])))
	}
	return "", nil
}

func teamHandoffRouter(
	assembler TeamInteractionAssembler,
	catalog TeamInteractionCatalog,
) loom.Router {
	return func(ctx context.Context, state loom.State) (string, error) {
		routeKey, _ := state["__delegate_to"].(string)
		if routeKey == "" {
			return "", nil
		}
		route, exists := catalog.Handoff[routeKey]
		if !exists || route.RouteKey != routeKey {
			return "", ErrTeamHandoffTargetInvalid
		}
		_, err := assembler.AuthorizeInteraction(
			ctx,
			catalog,
			InteractionAuthorizationRequest{
				WorkspaceID:   catalog.WorkspaceID,
				TeamID:        catalog.TeamID,
				RunID:         catalog.RunID,
				RunSnapshotID: catalog.RunSnapshotID,
				WorkerAgentID: route.WorkerAgentID,
				RouteKey:      routeKey,
				Kind:          InteractionHandoff,
			},
		)
		if err != nil {
			return "", codedError(CodeTeamHandoffTargetInvalid, err)
		}
		return handoffStepName(routeKey), nil
	}
}

func teamHandoffPatchPolicy(catalog TeamInteractionCatalog) *stdlib.StatePatchPolicy {
	return &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{
		"transfer_to": {
			"__delegate_to": func(value any) error {
				route, ok := value.(string)
				if !ok {
					return ErrTeamHandoffTargetInvalid
				}
				if _, exists := catalog.Handoff[route]; !exists {
					return ErrTeamHandoffTargetInvalid
				}
				return nil
			},
		},
	}}
}

func persistTeamCatalog(
	ctx context.Context,
	store loom.Store,
	manifest TeamCompileManifest,
) error {
	if isNilValue(store) {
		return ErrTeamHandoffUnavailable
	}
	encoded, err := json.Marshal(teamCatalogCheckpoint{
		SchemaVersion: teamCatalogCheckpointSchemaVersion,
		Manifest:      manifest,
	})
	if err != nil {
		return codedError(CodeTeamInteractionCatalogMismatch, err)
	}
	namespace := teamCatalogNamespace(manifest.Catalog.WorkspaceID)
	key := manifest.Catalog.RunSnapshotID
	err = store.Tx(ctx, func(tx loom.Store) error {
		keys, err := tx.List(ctx, namespace, key)
		if err != nil {
			return err
		}
		for _, existingKey := range keys {
			if existingKey != key {
				continue
			}
			raw, err := tx.Get(ctx, namespace, key)
			if err != nil {
				return err
			}
			existing, err := decodeTeamCatalogCheckpoint(raw)
			if err != nil {
				return err
			}
			if existing.Mode != manifest.Mode ||
				existing.CatalogContentHash != manifest.CatalogContentHash {
				return ErrTeamInteractionCatalogMismatch
			}
			return nil
		}
		return tx.Put(ctx, namespace, key, encoded)
	})
	if errors.Is(err, ErrTeamInteractionCatalogMismatch) {
		return err
	}
	if err != nil {
		return codedError(CodeTeamHandoffUnavailable, err)
	}
	return nil
}

func restoreTeamCatalog(
	ctx context.Context,
	store loom.Store,
	workspaceID string,
	runSnapshotID string,
) (TeamCompileManifest, error) {
	if isNilValue(store) {
		return TeamCompileManifest{}, ErrTeamHandoffUnavailable
	}
	raw, err := store.Get(
		ctx, teamCatalogNamespace(workspaceID), runSnapshotID,
	)
	if err != nil {
		return TeamCompileManifest{}, codedError(CodeTeamHandoffUnavailable, err)
	}
	return decodeTeamCatalogCheckpoint(raw)
}

func decodeTeamCatalogCheckpoint(raw []byte) (TeamCompileManifest, error) {
	var checkpoint teamCatalogCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return TeamCompileManifest{}, codedError(CodeTeamInteractionCatalogMismatch, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return TeamCompileManifest{}, codedError(CodeTeamInteractionCatalogMismatch, err)
	}
	if checkpoint.SchemaVersion != teamCatalogCheckpointSchemaVersion ||
		checkpoint.Manifest.Mode == "" ||
		checkpoint.Manifest.CatalogContentHash == "" {
		return TeamCompileManifest{}, ErrTeamInteractionCatalogMismatch
	}
	if err := ValidateCatalogContentHash(
		checkpoint.Manifest.Catalog,
		checkpoint.Manifest.CatalogContentHash,
	); err != nil {
		return TeamCompileManifest{}, err
	}
	return checkpoint.Manifest, nil
}

func validateCompileInput(in TeamCompileInput) error {
	if in.WorkspaceID == "" || in.TeamID == "" || in.RunID == "" ||
		in.RunSnapshotID == "" || in.Lead.AgentID == "" ||
		in.Lead.AgentVersion < 1 || isNilValue(in.CheckpointStore) {
		return ErrTeamCompileInputInvalid
	}
	switch in.Mode {
	case TeamRunModeFixedWorkflow:
		if in.Workflow == nil || isNilValue(in.WorkerRunner) {
			return ErrTeamCompileInputInvalid
		}
	case TeamRunModeFreeCollaboration:
		if in.Workflow != nil || isNilValue(in.LLM) {
			return ErrTeamCompileInputInvalid
		}
	default:
		return ErrTeamCompileInputInvalid
	}
	return nil
}

func validateResumeInput(in TeamResumeInput) error {
	if in.WorkspaceID == "" || in.TeamID == "" || in.RunID == "" ||
		in.RunSnapshotID == "" || isNilValue(in.CheckpointStore) {
		return ErrTeamCompileInputInvalid
	}
	switch in.Mode {
	case TeamRunModeFixedWorkflow:
		if in.Workflow == nil || isNilValue(in.WorkerRunner) {
			return ErrTeamCompileInputInvalid
		}
	case TeamRunModeFreeCollaboration:
		if in.Workflow != nil || isNilValue(in.LLM) {
			return ErrTeamCompileInputInvalid
		}
	default:
		return ErrTeamCompileInputInvalid
	}
	return nil
}

func catalogContainsExactWorker(
	catalog TeamInteractionCatalog,
	id string,
	version int64,
) bool {
	for _, directory := range []map[string]AuthorizedWorker{
		catalog.Consult, catalog.Dispatch,
	} {
		worker, ok := directory[id]
		if ok && worker.WorkerAgentID == id && worker.WorkerAgentVersion == version {
			return true
		}
	}
	for _, route := range catalog.Handoff {
		if route.WorkerAgentID == id && route.WorkerAgentVersion == version {
			return true
		}
	}
	return false
}

func sortedHandoffRoutes(catalog TeamInteractionCatalog) []HandoffRoute {
	keys := make([]string, 0, len(catalog.Handoff))
	for key := range catalog.Handoff {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	routes := make([]HandoffRoute, 0, len(keys))
	for _, key := range keys {
		routes = append(routes, catalog.Handoff[key])
	}
	return routes
}

func deleteStateKey(update loom.State, key string) loom.State {
	result := make(loom.State, len(update)+1)
	for name, value := range update {
		result[name] = value
	}
	deleted, _ := update["__deleted_keys"].([]string)
	result["__deleted_keys"] = append(
		append([]string(nil), deleted...), key,
	)
	return result
}

func teamCatalogNamespace(workspaceID string) string {
	return "team-interaction-catalog:" + workspaceID
}

func teamGraphName(catalog TeamInteractionCatalog) string {
	return "team:" + catalog.WorkspaceID + ":" + catalog.TeamID + ":" + catalog.RunSnapshotID
}

func handoffStepName(routeKey string) string { return "handoff_" + routeKey }

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ TeamCompiler = (*teamCompiler)(nil)
var _ TeamResumer = (*teamCompiler)(nil)
