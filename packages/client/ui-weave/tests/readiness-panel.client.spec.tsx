// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { ReadinessSection } from '../src/client/ReadinessPanel.tsx'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('Weave readiness presentation', () => {
  it('renders localized product facts instead of host diagnostics', async () => {
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => Promise.resolve(Response.json({
      status: 'ready', checkedAt: new Date().toISOString(), serviceVersion: '1.0.0',
      teamCount: 10, dispatchableTeamCount: 2, runtimeCount: 1, healthyRuntimeCount: 1,
      checks: [
        { id: 'credential', tone: 'pass', detail: 'Workbench has a host-only Weave credential.' },
        { id: 'service', tone: 'pass', detail: 'Weave 1.0.0 is online.' },
        { id: 'access', tone: 'pass', detail: 'Workspace access is authorized.' },
        { id: 'teams', tone: 'pass', detail: '2 of 10 active teams can dispatch a default workflow.' },
        { id: 'runtimes', tone: 'pass', detail: '1 of 1 runtimes are available.' },
      ],
    }))))
    const props = { t } as unknown as Parameters<typeof ReadinessSection>[0]
    const view = render(<ReadinessSection {...props} />)

    expect(await view.findByText('Workbench 已安全连接 Weave。')).toBeTruthy()
    expect(view.container.textContent).toContain('10 支活跃团队中，2 支可以直接派发。')
    expect(view.container.textContent).toContain('1 个运行节点中，1 个当前可用。')
    expect(view.container.textContent).not.toContain('host-only')
    expect(view.container.textContent).not.toContain('authorized')
  })
})
