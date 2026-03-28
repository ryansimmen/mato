---
id: add-preflight-validation-command
priority: 26
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/doctor/
  - README.md
  - docs/architecture.md
  - docs/configuration.md
  - docs/task-format.md
---
# Add Queue-Only Preflight Validation to `mato doctor`

> **Status: Implemented** — This proposal has been fully implemented.
> The text below describes the implemented design; see the source code for the
> current behavior.

## Problem

Mato has a real usability gap: there is no clean, queue-focused preflight check
for authors, CI, or editor tooling.

Today the closest options are:

- `mato doctor`, which already checks queue layout, task parsing, invalid
  `affects` entries, and dependency integrity, but is framed as a broader host
  health command
- `mato --dry-run`, which is read-only but mixes validation with runtime-style
  output such as promotions, deferred tasks, and execution-order previews

That makes it awkward to answer the narrow question: "Are my task files and
queue metadata valid before I run mato?"

## Goal

Provide a first-class queue-only preflight validation workflow without adding a
new parallel validation subsystem.

## Non-goals

- no new standalone `internal/validate/` package
- no duplicate parsing or dependency logic outside existing queue/doctor code
- no Docker, Copilot, or host-tool checks in queue-only mode
- no `--fix` behavior beyond the existing `doctor` repair model
- no runtime preview sections like execution order or claim simulation

## Decision

Do not add a new `mato validate` implementation stack.

Instead, extend `mato doctor` so queue-only validation is a clear, documented,
first-class mode built from the existing `queue`, `tasks`, and `deps` checks.

Scope this proposal to the minimum useful change:

- fix the current coupling bug in queue-only `doctor` runs
- document `mato doctor --only queue,tasks,deps` as the recommended preflight
  workflow
- do not add `--queue-only` in this change
- do not add a `mato validate` alias in this change

Why this is the right shape:

- it solves the real UX problem without creating a third overlapping diagnostic
  surface beside `doctor` and `--dry-run`
- it keeps queue correctness tied to the same code paths `doctor` already uses
- it reduces drift risk in severity rules, JSON output, docs, and tests
- it makes future maintenance cheaper than introducing a separate package

## Intended behavior

Queue-only preflight validation should be available through `mato doctor`
without requiring unrelated host-health checks.

At minimum, the mode should cover:

- queue layout problems under `.mato/`
- frontmatter parse errors
- invalid `affects` glob syntax
- unsafe `affects` entries
- duplicate waiting task IDs
- ambiguous IDs across completed and non-completed states
- self-dependencies
- circular dependencies
- unknown dependency IDs

The results should stay aligned with current runtime and doctor semantics:

- parse failures: error
- invalid `affects` globs: error
- unsafe `affects` entries: error
- duplicate waiting IDs: error
- ambiguous IDs: warning
- self-dependencies: warning
- circular dependencies: warning
- unknown dependencies: warning

## Design

### 1. Make queue-only `doctor` usage explicit

Support and document a queue-only invocation built from existing checks, for
example:

```text
mato doctor --only queue,tasks,deps
```

This proposal should stop there. If the current `--only` UX later proves too
implicit, a convenience flag such as `--queue-only` can be proposed
separately, but it is out of scope here.

### 2. Remove unrelated coupling in queue-only mode

Queue-only validation should not fail just because unrelated doctor setup wants
to resolve Docker image configuration or other non-queue inputs.

In particular:

- queue-only runs should not require Docker checks
- queue-only runs should not depend on Docker-image resolution
- queue-only runs should avoid failing on irrelevant config paths when the
  requested checks do not use that data

This is the real missing capability today.

### 3. Reuse existing internals

The implementation should continue to rely on:

- queue layout checks in `internal/doctor/checks.go`
- `queue.BuildIndex()` for parsing and `affects` validation
- `queue.DiagnoseDependencies()` for dependency diagnostics

Do not introduce a second report pipeline that reclassifies the same findings.

### 4. Keep `--dry-run` separate

`mato --dry-run` should remain the answer to "what would one reconcile pass do
right now?" It is still useful, but it should not be the canonical interface
for validation consumers.

## Output

Queue-only doctor output should keep the existing doctor format and exit-code
rules:

- exit code `0`: no warnings or errors
- exit code `1`: warnings only
- exit code `2`: one or more errors

JSON consumers should continue using the doctor report schema rather than a new
validate-specific schema.

## Tests

Add or update tests for at least:

- `mato doctor --only queue,tasks,deps` on a clean queue
- warnings-only vs errors exit-code behavior
- parse failure, invalid `affects`, unsafe `affects`, duplicate ID, unknown
  dependency, self-dependency, and cycle cases
- queue-only mode avoiding unrelated Docker/config coupling
- JSON output for queue-only runs

If a thin `mato validate` alias is ever added later, it should only need
wiring tests, not a second copy of doctor/queue validation coverage.

## Documentation

Update:

- `README.md` to explain when to use queue-only `doctor` vs `--dry-run`
- `docs/configuration.md` to show the recommended preflight command
- `docs/architecture.md` to describe this as a `doctor` mode, not a new
  validation subsystem
- `docs/task-format.md` where helpful to mention that malformed task metadata
  is surfaced through queue-focused doctor checks

## Verification

Run:

```bash
go build ./... && go test -count=1 ./...
```
