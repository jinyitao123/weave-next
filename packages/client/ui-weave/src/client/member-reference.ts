/** A member reference discusses an exact run and its existing records without submitting a correction. */
import type { InputTriggerSource, ReferenceInsert } from '@deepseek-ai/dsh-client-ui-input-trigger/client'

/** Stable task facts carried behind a readable composer reference chip. */
export interface WorkTaskMemberReference {
  readonly runId: string
  readonly memberId: string
  readonly memberName: string
  readonly stages: readonly {
    readonly nodeId: string
    readonly name: string
    readonly outputIds: readonly string[]
    readonly outputTitles: readonly string[]
  }[]
}

/**
 * Project the selected member to a readable input chip; exact IDs stay in its serialization.
 * @param member - The selected run, member, and recorded stage/output references.
 * @param label - Product-owned visible label.
 * @returns A source-owned reference that preserves the current composer draft.
 */
export function memberReference(member: WorkTaskMemberReference, label: string): ReferenceInsert {
  return { source: 'weave-member', ref: JSON.stringify(member), label, clipboardText: label, appearance: 'session' }
}

/**
 * Register serialization only; this source does not claim ordinary messages or run commands.
 * @param instruction - Context and the existing discussion/correction flow included with an explicit submission.
 * @returns The input source whose references retain exact task identities across draft persistence.
 */
export function memberReferenceSource(instruction: string): InputTriggerSource {
  return {
    trigger: '@', name: 'weave-member', showGroupTitle: false,
    candidates: () => Promise.resolve([]), onPick: () => undefined,
    codec: {
      clipboardText: (ref) => {
        const member = JSON.parse(ref) as WorkTaskMemberReference
        return member.memberName
      },
      serialize: ref => Promise.resolve(`${instruction}\n<weave_member_reference>${JSON.stringify(JSON.parse(ref) as unknown)}</weave_member_reference>`),
    },
  }
}
