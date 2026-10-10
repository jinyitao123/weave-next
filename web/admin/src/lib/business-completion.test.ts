import { describe, expect, it } from 'vitest'
import { businessCompletionIssue, requiredActions, setRequiredActions, syncBusinessCompletion } from './business-completion'
import { withStarterWorkflow, type BusinessCatalog, type DevelopmentDocument, type DevelopmentMember } from './teams'
import { initialGraph, WORKBENCH_RESULT_PROTOCOL } from './workflow-graph'

const convert = 'forge:action:forge_sales_lead.convert', read = 'forge:action:forge_sales_lead.read'
const catalog: BusinessCatalog = { available: true, capabilities: [
  { id: convert, name: '转为商机', effect: 'write', status: 'available' },
  { id: read, name: '读取线索', effect: 'read', status: 'available' },
] }
const member = (id: string, role: string, ids: string[] = []): DevelopmentMember => ({
  id, configuration: { display_name: id, role, engine: 'loom', runtime_id: '', model: 'm', system_prompt: 'x', business_capability_ids: ids },
  relationship: { duty: 'd', result_requirement: '', enabled: true },
})
const team = (ids: string[]): DevelopmentDocument => {
  const worker = member('办理员', 'worker', ids)
  return { name: '团队', objective: '目标', audience: [], members: [member('负责人', 'avatar'), worker],
    workflows: [{ id: 'flow', name: '业务流程', description: '', trigger_config: {}, graph_definition: initialGraph(worker) }] }
}

describe('the receipt check a flow with business actions must declare', () => {
  it('declares the writing actions, in the shape the server publishes', () => {
    const before = team([convert, read])
    expect(businessCompletionIssue(before, before.workflows[0])).toContain('业务流程')
    const after = syncBusinessCompletion(before, catalog)
    const graph = after.workflows[0].graph_definition
    expect(requiredActions(graph)).toEqual([convert])
    expect(graph.result_protocol).toBe(WORKBENCH_RESULT_PROTOCOL)
    expect(graph.delivery_contract).toMatchObject({
      version: 1, coverage: 'explicit', output: graph.output_contract, external_effects: 'required', external_effects_check_id: 'business-action-receipts',
      required_checks: [{ id: 'business-action-receipts', verifier_id: 'weave.business-action-receipts', verifier_version: 'v1', parameters: { required_capability_ids: [convert], when_authorized: true, allow_needs_input: true } }],
    })
    expect(businessCompletionIssue(after, after.workflows[0])).toBeUndefined()
    expect(syncBusinessCompletion(after, catalog)).toBe(after)
  })

  it('requires the reading actions when a flow has no writing ones', () => {
    expect(requiredActions(syncBusinessCompletion(team([read]), catalog).workflows[0].graph_definition)).toEqual([read])
  })

  it('keeps a choice the developer made and withdraws the check with the last action', () => {
    const declared = syncBusinessCompletion(team([convert, read]), catalog)
    const chosen = { ...declared, workflows: [{ ...declared.workflows[0], graph_definition: setRequiredActions(declared.workflows[0].graph_definition, [convert, read]) }] }
    expect(requiredActions(syncBusinessCompletion(chosen, catalog).workflows[0].graph_definition)).toEqual([convert, read])
    const emptied = { ...chosen, members: chosen.members.map((item) => ({ ...item, configuration: { ...item.configuration, business_capability_ids: [] } })) }
    const graph = syncBusinessCompletion(emptied, catalog).workflows[0].graph_definition
    expect(requiredActions(graph)).toBeUndefined()
    expect(graph.delivery_contract).toBeUndefined()
  })

  it('leaves a flow alone when nothing carries a business action', () => {
    const plain = team([])
    expect(syncBusinessCompletion(plain, catalog)).toBe(plain)
    expect(businessCompletionIssue(plain, plain.workflows[0])).toBeUndefined()
  })
})

describe('starting flow', () => {
  it('gives a team with one member the plain lead, member, delivery flow', () => {
    const empty: DevelopmentDocument = { ...team([]), workflows: [] }
    const started = withStarterWorkflow(empty)
    expect(started.workflows).toHaveLength(1)
    expect(started.workflows[0].graph_definition.nodes.map((node) => node.type)).toEqual(['lead', 'worker', 'deliver'])
  })
})
