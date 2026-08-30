---
description: "Weave-native browser presentations for Workbench team discovery and final deliverables."
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

English | [中文](README.zh.md)

## Summary

`dsh-client-ui-weave` turns durable Weave MCP call results into Workbench product surfaces. It presents `mcp__weave__team_list` as candidate teams with business facts and presents `mcp__weave__deliverable_get` as a visible, downloadable final file instead of raw JSON.

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

Mount the package after `ui-tool`. When the Weave MCP server returns `team_list`, the conversation shows a `Team match` card. Candidate facts are visible immediately. When it returns `deliverable_get`, the conversation shows the title, inferred filename, full text preview, and a download action. Unavailable data, transport failures, interruptions, and malformed responses get explicit states.

The card deliberately does not claim a team was selected. Selection is confirmed by a later dispatch call. It also omits team ids, workflow ids, and internal health observations from the ordinary product surface.

<a id="understand-the-implementation"></a>
## Understand the implementation

The browser half registers the exact MCP wire names `mcp__weave__team_list` and `mcp__weave__deliverable_get` in ui-tool's keyed `tool.call.toolview` slot. Rendering derives only from the frozen call/result block, so history replay does not consult live Weave state. The deliverable view treats the returned content as a remote immutable file and downloads a browser-created copy; it does not pretend that the file exists in the local workspace.

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

- **Dispatch and human tasks are not specialized yet** — those calls still use the generic tool card.
- **Candidates are not a decision** — the list card exposes comparable facts but does not infer or persist a selected team.
- **Compact JSON is the contract** — non-array or team entries without stable ids and names fall back to a malformed-result state.
- **Deliverables are immutable remote files** — Workbench previews and downloads their recorded contents but does not materialize them into the active workspace.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

The Workbench profile intentionally keeps this package separate from generic DSH branding so upstream harness builds remain unchanged.

</details>
