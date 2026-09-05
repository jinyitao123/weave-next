# Product boundary check

`scripts/productguard.sh` rejects restored business UI source paths, retired
backend implementations, the six retired product documents, and standalone
client registration in bootstrap.
It scans the working tree, including untracked files; an invalid or incomplete
repository root fails the check. Codex execution engines and developer tooling
remain valid.

`python3 -m unittest discover -s tools/tests -p test_product_boundary.py -v`
checks the guard by recreating retired artifacts in temporary repositories.
The registered HTTP surface and authentication are covered by
`internal/app/api/workbench_boundary_test.go` through the real Echo router.

This is an explicit regression boundary, not semantic classification of every
future file or API. New product surfaces still require review against the
Workbench product contract.
