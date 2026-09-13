---
description: "Weave Workbench product profile for users and maintainers connecting the DSH browser runtime to durable Weave teams through MCP."
kind: "package-bundle"
---

# `@deepseek-ai/dsh-workbench-app`

English | [中文](README.zh.md)

## Summary

The `/api/weave.capability-apps` proxy manages applications, exact-version grants and application credentials. It accepts a fixed operation set through the Host's manage scope. Only a newly issued application key is returned once; list responses contain credential metadata without key hashes or raw secrets.

Reusable capability authoring stays in the main Workbench conversation. The foreground agent lists existing capabilities first, asks Weave to generate and save a reviewable draft when reuse does not fit, presents only the business proposal, and publishes an immutable revision only after the user confirms that exact proposal. Workbench does not expose a manual definition editor. Application identities, exact-version grants and one-time credentials remain in Settings as a separate administration flow.

This bundle turns the general DSH Web runtime into Weave Workbench without changing the agent loop. It adds the Workbench browser identity, connects the local `weave mcp serve` process when a business API key is present, and gives the foreground agent one product rule: match an existing team before dispatching work, or state that no suitable team exists and help define one. A Host-side WorkTask projection records the dispatch and keeps its Weave status synchronized after the foreground turn ends. The same authenticated Host exposes a bounded runtime-node registry to the main Workbench surface. The dispatched Weave run is the durable task; the foreground agent does not create a shadow DSH goal, poll it to completion, or save a duplicate deliverable.

Creation calls persist a separate preparation record before a run exists. Explicit template failures remain visible across directory reads; transport errors remain unconfirmed. Exact build identifiers and server update times protect background progress from late responses. A new creation after a terminal run keeps previous attempts. The wire projection supplies one display state to navigation, task cards, and project activity; pending stops, parallel execution, scheduled waits, missing outputs, and runtime interruptions remain distinct. Team selection does not authorize dispatch, and an explicit create-first request still requires the separately requested dispatch decision through the existing question or plan-review flow.

## Table of Contents

