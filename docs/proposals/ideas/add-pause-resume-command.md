---
id: add-pause-resume-command
priority: 45
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/runner/
  - internal/status/
  - internal/doctor/
  - internal/pause/
  - README.md
  - docs/architecture.md
  - docs/configuration.md
tags: [feature, cli]
estimated_complexity: medium
---
# Add `mato pause` and `mato resume` commands

## Goal

Add durable repo-local pause mode to stop new agent and review work without
killing the orchestrator, while letting already-running agent tasks finish and
letting ready-to-merge work continue. Expose pause mode in `mato status` and
`mato doctor` so operators can immediately see why queue progress has stopped.

## Scope

### In scope

- new `mato pause` / `mato resume` Cobra subcommands with `--repo`
- a shared pause-state helper for `.mato/.paused`
- runner gating that skips task-claim and review-agent phases while paused but
  continues cleanup, reconcile, queue-manifest refresh with all existing
  exclusions, and merge processing
- status text and JSON reporting for pause state
- doctor hygiene warnings for long-lived, malformed, or unreadable pause state
- documentation and test coverage for the new operational behavior

### Out of scope

- pausing, cancelling, or killing already-running agent containers
- per-task or per-agent pause controls
- config/env-driven pause defaults
- automatic pause expiry, scheduled resumes, or maintenance windows
- pause metadata beyond the original pause timestamp
- changing merge-queue behavior during pause
- converting pause into a new queue directory/state

## Design

### Pause state model

Use a durable sentinel file at `<repo>/.mato/.paused`, not a PID-tied lock in
`.mato/.locks/`. Pause is an operator toggle, not ownership of a critical
section: it must survive orchestrator restart and must not disappear just
because the process that created it exits.

The sentinel contains a single RFC3339 UTC timestamp line representing when the
repository entered pause mode. Missing file means unpaused.

Introduce a small shared package such as `internal/pause` so `cmd/mato`,
`runner`, `status`, and `doctor` all use the same path construction, parsing,
and semantics. The helper should own:

- sentinel path resolution from `tasksDir`
- `Pause(tasksDir, now)` create/repair semantics via `atomicwrite.WriteFile`
- `Resume(tasksDir)` idempotent removal
- `Read(tasksDir)` / `Status(tasksDir)` returning pause state, timestamp when
  available, and surfaced problems for malformed or unreadable sentinel content

The helper should follow one authoritative decision tree:

- `os.ErrNotExist`: repository is not paused
- sentinel exists and parses: repository is paused since the stored timestamp
- sentinel exists but cannot be read or parsed: repository is treated as paused
  for safety, timestamp is unavailable, and the helper returns problem details
  for caller-specific warnings/diagnostics
- unexpected path/stat failure: return a wrapped error so the caller can decide
  whether the operation should fail hard or degrade to a warning

Explicit command semantics:

- `mato pause` on a missing sentinel creates it with the current UTC timestamp
- `mato pause` on a valid sentinel is idempotent and preserves the original
  timestamp
- `mato pause` on an unreadable or malformed sentinel repairs it by atomically
  overwriting the file with a fresh current timestamp and reports that repair
  in command output
- `mato resume` succeeds whether the sentinel is valid, invalid, unreadable, or
  absent; if present, it removes the file

### Initialization precondition

`pause` and `resume` are operational controls for an existing mato queue. They
should require `<repo>/.mato/` to exist and should not implicitly create queue
directories or bootstrap the repository. If `.mato/` is missing, the commands
should fail with a clear error directing the user to run `mato init` first.

### CLI surface

Add normal Cobra subcommands from `newRootCmd()`, following `status`, `init`,
and `retry` patterns instead of the root command's custom flag parser:

- `mato pause [--repo <path>]`
- `mato resume [--repo <path>]`

Both commands resolve the repo root exactly like other subcommands and should
reject positional arguments.

Expected user-facing behavior:

- `pause`: create or repair `.mato/.paused`, then print the effective pause
  timestamp
- `pause` when already paused with a valid sentinel: print `Already paused since ...`
- `resume`: remove `.mato/.paused` and print a confirmation
- `resume` when not paused: print `Not paused` and exit 0

This change does not add `--branch`, `--reason`, or `--fix` flags.

### Runner behavior

Pause is a runner-level gate, not a queue-state transition. Queue directories
remain unchanged; pause only controls whether the host starts new agent or
review work.

