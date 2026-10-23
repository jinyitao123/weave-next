import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DeploymentPage, legacyDeploymentPath, visibleDeploymentTabs } from './DeploymentPage'

vi.mock('./DeploymentConnectionPage', () => ({ DeploymentConnectionSection: () => <p>连接内容</p> }))
vi.mock('./NodesPage', () => ({ NodesSection: () => <p>节点内容</p> }))
vi.mock('./ModelsPage', () => ({ ModelsSection: () => <p>模型内容</p> }))
vi.mock('./TimingPage', () => ({ TimingSection: () => <p>时间内容</p> }))
afterEach(() => { cleanup(); vi.resetAllMocks() })

const admin = { name: '管理员', role: 'admin', source: 'forge' as const }
const developer = { name: '开发者', role: 'developer', source: 'forge' as const }

describe('deployment page', () => {
  it('keeps the connection part for administrators only', () => {
    expect(visibleDeploymentTabs(admin).map(([value]) => value)).toEqual(['connection', 'nodes', 'models', 'timing'])
    expect(visibleDeploymentTabs(developer).map(([value]) => value)).toEqual(['nodes', 'models', 'timing'])
  })
  it('sends the replaced pages to their part of the merged page', () => {
    expect(legacyDeploymentPath('/deployment-connection')).toBe('/deployment/connection')
    expect(legacyDeploymentPath('/nodes')).toBe('/deployment/nodes')
    expect(legacyDeploymentPath('/models')).toBe('/deployment/models')
    expect(legacyDeploymentPath('/deployment/models')).toBeUndefined()
    expect(legacyDeploymentPath('/teams')).toBeUndefined()
  })
  it('opens the requested part and navigates between parts', () => {
    const navigate = vi.fn()
    render(<DeploymentPage session={admin} tab="models" navigate={navigate} />)
    screen.getByText('模型内容')
    expect(screen.getByRole('tab', { name: '模型' }).getAttribute('aria-selected')).toBe('true')
    fireEvent.click(screen.getByRole('tab', { name: '执行节点' }))
    expect(navigate).toHaveBeenCalledWith('/deployment/nodes')
  })
  it('does not reveal the connection part to a developer who asks for it', () => {
    render(<DeploymentPage session={developer} tab="connection" navigate={vi.fn()} />)
    expect(screen.queryByText('连接内容')).toBeNull()
    expect(screen.queryByRole('tab', { name: '连接' })).toBeNull()
    screen.getByText('节点内容')
  })
})
