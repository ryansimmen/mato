# Security Assurance Case

This document summarizes why `mato` is designed to meet its security requirements and where residual risk remains. It complements [Architecture](architecture.md), [Task Format](task-format.md), [Messaging](messaging.md), and [SECURITY.md](../SECURITY.md).

## Security Requirements

`mato` is an operator tool for running autonomous coding agents against a local repository. The project aims to provide these security properties:

- Agents work in isolated temporary clones instead of the operator's checked-out target branch.
- Agents do not merge directly into the target branch.
- Completed work lands on task branches first and passes through a review gate before merge.
- Host-side queue transitions are filesystem-backed, inspectable, and recoverable.
- Task movement avoids destination overwrite races and unsafe cross-device behavior.
- Task metadata, branch names, path-like fields, message types, and configuration inputs are validated before use.
- GitHub credentials remain external to repository configuration and can be rotated without recompiling `mato`.
- Failure cases should fail closed by requeueing, quarantining, or moving tasks to `failed/` with diagnostic markers instead of silently proceeding with ambiguous state.

## Trust Boundaries

Important trust boundaries are:

- Host process to Docker container: the host owns task selection, branch assignment, review handoff, merge, and recovery; the container performs one task or review.
- Temporary clone to target repository: agent changes are pushed through task branches and inspected before squash merge.
- `.mato/` queue to source tree: queue files are operational state, not source code; `.mato/` is Git-ignored and inspected by host commands.
- Task metadata to host decisions: frontmatter controls priority, dependencies, affects metadata, and retry behavior, so it is parsed and validated before scheduling.
- Message files to queue transitions: inter-agent messages are advisory; queue file moves and Git state remain authoritative.
- Host credentials to container: GitHub/Copilot credentials are supplied from host tools or explicit environment variables, not from repository config files.

## Threat Model

The design considers these threats:

- A buggy or malicious agent produces unsafe code, broad edits, or incorrect commits.
- A task file contains malformed frontmatter, unsafe paths, invalid globs, ambiguous dependencies, or misleading runtime markers.
- Concurrent agents, reviews, or merges race while touching queue files or overlapping source paths.
- A task branch, review handoff, or merge record is missing, stale, corrupt, or ambiguous.
- Credential values are accidentally exposed in command output, Docker args, config files, or task files.
- A host process is interrupted while work is in progress.
- A Docker container or mounted tool behaves unexpectedly.

The model does not treat Docker as a perfect sandbox. Operators should only run `mato` in repositories and environments where autonomous code execution is acceptable.

## Design Arguments

### Agent Isolation

Agents run in short-lived Docker containers against temporary clones. The host controls the branch name, task metadata, Docker arguments, and task lifecycle. The target branch is not the agent's working branch, and agent output must be pushed to a task branch before the host can review or merge it.

### Review And Merge Control

The review phase is separate from the work phase. Review verdicts are handed to the host through a narrow JSON verdict file. The host writes queue markers and performs all authoritative task moves. Approved tasks are squash-merged serially under a merge lock, reducing concurrent writes to the target branch.

### Filesystem Queue Safety

Queue movement uses atomic link/remove semantics through `queue.AtomicMove` and cross-device fallback handling. Stale locks, orphaned in-progress tasks, pushed handoffs, missing branch markers, and failed review states are explicitly recovered, retried, or quarantined.

### Input Validation

Task frontmatter rejects unknown top-level keys. Branch markers, claimed-by metadata, failure markers, affects entries, message types, config files, and CLI flag values have validation coverage. Unsafe affects entries such as absolute paths or path traversal are stripped and reported by `mato doctor`.

### Messaging Scope

Messaging is intentionally advisory. Agents can use file-claim and event messages to coordinate, but host-owned queue state, branch markers, review verdicts, and Git state decide actual progress. Messaging read/write failures produce warnings or degraded behavior without granting authority to stale messages.

### Credential Handling

`mato` forwards `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN` into containers when explicitly set, or obtains a host `gh auth token` as a fallback. These tokens are not stored in `.mato.yaml`, task files, or source-controlled state. Docker argument construction avoids embedding secret values directly in logged command arguments.

### Verification

The project uses unit tests, integration tests, native Go fuzz targets, race tests, CodeQL, govulncheck, golangci-lint, staticcheck, and deadcode checks. Parser, sanitizer, messaging, taskfile, queue, review, merge, and runtime recovery behavior have targeted coverage.

## Common Weakness Countermeasures

- Path traversal: affects entries and runtime file paths are validated and sanitized before use.
- Command injection: agent prompts avoid shell-expanded command substitution, and host code uses direct argument arrays for external commands.
- Race conditions: queue moves use atomic file operations, lock files use PID/start-time identity, and merge/review paths use dedicated locks.
- Unsafe parsing: task frontmatter, message JSON, verdict JSON, branch markers, and runtime markers have unit and fuzz coverage.
- Credential exposure: auth values are forwarded as environment variables and tests assert sensitive values are not embedded into Docker args.
- Silent failure: warnings and diagnostic markers are preserved for doctor/status/inspect/log surfaces where practical.

## Residual Risks

- Autonomous agents can still produce incorrect or insecure code that passes automated and AI review. Human oversight remains necessary.
- Docker isolation depends on the host Docker daemon and mounted paths; it is not a hardened multi-tenant sandbox.
- Forwarded GitHub/Copilot credentials are available inside the container for the duration of the agent run.
- AI review is not a substitute for project policy, maintainer judgment, or external security review.
- Pre-`v1` behavior may change as the project hardens runtime, packaging, and release workflows.
