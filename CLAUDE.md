# Weave New-Repository Notes

The old repository is frozen as the reference implementation. This repository is the migrated structure.

Use the four-band layout under `internal/`:

- `base` must stay independent of kernel, build, and app code.
- `kernel` may depend on base and kernel only.
- `build` may depend on base, kernel, and build only.
- `app` may assemble all bands.

Known migration baselines are documented and enforced by `scripts/depguard.sh`:

- `base/teamrun` may import `kernel/workflow`, `kernel/workflow/machine`, and `kernel/loomruntime`.

Validation commands:

```bash
go build ./...
go vet ./...
go test ./internal/... ./cmd/...
make depguard
```
