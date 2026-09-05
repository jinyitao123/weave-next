---
description: "JSON-RPC protocol and stdio server retained for the Workbench runtime."
kind: "package-group"
---

# sdk/ — drive a Harness runtime from another process

English | [中文](README.zh.md)

## Summary

This group retains the JSON-RPC wire protocol and stdio server used by the runtime. The standalone TypeScript and Python clients are not included in this workspace. Each package README owns its protocol or server contract.

## Table of Contents

- [Packages](#packages)
- [Related documentation](#related-documentation)
- [Dev Note](#dev-note)

-----

<a id="packages"></a>
## Packages

Each package README describes what you can do with its part of the stack.

| Package | Role |
|---|---|
| [`protocol/`](protocol/README.md) | Wire protocol: the newline-delimited JSON-RPC transport and the named request, result, and notification types |
| [`server/`](server/README.md) | `jsonrpc` plugin that serves out-of-process SDK clients over stdio |

-----

<a id="related-documentation"></a>
## Related documentation

See the protocol and server package references, then the decision records for historical context.

- [SDK application bundle](../bundle/sdk-app/README.md) — the `dsh --profile sdk` application that boots the JSON-RPC server.
- [Python profile-runtime decision](../../.agents/notes/implemented/architecture/2026-08-23-python-sdk-dsh-profile-runtime.md) — why the packaged Python client launches the same named profiles.
- [TypeScript SDK and SDK subagent backend decision](../../.agents/notes/implemented/feature/2026-07-27-typescript-sdk-and-sdk-subagent-backend.md) — the client contract and the subagent backend built on it.
- [SDK project toolchain removal](../../.agents/notes/implemented/simplification/2026-08-11-remove-sdk-project-toolchain.md) — why this group never creates, configures, or builds developer projects.

<a id="dev-note"></a>
## Dev Note

None.
