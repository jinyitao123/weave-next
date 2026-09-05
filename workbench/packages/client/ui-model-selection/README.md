---
description: "Model selection for the Web GUI: the /model popup and the composer model seat over one per-session provider-grouped directory; for users and maintainers of model routing."
kind: "package-reference"
---

# @deepseek-ai/dsh-client-ui-model-selection

English | [中文](README.zh.md)

## Summary

This package provides model selection in the Web GUI: the `/model` popup command and the composer's model seat, both over one per-session directory of provider-grouped models. Choosing a model submits the complete selection — provider, model, and reasoning effort — which the Host snapshots at the next prompt-assembly boundary, so the following request uses it while a running step keeps its assembled selection. The composer seat opens a compact model-and-effort popover: the current model drills into the provider-grouped model list, while the selected exact model supplies a discrete effort slider, its adapter-owned effort names, and its default. When the Host reports that no adapter serves the session's route, the composer input goes inert until a route becomes available.

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

Mount this plugin alongside `ui-conversation` and the commands package; the composer then shows the model seat next to the pending indicator, and `/model` opens the same directory as a popup. Both surfaces show the host-reported current selection when the exact provider/model pair remains in the advertised groups; a missing catalog row leaves the routable selection intact and the trigger displays its provider/model identifier.

### Model and effort

Models stay grouped by provider. The compact overview keeps the current model, localized standard effort name, and discrete effort slider together; selecting the model summary opens the provider-grouped list with an explicit back action. The card keeps one width across both panes, and choosing a model returns to its effort overview without closing the card. Slider ticks are exactly the efforts advertised by the selected model, and the native range control preserves arrow-key and assistive-technology operation. The slider follows pointer movement locally and commits the selected level on release, avoiding a transient disabled flash. The `/model` popup applies the selected model's default effort; the composer can then choose any advertised effort without closing the popover. An adapter without reasoning metadata leaves the slider absent; there is no arbitrary effort input.

### Unroutable sessions

When the Host reports that no adapter serves the session's route, the trigger says `No usable model selected` and opens the model list directly. The composer blocks submission while retaining its draft and keeping model selection available. An empty list directs the user to Settings in the sidebar and the Models page. Saving settings, updating adapters, or changing credential references refreshes the shared catalog and revalidates the current route. A durable selection that remains unavailable requires an explicit replacement; recovery clears the composer block without reloading the page. A `null` before the first load or after one failed never blocks, and catalog membership alone never blocks a route the Host can serve.

-----

<a id="understand-the-implementation"></a>
## Understand the implementation

<details>
<summary>Implementation internals — click to expand</summary>

The `/model` popup and composer seat share each session's `ModelDirectory`, which combines the Host-generation catalog with the durable selection projection. Model selection uses `session.selectModel`; settings, adapter, and credential changes invalidate the shared catalog so every resident directory recomputes route availability. Catalog refreshes reject stale responses by generation, and the selection projection retains the session's choice across reconnects. Directories are resolved lazily and disposed with the session scope; addressed subagent sessions expose neither entry.

</details>

-----

<a id="further-exploration"></a>
## Further Exploration

Read these pages when the model surface is not enough. They move from the browser surfaces to the command popup shell and the selection contract.

- [ui-commands](../ui-commands/README.md) — the popupSelect shell the `/model` contribution registers into.
- [ui-conversation](../ui-conversation/README.md) — declares the composer's `conversation.input.model` seat and the composer block.
- [dsh-agent-default-model](../../core/agent-default-model/README.md) — the default-model service for sessions that never choose.
- [Client package map](../README.md) — adjacent browser UI packages.

-----

<a id="model-experience"></a>
## Model Experience

Indirectly, through the `session.selectModel` selection both entries submit: the Host snapshots the complete `ModelSelection` at the next prompt-assembly boundary and owns the model-visible effect, while a running step keeps its assembled selection.

#### KV Cache effect

Switching the route can reduce or invalidate provider-side cache reuse for subsequent requests; the prompt prefix itself is untouched.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>


These limits define the current model surface. They are current package constraints, not a general model-router comparison or a task backlog.

- **No create-time or addressed-subagent selection** — both entries require an existing ordinary session's Agent; there is no draft-phase model choice to fold into session creation, and subagent continuation deliberately exposes no independent model-selection contract.
- **Directory names are presentation-only** — selection and persistence use provider/model/effort ids; a provider whose catalog or exact-model metadata lookup fails lists as an unselectable failure row until reload.
- **Settings navigation remains in the sidebar** — the empty selector gives the configuration path; it does not open the settings dialog directly.
- **No arbitrary effort input** — the composer offers only the exact model's adapter-advertised levels; an adapter without reasoning metadata leaves the Effort row absent.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

None.

</details>
