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

Approved 2026-09-10 scoped exception: the Guandan demonstration may admit
server-to-server candidate-selection decisions through a dedicated service key
bound to one published Team/Workflow version. Reuse the existing workflow engine,
run ledger and delivery projection. This does not open arbitrary team dispatch
or Workbench user-event impersonation to service clients.
