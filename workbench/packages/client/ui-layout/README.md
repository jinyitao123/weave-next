---
description: "Shell layout for the Web GUI: the three-column AppFrame with drag handles, concession behavior, the panel-geometry service, and theme presentation; for users and maintainers of the window chrome."
kind: "package-reference"
---

# @deepseek-ai/dsh-client-ui-layout

English | [中文](README.zh.md)

## Summary

This package provides the shell layout of the Web GUI: a three-column AppFrame with resizable sidebar and details panels, a concession chain that shrinks the details column and then auto-closes it when space runs out, and the `ctx.layout` panel-geometry service other plugins call to open or close the details column. It also seats the theme presenter, which projects the resolved color scheme, alias tokens, content font size, and `theme-color` metadata onto the document. Choose it for the standard window chrome; Workbench retains the scene width while panel open state resets on reload.

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

Mount this plugin at the root slot to render navigation, conversation, and details. Workbench opens the conversation and work scene at an initial 40/60 ratio, with readable minimum widths. Drag the separator or use its left/right arrow keys to resize; the last scene width survives close and reload. Limited space first collapses navigation to its 56px rail, then switches between conversation and scene without an overlay. Explicit focus mode fills the content area; ordinary opening restores the split. Both content trees remain mounted; hidden views are excluded from keyboard and assistive navigation. Other profiles retain the standard details concession chain.

### Theme presentation

The presenter consumes resolved theme snapshots and projects them onto the document: `html { color-scheme }` for native UA chrome, `body[data-ds-dark-theme]` from the active color scheme, the theme's alias tokens and `--dsh-content-font-size` as inline variables on body, and one owned `<meta name="theme-color">` whose content follows the computed body background. Disposing the presenter removes its metadata node with its other global writes.

-----

<a id="understand-the-implementation"></a>
## Understand the implementation

<details>
<summary>Implementation internals — click to expand</summary>

One `register()` call contributes `AppFrame` into the runtime's built-in `'root'` slot and, in the same breath, declares the four child slots (`sidebar`, `conversation`, `details`, `shell.overlay`), seats the layout store (panel geometry), and wires the `ctx.layout` panel-action service. The layout store starts the sidebar at its default width and details closed. Workbench stores only its user-dragged scene width under `weave.workbench.detailsWidth`; storage failures leave the current layout usable. AppFrame always mounts the conversation and details columns; a connected Session renders through `SessionProvider`. It projects the selected Session title over the build-configured product title or the localized `common.brand.localBuild` fallback, so locale revisions update document metadata with the root entry. The theme presenter is a second effect: pure DOM writes from resolved snapshots — initial state through the getter once, then event-driven only, with no React path. It applies palette, font-size, and token variables before measuring the rendered background as the single color authority.

</details>

-----

<a id="further-exploration"></a>
## Further Exploration

Read these pages when the layout surface is not enough. They move from the frame to the columns it renders and the theme it presents.

- [ui-sidebar](../ui-sidebar/README.md) — occupies the `sidebar` column and its seats.
- [ui-conversation](../ui-conversation/README.md) — occupies the `conversation` and `details` columns.
- [ui-theme](../ui-theme/README.md) — the theme seam whose resolved snapshots the presenter consumes.
- [Web client architecture](../../../.agents/notes/implemented/architecture/2026-07-19-gui-web-client-architecture.md) — how browser plugin rows load and register slots.

-----

<a id="model-experience"></a>
## Model Experience

None, as the layout shell manages browser viewing state; nothing here reaches a model request.

#### KV Cache effect

None; this package neither assembles nor sends a provider request.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>


These limits define the current layout behavior. They are current package constraints, not a general window-manager comparison or a task backlog.

- **Open state is transient** — reload and switching to another Session close details. Workbench retains its preferred scene width; other profiles reset it. Unselected surfaces render details at zero width without modifying geometry.
- **Concession-chain auto-close derives a zero width without touching the preferred width** — the panel restores itself when the window widens; consumers must not read the stored details width as the rendered truth.
- **No scroll anchoring during squeeze reflow** — layout changes may move the reader's viewport.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

None.

</details>
