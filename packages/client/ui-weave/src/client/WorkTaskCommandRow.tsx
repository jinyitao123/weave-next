import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './WorkTaskCommandRow.module.css'

type Props = PropsRuntime<'conversation.chat.commandview'> & PropsLocale<'weave'>

function actionKey(name: string) {
  if (name === 'weave-stop') return 'task.command.stop' as const
  if (name === 'weave-rerun') return 'task.command.rerun' as const
  if (name === 'weave-correct') return 'task.command.correct' as const
  if (name === 'weave-confirm-correction') return 'task.command.confirmCorrection' as const
  if (name === 'weave-retry-stage') return 'task.command.retryStage' as const
  if (name === 'weave-assess') return 'task.command.assess' as const
  return 'task.command.action' as const
}

/** Product history for legacy Workbench actions recorded through the command plane. */
export function WorkTaskCommandRow({ node, t }: Props) {
  const name = node.name === null ? '' : node.name
  const action = actionKey(name)
  const state = node.outcome === null ? 'running' : node.outcome.kind === 'error' ? 'error' : 'done'
  return (
    <div className={css.row} data-state={state}>
      <span className={css.dot} aria-hidden />
      <span className={css.text}><strong>{t(action)}</strong><small>{t(`task.command.status.${state}` as const)}</small></span>
    </div>
  )
}
