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

One Session retains ordered run attempts and at most one `pendingAction`. A team-card selection asks the conversation to prepare a complete dispatch brief but creates no remote side effect. The only live control is whole-run stop: the Host persists the exact target and idempotency key before calling Weave. After Weave reaches a stopped terminal state, the user can edit the complete brief and confirm a new request and run. The prior run and its outputs remain visible. Member-level pause, same-run resume, and causal impact claims remain outside this phase.

Session list rows consume the same projection and show the owning team plus stage count. The right panel prefers the Host projection for lifecycle facts while retaining richer stage detail from loaded Chat nodes. Closing or navigating away from the conversation therefore no longer freezes the task status.

## Alternatives considered

**Replace the conversation with a dedicated task application.** Rejected because the existing three-column shell already provides navigation, conversation, and a resizable details column; replacement would discard working input, history, tool inspection, and deliverable presentation before the task lifecycle exists on the Host.

**Let the model summarize task state in assistant messages.** Rejected because model prose can omit, confuse, or stale run identity. The browser derives task state from recorded tool facts and keeps assistant prose as explanation only.

**Count every visible deliverable.** Rejected because `deliverable_list` can return files from another run. The panel prefers an empty current-task state over displaying an unverified file.

## Consequences

Weave sessions expose team, progress, runtimes, blockers, human decisions, completeness, ordered attempts, and exact-run deliverables without requiring users to expand tool calls. A user can stop the whole run without a model call and create a new run from a complete revised brief; persisted pending actions recover unknown network outcomes. This phase remains bounded to one team and workflows without irreversible external writes.
