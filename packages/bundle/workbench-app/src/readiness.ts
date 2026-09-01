/** Host-only Weave connection inspection with no secret material in its result. */

export type WeaveReadinessTone = 'pass' | 'warning' | 'fail'

export interface WeaveReadinessCheck {
  readonly id: 'credential' | 'service' | 'access' | 'teams' | 'runtimes'
  readonly tone: WeaveReadinessTone
  readonly detail: string
}

export interface WeaveReadiness {
  readonly status: 'ready' | 'attention' | 'unconfigured'
  readonly checkedAt: string
  readonly serviceVersion: string
  readonly teamCount: number
  readonly dispatchableTeamCount: number
  readonly runtimeCount: number
  readonly healthyRuntimeCount: number
  readonly checks: readonly WeaveReadinessCheck[]
}

type Fetch = (input: string | URL, init?: RequestInit) => Promise<Response>

function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined
}

function collection(value: unknown, key: string): readonly unknown[] {
  if (Array.isArray(value)) return value
  const nested = object(value)?.[key]
  return Array.isArray(nested) ? nested : []
}

async function json(response: Response): Promise<unknown> {
  try { return await response.json() as unknown } catch { return undefined }
}

function requestHeaders(apiKey: string): Headers {
  const headers = new Headers({ Accept: 'application/json' })
  if (apiKey !== '') headers.set('Authorization', `Bearer ${apiKey}`)
  return headers
}

/** Inspect only the minimum facts required before a first team dispatch. */
export async function inspectWeaveReadiness(
  apiUrl: string,
  apiKey: string,
  fetcher: Fetch = fetch,
): Promise<WeaveReadiness> {
  const checkedAt = new Date().toISOString()
  if (apiKey === '') {
    return {
      status: 'unconfigured', checkedAt, serviceVersion: '', teamCount: 0, dispatchableTeamCount: 0,
      runtimeCount: 0, healthyRuntimeCount: 0,
      checks: [
        { id: 'credential', tone: 'fail', detail: 'WEAVE_API_KEY is not configured on the Workbench host.' },
        { id: 'service', tone: 'warning', detail: 'Service availability has not been verified.' },
        { id: 'access', tone: 'warning', detail: 'Workspace access has not been verified.' },
        { id: 'teams', tone: 'warning', detail: 'No dispatchable team has been verified.' },
        { id: 'runtimes', tone: 'warning', detail: 'No available runtime has been verified.' },
      ],
    }
  }

  const headers = requestHeaders(apiKey)
  let healthResponse: Response | undefined
  try {
    healthResponse = await fetcher(`${apiUrl}/v1/health`, { headers, signal: AbortSignal.timeout(5_000) })
  } catch { /* The service check below owns the failure. */ }
  const health = healthResponse?.ok === true ? await json(healthResponse) : undefined
  const serviceVersion = typeof object(health)?.version === 'string' ? String(object(health)?.version) : ''
  if (healthResponse?.ok !== true) {
    return {
      status: 'attention', checkedAt, serviceVersion: '', teamCount: 0, dispatchableTeamCount: 0,
      runtimeCount: 0, healthyRuntimeCount: 0,
      checks: [
        { id: 'credential', tone: 'pass', detail: 'Workbench has a host-only Weave credential.' },
        { id: 'service', tone: 'fail', detail: 'Weave service is unreachable.' },
        { id: 'access', tone: 'warning', detail: 'Workspace access cannot be checked while the service is offline.' },
        { id: 'teams', tone: 'warning', detail: 'Team availability cannot be checked while the service is offline.' },
        { id: 'runtimes', tone: 'warning', detail: 'Runtime availability cannot be checked while the service is offline.' },
      ],
    }
  }

  const authorized = async (path: string): Promise<Response | undefined> => {
    try { return await fetcher(`${apiUrl}${path}`, { headers, signal: AbortSignal.timeout(10_000) }) }
    catch { return undefined }
  }
  const [teamsResponse, runtimesResponse] = await Promise.all([
    authorized('/v1/teams?status=active&include=summary'),
    authorized('/v1/runtimes'),
  ])
  const accessOK = teamsResponse?.ok === true && runtimesResponse?.ok === true
  const teamItems = teamsResponse?.ok === true ? collection(await json(teamsResponse), 'teams') : []
  const runtimeItems = runtimesResponse?.ok === true ? collection(await json(runtimesResponse), 'runtimes') : []
  const dispatchableTeamCount = teamItems.filter((candidate) => {
    const item = object(candidate)
    const team = object(item?.team) ?? item
    const summary = object(item?.summary)
    const defaultWorkflow = team?.default_workflow_id
    const published = summary?.published_workflow_count
    return typeof defaultWorkflow === 'string' && defaultWorkflow !== ''
      && typeof published === 'number' && published > 0
  }).length
  const healthyRuntimeCount = runtimeItems.filter((candidate) => {
    const item = object(candidate)
    return item?.online === true && item.enabled !== false && item.revoked_at == null && item.deleted_at == null
      && item.health_status !== 'quarantined' && item.health_status !== 'offline'
  }).length
  const ready = accessOK && dispatchableTeamCount > 0 && healthyRuntimeCount > 0
  return {
    status: ready ? 'ready' : 'attention', checkedAt, serviceVersion,
    teamCount: teamItems.length, dispatchableTeamCount,
    runtimeCount: runtimeItems.length, healthyRuntimeCount,
    checks: [
      { id: 'credential', tone: 'pass', detail: 'Workbench has a host-only Weave credential.' },
      { id: 'service', tone: 'pass', detail: serviceVersion === '' ? 'Weave service is online.' : `Weave ${serviceVersion} is online.` },
      { id: 'access', tone: accessOK ? 'pass' : 'fail', detail: accessOK ? 'Workspace access is authorized.' : 'The configured credential cannot read teams and runtimes.' },
      { id: 'teams', tone: dispatchableTeamCount > 0 ? 'pass' : 'fail', detail: `${String(dispatchableTeamCount)} of ${String(teamItems.length)} active teams can dispatch a default workflow.` },
      { id: 'runtimes', tone: healthyRuntimeCount > 0 ? 'pass' : 'fail', detail: `${String(healthyRuntimeCount)} of ${String(runtimeItems.length)} runtimes are available.` },
    ],
  }
}
