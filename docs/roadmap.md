# Roadmap

This roadmap is directional, not a delivery commitment. Items should be concrete enough to verify when completed.

## Direction

- Keep task selection, branch assignment, review handoff, merge, and recovery host-owned.
- Keep the filesystem-backed queue; do not add a required service, daemon, or database.
- Support Docker and native sandbox runtimes behind one execution model.
- Treat agents as untrusted contributors; work lands on task branches before review and merge.

## Runtime Portability

- Add an agent runtime interface for launch, cancellation, output forwarding, and error reporting.
- Add runtime backend selection for `docker` and future `native-sandbox` modes.
- Add `mato doctor` checks for runtime tools, platform support, and sandbox availability.
- Publish a Docker-backed agent image with Linux-native `copilot`, `git`, `gh`, Go, and `gopls` dependencies.
- Prototype a macOS Seatbelt-based native sandbox backend.
- Decide macOS sandbox viability after testing roots, auth/cache access, networking, timeout cleanup, paths, symlinks, and case-insensitive filesystems.
- Evaluate Linux and Windows native sandbox backends after Docker and macOS share a stable runtime policy model.
- Keep Docker supported for reproducible Linux container execution.

## Release And Install

- Publish release artifacts with checksums, provenance, and verification instructions.
- Document install paths, upgrades, and uninstall steps.
- Add macOS and Windows CLI builds for commands that do not require agent execution.
- Decide the next packaging channel after signed binary releases are stable.
- Keep the bundled `mato` skill workflow aligned with `gh skill`.

## Queue Reliability

- Expand regression coverage for parsing, dependencies, overlap deferral, claiming, review, merge, runtime state, and backend selection.
- Test interrupted and timed-out agent runs across requeue, quarantine, and safe handoff paths.
- Keep `status`, `list`, `inspect`, `doctor`, and failure markers actionable.
- Preserve atomic queue moves and branch-marker validation across runtime backends.

## Operator Experience

- Explain stuck runs using queue state, locks, runtime sidecars, and branch markers.
- Document troubleshooting for Docker, native sandboxing, GitHub CLI, Copilot CLI, auth, queue corruption, and recovery markers.
- Surface auth-source diagnostics without printing secret values.
- Improve daemon progress and completion reporting without making `.mato/` messages authoritative.

## Task Authoring

- Improve the `mato` skill so generated tasks are small, scoped, dependency-aware, and schedulable.
- Keep frontmatter validation strict for unsafe paths, invalid globs, unknown keys, and ambiguous dependencies.
- Document practical examples for `depends_on`, `affects`, `priority`, and `max_retries`.

## Security Boundaries

- Keep credentials explicit, test-covered, and out of repo config and task files.
- Validate paths and branch names in host code, not prompts or sandbox policy alone.
- Test writable, read-only, and denied paths for each runtime backend.
- Treat Docker and native sandboxing as risk reduction, not proof of safety.

## Non-Goals

- `mato` will not replace human review, repository policy, or release ownership.
- `mato` will not make agents trusted actors on the target branch.
- `mato` will not require a hosted control plane, service, daemon, or database.
- Native sandbox work does not remove Docker support.
- macOS and Windows CLI builds do not imply full agent-runtime parity.
- Non-Linux agent runtimes must preserve host-owned lifecycle, review, merge, and recovery semantics.

## Contribution Areas

- Bug fixes with regression tests.
- Runtime abstraction, sandbox policy, macOS portability, and Docker image packaging.
- Queue, runner, review, merge, parser, backend, and recovery tests.
- Setup, troubleshooting, task authoring, and platform-limit docs.
- Release, install, checksum, signature, and provenance improvements.
- Diagnostics that clarify operator action without changing queue authority.
