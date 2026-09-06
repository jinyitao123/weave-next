/** A compact index into the current task's outputs, members, and recorded input references. */
import { IconChevronRightOutline14 } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskDeliverable, WorkTaskMember } from './work-task-model.ts'
import css from './WorkTaskPanel.module.css'

interface Props extends PropsLocale<'weave'> {
  readonly deliverables: readonly WorkTaskDeliverable[]
  readonly members: readonly WorkTaskMember[]
  readonly brief: string
  readonly openOutput: (id: string) => void
  readonly openOutputs: () => void
  readonly openMembers: () => void
}

/**
 * Draw an output's format without borrowing status or disclosure symbols.
 * @param props - The recorded content type and filename.
 * @returns A decorative format icon; the adjacent row owns the accessible name.
 */
export function WorkFileIcon({ contentType = '', title = '' }: { readonly contentType?: string; readonly title?: string }) {
  const type = contentType.toLowerCase()
  const web = type === 'text/html' || /\.html?$/iu.test(title)
  const picture = type.startsWith('image/') || /\.(?:svg|scad|dxf)$/iu.test(title)
  const data = /(?:json|csv|tab-separated|javascript|yaml)/u.test(type) || /\.(?:py|js|cjs|ts|json|yaml|yml|csv|sh)$/iu.test(title)
  return <svg className={css.workFileIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    {web ? <><circle cx="12" cy="12" r="8.5" /><path d="M3.5 12h17M12 3.5c5 5 5 12 0 17-5-5-5-12 0-17Z" /></>
      : picture ? <><rect x="3.5" y="4.5" width="17" height="15" rx="2" /><circle cx="8" cy="9" r="1.5" /><path d="m4 17 5-5 3 3 4-5 4 5" /></>
        : data ? <><rect x="3.5" y="4.5" width="17" height="15" rx="2" /><path d="m9 9-3 3 3 3m6-6 3 3-3 3" /></>
          : <><path d="M6 3.5h8l4 4v13H6zM14 3.5v4h4M9 12h6M9 16h6" /></>}
  </svg>
}

/**
 * Keep large file sets and long member records one explicit navigation away.
 * @param props - Current-run facts and the scene owner's navigation callbacks.
 * @returns Three resource groups without initiating execution or fetching extra facts.
 */
export function WorkSceneOverview({ deliverables, members, brief, openOutput, openOutputs, openMembers, t }: Props) {
  const final = deliverables.filter(item => item.kind !== 'stage')
  // A compact format sample is an index, not a judgment of professional importance.
  const sample = [...final.filter(item => item.kind === 'summary'), ...final.filter(item => item.kind === 'final')]
  const preview: WorkTaskDeliverable[] = []
  for (const item of sample) {
    if (preview.length < 3 && !preview.some(previous => previous.contentType === item.contentType)) preview.push(item)
  }
  for (const item of sample) {
    if (preview.length < 3 && !preview.some(previous => previous.id === item.id)) preview.push(item)
  }
  const completed = members.filter(member => member.status === 'completed').length
  const active = members.filter(member => member.status === 'running').length
  const inputs = [...new Map(members.flatMap(member => member.stages.flatMap(stage => stage.inputs))
    .filter(input => input.path.trim() !== '')
    .map(input => [input.path, input])).values()]
  return <div className={css.overview}>
    <section className={css.resourceSection} aria-label={t('task.overview.outputs')}>
      <div className={css.resourceHeading}><h2>{t('task.overview.outputs')}</h2><span>{final.length}</span></div>
      {preview.length === 0 ? <p className={css.resourceEmpty}>{t('task.deliverables.noFinal')}</p> : preview.map(item => <button type="button" key={item.id} className={css.resourceRow} onClick={() => { openOutput(item.id) }}>
        <WorkFileIcon contentType={item.contentType} title={item.title} />
        <span className={css.resourceName} title={item.title}>{item.title}</span>
        <IconChevronRightOutline14 className={css.resourceArrow} />
      </button>)}
      {deliverables.length === 0 ? null : <button type="button" className={css.resourceMore} onClick={openOutputs}>
        {t('task.overview.allOutputs', { count: deliverables.length })}<IconChevronRightOutline14 />
      </button>}
    </section>
    <section className={css.resourceSection} aria-label={t('task.members')}>
      <div className={css.resourceHeading}><h2>{t('task.members')}</h2><span>{members.length}</span></div>
      <button type="button" className={`${css.resourceRow} ${css.teamResource}`} onClick={openMembers}>
        <span className={css.avatarStack} aria-hidden>{members.slice(0, 5).map((member, index) => (
          <span key={member.agentId} className={css.memberAvatar} data-tone={index % 3}>{member.name.slice(0, 1)}</span>
        ))}</span>
        <span className={css.teamResourceState}>{members.length === 0 ? t('task.members.pending') : active > 0 ? t('task.overview.activeMembers', { count: active }) : t('task.overview.completedMembers', { completed, total: members.length })}</span>
        <IconChevronRightOutline14 className={css.resourceArrow} />
      </button>
    </section>
    <section className={css.resourceSection} aria-label={t('task.overview.sources')}>
      <div className={css.resourceHeading}><h2>{t('task.overview.sources')}</h2><span>{inputs.length + (brief === '' ? 0 : 1)}</span></div>
      {brief === '' ? null : <details className={css.sourceReference}>
        <summary><WorkFileIcon /><span>{t('task.brief')}</span></summary><p>{brief}</p>
      </details>}
      {inputs.length === 0 ? null : <details className={css.sourceReference}>
        <summary><WorkFileIcon contentType="application/json" /><span>{t('task.overview.inputReferences', { count: inputs.length })}</span></summary>
        <ul className={css.sourceList}>{inputs.map(input => <li key={input.path}><strong>{input.name || input.path.split('/').at(-1)}</strong><span>{input.path}</span></li>)}</ul>
      </details>}
      {brief === '' && inputs.length === 0 ? <p className={css.resourceEmpty}>{t('task.overview.noSources')}</p> : null}
    </section>
  </div>
}
