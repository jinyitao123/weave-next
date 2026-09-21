# Weave

[English](README.md) | [中文](README.zh-CN.md)

**The complete Weave Workbench product repository.**

Users choose a team, confirm the task, follow progress, resolve problems, and collect results in Workbench. Weave saves the inputs and workflow version, coordinates execution, records waiting and recovery, and stores the resulting work.

Workbench is the only supported browser interface. Runtime onboarding and task-relevant status live in Workbench; service diagnostics and machine process operations use the `weave` command, the host service manager, or Compose. Standalone business CLI commands and Codex/Claude client setup are retired. Codex, Claude, and other supported execution engines remain runtime implementation choices.

The current scope is an invited, privately deployed pilot with maintainer support. See the [current product contract](docs/架构/2026-09-05-Weave-当前产品合同.md), [Workbench interaction plan](docs/架构/2026-09-05-Workbench-工作对话方案.md), and [acceptance checklist](docs/验收/2026-09-05-Workbench统一验收清单.md). Historical releases and tests do not certify the current working tree.

## Quick start

Install Docker Desktop or Docker Engine with Compose, then run this from a repository checkout on macOS, Linux, or WSL2:

```sh
./scripts/install-weave.sh
```

The installer creates a private `.env`, starts PostgreSQL and Weave, creates the Workbench credential, registers a local runtime, starts Workbench, and prints the authenticated browser URL. It reuses the same configuration and data when run again. Tagged releases pull matching amd64 or arm64 images when available; a source checkout builds the same images locally when they are not published.

After opening the URL, add one model provider and create or import a team. Workbench then shows whether the service, credential, team, and runtime are ready before the first dispatch.

```sh
docker compose -f docker-compose.platform.yml ps
docker compose -f docker-compose.platform.yml logs -f workbench runtime
docker compose -f docker-compose.platform.yml stop
```

If installation stops, inspect the failing service with `docker compose -f docker-compose.platform.yml logs weave workbench runtime`. Port conflicts can be resolved by setting `WEAVE_API_PORT` or `WORKBENCH_PORT` before the first run. If `.env` already exists, edit those values there. Keep `.env`, `data/workbench`, and `data/workspaces` when upgrading; removing them discards credentials or local Workbench data.

## Responsibilities

| Component | Responsibility |
|---|---|
| Workbench | Work conversation, team selection, task confirmation, progress, human decisions, recovery actions, and reading results |
| Weave API and execution services | Durable tasks, frozen workflows, queueing, execution state, permissions, recovery, usage, and saved deliverables |
| Runtime | Execute assigned work on a configured machine and return available outputs and activity |
| Workbench settings | Runtime registration, connection health, capacity, naming, and revocation |

Workbench's host connects to the Weave HTTP API and starts `weave mcp serve` as an internal bridge. The browser uses only Workbench; service credentials remain on the host. Low-level diagnostics stay outside the browser.

Execution ending does not establish that a result is usable. Saved outputs must belong to the correct run and remain readable. Missing files, incomplete activity, unavailable usage, and waiting reasons must be reported as such. Recovery should retain saved work; a stop request is distinct from confirmed termination.

## Maintainer setup

Use PostgreSQL 16, the Go version declared in [go.mod](go.mod), Node.js 22.19 or newer from [.node-version](.node-version), and the pnpm version declared by `workbench/package.json`. The backend and Workbench are built and verified from one commit.

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

After setting `WEAVE_API_URL` and `WEAVE_API_KEY`, run `weave doctor` to inspect the service version, dependency readiness, and runtime registry in one command. Start and stop machine processes through Compose, systemd, or launchd so the browser never controls host processes directly.

### Connect Workbench

From this repository:

```sh
make workbench-install
make workbench-build
cd workbench
WEAVE_API_URL='http://127.0.0.1:8080' \
WEAVE_API_KEY='<owner-bound-api-key>' \
pnpm workbench --host 127.0.0.1 --port 3080 --no-open
```

Use Workbench's local sign-in flow at `http://127.0.0.1:3080/`. Workbench starts `weave mcp serve` from `PATH`; set `WEAVE_COMMAND` to an absolute executable path only when selecting another trusted binary. The launcher rejects an explicit missing path instead of opening a partially connected Workbench. A usable team and configured execution environment are also needed before dispatch. Opening the page or passing health checks alone does not prove a business task can complete.

