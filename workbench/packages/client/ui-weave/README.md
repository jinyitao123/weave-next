---
description: "Weave-native browser presentations for Workbench team discovery and final deliverables."
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

English | [中文](README.zh.md)

## Summary

`dsh-client-ui-weave` turns durable Weave MCP call results into a visible work task and places runtime-node management in the same Workbench surface. Users can follow the selected team, progress, member activity, runtime placement, human decisions, and exact-run deliverables without reading internal identifiers or tool vocabulary.

## Table of Contents

- [Use this package](#use-this-package)
- [Understand the implementation](#understand-the-implementation)
- [Further Exploration](#further-exploration)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)
- [Dev Note](#dev-note)

-----

<a id="use-this-package"></a>
## Use this package

Mount the package after `ui-chat` and `ui-tool`. Settings owns runtime-node management. The task action beside the Session title opens the existing work scene beside the conversation; full width requires an explicit action. The scene has Progress and Outputs tabs. Live work puts recovery, corrections, and members first. Failed work opens Progress and names the failed stage in the scene and compact conversation receipt, with its reported reason available on demand. Completed work opens Outputs; stopped work does so only when retained outputs exist. A missing team name never implies that a dispatched run is still matching a team. The selection and up to three followed members are browser viewing preferences. Member records show the assigned runtime, actual stages, readable inputs, tool activity, and directly expandable outputs. Proposing a member adjustment inserts a readable reference into the existing composer without replacing its draft or sending a message. The reference retains the exact run, member, stage, and output identities for subsequent discussion; it does not create an independent member conversation or pause.

The main conversation has one compact task receipt in `conversation.input.dock`. It shows current status, available actions, a direct final-result entry, and a stage-output count. Complete member records and deliverable previews stay in the work scene. Human responses, correction impact, and operation history expand only on request; retry, stop, and correction confirmation remain available in the conversation. Resolved user actions remain as durable acceptance or rejection receipts; acceptance does not imply that execution has finished. Human response forms preserve the field names and types from Weave's `resume_schema`; complex schemas direct the user to discussion rather than inventing a free-text answer field. Plain questions never automatically submit a correction.

Waiting tasks distinguish timed pauses, team-stage work, human input, correction confirmation, and runtime interruption. When Weave identifies a current interrupted stage as waiting for the previous execution to confirm it has stopped, the conversation, scene, and member record name the stage and explain reconnecting the runtime first. No retry is offered until Weave reports that stage eligible; the pending acknowledgement is not described as permanently unrecoverable. A retry requires the exact current recoverable wait. Running, stopping, and terminal tasks cannot expose stale failure retries. Only a confirmed cancellation is labeled stopped; an abandoned execution is labeled stop unconfirmed only when Weave explicitly supplies `stop_unconfirmed` from the matching cancellation transition. A completed run without an `artifact_kind=final` deliverable remains a visible delivery gap, offers review of existing work, and does not offer a success assessment. Filename-free final delivery summaries qualify as final outputs; stage and unknown artifacts do not.

Public member updates use the observed runtime capability. Recent running facts add a restrained pulse to task and member status dots, including a fanout wait with a running member. The conversation and scene identify the active members, and the latest public record briefly highlights while that stage runs. No placeholder text stream is generated. Stop-confirmation waits, stale observations, stopping, and terminal states stay still; reduced-motion preferences disable these effects. A current Codex runtime may publish public text and tool events during execution; other or older runtimes update after stage completion. Public updates are separate from final deliverables and contain no private reasoning. While the reader scrolls through earlier records, new records do not move the viewport; an explicit action returns to the latest entry. Truncation is visible. Returning between members or tabs preserves member selection and up to 100 reading positions, including follow and unread state; new content does not mark itself read during historical scrolling. Project output links select the exact retained artifact in its original conversation. The sidebar's project activity entry reads only the linked Workspace Sessions' current persisted tasks, deduplicates an identical run by latest observation, and excludes archived or unlinked Sessions. Missing records are explicit. Every task and output link returns to its original conversation; a recorded final artifact does not imply quality acceptance.

<a id="understand-the-implementation"></a>
## Understand the implementation

The browser uses existing keyed tool views, Session-header actions, details, input-dock, Settings, and Workspace project-activity slots. The Host owns the durable `workTask` projection, background polling, and authenticated task-action and content routes. Browser UI and stored viewing preferences create no second execution authority. New project navigation waits for the target Session view to commit before opening its scene, so the layout's Session-change cleanup cannot close the newly requested view.

Deliverable bodies render only after expansion. Markdown uses the shared renderer; SVG previews use the authenticated content route as an image with sandbox CSP; HTML is a static sandboxed document with a restrictive CSP. CSV and TSV use bounded table previews of at most 200 rows and 40 columns; SVG images have explicit zoom controls. The UI offers the real full-content download when the preview is bounded. A download streams the complete retained artifact through `/api/weave.deliverable` to Weave's existing `/v1/deliverables/:id/content`, even when the projection preview was truncated. The Host binds downloads to the selected Session/run/deliverable and never exposes its business key. No local path or live application URL is inferred from generated prose.

<a id="further-exploration"></a>
## Further Exploration

- [ui-tool](../ui-tool/README.md) — owns the keyed tool-view slot and generic fallback.
- [Workbench app](../../bundle/workbench-app/README.md) — mounts the Weave connection and this presentation package.

<a id="model-experience"></a>
## Model Experience

None, as this browser package initiates no independent model request and submits explicitly chosen references and discussion through the existing conversation.

#### KV Cache effect

Projection refreshes add no model context; explicit user submissions extend the existing conversation and affect its context and cache normally.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>

- **One task per Session** — the current projection tracks the latest Weave dispatch in a Session; multi-dispatch task grouping remains deferred.
- **Runtime process control stays with each machine** — the runtime center manages registration and scheduling facts; it does not claim to start or stop a local Codex, Claude Code, OpenCode, or built-in worker process from the browser.
- **Compact JSON is the contract** — non-array or team entries without stable ids and names fall back to a malformed-result state.
- **Deliverables are immutable remote files** — Workbench previews and downloads their recorded contents but does not materialize them into the active workspace.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

The Workbench profile intentionally keeps this package separate from generic DSH branding so upstream harness builds remain unchanged.

</details>
