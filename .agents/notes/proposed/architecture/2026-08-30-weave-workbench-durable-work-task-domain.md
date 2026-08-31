# Agent Note: Minimal Weave Workbench task architecture

Status: proposed

English | [中文](2026-08-30-weave-workbench-durable-work-task-domain.zh.md)

## Problem

Weave Workbench must make durable team execution understandable and controllable for an FDE. A user should be able to choose or confirm a team, leave the conversation while Weave continues, return to see who is doing what and on which runtime, respond to HumanTasks, inspect deliverables, stop a wrong run, revise the brief, and start a corrected attempt.

The previous proposal met that product goal but introduced too much architecture before the first real workflow had proved the need. It combined an independent WorkTask database, WorkStreams, attempts, command records, multiple local state machines, multi-authority recovery, reliable Remote streaming, safe-point pause, fences, causal impact plans, and a side-effect ledger. Each mechanism could be justified separately, but together they formed a second orchestration platform around Weave.

## Proposal

Build the complete user loop with the smallest ownership model:

- Weave remains the only execution authority.
- Workbench presents one durable work view from the existing DSH Session projection.
- The Browser never owns remote execution state.
- Phase 1 persists only run correlations and one unresolved local action before a network call.
- User-visible control is whole-run stop followed by a confirmed new run.
- Multi-team grouping and lossless in-place intervention remain evidence-gated extensions, not committed foundations.

The product concept is still called `WorkTask`, but Phase 1 does not create an independent WorkTask storage domain. One durable supervising Session projects one work item and retains its ordered Weave run attempts. Closing the conversation, disposing its foreground Agent, or closing the application does not cancel Weave execution.

## Minimalist adversarial review

One skeptical architecture reviewer and the primary reviewer attacked duplicated state, premature domain objects, excess endpoints, misleading intervention scope, and recovery behavior. The review produced five binding conclusions:

1. Persist Weave run status only in Weave. Workbench stores freshness and at most one unresolved `pendingAction`; labels such as `Stopping` and `Waiting for revision` are derived views.
2. Keep the current Session-backed WorkTask projection for Phase 1. Do not add a registry, separate command database, independent controller package, or multi-authority partition.
3. Add one exact-run activity read, one idempotent exact-run stop, and `run_id` filtering on the existing deliverable list. Recover a lost stop response by replaying the same request and key.
4. Revise the whole execution brief and create a new run. Do not imply that a member can be surgically redirected while the old run continues.
5. Restrict the first release to workflows with no irreversible external writes. Do not build a general replay policy or side-effect ledger before a real workflow requires it.

This review supersedes the heavier Phase 1 mechanics in earlier drafts of this note. It does not claim that advanced intervention is impossible; it requires product evidence before that architecture becomes active work.

## Product contract

### Complete core journey

The product is complete for the first real user when it supports this journey without exposing internal evaluation or raw MCP JSON:

1. The user selects a team or describes an outcome and confirms a supported recommendation.
2. Workbench records the brief and dispatch identity before Weave starts work.
3. Weave continues independently of the conversation and application window.
4. The user returns to one work view showing team, stage, members, runtimes, HumanTasks, and deliverables.
5. The user may stop the exact current run and sees truthful progress until Weave reports `cancelled` or `abandoned`.
6. Workbench shows observed completed, interrupted, unstarted, and unknown facts.
7. The user confirms a complete revised brief; Workbench starts a new run and retains the old attempt.
8. The user reads and accepts the corrected run's own deliverable.

### Honest boundaries

Phase 1 does not promise same-run resume, checkpoint restart, member-only pause, causal artifact invalidation, rollback, compensation, cross-device local-state recovery, or multi-user approval. It does not show private chain-of-thought. It distinguishes missing data from empty activity and stale data from stopped work.

No team match produces an honest gap discussion. No published default workflow produces an explicit error. Free collaboration remains an explicit mode and never a silent fallback.

## Two-owner architecture

