import { useEffect, useState } from 'react'
import { InlineError } from '../components/ui'
import { errorMessage } from '../lib/api'
import { readMetrics, type EffectiveTiming } from '../lib/tasks'

// What each deployment-wide bound limits, in the order a call meets them.
const timingRows: Array<[string, string, string]> = [
  ['provider_attempt_default', '单次供应方请求（缺省）', '供应方没有设置请求超时时，一次请求最多等待的时间'],
  ['member_step_limit', '成员单步上限', '一个成员步骤及其全部模型调用；成员的等待预算必须比它短'],
  ['slow_run_alert', '慢运行告警', '健康统计把运行计为慢的时长，只用于观测，不会结束运行'],
  ['shutdown_drain', '停机排空', 'pooled 执行器停机时等待在途工作的时长，0 表示不等待'],
]
const sourceLabels: Record<string, string> = { env: '部署设置', default: '缺省', fixed: '固定' }

export function formatSeconds(seconds: number): string {
  if (seconds === 0) return '0'
  if (seconds >= 60 && seconds % 60 === 0) return `${seconds / 60} 分钟`
  return `${seconds} 秒`
}

// The deployment page's "时间上限" section: the bounds in force and where each
// came from. A member's own wait budgets are frozen with it and shown there.
export function TimingSection() {
  const [timing, setTiming] = useState<EffectiveTiming>()
  const [error, setError] = useState('')
  useEffect(() => {
    void readMetrics().then((body) => { setTiming(body.timing ?? {}); setError('') }).catch((failure) => setError(errorMessage(failure)))
  }, [])
  return <div className="stack">
    <p className="muted">同时生效的上限中，最早到期的那个结束调用。成员的首个输出、输出间隔和整次调用上限在成员的“执行限制”里设置，随发布冻结。</p>
    {error ? <InlineError message={error} /> : null}
    {timing ? <table className="table">
      <thead><tr><th>上限</th><th>当前值</th><th>来源</th><th>作用</th></tr></thead>
      <tbody>{timingRows.filter(([key]) => timing[key]).flatMap(([key, label, scope]) => [<tr key={key}>
        <td>{label}</td><td>{formatSeconds(timing[key].seconds)}</td><td>{sourceLabels[timing[key].source] ?? timing[key].source}</td><td className="muted small">{scope}</td>
      </tr>, ...Object.entries(timing[key].overrides ?? {}).sort(([a], [b]) => a.localeCompare(b)).map(([workflow, seconds]) => <tr key={`${key}:${workflow}`}>
        <td>{label}：{workflow}</td><td>{formatSeconds(seconds)}</td><td>部署设置</td><td className="muted small">只对这个工作流生效（WEAVE_HEALTH_SLOW_RUN_OVERRIDES）</td>
      </tr>)])}</tbody>
    </table> : null}
  </div>
}
