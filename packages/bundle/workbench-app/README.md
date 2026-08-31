---
description: "Weave Workbench product profile for users and maintainers connecting the DSH browser runtime to durable Weave teams through MCP."
kind: "package-bundle"
---

# `@deepseek-ai/dsh-workbench-app`

English | [中文](README.zh.md)

## Summary

This bundle turns the general DSH Web runtime into Weave Workbench without changing the agent loop. It adds the Workbench browser identity, connects the local `weave mcp serve` process when a business API key is present, and gives the foreground agent one product rule: match an existing team before dispatching work, or state that no suitable team exists and help define one. A Host-side WorkTask projection records the dispatch and keeps its Weave status synchronized after the foreground turn ends.

## Table of Contents

- [Use this package](#use-this-package)
- [Surface boundary](#surface-boundary)
- [Credential boundary](#credential-boundary)
- [Dev Note](#dev-note)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)

<a id="use-this-package"></a>
## Use this package

Run `dsh --profile workbench` or the repository shortcut `pnpm workbench`. The shortcut prefers an explicit `WEAVE_API_KEY`, then checks the macOS Keychain service `weave-workbench-api-key`; other platforms use the explicit environment value. The shipped profile stacks `dsh-base`, `dsh-web-app`, and this bundle. When a key is present, the bundle starts `weave mcp serve` over stdio and publishes its tools under the `mcp__weave__*` namespace. This local Workbench defaults `WEAVE_API_URL` to `http://127.0.0.1:18080`, and `WEAVE_COMMAND` may select a different trusted local binary.

When `WEAVE_API_KEY` is absent, the MCP row is disabled and the browser still starts. This keeps the foreground runtime usable while making the missing Weave connection observable; the agent is instructed not to claim team work occurred without the tools.

When a dispatch succeeds, the Host stores its `client_request_id`, run, team, workflow, progress, runtime assignment, and blocker state in the Session log. A bounded poller reads `/v1/chat-requests/:client_request_id` with the same business identity and appends whole snapshots. Closing the conversation therefore stops neither Weave execution nor Workbench status recovery.

<a id="surface-boundary"></a>
## Surface boundary

The Workbench client keeps the DSH session, workspace, model, permission, approval, tool, and deliverable foundations. It suppresses DSH's internal-testing notice, official-DeepSeek credential onboarding, Preview badge, official brand occupant, Subagent presentation, message-feedback controls, and trajectory inspection surface. Their generic DSH behavior remains unchanged in non-Workbench builds, and the Workbench Models settings section remains available without blocking first-run dialogs.

<a id="credential-boundary"></a>
## Credential boundary

`WEAVE_API_KEY` is passed only from the trusted host process to the local MCP subprocess. It is not embedded in client artifacts and is not added to model context. `WEAVE_SECRET_KEY` and `WEAVE_SECRET_KEY_FILE` belong exclusively to the Weave server because they encrypt stored credentials; this bundle never reads or forwards either value.

<a id="dev-note"></a>
## Dev Note

None.

<a id="model-experience"></a>
## Model Experience

### Workbench team-routing persona

#### What the model sees

The profile tells the foreground agent to list and match Weave teams for substantive business work, using tools such as `mcp__weave__team_list`, then dispatch a suitable team's default workflow, stop for actual human tasks, and return the final saved deliverable. With no match it must say so and collaborate on a team definition. It may use free collaboration only after an explicit user request, and it must report an unavailable Weave connection honestly.

#### Token effect

One stable product persona plus the tool schemas published by the Weave MCP server when connected.

#### KV Cache effect

Stable while the Workbench persona and connected Weave tool roster remain unchanged. Connecting, disconnecting, or changing the MCP tool set changes the request prefix.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>

- **Credential discovery differs by host** — the repository launcher supports the macOS Keychain; other packaged hosts currently provide `WEAVE_API_KEY` through their process environment.
- **Connection state has no dedicated card yet** — startup logs reveal an unavailable MCP process, while a Workbench-native connection indicator remains to be added.