```text
User / Codex / Workbench UI
            |
            v
DSH Session work projection
  brief, run history, freshness, pendingAction
            |
            v
Workbench Host poller and retry
            |
            v
Weave
  teams, workflows, TeamRuns, runtimes,
  HumanTasks, deliverables, cancellation
```

### Weave ownership

Weave owns team and workflow identity, exact TeamRun identity and status, stages, member activity, runtime assignments, HumanTasks, deliverable provenance, cancellation transitions, authorization, and audit. An execution status changes only because Weave changed it.

### Workbench and DSH ownership

Workbench owns the user brief, chosen team snapshot, supervising Session, ordered attempt links, local freshness, one unresolved action, correction draft, and presentation. DSH supplies conversation persistence, Host lifecycle, MCP access, Browser extension slots, localization, and generic tool presentation.

DSH `Agent.cancel()` and local Job cancellation may stop a foreground conversation or local wait. They never mean that a remote TeamRun stopped.

## Minimal concepts

### WorkTask projection

A Phase 1 WorkTask is a derived Session projection, not a second business database. It contains:

- one user objective and expected output;
- one confirmed team and workflow snapshot;
- an ordered list of attempts, each linked by `client_request_id` and exact run identity;
- the newest authoritative activity snapshot and freshness;
- zero or one unresolved `pendingAction`;
- HumanTask and deliverable references returned for the exact run.

The projection may be promoted into an independent domain only when one real outcome must span several teams or survive deletion of all linked Sessions. That migration must reuse exact run and request identities and must not guess by title or team name.

### Attempt

An attempt is a local link to one immutable Weave TeamRun. A terminal attempt never becomes running again. A revision creates a new `client_request_id` and a new run. The old attempt and its deliverables remain visible.

### Pending action

`pendingAction` is the only new local recovery fact. It records either a stop request or a corrected dispatch, its canonical payload digest, idempotency or request key, exact target, and local lifecycle. It is persisted before network send and cleared only after Workbench observes the authoritative result.

It is not a second run state. The UI derives its wording from `pendingAction`, Weave status, and freshness.

### WorkStream

`WorkStream` is deferred. If real FDE use proves that one outcome needs several specialist teams and a synthesis team, an independent WorkTask may contain bounded WorkStreams. Workbench still does not schedule their internal workflow or invent cross-run dependencies; Weave owns every dispatch.

## Required Weave contracts

Phase 1 requires only three public changes:

1. **Exact-run activity:** one bounded snapshot returns TeamRun status, stages, known members, runtime assignments, HumanTasks, exact-run deliverable references, freshness revision, and per-section completeness.
2. **Exact-run stop:** one authenticated idempotent command transitions queued, running, or parked execution through Weave's existing cancellation semantics. Repeating the same payload and key returns the same outcome; a changed payload conflicts.
3. **Exact-run deliverable filtering:** the existing deliverable list accepts `run_id`; body retrieval continues through the existing deliverable getter.

The existing `team_dispatch`, `dispatch_status`, HumanTask tools, and deliverable getter remain. A corrected attempt uses ordinary `team_dispatch` with a new `client_request_id`; Phase 1 does not add correction digests, member targets, a separate command-status endpoint, or a separate stop-summary record.

The terminal activity snapshot itself provides the observed stop facts. Workbench never derives causal validity from model prose.

## Workbench integration

Phase 1 keeps the existing package surface:

- `packages/bundle/workbench-app` owns the Host Session projection, polling, and retry adapter.
- `packages/client/ui-weave` owns the work row, inspector, activity details, stop confirmation, revision editor, run history, HumanTasks, and deliverable cards.
- Existing DSH Session, Remote, MCP, locale, and tool packages remain unchanged unless a narrowly proven extension is required.

No `work-task-controller`, registry, standalone command store, reliable event stream, or generic synchronization framework is added in Phase 1. The Browser refreshes from the Host baseline after mount, reconnect, and commands. Polling is bounded and stops after terminal reconciliation.

