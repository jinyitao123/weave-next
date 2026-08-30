# Agent Note: Weave Workbench on the DeepSeek Harness foundation

Status: proposed

English | [中文](2026-08-30-weave-workbench-dsh-foundation.zh.md)

## Problem

Weave has a reliable team execution service, but its product interface duplicates capabilities already present in coding agents: conversation, task interpretation, planning, tool presentation, and project navigation. Keeping a second general-purpose application makes users learn two interaction models and makes Weave maintain product code outside its core responsibility.

Codex proves the intended Weave workflow through MCP, but a Codex-only product cannot offer a Weave-owned desktop distribution, provider choice, or interaction design. Building that application from an empty shell would recreate session persistence, model streaming, tool calls, approvals, terminal integration, settings, and packaging before it could validate one Weave task.

## Proposal

Weave Workbench is a separate application repository derived from DeepSeek Harness. The repository pins upstream commit `cd5ef8148158c3a752a658978873241fdf8e2bbc`, tracks the official project through the read-only `upstream` remote, and keeps Weave changes on its own branch and future private origin. The partial clone retains the complete pinned Git tree while downloading source blobs on demand.

DeepSeek Harness owns the foreground interaction runtime: conversations, model providers, sessions, tool calls, approval presentation, terminal access, settings, and desktop packaging. Workbench changes branding and assembles a Weave profile; it does not replace the DSH agent loop or create a parallel application framework.

Workbench removes development-facing DSH presentation only at its profile boundary: the internal-testing and official-provider onboarding dialogs, Preview badge, official brand, Subagent presentation, message feedback, and trajectory inspector are absent. The underlying runtime services remain intact, and non-Workbench DSH builds retain their original behavior.

Weave remains the business execution service. The Workbench agent reaches it only through the public Weave MCP tools. A normal request follows one product sequence: inspect teams, dispatch through the selected team's default workflow, request user input only for a yielded human task, and read the final deliverable after completion. Workbench may coordinate several team runs for a complex FDE engagement, but Weave owns each durable run and deliverable.

The MCP integration exposes `WEAVE_API_KEY` to the local Weave MCP subprocess through an operating-system credential or a process-scoped environment value. The key authorizes product API calls and can be rotated or revoked. `WEAVE_SECRET_KEY` stays inside the Weave server because it encrypts server-managed credentials; the Workbench process, its model, tool output, logs, and settings never receive that secret.

The user-facing run states are `queued`, `running`, `yielded`, `completed`, and `failed`. Weave-owned browser plugins progressively replace generic MCP JSON with narrow product cards while preserving the recorded tool result as the only authority. The team card renders `team_list` as candidate facts and default-workflow availability; it neither invents a selection nor exposes internal health observations. The final-deliverable card renders `deliverable_get` as a titled file with its complete recorded text and a browser download action. It does not imply that a remote Weave artifact already exists in the local workspace. Later dispatch and human-task cards render the five product states and retain the run and human-task identifiers. Weave's detailed lifecycle classifications remain diagnostic metadata and do not become additional product states.

Workbench does not add pages for team editing, workflow editing, evaluation runs, build candidates, or governance. Team creation remains an MCP conversation followed by one declarative submission after user confirmation. The existing Weave application remains a runtime operations console until Workbench can present node registration, connectivity, capacity, engine support, and failure state without importing Weave's internal orchestration code.

Implementation proceeds in three independently usable increments. The first increment builds the pinned DSH desktop and Web applications unchanged and records a smoke baseline. The second adds Workbench identity, a bundled Weave MCP profile, credential setup, and product-state tool cards while preserving the upstream agent loop. The third moves only the multi-runtime operations view into Workbench and removes the standalone Weave console after parity verification. Every increment rebases or merge-forwards from a named upstream commit and keeps Weave-specific changes separable from upstream code.

## Alternatives considered

**Use Codex as the only interface.** This remains the fastest integration and the reference client for MCP behavior, but it leaves distribution, provider selection, and Weave-specific interaction outside the product's control. Workbench must therefore preserve Codex compatibility instead of replacing it.

**Extend the existing Weave Web application.** That code already has runtime data, but turning it into a coding-agent interface would require a new session runtime, tool protocol, approval model, terminal, provider system, and packaging layer. It would also invite Weave's internal administration concepts back into the user workflow.

**Build a minimal desktop shell from scratch.** A smaller initial codebase would offer complete ownership, but most early work would reproduce solved harness infrastructure rather than validate Weave. The option becomes preferable only if upstream DSH integration costs repeatedly exceed the cost of the retained capabilities.

**Embed DSH packages without deriving its repository.** Package-level reuse reduces the visible fork, but DSH is a pre-release monorepo whose supported application launch is profile-owned and whose packages evolve together. Deriving the repository preserves its tested assembly and makes upstream changes reviewable as Git history.

**Move team and workflow orchestration into Workbench.** This could make the client appear self-contained, but it would create two authorities for retries, human waits, terminal state, and deliverables. Workbench coordinates user intent; Weave remains the only durable execution authority.

## Acceptance criteria

- The Workbench repository identifies the pinned DSH commit, keeps `upstream` read-only, and can materialize any omitted source path from that commit.
- An unchanged baseline build launches the supported DSH Web or desktop entry path before Weave-specific runtime changes begin.
- A bundled profile starts the local Weave MCP server without copying `WEAVE_SECRET_KEY` into application configuration, model context, tool results, or logs.
- A user can complete the Weave sequence `team_list` to `team_dispatch` to optional human-task completion to `deliverable_get` in one Workbench conversation.
- `team_list` renders as candidate business facts rather than raw MCP JSON, does not claim that a candidate was selected, and omits internal health observations from the ordinary surface.
- `deliverable_get` renders the title, inferred filename, complete recorded contents, and download action without requiring the user to inspect raw MCP JSON.
- Run presentation branches only on `queued`, `running`, `yielded`, `completed`, and `failed`; diagnostic lifecycle data remains inspectable without blocking use.
- Workbench contains no duplicate team, workflow, evaluation, candidate, or governance application pages, and multi-runtime UI migration requires explicit parity evidence before the standalone console is removed.

## Risks

DSH is an alpha foundation with no compatibility promise. Upstream changes can reshape packages, profiles, session formats, and UI composition, so Workbench must pin each integration point, keep its changes in narrow plugins or profiles, and budget regular upstream reconciliation.

A locally configured MCP subprocess can still leak an API key through careless diagnostics or child-process inheritance. The integration must redact authorization values, constrain the key's scopes, pass it only to the Weave MCP process, and verify that session logs and model-visible events contain no credential material.

The DSH default product assumptions may conflict with Workbench terminology or task flow. Branding changes alone are insufficient if tool results remain infrastructure-oriented, but invasive agent-loop changes would make upstream updates expensive. Workbench therefore accepts some generic DSH interaction until repeated user evidence justifies a plugin-level presentation change.

Keeping Codex and Workbench as clients increases contract pressure on Weave MCP. This cost is intentional: neither client may depend on hidden database states or client-specific fallbacks, and both must observe the same run and deliverable facts.
