import { apiResponse, ApiError } from './api'

export interface OrganizationConnection { version: 1; forgeOrigin: string; weaveOrigin: string }

export async function readOrganizationConnection(): Promise<{ config: OrganizationConnection; file: Blob }> {
  const response = await apiResponse('/v1/admin/organization-connection', { cache: 'no-store' })
  const file = await response.blob()
  if (file.size > 8192) throw new ApiError(503, 'organization_connection_invalid')
  let config: OrganizationConnection
  try { config = JSON.parse(await file.text()) as OrganizationConnection } catch { throw new ApiError(503, 'organization_connection_invalid') }
  if (!config || config.version !== 1 || Object.keys(config).length !== 3 || typeof config.forgeOrigin !== 'string' || typeof config.weaveOrigin !== 'string') throw new ApiError(503, 'organization_connection_invalid')
  return { config, file }
}

export function saveOrganizationConnection(file: Blob) {
  const url = URL.createObjectURL(file)
  const link = document.createElement('a')
  link.href = url; link.download = 'organization-connection.json'
  document.body.append(link); link.click(); link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
