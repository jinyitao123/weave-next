# Agent Note: Phase 1 exact-run stop and corrected attempt

Status: proposed

English | [中文](2026-08-30-weave-exact-run-stop-and-rerun.zh.md)

## Problem

The current Workbench can project Weave work from a durable DSH Session, but it cannot truthfully stop the exact remote TeamRun or show enough exact-run facts to support a corrected attempt. Local Agent cancellation, Browser request cancellation, and polling suspension do not stop Weave.

Phase 1 must complete the first real FDE loop without first building an independent task platform. The required result is simple: start work, leave and recover it, inspect it, stop one exact run, revise the whole brief, start a new run, and retrieve that new run's deliverable.

## Proposal

Extend the current Session-backed work projection and existing Weave execution authority with only an exact-run activity read, an idempotent whole-run stop, deliverable `run_id` filtering, one unresolved local action, and corrected dispatch through the existing API. The release is complete when this narrow path survives real cancellation races and one real FDE task.

## Adversarial verdict

The first draft was rejected as too broad. It added a standalone WorkTask domain, WorkStream, two storage record kinds, a command worker, six control states, separate command-status and deliverable endpoints, member-targeted correction, rerun lineage fields, and a general replay policy.

The accepted design removes those additions. Phase 1 has:

- one existing Session-backed work projection;
- one authoritative Weave TeamRun status;
- one local freshness fact;
- zero or one unresolved `pendingAction`;
- one exact-run activity read;
- one idempotent whole-run stop;
- one complete-brief correction followed by ordinary dispatch.

This is the first implementation slice of the [minimal WorkTask architecture](../architecture/2026-08-30-weave-workbench-durable-work-task-domain.md).

## Product promise

After confirming a team and brief, the user may close the conversation or application while Weave continues. Reopening the work shows the team, current stage, known member activity, runtime placement, HumanTasks, exact-run deliverables, and data freshness.

The user can choose `Stop this run`. Workbench names the exact run and explains that committed output remains, in-flight work may be lost, and continuation requires a new run. It shows `Stopping` only as a view derived from Weave `cancel_requested` or an unresolved persisted stop action. It shows `Stopped` only after Weave reports `cancelled` or `abandoned`.

After cancellation, Workbench shows observed facts and a complete brief editor. Confirming the revision creates a new request and run. The old run remains immutable and visible. Phase 1 does not call this pause or resume.

## Included scope

Phase 1 includes:

- direct team selection and evidence-based recommendation through existing MCP tools;
- explicit failure when no team or published workflow matches;
- durable Session work recovery without a model turn;
- exact-run stage, member, runtime, HumanTask, and deliverable inspection;
- idempotent whole-run stop and unknown-response recovery;
- observed terminal facts with explicit unknowns;
- whole-brief correction and a new dispatch;
- attempt history and exact-run deliverable isolation;
- one real side-effect-safe 日冕 workflow test.

Phase 1 excludes independent WorkTask storage, multiple teams per work item, member-only correction, same-run resume, checkpoint restart, causal impact plans, automatic rollback, a side-effect ledger, cross-device recovery, and multi-user approval.

## State model

Workbench does not persist a new control enum. It renders actions from three facts:

| Weave status | Local fact | Product view |
|---|---|---|
| non-terminal | no pending action | Working or Waiting for input |
| non-terminal | pending stop | Stop requested |
| `cancel_requested` | any | Stopping |
| `cancelled` or `abandoned` | none | Stopped; revision available |
| terminal success or failure | none | Completed or Failed |
| any | stale freshness | Last known state, with stale warning |

`pendingAction` is not execution status. It records an admitted local intent whose remote outcome is not yet known. Once authoritative activity or dispatch status resolves the intent, the projection clears it.

## Weave changes

### Exact-run activity

Add one authenticated exact-TeamRun activity query and expose it through MCP. The response contains:

- exact TeamRun id, team and published workflow identity;
- TeamRun status and monotonically comparable revision or ETag;
- stages with known completed, active, interrupted, or unstarted status;
- stable known members and their current or latest business activity;
- runtime assignments and disclosed engine or node facts;
- HumanTask references and statuses;
- deliverable references attributed to the exact run;
- per-section completeness and updated time.

Private provider reasoning, credentials, raw unbounded trace, and guessed member identity are excluded. A terminal cancellation response supplies the observed impact facts directly; no separate `stop_summary` object is stored.

The existing MCP `team_run_status` may be extended to return this bounded activity snapshot instead of adding another overlapping read tool. The HTTP route may remain a TeamRun-specific adapter over the same application service.

### Idempotent stop

Add one authenticated whole-TeamRun stop application service, API route, and MCP `team_run_stop`. Its input is exact run identity, bounded reason, and caller-generated idempotency key.

It reuses existing TeamRun transitions:

| Current status | Result |
|---|---|
| `queued` | Cancel queued work and become `cancelled` |
| `running` or `parked` | Become `cancel_requested`, terminate work, then `cancelled` |
| `cancel_requested` | Replay the same request or reject a changed payload |
| terminal | Return the existing terminal result without reopening the run |
| grace expired | Become `abandoned` and advance the execution lease epoch |

A lost response is recovered by repeating the same request and key, then reading exact-run activity. Phase 1 does not add a command-status endpoint or MCP `team_run_stop_status`.

