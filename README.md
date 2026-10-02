# Weave

[English](README.md) | [中文](README.zh-CN.md)

**Weave is the team orchestration and execution service behind the GooeyPi desktop and Forge product.**

The supported employee and developer product path is the [GooeyPi desktop plus Forge](https://github.com/jinyitao123/weave-workbench). Weave owns teams, published workflow versions, scheduling, execution state, leases, budgets, cancellation, recovery, and usage. Forge owns employee identity, business records and actions, permissions, approvals, and final business state.

The web Workbench in `workbench/` is retired and is not a supported browser interface or standalone business client. Its code remains only as migration material until removal. Direct HTTP and MCP interfaces support the product integration and service operations; they do not create a second employee-facing business application.

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

For product setup, deployment boundaries, and current acceptance status, use the [product repository](https://github.com/jinyitao123/weave-workbench). The platform Compose file remains a development and validation aid; it does not make the retired web Workbench a supported client.

The [architecture index](docs/架构/README.md) is the current navigation for Weave's layering, contracts, and migration evidence. Dated acceptance records preserve the conclusions and limitations observed at the time; they do not certify the current working tree or deployment.

## License

This repository is licensed under [MIT](LICENSE). `workbench/` retains its original MIT license and third-party notices for the original and derived code in that directory.
