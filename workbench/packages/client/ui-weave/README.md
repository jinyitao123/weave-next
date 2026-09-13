---
description: "Weave-native browser presentations for Workbench team discovery and final deliverables."
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

English | [中文](README.zh.md)

## Summary

Workbench starts with account sign-in before mounting business views. The sidebar shows the current account and sign-out. Account names and an optional workspace are the only identity inputs alongside the password; passwords are cleared on submission and are never written to browser storage. The Host retains authentication credentials behind an HttpOnly cookie. Sign-in and sign-out rebuild the browser runtime, while a business request reporting expired access immediately hides the old work view and discards delayed responses. Account changes are also checked on focus, periodically, and across supported browser tabs.

The Calling applications settings section creates stable application identities, grants specific published versions and issues scoped access keys. Keys can overlap during rotation; revoking the old key does not change the application's invocation identity. Newly issued application keys are shown once and can be hidden, while the Workbench Host credential remains private.

Reusable capability authoring remains in the main conversation through Weave tools; this browser package does not expose a manual definition editor. Settings contains only application access administration: stable application identities, exact published-version grants, scoped one-time keys, rotation and revocation.

`dsh-client-ui-weave` turns durable Weave MCP call results into a visible work task and places runtime-node management in the same Workbench surface. Users can follow the selected team, progress, member activity, runtime placement, human decisions, and exact-run deliverables without reading internal identifiers or tool vocabulary.

The current account role controls Host management entrances. Members create personal tasks without choosing a directory, see a flat personal task list, and can archive their own tasks. Administrators retain shared project and Host settings controls. Personal creation sends no path or workspace override, deduplicates pending clicks, and discards navigation after account expiry; the Host remains the authorization and directory authority.

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

Mount the package after `ui-chat` and `ui-tool`. Settings owns runtime-node management. The task action beside the Session title opens the existing work scene beside the conversation; full width requires an explicit action. The scene has a stable Overview, Progress, and Outputs tab bar shared by team and member reading. Each scene view retains its main scroll position, including when navigation starts in the conversation receipt. Live work puts recovery, corrections, and members first. Failed work opens Progress and names the failed stage in the scene and compact conversation receipt, with its reported reason available on demand. First opening completed work with a final deliverable selects Overview; completed work missing final delivery selects Outputs. Stopped work selects Overview when retained outputs exist. Completion during reading keeps the current tab. A missing team name never implies that a dispatched run is still matching a team. The selection and up to three followed members are browser viewing preferences. Member records show the assigned runtime, actual stages, readable inputs, tool activity, and directly expandable outputs. Proposing a member adjustment inserts a readable reference into the existing composer without replacing its draft or sending a message. The reference retains the exact run, member, stage, and output identities for subsequent discussion; it does not create an independent member conversation or pause.

Member names use recorded human-readable names. Empty names, UUIDs, and names equal to the internal member identifier display as Team lead or Member N; duplicate display names receive a roster-position suffix. The scene, followed members, recovery owners, correction targets, and adjustment references share these labels. Identifiers remain unchanged and are available in collapsed run-identification details. Numbers describe roster positions, not new or persisted identities.

The scene's content column caps at 880px, while long prose retains its 72ch reading measure. Container-based adjustments follow the scene's own width, including a narrow split on a wide screen. Member rows align an initial avatar, identity, status, and navigation in fixed columns; duties remain one secondary line, with the full assignment in the member record. A single execution stage has no extra enclosing frame. Metadata and download controls remain visually secondary to the public text and deliverable title.

Overview indexes up to three final outputs across recorded formats, preferring the delivery summary, webpages, drawings, tables, and documents before auxiliary code or logs, the team roster, and the task request plus deduplicated recorded input paths. Input references do not imply that a member read a source. Its member link opens the roster at the start; ordinary tab changes retain reading position. All Outputs includes stage files and supports filename search when five or more outputs exist. Exact output links clear an incompatible search and expand the selected file. Completed correction records remain available under a collapsed disclosure; active corrections and failures retain their existing progress controls.

The main conversation has one compact task receipt in `conversation.input.dock`. Workbench places it in normal document flow above the sticky composer, so it scrolls with the conversation instead of covering earlier messages. It shows current status, available actions, a direct final-result entry, and a stage-output count. Complete member records, deliverable previews, and operation history stay in the work scene. The header uses an outlined work-scene icon with a small state indicator; hover and keyboard focus expose the scene label, current state and progress. Human responses and correction impact expand only on request; retry, stop, and correction confirmation remain available in the conversation. Resolved user actions remain as durable acceptance or rejection receipts in the scene; acceptance does not imply that execution has finished. Previously submitted answers are labeled as historical submissions. Human response forms preserve the field names and types from Weave's `resume_schema`; complex schemas direct the user to discussion rather than inventing a free-text answer field. Plain questions never automatically submit a correction.