- [Use this package](#use-this-package)
- [Surface boundary](#surface-boundary)
- [Credential boundary](#credential-boundary)
- [Dev Note](#dev-note)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)

<a id="use-this-package"></a>
## Use this package

Run `dsh --profile workbench` or the repository shortcut `pnpm workbench`. The shortcut prefers an explicit `WEAVE_API_KEY`, then checks the macOS Keychain service `weave-workbench-api-key`; other platforms use the explicit environment value. The shipped profile stacks `dsh-base`, `dsh-web-app`, and this bundle. When a key is present, the bundle starts `weave mcp serve` over stdio and publishes its tools under the `mcp__weave__*` namespace. This local Workbench defaults `WEAVE_API_URL` to `http://127.0.0.1:18080`, and `WEAVE_COMMAND` may select a different trusted local binary. Users sign in with their platform account. Ordinary members start personal Sessions without choosing a Host directory; the Host creates a user-partitioned working directory. Administrators manage explicitly selected shared project directories through the administration interface. Workbench does not inherit DSH's native DeepSeek adapter or official default model; a saved provider and model from the Models page must exist before a new Session can run.

When `WEAVE_API_KEY` is absent, the MCP row is disabled and the browser shows the account connection boundary. Business requests and foreground execution remain unavailable until the operator restores the platform connection.

The browser receives an opaque HttpOnly session cookie; platform tokens stay in Host memory. Each Session persists its initiating user and workspace. Direct requests, background observation, and each MCP call carry that Session's active user authorization. Logout, expiry and Host restart invalidate the in-memory authorization. Lists, history, events, human responses and archive receipts enforce the same Session ownership. The connection waits for the account probe before opening business RPC or event traffic.

The foreground Agent exposes Weave product tools and user questions. Scope restrictions and a final execution guard reject terminal, arbitrary file, environment, code transport and unrelated MCP tools, including tools registered later. Research and code execution belong to the authenticated execution Runtime. This is a product and credential boundary; user directory partitioning does not claim an operating-system sandbox.

The Host registers `weave_dispatch` for fixed published workflows. Its model arguments contain only the agreed team, workflow and optional project. The Host freezes unconsumed original user messages in the Session log before registering an immutable input revision with Weave. UI-generated prompts are recorded as `plugin/ui-control` context and excluded from that material. Registration pins the published workflow and assigns the only accepted dispatch request identity; the dispatch call carries that identity and revision without a model-authored task body. The Workbench MCP subprocess hides raw `team_dispatch` through `WEAVE_WORKBENCH_BOUND_DISPATCH=1`.

Each network attempt follows a successful Session flush. A lost response retries the same registration and request. HTTP errors, including authentication failures, do not release an unknown request; only explicit team or workflow rejection before registration allows a changed selection. If new user input arrives before an unresolved request is retried, Weave atomically returns its existing run or closes its unconsumed revision; the Host cannot enqueue that old request again. Closing a revision does not discard the original user material. The next dispatch retains that material with the later amendments. Explicit reruns use the same registration path with the complete edited brief retained byte for byte in their user action.

A fork inherits consumed-input history, but its new dispatch has its own revision head and request identity. Unresolved inherited dispatches and rerun actions are not replayed. Resolve them in the original Session and fork from that resolved history, or submit an independent task in an empty Session; a fork does not silently absorb later parent events.

When a dispatch succeeds, the Host stores its `client_request_id`, run, team, workflow, progress, member activity, named runtime assignment, exact-run deliverables, and blocker state in the Session log. A bounded poller reads the exact run activity and deliverable contracts with the same business identity and appends whole snapshots. Closing the conversation therefore stops neither Weave execution nor Workbench status recovery. UTF-8 text files explicitly produced by the current CLI response and collected by Weave arrive as immutable deliverables, so the panel can preview and download them without accessing an arbitrary host path. Published-workflow member files remain stage outputs; the delivery node owns final output. Activity observations correct cached stage labels even when the file count is unchanged.

Workbench Settings gives authenticated administrators access to registered runtime nodes without adding a separate infrastructure shortcut to the global sidebar. Ordinary members use personal task creation, history and archival; shared project movement and Host settings are not member actions. Its Runtime Nodes page summarizes availability and shows capacity, engine identity, authentication mode, binary version, last connection, and failover-group membership. Users can add, rename, group, or remove a node without visiting a separate administration application. Removing a node revokes its connection immediately and can interrupt active work; saved task facts and deliverables remain. A newly created runtime token appears only in the creating browser response, together with a complete copyable connection command. The Host uses `WEAVE_RUNTIME_SERVER_URL` when configured; otherwise it keeps an already routable API URL or replaces a loopback and container-only hostname with the preferred LAN IPv4 while retaining the API port. Whole-task controls use a dedicated authenticated product route and append WorkTask actions directly, so corrections and reruns do not appear as internal command records in the conversation.

Each Session has one in-flight poll at the configured interval; publishing its snapshot does not trigger an immediate replacement read. Business changes append immediately. Unchanged observations append at most once per 30 seconds to refresh the browser freshness deadline; an attempt’s update time changes only when its retained fields change. Object field order does not establish a business change. The bundle registers `weave/work-task` as projection-backed history: an accepted exact `workTask` baseline lets the browser receive contiguous ranges in place of old snapshots. Conversation, tool, and task-action records stay in the history window; the persisted log remains complete. Team discovery and exact-team detail responses supply the displayed team name. Terminal activity retains member failures while failed team-detail or deliverable reads retry before observation settles. Late reads preserve the latest pending action, local assessment, and explicit receipts; responses for a replaced run cannot write into the new task. Only the corresponding action’s actual HTTP success or rejection produces a receipt. Stage retries persist and replay one idempotency key per explicit user action, including after a lost response or another failure. Human answers persist the server’s `interaction_id` and submit it unchanged, so an old form cannot answer a later question in the same run.

The durable task records the exact wait kind and node supplied by Weave. Browser actions and commands accept stage retries only during a recoverable wait for that stage. The Host fetches human-task details from `/v1/human-tasks/:run_id`, retains the exact wait identity and response schema, and records an answer before submitting it to the existing completion endpoint with a stable idempotency key. The wait clears only after an authoritative response. Completed browser actions fold into durable acceptance or rejection receipts and are deduplicated on replay; accepted requests are not labeled completed work. Abandoned execution is distinct from confirmed cancellation: only Weave’s explicit `stop_unconfirmed` fact from the matching cancellation transition permits the stop-unconfirmed explanation.

The browser-authenticated `/api/weave.deliverable` route verifies the selected Session, current run, and known deliverable before streaming its complete retained content from Weave's `/v1/deliverables/:id/content`. The Host credential stays private. Downloads are attachments; SVG image previews have sandbox CSP, and HTML remains a static sandboxed browser document. A truncated projection does not become a truncated generated download or an invented local file.

The activity projection carries public runtime text, its transport completeness, and current physical-task identity. Repeated `(task_id, seq)` observations do not duplicate public records. Only an observed runtime capability marks live updates; older and other runtimes remain stage-completion updates. These public messages never establish final delivery or expose private reasoning.

Weave saves member execution settings and republishes affected workflows in one transaction. A conflicting draft or failed publication leaves both unchanged. Existing runs keep their frozen configuration. Native CLI model names use the selected runtime; the Host returns only editable fields and safe failure codes.

Execution, delivery verification, and user assessment are independent task facts. The Host projects Weave's saved check summaries for the current immutable delivery revision. A final file or summary establishes that an output can be opened; neither file counts nor `PASS` prose establish verified delivery. Unknown requirements and failed checks remain visible. The pilot report exposes execution completion rate and separate verification counts.

The assessment action carries the displayed delivery revision and checks it against Weave before saving the user's decision. The latest assessment receipt remains in the Session log during a temporary evidence-read failure; it applies again only when the same run and revision are observed. A different revision starts unrated. Rechecking the same revision preserves its assessment and reads saved results through Weave's bounded verification endpoint without dispatching members. The Host refreshes after the response even when an older terminal poll is in flight. A late assessment read cannot replace a newer verification report. The [delivery verification decision](../../../.agents/notes/implemented/bug-fix/2026-09-08-workbench-delivery-verification.md) explains these identities and limitations.

<a id="surface-boundary"></a>
## Surface boundary

The Workbench client keeps the DSH session, optional project workspace, provider-neutral model settings, permission, approval, tool, and deliverable foundations. It fixes the foreground agent to the full standard capability preset and hides the agent-preset chooser because prompt-composition modes are a host concern rather than a business-task choice. It disables the inherited native DeepSeek adapter and official DeepSeek default, and also suppresses DSH's internal-testing notice, official-DeepSeek credential onboarding, Preview badge, official brand occupant, Subagent presentation, message-feedback controls, and trajectory inspection surface. Their generic DSH behavior remains unchanged in non-Workbench builds; the Workbench Models settings section remains the explicit place to configure a provider without blocking the browser with a vendor-specific first-run dialog.

<a id="credential-boundary"></a>
## Credential boundary

`WEAVE_API_KEY` is passed only from the trusted host process to the local MCP subprocess and authenticated Weave requests. It is not embedded in client artifacts and is not added to model context. Runtime-list responses expose only display, health, capacity, and scheduling facts; mutation responses expose a newly created runtime token once. `WEAVE_RUNTIME_SERVER_URL` contains no credential and is returned only as the connection address paired with that one-time token. `WEAVE_SECRET_KEY` and `WEAVE_SECRET_KEY_FILE` belong exclusively to the Weave server because they encrypt stored credentials; this bundle never reads or forwards either value.

The current desktop Host uses one configured Weave credential and therefore represents one signed-in developer identity. Cross-workspace data is isolated by that credential's workspace. A shared multi-user Host requires delegated per-user identity before it can preserve individual attribution; until that exists, one Host must not be shared by multiple signed-in users.

<a id="dev-note"></a>
## Dev Note

None.

<a id="model-experience"></a>
## Model Experience

### Workbench team-routing persona

#### What the model sees

The profile registers the rule as a dedicated Workbench prompt section and fixes the foreground agent to the standard full-capability preset, so a session mode cannot shadow it. The foreground agent must list and match Weave teams for substantive business work, using tools such as `mcp__weave__team_list`, then confirm the selected team, task scope, and expected deliverables before calling `weave_dispatch` for the published workflow without a task-body argument. Existing explicit confirmation is sufficient; internal construction and evaluation add no user approval step. After dispatch it may make at most one status call to confirm the handoff, then it returns the selected team and a short human-facing state. It must not include internal identifiers, raw workflow versions, orchestration phases, or backend enums unless the user explicitly requests technical details. It must not create a DSH goal, poll the Weave run in the foreground, or save a duplicate deliverable. Workbench owns background status projection; the model checks for an exact-run final deliverable when the run is terminal or the user later requests it. If only stage records exist, it reports that final output remains unconfirmed. When a final deliverable exists, it reads it and answers with a short user-facing completion summary: what finished, the main findings or decisions, the files the user can open, and any action still needed. Internal run IDs, deliverable IDs, runtime IDs, host paths, hashes, validation command names, and engine details stay out of the main answer unless the user asks for technical details. With no match it must say so and collaborate on a team definition. It may use free collaboration only after an explicit user request, and it must report an unavailable Weave connection honestly.

### Workbench capability-authoring persona

The same conversation first calls `mcp__weave__capability_list` and reuses a suitable published capability when possible. Otherwise it calls `mcp__weave__capability_plan` with a stable idempotency key to create or revise one draft, summarizes purpose, responsibilities, flow, inputs and outputs in business language, and waits for confirmation. It calls `mcp__weave__capability_publish` only after the user confirms the proposal and publication. Published revisions remain immutable. Raw definitions, schemas and internal execution details stay out of the user-facing flow.

#### Token effect

One stable product persona, the Host-owned dispatch schema, and the tool schemas published by the Weave MCP server when connected.

#### KV Cache effect

Stable while the Workbench persona and connected Weave tool roster remain unchanged. Connecting, disconnecting, or changing the MCP tool set changes the request prefix.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>

- **Credential discovery differs by host** — the repository launcher supports the macOS Keychain; other packaged hosts currently provide `WEAVE_API_KEY` through their process environment.
- **Connection state has no dedicated card yet** — startup logs reveal an unavailable MCP process, while a Workbench-native connection indicator remains to be added.
- **Input attribution remains a conversation window** — original user messages since the last accepted initial dispatch remain together. This prevents silent body replacement, but does not establish which later conversation messages belong to an independent new task. Typed task targeting remains separate work. Bound dispatch currently supports fixed workflows only; legacy raw HTTP callers do not receive its input-source guarantee.
- **Assessment belongs to this Host** — user decisions persist in its Session log. They are not a shared cross-Host acceptance ledger. Historical deliveries without a revision remain readable but cannot receive a version-bound assessment or recheck. Verification can only resolve requirements supported by registered trusted checks; professional review and unsupported file formats remain explicit gaps.

Execution planning happens during proposal generation: known procedures execute directly; only concrete unknowns receive bounded method discovery. External access and permissions alone do not trigger research. The structured team definition supports lead → parallel workers → finalizer; custom control flow requires an actual supported graph. This is model guidance, not a runtime classifier or a guarantee of fallback execution.
