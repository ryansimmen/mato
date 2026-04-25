# Roadmap

This roadmap describes the intended direction for `mato` over the next year. It is not a guarantee of delivery order or release timing; it exists to help users and contributors understand project priorities and non-goals.

## Near-Term Priorities

- Improve public project readiness, including OpenSSF Best Practices Silver documentation, installation compliance, governance, and security assurance evidence.
- Harden release and installation workflows so users can verify binaries, provenance, checksums, and installed paths consistently.
- Add a project-maintained Dockerfile/image for agent execution so containers can depend on a known toolchain instead of bind-mounting most host tools into each run.
- Keep queue diagnostics clear and actionable through `mato status`, `mato list`, `mato inspect`, `mato doctor`, and failure markers.
- Continue improving test coverage around task parsing, queue state transitions, review flow, merge recovery, and runtime sidecar cleanup.

## Mid-Term Priorities

- Improve task-planning guidance in the bundled `mato` skill so generated tasks are smaller, better scoped, and easier to schedule safely.
- Expand troubleshooting documentation for common Docker image, GitHub CLI, Copilot CLI, authentication, and queue-state failures.
- Improve observability for long-running sessions, including clearer progress, completion history, and stuck-work explanations.
- Continue hardening autonomous-agent boundaries, credential forwarding behavior, path validation, and branch/queue race handling.
- Add macOS and Windows CLI builds for commands that can run safely outside the Linux-only agent runtime, and clearly document any platform-specific limitations.

## Longer-Term Direction

- Evaluate additional packaging channels once the release process is stable and maintainable.
- Evaluate broader agent-host skill installation support as `gh skill` support evolves.
- Reduce dependence on host-mounted executables by moving agent-runtime dependencies into the maintained Docker image where practical.
- Preserve the filesystem-backed queue model and avoid introducing a required hosted service, daemon, or database.
- Evaluate full non-Linux runtime support after macOS and Windows builds exist and Linux-specific process supervision assumptions have portable replacements.

## Non-Goals

- `mato` will not replace human review, repository policy, or release ownership.
- `mato` will not make autonomous agents trusted actors on the target branch.
- `mato` will not require a central service or hosted control plane.
- macOS and Windows builds do not imply immediate full parity with Linux agent execution; non-Linux runtime support must preserve the same safety and recovery model.

## Contribution Areas

Useful contributions include:

- focused bug fixes with regression tests
- documentation improvements for setup, troubleshooting, and task authoring
- additional test coverage for queue, runner, review, merge, and parser edge cases
- packaging and install-script improvements that preserve signature/provenance verification
- diagnostics that make operator action clearer without changing queue semantics
