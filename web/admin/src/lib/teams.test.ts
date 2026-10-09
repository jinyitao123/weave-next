import { describe, expect, it } from 'vitest'
import { nodeReadinessIssues, type DevelopmentDocument, type DevelopmentMember } from './teams'

const member = (name: string, engine: string, runtime = '', enabled = true): DevelopmentMember => ({
  id: name,
  configuration: { display_name: name, role: 'worker', engine, runtime_id: runtime, model: '', system_prompt: '' },
  relationship: { duty: '', result_requirement: '', enabled },
})
const team = (...members: DevelopmentMember[]): DevelopmentDocument => ({ name: '团队', objective: '', audience: [], members, workflows: [] })

describe('nodeReadinessIssues', () => {
  it('does not ask built-in engines for a node', () => {
    expect(nodeReadinessIssues(team(member('检查员', 'loom')), {})).toEqual([])
  })

  it('reports a CLI member once when it has neither an accepting node nor a pinned node', () => {
    expect(nodeReadinessIssues(team(member('验证', 'codex')), {})).toEqual(['“验证”使用的 Codex 当前没有可接任务的节点，也还没有指定节点'])
  })

  it('keeps the node-less line for a pinned member whose engine has no accepting node', () => {
    expect(nodeReadinessIssues(team(member('编码', 'claude', 'node-1')), {})).toEqual(['“编码”使用的 Claude 当前没有可接任务的节点'])
  })

  it('still asks an engine-ready member to pin a node', () => {
    expect(nodeReadinessIssues(team(member('编码', 'claude')), { claude: 1 })).toEqual(['“编码”还没有指定节点'])
  })

  it('ignores disabled members for the accepting-node check', () => {
    expect(nodeReadinessIssues(team(member('停用', 'codex', 'node-1', false)), {})).toEqual([])
  })
})
