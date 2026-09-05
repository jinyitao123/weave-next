---
description: "Weave Workbench brand occupants for maintainers building the Workbench browser identity on the generic DSH brand slots."
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-brand-workbench`

English | [中文](README.zh.md)

## Summary

This package fills the browser's existing brand slots with the Weave Workbench mark and the build-configured product title. It activates only when `DSH_CLIENT_BUILD_PROFILE=workbench`, so upstream Web and official builds retain their identities. It contributes presentation only and does not change prompts, tools, sessions, or persisted state.

## Table of Contents

- [Use this package](#use-this-package)
- [Dev Note](#dev-note)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)

<a id="use-this-package"></a>
## Use this package

Mount the package as a browser row and build with the `workbench` client profile. The package waits for all three generic declarations before installing the sidebar mark, sidebar name, and conversation hero mark as one reversible effect. The visible name comes from `DSH_CLIENT_TITLE`; the Workbench build profile sets it to `Weave Workbench`.

<a id="dev-note"></a>
## Dev Note

None.

<a id="model-experience"></a>
## Model Experience

None, as browser-side presentation occupants register nothing model-facing.

#### KV Cache effect

None. The package changes browser rendering only.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>

- **The title is a build-time value** — changing the product name requires rebuilding the browser artifacts; runtime profile reload changes no embedded browser value.
