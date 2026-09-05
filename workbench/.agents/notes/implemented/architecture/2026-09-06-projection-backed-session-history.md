# Agent Note: Projection-backed Session history

Status: implemented

English | [中文](2026-09-06-projection-backed-session-history.zh.md)

## Problem

A Session can contain a few conversation messages followed by thousands of complete background snapshots. Pagination by message count carries all those snapshots in the opening frame. One observed Workbench Session produced about 199 million characters even though its displayed conversation was short. Repeating state also increases browser memory and transport instrumentation costs.

Reducing new writes does not remove existing history. Truncating the event tail can hide the conversation behind many pages of snapshots, while deleting arbitrary events violates the journal's sequence continuity. The current domain projection already carries the finished state used by the Client, but the carrier needs an explicit rule for which historical events it can represent through that state.

## Decision

Session Controller owns a reversible `registerHistoryProjection` contribution. A domain declares its Client projection key and log-only snapshot event types, and guarantees that this projection retains every Client-visible fact from those snapshots. Conversation, tool, and action-receipt events remain raw. The controller has no Workbench-specific event list; Workbench declares only `weave/work-task` under `workTask`.

An exact follow opening carries the authoritative projection baseline and replaces each uninterrupted declared snapshot run with one `history/projection` record. The record preserves the first sequence and timestamp, projection key, and inclusive final sequence. Surface records and undeclared events interrupt runs and remain intact. Live follow events remain raw. Persistence, replay, and raw log export are unchanged.

The Client validates each range against the opening projection keys and watermark before Gateway accepts the contiguous page. It commits the projection receipt only when Gateway publishes the opening replacement. Older-page and gap-repair requests carry that receipt; the Host can fold only declared snapshots covered by its cursor. Events beyond the accepted watermark stay raw. Pages without a receipt return raw events plus the existing lossless Assistant chunk packing.

This partially supersedes the universal raw-history statement in [Session history and event transport](2026-08-18-session-history-and-event-transport.md), while retaining its activation rules, raw live journal, cursor continuity, and Gateway ownership. It applies the authoritative Client projection rule from [Session observations and projection-owned state](2026-08-25-session-observations-and-projection-owned-client-state.md). Both older decisions remain active.

The Workbench poller compares field values independently of object property order. An unchanged attempt retains its last change time, so observing the same run does not create a business change through a nested timestamp. Business changes append immediately; unchanged observations retain the existing 30-second freshness refresh. This reduces future snapshot growth while the history representation handles existing logs.

## Alternatives considered

- A byte-limited raw tail preserves events but can leave the initial conversation empty and require many snapshot-only pages before reaching user messages.
- Removing snapshots without explicit ranges breaks journal continuity and makes reconnect repair ambiguous.
- Generic JSON differences preserve every historical value but add a codec and Client reconstruction work for facts whose current owner is already a projection.
- Rewriting persisted history changes audit and replay facts and does not belong to this read-path correction.

## Consequences

Redundant snapshots are bounded to one record per uninterrupted declared run, so increasing only that run's length does not repeat its state in the opening payload. This is not a universal byte limit: large projections, tool output, messages, or undeclared events can still produce large pages. Original large logs also remain a Host storage and observation cost.

A domain registration is a semantic obligation. A future Client needing historical facts absent from the projection must retain those events or add an explicit history reader instead of declaring them disposable. Unavailable projection keys disable folding, and disposing either the declaration or projection restores raw history reads.

## Testing

Controller tests cover 12,000 large snapshots with a complete UTF-8 opening below 52 KB, retained user and assistant messages, raw tool metadata and actions, current projection state, raw live continuation, unchanged original events, page receipts, range boundaries, unavailable projections, and registration disposal. Client adapter tests cover contiguous projection ranges, live appends, older-page receipts, and rejection of ranges outside the accepted baseline.

The Workbench poller regression checks 60 consecutive observations without an extra business snapshot, the exact 30-second freshness refresh, and an immediate stage-progress update that preserves the attempt creation time.
