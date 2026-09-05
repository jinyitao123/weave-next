# Weave New-Repository Agent Guide

This repository is migrated in four bands:

- `internal/base`: runtime-independent collaboration base.
- `internal/kernel`: agent, runtime, workflow, MCP, and execution mechanisms.
- `internal/build`: team construction, evaluation, orchestration, and restore mechanisms.
- `internal/app`: product assembly, API, daemon, web UI embedding, and user-facing domains.
- `workbench`: the only supported business UI and its TypeScript runtime workspace.
- `weave-app`: the maintainer-only runtime management UI embedded in the Go binary.

Rules:

- Keep `module github.com/jinyitao123/weave`.
- Do not import upward across bands. Run `make depguard`.
- Use `make test`, which is fixed to `go test ./internal/... ./cmd/...`.
- Do not use `vendor/`; Go modules are the source of dependency resolution.
- Use `docker-compose.platform.yml` for platform validation.
- Workbench is the only supported business interface. Keep MCP, HTTP, engines, storage and maintenance only to support Workbench; do not reintroduce standalone business consoles, client setup, or business CLI commands.
- Keep Workbench dependencies inside `workbench/`. Use the pinned pnpm version and run `make workbench-install workbench-check` from the repository root.
- Keep the maintenance UI on its npm lockfile. Do not turn `weave-app` into a second business interface.
- Do not restore standalone DeepSeek Harness branding, publishing workflows, or user-facing configuration. Compatibility names may remain only where the migration plan explicitly allows them.
- Run `make productguard`; refresh the embedded runtime UI with `make ui-embed` after frontend changes.
- Do not add new product features during migration.
