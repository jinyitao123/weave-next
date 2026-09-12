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
- Workbench is the only supported business UI and owns capability management. Application backends may use the published-capability service API; keep all other MCP, HTTP, engines, storage and maintenance surfaces in support of Workbench or that service API. Do not reintroduce standalone business consoles, client setup, or business CLI commands.
- Keep Workbench dependencies inside `workbench/`. Use the pinned pnpm version and run `make workbench-install workbench-check` from the repository root.
- Do not restore standalone DeepSeek Harness branding, publishing workflows, or user-facing configuration. Compatibility names may remain only where the migration plan explicitly allows them.
- Run `make productguard`; runtime administration belongs in Workbench or the operator CLI.
- Do not add new product features during migration.
