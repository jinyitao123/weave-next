# Agent Note: Weave work tasks expose observed runtimes, tool facts, and files

Status: implemented

English | [中文](2026-09-01-weave-observed-work-files.zh.md)

## Problem

Workbench could supervise a durable Weave run, but the visible work scene still weakened the product claim. Runtime UUIDs obscured where a member ran, external Codex work ended with zero visible tool calls, and a final message could name CSV or JSON paths that the browser could neither inspect nor download. Resolving arbitrary host paths in the browser would have made the UI appear complete while bypassing Weave ownership and failing for remote runtimes.

## Decision

Weave remains the owner of execution facts and files. Its existing append-only run activity ledger receives bounded tool start/completion facts parsed from Codex JSONL, including bounded input and output previews. The server forwards each physical attempt's events from the remote-result decode boundary through the current execution context, before higher-level result aggregation can discard attempt detail; the terminal result remains a bounded fallback. Runtime activity joins frozen engine/provider/model facts with the registered runtime display name. Workbench only projects those observed facts; it does not infer tools from prose or replace an absent fact with a guess.

CLI workers publish files by writing supported UTF-8 formats below their reserved `outputs/` directory. The runtime snapshots eligible files before each invocation and returns only files that were created, rewritten, or changed, bounded to twelve files, 256 KiB per file, and 512 KiB total. Symlinks, traversal, host paths, unsupported binary formats, untouched stale files, and oversized files are excluded. Weave persists each returned file as an immutable exact-run deliverable with its relative filename and content type. Workbench previews the recorded content and offers a browser download only when its complete bounded content is present.

The Host `workTask` projection owns the durable browser copy. It records the bounded file content needed for download, names runtimes, and carries tool input/output previews. The projection schema version increments so restored sessions replay authoritative events rather than retaining the older path-only shape. The `team_create` MCP schema also makes a declarative workflow structurally required with YAML, preventing models from repeatedly submitting an incomplete team definition.

## Alternatives considered

**Dereference paths printed in the final answer.** Rejected because the path may belong to another host or a remote runtime, and allowing browser access to arbitrary paths would create a security boundary outside Weave's deliverable ledger.

**Add a general file browser or artifact service.** Rejected because the current product only needs the explicit outputs of a work task. A reserved bounded directory completes that contract without introducing storage APIs, upload sessions, or a second file lifecycle.

**Stream every CLI event live.** Deferred because terminal, observed tool facts already close the trust gap and fit the append-only activity contract. A live stream would add backpressure, reconnect, ordering, and retention semantics that the first product loop does not need.

## Consequences

Users can identify the actual runtime and engine for each member, inspect real Codex tool activity, and preview or download real CSV, JSON, Markdown, YAML, HTML, SVG, TSV, and text outputs after completion. The bounds keep runtime result payloads and Session projections finite, at the cost of excluding binary, oversized, untouched, or non-`outputs/` files. Tool input/output is a bounded observation and may be incomplete; the UI states only what Weave recorded. Tests pin Codex event normalization, physical-attempt event projection, stale-file exclusion, same-content rewrites, projection compatibility, and the assembled browser flow.
