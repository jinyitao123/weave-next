# Weave New-Repository Agent Guide

This repository is migrated in four bands:

- `internal/base`: runtime-independent collaboration base.
- `internal/kernel`: agent, runtime, workflow, MCP, and execution mechanisms.
- `internal/build`: team construction, evaluation, orchestration, and restore mechanisms.
- `internal/app`: product assembly, API, daemon, and client-facing domains.
- `workbench`: the retired web Workbench. It stays only until removal; make no new changes there.

Weave is an agent-team platform. Its only supported business client is the GooeyPi desktop plus Forge product in `weave-workbench`; business facts belong to Forge. Client contracts live in `weave-workbench/contracts/v1`, and Weave owns compatibility and regression for the parts it implements. Keep: team and member definitions, workflow orchestration, publication freezing, scheduling, leases, budgets, cancellation, recovery, usage, and multi-runtime members (Loom, Codex, Claude, OpenCode). Retired: team templates, team-build runs and evaluation (including judges), the meta-team, and the web Workbench. Decision source: `weave-workbench/docs/decisions/001-Weave平台范围收窄与唯一客户端.md`.

Rules:

- Keep `module github.com/jinyitao123/weave`.
- Do not import upward across bands. Run `make depguard base-depguard`. Capability definition, compilation, and execution semantics belong to `internal/kernel/capability`; engine binding belongs to `internal/kernel/capabilityruntime`. Do not restore execution behavior or forwarding aliases under `internal/base`.
- Before changing execution, orchestration, revision context, or delivery verification, follow [`docs/架构/2026-09-13-分层收敛与Loom职责基线.md`](docs/架构/2026-09-13-分层收敛与Loom职责基线.md). Identify the responsibility layer, existing state owner, reused entry point, public contract changes, and validation before editing. Keep task-specific rules in definitions or acceptance cases; do not add a parallel queue, execution loop, or terminal-state authority to solve one scenario. Do not expand dependency exceptions; remove each exact exception when its coupling is eliminated. Existing debt is not permission for new coupling.
- For this migration, converge directly on the new contracts. Do not add legacy-data readers, snapshot conversion, dual execution paths, or old Host compatibility. Keep publication immutability, identity isolation, idempotency, and cancellation as new-system invariants. Historical evidence remains traceable; do not erase unrelated local data.
- Start architecture work from `docs/架构/README.md`. Documents under `docs/历史/` and historical redirect pages are evidence, not current implementation instructions. Preserve source and replacement links when consolidating designs; do not treat archived single-repository decisions as overriding the current future repository split.
- Use `make test`, which is fixed to `go test ./internal/... ./cmd/...`.
- Do not use `vendor/`; Go modules are the source of dependency resolution.
- Use `docker-compose.platform.yml` for platform validation.
- The GooeyPi desktop plus Forge is the only supported business interface. Keep HTTP, MCP, engines, storage and maintenance only to support it; do not reintroduce standalone business consoles, client setup, or business CLI commands. Employees and developers log in only through Forge identity exchange; keep Weave local password login and first-user registration disabled. Operator API keys come from `weave bootstrap` and other operator entry points.
- Retire the capabilities listed above by first switching them off in deployment (`WEAVE_METATEAM_ENABLED=false`, no web Workbench or gateway containers, no routes registered), then deleting code after one release with no callers. Before deleting `internal/app/teamconstruction`, move the product publication code that workflow publishing uses out of it, and remove the agent API's references to meta-team member names. Until removal, `workbench/` dependencies stay inside `workbench/` with the pinned pnpm version.
- Do not restore standalone DeepSeek Harness branding, publishing workflows, or user-facing configuration. Compatibility names may remain only where the migration plan explicitly allows them.
- Run `make productguard`; runtime administration belongs in the operator CLI.
- `weave-next` is the only Weave source trunk. The split-repository migration (G3/G7) is paused until the desktop product's MVP1 acceptance; independent repositories take no feature changes meanwhile. They will be re-cut from this trunk afterwards: no separate web Workbench repository, and `weave-builder` re-scoped to the kept orchestration and recovery strategies. New features must serve the supported client and keep the layering rules above (depguard, no parallel queue, execution loop or terminal-state authority).
- Durable recovery is a core platform capability. The model/tool replay contract belongs to Loom (`stdlib.ExecutionJournal`); Weave supplies storage, leases and fencing. Add provider-specific recovery regressions to Loom instead of patching around them in Weave.
- Product acceptance follows `weave-workbench`'s acceptance rules: start from the GooeyPi desktop's normal path and read back business results independently in Forge pages. The web Workbench protocol in [`docs/验收/Workbench真人式验收协议.md`](docs/验收/Workbench真人式验收协议.md) is historical; its principles still apply. Use the product's normal user path before inspecting APIs or fixtures. Test the same task as a user, a task owner, a deliverable consumer, and an independent auditor. Keep deterministic tasks direct; add method discovery only for a concrete unknown that blocks route selection. Report observed behavior, evidence, defects, and unverified gaps separately; a model claim, completed stage, tool call, or health response is not delivery acceptance.

Approved 2026-09-10 scoped exception: the Guandan demonstration may admit
server-to-server candidate-selection decisions through a dedicated service key
bound to one published Team/Workflow version. Reuse the existing workflow engine,
run ledger and delivery projection. This does not open arbitrary team dispatch
or Workbench user-event impersonation to service clients.
