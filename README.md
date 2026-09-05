# Weave

[English](README.md) | [中文](README.zh-CN.md)

**The execution backend for [Weave Workbench](https://github.com/jinyitao123/weave-workbench).**

Users choose a team, confirm the task, follow progress, resolve problems, and collect results in Workbench. Weave saves the inputs and workflow version, coordinates execution, records waiting and recovery, and stores the resulting work.

Workbench is the only supported business interface. This repository's embedded UI serves maintainers who manage runtimes. The `weave` command supports service deployment, runtime operation, and the MCP connection used by Workbench. Standalone business CLI commands and Codex/Claude client setup are retired. Codex, Claude, and other supported execution engines remain runtime implementation choices.

The current scope is an invited, privately deployed pilot with maintainer support. See the [current product contract](docs/架构/2026-09-05-Weave-当前产品合同.md), [Workbench interaction plan](docs/架构/2026-09-05-Workbench-工作对话方案.md), and [acceptance checklist](docs/验收/2026-09-05-Workbench统一验收清单.md). Historical releases and tests do not certify the current working tree.

## Responsibilities

| Component | Responsibility |
|---|---|
| Workbench | Work conversation, team selection, task confirmation, progress, human decisions, recovery actions, and reading results |
| Weave API and execution services | Durable tasks, frozen workflows, queueing, execution state, permissions, recovery, usage, and saved deliverables |
| Runtime | Execute assigned work on a configured machine and return available outputs and activity |
| Runtime management UI | Maintainer login, runtime registration, connection, capacity, and local runtime controls |

Workbench's host connects to the Weave HTTP API and starts `weave mcp serve` as an internal bridge. The browser uses Workbench; service credentials remain on the host. The runtime management page is a maintenance surface, not another business client.

Execution ending does not establish that a result is usable. Saved outputs must belong to the correct run and remain readable. Missing files, incomplete activity, unavailable usage, and waiting reasons must be reported as such. Recovery should retain saved work; a stop request is distinct from confirmed termination.

## Maintainer setup

Use PostgreSQL 16 and the Go version declared in [go.mod](go.mod). Workbench has its own Node.js/pnpm dependencies and release lifecycle. Record and validate the two source versions together.

### Start Weave locally

Prepare a private PostgreSQL database. Generate the server secrets once and keep them stable across restarts:

```sh
export DATABASE_URL='postgres://weave:<database-password>@127.0.0.1:5432/weave?sslmode=disable'
export JWT_SECRET="$(openssl rand -hex 32)"
export WEAVE_SECRET_KEY="$(openssl rand -hex 32)"
export WEAVE_API_URL='http://127.0.0.1:8080'

go build -o ./bin/weave ./cmd/weave
umask 077
./bin/weave bootstrap > bootstrap.json
export WEAVE_API_KEY="$(jq -r '.api_key // empty' bootstrap.json)"
./bin/weave serve
```

Bootstrap creates an administrator and an owner-bound API key on first use. Repeating it retains the account and key; the raw key is only returned when created. On subsequent starts use the key already held in your secret store, rather than replacing it with an empty bootstrap field. Keep the bootstrap output private and remove the working copy after storing its credentials securely.

Set exactly one of `WEAVE_SECRET_KEY` and `WEAVE_SECRET_KEY_FILE`. The key is 32 bytes encoded as 64 hexadecimal characters or standard base64; the file setting points to a regular file containing that value. Preserve it with the database backup. Neither setting nor `DATABASE_URL` belongs in Workbench or the MCP child process.

Confirm both service endpoints respond successfully:

```sh
curl --fail http://127.0.0.1:8080/v1/health
curl --fail http://127.0.0.1:8080/v1/ready
```

### Connect Workbench

In the separately checked-out Workbench repository:

```sh
pnpm install --frozen-lockfile
pnpm build:workbench
WEAVE_COMMAND='/absolute/path/to/weave-next/bin/weave' \
WEAVE_API_URL='http://127.0.0.1:8080' \
WEAVE_API_KEY='<owner-bound-api-key>' \
pnpm workbench --host 127.0.0.1 --port 3080 --no-open
```

Use Workbench's local sign-in flow at `http://127.0.0.1:3080/`. `WEAVE_COMMAND` selects the matching Weave binary; Workbench starts its MCP process. A usable team and configured execution environment are also needed before dispatch. Opening the page or passing health checks alone does not prove a business task can complete.

### Platform containers

Use [docker-compose.platform.yml](docker-compose.platform.yml) for platform deployment and validation:

```sh
cp .env.example .env
# Set actual generated JWT_SECRET, WEAVE_SECRET_KEY, WEAVE_ADMIN_PASS,
# and a private POSTGRES_PASSWORD. Do not put shell expressions in .env.
scripts/refresh-weave.sh
docker compose -f docker-compose.platform.yml ps
```

This starts Weave and its database; Workbench is installed separately. Weave exposes port 8080 for API and runtime maintenance. PostgreSQL is not published to the host. The refresh script updates the main checkout's existing platform stack, including when invoked from a Git worktree. For an isolated test, use an explicit Compose project, private environment, separate volumes, and port overrides instead.

The optional `runtime` profile retains `/data/runtime-workspaces` in the `runtime_data` named volume, including the `.weave-public-events` spool. Recreating the runtime container preserves those files; removing its volume deletes them.

The maintainer login uses `WEAVE_ADMIN_USER` (default `admin`) and the configured password; there is no default password. Register a runtime and store its token before enabling the optional `runtime` profile. Configure only the model engines required by that deployment.

## Architecture and boundaries

| Directory | Role |
|---|---|
| `internal/base` | Runtime-independent collaboration state, task queue, execution records, and persistence |
| `internal/kernel` | Agents, workflows, execution engines, runtimes, and MCP mechanisms |
| `internal/build` | Team construction, compilation, evaluation, and restore mechanisms |
| `internal/app` | Workbench-facing API, MCP bridge, daemon, and product assembly |
| `weave-app` | Embedded runtime maintenance UI and local runtime controls |
| `cmd/weave` | Service and maintenance entry point |

Dependencies must not import upward across the four bands. The Go module remains `github.com/jinyitao123/weave`; Go modules, rather than `vendor/`, resolve dependencies. [Loom](https://github.com/jinyitao123/loom) provides graph execution; freezing a graph does not guarantee identical model responses or external effects.

MCP calls explicitly declared in `write_tools` are rejected by the write gate. Undeclared tools are not automatically rejected by name, and CLI engines run with their runtime host's permissions. This requires a trusted deployment environment; it is not a universal sandbox.

Runtime collection accepts supported UTF-8 files attributable to the current execution, with a 256 KiB per-file and 512 KiB aggregate limit. A promised but unsaved file is a delivery failure, not a valid final receipt. Preview limits are separate from collection limits; complete downloads can only return content actually saved by Weave. Unknown usage is not zero cost.

## Validation and records

```sh
make ci
npm --prefix weave-app ci
make ui-embed
make compose-check
# Use an isolated database, not a business database:
TEST_DATABASE_URL='<isolated-postgresql-url>' make test-integration
```

`make test` covers `./internal/... ./cmd/...`; integration coverage requires PostgreSQL. Container health, engineering checks, actual Workbench use, and the [20-task pilot ledger](docs/验收/2026-09-05-20项真实任务试点台账.md) are separate evidence. Record exact source versions and results without promoting an unrun item to a pass.

Before an upgrade, back up PostgreSQL, the stable credential key, and Workbench's `DSH_HOME`. Database migrations move forward; reverting code alone does not undo them. Test restoration in an isolated environment.

The [architecture index](docs/架构/README.md) indexes current contracts and the execution and migration references still supporting Workbench. The [August 31 release baseline](docs/验收/2026-08-31-Weave-Workbench-v0.1-设计伙伴版发布基线.md) retains its exact historical version pair and acceptance evidence; it does not describe the current source version.

## License

Apache-2.0