Cancellation characterization must prove that stale generation or lease work cannot become the current successful result or silently promote a late deliverable.

### Deliverable isolation

Add optional exact `run_id` filtering to the existing `deliverable_list`. Continue using `deliverable_get` for content. Do not add another exact-run deliverables resource.

The activity snapshot contains deliverable references so Workbench does not scan or join a workspace-wide list. A stopped run's outputs stay on that attempt and never become the corrected attempt's primary output.

### Corrected dispatch

Use existing `team_dispatch` and `dispatch_status`. The corrected attempt has a new `client_request_id`, the same confirmed team unless the user changes it explicitly, and the complete revised task text. Workbench retains the local predecessor link; Phase 1 does not add `supersedes_run_id`, correction digest, or member-target fields to Weave.

Phase 1 is enabled only for a configured workflow known to have no irreversible external writes. This is a rollout constraint, not a new general replay-policy subsystem. Every corrected dispatch still requires explicit confirmation.

## Workbench changes

### Session projection

Extend the existing Host projection rather than create a new domain. One projection stores the brief, selected team and workflow snapshot, ordered attempts, newest activity snapshot, freshness, and optional `pendingAction`.

Before stop, the Host appends the canonical stop intent and idempotency key to durable Session state. Before corrected dispatch, it appends the complete confirmed brief and new `client_request_id`. The network call happens only after that event is durable.

On application start, the Host scans known non-terminal work projections and unresolved actions. It replays the same stop key or asks `dispatch_status` for the same request id. No model turn is required.

### UI

Keep implementation in the existing Workbench bundle and `ui-weave` package. Add only:

- attempt history in the current work panel;
- bounded member and runtime activity details;
- `Stop this run` confirmation;
- stale and incomplete-data indicators;
- observed terminal facts;
- complete brief revision and dispatch confirmation;
- exact-run deliverable cards and content access.

Do not add a second task navigation system, a generic activity-stream framework, a dedicated controller package, or a new command database in Phase 1.

## Delivery plan

### Slice 1 — prove Weave truth

Characterize every existing TeamRun cancellation state and race. Add exact-run activity, idempotent stop, deliverable `run_id` filtering, authorization, audit, HTTP coverage, and the minimal MCP changes.

### Slice 2 — complete the existing Workbench projection

Add attempt history, one `pendingAction`, startup recovery, activity inspection, stop, revision, corrected dispatch, and exact-run delivery to the existing projection and UI. Verify that Browser closure and foreground Agent cancellation do not alter Weave execution.

### Slice 3 — validate real FDE work

Run one long 日冕 analytical task through Codex MCP and Workbench. During the run, inspect members and runtimes, lose one stop response, close and reopen the application, reach authoritative cancellation, revise the brief, start a new run, and accept its own corrected deliverable.

The release stops if this scenario exposes ambiguous run identity, a late result winning after cancellation, cross-run deliverables, or recovery that needs a model turn.

## Verification

Required tests are limited to the failure modes that can falsify the product promise:

1. queued, running, parked, cancel-requested, every terminal state, grace expiry, stale generation, stale lease, and late completion;
2. same-key replay and changed-payload conflict before and after an unknown response;
3. exact-run isolation of activity, HumanTasks, and deliverables;
4. Session disposal, Browser closure, Host restart, and application restart during stop and corrected dispatch;
5. UI truth table for working, stale, stop requested, stopping, stopped, completed, and failed;
6. Codex MCP and Workbench parity on one real run;
7. the real 日冕 corrected-deliverable scenario.

## Alternatives considered

**Create an independent WorkTask domain now.** This improves future multi-team grouping but duplicates persistence and recovery before the first single-team control loop is proven.

**Add a command-status endpoint.** Replaying the same idempotent stop and reading activity resolves the same uncertainty with one less contract.

**Add member-level correction.** Without server fences it creates a misleading promise; a complete revised brief and new run are honest.

**Add a generic replay policy.** Restricting the pilot to a known side-effect-safe workflow is smaller than inventing incomplete side-effect governance.

## Acceptance criteria

- A confirmed single-team work item survives conversation and application closure while Weave continues.
- Reopening shows authoritative team, stage, known member activity, runtimes, HumanTasks, deliverables, and freshness without a model call.
- Whole-run stop is idempotent and never reports stopped before `cancelled` or `abandoned`.
- An unknown stop response recovers by the same key; an unknown dispatch recovers by the same `client_request_id`.
- Observed terminal facts contain no causal, rollback, or artifact-validity claims.
- Revision creates a new run from a complete confirmed brief and preserves the old run and output.
- Workbench persists no duplicate run state machine and has at most one unresolved local action per work item.
- The first real 日冕 scenario ends with a corrected deliverable belonging to the corrected run.

## Risks

Using a Session projection means Phase 1 durability depends on retained local Session data. This matches the current single-installation product and is stated honestly.

Whole-run stop can waste uncommitted work. Product evidence, not architecture preference, will determine whether lossless intervention deserves a later design.

Restricting the first workflow to no irreversible writes narrows the pilot. It avoids pretending that a Boolean or three-state policy is a real side-effect ledger.

The existing internal cancellation path may not yet protect every late artifact race. Failure of characterization blocks release and must be fixed in Weave, not hidden in Workbench.
