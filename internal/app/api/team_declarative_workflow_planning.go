package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const teamDeclarativeWorkflowPlanToolName = "tf_declarative_workflow_plan"

var teamDeclarativeWorkflowPlanInputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "oneOf":[{"required":["spec"]},{"required":["pattern"]}],
  "properties":{
    "build_run_id":{"type":"string"},
    "pattern":{"$ref":"#/$defs/pattern"},
    "spec":{
      "type":"object","additionalProperties":false,
      "required":["schema_version","entry_node_id","input_contract","output_contract","nodes","edges"],
      "properties":{
        "schema_version":{"const":1},
        "entry_node_id":{"type":"string"},
        "input_contract":{"$ref":"#/$defs/contract"},
        "output_contract":{"$ref":"#/$defs/contract"},
        "nodes":{"type":"array","minItems":1,"maxItems":128,"items":{"$ref":"#/$defs/node"}},
        "edges":{"type":"array","maxItems":256,"items":{"$ref":"#/$defs/edge"}}
      }
    }
  },
  "$defs":{
    "pattern_worker":{"type":"object","additionalProperties":false,"required":["stable_ref","result_requirement"],"properties":{"stable_ref":{"type":"string","pattern":"^[a-z][a-z0-9_-]*$"},"label":{"type":"string"},"result_requirement":{"type":"string","minLength":1}}},
    "pattern":{"type":"object","additionalProperties":false,"required":["kind","lead_instruction","parallel_workers","primary_worker","reviewer_worker","max_iterations"],"properties":{"kind":{"const":"parallel_join_review_loop"},"lead_instruction":{"type":"string","minLength":1,"description":"Frozen run contract passed deterministically to every worker; it does not create an executable lead LLM node."},"parallel_workers":{"type":"array","minItems":2,"maxItems":16,"items":{"$ref":"#/$defs/pattern_worker"}},"primary_worker":{"$ref":"#/$defs/pattern_worker"},"reviewer_worker":{"$ref":"#/$defs/pattern_worker"},"max_iterations":{"type":"integer","minimum":1,"maximum":5}},"description":"Deterministic requirements -> fixed-N parallel -> join -> primary/reviewer bounded rework loop -> deliver shape. The team lead remains ownership metadata; all model execution is performed by the bound worker nodes. The platform owns all nodes, edges, ValueRefs, iteration markers and latch wiring."},
    "contract":{"type":"object","additionalProperties":false,"required":["type"],"properties":{"type":{"type":"string","enum":["text","json","boolean","number"]},"schema":{"type":"object"}}},
    "value_ref":{"type":"object","additionalProperties":false,"required":["source"],"properties":{"source":{"type":"string","enum":["run_input","node_output","literal"]},"path":{"type":"string"},"node_id":{"type":"string"},"value":{},"iteration":{"type":"string","enum":["current_iteration","previous_iteration"]},"default":{"$ref":"#/$defs/value_ref"}}},
    "binding":{"type":"object","additionalProperties":false,"required":["expected_type","value"],"properties":{"expected_type":{"type":"string","enum":["text","json","boolean","number"]},"value":{"$ref":"#/$defs/value_ref"}}},
    "predicate":{"type":"object","additionalProperties":false,"required":["left","operator"],"properties":{"left":{"$ref":"#/$defs/value_ref"},"operator":{"type":"string","enum":["exists","eq","neq","gt","gte","lt","lte","contains","in"]},"right":{"$ref":"#/$defs/value_ref"}}},
    "lead_config":{"type":"object","additionalProperties":false,"required":["instruction"],"properties":{"instruction":{"type":"string"}}},
    "worker_config":{"type":"object","additionalProperties":false,"required":["kind","result_requirement"],"description":"kind=dispatch is only for an immediate parallel branch worker and requires exactly one join outgoing route. Use kind=consult for sequential or loop-body workers; a consult worker requires exactly one success outgoing route, except a loop latch which requires exactly one back route.","properties":{"kind":{"type":"string","enum":["consult","dispatch"]},"result_requirement":{"type":"string"}}},
    "transform_config":{"type":"object","additionalProperties":false,"required":["operation"],"properties":{"operation":{"type":"string","enum":["identity","object","array"]},"value":{"$ref":"#/$defs/value_ref"},"fields":{"type":"object","additionalProperties":{"$ref":"#/$defs/value_ref"}},"items":{"type":"array","items":{"$ref":"#/$defs/value_ref"}}}},
    "condition_case":{"type":"object","additionalProperties":false,"required":["to_node_id","priority","predicate"],"properties":{"to_node_id":{"type":"string"},"priority":{"type":"integer"},"predicate":{"$ref":"#/$defs/predicate"}}},
    "condition_config":{"type":"object","additionalProperties":false,"required":["cases","default_node_id"],"properties":{"cases":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/condition_case"}},"default_node_id":{"type":"string"}}},
    "parallel_config":{"type":"object","additionalProperties":false,"required":["join_node_id"],"properties":{"join_node_id":{"type":"string"}}},
    "join_config":{"type":"object","additionalProperties":false,"required":["policy"],"description":"A join node must omit node.output; the platform derives its output from joined branches.","properties":{"policy":{"type":"string","enum":["all_success","quorum","deadline","fail_fast"]},"success_count":{"type":"integer"},"deadline_seconds":{"type":"integer"}}},
    "human_task":{"type":"object","additionalProperties":false,"required":["title","instructions"],"properties":{"title":{"type":"string","minLength":1},"instructions":{"type":"string","minLength":1},"audience_ref":{"type":"string"}}},
    "wait_config":{"type":"object","additionalProperties":false,"required":["kind","resume_schema","task"],"properties":{"kind":{"const":"human"},"resume_schema":{"type":"object"},"timeout_seconds":{"type":"integer","minimum":1},"task":{"$ref":"#/$defs/human_task"}}},
    "loop_config":{"type":"object","additionalProperties":false,"required":["max_iterations","latch_node_id","continue_predicate"],"description":"The loop node owns exactly body and exit outgoing routes and must omit node.output. The latch node points back to the loop with back. Nodes inside the loop body use normal success/failure routes. The platform derives the loop output as {iteration_count, limit_reached, latch_result}; outside the loop, read the latch value through the loop node with /latch_result or /latch_result/<required_field>, never directly from a body node.","properties":{"max_iterations":{"type":"integer","minimum":1,"maximum":5},"latch_node_id":{"type":"string"},"continue_predicate":{"$ref":"#/$defs/predicate"}}},
    "deliver_config":{"type":"object","additionalProperties":false,"required":["result"],"properties":{"result":{"$ref":"#/$defs/value_ref"}}},
    "node":{"type":"object","additionalProperties":false,"required":["id","type","config"],"properties":{"id":{"type":"string"},"type":{"type":"string","enum":["lead","worker","transform","condition","parallel","join","wait","loop","deliver"]},"label":{"type":"string"},"stable_ref":{"type":"string","pattern":"^[a-z][a-z0-9_-]*$"},"inputs":{"type":"object","additionalProperties":{"$ref":"#/$defs/binding"}},"output":{"$ref":"#/$defs/contract"},"config":{"oneOf":[{"$ref":"#/$defs/lead_config"},{"$ref":"#/$defs/worker_config"},{"$ref":"#/$defs/transform_config"},{"$ref":"#/$defs/condition_config"},{"$ref":"#/$defs/parallel_config"},{"$ref":"#/$defs/join_config"},{"$ref":"#/$defs/wait_config"},{"$ref":"#/$defs/loop_config"},{"$ref":"#/$defs/deliver_config"}]}}},
    "edge":{"type":"object","additionalProperties":false,"required":["from","to","route"],"properties":{"from":{"type":"string"},"to":{"type":"string"},"route":{"type":"string","enum":["success","failure","case","default","branch","join","timeout","body","exit","back"]}}}
  }
}`)

type declarativeWorkflowPlanToolRequest struct {
	BuildRunID string                                  `json:"build_run_id,omitempty"`
	Pattern    *teamforge.DeclarativeWorkflowPatternV1 `json:"pattern,omitempty"`
	Spec       json.RawMessage                         `json:"spec,omitempty"`
}

func (d *teamBlueprintPlanningDispatcher) dispatchDeclarativeWorkflowPlan(
	ctx context.Context,
	call contract.ToolCall,
	result *contract.ToolResult,
) (*contract.ToolResult, error) {
	request, spec, err := decodeDeclarativeWorkflowPlanToolRequest(call.Args)
	if err != nil {
		result.IsError = true
		result.Content = mustJSON(map[string]any{
			"code": "declarative_request_invalid", "error": err.Error(),
			"guidance": "Submit one closed declarative_v1 spec; worker nodes use stable_ref and never agent_id or agent_version.",
		})
		return result, nil
	}
	if request.BuildRunID != "" && request.BuildRunID != d.buildRunID {
		if resolveErr := d.resolveBuildRunID(ctx); resolveErr != nil {
			result.IsError = true
			result.Content = mustJSON(map[string]any{"code": "build_run_unavailable", "error": resolveErr.Error()})
			return result, nil
		}
	}
	if request.BuildRunID != "" && request.BuildRunID != d.buildRunID {
		result.IsError = true
		result.Content = mustJSON(map[string]any{"code": "build_run_mismatch", "error": "build_run_id does not match the conversation-bound run"})
		return result, nil
	}
	if request.BuildRunID == "" {
		if resolveErr := d.resolveBuildRunID(ctx); resolveErr != nil {
			result.IsError = true
			result.Content = mustJSON(map[string]any{"code": "build_run_unavailable", "error": resolveErr.Error()})
			return result, nil
		}
	}
	planned, err := d.server.planDeclarativeWorkflow(ctx, d.workspaceID, d.buildRunID, spec)
	if err != nil {
		result.IsError = true
		var planning *blueprintPlanningError
		if errors.As(err, &planning) {
			result.Content = mustJSON(map[string]any{
				"code": planning.Code, "error": planning.Message, "problems": planning.Problems,
			})
		} else {
			result.Content = mustJSON(map[string]any{"code": "declarative_planning_failed", "error": err.Error()})
		}
		return result, nil
	}
	result.Content = mustJSON(planned)
	return result, nil
}

func decodeDeclarativeWorkflowPlanToolRequest(raw string) (
	declarativeWorkflowPlanToolRequest,
	teamforge.DeclarativeWorkflowSpecV1,
	error,
) {
	var request declarativeWorkflowPlanToolRequest
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, teamforge.DeclarativeWorkflowSpecV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return request, teamforge.DeclarativeWorkflowSpecV1{}, err
	}
	hasSpec := len(bytes.TrimSpace(request.Spec)) != 0
	hasPattern := request.Pattern != nil
	if hasSpec == hasPattern {
		return request, teamforge.DeclarativeWorkflowSpecV1{}, errors.New("exactly one of spec or pattern is required")
	}
	if hasPattern {
		spec, err := teamforge.BuildDeclarativeWorkflowPatternV1(*request.Pattern)
		return request, spec, err
	}
	spec, err := teamforge.DecodeDeclarativeWorkflowSpecV1(request.Spec)
	return request, spec, err
}

func (s *Server) planDeclarativeWorkflow(
	ctx context.Context,
	workspaceID, buildRunID string,
	spec teamforge.DeclarativeWorkflowSpecV1,
) (blueprintPlanResult, error) {
	if err := validateDeclarativeHumanWaitBounds(spec); err != nil {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code: "declarative_human_wait_too_large", Message: err.Error(),
		}
	}
	if s == nil || s.TeamBuild == nil {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code: "team_build_unavailable", Message: "team build store unavailable",
		}
	}
	run, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return blueprintPlanResult{}, err
	}
	if run.Status != teambuild.StatusPlanning {
		return blueprintPlanResult{}, teambuild.ErrBuildRunNotPlanning
	}
	latest, err := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err != nil {
		if errors.Is(err, teambuild.ErrBuildRunNotFound) {
			return blueprintPlanResult{}, &blueprintPlanningError{
				Code:    "declarative_blueprint_required",
				Message: "submit and freeze the TeamBlueprint roster before submitting declarative_v1",
			}
		}
		return blueprintPlanResult{}, err
	}
	var blueprint teambuild.TeamBlueprintV1
	decoder := json.NewDecoder(bytes.NewReader(latest.BlueprintJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&blueprint); err != nil {
		return blueprintPlanResult{}, fmt.Errorf("decode latest TeamBlueprint: %w", err)
	}

	s.TeamBuild.SetBaselineSources(s.OrgStore, s.Registry, s.Workflow, s.WorkflowArtifacts)
	preview, err := s.TeamBuild.PreviewCompilerBaseline(ctx, workspaceID, buildRunID)
	if err != nil {
		return blueprintPlanResult{}, err
	}
	baseline, err := projectCompilerBaseline(run, blueprint, preview)
	if err != nil {
		return blueprintPlanResult{}, err
	}
	var bindings []teamforge.DeclarativeWorkerBindingV1
	switch run.Mode {
	case teambuild.ModeCreate:
		bindings, err = teamforge.ResolveCreateDeclarativeWorkerBindingsV1(spec, blueprint)
	case teambuild.ModeOptimize:
		if preview.Snapshot == nil || preview.BaselineHash == "" {
			return blueprintPlanResult{}, &blueprintPlanningError{
				Code: "declarative_roster_binding_unavailable", Message: "frozen optimize roster snapshot is unavailable",
			}
		}
		bindings, err = teamforge.ResolveDeclarativeWorkerBindingsV1(spec, blueprint, *preview.Snapshot)
	default:
		err = fmt.Errorf("unsupported build mode %q", run.Mode)
	}
	if err != nil {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code: "declarative_worker_binding_failed", Message: err.Error(),
		}
	}
	assetScope := run.AssetScope
	if preview.Snapshot != nil {
		assetScope = preview.Snapshot.AssetScope
	}
	frozen, err := s.freezeDeclarativeWorkflow(
		ctx, workspaceID, run, blueprint, spec, bindings,
		teamforge.DeclarativeBuildBindingV1{
			BuildRunID: buildRunID, BriefHash: run.BriefHash, ContractHash: run.ContractHash,
			AssetScope: assetScope, BaselineHash: preview.BaselineHash,
		},
	)
	if err != nil {
		return blueprintPlanResult{}, declarativePlanningError(err)
	}
	blueprint.Workflow = teambuild.BlueprintWorkflowV1{
		Mode:                teambuild.BlueprintWorkflowDeclarativeV1,
		DeclarativeSpecHash: frozen.SpecHash,
		DeliveryContract:    blueprint.Workflow.DeliveryContract,
	}
	if err := validateBlueprintAgainstRun(run, blueprint); err != nil {
		return blueprintPlanResult{}, err
	}
	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		return blueprintPlanResult{}, planningValidationError(err)
	}
	if latest.BlueprintHash == blueprintHash {
		result, resultErr := blueprintPlanResultFromRevision(latest, true)
		result.DeclarativeSpecHash = frozen.SpecHash
		return result, resultErr
	}
	revisionNo := latest.RevisionNo + 1
	if revisionNo > blueprint.RevisionPolicy.MaxRevisions {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code:    "blueprint_revision_limit_reached",
			Message: "blueprint revision policy does not allow the declarative_v1 revision",
		}
	}
	changeSet, err := teamforge.CompileDeclarativeChangeSetV1(baseline, blueprint, frozen)
	if err != nil {
		return blueprintPlanResult{}, &blueprintPlanningError{
			Code: "declarative_compile_failed", Message: err.Error(),
		}
	}
	blueprintJSON, err := json.Marshal(blueprint)
	if err != nil {
		return blueprintPlanResult{}, fmt.Errorf("marshal declarative TeamBlueprint: %w", err)
	}
	changeSetJSON, err := changeSet.CanonicalBytes()
	if err != nil {
		return blueprintPlanResult{}, fmt.Errorf("marshal declarative ChangeSet: %w", err)
	}
	changeSetHash, err := changeSet.CanonicalHash()
	if err != nil {
		return blueprintPlanResult{}, fmt.Errorf("hash declarative ChangeSet: %w", err)
	}
	revision, err := s.TeamBuild.PersistCompilerAuthorizationBundle(ctx, workspaceID, buildRunID, teambuild.CompilerAuthorizationBundle{
		RevisionNo: revisionNo, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
		ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
		BaselineHash: preview.BaselineHash, BaselineCapturedAt: preview.CapturedAt,
		EvaluationContractHash: run.ContractHash,
	})
	if errors.Is(err, teambuild.ErrBlueprintRevisionNotAppendSafe) {
		current, getErr := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
		if getErr == nil && current.BlueprintHash == blueprintHash {
			result, resultErr := blueprintPlanResultFromRevision(current, true)
			result.DeclarativeSpecHash = frozen.SpecHash
			return result, resultErr
		}
	}
	if err != nil {
		return blueprintPlanResult{}, err
	}
	result, err := blueprintPlanResultFromRevision(revision, false)
	result.DeclarativeSpecHash = frozen.SpecHash
	return result, err
}

func validateDeclarativeHumanWaitBounds(spec teamforge.DeclarativeWorkflowSpecV1) error {
	for _, node := range spec.Nodes {
		if node.Type != machine.NodeWait {
			continue
		}
		if len(node.Config) > teamrun.HumanWaitDetailMaxBytes {
			return fmt.Errorf("human wait node %q config exceeds 16KiB", node.ID)
		}
		var config struct {
			Kind machine.WaitKind `json:"kind"`
			Task struct {
				Title        string `json:"title"`
				Instructions string `json:"instructions"`
			} `json:"task"`
		}
		if err := json.Unmarshal(node.Config, &config); err != nil {
			return fmt.Errorf("decode human wait node %q bounds: %w", node.ID, err)
		}
		if config.Kind == machine.WaitKindHuman && (len(config.Task.Title) > 256 || len(config.Task.Instructions) > 4*1024) {
			return fmt.Errorf("human wait node %q title or instructions exceeds byte limit", node.ID)
		}
	}
	return nil
}

// freezeDeclarativeWorkflow reuses the same proof assembly as workflow draft
// validation while the target workflow may not exist yet.
func (s *Server) freezeDeclarativeWorkflow(
	ctx context.Context,
	workspaceID string,
	run teambuild.TeamBuildRun,
	blueprint teambuild.TeamBlueprintV1,
	spec teamforge.DeclarativeWorkflowSpecV1,
	bindings []teamforge.DeclarativeWorkerBindingV1,
	binding teamforge.DeclarativeBuildBindingV1,
) (teamforge.FrozenDeclarativeWorkflowSpecV1, error) {
	return teamforge.FreezeDeclarativeWorkflowSpecWithDeliveryContractV1(spec, bindings, binding, blueprint.Workflow.DeliveryContract,
		func(trigger machine.TriggerConfig, graph machine.GraphDefinition) (machine.Report, error) {
			if run.Mode == teambuild.ModeCreate {
				planned := make([]teameval.PlannedWorkerBinding, 0, len(bindings))
				for _, worker := range bindings {
					planned = append(planned, teameval.PlannedWorkerBinding{
						StableRef: worker.StableRef, AgentID: worker.AgentID, AgentVersion: worker.AgentVersion,
					})
				}
				return teameval.ValidateWorkflowForBlueprint(workspaceID, blueprint, planned, trigger, graph)
			}
			return teameval.ValidateWorkflowForTeam(ctx, teameval.WorkflowValidateDeps{
				Teams: s.OrgStore, Roster: s.TeamWorkers, Agents: s.Registry, Workflows: s.Workflow,
			}, workspaceID, run.Brief.TeamID, trigger, graph)
		})
}

func declarativePlanningError(err error) *blueprintPlanningError {
	var validation *teamforge.DeclarativeWorkflowValidationError
	if !errors.As(err, &validation) {
		return &blueprintPlanningError{Code: "declarative_plan_failed", Message: err.Error()}
	}
	problems := make([]teambuild.BlueprintProblem, 0, len(validation.Problems))
	for _, problem := range validation.Problems {
		problems = append(problems, teambuild.BlueprintProblem{
			Path: problem.Path, Code: problem.Code, Message: declarativePlanningProblemMessage(problem),
		})
	}
	return &blueprintPlanningError{
		Code: "declarative_validation_failed", Message: validation.Error(), Problems: problems,
	}
}

func declarativePlanningProblemMessage(problem teamforge.DeclarativeWorkflowProblem) string {
	switch problem.Code {
	case machine.CodeEdgeCardinalityInvalid:
		return problem.Message + "; dispatch workers are parallel branches and require exactly one join route; sequential or loop-body workers must use kind=consult with one success route, except the loop latch which uses one back route"
	case machine.CodeSuccessPathUnterminated:
		return problem.Message + "; repair edge cardinality first, then ensure the loop exits to deliver and its latch returns to the loop with back"
	case machine.CodeOutputForbidden:
		return problem.Message + "; join and loop nodes must omit node.output because the platform derives it; a loop exposes {iteration_count, limit_reached, latch_result}"
	case machine.CodeJSONPointerUnprovable:
		return problem.Message + "; a loop exposes its latch output under /latch_result, so use /latch_result or /latch_result/<required_field> and require that field in the latch JSON output schema"
	case machine.CodeValueLoopScopeInvalid:
		return problem.Message + "; outside a loop, read body results only through the loop node at /latch_result or /latch_result/<required_field>"
	default:
		return problem.Message
	}
}
