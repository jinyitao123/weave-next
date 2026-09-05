// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { WorkTaskCommandRow } from '../src/client/WorkTaskCommandRow.tsx'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)
afterEach(cleanup)

describe('legacy Weave task action history', () => {
  it('shows business activity without command names, arguments, or raw failures', () => {
    const node = {
      name: 'weave-correct', args: 'runtime-id raw-instruction', outcome: { kind: 'error', text: 'backend failed with runtime-id' },
    } as unknown as Parameters<typeof WorkTaskCommandRow>[0]['node']
    const props = { node, t } as unknown as Parameters<typeof WorkTaskCommandRow>[0]
    const view = render(<WorkTaskCommandRow {...props} />)
    expect(view.container.textContent).toBe('调整团队执行未完成')
    expect(view.container.textContent).not.toContain('weave-correct')
    expect(view.container.textContent).not.toContain('runtime-id')
    expect(view.container.textContent).not.toContain('backend failed')
  })
})
