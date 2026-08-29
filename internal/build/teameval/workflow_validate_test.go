package teameval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type staticTeamReader struct {
	team org.Team
}

func (r staticTeamReader) ListTeams(context.Context, string) ([]org.Team, error) {
	return []org.Team{r.team}, nil
}

type emptyRosterReader struct{}

func (emptyRosterReader) ListByTeam(context.Context, string, string) ([]registry.TeamWorker, error) {
	return nil, nil
}

type emptyAgentReader struct{}

func (emptyAgentReader) List(context.Context, string) ([]registry.AgentRecord, error) {
	return nil, nil
}

func (emptyAgentReader) GetVersion(context.Context, string, string, int) (*registry.AgentRecord, error) {
	return nil, errors.New("agent not found")
}

func TestTemplateWorkflowValidationAloneAcceptsBuildingTeam(t *testing.T) {
	deps := WorkflowValidateDeps{
		Teams:  staticTeamReader{team: org.Team{ID: "team-1", Status: "building"}},
		Roster: emptyRosterReader{},
		Agents: emptyAgentReader{},
	}
	trigger := machine.TriggerConfig{
		SchemaVersion: machine.SchemaVersionV1,
		Type:          machine.TriggerConversationExplicit,
		Config:        machine.ConversationExplicitConfig{},
	}
	textContract := machine.OutputContract{Type: machine.ValueText}
	literal := machine.ValueRef{Source: machine.ValueLiteral, Value: json.RawMessage(`"ready"`)}
	graph := machine.GraphDefinition{
		SchemaVersion:  machine.SchemaVersionV1,
		EntryNodeID:    "prepare",
		InputContract:  textContract,
		OutputContract: textContract,
		Nodes: []machine.Node{
			{ID: "prepare", Type: machine.NodeTransform, Output: &textContract, Config: machine.TransformConfig{
				Operation: machine.TransformIdentity,
				Value:     &literal,
			}},
			{ID: "deliver", Type: machine.NodeDeliver, Config: machine.DeliverConfig{Result: machine.ValueRef{
				Source: machine.ValueNodeOutput,
				NodeID: "prepare",
			}}},
		},
		Edges: []machine.Edge{{
			ID: "prepare-deliver", FromNodeID: "prepare", ToNodeID: "deliver", Route: machine.RouteSuccess,
		}},
	}

	ordinary, err := ValidateWorkflowForTeam(context.Background(), deps, "workspace-1", "team-1", trigger, graph)
	if err != nil {
		t.Fatal(err)
	}
	if !reportHasCode(ordinary, machine.CodeTeamInactive) {
		t.Fatalf("ordinary validation must reject a building team: %+v", ordinary.Issues)
	}

	template, err := validateWorkflowForTeam(context.Background(), deps, "workspace-1", "team-1", trigger, graph, true)
	if err != nil {
		t.Fatal(err)
	}
	if reportHasCode(template, machine.CodeTeamInactive) {
		t.Fatalf("template validation must accept the transitional building team: %+v", template.Issues)
	}
}

func reportHasCode(report machine.Report, code string) bool {
	for _, issue := range report.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
