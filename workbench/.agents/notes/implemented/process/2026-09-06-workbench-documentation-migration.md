# Agent Note: Workbench documentation checks follow the retained workspace

Status: implemented

English | [中文](2026-09-06-workbench-documentation-migration.zh.md)

## Problem

Documentation checks depend on package ownership, executable build targets, and the location of the workspace inside Git. References to deleted owners and assumptions that the workspace is the Git root prevent the retained documentation from being validated.

## Decision

The [gate registry](../../../../scripts/run-gates.ts) checks the retained Workbench engineering documentation. The retired public website has no build or projection-test leaf. Service maps, type-equivalence registrations, library classifications, and README exceptions name existing owners; their completeness and freshness checks remain active. Generated catalogs use the retained source declarations, and reviewed Chinese counterparts follow the same package inventory.

The [archive checker](../../../../scripts/verify-archived-agent-notes.ts) resolves the committed manifest relative to its workspace through Git's `ref:./path` syntax. The baseline comparison continues to reject removed or replaced seals, independently of current artifact hashes. Archived contents remain frozen.

## Alternatives considered

**Restoring the public website or removed packages** would revive entrypoints outside the Workbench product scope merely to satisfy obsolete registrations. The checks instead follow the retained owners.

**Ignoring missing files or disabling document checks** would hide future broken links, undocumented services, and changed archive seals. Only obsolete registrations are removed; each remaining check retains its failure conditions.

## Consequences

The documentation pipeline validates the current workspace without requiring retired product assets. Changes to package ownership still require explicit registry and bilingual documentation updates. The archive regression runs the real checker in a nested Git workspace, accepts a sealed baseline, and rejects removal of its committed seals.
