# Weave New-Repository Agent Guide

This repository is migrated in four bands:

- `internal/base`: runtime-independent collaboration base.
- `internal/kernel`: agent, runtime, workflow, MCP, and execution mechanisms.
- `internal/build`: team construction, evaluation, orchestration, and restore mechanisms.
- `internal/app`: product assembly, API, daemon, and Workbench-facing domains.
- `workbench`: the only supported business UI and its TypeScript runtime workspace.

Rules:

- Keep `module github.com/jinyitao123/weave`.
- Do not import upward across bands. Run `make depguard`.
- Use `make test`, which is fixed to `go test ./internal/... ./cmd/...`.
- Do not use `vendor/`; Go modules are the source of dependency resolution.
- Use `docker-compose.platform.yml` for platform validation.
- Workbench is the only supported business interface. Keep MCP, HTTP, engines, storage and maintenance only to support Workbench; do not reintroduce standalone business consoles, client setup, or business CLI commands.
- Keep Workbench dependencies inside `workbench/`. Use the pinned pnpm version and run `make workbench-install workbench-check` from the repository root.
- Do not restore standalone DeepSeek Harness branding, publishing workflows, or user-facing configuration. Compatibility names may remain only where the migration plan explicitly allows them.
- Run `make productguard`; runtime administration belongs in Workbench or the operator CLI.
- Do not add new product features during migration.
- For Workbench product acceptance, follow [`docs/验收/Workbench真人式验收协议.md`](docs/验收/Workbench真人式验收协议.md). Use the real browser and the product's normal user path before inspecting APIs or fixtures. Test the same task as a user, a task owner, a deliverable consumer, and an independent auditor. Keep deterministic tasks direct; add method discovery only for a concrete unknown that blocks route selection. Report observed behavior, evidence, defects, and unverified gaps separately; a model claim, completed stage, tool call, or health response is not delivery acceptance.