Team selection requests proposal review and does not authorize dispatch. Creation stays visible in the compact receipt and scene before a run exists: submitting, building, ready, failed, or outcome unknown. The Host follows the recorded build identifier through the existing build-progress route; the scene shows only reported steps and attempts. A transport error preserves the uncertain creation record rather than returning to team matching. Structured questions and plan review retain proposal text and user responses in the conversation; this package does not introduce a versioned authorization store or execution permission check.

Waiting tasks distinguish timed pauses, team-stage work, human input, correction confirmation, and runtime interruption. When Weave identifies a current interrupted stage as waiting for the previous execution to confirm it has stopped, the conversation, scene, and member record name the stage and explain reconnecting the runtime first. No retry is offered until Weave reports that stage eligible; the pending acknowledgement is not described as permanently unrecoverable. A retry requires the exact current recoverable wait. Running, stopping, and terminal tasks cannot expose stale failure retries. Only a confirmed cancellation is labeled stopped; an abandoned execution is labeled stop unconfirmed only when Weave explicitly supplies `stop_unconfirmed` from the matching cancellation transition. A completed run without an `artifact_kind=final` deliverable remains a visible delivery gap and offers review of existing work. Final files and filename-free final summaries identify openable outputs; neither proves verified delivery.

The scene and conversation receipt show execution status, delivery verification (`pending`, `passed`, `failed`, or `unknown`), and the user's assessment separately. The scene exposes each saved check's status and reason. Missing verification records remain unknown, and prose such as PASS cannot establish verification. Assessment is available for a completed run with a final output and an available delivery revision. Historical output without a revision remains readable with disabled assessment controls and an explanation. Every assessment request carries the revision displayed when clicked. Host refreshes preserve assessment for another verification of the same revision and clear it for a new revision; the browser only displays an assessment bound to the displayed revision.

Verification details offer a recheck of registered checks against saved outputs without rerunning members. The request captures the displayed revision and requirements digest; missing either disables the button with an explanation. Acknowledgement leaves the displayed verification unchanged until the Host publishes its saved report, and conflicts remain visible. Rechecking cannot supply an unsupported check.

Public member updates use the observed runtime capability. Recent running facts add one restrained pulse to the primary task status in each task region; member dots and team candidates remain static. The conversation and scene identify active members, and the latest public record briefly highlights while that stage runs. No placeholder text stream is generated. Stop-confirmation waits, stale observations, stopping, and terminal states stay still; reduced-motion preferences disable these effects. A current Codex runtime may publish public text and tool events during execution; other or older runtimes update after stage completion. Public updates are separate from final deliverables and contain no private reasoning. Long records open as original-text excerpts with an explicit expansion control. While the reader scrolls through earlier records, new records do not move the viewport; an explicit action returns to the latest entry even when nothing is unread. Truncation is visible. Returning between members or tabs preserves member selection and up to 100 reading positions, including follow, unread, and expanded-record state. Retained records anchor history by record identity and viewport offset when earlier records are removed; records removed by the server cannot be restored.

Project output links select the exact retained artifact in its original conversation. The sidebar's project activity entry reads only the linked Workspace Sessions' current persisted tasks, deduplicates an identical run by latest observation, and excludes archived or unlinked Sessions. Missing records are explicit. Every task and output link returns to its original conversation; a recorded final artifact does not imply quality acceptance.

Settings includes member execution configuration. Changing engines clears the previous model override and fallback models. Saving applies the selected runtime and bounded retry policy to future tasks after workflow publication succeeds. Runtime details distinguish the configured endpoint and node default from models reported by execution receipts. Public attempt records describe observed execution history without implying model text or final delivery.

<a id="understand-the-implementation"></a>
## Understand the implementation

The browser uses existing keyed tool views, Session-header actions, details, input-dock, Settings, and Workspace project-activity slots. The Host owns the durable `workTask` projection, background polling, and authenticated task-action and content routes. The browser consumes the Host-derived `displayState` for shared task wording. Browser UI and stored viewing preferences create no second execution authority. New project navigation waits for the target Session view to commit before opening its scene, so the layout's Session-change cleanup cannot close the newly requested view.

After runtime creation, the browser validates the Host-provided server URL and one-time token, renders the complete shell-quoted connection command, and copies that command directly. It never asks the user to replace a service-address placeholder.

Deliverable bodies first render on expansion and remain mounted across collapse and tab changes. SVG zoom and table position stay with that mounted file; switching to a different file resets its preview. The known human-review summary format displays its decision and comments, with exact JSON under a secondary disclosure. Markdown uses the shared renderer; SVG previews use the authenticated content route as an image with sandbox CSP; HTML is a static sandboxed document with a restrictive CSP. CSV and TSV use bounded table previews of at most 200 rows and 40 columns; SVG images have explicit zoom controls. The UI offers the real full-content download when the preview is bounded. A download streams the complete retained artifact through `/api/weave.deliverable` to Weave's existing `/v1/deliverables/:id/content`, even when the projection preview was truncated. The Host binds downloads to the selected Session/run/deliverable and never exposes its business key. No local path or live application URL is inferred from generated prose.

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