Per poll iteration:

1. Keep existing cleanup and reconcile steps unchanged.
2. Refresh `.mato/.queue` from the current reconcile/deferred snapshot even
   while paused, preserving every current exclusion source the runner already
   applies today: overlap deferrals and the persistent `failedDirExcluded` set
   used to avoid livelock when `failed/` is unavailable.
3. Check pause state before starting claim work. If paused, skip only the
   claim/run portion after manifest refresh.
4. Re-check pause state immediately before invoking the review phase. If
   paused, skip `pollReview`, preventing new review-agent work from starting
   against tasks already sitting in `ready-for-review/`.
5. Always allow `pollMerge` to run so already-approved work can drain into
   `completed/`.
6. Suppress the normal idle heartbeat while paused so the CLI does not
   alternate between `idle` and `paused` messages. Print a periodic paused
   heartbeat such as `[mato] paused — run 'mato resume' to continue`, using the
   same throttled cadence style as the existing heartbeat logic rather than
   printing every poll.
7. If the sentinel exists but cannot be read or parsed, treat the repo as
   paused for safety, surface a warning, and do not auto-delete or ignore the
   file.
8. Throttle unreadable/malformed-sentinel warnings with the same cadence as the
   paused heartbeat so a bad file does not spam stderr every 10 seconds.
9. Treat pause entry/exit as heartbeat-state transitions: reset the relevant
   heartbeat bookkeeping when entering pause mode and when leaving it, so resume
   does not immediately emit a misleading idle heartbeat based on pre-pause
   counters.
10. Pause/resume take effect at phase boundaries, not only full poll
    boundaries: for example, if a repository is paused at the claim gate but
    resumed before the later review gate in the same iteration, review work may
    proceed in that iteration.

To keep this behavior testable, the implementation should extract the current
loop body into a one-iteration helper (or an equivalent deterministic seam)
that `pollLoop()` calls each cycle. That helper should expose enough outcome
data for tests to assert manifest refresh, phase gating, and heartbeat
decisions without needing time-based end-to-end loop tests.

### Status and observability

Extend gathered status data with pause state so both text and JSON outputs
report it from the same source of truth.

- Text view: add a pause-state line in `renderQueueOverview` (for example
  `pause state: paused since ...` or `pause state: not paused`). Keep
  merge-queue status separate so `paused + merge active` is visible.
- JSON view: add a stable top-level object such as:

```json
"paused": {
  "active": true,
  "since": "2026-03-23T10:00:00Z"
}
```

When not paused, emit `{"active": false}` for schema stability.

- Text rendering, JSON `paused` output, and any warnings about
  unreadable/malformed pause state should all be derived from the same gathered
  helper result so the two output modes do not drift.
- If the sentinel is unreadable or malformed, surface a warning in the existing
  `warnings` output and still mark `paused.active = true`, omitting `since`
  because it is unavailable.

### Doctor integration

Do not add a new doctor check name. Fold pause diagnostics into the existing
`hygiene` category, which already handles operator-forgotten runtime artifacts
like stale messages and stale merge locks.

Add these hygiene findings:

- `hygiene.paused`: warning when `.mato/.paused` exists and its timestamp is
  older than 24 hours; include the recorded timestamp and computed age in the
  message so the finding is immediately actionable
- `hygiene.invalid_pause_file`: warning when the sentinel exists but cannot be
  parsed as an RFC3339 timestamp
- `hygiene.pause_unreadable`: warning when the sentinel path exists but cannot
  be read

These findings should remain warning-level only and should not be auto-fixed by
`--fix`; auto-resume would be too surprising for an operational toggle.

## Step-by-Step Breakdown

1. **Introduce the shared pause helper**
   - Add a small internal package for `.mato/.paused` pathing, atomic write,
     parse, repair, and clear semantics.
   - Make the helper return enough state to distinguish missing, valid, and
     repaired/problem cases.
   - Add focused unit tests for missing, valid, malformed, unreadable,
     repaired, already-paused, and resume-idempotent cases.

2. **Wire CLI commands**
   - Register `newPauseCmd()` and `newResumeCmd()` in `cmd/mato/main.go`.
   - Reuse existing repo-resolution helpers and explicitly validate that
     `.mato/` exists before mutating pause state.
   - Add command tests for success, unreadable/malformed-sentinel repair,
     repeated pause preserving the original timestamp, resume on valid and
     invalid sentinels, missing `.mato/`, invalid repo input, and no-extra-args
     behavior.

