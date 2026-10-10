import type { Graph } from './graph'
import { workflowCapabilityIds, type BusinessCatalog, type DevelopmentDocument, type DevelopmentWorkflow } from './teams'
import { configureWorkflowResultProtocol, WORKBENCH_RESULT_PROTOCOL } from './workflow-graph'

// Weave publishes a flow whose members carry business actions only when the
// flow says which of them must end with a successful receipt. The desktop
// writes the same check; both clients keep one shape.
const VERIFIER = 'weave.business-action-receipts'
const CHECK_ID = 'business-action-receipts'

type Row = Record<string, unknown>
const record = (value: unknown): Row | undefined => value && typeof value === 'object' && !Array.isArray(value) ? value as Row : undefined
const checksOf = (contract: Row | undefined): unknown[] => Array.isArray(contract?.required_checks) ? contract.required_checks : []

/** The actions the flow has promised to finish, or undefined when it promises none. */
export function requiredActions(graph: Graph): string[] | undefined {
  const check = checksOf(record(graph.delivery_contract)).map(record).find((item) => item?.verifier_id === VERIFIER)
  const ids = record(check?.parameters)?.required_capability_ids
  return check && Array.isArray(ids) ? ids.filter((id): id is string => typeof id === 'string') : undefined
}

/** Declares the actions that must be finished; an empty list withdraws the declaration. */
export function setRequiredActions(graph: Graph, ids: string[]): Graph {
  const existing = record(graph.delivery_contract)
  const checks = checksOf(existing)
  const prior = checks.map(record).find((item) => item?.verifier_id === VERIFIER)
  if (!ids.length) {
    if (!prior) return graph
    const others = checks.filter((item) => record(item)?.verifier_id !== VERIFIER)
    const next: Graph = { ...graph }
    // A contract that only held this check goes with it; a larger one keeps the rest.
    if (!others.length && !Array.isArray(existing?.required_artifacts)) delete next.delivery_contract
    else {
      const contract: Row = { ...existing, required_checks: others }
      delete contract.external_effects_check_id
      delete contract.external_effects
      next.delivery_contract = contract
    }
    return next
  }
  // The receipt check reads the three-field result of the final step.
  const base = graph.result_protocol === WORKBENCH_RESULT_PROTOCOL ? graph : configureWorkflowResultProtocol({ graph_definition: graph }, true).graph_definition
  const id = typeof prior?.id === 'string' && prior.id.trim() ? prior.id : CHECK_ID
  const allowNeedsInput = record(prior?.parameters)?.allow_needs_input
  const check = {
    ...prior, id, title: prior?.title ?? '已授权业务动作具备成功回执', verifier_id: VERIFIER, verifier_version: 'v1',
    parameters: { required_capability_ids: [...ids].sort(), when_authorized: true, allow_needs_input: typeof allowNeedsInput === 'boolean' ? allowNeedsInput : true },
  }
  return { ...base, delivery_contract: {
    ...(existing ?? { version: 1, coverage: 'explicit' }),
    output: structuredClone(base.output_contract),
    required_checks: prior ? checks.map((item) => record(item)?.verifier_id === VERIFIER ? check : item) : [...checks, check],
    external_effects: 'required', external_effects_check_id: id,
  } }
}

const same = (left: string[], right: string[]) => left.length === right.length && [...left].sort().join('\n') === [...right].sort().join('\n')

// Actions that write are required by default; a flow with only reading
// actions requires those, because the check cannot be empty.
function defaultRequired(capabilities: string[], catalog?: BusinessCatalog): string[] {
  const writes = capabilities.filter((id) => catalog?.capabilities.find((capability) => capability.id === id)?.effect === 'write')
  return (writes.length ? writes : capabilities).slice(0, 16)
}

/** Keeps every flow's declaration in step with the actions its members carry. */
export function syncBusinessCompletion(document: DevelopmentDocument, catalog?: BusinessCatalog): DevelopmentDocument {
  let changed = false
  const workflows = document.workflows.map((flow) => {
    const capabilities = workflowCapabilityIds(document, flow), current = requiredActions(flow.graph_definition)
    const kept = (current ?? []).filter((id) => capabilities.includes(id))
    const wanted = !capabilities.length ? [] : kept.length ? kept : defaultRequired(capabilities, catalog)
    if (current ? same(current, wanted) && (!wanted.length || flow.graph_definition.result_protocol === WORKBENCH_RESULT_PROTOCOL) : !wanted.length) return flow
    try {
      const graph = setRequiredActions(flow.graph_definition, wanted)
      changed = true
      return { ...flow, graph_definition: graph }
    } catch {
      // The final step cannot carry the result yet; the page reports it instead.
      return flow
    }
  })
  return changed ? { ...document, workflows } : document
}

/** Why a flow with business actions cannot be published yet, in the page's words. */
export function businessCompletionIssue(document: DevelopmentDocument, flow: DevelopmentWorkflow): string | undefined {
  if (!workflowCapabilityIds(document, flow).length) return undefined
  const required = requiredActions(flow.graph_definition)
  if (required?.length && flow.graph_definition.result_protocol === WORKBENCH_RESULT_PROTOCOL) return undefined
  return `流程“${flow.name || '未命名流程'}”的成员带有业务动作，交付步骤还没有选定必须办成的业务动作`
}
