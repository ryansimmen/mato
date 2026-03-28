# Make `.queue` a Derived Manifest, Not a Claim-Time Input

**Priority:** High
**Effort:** Low

## Problem

The host poll loop already computes the effective runnable backlog in memory.
Today it then writes that ordering to `.mato/.queue`, only for the claim path to
immediately read the manifest back from disk and rebuild the candidate list.

Current flow:

```text
pollWriteManifest()
  -> ComputeRunnableBacklogView()   // derive runnable order from PollIndex
  -> WriteQueueManifestFromView()   // write .mato/.queue

pollClaimAndRun(..., view)
  -> SelectAndClaimTask()
       -> selectCandidates()        // read .mato/.queue back from disk
       -> re-parse each candidate   // claim-time validation from task files
```

That means the hot path does a write-then-read round trip through the
filesystem even though the authoritative ordering was already available in the
same poll cycle.

This duplication adds complexity without adding real authority:

- `ComputeRunnableBacklogView()` already excludes dependency-blocked and
  conflict-deferred backlog tasks.
- `SelectAndClaimTask()` still re-parses task files, re-checks `depends_on`,
  counts failure markers, and applies retry cooldown before claiming, so the
  manifest is not trusted as a full source of truth anyway.
- `selectCandidates()` already has stale-manifest repair logic that appends
  missing `backlog/` files, which is a sign that `.queue` is advisory rather
  than authoritative.
- Read-only surfaces such as `mato status` and `mato inspect` already derive
  ordering from `PollIndex` + `ComputeRunnableBacklogView()` without consulting
  `.queue`.

The result is a small but unnecessary TOCTOU window between manifest write and
claim-time read, plus extra code to reconcile stale filesystem state back into
the in-memory view the host already had.

## Proposal

Use the runnable backlog view as the claim-order source for the host loop.
Continue writing `.queue` as a best-effort derived manifest for operators and
lightweight tooling, but stop letting host-loop control flow depend on reading
it back.

Because mato is an internal tool here, this proposal prefers the simplest API
shape over backward compatibility: claiming should take an ordered `[]string`
candidate list and `.queue` should be explicitly documented as an operator/debug
artifact rather than a scheduler input.

In short:

- `ComputeRunnableBacklogView()` becomes the source of candidate ordering.
- `SelectAndClaimTask()` accepts an ordered `[]string` candidate list.
- `.queue` remains an exported artifact for inspection.
- Claim-time validation and atomic move semantics stay unchanged.

## Detailed Changes

1. **Claim ordering comes from the runnable view**

   `pollClaimAndRun()` already receives the current cycle's
   `RunnableBacklogView`. It should convert `view.Runnable` into an ordered
   `[]string` of candidate filenames and pass that into `SelectAndClaimTask()`.

2. **`SelectAndClaimTask()` stops reading `.queue`**

   The claim path should iterate the ordered candidates supplied by the caller
   instead of calling `selectCandidates()` to read `.queue`.

3. **Drop redundant deferred filtering in the claim path**

   `view.Runnable` already excludes `view.Deferred`, so the claim path should no
   longer rebuild the full deferred set. It only needs to filter additional
   exclusions that are not part of the runnable view, such as
   `failedDirExcluded`.

4. **Keep `.queue` as a derived manifest**

   `WriteQueueManifestFromView()` stays in place and the file format stays the
   same. Operators can still inspect `.mato/.queue`, and any lightweight local
   tooling that reads it continues to work.

## What Changes

- The `.queue`-reading branch in `selectCandidates()` goes away, or the helper
  disappears entirely.
- The stale-manifest repair path that appends missing `backlog/` entries is no
  longer needed for claim ordering.
- The claim path stops rebuilding `view.Deferred`; it only applies extra
  exclusions such as `failedDirExcluded`.
- One filesystem read disappears from the claim path in the steady-state poll
  loop.
- Claim ordering, `status`, and `inspect` all use the same derived runnable
  backlog model.

## What Stays the Same

- `.queue` is still written once per poll cycle for visibility and tooling.
- Runnable backlog computation still lives in `ComputeRunnableBacklogView()`.
- Claim-time safety still re-parses task files from disk before claiming.
- Dependency-blocked backlog tasks are still moved back to `waiting/` as a
  safety net if they slipped into `backlog/` after reconcile.
- Retry exhaustion, retry cooldown, branch collision handling, and multi-process
  safety via `AtomicMove` remain unchanged.

## Why This Is Better

- **Removes duplicate control flow:** the host no longer computes ordering in
  memory and then re-imports the same ordering from disk.
- **Shrinks the race window:** manual edits or stale manifests can no longer
  affect candidate ordering by landing between manifest write and claim read.
- **Simplifies the code:** no manifest-repair fallback logic is needed in the
  claim path.
- **Aligns the mental model:** `.queue` becomes clearly derived state, which is
  already how most of the codebase treats it.
- **Keeps operator value:** users still have a human-readable manifest showing
  the effective runnable backlog.

## Non-Goals

- Changing the `.queue` file format.
- Removing `.queue` from the repository-local operational state.
- Relaxing claim-time validation or moving trust into `PollIndex` alone.
- Changing queue semantics for dependencies, overlap deferral, retries, or
  claiming concurrency.

## Implementation Sketch

1. Add a small helper that converts `RunnableBacklogView.Runnable` into an
   ordered slice of candidate filenames, preserving existing priority/filename
   order while filtering any extra exclusions such as `failedDirExcluded`.
2. Update `pollClaimAndRun()` to pass that ordered candidate slice into
   `SelectAndClaimTask()`.
3. Change `SelectAndClaimTask()` to accept `candidates []string` and handle only
   claim-time validation plus atomic state transitions, not manifest loading.
4. Remove `selectCandidates()` entirely.
5. Reuse the same ordered-filename helper in manifest writing so claim order and
   `.queue` output are derived from one ordering path.

## Test Plan

- Update claim tests that currently assert `.queue` controls ordering so they
  assert the runnable backlog view controls ordering instead.
- Update direct `SelectAndClaimTask()` callers in tests and integration tests to
  pass ordered candidates explicitly.
- Keep unit tests deliberate about scope: claim-logic tests should usually pass
  hand-crafted `[]string` candidate lists so they exercise claim behavior in
  isolation, while integration-style tests should derive candidates from
  `ComputeRunnableBacklogView()` so they cover the full ordering path.
- Keep existing tests for claim-time re-validation, dependency demotion, retry
  exhaustion, cooldown handling, and concurrent claims.

## Docs Impact

If this lands, docs should consistently describe `.queue` as a derived manifest
for inspection and debugging, not as an input the host must read to decide what
to claim next. In particular, `README.md`, `docs/architecture.md`, and
`docs/task-format.md` should stop describing `.queue` as part of claim-time
control flow.

## Design Considerations

- Verify that no external automation depends on `.queue` to influence claim
  ordering rather than to observe it.
- Keep the manifest format stable so existing local inspection scripts do not
  break.
- Keep the change scoped to claim-path simplification; it does not depend on the
  state-machine formalization work.

## Relationship to Existing Features

- Simplifies the current scheduler path without changing user-visible queue
  behavior.
- Matches how `mato status` and `mato inspect` already reason about runnable
  work.
- Makes the role of `.queue` explicit: operator/debug output, not scheduler
  authority.
- Reduces one surface where stale filesystem state can create surprising claim
  behavior.
