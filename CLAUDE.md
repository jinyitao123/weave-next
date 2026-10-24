# Weave Repository Notes

Read [AGENTS.md](AGENTS.md) for the authoritative repository instructions and
[docs/架构/README.md](docs/架构/README.md) for current architecture entry points.
This file does not grant additional dependency exceptions.

The current layout has three bands: `internal/base`, `internal/kernel`, and
`internal/app`. Dependencies go downward. Execution and orchestration belong
to the kernel; the base has no execution behavior or forwarding aliases.
Before changing execution, revision context, or verification, follow the
responsibility and validation requirements linked from AGENTS.md.

`weave-next` is the source trunk. The supported business client is GooeyPi
desktop plus Forge; the browser interface is the operator/developer admin
console in `web/admin`. Keep retired product paths retired.

Run `make test depguard base-depguard productguard`. Console changes also run
`make admin-check admin-build`; platform validation uses
`docker-compose.platform.yml`. Report engineering checks and normal-path
product acceptance separately.
