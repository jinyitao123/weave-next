# Agent Note: Workbench task-first entry

Status: implemented

English | [中文](2026-09-04-workbench-task-first-entry.zh.md)

## Problem

An empty Workbench required a user to choose a filesystem workspace before the first request, even when the request needed only an existing Weave team. The same composer exposed DSH agent presets as if they were business execution modes. That mixed two host concerns into the primary task path and created a correctness hazard: the complete minimal preset can replace the assembled system prompt and suppress the mandatory Workbench team-routing policy.

## Decision

Workbench does not synthesize a private default Workspace. When no project has been selected, the composer remains unavailable and presents an explicit project picker. A chosen directory is the visible boundary for that project's team work and material output; existing registered projects remain available.

The Workbench profile disables the agent-preset selector and retains the standard full-capability preset as a host-owned choice. It also disables the inherited native DeepSeek adapter and replaces the official DeepSeek default selection with an inert provider-neutral fallback. A saved Models choice is authoritative; a clean deployment must configure one deliberately. Generic DSH profiles keep their existing workspace, preset, and native provider behavior. A real CLI profile-composition test owns these Workbench settings, while client tests own the Workbench-specific project-selection and composer language.

## Alternatives considered

**Create an invisible fallback Workspace.** Rejected: it hides where team output belongs and turns a product-owned scratch directory into an accidental project.

**Remove workspaces from Workbench.** Rejected: project files, source material, shell execution, and session grouping still need an explicit filesystem boundary. The workspace is optional context, not obsolete infrastructure.

**Keep the preset selector and merely default it to standard.** Rejected: it continues to present prompt composition as a business choice, and selecting a complete preset can invalidate Workbench's routing contract.

**Fork or delete the DSH preset system.** Rejected: other DSH products legitimately use it. A profile-level surface decision is smaller and preserves upstream behavior.

**Replace DeepSeek with another hard-coded vendor default.** Rejected: it would repeat the same vendor coupling under a different name and make a private deployment route part of the product contract.

## Consequences

A first-time Workbench user chooses a project before starting a Session. The host creates no hidden directory and keeps no environment-level fallback path. Workbench has one predictable foreground-agent contract and no vendor-owned model default, while DSH remains configurable elsewhere.
