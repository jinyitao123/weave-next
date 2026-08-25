export interface User {
  id: string;
  tenant_id: string;
  username?: string;
  display_name?: string;
  role: string;
  disabled?: boolean;
  source?: "apikey";
  created_at?: string;
  updated_at?: string;
}

export interface LoginRequest {
  username: string;
  password: string;
  tenant: string;
}

export interface LoginResponse {
  token: string;
  user: User;
}

export interface DevTokenResponse {
  token: string;
}

export interface RefreshResponse {
  token: string;
}
export interface UpdateMeInput {
  display_name: string;
}

export interface ChangeMyPasswordInput {
  current_password: string;
  new_password: string;
}

export interface RegisterUserInput {
  username: string;
  password: string;
  display_name: string;
}

export interface UpdateUserInput {
  display_name: string;
  role: string;
  disabled: boolean;
}

export type WorkspaceMemberRole = "owner" | "member";

export interface Workspace {
  id: string;
  slug: string;
  name: string;
  created_at: string;
}

export interface WorkspaceResponse {
  workspace: Workspace;
  role: WorkspaceMemberRole | "";
}

export interface WorkspaceMember {
  workspace_id: string;
  user_id: string;
  role: WorkspaceMemberRole;
  created_at: string;
  username: string;
  display_name: string;
  deleted: boolean;
}

export interface AddWorkspaceMemberInput {
  user_id: string;
  role: WorkspaceMemberRole;
}

export interface APIKey {
  id: string;
  tenant_id: string;
  name: string;
  role: string;
  scopes?: string[];
  created_by?: string;
  expires_at?: string | null;
  last_used?: string | null;
  created_at: string;
}

export interface CreateAPIKeyInput {
  name: string;
  role: string;
  scopes: string[];
  expires_at: string | null;
}

export interface CreateAPIKeyResponse {
  id: string;
  name: string;
  role: string;
  scopes: string[] | null;
  key: string;
  expires_at: string | null;
  created_at: string;
}

export type AgentRole = "worker" | "avatar";
export type AgentEngine = "" | "loom" | "opencode" | "codex" | "claude";
export type AgentMemoryScope = "tenant" | "user" | "session";

export interface AgentIdentitySpec {
  core?: string;
  extended?: string;
  raw?: string;
}

export interface AgentProfileEntry {
  system_addition?: string;
  greeting?: string;
}

export interface AgentSkillSpec {
  name: string;
  description: string;
  body?: string;
  always_active?: boolean;
  scripts?: string[];
  references?: string[];
}

export interface AgentSpec {
  system_prompt?: string;
  identity?: AgentIdentitySpec;
  profiles?: Record<string, AgentProfileEntry>;
  skills?: AgentSkillSpec[];
  health_check?: { content?: string } | null;
  sub_agents?: Array<{ name: string; description?: string; route_key?: string }>;
  graph_type?: string;
}

export interface AgentPermissionConfig {
  deny?: string[];
  allow?: string[];
  ask?: string[];
}

export interface AgentMCPServerConfig {
  server_id?: string;
  url?: string;
  filter?: string[];
  write_tools?: string[];
  headers?: Record<string, string>;
}

export interface AgentMemoryConfig {
  enabled: boolean;
  top_k?: number;
  auto_remember: boolean;
  scope?: AgentMemoryScope;
}

export interface AgentMemorySlot {
  key: string;
  label: string;
  description: string;
}

export interface AgentGuardConfig {
  enabled: boolean;
  max_input_len?: number;
  blocked_terms?: string[];
}

export interface AgentCompactionConfig {
  enabled: boolean;
  token_threshold?: number;
}

export interface AgentSubAgentRef {
  name: string;
  description?: string;
  route_key?: string;
}

export interface AgentGraphStep {
  name: string;
  type: "chat" | "llm_call" | "llm_check" | "yield" | "transform" | "builtin" | "worker";
  display: string;
  config: Record<string, unknown>;
  next?: string | null;
  condition?: { key: string; true: string | null; false: string | null } | null;
}

export interface AgentGraphDefinition {
  entry: string;
  steps: AgentGraphStep[];
}

export interface AgentWriteInput {
  name: string;
  owner_user_id?: string | null;
  display_name?: string;
  role?: AgentRole;
  engine?: AgentEngine | string;
  runtime_id?: string;
  model: string;
  spec: AgentSpec;
  permissions: AgentPermissionConfig;
  mcp_servers: AgentMCPServerConfig[];
  memory_config: AgentMemoryConfig;
  memory_slots: AgentMemorySlot[];
  output_schema?: unknown;
  max_cost_usd: number;
  max_tokens: number;
  max_output_tokens: number;
  step_budget: number;
  max_tool_repeats: number;
  fallback_models: string[];
  fallback_retries: number;
  guard: AgentGuardConfig;
  compaction: AgentCompactionConfig;
  sub_agents: AgentSubAgentRef[];
  graph_type: string;
  graph_definition?: AgentGraphDefinition;
  tags: string[];
}

export interface AgentRecord extends AgentWriteInput {
  id: string;
  workspace_id: string;
  team_id?: string;
  owner_user_id?: string | null;
  /** public | internal_tool | platform；platform 为平台内置资产，不进用户可见列表。 */
  visibility?: string;
  version: number;
  created_at: string;
  updated_at: string;
  deleted?: boolean;
}