3. **Refactor runner polling for deterministic testing**
   - Extract the per-iteration body from `pollLoop()` into a focused helper (or
     equivalent seam) that encapsulates manifest refresh, claim/review/merge
     gating, and heartbeat decisions.
   - Keep `pollLoop()` responsible for cancellation and sleep/backoff behavior.
   - Use existing repo conventions (focused helpers plus function-variable test
     hooks where useful) so tests can observe whether manifest refresh, claim,
     review, and merge phases were invoked.

4. **Gate runner work on pause state**
   - Keep `.queue` fresh even while paused by separating manifest writing from
     task claiming, while preserving both overlap deferrals and
     `failedDirExcluded` exclusions.
   - Read pause state after reconcile and again before review selection.
   - Skip claim/review launches when paused; keep merge processing unchanged.
   - Add paused-heartbeat behavior and ensure idle messaging is suppressed while
     paused.
   - Throttle unreadable/malformed pause warnings together with the paused
     heartbeat.
   - Reset heartbeat bookkeeping on pause/resume transitions.
   - Add runner tests that deterministically verify:
     - paused iterations still refresh `.queue`
     - paused iterations preserve `failedDirExcluded` exclusions in the manifest
     - paused iterations do not invoke the claim path
     - paused iterations do not invoke the review path
     - paused iterations still invoke merge processing
     - paused heartbeat replaces idle output
     - unreadable or malformed sentinel keeps the runner paused-safe and emits
       throttled warnings
     - resuming clears paused-heartbeat state so the next unpaused idle cycle
       behaves normally

5. **Surface pause state in status**
   - Extend `statusData`, gather helpers, text rendering, and JSON conversion.
   - Add/adjust tests so text and JSON stay aligned, including
     unreadable/malformed-sentinel warnings and the inactive
     `paused.active = false` case.

6. **Extend doctor hygiene**
   - Add pause-sentinel scanning to `checkHygiene`.
   - Add unit tests for not paused, paused recently, paused for more than
     24 hours, malformed sentinel, unreadable sentinel, and `--only hygiene`
     coverage.

7. **Update user-facing docs**
   - Update `README.md` command list, queue-layout snippets, and status/doctor
     descriptions.
   - Update `docs/configuration.md` CLI usage, subcommand docs, and pause-state
     reporting.
   - Update `docs/architecture.md` polling-loop description and filesystem/layout
     notes so pause is documented as runner-level state backed by the optional
     `.mato/.paused` sentinel, not a task state.

8. **Validate**
   - Run `go build ./...`
   - Run `go vet ./...`
   - Run `go test -count=1 ./...`

## File Changes

```text
cmd/mato/main.go
  Add `pause` and `resume` Cobra subcommands and register them on the root command.

cmd/mato/main_test.go
  Cover command wiring, initialized-repo preconditions, unreadable/malformed-sentinel repair, idempotent pause/resume behavior, and output.

internal/pause/pause.go
  New shared helper for reading, writing, repairing, and removing the `.mato/.paused` sentinel.

internal/pause/pause_test.go
  Unit tests for sentinel lifecycle, repair behavior, and read/parse-error handling.

internal/runner/runner.go
  Extract one-iteration polling logic, keep `.queue` fresh while paused, preserve `failedDirExcluded` manifest exclusions, gate claim/review phases on pause state, keep merge active, and add paused heartbeat handling.

internal/runner/runner_test.go
  Verify paused polling semantics, manifest refresh behavior, exclusion preservation, warning throttling, and heartbeat decisions through the new iteration seam.

internal/status/status_gather.go
  Read pause state into `statusData` and warnings.

internal/status/status_render.go
  Render pause state in the queue overview.

internal/status/status_json.go
  Add machine-readable pause state to JSON output.

internal/status/status_gather_test.go
internal/status/status_render_test.go
internal/status/status_test.go
  Update/extend status tests for pause state, warnings, and text/JSON parity.

internal/doctor/checks.go
  Add pause-sentinel hygiene diagnostics.

internal/doctor/checks_test.go
  Cover stale, malformed, and unreadable pause-sentinel findings.

README.md
  Document the new commands, the optional `.mato/.paused` sentinel, and paused behavior.

docs/configuration.md
  Document command usage and pause-state reporting.

docs/architecture.md
  Document the poll-loop pause gate and the optional `.mato/.paused` sentinel in architecture/layout descriptions.
```

