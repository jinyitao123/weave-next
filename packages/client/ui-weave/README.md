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

Mount the package after `ui-chat` and `ui-tool`. A blank Session places a runtime-management entry directly above the main composer. The first recorded Weave call opens the right-side work-status panel and adds a compact status beside the Session title. The panel prefers the Host `workTask` projection, which continues to update after the foreground turn ends, and retains richer stage details from loaded conversation history. A deliverable counts toward the task only when its recorded `run_id` matches the active run. The main-page runtime center shows availability, capacity, engine, recent connection, and failover-group membership, with add, configure, and remove actions; an active task shows only the nodes it actually used so the registry does not crowd out work information.

The conversation keeps specialized `team_list` and `deliverable_get` cards. The work-status panel keeps opaque ids under a collapsed diagnostic section, translates stage, tool, file-type, status, and legacy task-action records into product language, names each assigned runtime with its engine/provider/model facts, and reports a queued run or missing team runtime as an explicit issue. Full task requirements and tool input/output remain available only under explicit disclosures.

<a id="understand-the-implementation"></a>
## Understand the implementation

The browser half registers the exact MCP wire names `mcp__weave__team_list` and `mcp__weave__deliverable_get` in ui-tool's keyed `tool.call.toolview` slot. It also occupies the Session-header action list and the Chat details summary slot. The Workbench Host folds MCP facts into the durable `workTask` projection, synchronizes the corresponding Weave request outside the foreground turn, and serves browser-authenticated runtime and task-action routes without exposing the business API key. The work-task surface keeps file bodies out of the document until the user expands one deliverable, and the conversation card bounds its visible preview while retaining the complete recorded content for download. The browser downloads a generated copy when the complete bounded file is present; it does not pretend that the file exists in the local workspace or dereference a path printed by an agent.

<a id="further-exploration"></a>
## Further Exploration

- [ui-tool](../ui-tool/README.md) — owns the keyed tool-view slot and generic fallback.
- [Workbench app](../../bundle/workbench-app/README.md) — mounts the Weave connection and this presentation package.

<a id="model-experience"></a>
## Model Experience

None, as this package only renders logged tool calls in the browser and does not add tools, prompts, messages, or provider request content.

#### KV Cache effect

None; presentation happens after the runtime records the call result.

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