export interface AgentTeamMembershipSummaryAgent {
  id: string;
  name: string;
  display_name: string;
  role: AgentRole;
}

export interface AgentLeadTeamMembership {
  team_id: string;
  team_name: string;
  team_status: TeamStatus;
}

export interface AgentWorkerTeamMembership {
  team_id: string;
  team_name: string;
  team_status: TeamStatus;
  duty: string;
  allowed_kinds: TeamRosterKind[];
  default_kind: TeamRosterKind;
  enabled: boolean;
}

export interface AgentTeamMembershipsResponse {
  agent: AgentTeamMembershipSummaryAgent;
  lead_of: AgentLeadTeamMembership[];
  worker_of: AgentWorkerTeamMembership[];
}

export interface AgentRunSummaryLatestRun {
  run_id: string;
  classification: string;
  started_at: string;
}

export interface AgentRunSummaryResponse {
  agent: string;
  latest_run: AgentRunSummaryLatestRun | null;
  active_run_count: number;
  recent_failure_count: number;
  window: number;
}

export type AgentLinkType = "peer" | "manages";

export interface AgentLink {
  id: string;
  source_agent_id: string;
  target_agent_id: string;
  type: AgentLinkType;
  instruction: string;
}

export interface CreateAgentLinkInput {
  source_agent_id: string;
  target_agent_id: string;
  type?: AgentLinkType;
  instruction: string;
}

export interface OrphanWorker {
  id: string;
  name: string;
}

export interface OrphanWorkerScanResponse {
  orphans: OrphanWorker[];
}

export interface AgentChannel {
  id: string;
  name: string;
  position: number;
  is_default: boolean;
}

export interface PromptPreviewInput {
  user_message: string;
  profile?: string;
  context?: Record<string, unknown>;
}

export interface PromptPreviewResponse {
  prompt: string;
  tokens: number;
  active_skills: string[];
  profile_applied?: string;
  context_keys?: string[];
}

export interface AgentTopologyEdge {
  to: string;
  label?: string;
}

export interface AgentTopologyStep {
  name: string;
  detail?: string;
  edges: AgentTopologyEdge[];
}

export interface AgentTopologyResponse {
  agent: string;
  entry: string;
  steps: AgentTopologyStep[];
}

export interface Skill {
  id: string;
  name: string;
  description?: string;
  body: string;
  always_active?: boolean;
  category?: string;
  created_at: string;
  updated_at: string;
}

export interface SkillWriteInput {
  id: string;
  name: string;
  description: string;
  body: string;
  always_active: boolean;
  category: string;
}

export interface LegacySkillImportInput {
  schema_version: number;
  idempotency_key: string;
  reason: string;
  expected_source_hash?: string;
}

export interface LegacySkillImportResponse {
  schema_version: number;
  skill_id: string;
  version: number;
  changed: boolean;
  source_hash: string;
  content_hash: string;
}

export interface AgentMemory {
  id: string;
  content: string;
  metadata?: Record<string, unknown>;
  created_at: string;
  accessed_at: string;
  access_count: number;
}

export interface AgentMemorySearchResult extends AgentMemory {
  score: number;
}

export interface DisabledMemoryResponse {
  enabled: false;
  memories: [];
}

export type AgentMemoryListResponse = AgentMemory[] | DisabledMemoryResponse;
export type AgentMemorySearchResponse = AgentMemorySearchResult[] | DisabledMemoryResponse;

export interface AgentMemoryCreateInput {
  content: string;
  metadata?: Record<string, unknown>;
}

export interface AgentMemorySearchInput {
  query: string;
  top_k: number;
}

export interface AgentMemoryProfile {
  workspace_id: string;
  agent_id: string;
  user_id: string;
  slots: Record<string, string>;
  version: number;
  updated_at: string;
}

export type TeamStatus = "active" | "needs_repair" | "building" | "archived";
export type TeamRosterKind = "consult" | "dispatch" | "handoff";

export interface Team {
  id: string;
  workspace_id: string;
  name: string;
  objective: string;
  primary_scenario: string;
  success_criteria: string;
  lead_avatar_id: string;
  status: TeamStatus;
  evaluation: "evaluated" | "unevaluated";
  created_at: string;
  updated_at: string;
}

export interface TeamTemplateSample {
  name: string;
  display_name: string;
  description?: string;
  yaml: string;
}

export interface TeamTemplateSamplesResponse {
  samples: TeamTemplateSample[];
}

export interface CreateTeamFromTemplateInput {
  yaml: string;
  idempotency_key: string;
}

export interface CreateTeamFromTemplateOutcome {
  team_id?: string;
  build_run_id: string;
  status: "ready" | "building" | "authorization_required" | string;
  evaluation?: "unevaluated";
  progress_url?: string;
}

export interface TeamAgentSummary {
  id: string;
  name: string;
  display_name: string;
  engine: string;
  runtime_id: string;
  role: AgentRole;
  duty: string;
  configured_duty: string;
  when_to_use: string;
  context_instruction: string;
  allowed_kinds: TeamRosterKind[];
  default_kind: TeamRosterKind | "";
  result_requirement: string;
  enabled: boolean;
}

