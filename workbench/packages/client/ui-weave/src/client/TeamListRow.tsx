import { useRef, useState, type ReactNode } from 'react'
import { IconSparkle16, StateDot } from '@deepseek-ai/dsh-client-ui-primitives'
import type { ToolCallViewProps } from '@deepseek-ai/dsh-client-ui-tool/client'
import type { InjectFace, PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import css from './TeamListRow.module.css'

interface TeamListInjected {
  readonly selectTeam?: (teamId: string, teamName: string) => Promise<void>
}

type TeamListProps = ToolCallViewProps & PropsLocale<'weave'> & InjectFace<TeamListInjected>
type TeamListState = 'running' | 'ok' | 'empty' | 'error' | 'stopped' | 'invalid'

interface TeamCandidate {
  readonly teamId: string
  readonly name: string
  readonly status: string
  readonly objective: string
  readonly primaryScenario: string
  readonly successCriteria: string
  readonly responsibilities: readonly string[]
  readonly workflowAvailable: boolean
}

interface TeamListModel {
  readonly state: TeamListState
  readonly teams: readonly TeamCandidate[]
  readonly detail: string | null
}

function resultText(block: ToolCallViewProps['block']): string | null {
  if (!('kind' in block)) return null
  const parts = block.content.map(item => item.type === 'text' ? item.text : JSON.stringify(item, null, 2))
  if (parts.length === 0 && block.error !== undefined) parts.push(`${block.error.name}: ${block.error.code}`)
  return parts.join('\n') || null
}

function stringField(record: Record<string, unknown>, key: string): string {
  const value = record[key]
  return typeof value === 'string' ? value.trim() : ''
}

function candidate(value: unknown): TeamCandidate | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null
  const record = value as Record<string, unknown>
  const teamId = stringField(record, 'team_id')
  const name = stringField(record, 'name')
  if (teamId === '' || name === '') return null
  const responsibilities = Array.isArray(record.responsibilities)
    ? record.responsibilities.filter((item): item is string => typeof item === 'string' && item.trim() !== '')
    : []
  return {
    teamId,
    name,
    status: stringField(record, 'status'),
    objective: stringField(record, 'objective'),
    primaryScenario: stringField(record, 'primary_scenario'),
    successCriteria: stringField(record, 'success_criteria'),
    responsibilities,
    workflowAvailable: record.workflow_available === true,
  }
}

export function teamListModel(block: ToolCallViewProps['block']): TeamListModel {
  if (!('kind' in block)) return { state: 'running', teams: [], detail: null }
  const detail = resultText(block)
  if (block.error?.code === 'interrupted') return { state: 'stopped', teams: [], detail }
  if (block.isError) return { state: 'error', teams: [], detail }
  if (detail === null) return { state: 'invalid', teams: [], detail: null }
  try {
    const parsed = JSON.parse(detail) as unknown
    if (!Array.isArray(parsed)) return { state: 'invalid', teams: [], detail }
    const teams = parsed.map(candidate).filter((item): item is TeamCandidate => item !== null)
    if (parsed.length > 0 && teams.length === 0) return { state: 'invalid', teams: [], detail }
    return { state: teams.length === 0 ? 'empty' : 'ok', teams, detail: null }
  } catch {
    return { state: 'invalid', teams: [], detail }
  }
}

function leading(state: TeamListState): ReactNode {
  if (state === 'error' || state === 'invalid') return <StateDot state="error" />
  if (state === 'stopped') return <StateDot state="warning" />
  return <IconSparkle16 size={14} />
}

function statusLabel(team: TeamCandidate, t: TeamListProps['t']): string {
  if (team.status === 'archived') return t('teamList.archived')
  if (team.status === 'building') return t('teamList.building')
  if (team.status === 'needs_repair') return t('teamList.needsRepair')
  return team.status === 'active' && team.workflowAvailable
    ? t('teamList.dispatchable')
    : t('teamList.noWorkflow')
}

function displayName(value: string, t: TeamListProps['t']): string {
  const name = value.trim()
  if (/^[0-9a-f]{8}-[0-9a-f-]{27,}$/iu.test(name)) return t('teamList.unnamed')
  if (!/^[a-z0-9_-]+$/u.test(name) || (!name.includes('-') && !name.includes('_'))) return name
  return name.split(/[-_]+/u).filter(Boolean).map(word => word.charAt(0).toUpperCase() + word.slice(1)).join(' ')
}

