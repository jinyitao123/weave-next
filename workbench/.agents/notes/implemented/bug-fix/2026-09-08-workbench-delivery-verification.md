# Agent Note: Workbench delivery verification and version-bound assessment

Status: implemented

English | [中文](2026-09-08-workbench-delivery-verification.zh.md)

## Problem

A workflow can finish with missing files, an unperformed review, or unintended external changes. A final summary and the reviewer's `PASS` text do not establish delivery quality. A user assessment attached only to a run can also appear to approve a replacement output.

## Decision

The [Workbench Host](../../../../packages/bundle/workbench-app/README.md) projects three independent facts: execution status, Weave's persisted delivery checks, and the user's assessment of a specific delivery revision. Weave owns the frozen requirements, output manifest and immutable reports. Workbench presents these facts through its existing task panels and commands; its agent loop and execution preset are unchanged.

The assessment request carries the displayed run and revision. The Host checks both against Weave and the latest local task before appending a user receipt. A late identity read cannot replace the current verification report. The receipt retains its run, revision, decision, note and time in the existing Session log. Temporary evidence failures hide its current applicability without deleting it. The same revision restores that assessment; a different revision starts unrated. Historical records without a revision remain readable but cannot receive a version-bound assessment.

Rechecking invokes Weave's bounded observation of saved results. It does not enqueue members or replace artifacts. The current report changes only when Weave's selection check succeeds; conflicting observations remain historical. The Host refreshes after rechecking, including when an older terminal poll is still in flight. An unavailable delivery read remains eligible for retry. An explicit unknown report or an unsupported server response can settle without claiming that a verification job is running.

The pilot export reports execution completion rate, verification counts and user assessments separately. Successful execution is not labeled business success.

## Alternatives considered

**Infer quality from completion, file count or review prose.** These facts cannot prove that the frozen requirements were checked or that extra external effects were absent.

**Attach the user's assessment to the run alone.** A replacement output can then inherit a decision made about different content. Binding to Weave's output revision preserves the decision's actual subject.

**Run verification inside the foreground agent.** That would duplicate team execution and permit model assertions to replace saved evidence. The Host reads Weave's report and invokes its existing observation endpoint.

**Add a shared acceptance service in this change.** The current consumer already has a durable Session log. Cross-Host authoritative acceptance requires its own ownership and synchronization decision; local receipts do not claim that capability.

## Consequences

Users can open available outputs while seeing failed or unknown checks. Their assessment remains independent from those checks. Rechecking cannot make unsupported professional review or file formats supported. Weave's registered verifiers are trusted application code; the Host does not provide a permission sandbox for those implementations.

The [Host action regressions](../../../../packages/bundle/workbench-app/tests/human-task-action.spec.ts) exercise late reads, temporary failures, replacement revisions and refresh races. The [Loader and JSONL regression](../../../../packages/bundle/workbench-app/tests/dispatch-input-loader.spec.ts) exercises actual Host composition and durable replay. The [client checks](../../../../packages/client/ui-weave/tests/work-task-recovery.client.spec.tsx) keep the three statuses and displayed revision arguments visible.
