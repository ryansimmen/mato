---
id: compact-status-dashboard
priority: 30
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/status/
  - internal/integration/
  - docs/configuration.md
  - docs/architecture.md
  - README.md
---

# Compact `mato status` Dashboard

## 1. Goal

Make `mato status` easier to scan when many agents are active by replacing the
current long, section-heavy text dashboard with a compact default operator view.

The compact view should answer the highest-value operational questions first:

- Is the queue healthy?
- How many tasks are runnable, running, blocked, or failing?
- What are active agents doing right now?
- Which tasks are next up?
- What needs operator attention?

Detailed diagnostics should remain available, but they should not dominate the
default screen.

## 2. Problem

Today the text status dashboard renders many sections in sequence, including:

- queue overview
- runnable backlog
- active agents
- current agent progress
- in-progress tasks
- ready-for-review
- ready-to-merge
- dependency-blocked tasks
- conflict-deferred tasks
- failed tasks
- recent completions
- recent messages
- warnings

This is informative, but it is too long and repetitive for routine operator
use, especially in watch mode and especially once more than a handful of agents
are running.

The biggest usability issues are:

- Agent state is split across three separate sections, forcing the operator to
  correlate agent identity, claimed task, and latest progress manually.
- Large queues push the most important live information off-screen.
- History-ish content appears in `mato status` even though `mato log` already
  exists for recent durable outcomes.
- Empty or low-signal sections still consume vertical space.
- Long sections have no truncation strategy, so watch mode becomes noisy rather
  than glanceable.

## 3. Design Goals

The text view should optimize for the common operator loop:

1. glance at queue health
2. scan active work
3. identify exceptions
4. inspect a specific task only when needed

To support that loop, the new default text dashboard should be:

- compact
- prioritized
- non-redundant
- stable in watch mode
- bounded in vertical growth

JSON output should remain detailed and machine-readable. This proposal focuses
on the text renderer and CLI behavior for human operators.

## 4. Scope

### In scope

- redesign the default text layout for `mato status`
- add a compact summary-oriented default text view
- collapse agent/task/progress information into a single active-work section
- truncate long sections with explicit `+N more` summaries
- hide low-signal sections from the default view when they are empty or better
  served by another command
- preserve access to detailed diagnostics via an explicit flag or alternate
  view mode
- update docs and tests for the new text behavior

### Out of scope

- changing queue semantics, scheduler behavior, or task lifecycle rules
- reducing or restructuring JSON output
- replacing `mato inspect` or `mato log`
- adding terminal UI features such as scrolling, panes, keyboard shortcuts, or
  curses-style interaction
- adding historical filtering to `mato status`

## 5. Proposed UX

### 5.1 Default text view becomes compact

`mato status` should default to a short operator dashboard with four ordered
areas:

1. headline queue summary
2. active agents / active work
3. attention summary
4. next-up runnable tasks

Example shape:

```text
Queue: 24 backlog | 9 runnable | 7 running | 3 review | 1 merge | 2 failed
Pause: not paused   Merge queue: idle

Agents (8)
agent-a1  task-x.md   WORK      20s
agent-b2  task-y.md   TEST      1m
agent-c3  task-z.md   COMMIT    2m
... +5 more

Attention
2 failed
4 blocked by dependencies
3 conflict-deferred

Next Up
1. first-task.md
2. second-task.md
3. third-task.md
... +6 more
```

This is intentionally not a full dump of queue state. It is a concise status
screen for answering, "What is happening right now, and does anything need my
attention?"

Warnings are the exception to this compacting rule. Non-empty infrastructure
warnings should still appear in compact mode because they indicate queue-health
problems rather than routine task state. In the compact layout they should be
surfaced in the `Attention` section, either as a warning count summary when many
exist or as explicit warning lines when the list is short.

### 5.2 One row per active agent

The current text view splits live work across:

- `Active Agents`
- `Current Agent Progress`
- `In-Progress Tasks`

These should be collapsed into one canonical representation in the compact
view.

Each active row should aim to include:

- agent id
- current task name when known
- condensed progress stage or message
- recency (`20s`, `1m`, etc.)

When the latest progress body contains a recognizable structured step name such
as `WORK`, `TEST`, or `COMMIT`, compact mode should render that short stage
label directly. When no recognizable stage is available, compact mode should
fall back to a truncated raw progress message.

If presence info or progress is missing, the row should degrade gracefully
without creating a second section just to show partial data.

### 5.3 Bounded lists

Long sections should have sensible caps in the default view.

Initial recommended defaults:

- active agents: show first 5 rows, then `... +N more`
- next-up runnable tasks: show first 5 rows, then `... +N more`
- failures / blockers in the compact view: summarize counts first rather than
  rendering every task
- warnings: always surface in compact mode, but fold them into `Attention`
  rather than rendering a separate trailing section

These caps should be hardcoded initially. A configurable `--max-items` flag is
not needed for the first version.

