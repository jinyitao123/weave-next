# Agent Note: Workbench task state and reading continuity

Status: implemented

English | [中文](2026-09-05-workbench-task-state-and-reading-continuity.zh.md)

## Problem

A team creation failure could leave the conversation presenting team selection while no dispatch existed. Independent status wording obscured the difference between waiting for a user, waiting for execution to stop, and active team work. Long member records, changing list prefixes, and repeated status animations competed with the reader's current task. A layout that retained only 360px for conversation also squeezed the task receipt and its actions.

## Decision

The [Workbench Host](../../../../packages/bundle/workbench-app/src/index.ts) owns the durable preparation record and derives `displayState` for task consumers. Preparation retains the creation call, returned build identifier, reported steps and attempts, and known or unknown outcome until dispatch replaces it. Background refresh reads the existing build-progress route for that recorded build; a transport error does not establish creation failure. Task updates reject another run and older observations rather than replacing the current task with unrelated or stale facts.

Team-card selection requests proposal review through the existing conversation. The routing instruction distinguishes confirmation that includes dispatch from an explicit request to create first and decide dispatch separately. The displayed proposal and response remain in existing structured-question or plan-review records. This change does not add a versioned proposal authorization model or enforce new permissions at the execution API.

The [task UI](../../../../packages/client/ui-weave/README.md) keeps preparation and current operations in the conversation receipt, while the scene owns complete member records and deliverable previews. Long public records expose original-text excerpts and explicit expansion. Reading preferences retain expanded records, follow state, unread state, and a retained-record identity with its viewport offset. Removing older list entries therefore preserves the surviving text being read. Completion keeps an already selected progress view, and returning to the newest record remains an explicit action.

The task panel derives one set of member display names before rendering its list, details, follow controls, recovery owners, correction targets, and adjustment references. Recorded readable names remain visible; empty names, UUIDs, and internal identifiers use localized role or roster-position labels, with position suffixes for duplicates. This keeps unknown identities readable without inventing names. Exact member identifiers remain unchanged in action references and collapsed identification details.

The [layout owner](../../../../packages/client/ui-layout/README.md) requires 900px of content space for a split, retaining at least 420px for conversation and 480px for the scene before padding. Narrow layouts show one main view. Task regions use one primary executing-status pulse, while member dots and team candidates stay static. Freshness deadlines stop execution effects without requiring another projection update; reduced-motion preferences remove animation and transition effects.

Member identity and actions use a compact header and secondary controls before the reading region. Long duties disclose on request, record headings remain subordinate to the member title, and interrupted conversation receipts show the next recovery step without repeating the full member list. Workbench-scoped shell styling aligns scene insets and gives selected navigation rows a distinct resting state.

The scene keeps one mounted Overview, Progress, and Outputs tab bar across team, member, and output views. Completed work with final delivery first opens Overview, which indexes three final outputs, the roster, and recorded input references. Completed work without final delivery opens Outputs. The output list includes stage files and filename search; exact output links clear the filter. The overview member entry explicitly returns to the roster start. Resolved correction records stay collapsed until requested. Member records share the main scene scrollport; the scene retains a separate position for each run, tab, and selected member, including navigation from the conversation receipt. Hidden readers neither follow updates nor restore their old anchors. Output deep links are consumed by run, artifact, and request identity, so returning to a tab does not repeat a focus operation. File previews mount on first expansion and remain mounted through collapse and tab changes. A single stage file needs only its own disclosure. The known two-field human-review summary has readable decision and comments with the original JSON available on request.

The Workbench conversation places task receipts in document flow above the sticky input. Receipt expansion therefore adds its own space instead of enlarging the overlay over older messages. Operation history is available in the work scene, and historical human input is labeled as a previously submitted answer. A compact header combines the named scene entry, current status, and a chevron; team details remain in its tooltip. Workbench removes the session-log download button while retaining the underlying export service and command feedback.

## Alternatives considered

**Infer creation outcome from the absence of a dispatch.** A run does not exist while creation is still being submitted or checked. Preserving the actual creation record distinguishes failure from an unknown transport outcome without inventing a dispatch.

**Treat a selected team card as dispatch approval.** Selection does not establish the task scope or whether the user authorized immediate execution. The existing conversation review preserves that distinction without introducing a separate product workflow.

**Keep fixed scroll offsets and animate every running member.** Removing a prefix changes the text at a fixed offset, and multiple loops compete with the main status. Retained-record anchors and one primary signal preserve reading continuity and a clear status hierarchy.

The model selector uses a friendly unavailable label, keeps its menu actionable, and reuses catalog refresh to recover after configuration changes. The empty menu gives the existing Settings → Models path; this change does not add a settings-navigation service. The chat activity label stops while a user decision is pending, and collapsed reasoning does not preview process text.

## Consequences

These changes improve the presentation of existing creation, execution, and reading facts without adding product features or business permissions. They do not fix the backend causes of team construction failures or runtime interruption. Proposal revisions and durable authorization enforcement remain outside this implementation. A server-removed record cannot be recovered through a browser anchor; truncation remains visible. Width floors protect column geometry but do not prove that every translated label or document fits after padding.

## Verification

Focused component and projection suites cover creation failure and unknown outcomes, shared display states, stale observations, proposal-selection wording, completion without a forced tab switch, long-record expansion, retained-record anchors after list truncation, and readable member names with unchanged action targets. Column tests cover the split threshold and width floors. Further task-scene regressions cover stable tab focus, one-time output deep links, lazy preview mounting with retained zoom, and delayed-content restoration without persisting a clamped position. The Workbench build compiles the revised CSS.

Local Workbench screenshots verify historical failure, interruption, output access, long-record expansion and return, plus selected responsive sizes. An independent browser isolated a reload failure to native scroll anchoring while the scene column expanded; the task-scene body now opts out because the scene owns restoration. Isolated production-preview fixtures cover CSV, TSV, and SVG at 1440 and 390 pixels, including horizontal scrolling and retained zoom. These fixtures do not verify authenticated delivery downloads. The evidence does not establish a new successful run, fault recovery, or a complete visual quality review. The browser helpers now select the exact project entry and copy for the loaded client profile. Workbench replay passes the canonical persisted-session path and the complete cancellation path, including queued-draft removal and recovery after stopping. Separate Workbench goldens retain the generic profile's expectations. Independent real-page checks at 1440 and 390 pixels confirm task receipts scroll out of view while the input remains fixed, and operation history remains available in the scene. Other replay scenarios and the generic profile were not revalidated.
