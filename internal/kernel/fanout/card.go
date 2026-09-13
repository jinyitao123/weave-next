package fanout

import (
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type dispatchCardMetadata struct {
	Type     string            `json:"type"`
	GroupID  string            `json:"group_id"`
	Revision int               `json:"revision"`
	Terminal bool              `json:"terminal"`
	Legs     []dispatchCardLeg `json:"legs"`
}

type dispatchCardLeg struct {
	Worker string `json:"worker"`
	Status string `json:"status"`
}

// RenderCard renders one task group as human-readable progress and structured
// event-message metadata.
func RenderCard(group Group, legs []LegSnapshot, revision int, terminal bool) (string, json.RawMessage) {
	return renderCard(group, legs, revision, terminal)
}

func renderCard(group Group, legs []LegSnapshot, revision int, terminal bool) (string, json.RawMessage) {
	completed := 0
	cardLegs := make([]dispatchCardLeg, 0, len(legs))
	for _, leg := range legs {
		if leg.Status == taskqueue.StatusCompleted {
			completed++
		}
		// TODO: 按 viewer 身份脱敏成“一名后台员工”，待可见性模型。
		cardLegs = append(cardLegs, dispatchCardLeg{Worker: leg.Agent, Status: leg.Status})
	}
	state := "派工中"
	if terminal {
		state = "派工完成"
	}
	content := fmt.Sprintf("%s：%d 名员工，%d 完成", state, len(legs), completed)
	metadata, _ := json.Marshal(dispatchCardMetadata{
		Type:     "dispatch_card",
		GroupID:  group.ID,
		Revision: revision,
		Terminal: terminal,
		Legs:     cardLegs,
	})
	return content, metadata
}
