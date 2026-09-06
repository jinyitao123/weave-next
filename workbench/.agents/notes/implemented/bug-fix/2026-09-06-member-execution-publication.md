# Agent Note: Member execution publication

Status: implemented

English | [中文](2026-09-06-member-execution-publication.zh.md)

## Problem

Saving an engine choice only on the member leaves published workflows using a different frozen configuration.

## Decision

Weave versions the member and publishes affected workflows in one transaction. Conflicting drafts or invalid candidates roll back the complete edit. Existing runs keep their settings. Native CLI model names remain in the frozen member record, without platform provider bindings.

## Alternatives considered

**Save before publication.** A publication failure leaves the next task using settings different from the page.

## Consequences

The browser clears model choices when changing engines. Node defaults describe configuration; execution receipts identify reported models. Public attempt history does not imply model text or final delivery. Browser and Host regressions verify model clearing, credential omission, and conflict handling. Database validation verifies publication and rollback.
