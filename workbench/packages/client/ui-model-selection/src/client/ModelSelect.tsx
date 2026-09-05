/**
 * ModelSelect: the composer's named model seat (`conversation.input.model`).
 * The compact popover keeps the current model and effort together: its
 * overview offers a discrete effort slider, while the model summary drills
 * into the provider-grouped model list over the shared directory. The
 * trigger shows both model name and effort in the caption tone.
 * Data and submission ride the SAME per-session ModelDirectory as the
 * /model popup; exact-model reasoning metadata and the selected effort come
 * from the Host rather than a client-owned vocabulary. A rejected selection
 * announces through the shared transient Toast anchored to the composer
 * card; the in-menu strip with Retry remains the catalog-load surface.
 */
import {
  useEffect, useId, useMemo, useRef, useState, useSyncExternalStore,
  type CSSProperties, type KeyboardEvent, type FocusEvent,
} from 'react'
import clsx from 'clsx'
import type { ModelReasoningEffort, ModelSelection } from '@deepseek-ai/dsh-api-remotes/client'
import {
  IconCheckOutline16, IconChevronDownOutline14, IconChevronLeftOutline14,
  IconChevronRightOutline14, IconThinkOutline16, IconWarningOutline16, Toast,
} from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { ModelSelectInjected } from './slots.ts'
import css from './ModelSelect.module.css'

/** Which pane the dropdown shows: the model-and-effort overview or model list. */
type Pane = 'root' | 'model'

/** One dynamic effort row; undefined means preserve the provider default. */
interface EffortChoice {
  key: string
  effort: string | undefined
  label: string
}

/** Localize the shared effort vocabulary while preserving adapter-owned names. */
function effortName(t: PropsLocale<'model'>['t'], effort: ModelReasoningEffort): string {
  switch (effort.id) {
    case 'off': return t('effort.off')
    case 'low': return t('effort.low')
    case 'medium': return t('effort.medium')
    case 'high': return t('effort.high')
    case 'xhigh': return t('effort.xhigh')
    case 'max': return t('effort.max')
    default: return effort.name
  }
}

/**
 * Render the composer model seat.
 * @param props - owner share (locked) + injected face (shared directory
 * store/verbs) + the standard locale seat.
 * @returns the trigger and, while open, the two-level menu.
 */
