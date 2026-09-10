---
description: "Run the shared Workbench frontend in a native macOS preview and verify isolated service selection."
kind: "package-reference"
---

# Workbench desktop preview

English | [中文](README.zh.md)

## Summary

This Electron preview packages the existing Workbench frontend with native window controls, menus, connection settings and downloads. The current target is macOS arm64. Two owned loopback services and the existing browser fixture exercise interaction without connecting an account or starting business work.

## Table of Contents

- [Run the preview](#run-the-preview)
- [Understand the implementation](#understand-the-implementation)
- [Further Exploration](#further-exploration)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)
- [Dev Note](#dev-note)

-----

<a id="run-the-preview"></a>
## Run the preview

Use the pinned pnpm toolchain from the `workbench` directory. Build the shared frontend after changing its packages; desktop resource assembly consumes those outputs. Electron, Packager and Playwright are exact development dependencies in the workspace root. Playwright's Chromium headless shell is required for icon rendering and browser verification.

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium --only-shell
pnpm run build:workbench
pnpm run desktop:dev
pnpm run desktop:package
pnpm run desktop:test
node apps/desktop/scripts/web-smoke.mjs
```

Packaging writes `Weave Workbench Preview.app` under `.dsh-build/desktop/Weave Workbench Preview-darwin-arm64/`. The `.app` contains its runtime and local frontend; it needs no development server. `Cmd+,` opens connection settings, `Cmd+N` starts a frontend session intent, and `Cmd+B` toggles the existing sidebar. Editing uses native menus and right-click controls. Closing the application stops only its owned preview listeners.

The default data directory is `~/Library/Application Support/Weave Workbench Preview`. A test can supply `--desktop-data=<absolute-directory>` to own separate preferences, ports and browser partitions. Connection settings accept only the two displayed fixture services. Corrupt connection data or occupied saved fixture ports fail visibly; the app does not silently reset them. The toolbar labels this as a preview environment.

-----

<a id="understand-the-implementation"></a>
## Understand the implementation

<details>
<summary>Native and frontend responsibilities</summary>

[ConnectionSelection](src/connection-selection.ts) owns selected origin, pinned instance and response scopes; it has no business state or server cancellation authority. [FilePreferences](src/preferences.ts) writes an exclusive temporary file, flushes its contents and atomically renames it. The rename is the commit point; directory synchronization and power-loss durability are not promised. A single application instance owns its data directory.

The [main process](src/main.ts) loads trusted connection chrome through `weave-shell://app` and the shared Workbench through `weave-app://workbench`. The feature view has no preload, Node integration, filesystem or shell API. Only the trusted chrome gets a validated, narrow IPC bridge. Each origin and pinned instance has a separate persistent UI partition; candidate authentication uses a new in-memory partition that is cleared on retirement. A failed switch preserves the selected preference and leaves the client disconnected. Downloads remain under the accepted scope and are cancelled when it retires; interrupted transfers report failure. A stale completion cannot update the new service.

The [resource assembler](scripts/frontend.mjs) composes the existing Workbench profile's browser modules. The standalone preview omits the dynamic Cordis runner and its management panel because it has no Host plugin-management endpoints. It uses the existing fixture transport; the web profile retains its normal composition. Shared conversation, sidebar and work-scene components keep their owning packages. The existing schema compiler requires `unsafe-eval` in the local feature view's CSP; the view accepts only packaged resources and cannot fetch remote scripts or services. The trusted connection chrome has a separate strict CSP.

</details>

-----

<a id="further-exploration"></a>
## Further Exploration

[Selection ownership](../../.agents/notes/implemented/architecture/2026-09-08-desktop-connection-selection.md) describes the reusable policy. [Native resource ownership](../../.agents/notes/implemented/architecture/2026-09-08-native-workbench-preview.md) records the application boundary. The [parallel task plan](../../../docs/计划/2026-09-08-Workbench桌面接入并行任务计划.md) owns later integration.

<a id="model-experience"></a>
## Model Experience

None. The preview adds no model tools, prompts or execution protocol. Its frontend shows synthetic fixture conversations and does not call a real model.

<a id="known-limitations-and-deferred-work"></a>
## Known Limitations and Deferred Work

- Real accounts, Host transport, MCP, material dispatch and business receipts require integration against the accepted execution-repair commit. Local fixture authentication proves no user authorization.
- The current app is an unsigned macOS arm64 preview. Windows, other architectures, notarization, updates and clean-device installation are unverified.
- Saved UI partitions are separated by service and instance; there is no implemented multi-user logout policy or operating-system credential store.
- There is no npm workspace package or business CLI entrypoint here. Build scripts emit the Electron manifest only into generated resources; supported Node Hosts still start through `dsh` profiles.

<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers</summary>

The [native smoke](scripts/smoke.mjs) owns disposable data and listeners and can test the packaged executable through `WEAVE_DESKTOP_EXECUTABLE`. The [web smoke](scripts/web-smoke.mjs) launches a private, keyless supported Workbench profile. Neither submits business work. Release acceptance remains separate from these preview checks.

</details>