## Error Handling

- Wrap file I/O errors with the sentinel path and operation (`read pause
  sentinel`, `write pause sentinel`, `repair pause sentinel`, `remove pause
  sentinel`).
- Use atomic writes for creating or repairing the sentinel; never partially
  write pause state.
- Treat missing sentinel as the normal unpaused case in shared readers.
- Treat `resume` on `os.ErrNotExist` as success.
- Fail `pause`/`resume` with a clear initialization error when `.mato/` does
  not exist.
- Treat existing-but-unreadable or malformed sentinel content as paused-safe in
  the runner, with surfaced warnings, rather than assuming unpaused.
- Surface unreadable/malformed pause problems in `status` warnings instead of
  hiding them.
- Report stale, malformed, and unreadable pause state in doctor as
  warning-level findings, preserving normal exit-code semantics for warnings vs
  errors.

## Testing Strategy

- **Pause helper tests** for valid timestamp parsing, unreadable/malformed
  content, repair behavior, repeated pause preserving original timestamp, and
  resume of missing files.
- **Command tests** in `cmd/mato/main_test.go` for:
  - `mato pause` creates the sentinel
  - second `mato pause` preserves the original timestamp / reports already paused
  - `mato pause` repairs unreadable/malformed sentinel content
  - `mato resume` removes valid sentinel
  - `mato resume` removes invalid/unreadable sentinel
  - `mato resume` when absent is a no-op success
  - both commands fail clearly when `.mato/` is missing
- **Runner tests** for:
  - paused iteration still refreshes `.queue`
  - paused iteration preserves `failedDirExcluded` exclusions
  - paused iteration does not call claim work
  - paused iteration does not call review work
  - paused iteration still calls merge work
  - paused heartbeat replaces idle output
  - unreadable/malformed sentinel keeps work paused and warnings are throttled
  - resuming resets heartbeat state so the next idle cycle is not polluted by
    paused counters
- **Status tests** for:
  - text output shows pause state in queue overview
  - JSON output includes stable `paused` object
  - inactive status returns `paused.active = false`
  - unreadable/malformed sentinel produces warning output / JSON warning while
    still reporting `paused.active = true`
- **Doctor tests** for:
  - no finding when not paused
  - no finding when paused recently
  - warning when paused more than 24 hours
  - warning when sentinel is malformed
  - warning when sentinel is unreadable
  - findings appear under `hygiene`
- **Validation**: `go build ./... && go vet ./... && go test -count=1 ./...`

## Risks & Mitigations

- **Race between `mato pause` and the live poll loop**: a pause request that
  lands after the claim gate may still allow the currently-starting task to
  run. Mitigation: gate claim and review separately, document that pause takes
  effect at phase boundaries, and never interrupt already-running agent work.
- **Stale or incorrect `.queue` while paused**: separating manifest refresh
  from claim could accidentally drop existing exclusions. Mitigation: preserve
  both overlap deferrals and `failedDirExcluded` when refreshing the manifest
  during pause.
- **Confusing operator output while paused**: existing idle messaging would
  make the queue look merely empty. Mitigation: add explicit pause-state lines
  in `status` and a paused heartbeat in the runner.
- **Drift between text and JSON status**: adding state to only one surface
  would break observability parity. Mitigation: source both from shared
  gathered pause data and extend parity-focused tests.
- **Accidental auto-resume via doctor `--fix`**: treating pause like a stale
  lock would surprise operators. Mitigation: keep pause findings read-only
  warnings.
- **Inconsistent semantics across packages**: duplicating sentinel parsing
  across `cmd`, `runner`, `status`, and `doctor` would drift over time.
  Mitigation: centralize behavior in the shared pause helper.

## Open Questions

None for implementation, assuming the intended semantics remain:

- `pause` halts new task claims and new AI review runs
- merge processing continues
- pause metadata is limited to the original pause timestamp unless the sentinel
  must be repaired

If maintainers instead want a broader `drain mode` that continues review agents
while paused, that should be treated as a deliberate scope change rather than
folded into this plan.