export interface TeamRoster {
  team: Team;
  lead: TeamAgentSummary | null;
  workers: TeamAgentSummary[];
}

export interface TeamRosterWorkerInput {
  worker_agent_id: string;
  duty: string;
  when_to_use: string;
  context_instruction: string;
  allowed_kinds: TeamRosterKind[];
  default_kind: TeamRosterKind | "";
  result_requirement: string;
  enabled: boolean;
}

export type InitialTeamWorkerInput = Omit<TeamRosterWorkerInput, "enabled">;

export interface CreateTeamInput {
  name: string;
  objective: string;
  primary_scenario: string;
  success_criteria: string;
  lead_avatar_id: string;
  workers: InitialTeamWorkerInput[];
}

export interface CreateTeamResponse extends Team {
  workers: Array<TeamRosterWorkerInput & { workspace_id: string; team_id: string; created_at: string; updated_at: string }>;
}

export interface UpdateTeamRosterInput {
  idempotency_key: string;
  expected_updated_at: string;
  desired_team_status: TeamStatus;
  lead_agent_id: string;
  workers: TeamRosterWorkerInput[];
  reason: string;
}

export interface TeamRosterResult {
  schema_version: number;
  team_id: string;
  team_status: TeamStatus;
  lead_agent_id: string;
  updated_at: string;
  workers: TeamRosterWorkerInput[];
  changed: boolean;
  audit_id: string | null;
  affected_workers: Array<{ worker_agent_id: string; affected_published_version_count: number; revocation_impact_url: string }>;
}

export type TeamDispatchExecution = "parallel";

export interface TeamDispatchRules {
  team_id: string;
  execution: TeamDispatchExecution;
  leg_timeout_sec: number;
  group_deadline_sec: number;
  quorum: number;
}

export interface UpdateTeamDispatchRulesInput {
  execution: TeamDispatchExecution;
  leg_timeout_sec: number;
  group_deadline_sec: number;
  quorum: number;
}

export type WorkflowStatus = "active" | "archived";
export type WorkflowVersionStatus = "draft" | "published";
export type WorkflowTriggerType = "conversation_explicit" | "conversation_auto" | "schedule" | "api" | "event";
export type WorkflowDeliveryKind = "job_record" | "callback_ref" | "target_ref";
export type WorkflowValueType = "text" | "json" | "boolean" | "number";
export type WorkflowValueSource = "run_input" | "node_output" | "literal";
export type WorkflowIteration = "current_iteration" | "previous_iteration";
export type WorkflowNodeType = "lead" | "worker" | "transform" | "condition" | "parallel" | "join" | "wait" | "loop" | "deliver" | "handoff";
export type WorkflowEdgeRoute = "success" | "failure" | "case" | "default" | "branch" | "join" | "timeout" | "body" | "exit" | "back";
export type WorkflowJoinPolicy = "all_success" | "quorum" | "deadline" | "fail_fast";
export type WorkflowWorkerKind = "consult" | "dispatch";
export type WorkflowTransformOperation = "identity" | "object" | "array";
export type WorkflowPredicateOperator = "exists" | "eq" | "neq" | "gt" | "gte" | "lt" | "lte" | "contains" | "in";

export interface WorkflowContract {
  type: WorkflowValueType;
  schema?: Record<string, unknown>;
}

export interface WorkflowValueRef {
  source: WorkflowValueSource;
  path?: string;
  node_id?: string;
  value?: unknown;
  iteration?: WorkflowIteration;
  default?: { source: "literal"; value: unknown };
}

export interface WorkflowInputBinding {
  expected_type: WorkflowValueType;
  value: WorkflowValueRef;
}

export type WorkflowNodeConfig =
  | { instruction: string }
  | { agent_id: string; agent_version: number; kind: WorkflowWorkerKind; result_requirement: string }
  | { operation: WorkflowTransformOperation; value?: WorkflowValueRef; fields?: Record<string, WorkflowValueRef>; items?: WorkflowValueRef[] }
  | Record<string, never>
  | { join_node_id: string }
  | { policy: WorkflowJoinPolicy; success_count?: number; deadline_seconds?: number }
  | { resume_schema: Record<string, unknown>; timeout_seconds?: number }
  | { request: WorkflowValueRef; response_schema: Record<string, unknown>; timeout_seconds?: number }
  | { max_iterations: number; latch_node_id: string; continue_predicate: WorkflowPredicate }
  | { result: WorkflowValueRef }
  | { agent_id: string; agent_version: number; instruction: string; timeout_seconds?: number };

export interface WorkflowNode {
  id: string;
  type: WorkflowNodeType;
  label?: string;
  inputs?: Record<string, WorkflowInputBinding>;
  output?: WorkflowContract;
  config: WorkflowNodeConfig;
}

export interface WorkflowPredicate {
  left: WorkflowValueRef;
  operator: WorkflowPredicateOperator;
  right?: WorkflowValueRef;
}

export interface WorkflowEdge {
  id: string;
  from_node_id: string;
  to_node_id: string;
  route: WorkflowEdgeRoute;
  priority?: number;
  predicate?: WorkflowPredicate;
}

