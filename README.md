# Weave

[English](README.md) | [中文](README.zh-CN.md)

**Weave is the team orchestration and execution service behind the GooeyPi desktop and Forge product.**

The supported employee and developer product path is the [GooeyPi desktop plus Forge](https://github.com/jinyitao123/weave-workbench). Weave owns teams, published workflow versions, scheduling, execution state, leases, budgets, cancellation, recovery, and usage. Forge owns employee identity, business records and actions, permissions, approvals, and final business state.

The retired web Workbench has been removed. Direct HTTP and MCP interfaces support the product integration and service operations; they do not create a second employee-facing business application. The server binary also serves the admin console at `/admin` for developers and operators to manage runtimes, environments, teams and tasks; its source is in [web/admin](web/admin/README.md).

## Responsibilities

| Component | Responsibility |
|---|---|
| Weave | Team and member definitions, frozen workflows, scheduling, execution, leases, budgets, cancellation, recovery, usage, and runtime integration |
| Forge | Employee identity, business objects and actions, access control, approvals, and authoritative business results |
| GooeyPi desktop plus Forge | The supported employee and developer product experience; business results are read back from Forge |

Execution ending does not establish that a result is usable. Saved outputs must belong to the correct run and remain readable. Missing files, incomplete activity, unavailable usage, and waiting reasons must be reported as such. Recovery must retain saved work; a stop request is distinct from confirmed termination.

## Develop and validate

Use the Go version declared in [go.mod](go.mod). The primary checks are:

```sh
make test
make depguard base-depguard
make productguard
make compose-check
```

`make test` covers `./internal/... ./cmd/...`; database integration checks require PostgreSQL. Container health and engineering checks establish component behavior only. Product acceptance starts from the desktop's normal path and reads the business result back independently in Forge.

## Service operations

Build the operator binary with `go build -o ./bin/weave ./cmd/weave`. The `weave bootstrap` command provisions service operator credentials; keep its output in a private secret store. Weave does not provide local user-password login or first-user registration. Employees and developers authenticate through Forge as part of the supported product flow.

For product setup, deployment boundaries, and current acceptance status, use the [product repository](https://github.com/jinyitao123/weave-workbench). The platform Compose file remains a development and validation aid.

The default Docker build and the Compose `weave` service use the `server` target. It contains the Go service, Node for the health probe, and the six-platform Runtime Host downloads; it does not install Codex, Claude Code, or OpenCode. Loom can execute in the service using a configured model provider. CLI members, including CLI-backed inference for providerless Loom, require a registered execution node with the corresponding engine and authentication ready.

Install the required CLI engines on execution nodes, register each node through runtime maintenance, and start the existing `weave runtime` command with its node token. The `/install.sh` and `/install.ps1` helpers install the Runtime Host; the Host reports engines visible in its execution environment. For container-based nodes, explicitly build `docker build --target executor -t weave-executor .`; the optional Compose `runtime` profile builds that target under the separate `WEAVE_RUNTIME_IMAGE` tag (default `weave-executor`). `WEAVE_PLATFORM_IMAGE` continues to name only the server image.

`WEAVE_LOCAL_RUNTIME_ENABLED` defaults to `false`, and the server Compose service keeps it disabled. Operators who deliberately assemble a co-located Host can still opt in with `true` in their own deployment, which must supply its CLI engines. This option does not change a published workflow's frozen node binding or replace a missing external node.

The [architecture index](docs/架构/README.md) is the current navigation for Weave's layering, contracts, and migration evidence. Dated acceptance records preserve the conclusions and limitations observed at the time; they do not certify the current working tree or deployment.

## License

This repository is licensed under [MIT](LICENSE).
