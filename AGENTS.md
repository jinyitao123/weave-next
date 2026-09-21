# Weave New-Repository Agent Guide

This repository is migrated in four bands:

- `internal/base`: runtime-independent collaboration base.
- `internal/kernel`: agent, runtime, workflow, MCP, and execution mechanisms.
- `internal/build`: team construction, evaluation, orchestration, and restore mechanisms.
- `internal/app`: product assembly, API, daemon, and Workbench-facing domains.
- `workbench`: the only supported business UI and its TypeScript runtime workspace.

Rules:

- Keep `module github.com/jinyitao123/weave`.
- Do not import upward across bands. Run `make depguard base-depguard`. Capability definition, compilation, and execution semantics belong to `internal/kernel/capability`; engine binding belongs to `internal/kernel/capabilityruntime`. Do not restore execution behavior or forwarding aliases under `internal/base`.
- Before changing execution, orchestration, revision context, or delivery verification, follow [`docs/架构/2026-09-13-分层收敛与Loom职责基线.md`](docs/架构/2026-09-13-分层收敛与Loom职责基线.md). Identify the responsibility layer, existing state owner, reused entry point, public contract changes, and validation before editing. Keep task-specific rules in definitions or acceptance cases; do not add a parallel queue, execution loop, or terminal-state authority to solve one scenario. Do not expand dependency exceptions; remove each exact exception when its coupling is eliminated. Existing debt is not permission for new coupling.
- For this migration, converge directly on the new contracts. Do not add legacy-data readers, snapshot conversion, dual execution paths, or old Host compatibility. Keep publication immutability, identity isolation, idempotency, and cancellation as new-system invariants. Historical evidence remains traceable; do not erase unrelated local data.
- Start architecture work from `docs/架构/README.md`. Documents under `docs/历史/` and historical redirect pages are evidence, not current implementation instructions. Preserve source and replacement links when consolidating designs; do not treat archived single-repository decisions as overriding the current future repository split.
- Use `make test`, which is fixed to `go test ./internal/... ./cmd/...`.
- Do not use `vendor/`; Go modules are the source of dependency resolution.
- Use `docker-compose.platform.yml` for platform validation.
- Workbench is the only supported business interface. Keep MCP, HTTP, engines, storage and maintenance only to support Workbench; do not reintroduce standalone business consoles, client setup, or business CLI commands.
- Keep Workbench dependencies inside `workbench/`. Use the pinned pnpm version and run `make workbench-install workbench-check` from the repository root.
- Do not restore standalone DeepSeek Harness branding, publishing workflows, or user-facing configuration. Compatibility names may remain only where the migration plan explicitly allows them.
- Run `make productguard`; runtime administration belongs in Workbench or the operator CLI.
- Do not add new product features during migration.
- For Workbench product acceptance, follow [`docs/验收/Workbench真人式验收协议.md`](docs/验收/Workbench真人式验收协议.md). Use the real browser and the product's normal user path before inspecting APIs or fixtures. Test the same task as a user, a task owner, a deliverable consumer, and an independent auditor. Keep deterministic tasks direct; add method discovery only for a concrete unknown that blocks route selection. Report observed behavior, evidence, defects, and unverified gaps separately; a model claim, completed stage, tool call, or health response is not delivery acceptance.

Approved 2026-09-10 scoped exception: the Guandan demonstration may admit
server-to-server candidate-selection decisions through a dedicated service key
bound to one published Team/Workflow version. Reuse the existing workflow engine,
run ledger and delivery projection. This does not open arbitrary team dispatch
or Workbench user-event impersonation to service clients.