export interface WorkflowGraphDefinition {
  schema_version: 1;
  entry_node_id: string;
  input_contract: WorkflowContract;
  output_contract: WorkflowContract;
  nodes: WorkflowNode[];
  edges: WorkflowEdge[];
}

export interface WorkflowTriggerConfig {
  schema_version: 1;
  type: WorkflowTriggerType;
  config: Record<string, never> | { catalog_key: string } | { schedule_id: string } | { endpoint_key: string } | { event_type: string };
  delivery?: { kind: WorkflowDeliveryKind; ref?: string };
}

export interface TeamWorkflow {
  workspace_id: string;
  id: string;
  team_id: string;
  name: string;
  description: string;
  status: WorkflowStatus;
  published_version?: number;
  created_at: string;
  updated_at: string;
}

export interface TeamWorkflowVersion {
  workspace_id: string;
  workflow_id: string;
  version: number;
  status: WorkflowVersionStatus;
  trigger_config: WorkflowTriggerConfig;
  graph_definition: WorkflowGraphDefinition;
  created_by: string;
  created_at: string;
  updated_at: string;
  published_at?: string;
}

export interface WorkflowWorkerReference { id: string; name: string; agent_version: number }
export interface WorkflowLatestRun { run_id: string; classification: string }
export interface WorkflowSummary {
  id: string;
  name: string;
  description: string;
  status: WorkflowStatus;
  published_version: number | null;
  draft_version: number | null;
  trigger_summary: { type: string } | null;
  referenced_workers: WorkflowWorkerReference[];
  latest_run: WorkflowLatestRun | null;
  created_at: string;
  updated_at: string;
}
export interface WorkflowListResponse { workflows: WorkflowSummary[] }
export interface WorkflowVersionSlot { version: number; updated_at: string; trigger_config: WorkflowTriggerConfig }
export interface WorkflowDetailResponse { team_id: string; workflow: WorkflowSummary; published: WorkflowVersionSlot | null; draft: WorkflowVersionSlot | null }
export interface WorkflowVersionResponse { workflow_id: string; team_id: string; version: number; status: WorkflowVersionStatus; trigger_config: WorkflowTriggerConfig; graph_definition: WorkflowGraphDefinition; created_at: string; updated_at: string }
export interface CreateWorkflowInput { name: string; description: string; trigger_config: WorkflowTriggerConfig; graph_definition: WorkflowGraphDefinition }
export interface CreateWorkflowResponse { workflow: TeamWorkflow; draft: TeamWorkflowVersion }
export interface UpdateWorkflowDraftInput { expected_updated_at: string; trigger_config: WorkflowTriggerConfig; graph_definition: WorkflowGraphDefinition }
export interface WorkflowValidationIssue { phase: number; path: string; node_id?: string; code: string; message: string; occurrence: number }
export interface WorkflowValidationResponse { valid: boolean; issues: WorkflowValidationIssue[] }
export interface WorkflowDependency { kind: string; key: string; owner: string; hash: string }
export interface WorkflowDependenciesResponse { workflow_id: string; version: number; artifact_hash: string; dependencies: WorkflowDependency[] }
export type WorkflowAdmissionStatus = "admitted" | "blocked" | "grandfathered" | "unknown";
export interface WorkflowAdmissionStatusResponse { status: WorkflowAdmissionStatus; reasons: string[]; updated_at: string | null }
export interface WorkflowArchiveResponse { id: string; status: "archived" }
export interface WorkflowManualRunRequest { project_id: string; conversation_id: string; input?: unknown }
export interface WorkflowLegacyManualRunRequest { input?: unknown }
export interface WorkflowManualRunResponse { run_id: string; workflow_id: string; workflow_version: number; task_id: string; project_id?: string; conversation_id?: string }

export interface Project {
  id: string;
  workspace_id: string;
  team_id: string;
  avatar_id: string;
  name: string;
  description: string;
  /** 系统兜底项目标记（"unclassified"），普通项目为空串。 */
  system_kind?: string;
  archived_at: string | null;
  created_at: string;
  updated_at: string;
  last_activity_at: string | null;
}

export interface ProjectListResponse {
  projects: Project[];
}

export interface ProjectCollaborator {
  project_id: string;
  team_id: string;
  added_by: string;
  added_at: string;
  removed_at?: string | null;
}

export interface ProjectCollaboratorListResponse {
  collaborators: ProjectCollaborator[];
}

export interface CreateProjectInput {
  team_id: string;
  name: string;
  description: string;
}

export interface UpdateProjectInput {
  name: string;
  description: string;
}

export interface MoveProjectInput {
  team_id: string;
}
export type ProjectResourceKind = "attachment" | "mcp_server" | "local_workspace";

export interface ProjectResource {
  id: string;
  workspace_id: string;
  project_id: string;
  kind: ProjectResourceKind;
  resource_ref: string;
  display_name: string;
  runtime_id?: string;
  available: boolean;
  unavailable_reason?: string;
  created_at: string;
}

export interface CreateProjectResourceInput {
  kind: ProjectResourceKind;
  resource_ref: string;
  display_name: string;
  runtime_id?: string;
}

