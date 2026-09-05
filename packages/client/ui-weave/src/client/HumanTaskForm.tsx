import { useEffect, useState } from 'react'
import { MarkdownText } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskHumanTask } from './work-task-model.ts'
import css from './WorkTaskPanel.module.css'

type Schema = Readonly<Record<string, unknown>>
const object = (value: unknown): Schema | undefined => value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Schema : undefined
const string = (value: unknown): string => typeof value === 'string' ? value : ''

function supported(schema: Schema, depth = 0): boolean {
  if (depth > 6 || ['$ref', 'oneOf', 'anyOf', 'allOf', 'if', 'not'].some(key => key in schema)) return false
  if (schema.const !== undefined) return true
  if (Array.isArray(schema.enum)) return schema.enum.every(value => ['string', 'number', 'boolean'].includes(typeof value))
  if (schema.type === 'object') return Object.values(object(schema.properties) ?? {}).every(value => object(value) !== undefined && supported(object(value) ?? {}, depth + 1))
  return ['string', 'number', 'integer', 'boolean'].includes(string(schema.type))
}

function initial(schema: Schema, root = true): unknown {
  if (schema.const !== undefined) return schema.const
  if (schema.type === 'object') {
    const entries = Object.entries(object(schema.properties) ?? {})
      .map(([key, child]) => [key, initial(object(child) ?? {}, false)] as const).filter(([, value]) => value !== undefined)
    return entries.length > 0 || root ? Object.fromEntries(entries) : undefined
  }
  return undefined
}

interface Props extends PropsLocale<'weave'> {
  readonly task: WorkTaskHumanTask
  readonly id: string
  readonly disabled: boolean
  readonly submit: (payload: unknown) => Promise<string | null>
  readonly discuss?: (() => void) | undefined
  readonly rejectionId: string
}

/**
 * Answer the current human wait using the workflow's declared field names and types.
 * @param props - The exact current question, explicit submit action, and localized labels.
 * @returns A schema-backed response form or an honest unsupported-schema explanation.
 */
export function HumanTaskForm({ task, id, disabled, submit, discuss, rejectionId, t }: Props) {
  const [payload, setPayload] = useState<unknown>(() => initial(task.resumeSchema))
  const [pending, setPending] = useState(false)
  const [recorded, setRecorded] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => { if (rejectionId !== '') setRecorded(false) }, [rejectionId])
  const simple = supported(task.resumeSchema)
  const field = (schema: Schema, value: unknown, change: (next: unknown) => void, path: string, label: string, required: boolean) => {
    if (schema.const !== undefined) return <p key={path}>{label}: {JSON.stringify(schema.const)}</p>
    if (schema.type === 'object') {
      const current = object(value) ?? {}
      const requiredKeys = Array.isArray(schema.required) ? schema.required : []
      return <fieldset key={path} className={css.humanFields}><legend>{label}</legend>
        {Object.entries(object(schema.properties) ?? {}).map(([key, raw]) => {
          const child = object(raw) ?? {}
          return field(child, current[key], (next) => { change({ ...current, [key]: next }) },
            `${path}-${key}`, string(child.title) || key, requiredKeys.includes(key))
        })}</fieldset>
    }
    const fieldId = `${id}-${path}`
    const choices = Array.isArray(schema.enum) ? schema.enum : schema.type === 'boolean' ? [true, false] : null
    return <div className={css.humanField} key={path}>
      <label htmlFor={fieldId}>{label}{required ? ` · ${t('task.human.required')}` : ''}</label>
      {string(schema.description) === '' ? null : <p id={`${fieldId}-help`}>{string(schema.description)}</p>}
      {choices !== null ? <select id={fieldId} aria-describedby={string(schema.description) === '' ? undefined : `${fieldId}-help`} required={required} value={value === undefined ? '' : JSON.stringify(value)} onChange={(event) =>{  change(event.currentTarget.value === '' ? undefined : JSON.parse(event.currentTarget.value) as unknown) }}>
        <option value="">{t('task.human.choose')}</option>{choices.map(choice => <option key={JSON.stringify(choice)} value={JSON.stringify(choice)}>{typeof choice === 'boolean' ? t(choice ? 'task.human.yes' : 'task.human.no') : String(choice)}</option>)}
      </select> : schema.type === 'string' ? <textarea id={fieldId} aria-describedby={string(schema.description) === '' ? undefined : `${fieldId}-help`} required={required} minLength={typeof schema.minLength === 'number' ? schema.minLength : undefined} maxLength={typeof schema.maxLength === 'number' ? schema.maxLength : undefined} value={string(value)} onChange={(event) =>{  change(event.currentTarget.value) }} />
        : <input id={fieldId} type="number" required={required} step={schema.type === 'integer' ? 1 : 'any'} min={typeof schema.minimum === 'number' ? schema.minimum : undefined} max={typeof schema.maximum === 'number' ? schema.maximum : undefined} value={typeof value === 'number' ? value : ''} onChange={(event) =>{  change(event.currentTarget.value === '' ? undefined : event.currentTarget.valueAsNumber) }} />}
    </div>
  }
  return <section className={css.humanTask} aria-label={task.title || t('task.human')}>
    <h3>{task.title || t('task.human')}</h3>
    {task.instructions === '' ? null : <MarkdownText text={task.instructions} labels={{ code: { copyLabel: t('task.document.copy'), copiedLabel: t('task.document.copied') }, footnotes: t('task.document.footnotes') }} />}
    {simple ? <form onSubmit={(event) => {
      event.preventDefault()
      if (disabled || pending || recorded) return
      setPending(true); setError(null)
      void submit(payload ?? initial(task.resumeSchema)).then((result) => { setError(result); if (result === null) setRecorded(true) }).catch(() => { setError(t('task.action.offline')) }).finally(() => { setPending(false) })
    }}>
      <fieldset className={css.humanFields} disabled={disabled || pending || recorded}>
        {field(task.resumeSchema, payload, setPayload, 'answer', t('task.human.response'), true)}
        <button type="submit" className={css.primaryButton}>{t(recorded || pending ? 'task.human.recorded' : 'task.human.submit')}</button>
      </fieldset>
    </form> : <p>{t('task.human.complex')}</p>}
    {error === null ? null : <p role="alert">{error}</p>}
    {discuss === undefined ? null : <button type="button" className={css.secondaryButton} onClick={discuss}>{t('task.human.discuss')}</button>}
  </section>
}
