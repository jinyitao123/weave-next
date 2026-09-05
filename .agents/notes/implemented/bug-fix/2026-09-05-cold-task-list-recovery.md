# Agent Note: Cold task list recovery

Status: implemented

English | [中文](2026-09-05-cold-task-list-recovery.zh.md)

## Problem

A projection version change invalidates its persisted cache row. A cold Session with valid nonblank metadata could therefore lose its task status and team from the sidebar indefinitely, until someone opened the conversation. The project summary also omitted that task without identifying the missing observation.

## Decision

A domain marks values needed for Session lists with `wire.list: true`. Session Controller observes a cold log within its configured byte bound when such a value is unavailable. The Workbench profile uses a 4 MiB bound. The registry version and physical source identity govern reuse of the process-local observation; unrelated wire keys do not trigger full reads. The observation never activates an Agent or calls a model, and potentially uncommitted recovery events are not written into the durable projection cache.

The list exposes unavailable keys when a bounded observation cannot recover them. A computed null remains an ordinary value. The Client removes an unavailable marker once a newer projection arrives; stale list responses cannot restore it. Sidebar rows show that records still need reading, and project activity reports the incomplete observation. Opening the conversation retains the existing complete-read path.

## Alternatives considered

Serving an older state version could misrepresent changed task semantics. Unbounded list reads could make one large log stall navigation. Treating every missing wire key as a reason to refold would repeatedly read unrelated state. These options are rejected.

## Consequences

Small cold tasks return to the list automatically. Large or unreadable records remain visible and explicitly unknown. Regressions cover version invalidation, repeated listing, null values, oversized records, declaration disposal, push/list races, and project incompleteness. Real Workbench reload remains the integration acceptance for the shipped profile.
