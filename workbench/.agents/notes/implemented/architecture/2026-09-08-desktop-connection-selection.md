# Agent Note: Desktop service selection without transport ownership

Status: implemented

English | [中文](2026-09-08-desktop-connection-selection.zh.md)

## Problem

Changing a desktop service can race description, login, and older page responses. Reusing the Host transport generation would couple client preferences to active execution repair, while treating a successful login as a business receipt would invent authority the client does not have.

## Decision

The [desktop source library](../../../../apps/desktop/README.md) owns a non-sensitive selected origin and instance, a cancellable candidate, and response scopes. It commits selection only after trusted adapters agree on service identity and the exclusive preference store succeeds. Failed and cancelled candidates preserve preferences. Every attempt invalidates previous response scopes, including a return to the same origin; scopes cannot cross owners.

This decision does not supersede [Host ConnectionController ownership](2026-07-19-gui-web-client-architecture.md). That controller still owns RPC and transport reconnect generations. The new source module is not registered in a product profile and does not change it. It has no business receipt store, task state projection, or server cancellation path.

## Alternatives considered

Mutating the existing Connection package would mix desktop selection with current execution-related changes. A second full transport client would duplicate reconnect and RPC behavior. A source library using injected adapters isolates testable selection rules without adding another transport or package dependency.

## Consequences

Focused tests cover preference reload, aborted and superseded attempts, disk-write failure, instance mismatch, owner replacement, and real loopback connection loss. Fixtures are not account or business acceptance. Atomic disk durability, candidate session cleanup, server instance checks and production wiring remain adapter responsibilities. The source library alone is not an installable desktop application; its [native preview adapter](2026-09-08-native-workbench-preview.md) owns that separate boundary.