export interface Attachment {
  id: string;
  workspace_id: string;
  filename: string;
  size: number;
  sha256: string;
  content_type: string;
  created_by: string;
  created_at: string;
}

export type MCPTransport = "streamable_http" | "stdio";

export interface MCPServer {
  id: string;
  workspace_id: string;
  slug: string;
  display_name: string;
  transport: MCPTransport;
  url?: string;
  command?: string;
  args?: string[];
  functional_revision: number;
  enabled: boolean;
  revoked_at?: string | null;
  status: string;
  protocol_version?: string;
  last_error?: string;
  server_info?: unknown;
  last_probed_at?: string | null;
  last_handshake_at?: string | null;
  created_by: string;
  created_at: string;
  updated_at: string;
  deleted_at?: string | null;
  headers: Record<string, string>;
  env: Record<string, string>;
  tool_count: number;
  agent_count: number;
}

export interface MCPServerUpsertInput {
  slug: string;
  display_name: string;
  transport: MCPTransport;
  url: string;
  command: string;
  args: string[];
  headers: Record<string, string>;
  deleted_headers: string[];
  env: Record<string, string>;
  deleted_env: string[];
  enabled: boolean;
}

export interface MCPTool {
  server_id: string;
  name: string;
  description: string;
  input_schema: unknown;
  annotations?: unknown;
  read_only_hint?: boolean | null;
  discovered_at: string;
}

export interface MCPProbeResult {
  server: MCPServer;
  tools: MCPTool[];
}

export interface ProviderConfigInput {
  id: string;
  name: string;
  base_url: string;
  api_key?: string;
  models: string[];
  json_object_mode: boolean;
}

export interface ProviderHead extends ProviderConfigInput {
  workspace_id: string;
  latest_revision: number;
  source_kind: string;
  source_provider_id?: string | null;
  enabled: boolean;
  revoked_at?: string | null;
  deleted_at?: string | null;
}

export type ProviderMirrorOutcome = "created" | "functional_updated" | "credential_rotated" | "noop";

export interface ProviderMirrorResult {
  provider_id: string;
  revision: number;
  outcome: ProviderMirrorOutcome;
}

export interface SystemProviderSummary {
  id: string;
  name: string;
  base_url: string;
  models: string[];
  mirrored: boolean;
  mirrored_as?: string;
  mirror_revision?: number;
}

export interface EmbedderConfig {
  url: string;
  api_key?: string;
  model: string;
  dimension: number;
}

export type DeliveryTargetKind = "callback" | "target";

