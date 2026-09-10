---
description: "Host and Client session control: create, resume, prompt, follow history, and project live session state."
kind: "package-reference"
---
# Session Controller

English | [中文](README.zh.md)

## Summary

`@deepseek-ai/dsh-api-session-controller` owns the Host `ctx.sessionController` service and the generated Client `session`, `skills`, and `fileReferences` Remote namespaces. It serves Session lifecycle and history, the Host-generation model catalog, workspace-path opening, user-invocable skill discovery, and the adapter for Agent-scoped file references. Use it through API Gateway when a Client needs operations addressed by a Session.

## Table of Contents

- [Use this package](#use-this-package)
- [Configuration](#configuration)
- [Model Experience](#model-experience)
- [Known Limitations and Deferred Work](#known-limitations-and-deferred-work)
- [Dev Note](#dev-note)

-----

<a id="use-this-package"></a>
## Use this package

History pages and follow opening snapshots carry a discriminated `SessionHistoryRecord` using `{ type, event }`. `type: 'event'` carries a raw `SessionWireEvent`; `type: 'chunks'` carries a lossless `ChunkRowEvent` for consecutive same-block `assistant/chunk` deltas. Every inner value exposes `type`, `seq`, `time`, and `data`, so the Client retains accepted records without record-by-record conversion. A packed event's `seq` and `time` identify its first member; `data` retains fragment and timestamp-gap arrays. `type: 'projection'` carries a `history/projection` range with its first sequence, first timestamp, projection `key`, and inclusive `throughSeq`. Live follow frames remain individual events. Tool arguments, results, failures, and `tool/result.data.meta` pass through unchanged; the controller neither resolves Tool definitions nor runs presenters.

`registerHistoryProjection(definition: SessionHistoryProjection)` lets a domain declare a projection `key` and its snapshot `eventTypes`. The domain guarantees that the projection preserves every Client-visible fact in those snapshots; conversation, tool, and action-receipt events are not candidates. Registration belongs to the calling fiber and returns a disposer. Follow folds only declared non-surface snapshots covered by an available opening projection. Each consecutive run for one key uses one range record, without rewriting persistence. Page requests opt in with a `projectionBaseline` receipt containing the accepted follow cursor and keys; events after that cursor stay raw. Requests without the receipt return raw events and lossless chunk runs. The Client commits receipts only with an accepted opening, validates range coverage, and carries the receipt into older pages and repairs. See the [projection-backed history decision](../../../.agents/notes/implemented/architecture/2026-09-06-projection-backed-session-history.md).

Each endpoint states its activation policy. List, search, attachment, history pages, log following, skill discovery, and workspace-path opening can inspect persistence without activating an Agent; `canOpenWorkspacePath()` reports native-opening availability without addressing a Session. Queue mutation and cancellation require live state; model, rename, prompt, and file-reference operations may resolve or resume an ordinary Session. Create and fork are the only operations that create a new Agent directly. The skill catalog instead uses a live Agent when present or the recorded preset's standing scope when cold, so listing never starts an Agent.

Cold lists recover explicitly requested projection values when their cached state version is unavailable and the Session artifact fits `coldBlankProbeMaxBytes`. The observation reads persisted work without activating an Agent. Unchanged source files reuse a process-local observation; file identity or projection-version changes invalidate it. Oversized or unreadable records expose `projectionUnavailableKeys`, while an observed null value remains an ordinary value. Opening the Session supplies the complete history projection.

The Client adapter exposes `SessionEventStream`, a Gateway `RemoteJournalStream` bound to one ordinary or direct-subagent address. It opens follow before the initial page, publishes only contiguous `replace`, `prepend`, and `append` changes, and repairs reconnect or sequence gaps through a tail page. Ordinary records cover `[event.seq, event.seq]`; packed rows cover `[event.seq, event.seq + memberCount - 1]`, and projection ranges cover `[event.seq, event.data.throughSeq]`. A business, persistence, or unresolved continuity failure terminates the stream, while only physical carrier loss selects automatic resumption. `SessionControlStream` is a Gateway `RemoteSnapshotStream`; every generation opens with a complete process-local baseline, so reconnect replaces queue, jobs, and projection state instead of treating transient values as durable events.

The Session object also carries local submission echoes: `session.beginSubmission` inserts one into `SessionSnapshot.pendingSubmissions` synchronously, before the caller serializes and prompts, so a conversation UI can show the message on the submit click's own frame. The prompt's `requestId` is the correlation identity. The Host echoes it as `rpcId` on ordinary `{ kind: 'user' }` sources and on the `{ kind: 'plugin', plugin: 'ui-control', form: 'relay' }` source selected by `origin: 'ui-control'` for generated UI controls; both sources retain the optional Host-validated `clientTimeZone`. Queue occurrences project the identity as `SessionQueuedItem.rpcId`. An echo retires one animation frame after its durable event or queue occurrence is observed (the delay keeps it renderable until the transcript node is), immediately when its identified prompt fails or is abandoned, and as failed on disposal; each retirement fires the registered `onRetire` callback exactly once. Echoes are Client memory only — reload and reconnect rebuild the conversation from durable events alone.

-----

<a id="configuration"></a>
## Configuration

| Field | Default | Meaning |
|---|---:|---|
| `coldBlankProbeMaxBytes` | `1,024` | Maximum physical size eligible for cold blankness or requested projection recovery; `0` disables probes |
| `nativeOpen` | platform-detected | Whether Session workspace paths can be handed to a native desktop opener |

The generated [configuration catalog](../../../docs/config-catalog.md#deepseek-aidsh-api-session-controller) is the exhaustive source for accepted fields and their JSDoc.

-----

<a id="model-experience"></a>
## Model Experience

None, as invoked Agent commands own any model-visible effect.

#### KV Cache effect

No direct effect; model requests remain owned by the Agent and LLM packages.

## Known Limitations and Deferred Work

<a id="known-limitations-and-deferred-work"></a>

- Projection folding bounds each uninterrupted run of declared snapshots to one record; it does not impose a general byte limit on projections, messages, tool output, or unregistered history.
- Control baselines represent process-local state and therefore cannot reconstruct jobs after a Host restart.
- A failed follow resumption remains visible to the caller instead of retrying indefinitely.
- File-reference completion uses the shared Agent lookup and can resume a cold Session; the `skills/list` catalog is the non-activating alternative for skill metadata.


<a id="dev-note"></a>
### Dev Note

<details>
<summary>Working context for maintainers — click to expand</summary>

None.

</details>