The Host writes `pendingAction` into the durable Session projection before calling Weave. On restart it scans non-terminal work projections and unresolved actions. An unknown stop result replays the same request and idempotency key, then reads activity. An unknown corrected dispatch resolves through the existing `dispatch_status` and same `client_request_id`.

## Product surface

The default task view shows:

- objective and expected output;
- selected team and workflow;
- current stage and simple progress;
- members, current or latest business activity, and runtime placement;
- items needing human attention;
- exact-run deliverables and run history;
- freshness and honest missing-data indicators.

Diagnostics, raw trace, checkpoints, transport details, model metadata, and internal evaluation remain behind drill-down or absent. The UI has one whole-run control: `Stop this run`. After terminal cancellation it offers `Revise and start a new run`.

## Delivery phases

### Phase 1 — one team, one complete control loop

Characterize the existing TeamRun cancellation races, expose the three Weave contracts, extend the current Session work projection with run history and one `pendingAction`, complete the Workbench flow, and validate it with a real long-running 日冕 analysis workflow that performs no irreversible external write.

### Phase 2 — multiple specialist teams, only after evidence

If the FDE repeatedly needs several teams for one outcome, promote WorkTask into an independent local domain and add bounded WorkStreams plus an explicit synthesis stream. Do not add a general dependency graph or automatic cross-team scheduler.

### Evidence-gated advanced control

Safe-point pause, checkpoint continuation, member fences, causal impact plans, and a side-effect ledger are not scheduled phases. They become a separate design only if real stop-and-rerun use loses unacceptable work or an external-write workflow becomes a proven priority. Until then Workbench must not expose those controls.

## Verification

The minimum deterministic suite proves:

1. queued, running, parked, already requested, terminal, grace-expired, stale-generation, and late-result cancellation behavior;
2. same-key stop replay and changed-payload conflict before and after an unknown response;
3. Session or application restart with one unresolved stop or dispatch;
4. exact-run isolation for activity, HumanTasks, and deliverables;
5. UI wording derived from authoritative status, freshness, and `pendingAction` without false stopped or paused states;
6. one real 日冕 run that is inspected, stopped, revised, rerun, and completed with a corrected deliverable.

## Acceptance criteria

- Closing the conversation or application does not stop Weave work, and reopening restores the task view without a model turn.
- The user can identify the selected team, current stage, known member activity, runtime placement, attention, and deliverables.
- A stop request targets one exact run, survives an unknown response, and remains visibly in progress until Weave reports a terminal cancellation result.
- A terminal activity view reports only persisted completed, interrupted, unstarted, delivered, and unknown facts.
- Revision always creates a new request and run; the old run and its deliverables remain immutable and separate.
- Workbench has no persisted execution-status state machine beyond freshness and one unresolved local action.
- Codex MCP and Workbench consume the same Weave authority and report the same run facts.
- No advanced intervention or multi-team architecture is implemented without the corresponding evidence gate.

## Alternatives considered

**Build the independent WorkTask domain immediately.** It prepares for multi-team work but duplicates local persistence, recovery, revisions, and navigation before one real single-team task has passed. The Session projection is sufficient for Phase 1.

**Wait for safe pause.** It withholds useful control until Weave becomes a much larger execution engine. Whole-run stop and new-run correction are honest and complete for side-effect-safe analytical work.

**Use local Agent or Job cancellation.** It cannot stop remote workers and would produce a false product state.

**Send live correction to one member.** Without fences and causal execution identity, Workbench cannot know which work consumed the new instruction. Phase 1 revises the whole brief for a new run.

## Risks

Session-backed work cannot yet group several teams or recover after all linked Session data is destroyed. This is an explicit Phase 1 boundary, not a hidden durability claim.

Whole-run stop may discard uncommitted work. The product preserves committed facts and validates the cheaper interaction before investing in lossless intervention.

Restricting Phase 1 to workflows without irreversible writes excludes some future automation. That exclusion is safer and smaller than building a generic replay-governance system prematurely.

The current cancellation implementation may expose late-result races. Such a failure blocks the Weave stop contract; Workbench must not mask it with local state.