This makes the dashboard stable in watch mode and keeps the top of the screen
useful even when the queue is large.

### 5.4 Move history-like details out of default status

The default compact view should not render:

- `Recent Completions`
- `Recent Messages`

Those are useful, but they are not the highest-value live operator data, and
they already overlap with the purpose of `mato log`.

They may remain available in a fuller status mode if desired, but they should
not be part of the default text path.

### 5.5 Keep a detailed mode

Operators sometimes need the current deeper status breakdown. The proposal
should preserve a fuller diagnostic path behind an explicit flag.

Recommended command shape:

- `mato status` -> compact default
- `mato status --verbose` -> expanded status view

Alternative naming such as `--view compact|full` is possible, but `--verbose`
is likely the smallest CLI change and easiest to understand.

## 6. Implementation Plan

### Phase 1: Add an explicit text view mode

1. Extend `newStatusCmd()` in `cmd/mato/main.go` with a text-view flag,
   preferably `--verbose` for an expanded text dashboard.
2. Update `status.Show()` / `status.ShowTo()` to accept the selected text view
   mode while preserving existing JSON behavior.
3. Add CLI tests in `cmd/mato/main_test.go` covering help text, default compact
   behavior, and `--verbose` acceptance.

### Phase 2: Introduce a compact render path

4. Add a compact text renderer in `internal/status/status_render.go` or a new
   companion file such as `status_render_compact.go`.
5. Keep data gathering largely unchanged at first; derive the compact screen
   from the existing `statusData` model to minimize risk.
6. Render a compact queue headline and compact pause / merge-lock status.
7. Add render tests for empty, small, and large queue scenarios.

### Phase 3: Collapse active-work presentation

8. Introduce a compact active-work row model derived from `agents`,
   `presenceMap`, `activeProgress`, and `inProgressTasks`.
9. Render one row per active agent in stable sorted order.
10. Define fallback display rules when only some of presence, progress, or
    claimed-task metadata is available.
11. Add tests covering:
    - agent with presence and progress
    - agent with presence but no progress
    - agent with progress but no presence
    - many active agents with truncation

### Phase 4: Reduce default detail and bound list growth

12. Cap active-agent and next-up sections in compact mode.
13. Replace per-task blocked/deferred rendering in compact mode with concise
    counts or short summaries.
14. Remove recent completions and recent messages from the compact default view.
15. Preserve those sections in verbose mode if they remain useful there.
16. Add tests asserting `+N more` behavior and omission of compact-hidden
    sections.

### Phase 5: Docs and integration coverage

17. Update `docs/configuration.md` for the new text behavior and flags.
18. Update `docs/architecture.md` if the status package gains separate compact
    and verbose render paths.
19. Update `README.md` command documentation and examples.
20. Add integration tests that exercise representative compact and verbose
    outputs in a real repo setup.

## 7. Recommended Delivery Order

To keep the change safe and reviewable, ship it in this order:

1. Add the CLI flag and internal mode plumbing.
2. Add the compact queue summary and compact active-agent section.
3. Add list truncation.
4. Remove history-like sections from the default compact view.
5. Polish wording, tests, and docs.

This sequence allows the compact renderer to land incrementally without forcing
an immediate rewrite of all status rendering code.

## 8. Testing Strategy

### Unit tests

- renderer tests for compact mode using `plainColorSet()` and constructed
  `statusData`
- truncation tests for active agents and runnable backlog
- mixed-metadata tests for active agent rows
- assertions that compact mode omits recent messages and recent completions
- assertions that verbose mode still includes expanded sections when expected

### CLI tests

- `mato status --help` mentions the new text flag
- `mato status --verbose` succeeds
- `mato status --format json --verbose` behavior is defined and tested
  (recommended: ignore text-only verbosity in JSON mode or reject it explicitly)

### Integration tests

- queue with no active agents
- queue with several active agents and mixed progress freshness
- queue with many runnable tasks to verify truncation
- queue with failed, blocked, and deferred work to verify compact attention
  summaries

## 9. Decisions

1. Compact becomes the default text view immediately. This proposal exists
   because the current default output is the problem; hiding the new design
   behind an opt-in flag would not solve that.
2. Verbose mode does not need to preserve the exact current output shape.
   Preserving equivalent information access is sufficient, which leaves room to
   reorder sections, tighten formatting, and hide truly empty sections.
3. Compact active-agent rows should normalize progress into short stage labels
   when feasible, using recognizable step names such as `WORK`, `TEST`, and
   `COMMIT`, with fallback to truncated raw message text.
4. List caps should be hardcoded initially at 5. Additional item-count flags
   are unnecessary until operators demonstrate a concrete need.

## 10. Recommendation

Make compact the default text view and keep the fuller screen behind
`--verbose`.

The current dashboard is closer to a diagnostic report than a glanceable status
screen. A compact default will better match how operators actually use
`mato status`, especially in watch mode and in repos with many concurrently
active agents.