export interface DeliveryTarget {
  id: string;
  latest_revision: number;
  enabled: boolean;
  revoked_at: string | null;
  deleted_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface DeliveryTargetRevision {
  target_id: string;
  revision: number;
  kind: DeliveryTargetKind;
  transport: string;
  url: string;
  method: string;
  content_type: string;
  timeout_seconds: number;
  header_names: string[];
  content_hash: string;
  created_at: string;
}

export interface DeliveryTargetConfigInput {
  kind: DeliveryTargetKind;
  url: string;
  timeout_seconds: number;
  headers: Record<string, string>;
}

export interface CreateDeliveryTargetInput extends DeliveryTargetConfigInput {
  id: string;
}

export interface DeliveryTargetMutation {
  target: DeliveryTarget;
  revision: DeliveryTargetRevision;
  advanced: boolean;
}

export interface DeliveryTargetListResponse {
  targets: DeliveryTarget[];
}

export interface DeliveryTargetDetailResponse {
  target: DeliveryTarget;
  revision: DeliveryTargetRevision;
}

export interface DeliveryTargetRevisionResponse {
  revision: DeliveryTargetRevision;
}

export interface RuntimeCreateResponse {
  id: string;
  name: string;
  token: string;
}

export interface RuntimeCapabilityFact {
  runtime_id: string;
  name: string;
  engines: string[];
  runtime_revision: number;
  pool_id?: string;
  health_status: string;
  total_slots: number;
  active_slots: number;
  enabled: boolean;
  online: boolean;
  eligible: boolean;
  unavailable_reason?: string;
}

export interface RuntimeAssignment {
  runtime_id?: string;
  runtime_revision?: number;
  mode: "explicit" | "auto" | string;
  reason_code: string;
  engine: string;
  pool_id?: string;
  capability_facts: RuntimeCapabilityFact[];
}

export type AgentScheduleKind = "daily" | "once";

export interface AgentSchedule {
  id: string;
  agent: string;
  message: string;
  kind: AgentScheduleKind;
  time_of_day: string;
  run_at?: string | null;
  timezone: string;
  enabled: boolean;
  last_run_date: string;
  last_run_at?: string | null;
  created_at: string;
  updated_at: string;
}

export interface AgentScheduleInput {
  id?: string;
  agent: string;
  message: string;
  kind: AgentScheduleKind;
  time_of_day: string;
  run_at: string | null;
  timezone: string;
  enabled: boolean;
}

export type ConnectorSyncMode = "batch" | "event" | "manual";
export type ConnectorFrequency = "hourly" | "daily" | "weekly";
export type ConnectorStrategy = "full" | "incremental";
export type ConnectorConflict = "source" | "local" | "mark";

export interface ConnectorSchedule {
  source_url: string;
  tenant?: string;
  display_name: string;
  sync_mode: ConnectorSyncMode;
  frequency: ConnectorFrequency;
  time_of_day: string;
  strategy: ConnectorStrategy;
  conflict: ConnectorConflict;
  tables: string[];
  updated_at?: string;
}

export interface AgentUsage {
  agent: string;
  runs: number;
  cost_usd: number;
  tokens_in: number;
  tokens_out: number;
}

export interface UsageReport {
  total_runs: number;
  total_cost_usd: number;
  total_tokens_in: number;
  total_tokens_out: number;
  by_agent: AgentUsage[];
}

export type TeamBuildRunStatus = "planning" | "authorized" | "round_running" | "publishing" | "passed" | "blocked" | "cancelled";

export interface TeamBuildRunSummary {
  build_run_id: string;
  mode: string;
  status: TeamBuildRunStatus;
  conversation_id?: string;
  target_team_id?: string;
  target_team_name?: string;
  new_team_name?: string;
  current_round: number;
  max_rounds: number;
  latest_conclusion?: string;
  latest_failure_category?: string;
  budget_usage: {
    input_tokens: number;
    output_tokens: number;
    cost_usd: number;
    tool_calls: number;
  };
  publish_eligible: boolean;
  rollback_status: string;
  final_ref?: { ref: string; audit_id?: string };
  confirmed_by?: string;
  expires_at: string;
  updated_at: string;
}

export interface TeamBuildRunListResponse {
  items: TeamBuildRunSummary[];
  total: number;
  limit: number;
  offset: number;
}

export interface ProjectMemory {
  id: string;
  content: string;
  score?: number;
  metadata?: Record<string, unknown>;
  created_at: string;
  accessed_at: string;
  access_count: number;
}

export interface MemoryDisabledResponse {
  enabled: false;
  memories: ProjectMemory[];
}

export type ProjectMemoryResponse = ProjectMemory[] | MemoryDisabledResponse;

export interface ProjectMemorySearchInput {
  query: string;
  top_k?: number;
}
export interface Runtime {
  id: string;
  name: string;
  engines: string[];
  engine_capabilities: Record<string, {
    engine: string;
    binary_path: string;
    binary_version: string;
    auth_mode: "chatgpt" | "oauth" | "provider" | "unknown" | string;
    protocol_version: string;
    endpoint_class: string;
  }>;
  functional_revision: number;
  health_status: "healthy" | "busy" | "offline" | string;
  total_slots: number;
  active_slots: number;
  pool_id?: string;
  consecutive_infra_failures: number;
  quarantine_until?: string | null;
  last_failure_reason?: string;
  enabled: boolean;
  revoked_at: string | null;
  deleted_at: string | null;
  online: boolean;
  last_heartbeat_at: string | null;
  created_at: string;
}

export interface ContentBlock {
  type: "text" | "code" | "chart" | "diagram" | "table" | "component" | string;
  text?: string;
  language?: string;
  code?: string;
  filename?: string;
  chart_type?: string;
  title?: string;
  data?: { labels: string[]; datasets: Array<{ label: string; values: number[]; color?: string }> };
  format?: string;
  source?: string;
  caption?: string;
  headers?: string[];
  rows?: string[][];
  component_type?: string;
  props?: Record<string, unknown>;
}

export interface AssistantMessageMetadata {
  blocks?: ContentBlock[];
  grounding?: unknown;
  runtime_assignment?: RuntimeAssignment;
  execution?: AssistantExecutionMetadata;
  execution_segments?: ExecutionSegment[];
  blueprint_build_run_id?: string;
  blueprint_revision_token?: BlueprintRevisionToken;
  blueprint_authorization_semantic?: string;
  team_blueprint_summary?: TeamBlueprintSummary;
  terminal_outcome?: TerminalOutcome;
}

export interface TerminalOutcome {
  schema_version: 1;
  turn_state: "plan_ready" | "needs_clarification" | "blocked" | "completed";
  build_run_id?: string;
  build_run_status?: "planning" | "authorized" | "round_running" | "publishing" | "passed" | "blocked" | "failed" | "cancelled";
  reason_code?: "blueprint_plan_budget_exhausted" | "blueprint_planning_no_revision" | "blueprint_planning_transition_failed" | "blueprint_validation_failed" | "candidate_run_failed" | "infra_failure";
  agent_summary?: string;
  report?: TeamArchitectReport;
  mode_note?: {
    mode: "optimize";
    team_name: string;
  };
}

export interface TeamArchitectReport {
  conclusion: string;
  team_name: string;
  members: Array<{
    name: string;
    role: string;
    duty: string;
  }>;
  next_action: "等待继续" | "已受阻" | "需澄清" | "已完成";
}

export interface BlueprintRevisionToken {
  revision_no: number;
  blueprint_hash: string;
  change_set_hash: string;
}

export interface TeamBlueprintSummary {
  mode?: "create" | "optimize";
  team_name?: string;
  goal?: string;
  template?: string;
  workflow_summary?: string;
  acceptance_criteria?: string[];
  members?: Array<{
    ref: string;
    name: string;
    display_name?: string;
    role: string;
    duty?: string[];
    responsibilities?: string[];
    capabilities?: string[];
  }>;
}

export interface AssistantExecutionMetadata {
  schema_version: number;
  run_id?: string;
  agent: string;
  status: "running" | "yielded" | "completed" | "failed" | string;
  stop_reason?: string;
  input: string;
  output: string;
  started_at: string;
  completed_at?: string;
  tool_calls?: ToolCallRecord[];
}

export interface MessageAttachment {
  id: string;
  filename: string;
}

export interface UserMessageMetadata {
  attachments?: MessageAttachment[];
}

export interface ToolCallRecord {
  call_id?: string;
  agent?: string;
  name: string;
  args?: string;
  result?: string;
  status: string;
  started_at?: string;
  completed_at?: string;
}

export type ExecutionSegment =
  | { id: string; type: "text"; agent: string; content: string; created_at: string }
  | { id: string; type: "tool"; agent: string; tool: ToolCallRecord; created_at: string }
  | { id: string; type: "step"; agent: string; label: string; created_at: string };

export interface ChatRequest {
  agent: string;
  project_id: string;
  conversation_id?: string;
  intent?: "create_team";
  runtime_id?: string;
  client_request_id: string;
  session_id?: string;
  message: string;
  channel?: string;
  stream: true;
  attachment_ids?: string[];
  blueprint_change_requested?: boolean;
}

export interface ChatTerminalPayload {
  output?: string;
  stop_reason?: string;
  session_id?: string;
  run_id?: string;
  project_id?: string;
  conversation_id?: string;
  user_message_id?: string;
  yield_type?: string;
  error?: string;
  replayed?: boolean;
  runtime_assignment?: RuntimeAssignment;
  terminal_outcome?: TerminalOutcome;
}

export type ChatRequestStatus = "admitting" | "admitted" | "queued" | "running" | "yielded" | "completed" | "failed";

export interface ChatRequestRecord {
  workspace_id: string;
  user_id: string;
  client_request_id: string;
  project_id: string;
  agent_id: string;
  session_id?: string;
  conversation_id?: string;
  user_message_id?: string;
  task_id?: string;
  run_id?: string;
  status: ChatRequestStatus;
  response?: ChatTerminalPayload;
  error_code?: string;
  runtime_assignment?: RuntimeAssignment;
  workflow_progress?: {
    status: "queued" | "running" | "parked" | "cancel_requested" | "succeeded" | "failed" | "cancelled" | "abandoned";
    completed_stages: number;
    total_stages: number;
    latest_stage?: string;
    updated_at: string;
  };
  created_at: string;
  updated_at: string;
}

export interface ChatStreamEvent {
  type: string;
  data: Record<string, unknown>;
}

export interface ChatStreamResult {
  terminal: ChatTerminalPayload;
  content: string;
}

export interface RunCheckpoint {
  seq: number;
  last_step?: string;
  saved_at?: string;
}

export interface ForkRunInput {
  seq: number;
  agent: string;
  input: Record<string, unknown>;
  stream: true;
}

export interface ForkTerminalPayload extends ChatTerminalPayload {
  run_id: string;
  parent_run_id: string;
  parent_seq: number;
}

export type ForkStreamEvent = ChatStreamEvent;

export interface ForkStreamResult {
  terminal: ForkTerminalPayload;
  content: string;
}

export interface ResumeRunInput {
  run_id: string;
  agent: string;
  input: Record<string, unknown>;
  stream: true;
}

export type ResumeStreamEvent = ChatStreamEvent;
export type ResumeStreamResult = ChatStreamResult;

export interface Conversation {
  id: string;
  workspace_id: string;
  project_id: string;
  agent_id: string;
  user_id: string;
  title: string;
  channel: string;
  intent?: "create_team";
  session_key?: string;
  parent_message_id?: string;
  thread_title?: string;
  content_version: number;
  created_at: string;
  updated_at: string;
  read_only?: boolean;
  build_run?: BuildRunSummary;
}

export interface BuildRunSummary {
  build_run_id: string;
  status: string;
}

export type BuildRunOperationStepStatus = "pending" | "running" | "succeeded" | "skipped" | "failed";

export interface BuildRunOperationStep {
  operation_id: string;
  operation_index: number;
  operation_type: string;
  target?: string;
  target_name?: string;
  target_role?: string;
  display_label?: string;
  status: BuildRunOperationStepStatus | string;
  depends_on: string[];
  attempt: number;
  error_class?: string;
  error_code?: string;
  error_detail?: string;
  started_at?: string;
  completed_at?: string;
  updated_at: string;
  candidate_evaluation?: CandidateEvaluation;
}

export interface CandidateEvaluation {
  scenario_input: string;
  conclusion: "pass" | "revise" | "blocked" | string;
  rubric_scores: Array<{ name: string; score: number; max: number; pass: number }>;
  gate_failures: string[];
  artifact_ref?: string;
}

export interface BuildRunProgress {
  workspace_id: string;
  build_run_id: string;
  run_status: string;
  revision_no?: number;
  steps: BuildRunOperationStep[];
  final_ref?: { ref: string; audit_id?: string; team_id?: string };
  updated_at: string;
}

export interface SubmitBuildRunResponse {
  workspace_id: string;
  build_run_id: string;
  run_status: string;
  queue_status: string;
  authority: string;
}

export interface ThreadSummary {
  thread_id: string;
  parent_message_id: string;
  thread_title: string;
  reply_count: number;
  last_reply_at: string | null;
  created_at: string;
}

export interface ThreadListResponse {
  thread_summaries: ThreadSummary[];
}

export interface CreateThreadInput {
  parent_message_id: string;
}

export interface CreateThreadResponse {
  thread_id: string;
}

export interface Message {
  id: string;
  seq: number;
  conversation_id: string;
  workspace_id: string;
  role: "user" | "assistant" | "event" | string;
  content: string;
  parent_message_id?: string;
  metadata?: unknown;
  event_id?: string;
  lease_epoch?: number;
  created_at: string;
}

export interface UnreadConversation {
  workspace_id: string;
  user_id: string;
  conversation_id: string;
  unread_count: number;
  last_message_id?: string;
  updated_at: string;
}

export interface RealtimeEvent {
  type: string;
  conversation_id?: string;
  payload?: unknown;
}

export interface TaskGroupLeg {
  id: string;
  agent: string;
  status: string;
  run_id?: string;
  error?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  updated_at: string;
}

export interface TaskGroup {
  id: string;
  workspace_id: string;
  project_id?: string;
  avatar_agent: string;
  user_id: string;
  status: "active" | "resolving" | "resolved" | string;
  original_request: string;
  quorum: number;
  deadline_at?: string;
  group_outcome?: string;
  card_message_id?: string;
  conversation_id?: string;
  created_at: string;
  updated_at: string;
  resolved_at?: string;
  legs: TaskGroupLeg[];
}

export interface TaskGroupListResponse {
  groups: TaskGroup[];
  total: number;
}

export interface TaskGroupDetailLeg {
  agent: string;
  status: string;
  job_id: string;
  run_id: string;
  input: string;
  output: string;
  result_summary: string;
  error: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  updated_at: string;
}

export interface TaskGroupDetail {
  id: string;
  workspace_id: string;
  avatar_agent: string;
  user_id: string;
  status: string;
  original_request: string;
  quorum: number;
  deadline_at?: string;
  group_outcome: string;
  resolved_at?: string;
  conversation_id: string;
  card_message_id: string;
  created_at: string;
  updated_at: string;
  legs: TaskGroupDetailLeg[];
}

export interface Job {
  id: string;
  workspace_id: string;
  project_id?: string;
  agent: string;
  agent_id?: string;
  agent_version?: number;
  identity_kind: string;
  identity_schema_version: number;
  execution_scope?: string;
  workflow_id?: string;
  workflow_version?: number;
  run_snapshot_id?: string;
  source: string;
  kind: string;
  runtime_id?: string;
  runtime_assignment?: RuntimeAssignment;
  status: string;
  priority: number;
  context_key?: string;
  trace_id?: string;
  parent_task_id?: string;
  task_group_id?: string;
  subtask_deadline_at?: string;
  payload: unknown;
  result?: unknown;
  error?: string;
  run_id?: string;
  worker_id?: string;
  lease_expires_at?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  updated_at: string;
}

export interface JobListResponse {
  jobs: Job[];
  total: number;
}

export interface RunSummary {
  run_id: string;
  agent?: string;
  tenant?: string;
  step?: string;
  status: string;
  stop_reason?: string;
  duration_ms: number;
  started_at: string;
  ended_at: string;
  schema_version: number;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
  timestamp?: string;
  parent_run_id?: string;
  parent_seq?: number;
  project_id?: string;
  conversation_id?: string;
  attribution: "project_attributed" | "legacy_unattributed" | string;
}


export interface RunDetail extends Record<string, unknown> {
  run_id?: string;
  project_id?: string;
  attribution: "project_attributed" | "legacy_unattributed" | string;
}
export interface RunListResponse {
  runs: RunSummary[];
  total: number;
  limit: number;
  offset: number;
}

export interface TeamRunUsage {
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
}

export interface TeamRunTerminal {
  run_id: string;
  agent: string;
  status: string;
  stop_reason: string;
  started_at: string;
  ended_at: string;
  duration_ms: number;
  self_exclusive: TeamRunUsage;
}

export interface TeamRunRow {
  run_id: string;
  classification: string;
  terminal?: TeamRunTerminal;
}

export interface TeamRunListResponse {
  runs: TeamRunRow[];
  total: number;
  limit: number;
  offset: number;
  totals?: TeamRunUsage;
  diagnostics: {
    aggregation_complete: boolean;
    aggregation_mode: string;
  };
}

export interface RunState {
  run_id: string;
  agent: string;
  last_step: string;
  yield_phase: string;
  saved_at: string;
  state: Record<string, unknown>;
}

export interface FinalDeliverable {
  id: string;
  workspace_id: string;
  project_id?: string;
  conversation_id?: string;
  user_id: string;
  lead_avatar_id: string;
  session_id: string;
  event_id: string;
  run_id: string;
  run_snapshot_id: string;
  title: string;
  content: string;
  content_type: string;
  metadata: unknown;
  created_at: string;
}

export interface DeliverableListResponse {
  deliverables: FinalDeliverable[];
}

export interface DownloadedContent {
  blob: Blob;
  filename: string;
}
