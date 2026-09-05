---
description: "Weave Workbench product profile for users and maintainers connecting the DSH browser runtime to durable Weave teams through MCP."
kind: "package-bundle"
---

# `@deepseek-ai/dsh-workbench-app`

English | [中文](README.zh.md)

## Summary

This bundle turns the general DSH Web runtime into Weave Workbench without changing the agent loop. It adds the Workbench browser identity, connects the local `weave mcp serve` process when a business API key is present, and gives the foreground agent one product rule: match an existing team before dispatching work, or state that no suitable team exists and help define one. A Host-side WorkTask projection records the dispatch and keeps its Weave status synchronized after the foreground turn ends. The same authenticated Host exposes a bounded runtime-node registry to the main Workbench surface. The dispatched Weave run is the durable task; the foreground agent does not create a shadow DSH goal, poll it to completion, or save a duplicate deliverable.

## Table of Contents

- [Use this package](#use-this-package)
- [Surface boundary](#surface-boundary)
- [Credential boundary](#credential-boundary)
- [Dev Note](#dev-note)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)

<a id="use-this-package"></a>
## Use this package

Run `dsh --profile workbench` or the repository shortcut `pnpm workbench`. The shortcut prefers an explicit `WEAVE_API_KEY`, then checks the macOS Keychain service `weave-workbench-api-key`; other platforms use the explicit environment value. The shipped profile stacks `dsh-base`, `dsh-web-app`, and this bundle. When a key is present, the bundle starts `weave mcp serve` over stdio and publishes its tools under the `mcp__weave__*` namespace. This local Workbench defaults `WEAVE_API_URL` to `http://127.0.0.1:18080`, and `WEAVE_COMMAND` may select a different trusted local binary. Workbench does not create a fallback Workspace: a first-time user chooses an explicit project directory before starting a Session, so the project's team work and material output have a visible filesystem boundary. Workbench does not inherit DSH's native DeepSeek adapter or official default model; a saved provider and model from the Models page must exist before a new Session can run.

When `WEAVE_API_KEY` is absent, the MCP row is disabled and the browser still starts. This keeps the foreground runtime usable while making the missing Weave connection observable; the agent is instructed not to claim team work occurred without the tools.

When a dispatch succeeds, the Host stores its `client_request_id`, run, team, workflow, progress, member activity, named runtime assignment, exact-run deliverables, and blocker state in the Session log. A bounded poller reads the exact run activity and deliverable contracts with the same business identity and appends whole snapshots. Closing the conversation therefore stops neither Weave execution nor Workbench status recovery. UTF-8 text files explicitly produced by the current CLI response and collected by Weave arrive as immutable deliverables, so the panel can preview and download them without accessing an arbitrary host path.

Workbench Settings keeps registered runtime nodes available from every Session without adding a separate infrastructure shortcut to the global sidebar. Its Runtime Nodes page summarizes availability and shows capacity, engine identity, authentication mode, binary version, last connection, and failover-group membership. Users can add, rename, group, or remove a node without visiting a separate administration application. Removing a node revokes its connection immediately and can interrupt active work; saved task facts and deliverables remain. A newly created runtime token appears only in the creating browser response, together with a complete copyable connection command. The Host uses `WEAVE_RUNTIME_SERVER_URL` when configured; otherwise it keeps an already routable API URL or replaces a loopback and container-only hostname with the preferred LAN IPv4 while retaining the API port. Whole-task controls use a dedicated authenticated product route and append WorkTask actions directly, so corrections and reruns do not appear as internal command records in the conversation.

Each Session has one in-flight poll at the configured interval; publishing its snapshot does not trigger an immediate replacement read. Team discovery and exact-team detail responses supply the displayed team name. Terminal activity retains member failures while failed team-detail or deliverable reads retry before observation settles. Late reads preserve the latest pending action, local assessment, and explicit receipts; responses for a replaced run cannot write into the new task. Only the corresponding action’s actual HTTP success or rejection produces a receipt. Stage retries persist and replay one idempotency key per explicit user action, including after a lost response or another failure. Human answers persist the server’s `interaction_id` and submit it unchanged, so an old form cannot answer a later question in the same run.

The durable task records the exact wait kind and node supplied by Weave. Browser actions and commands accept stage retries only during a recoverable wait for that stage. The Host fetches human-task details from `/v1/human-tasks/:run_id`, retains the exact wait identity and response schema, and records an answer before submitting it to the existing completion endpoint with a stable idempotency key. The wait clears only after an authoritative response. Completed browser actions fold into durable acceptance or rejection receipts and are deduplicated on replay; accepted requests are not labeled completed work. Abandoned execution is distinct from confirmed cancellation: only Weave’s explicit `stop_unconfirmed` fact from the matching cancellation transition permits the stop-unconfirmed explanation.

The browser-authenticated `/api/weave.deliverable` route verifies the selected Session, current run, and known deliverable before streaming its complete retained content from Weave's `/v1/deliverables/:id/content`. The Host credential stays private. Downloads are attachments; SVG image previews have sandbox CSP, and HTML remains a static sandboxed browser document. A truncated projection does not become a truncated generated download or an invented local file.

The activity projection carries public runtime text, its transport completeness, and current physical-task identity. Repeated `(task_id, seq)` observations do not duplicate public records. Only an observed runtime capability marks live updates; older and other runtimes remain stage-completion updates. These public messages never establish final delivery or expose private reasoning.

<a id="surface-boundary"></a>
## Surface boundary

The Workbench client keeps the DSH session, optional project workspace, provider-neutral model settings, permission, approval, tool, and deliverable foundations. It fixes the foreground agent to the full standard capability preset and hides the agent-preset chooser because prompt-composition modes are a host concern rather than a business-task choice. It disables the inherited native DeepSeek adapter and official DeepSeek default, and also suppresses DSH's internal-testing notice, official-DeepSeek credential onboarding, Preview badge, official brand occupant, Subagent presentation, message-feedback controls, and trajectory inspection surface. Their generic DSH behavior remains unchanged in non-Workbench builds; the Workbench Models settings section remains the explicit place to configure a provider without blocking the browser with a vendor-specific first-run dialog.

<a id="credential-boundary"></a>
## Credential boundary

`WEAVE_API_KEY` is passed only from the trusted host process to the local MCP subprocess and authenticated Weave requests. It is not embedded in client artifacts and is not added to model context. Runtime-list responses expose only display, health, capacity, and scheduling facts; mutation responses expose a newly created runtime token once. `WEAVE_RUNTIME_SERVER_URL` contains no credential and is returned only as the connection address paired with that one-time token. `WEAVE_SECRET_KEY` and `WEAVE_SECRET_KEY_FILE` belong exclusively to the Weave server because they encrypt stored credentials; this bundle never reads or forwards either value.

<a id="dev-note"></a>
## Dev Note

None.

<a id="model-experience"></a>
## Model Experience

### Workbench team-routing persona

#### What the model sees

The profile registers the rule as a dedicated Workbench prompt section and fixes the foreground agent to the standard full-capability preset, so a session mode cannot shadow it. The foreground agent must list and match Weave teams for substantive business work, using tools such as `mcp__weave__team_list`, then confirm the selected team, task scope, and expected deliverables before dispatching the default workflow with `wait=false`. Existing explicit confirmation is sufficient; internal construction and evaluation add no user approval step. After dispatch it may make at most one status call to confirm the handoff, then it returns the selected team and a short human-facing state. It must not include internal identifiers, raw workflow versions, orchestration phases, or backend enums unless the user explicitly requests technical details. It must not create a DSH goal, poll the Weave run in the foreground, or save a duplicate deliverable. Workbench owns background status projection; the model checks for an exact-run final deliverable when the run is terminal or the user later requests it. If only stage records exist, it reports that final output remains unconfirmed. When a final deliverable exists, it reads it and answers with a short user-facing completion summary: what finished, the main findings or decisions, the files the user can open, and any action still needed. Internal run IDs, deliverable IDs, runtime IDs, host paths, hashes, validation command names, and engine details stay out of the main answer unless the user asks for technical details. With no match it must say so and collaborate on a team definition. It may use free collaboration only after an explicit user request, and it must report an unavailable Weave connection honestly.

#### Token effect

One stable product persona plus the tool schemas published by the Weave MCP server when connected.

#### KV Cache effect

Stable while the Workbench persona and connected Weave tool roster remain unchanged. Connecting, disconnecting, or changing the MCP tool set changes the request prefix.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>

- **Credential discovery differs by host** — the repository launcher supports the macOS Keychain; other packaged hosts currently provide `WEAVE_API_KEY` through their process environment.
- **Connection state has no dedicated card yet** — startup logs reveal an unavailable MCP process, while a Workbench-native connection indicator remains to be added.
