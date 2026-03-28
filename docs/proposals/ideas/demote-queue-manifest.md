# Demote .queue from Control Plane to Debug Artifact

**Priority:** Medium
**Effort:** Low

## Problem

The host loop computes the runnable backlog view in memory via
`ComputeRunnableBacklogView()`, writes it to `.mato/.queue` via
`WriteQueueManifestFromView()`, and then `SelectAndClaimTask()` reads `.queue`
back from disk via `selectCandidates()` and re-parses every candidate.

This creates a write-after-read round-trip through the filesystem:

```
pollIterate
  → ComputeRunnableBacklogView()   // computes runnable order in memory
  → WriteQueueManifestFromView()   // writes .queue to disk
  → SelectAndClaimTask()
      → selectCandidates()         // reads .queue back from disk
      → re-parses each candidate   // re-validates from task files
```

The `.queue` file is already partially advisory: `selectCandidates` appends
missing backlog files so stale manifests cannot strand work. Read-only surfaces
like `mato status` and `mato inspect` already derive ordering from `PollIndex`
and `ComputeRunnableBacklogView` without reading `.queue` at all.

This means `.queue` is a derived artifact that is written to disk and then
immediately read back — a TOCTOU window and unnecessary complexity in the hot
path.

## Idea

Stop reading `.queue` in the claim path. Keep writing it as a debug/operator
artifact.

### Changes

1. **`selectCandidates`**: Instead of reading `.queue` and falling back to
   `backlog/`, always derive candidate order from the in-memory runnable backlog
   view. Pass the pre-computed ordered list from the poll cycle directly to the
   claim function.

2. **`WriteQueueManifestFromView`**: Keep writing `.queue` for operator
   inspection and `mato status --format json`, but it is no longer read by any
   host-loop code path.

3. **`SelectAndClaimTask` signature**: Accept the ordered candidate list as a
   parameter instead of reading it from disk.

### What this removes

- The `selectCandidates` function (or its `.queue`-reading branch).
- One filesystem read per poll cycle in the claim path.
- The TOCTOU window between manifest write and claim read.
- The fallback logic that patches missing files into the `.queue`-derived list.

### What stays the same

- `.queue` is still written for operators and tooling.
- The runnable backlog computation logic is unchanged.
- The claim validation (re-parsing candidates, checking deps, counting failures)
  is unchanged.
- Multi-process claim safety via `AtomicMove` is unchanged.

## Design Considerations

- Verify that no external scripts or CI pipelines depend on `.queue` for claim
  ordering (as opposed to inspection). If any do, document the change.
- The `.queue` file format and content stay identical — only the consumer changes.
- This can be implemented concurrently with the state machine formalization since
  it touches different code paths.

## Relationship to Existing Features

- Simplifies the claim path without changing user-visible behavior.
- Aligns the claim path with how `status` and `inspect` already work (derived from
  in-memory state, not from `.queue`).
- Reduces one surface where stale filesystem state could cause surprising behavior.