export function ModelSelect(
  { locked, available, directory, load, select, t }:
  ModelSelectInjected & { locked: boolean } & PropsLocale<'model'>,
) {
  const state = useSyncExternalStore(
    fn => directory.subscribe(fn),
    () => directory.getSnapshot(),
  )
  const [open, setOpen] = useState(false)
  const [pane, setPane] = useState<Pane>('root')
  // The in-menu error strip serves catalog loads (its Retry re-runs the
  // load); a rejected SELECTION announces through the transient toast
  // instead, so the strip renders only while the latest failure-capable
  // action was a load.
  const lastActionRef = useRef<'load' | 'select'>('load')
  const [toast, setToast] = useState<{ seq: number; text: string } | null>(null)
  const toastSeq = useRef(0)
  const rootRef = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const summaryRef = useRef<HTMLButtonElement | null>(null)
  const itemRefs = useRef<(HTMLButtonElement | null)[]>([])
  const selectionPendingRef = useRef(false)
  const returnToOverviewRef = useRef(false)
  const [draftEffortIndex, setDraftEffortIndex] = useState<number | null>(null)
  const id = useId()

  const choices = useMemo(() => state.groups.flatMap(group =>
    group.models.map(model => ({
      group,
      model,
      selection: {
        provider: group.id,
        model: model.id,
        ...model.reasoning?.defaultEffort === undefined
          ? {}
          : { reasoningEffort: model.reasoning.defaultEffort },
      } satisfies ModelSelection,
    }))), [state.groups])
  const selectedIndex = state.current === null
    ? -1
    : choices.findIndex(c => c.selection.provider === state.current?.provider && c.selection.model === state.current.model)
  const currentChoice = choices[selectedIndex]
  const unavailable = state.routable === false
  const reasoning = unavailable ? undefined : currentChoice?.model.reasoning
  const effectiveEffort = state.current?.reasoningEffort ?? reasoning?.defaultEffort
  const effortChoices = useMemo<readonly EffortChoice[]>(() => reasoning === undefined
    ? []
    : [
      ...reasoning.defaultEffort === undefined
        ? [{ key: 'provider-default', effort: undefined, label: t('effort.providerDefault') }]
        : [],
      ...reasoning.efforts.map((effort: ModelReasoningEffort) => ({
        key: `effort:${effort.id}`,
        effort: effort.id,
        label: effortName(t, effort),
      })),
    ], [reasoning, t])
  const selectedEffortIndex = Math.max(0, effortChoices.findIndex(level => level.effort === effectiveEffort))
  const visibleEffortIndex = draftEffortIndex ?? selectedEffortIndex
  const effortLabel = reasoning === undefined
    ? undefined
    : effortChoices[visibleEffortIndex]?.label ?? t('effort.providerDefault')
  const effortProgress = effortChoices.length <= 1
    ? 0
    : visibleEffortIndex / (effortChoices.length - 1) * 100
  const busy = state.status === 'selecting'

  const reload = (): void => {
    lastActionRef.current = 'load'
    load()
  }

  useEffect(() => {
    if (!open) return
    const closeOutside = (event: MouseEvent): void => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', closeOutside)
    return () => { document.removeEventListener('mousedown', closeOutside) }
  }, [open])

  useEffect(() => {
    setDraftEffortIndex(null)
  }, [selectedEffortIndex])

  useEffect(() => {
    if (!open || pane !== 'root' || !returnToOverviewRef.current) return
    returnToOverviewRef.current = false
    summaryRef.current?.focus()
  }, [open, pane])

  if (!available) return null

  const show = (): void => {
    setPane(unavailable || state.current === null || choices.length === 0 ? 'model' : 'root')
    setOpen(true)
    reload()
  }

  const close = (restoreFocus = false): void => {
    setOpen(false)
    setPane('root')
    setDraftEffortIndex(null)
    returnToOverviewRef.current = false
    if (restoreFocus) queueMicrotask(() => { triggerRef.current?.focus() })
  }

  const moveFocus = (offset: number): void => {
    const items = itemRefs.current.filter(item => item !== null)
    if (items.length === 0) return
    const active = items.findIndex(item => item === document.activeElement)
    const next = (Math.max(active, 0) + offset + items.length) % items.length
    items[next]?.focus()
  }

  const onRootKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key === 'Escape' && open) {
      event.preventDefault()
      // Escape backs out of a drilled pane first, then closes.
      if (pane !== 'root') setPane('root')
      else close(true)
      return
    }
    if (!open) return
    if (event.target instanceof HTMLInputElement && event.target.type === 'range') return
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      moveFocus(event.key === 'ArrowDown' ? 1 : -1)
    }
  }

  const onBlur = (event: FocusEvent<HTMLDivElement>): void => {
    if (returnToOverviewRef.current) return
    if (event.relatedTarget instanceof Node && rootRef.current?.contains(event.relatedTarget)) return
    close()
  }

  const settleSelection = (accepted: boolean): void => {
    if (accepted) {
      setPane('root')
      return
    }
    returnToOverviewRef.current = false
    const message = directory.getSnapshot().error
    if (message !== null) {
      toastSeq.current += 1
      setToast({ seq: toastSeq.current, text: t('error.action', { message }) })
    }
  }

  const choose = (selection: ModelSelection): void => {
    if (busy || selectionPendingRef.current) return
    if (state.current?.provider === selection.provider && state.current.model === selection.model) {
      returnToOverviewRef.current = true
      setPane('root')
      return
    }
    selectionPendingRef.current = true
    returnToOverviewRef.current = true
    lastActionRef.current = 'select'
    void select(selection).then(settleSelection).finally(() => { selectionPendingRef.current = false })
  }

  const chooseEffort = (effort: string | undefined): void => {
    if (state.current === null || busy || selectionPendingRef.current) {
      setDraftEffortIndex(null)
      return
    }
    if (effectiveEffort === effort) {
      setDraftEffortIndex(null)
      return
    }
    const selection: ModelSelection = {
      provider: state.current.provider,
      model: state.current.model,
      ...effort === undefined ? {} : { reasoningEffort: effort },
    }
    selectionPendingRef.current = true
    lastActionRef.current = 'select'
    void select(selection).then((accepted) => {
      if (accepted) return
      setDraftEffortIndex(null)
      const message = directory.getSnapshot().error
      if (message === null) return
      toastSeq.current += 1
      setToast({ seq: toastSeq.current, text: t('error.action', { message }) })
    }).finally(() => { selectionPendingRef.current = false })
  }

  const waiting = state.current === null && state.status === 'loading'
  const modelLabel = waiting
    ? t('trigger.loading')
    : unavailable
      ? t('trigger.fallback')
      : currentChoice?.model.name
        ?? (state.current === null ? t('trigger.fallback') : `${state.current.provider}/${state.current.model}`)
  const triggerLabel = effortLabel === undefined ? modelLabel : `${modelLabel} · ${effortLabel}`
  const triggerAria = waiting
    ? t('trigger.loading')
    : state.current === null || unavailable
      ? t('trigger.selectAria')
      : effortLabel === undefined
        ? t('trigger.aria', { model: modelLabel })
        : t('trigger.ariaEffort', { model: modelLabel, effort: effortLabel })
  itemRefs.current = []
  let itemIndex = 0
  const itemRef = () => {
    const at = itemIndex++
    return (node: HTMLButtonElement | null) => { itemRefs.current[at] = node }
  }
  const summaryItemRef = itemRef()

  return (
    <div ref={rootRef} className={css.root} onKeyDown={onRootKeyDown} onBlur={onBlur}>
      <button
        ref={triggerRef}
        type="button"
        className={css.trigger}
        aria-label={triggerAria}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? `${id}-menu` : undefined}
        title={triggerLabel}
        disabled={locked}
        onClick={() => {
          if (open) {
            close()
          } else {
            show()
          }
        }}
      >
        <span className={css.triggerLabel}>{modelLabel}</span>
        {effortLabel !== undefined && <span className={css.triggerEffort}>{effortLabel}</span>}
        <IconChevronDownOutline14 className={clsx(css.chevron, open && css.chevronOpen)} />
      </button>

      {open && (
        <div
          id={`${id}-menu`}
          className={css.menu}
          role="dialog"
          aria-label={t('menu.aria')}
          aria-busy={state.status === 'loading' || busy}
        >
          {pane === 'root' && (
            <div className={css.overview}>
              <button
                ref={(node) => {
                  summaryRef.current = node
                  summaryItemRef(node)
                }}
                type="button"
                className={css.summary}
                onClick={() => { setPane('model') }}
              >
                <IconThinkOutline16 className={css.summaryIcon} />
                <span className={css.summaryModel}>{modelLabel}</span>
                {effortLabel !== undefined && <span className={css.summaryEffort}>{effortLabel}</span>}
                <IconChevronRightOutline14 className={css.summaryChevron} />
              </button>
              {reasoning !== undefined && effortChoices.length > 0 && (
                <div className={css.effortControl}>
                  <div className={css.effortRail} aria-hidden="true">
                    <span
                      className={css.effortFill}
                      style={{ '--effort-progress': `${String(effortProgress)}%` } as CSSProperties}
                    />
                    <span className={css.effortMarks}>
                      {effortChoices.map((level, index) => (
                        <span
                          className={clsx(css.effortMark, index <= visibleEffortIndex && css.effortMarkActive)}
                          key={level.key}
                        />
                      ))}
                    </span>
                  </div>
                  <input
                    className={css.effortSlider}
                    type="range"
                    min={0}
                    max={effortChoices.length - 1}
                    step={1}
                    value={visibleEffortIndex}
                    aria-label={t('effort.sliderAria')}
                    aria-valuetext={effortLabel}
                    onChange={(event) => {
                      setDraftEffortIndex(Number(event.currentTarget.value))
                    }}
                    onPointerUp={(event) => {
                      chooseEffort(effortChoices[Number(event.currentTarget.value)]?.effort)
                    }}
                    onPointerCancel={() => { setDraftEffortIndex(null) }}
                    onKeyUp={(event) => {
                      if (['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End', 'PageUp', 'PageDown'].includes(event.key)) {
                        chooseEffort(effortChoices[Number(event.currentTarget.value)]?.effort)
                      }
                    }}
                  />
                </div>
              )}
            </div>
          )}

          {pane === 'model' && (
            <>
              <button
                ref={itemRef()}
                type="button"
                className={css.paneBack}
                onClick={() => { setPane('root') }}
              >
                <IconChevronLeftOutline14 />
                <span>{t('menu.model')}</span>
              </button>
              {state.status === 'loading' && (
                <div className={css.status}>{t('status.loading')}</div>
              )}
              {state.error !== null && lastActionRef.current === 'load' && (
                <div className={css.error}>
                  <span>{t('error.action', { message: state.error })}</span>
                  <button type="button" className={css.retry} onClick={reload}>{t('retry')}</button>
                </div>
              )}
              {state.failures.map(failure => (
                <div className={css.warning} key={failure.id}>
                  <span>{t('warning.groupLoad', { name: failure.name, message: failure.message })}</span>
                  <button type="button" className={css.retry} onClick={reload}>{t('retry')}</button>
                </div>
              ))}
              <div className={clsx(css.groups, 'scrollable')} role="menu">
                {state.groups.map((group) => {
                  const headingId = `${id}-${group.id}`
                  return (
                    <section role="group" aria-labelledby={headingId} className={css.group} key={group.id}>
                      <div className={css.groupTitle} id={headingId}>{group.name}</div>
                      {group.models.map((model) => {
                        const selected = state.current?.provider === group.id && state.current.model === model.id
                        return (
                          <button
                            ref={itemRef()}
                            type="button"
                            role="menuitemradio"
                            aria-checked={selected}
                            className={clsx(css.option, selected && css.selected)}
                            key={model.id}
                            title={model.name}
                            aria-disabled={busy}
                            onClick={() => { choose({ provider: group.id, model: model.id }) }}
                          >
                            <span className={css.optionCopy}>
                              <span className={css.modelName}>{model.name}</span>
                            </span>
                            <span className={css.check}>
                              {selected ? <IconCheckOutline16 /> : null}
                            </span>
                          </button>
                        )
                      })}
                    </section>
                  )
                })}
              </div>
              {state.status === 'ready' && choices.length === 0 && (
                <div className={css.empty}>{t('empty.models')} {t('empty.configure')}</div>
              )}
            </>
          )}

        </div>
      )}
      {toast !== null && (
        <Toast
          key={toast.seq}
          text={toast.text}
          icon={<IconWarningOutline16 />}
          anchor={rootRef.current?.closest<HTMLElement>('[data-composer-card]') ?? null}
          onDone={() => { setToast(null) }}
        />
      )}
    </div>
  )
}
