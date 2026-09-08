---
description: "Select a desktop service and reject stale responses without changing Host or execution protocols."
kind: "package-library"
---

# Desktop connection selection

English | [中文](README.zh.md)

## Summary

The desktop source library preserves a selected service across restarts and prevents replies from an older selection from updating the active page. Trusted adapters supply service identity and authentication; an injected store saves only non-sensitive preferences. The library has no application entrypoint, package registration, or production consumer.

## Table of Contents

- [Use this library](#use-this-library)
- [Understand the implementation](#understand-the-implementation)
- [Further Exploration](#further-exploration)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)
- [Dev Note](#dev-note)

-----

<a id="use-this-library"></a>
## Use this library

Import [ConnectionSelection](src/connection-selection.ts) within the desktop source tree. Construct it with an exclusive atomic preference store and a first-launch default. Call `select` with trusted description and authentication adapters; success returns an immutable scope. Adapter or persistence failure preserves the saved choice. Corrupt saved data fails explicitly and requires restoration or an explicit settings reset by its storage owner.

Capture a scope before each operation and check `isCurrent` before updating active UI state. Starting a selection attempt invalidates earlier scopes and prevents new captures until it settles. Cancelling or failing a candidate allows fresh captures of the previous active identity, except when a replacement is detected at that active origin; that case clears readiness and keeps the pinned preference for resolution. `disconnect` invalidates local readiness while retaining the selected service. Preserve old business receipts under their original service; this library cannot cancel or reconcile server work.

From the `workbench` directory, the existing pinned toolchain runs these checks without registering a new package:

```sh
./node_modules/.bin/tsc -p apps/desktop/tsconfig.json
./node_modules/.bin/vitest run apps/desktop/tests/connection-selection.spec.ts
./node_modules/.bin/oxlint apps/desktop/src apps/desktop/tests
```

-----

<a id="understand-the-implementation"></a>
## Understand the implementation

<details>
<summary>Implementation responsibilities</summary>

Selection generations are local to one owner and separate from existing Host transport generations. Saved preferences contain only a version, origin, and expected instance. Scopes are owner-specific; an A→B→A transition or owner replacement cannot reactivate an old response. The policy has no HTTP parser, credentials, business states, or retry dispatcher. The [HTTP fixture](tests/fixture-service.ts) validates its own `/fixture/` responses and closes every listener during test teardown.

</details>

-----

<a id="further-exploration"></a>
## Further Exploration

The [decision record](../../.agents/notes/implemented/architecture/2026-09-08-desktop-connection-selection.md) explains why this policy remains independent of transport. The [consumer inventory](../../../docs/验收/2026-09-08-Workbench桌面并行准备/消费侧契约清单.md) distinguishes verified source fields from proposed integration requirements.

<a id="model-experience"></a>
## Model Experience

None. This source library registers no tools, events, prompts, or product UI.

<a id="known-limitations-and-deferred-work"></a>
## Known Limitations and Deferred Work

- The injected store must guarantee atomic writes; the library does not provide crash-safe disk persistence or cross-process coordination.
- Trusted adapters must isolate candidate credentials, reject cross-origin redirects, revalidate the authenticated instance and finish aborted network teardown. Client identity checks do not establish server authorization.
- Only the selected service is remembered. Historical operation identities and receipts belong to the operation owner; they are never replayed here.
- Electron, native credential storage, installation, updates, and real account/Host integration are outside this source library.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers</summary>

The [parallel task plan](../../../docs/计划/2026-09-08-Workbench桌面接入并行任务计划.md) owns integration order and deferred work.

</details>
