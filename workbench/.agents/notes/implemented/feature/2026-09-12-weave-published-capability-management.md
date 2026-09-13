# Agent Note: Workbench manages published capability access

Status: implemented

English | [中文](2026-09-12-weave-published-capability-management.zh.md)

## Problem

Weave can expose a published workflow as a versioned service capability, but operators need one business interface for creating calling applications, rotating their credentials, publishing immutable capability versions, granting access, and stopping an invocation. Managing these records through database access or ad hoc HTTP calls would bypass the supported Workbench interface and make credential handling and operational state hard to review.

## Decision

Workbench Settings contains a Published Capabilities section. An administrator creates a stable application identity and capability, issues or revokes application credentials, binds the next immutable capability version to an admitted published workflow, enables or disables new calls, and grants or revokes one application’s access to one exact version. The page lists recent invocations and presents execution, result availability, and cancellation acknowledgement as separate facts.

The Workbench Host owns the browser route `/api/weave.capabilities`. It accepts only a closed set of management actions, authenticates upstream requests with the Host-only `WEAVE_API_KEY`, and maps upstream failures to bounded browser error codes. Application credentials are returned to the creating browser once; stored credential hashes, invocation inputs, and the Host credential never enter the browser response.

The browser reads only the management projection and sends explicit administrator actions. It does not derive execution truth, mutate a published release, or invoke a model. Disabling an application, capability, or release stops later submissions while historical invocations remain readable. Administrator cancellation uses Weave’s durable cancellation and TeamRun control path and records the administrator as the actor.

## Alternatives considered

**Add another administration application.** Rejected because Workbench is the supported business interface and already owns authenticated settings surfaces. A second console would split the operator path and credential boundary.

**Expose the Weave API key to the browser.** Rejected because the browser needs bounded management results rather than an unrestricted business credential. The Host can authorize exact operations without transferring its credential.

**Edit a published capability version.** Rejected because service callers need a stable version identity. Contract, workflow, limits, and result policy changes produce the next version while enablement remains a separate reversible control.

## Consequences

Operators can complete the capability access lifecycle in Workbench and can rotate an application credential without changing the application identity. The page is an administration view over Weave’s durable records; it does not prove that a particular business capability produces acceptable results. Business acceptance still requires real inputs, a service invocation, the returned result, and independent review.
