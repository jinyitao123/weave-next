import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../lib/api'
import { readOrganizationConnection, saveOrganizationConnection } from '../lib/organization-connection'
import { DeploymentConnectionPage } from './DeploymentConnectionPage'

vi.mock('../lib/organization-connection', () => ({ readOrganizationConnection: vi.fn(), saveOrganizationConnection: vi.fn() }))
afterEach(() => { cleanup(); vi.resetAllMocks() })
const session = { name: '管理员', role: 'admin', source: 'forge' as const }
const config = { version: 1 as const, forgeOrigin: 'http://forge.example.test:8080', weaveOrigin: 'https://teams.example.test' }
const file = new Blob(['exact server bytes'])

describe('deployment connection', () => {
  it('shows both installed addresses and downloads the freshly fetched server file', async () => {
    vi.mocked(readOrganizationConnection).mockResolvedValue({ config, file })
    render(<DeploymentConnectionPage session={session} />)
    await screen.findByText(config.forgeOrigin)
    screen.getByText(config.weaveOrigin)
    fireEvent.click(screen.getByRole('button', { name: '下载组织连接文件' }))
    await waitFor(() => expect(saveOrganizationConnection).toHaveBeenCalledWith(file))
    expect(readOrganizationConnection).toHaveBeenCalledTimes(2)
  })
  it('refreshes changed addresses for inspection before allowing their download', async () => {
    vi.mocked(readOrganizationConnection).mockResolvedValueOnce({ config, file }).mockResolvedValue({ config: { ...config, weaveOrigin: 'https://new.example.test' }, file })
    render(<DeploymentConnectionPage session={session} />)
    await screen.findByText(config.forgeOrigin)
    fireEvent.click(screen.getByRole('button', { name: '下载组织连接文件' }))
    await screen.findByText('https://new.example.test')
    expect(saveOrganizationConnection).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '下载组织连接文件' }))
    await waitFor(() => expect(saveOrganizationConnection).toHaveBeenCalledTimes(1))
  })
  it('refuses developers without even requesting the connection file', () => {
    render(<DeploymentConnectionPage session={{ ...session, role: 'developer' }} />)
    expect(screen.queryByRole('button', { name: '下载组织连接文件' })).toBeNull()
    expect(readOrganizationConnection).not.toHaveBeenCalled()
  })
  it('explains missing state and offers retry instead of a download', async () => {
    vi.mocked(readOrganizationConnection).mockRejectedValue(new ApiError(503, 'organization_connection_unavailable'))
    render(<DeploymentConnectionPage session={session} />)
    await screen.findByText(/部署连接尚未配置/)
    expect(screen.queryByRole('button', { name: '下载组织连接文件' })).toBeNull()
    vi.mocked(readOrganizationConnection).mockResolvedValue({ config, file })
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await screen.findByText(config.forgeOrigin)
  })
})
