# Agent Note: Weave work-task projection in the conversation shell

Status: implemented

English | [中文](2026-08-30-weave-work-task-projection.zh.md)

## Problem

Workbench recorded team discovery, dispatch, status, human-task, runtime, and deliverable calls as separate conversation rows. A user had to inspect tool names and opaque ids to determine which team was working, whether the workflow had started, where it was blocked, and whether a visible deliverable belonged to the current run. The details column stayed empty until the user selected one tool call, so the page hid the operational state that a long-running FDE task requires.

## Decision

`ui-chat` declares `conversation.details.summary` and renders it whenever no individual tool call is selected. `ui-weave` occupies that slot with a work-task projection and contributes a compact status control beside the Session title. The first recorded Weave call opens the details column; selecting a tool call still replaces the summary with the existing input/output inspector.

The projection folds the loaded Chat nodes without querying Weave. It correlates team discovery, the unique dispatch, status reports, human tasks, runtime records, and deliverables by their recorded fields. Dispatch status owns workflow lifecycle and progress; terminal-run status supplies secondary runtime health, so a `terminal_missing` projection cannot overwrite an authoritative `queued 0/9` dispatch. A deliverable contributes to the task only when its `run_id` equals the active run. Opaque ids remain in a collapsed diagnostic section. A queued dispatch, failed run, missing terminal, or failed attempt to treat the selected team as an agent becomes an explicit blocker instead of generic tool history.

The shared layout keeps its center-width concession rule. When that rule reduces an explicitly opened details track to zero, the details surface becomes a right-side drawer instead of disappearing. Closing the surface still removes it, and sufficiently wide viewports continue to use the resizable third column.

The Session log remains the durable source for this presentation. The Workbench Host registers a `workTask` projection that folds team discovery and dispatch results, then uses the dispatch's `client_request_id` to poll Weave independently of the foreground agent turn. Every synchronized update is appended as a whole `weave/work-task` snapshot and flushed before publication. Weave remains the execution owner; Workbench owns only durable correlation, status recovery, and presentation. Transient network failures preserve the last known state instead of inventing a terminal failure.

One Session retains ordered run attempts and at most one `pendingAction`. A team-card selection asks the conversation to prepare a complete dispatch brief but creates no remote side effect. The Host persists whole-run stop commands before calling Weave. A fanout stage that Weave classifies as an infrastructure failure remains attached to its parked parent and exposes one exact-stage retry; the confirmation states that successful sibling stages and outputs remain preserved. Work-product and verification failures never receive that retry control. After Weave reaches a terminal state, the user can edit the complete brief and confirm a new request and run. The prior run and its outputs remain visible. Member-level pause remains outside this phase.

The global DSH sidebar exposes the Weave runtime registry in every Session, while an active WorkTask shows only the nodes used by that task. Its entry projects available and registered counts with a health signal. An authenticated Host route returns only node display, health, capacity, engine identity, authentication mode, binary version, and scheduling facts and accepts bounded create, configure, and delete requests. The business API key remains on the Host; only a new runtime token crosses to the creating browser, once. Task controls use a dedicated Host route that appends WorkTask actions directly instead of creating command records in the conversation. Earlier command-backed action records receive a business-event presentation, while complete task briefs remain collapsed until requested. Runtime registration and scheduling belong to this surface, while starting or stopping a machine-local worker remains outside its authority.

Session list rows consume the same projection and show the owning team plus stage count. The right panel prefers the Host projection for lifecycle facts while retaining richer stage detail from loaded Chat nodes. Closing or navigating away from the conversation therefore no longer freezes the task status.

## Alternatives considered

**Replace the conversation with a dedicated task application.** Rejected because the existing three-column shell already provides navigation, conversation, and a resizable details column; replacement would discard working input, history, tool inspection, and deliverable presentation before the task lifecycle exists on the Host.

**Let the model summarize task state in assistant messages.** Rejected because model prose can omit, confuse, or stale run identity. The browser derives task state from recorded tool facts and keeps assistant prose as explanation only.

**Count every visible deliverable.** Rejected because `deliverable_list` can return files from another run. The panel prefers an empty current-task state over displaying an unverified file.

## Consequences

Weave sessions expose team, progress, runtimes, blockers, failure classes, human decisions, completeness, ordered attempts, and exact-run deliverables without requiring users to expand tool calls. The same surface manages registered runtime nodes without exposing the Weave business credential or pretending to control local processes. A user can stop the whole run, retry one infrastructure-failed fanout stage, and create a new run from a complete revised brief without a model call; persisted pending actions recover unknown network outcomes. Exact-stage retry deliberately preserves the original frozen stage input and does not apply to professional or verification failures. This phase remains bounded to one team and workflows without irreversible external writes.
