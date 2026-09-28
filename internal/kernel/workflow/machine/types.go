package machine

import (
	"encoding/json"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

const SchemaVersionV1 = 1

type TriggerType string

const (
	TriggerConversationExplicit TriggerType = "conversation_explicit"
	TriggerConversationAuto     TriggerType = "conversation_auto"
	TriggerSchedule             TriggerType = "schedule"
	TriggerAPI                  TriggerType = "api"
	TriggerEvent                TriggerType = "event"
)

type TriggerConfig struct {
	SchemaVersion int            `json:"schema_version"`
	Type          TriggerType    `json:"type"`
	Config        TriggerVariant `json:"config"`
	Delivery      *Delivery      `json:"delivery,omitempty"`
}

type TriggerVariant interface {
	triggerVariant()
}

type ConversationExplicitConfig struct{}

func (ConversationExplicitConfig) triggerVariant() {}

type ConversationAutoConfig struct {
	CatalogKey string `json:"catalog_key"`
}

func (ConversationAutoConfig) triggerVariant() {}

type ScheduleConfig struct {
	ScheduleID string `json:"schedule_id"`
}

func (ScheduleConfig) triggerVariant() {}

type APIConfig struct {
	EndpointKey string `json:"endpoint_key"`
}

func (APIConfig) triggerVariant() {}

type EventConfig struct {
	EventType string `json:"event_type"`
}

func (EventConfig) triggerVariant() {}

type DeliveryKind string

const (
	DeliveryJobRecord   DeliveryKind = "job_record"
	DeliveryCallbackRef DeliveryKind = "callback_ref"
	DeliveryTargetRef   DeliveryKind = "target_ref"
)

type Delivery struct {
	Kind DeliveryKind `json:"kind"`
	Ref  string       `json:"ref,omitempty"`
}

type GraphDefinition struct {
	SchemaVersion    int                           `json:"schema_version"`
	EntryNodeID      string                        `json:"entry_node_id"`
	ResultProtocol   string                        `json:"result_protocol,omitempty"`
	InputContract    OutputContract                `json:"input_contract"`
	OutputContract   OutputContract                `json:"output_contract"`
	DeliveryContract *deliverable.DeliveryContract `json:"delivery_contract,omitempty"`
	Nodes            []Node                        `json:"nodes"`
	Edges            []Edge                        `json:"edges"`
}

type ValueType string

const (
	ValueText    ValueType = "text"
	ValueJSON    ValueType = "json"
	ValueBoolean ValueType = "boolean"
	ValueNumber  ValueType = "number"
)

type OutputContract struct {
	Type   ValueType       `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type InputBinding struct {
	ExpectedType ValueType `json:"expected_type"`
	Value        ValueRef  `json:"value"`
}

type ValueSource string

const (
	ValueRunInput   ValueSource = "run_input"
	ValueNodeOutput ValueSource = "node_output"
	ValueLiteral    ValueSource = "literal"
)

type Iteration string

const (
	IterationCurrent  Iteration = "current_iteration"
	IterationPrevious Iteration = "previous_iteration"
)

type ValueRef struct {
	Source    ValueSource     `json:"source"`
	Path      string          `json:"path,omitempty"`
	NodeID    string          `json:"node_id,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Iteration Iteration       `json:"iteration,omitempty"`
	Default   *ValueRef       `json:"default,omitempty"`
}

type NodeType string

const (
	NodeLead      NodeType = "lead"
	NodeWorker    NodeType = "worker"
	NodeTransform NodeType = "transform"
	NodeCondition NodeType = "condition"
	NodeParallel  NodeType = "parallel"
	NodeJoin      NodeType = "join"
	NodeWait      NodeType = "wait"
	NodeLoop      NodeType = "loop"
	NodeDeliver   NodeType = "deliver"
	NodeHandoff   NodeType = "handoff"
)

type Node struct {
	ID     string                  `json:"id"`
	Type   NodeType                `json:"type"`
	Label  string                  `json:"label,omitempty"`
	Inputs map[string]InputBinding `json:"inputs,omitempty"`
	Output *OutputContract         `json:"output,omitempty"`
	Config NodeConfig              `json:"config"`
}

type NodeConfig interface {
	nodeConfig()
}

type LeadConfig struct {
	Instruction string `json:"instruction"`
}

func (LeadConfig) nodeConfig() {}

type WorkerKind string

const (
	WorkerConsult  WorkerKind = "consult"
	WorkerDispatch WorkerKind = "dispatch"
)

type WorkerConfig struct {
	AgentID           string     `json:"agent_id"`
	AgentVersion      int64      `json:"agent_version"`
	Kind              WorkerKind `json:"kind"`
	ResultRequirement string     `json:"result_requirement"`
}

func (WorkerConfig) nodeConfig() {}

type TransformOperation string

const (
	TransformIdentity TransformOperation = "identity"
	TransformObject   TransformOperation = "object"
	TransformArray    TransformOperation = "array"
)

type TransformConfig struct {
	Operation TransformOperation  `json:"operation"`
	Value     *ValueRef           `json:"value,omitempty"`
	Fields    map[string]ValueRef `json:"fields,omitempty"`
	Items     []ValueRef          `json:"items,omitempty"`
}

func (TransformConfig) nodeConfig() {}

type ConditionConfig struct{}

func (ConditionConfig) nodeConfig() {}

type ParallelConfig struct {
	JoinNodeID string `json:"join_node_id"`
}

func (ParallelConfig) nodeConfig() {}

type JoinPolicy string

const (
	JoinAllSuccess JoinPolicy = "all_success"
	JoinQuorum     JoinPolicy = "quorum"
	JoinDeadline   JoinPolicy = "deadline"
	JoinFailFast   JoinPolicy = "fail_fast"
)

type JoinConfig struct {
	Policy          JoinPolicy `json:"policy"`
	SuccessCount    *int64     `json:"success_count,omitempty"`
	DeadlineSeconds *int64     `json:"deadline_seconds,omitempty"`
}

func (JoinConfig) nodeConfig() {}

type WaitKind string

const (
	// An omitted kind remains a timer wait for machine-v1 compatibility.
	WaitKindTimer WaitKind = "timer"
	WaitKindHuman WaitKind = "human"
)

type HumanTaskConfig struct {
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
	AudienceRef  string `json:"audience_ref,omitempty"`
}

type WaitConfig struct {
	Kind           WaitKind         `json:"kind,omitempty"`
	ResumeSchema   json.RawMessage  `json:"resume_schema"`
	TimeoutSeconds *int64           `json:"timeout_seconds,omitempty"`
	Task           *HumanTaskConfig `json:"task,omitempty"`
}

func (WaitConfig) nodeConfig() {}

func (c WaitConfig) EffectiveKind() WaitKind {
	if c.Kind == "" {
		return WaitKindTimer
	}
	return c.Kind
}

type PredicateOperator string

const (
	OperatorExists   PredicateOperator = "exists"
	OperatorEQ       PredicateOperator = "eq"
	OperatorNEQ      PredicateOperator = "neq"
	OperatorGT       PredicateOperator = "gt"
	OperatorGTE      PredicateOperator = "gte"
	OperatorLT       PredicateOperator = "lt"
	OperatorLTE      PredicateOperator = "lte"
	OperatorContains PredicateOperator = "contains"
	OperatorIn       PredicateOperator = "in"
)

type Predicate struct {
	Left     ValueRef          `json:"left"`
	Operator PredicateOperator `json:"operator"`
	Right    *ValueRef         `json:"right,omitempty"`
}

type LoopConfig struct {
	MaxIterations     int64     `json:"max_iterations"`
	LatchNodeID       string    `json:"latch_node_id"`
	ContinuePredicate Predicate `json:"continue_predicate"`
}

func (LoopConfig) nodeConfig() {}

type DeliverConfig struct {
	Result ValueRef `json:"result"`
}

func (DeliverConfig) nodeConfig() {}

type HandoffConfig struct {
	AgentID        string `json:"agent_id"`
	AgentVersion   int64  `json:"agent_version"`
	Instruction    string `json:"instruction"`
	TimeoutSeconds *int64 `json:"timeout_seconds,omitempty"`
}

func (HandoffConfig) nodeConfig() {}

type EdgeRoute string

const (
	RouteSuccess EdgeRoute = "success"
	RouteFailure EdgeRoute = "failure"
	RouteCase    EdgeRoute = "case"
	RouteDefault EdgeRoute = "default"
	RouteBranch  EdgeRoute = "branch"
	RouteJoin    EdgeRoute = "join"
	RouteTimeout EdgeRoute = "timeout"
	RouteBody    EdgeRoute = "body"
	RouteExit    EdgeRoute = "exit"
	RouteBack    EdgeRoute = "back"
)

type Edge struct {
	ID         string     `json:"id"`
	FromNodeID string     `json:"from_node_id"`
	ToNodeID   string     `json:"to_node_id"`
	Route      EdgeRoute  `json:"route"`
	Priority   *int64     `json:"priority,omitempty"`
	Predicate  *Predicate `json:"predicate,omitempty"`
}
