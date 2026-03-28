# Demote .queue from Control Plane to Debug Artifact

**Priority:** High
**Effort:** Low

## Problem

The host loop already computes the runnable backlog in memory via
`ComputeRunnableBacklogView()`, writes that ordering to `.mato/.queue` via
`WriteQueueManifestFromView()`, and then `SelectAndClaimTask()` reads `.queue`
back from disk via `selectCandidates()` before re-parsing each candidate.

That creates a write-after-read round-trip through the filesystem:

```
pollIterate
  -> ComputeRunnableBacklogView()   // computes runnable order in memory
  -> WriteQueueManifestFromView()   // writes .queue to disk
  -> SelectAndClaimTask()
      -> selectCandidates()         // reads .queue back from disk
      -> re-parses each candidate   // re-validates from task files
```

The `.queue` file is already partly advisory: `selectCandidates()` appends
missing backlog files so stale manifests cannot strand work. Read-only surfaces
like `mato status` and `mato inspect` already derive ordering from `PollIndex`
and `ComputeRunnableBacklogView()` without reading `.queue` at all.

That means `.queue` is a derived artifact that is written to disk and then
immediately read back in the hot path. It adds a TOCTOU window and extra
complexity without adding real authority.

## Idea

Stop reading `.queue` in the claim path. Keep writing it as a debug and operator
artifact.

### Changes

1. **Claim ordering**: derive the candidate order from the already-computed
   runnable backlog view instead of from `.queue`.

2. **`SelectAndClaimTask` input**: change the claim path to accept either the
   ordered candidate list or the runnable backlog view directly from the poll
   cycle.

3. **`.queue` role**: keep `WriteQueueManifestFromView()` and the current file
   format for inspection and tooling, but do not let host-loop control flow
   depend on the file.

### What this removes

- The `.queue`-reading branch in `selectCandidates()` or the helper entirely.
- One filesystem read per poll cycle in the claim path.
- The TOCTOU window between manifest write and claim read.
- The fallback logic that patches missing files into a `.queue`-derived list.

### What stays the same

- `.queue` is still written for operators and tooling.
- Runnable backlog computation stays in `ComputeRunnableBacklogView()`.
- Claim-time validation still reparses candidates, checks dependencies, counts
  failure markers, and applies retry cooldown.
- Multi-process claim safety via `AtomicMove` stays unchanged.

## Implementation Notes

- `pollClaimAndRun()` already has the runnable view from `pollWriteManifest()`,
  so this is mostly a claim-path API cleanup.
- Tests that currently assert `.queue` controls claim ordering should be updated
  to assert that the runnable backlog view controls ordering instead.
- If this lands, docs that currently describe `.queue` as claim input should be
  updated to describe it as a derived manifest.

## Design Considerations

- Verify that no external scripts or CI glue depend on `.queue` for claim
  ordering rather than inspection.
- Keep the `.queue` file format identical so `mato status --format json` and any
  lightweight tooling remain unaffected.
- This can be implemented independently of state-machine work.

## Relationship to Existing Features

- Simplifies the claim path without changing user-visible queue behavior.
- Aligns claiming with how `status` and `inspect` already work: derived from
  in-memory state, not from `.queue`.
- Reduces one surface where stale filesystem state can cause surprising behavior.
