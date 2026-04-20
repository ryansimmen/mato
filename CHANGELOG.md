# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the project is pre-`v1`, breaking changes may occur in any release.

## [Unreleased]

### Added

- Native Go fuzz harnesses for `internal/taskfile` covering
  `ParseBranchMarkerLine`, `ReplaceBranchMarkerLine`, `RemoveBranchMarkerLine`,
  `ParseClaimedBy`, `StripFailureMarkers`, and `SanitizeCommentText`. Run
  with `go test -run=^$ -fuzz=Fuzz... ./internal/taskfile/`.
- Native Go fuzz harnesses for `internal/frontmatter` covering
  `ParseTaskData`, `ExtractTitle`, `SanitizeBranchName`, and
  `ValidateAffectsGlobs`. Run with `go test -run=^$ -fuzz=Fuzz... ./internal/frontmatter/`.


- Scope `security-events: write` permission to the `analyze` job in
  `.github/workflows/codeql.yml` instead of granting it workflow-wide,
  following the principle of least privilege.
- Pin all GitHub Actions in `.github/workflows/` to commit SHAs with
  human-readable version comments. Dependabot continues to manage updates
  via the existing `github-actions` package ecosystem grouping.

### Fixed

### Removed

## [0.1.0] - 2026-04-20

Initial public alpha release.

### Added

- Filesystem-backed task queue with atomic claim, move, and squash-merge semantics.
- Autonomous Copilot coding agents running in per-task Docker sandboxes.
- AI-assisted review gate before changes merge into the target branch.
- Inter-agent messaging protocol for coordinating dependent work.
- Dependency DAG analysis with cycle detection (Kahn + Tarjan).
- Repository-local `.mato.yaml` configuration with source attribution.
- `mato status`, `mato log`, `mato inspect`, and `mato doctor` commands.
- Durable `.paused` sentinel for safely halting the queue.
- Lockfile-based agent identity and orphan recovery.
- Embedded task-instructions prompt with integration-test validation.
- `skills/mato/SKILL.md` task planner skill installable via `gh skill`.

### Security

- CodeQL advanced scanning with `security-extended` query suite.
- govulncheck workflow on push and daily schedule.
- OpenSSF Scorecard workflow with SARIF upload.
- Secret scanning, push protection, and private vulnerability reporting enabled.
- MIT licensed; see [SECURITY.md](SECURITY.md) for the disclosure process.

[Unreleased]: https://github.com/ryansimmen/mato/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ryansimmen/mato/releases/tag/v0.1.0
