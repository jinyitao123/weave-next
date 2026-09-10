# Agent Note: Local Workbench resources in a native preview

Status: implemented

English | [中文](2026-09-08-native-workbench-preview.zh.md)

## Problem

A remotely loaded page leaves the desktop presentation tied to web hosting and adds a second privilege boundary around navigation. Reimplementing conversation rendering in a native shell duplicates existing Session, composer and work-scene behavior. The ongoing execution repair also makes a new parallel business transport contract premature.

## Decision

The [desktop preview](../../../../apps/desktop/README.md) packages the existing Workbench browser modules locally. Electron owns window chrome, native menus, connection selection, scoped downloads and exclusive application data. The feature view keeps sandboxing and context isolation, has no preload bridge, and loads only local application resources. This extends [selection ownership](2026-09-08-desktop-connection-selection.md); it does not supersede its pure policy or the [Host transport architecture](2026-07-19-gui-web-client-architecture.md).

The native resource assembler consumes built modules from the same Workbench profile layers. Its explicit fixture-only omissions are the dynamic Cordis runner and plugin-management panel, whose Host endpoints are absent. The existing schema compiler needs `unsafe-eval`; the local feature view retains that exception while its network policy admits only packaged resources. The connection chrome has separate strict CSP and IPC validation. Shared sidebar, composer, welcome and work-scene styling lives with its package owners and therefore reaches both desktop and web.

## Alternatives considered

A second chat renderer would own duplicated interaction and state. Remote HTML would turn frontend releases into service deployment dependencies. Packaging local shared resources preserves one implementation, at the cost of a resource build step and a large bundled Electron runtime. The [official Codex app example](https://learn.chatgpt.com/images/codex/app/codex-app-basic-light.webp) informs restrained sidebar and canvas hierarchy; it supplies no assumptions about Codex internals or copied branding.

## Consequences

The preview is runnable without a development server, but its conversations and service authentication are fixtures. It proves no real account, model, MCP, material or business completion. Native and web smokes exercise actual controls with private data; the web smoke additionally starts the supported Workbench profile. Signing, updates, other platforms and real Host integration remain separate. The generated Electron manifest is not an npm workspace or a new business CLI entrypoint.
