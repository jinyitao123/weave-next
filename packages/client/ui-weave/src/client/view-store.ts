/** Browser viewing preferences; no execution state or business records live here. */
import { defineStore, type EngineStoreHandle } from '@deepseek-ai/dsh-client-store'

/** A bounded reader position retained when switching members or tabs. */
export interface WorkTaskReadingPosition { readonly top: number; readonly follow: boolean; readonly lastEvent: string }
type WorkTaskViewState = {
  followed: Record<string, string[]>
  tabs: Record<string, 'progress' | 'outputs'>
  selectedMembers: Record<string, string>
  reading: Record<string, WorkTaskReadingPosition>
  expandedOutputs: Record<string, string[]>
  outputSelection: Record<string, { id: string; request: number }>
}
type WorkTaskViewActions = {
  toggleFollow: (draft: WorkTaskViewState, runId: string, memberId: string) => void
  selectTab: (draft: WorkTaskViewState, runId: string, tab: 'progress' | 'outputs') => void
  selectMember: (draft: WorkTaskViewState, runId: string, memberId: string) => void
  rememberReading: (draft: WorkTaskViewState, key: string, position: WorkTaskReadingPosition) => void
  expandOutput: (draft: WorkTaskViewState, runId: string, id: string, expanded: boolean) => void
  showOutput: (draft: WorkTaskViewState, runId: string, id: string) => void
}

/**
 * Keep task reading choices across mounted surfaces and reloads; retain at most 100 reader positions.
 * @returns The registration-owned preference store handle.
 */
export function createWorkTaskViewStore(): EngineStoreHandle<WorkTaskViewState, WorkTaskViewActions> {
  return defineStore({
    init: (): WorkTaskViewState => ({ followed: {}, tabs: {}, selectedMembers: {}, reading: {}, expandedOutputs: {}, outputSelection: {} }),
    persist: 'weave.workbench.member-follow.v1',
    actions: {
      selectTab: (draft, runId: string, tab: 'progress' | 'outputs') => { draft.tabs[runId] = tab },
      selectMember: (draft, runId: string, memberId: string) => { draft.selectedMembers[runId] = memberId; draft.tabs[runId] = 'progress' },
      rememberReading: (draft, key: string, position: WorkTaskReadingPosition) => {
        const entries = Object.entries(draft.reading).filter(([id]) => id !== key).slice(-99)
        draft.reading = Object.fromEntries([...entries, [key, position]])
      },
      expandOutput: (draft, runId: string, id: string, expanded: boolean) => {
        const current = draft.expandedOutputs[runId] ?? []
        draft.expandedOutputs[runId] = expanded ? [...current.filter(item => item !== id), id] : current.filter(item => item !== id)
      },
      showOutput: (draft, runId: string, id: string) => {
        draft.tabs[runId] = 'outputs'
        draft.outputSelection[runId] = { id, request: (draft.outputSelection[runId]?.request ?? 0) + 1 }
        const expanded = draft.expandedOutputs[runId] ?? []
        draft.expandedOutputs[runId] = [...expanded.filter(item => item !== id), id]
      },
      toggleFollow: (draft, runId: string, memberId: string) => {
        const current = draft.followed[runId] ?? []
        if (current.includes(memberId)) draft.followed[runId] = current.filter(id => id !== memberId)
        else if (current.length < 3) draft.followed[runId] = [...current, memberId]
      },
    },
  })
}
