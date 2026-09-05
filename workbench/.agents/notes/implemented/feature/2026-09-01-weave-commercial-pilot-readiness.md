# Agent Note: Weave commercial pilots expose readiness and outcome evidence

Status: implemented

English | [中文](2026-09-01-weave-commercial-pilot-readiness.zh.md)

## Problem

Workbench could supervise and correct a Weave task after dispatch, but a first user had no truthful product surface for deciding whether the host, credential, teams, and runtimes were ready before dispatch. Completed work also lacked a lightweight user judgment, so a pilot could count technical success without knowing whether the delivery was adopted.

## Decision

The Workbench Host owns two authenticated exact Fetch routes. `/api/weave.status` probes the configured Weave service with the Host-only API key and returns only bounded readiness facts: service version, authorization outcome, dispatchable-team count, available-runtime count, and per-check status. `/api/weave.pilot-report` derives a secretless JSON report from durable `workTask` projections, covering completion, duration, recorded cost and tokens, correction count, rerun count, final-deliverable count, and the user's delivery outcome.

The Weave settings page presents the same readiness facts and the four-step first-use path. A one-time browser acknowledgement introduces durable task ownership without storing credentials. Completed tasks with a final deliverable accept one `adopted` or `needs-revision` judgment plus a bounded note through a human command; the command appends the whole updated work-task state and does not invoke a model. The report omits task prompts, run ids, request ids, credentials, runtime ids, and host paths.

Weave remains the source of execution usage. Its exact-run activity response projects existing terminal usage and start/finish facts; Workbench records those fields with the task instead of estimating cost. Missing or incomplete usage stays explicit in the activity completeness map.

Every terminal attempt can be revised into a new run, including failures and completed deliveries. The old attempt remains in history, while the new run starts with empty execution facts; terminal status, members, deliverables, corrections, timestamps, and usage never bleed across the run boundary. A fanout-waiting task whose external member is still running exposes the same correction composer as a directly running task. Weave durably records that correction and turns it into an impact plan at the next workflow boundary.

## Alternatives considered

**Add a separate analytics service.** Rejected because a design-partner pilot needs a bounded export over existing durable facts, not ingestion, dashboards, identities, or another retention lifecycle.

**Treat terminal success as delivery quality.** Rejected because successful execution only proves that the workflow terminated. A small user outcome captures whether the result entered real work without turning evaluation into a publish gate.

**Send the Weave API key to the browser.** Rejected because browser readiness needs status, not credentials. The authenticated Workbench Host performs every Weave probe and returns a secretless projection.

## Consequences

A first user can identify a missing connection, authorization, team workflow, or runtime before starting work and can download a pilot review from the same product. The report is operational evidence rather than billing-grade metering or an automated quality score. Session export, session deletion, deliverable download, and single-organization deployment controls remain the data-management boundary for this release; public multi-tenant retention and billing stay outside the Workbench pilot.