function summary(model: TeamListModel, t: TeamListProps['t']): string {
  switch (model.state) {
    case 'running': return t('teamList.running')
    case 'error': return t('teamList.failed')
    case 'stopped': return t('teamList.stopped')
    case 'invalid': return t('teamList.invalid')
    case 'empty': return t('teamList.empty')
    case 'ok': return t('teamList.found', { count: model.teams.length })
  }
}

function TeamCard({ team, t, selectTeam }: { team: TeamCandidate; t: TeamListProps['t']; selectTeam?: TeamListInjected['selectTeam'] }) {
  const submitting = useRef(false)
  const [selection, setSelection] = useState<'idle' | 'pending' | 'done' | 'error'>('idle')
  const purpose = team.objective || team.primaryScenario
  const dispatchable = team.status === 'active' && team.workflowAvailable
  const select = async () => {
    if (submitting.current || selectTeam === undefined) return
    submitting.current = true
    setSelection('pending')
    try {
      await selectTeam(team.teamId, team.name)
      setSelection('done')
    } catch {
      setSelection('error')
    } finally {
      submitting.current = false
    }
  }
  return (
    <article
      className={css.team}
      data-dispatchable={dispatchable || undefined}
    >
      <div className={css.teamHeader}>
        <strong className={css.teamName}>{displayName(team.name, t)}</strong>
        <span className={css.workflowState}>{statusLabel(team, t)}</span>
      </div>
      {purpose !== '' ? <p className={css.purpose}>{purpose}</p> : null}
      {team.primaryScenario !== '' && team.primaryScenario !== purpose ? (
        <p className={css.fact}><span>{t('teamList.scenario')}</span>{team.primaryScenario}</p>
      ) : null}
      {team.responsibilities.length > 0 ? (
        <p className={css.fact}>
          <span>{t('teamList.responsibilities')}</span>
          {team.responsibilities.slice(0, 3).join(' · ')}
        </p>
      ) : null}
      {team.successCriteria !== '' ? (
        <p className={css.fact}><span>{t('teamList.success')}</span>{team.successCriteria}</p>
      ) : null}
      {!dispatchable || selectTeam === undefined ? null : (
        <button className={css.selectButton} type="button" disabled={selection === 'pending' || selection === 'done'} onClick={() => { void select() }}>
          {t(selection === 'pending' ? 'teamList.selecting' : selection === 'done' ? 'teamList.selected' : 'teamList.select')}
        </button>
      )}
      {selection === 'error' ? <p role="alert">{t('teamList.selectionFailed')}</p> : null}
    </article>
  )
}

/** Render Weave's team-list result as candidate facts rather than raw MCP JSON. */
export function TeamListRow({ block, selectTeam, useChat, useProjection, t }: TeamListProps) {
  const projection = useProjection('workTask')
  const superseded = useChat((snapshot) => {
    if (!('kind' in block)) return false
    for (const node of snapshot.nodes.values()) {
      if (node.anchorSeq <= block.seq) continue
      if (node.kind === 'user' || node.kind === 'steering') return true
      if (node.kind !== 'tool-call') continue
      const root = (node.data as { root?: ToolCallViewProps['block'] }).root
      if (root === undefined) continue
      const name = 'kind' in root ? root.call?.name ?? '' : root.name
      if (['mcp__weave__team_list', 'mcp__weave__team_create', 'mcp__weave__team_dispatch'].includes(name)) return true
    }
    return false
  }) || (projection != null && (projection.runId !== '' || projection.preparation !== undefined))
  const model = teamListModel(block)
  return (
    <section className={css.card} data-tool="mcp__weave__team_list" data-state={model.state}>
      <header className={css.header}>
        <span className={css.leading}>{leading(model.state)}</span>
        <span className={css.title}>{t('teamList.title')}</span>
        <span className={css.separator} aria-hidden />
        <span className={css.summary}>{summary(model, t)}</span>
      </header>
      {superseded ? <p className={css.summary}>{t('teamList.superseded')}</p> : null}
      {model.state === 'ok' ? (
        <div className={css.teams} aria-label={summary(model, t)}>
          {model.teams.map(team => <TeamCard key={team.teamId} team={team} t={t} selectTeam={superseded ? undefined : selectTeam} />)}
        </div>
      ) : null}
    </section>
  )
}
