---
description: "Weave-native browser presentations for Workbench tool calls, beginning with team discovery."
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

English | [中文](README.zh.md)

## Summary

`dsh-client-ui-weave` turns durable Weave MCP call results into Workbench product surfaces. Its first view presents `mcp__weave__team_list` as candidate teams with their purpose, scenario, responsibilities, completion standard, and default-workflow availability instead of raw JSON.

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

Mount the package after `ui-tool`. When the Weave MCP server returns `team_list`, the conversation shows a `Team match` card. Candidate facts are visible immediately. Unavailable workflows and repair states remain explicit; an empty list, transport failure, interruption, or malformed response gets its own honest state.

The card deliberately does not claim a team was selected. Selection is confirmed by a later dispatch call. It also omits team ids, workflow ids, and internal health observations from the ordinary product surface.

<a id="understand-the-implementation"></a>
## Understand the implementation

The browser half registers the exact MCP wire name `mcp__weave__team_list` in ui-tool's keyed `tool.call.toolview` slot. Rendering derives only from the frozen call/result block, so history replay does not consult live Weave state. Valid compact team documents become typed candidate cards; errors retain their durable diagnostic text.

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

- **Only team discovery is specialized** — dispatch, human-task, and deliverable calls still use the generic tool card.
- **Candidates are not a decision** — the list card exposes comparable facts but does not infer or persist a selected team.
- **Compact JSON is the contract** — non-array or team entries without stable ids and names fall back to a malformed-result state.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

The Workbench profile intentionally keeps this package separate from generic DSH branding so upstream harness builds remain unchanged.

</details>
