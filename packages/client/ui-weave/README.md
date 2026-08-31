---
description: "Weave-native browser presentations for Workbench team discovery and final deliverables."
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

English | [中文](README.zh.md)

## Summary

`dsh-client-ui-weave` turns durable Weave MCP call results into a visible work task. Workbench shows the selected team, workflow progress, active runtimes, human decisions, exact-run deliverables, and explicit blockers while preserving the conversation as the control surface.

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

Mount the package after `ui-chat` and `ui-tool`. The first recorded Weave call opens the right-side work-status panel and adds a compact status beside the Session title. The panel prefers the Host `workTask` projection, which continues to update after the foreground turn ends, and retains richer stage details from loaded conversation history. A deliverable counts toward the task only when its recorded `run_id` matches the active run.

The conversation keeps specialized `team_list` and `deliverable_get` cards. The work-status panel keeps opaque ids under a collapsed diagnostic section and reports a queued run or missing team runtime as an explicit issue.

<a id="understand-the-implementation"></a>
## Understand the implementation

The browser half registers the exact MCP wire names `mcp__weave__team_list` and `mcp__weave__deliverable_get` in ui-tool's keyed `tool.call.toolview` slot. It also occupies the Session-header action list and the Chat details summary slot. The Workbench Host folds MCP facts into the durable `workTask` projection and synchronizes the corresponding Weave request outside the foreground turn. The deliverable view treats returned content as a remote immutable file and downloads a browser-created copy; it does not pretend that the file exists in the local workspace.

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
- **Runtime detail depends on recorded status** — the panel shows role, provider, and model only when Weave status results include them.
- **Compact JSON is the contract** — non-array or team entries without stable ids and names fall back to a malformed-result state.
- **Deliverables are immutable remote files** — Workbench previews and downloads their recorded contents but does not materialize them into the active workspace.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

The Workbench profile intentionally keeps this package separate from generic DSH branding so upstream harness builds remain unchanged.

</details>