For the shared product login, set `WEAVE_FORGE_SESSION_URL` to Forge's authenticated session endpoint and `WEAVE_FORGE_DEFAULT_WORKSPACE` to the initial Weave workspace. Workbench submits the account password only to Forge, then exchanges the returned Forge credential for a Weave session. The first successful exchange creates a stable Weave account binding. Team-development and platform-management access are derived from Forge's effective permission sets on every exchange; the local Weave user role is not an authority for Forge-bound sessions.

Workbench puts a complete Weave address in each new runtime connection command. Set `WEAVE_RUNTIME_SERVER_URL` to the public or otherwise routable Weave URL for a deployed service; a bare-host Workbench falls back from a loopback API URL to the preferred LAN IPv4 and keeps the configured API port.

### Platform containers

Use [docker-compose.platform.yml](docker-compose.platform.yml) for platform deployment and validation:

```sh
cp .env.example .env
# Set actual generated JWT_SECRET, WEAVE_SECRET_KEY, WEAVE_ADMIN_PASS,
# and a private POSTGRES_PASSWORD. Do not put shell expressions in .env.
./scripts/install-weave.sh
docker compose -f docker-compose.platform.yml ps
```

This starts the database, Weave, Workbench, and its gateway together; the optional runtime remains profile-controlled. Weave exposes port 8080 for the API only, Workbench defaults to 3080, and PostgreSQL is not published to the host. For an existing deployment, point `WORKBENCH_DATA_PATH` and `WORKBENCH_WORKSPACE_PATH` at the previous data locations. For an isolated test, use an explicit Compose project, private environment, separate volumes, and port overrides.

The optional `runtime` profile retains `/data/runtime-workspaces` in the `runtime_data` named volume, including the `.weave-public-events` spool. Recreating the runtime container preserves those files; removing its volume deletes them.

The maintainer login uses `WEAVE_ADMIN_USER` (default `admin`) and the configured password; there is no default password. Register a runtime and store its token before enabling the optional `runtime` profile. Configure only the model engines required by that deployment.

## Architecture and boundaries

| Directory | Role |
|---|---|
| `internal/base` | Runtime-independent collaboration state, task queue, execution records, and persistence |
| `internal/kernel` | Agents, workflows, execution engines, runtimes, and MCP mechanisms |
| `internal/build` | Team construction, compilation, evaluation, and restore mechanisms |
| `internal/app` | Workbench-facing API, MCP bridge, daemon, and product assembly |
| `workbench` | The only business interface and its TypeScript runtime foundation |
| `cmd/weave` | Service and maintenance entry point |

Dependencies must not import upward across the four bands. The Go module remains `github.com/jinyitao123/weave`; Go modules, rather than `vendor/`, resolve dependencies. [Loom](https://github.com/jinyitao123/loom) provides graph execution; freezing a graph does not guarantee identical model responses or external effects.

MCP calls explicitly declared in `write_tools` are rejected by the write gate. Undeclared tools are not automatically rejected by name, and CLI engines run with their runtime host's permissions. This requires a trusted deployment environment; it is not a universal sandbox.

Runtime collection accepts supported UTF-8 files attributable to the current execution, with a 256 KiB per-file and 1 MiB aggregate limit. A promised but unsaved file is a delivery failure, not a valid final receipt. Preview limits are separate from collection limits; complete downloads can only return content actually saved by Weave. Unknown usage is not zero cost.

## Validation and records

```sh
make ci
make workbench-install
make workbench-check
make compose-check
# Use an isolated database, not a business database:
TEST_DATABASE_URL='<isolated-postgresql-url>' make test-integration
```

`make test` covers `./internal/... ./cmd/...`; integration coverage requires PostgreSQL. Container health, engineering checks, actual Workbench use, and the [20-task pilot ledger](docs/验收/2026-09-05-20项真实任务试点台账.md) are separate evidence. Record exact source versions and results without promoting an unrun item to a pass.

Before an upgrade, back up PostgreSQL, the stable credential key, and the directory configured by `WORKBENCH_DATA_PATH`. Database migrations move forward; reverting code alone does not undo them. Test restoration in an isolated environment.

The [architecture index](docs/架构/README.md) indexes current contracts and the execution and migration references still supporting Workbench. The [August 31 release baseline](docs/验收/2026-08-31-Weave-Workbench-v0.1-设计伙伴版发布基线.md) retains its exact historical version pair and acceptance evidence; it does not describe the current source version.

## License

The root repository is MIT licensed. `workbench/` retains its original MIT license and third-party notices for the original and derived code in that directory.
