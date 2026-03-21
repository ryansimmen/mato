# The Great Refactoring: 24 Tasks, 49 Files, Zero Behavior Changes

*March 2026*

We ran a codebase-wide refactoring of mato — 24 tasks planned, executed, and
merged in a single session. The twist: we used mato to orchestrate its own
refactoring. The task files, dependency chains, and `affects` metadata that
mato uses to coordinate AI agents were the same mechanisms we used to plan
and sequence the work. Every task was a pure structural improvement: no
features added, no behavior changed, all existing tests passing throughout.

## By the numbers

- **24 commits** landed (23 `refactor:` + 1 `fix:` for error context enrichment)
- **49 files** changed
- **3,980 lines added**, **3,078 removed** (net +902, mostly new tests and split files)
- **13 new source files** extracted (1 new package, 12 split-out files)
- **4 monolithic files** decomposed
- **0 bugs** introduced (confirmed by code review and full test suite)

## What we did

The refactoring broke down into four themes:

### 1. Eliminate duplication

The codebase had several patterns copy-pasted across packages. We extracted
each into a single-source-of-truth helper:

| Pattern | Occurrences | Extracted to |
|---------|-------------|-------------|
| Atomic temp-file writes (create → chmod → write → rename) | 6 sites | `internal/atomicwrite/` package |
| TOCTOU-safe file moves (`os.Link` + `os.Remove`) | 6 sites | `queue.AtomicMove()` |
| Task file enumeration (`os.ReadDir` + filter `.md`) | ~20 sites | `queue.ListTaskFiles()` |
| Queue directory name strings (`"backlog"`, `"in-progress"`, etc.) | 56+ sites | `queue.DirBacklog` et al. in `queue/dirs.go` |
| Git helper tool lookup (`exec.LookPath` + fallback) | 2 sites | `findGitHelper()` in `runner/tools.go` |
| HTML comment metadata regexes (branch, claimed-by, failure records) | 8 regexes across 4 packages | `taskfile/metadata.go` |
| Test file-writing helpers (`writeFile`, `writeTestFile`, etc.) | 4 variants | `testutil.WriteFile()` |
| Task ID collection across directories | 2 near-identical functions | `collectTaskIDs()` helper |
| Ready-queue dependency resolution | 2 duplicated loops | `resolvePromotableTasks()` |
| Queue manifest generation | 2 duplicated implementations | `WriteQueueManifest` delegates to `ComputeQueueManifest` |
| Active affects collection | 2 duplicated scanners | Consolidated into `taskfile.CollectActiveAffects` |

### 2. Decompose large functions

Three functions had grown past 100 lines with mixed responsibilities:

| Function | Before | After |
|----------|--------|-------|
| `status.ShowTo()` | 322 lines — data gathering + 15 render sections | Split into `gatherStatus()` + focused render helpers in `status_render.go` |
| `runner.Run()` | 297 lines — tool discovery + Docker config + poll loop | Split into `discoverHostTools()`, `buildDockerCommand()`, and a focused poll loop |
| `merge.ProcessQueue()` | 118 lines | File split into `merge.go` + `squash.go` + `taskops.go` |

Additional function-level cleanups:
- `extractKnownFlags()` — deduplicated switch statements, returns a `runConfig` struct
- `SelectAndClaimTask()` — extracted rollback helpers to reduce nesting
- `postAgentPush()` — extracted `moveTaskToReviewWithMarker()` helper
- `postReviewAction()` — extracted `resolveReviewVerdict()` and `reviewDisposition` struct
- `EnsureGitignored()` — split into `EnsureGitignoreContains()` + `CommitGitignore()`, eliminating the fragile stash/restore dance that had caused three previous bugs (`fix-startup-gitignore-commit`, `add-gitignore-staging-guard`, `fix-ensure-gitignored-error-handling`)
- `dockerConfig` (27 fields) — split into immutable `envConfig` + per-task `runContext`

### 3. Improve file organization

Several files had grown to contain multiple unrelated concerns:

| File | Before | After |
|------|--------|-------|
| `queue/queue.go` | 782 lines — everything | 6 new files split out: `reconcile.go` (252), `overlap.go` (166), `manifest.go` (69), `locks.go` (57), `taskfiles.go` (24), `dirs.go` (20); `queue.go` down to 174 |
| `merge/merge.go` | 554 lines | 3 files: `squash.go` (220), `merge.go` (172), `taskops.go` (122) |
| `status/status.go` | 707 lines | 3 files: `status.go` (376), `status_render.go` (280), `status_gather.go` (145) |
| `runner/runner.go` | 517 lines | `runner.go` (502) + `tools.go` (112) for host tool discovery |

### 4. Strengthen the type system

- **Lock management unified** — three separate lock patterns (agent PID locks,
  review locks, merge locks) now share primitives from `internal/lockfile/`
- **Error context enriched** — `fmt.Errorf` calls now include file paths for
  easier debugging
- **Metadata API centralized** — a single `taskfile/metadata.go` owns all HTML
  comment marker parsing, replacing 8 scattered regexes with one canonical set.
  The regexes weren't just duplicated — they were subtly inconsistent:
  `branchCommentRe` in `queue/claim.go` was anchored (`^...$`) while the
  same-named variable in `status/status.go` was unanchored. No automated tool
  would have flagged this; it required reading the patterns side by side

## How we planned it

Three parallel code analysis passes — structure, duplication, and API design —
identified 15 refactoring opportunities. Cross-referencing against the existing
backlog eliminated 3 duplicates (tasks that were already filed under different
names), bringing the final count to 12 new tasks plus 2 added during review.

The refactoring was planned as a set of mato task files with explicit
dependency chains. Tasks were organized into three priority tiers:

- **P10-15 (foundational):** Extract constants, atomic write utility,
  deduplicate flag parsing — no dependencies, ran first
- **P18-25 (cleanup):** Extract helpers, consolidate duplicates, decouple
  `EnsureGitignored` — depended on foundational tasks
- **P25-35 (decomposition):** Break up large functions, split files, unify
  locks — depended on cleanup tasks

The `depends_on` and `affects` metadata ensured tasks ran in the right order
and never touched the same files concurrently. Three dependency issues were
caught during planning review and fixed before execution began.

## What we learned

1. **Audit before you act.** Three parallel code exploration passes (structure,
   duplication, and API design) found more issues than any single pass would
   alone. The duplication-focused pass caught the scattered atomic write
   pattern that a structural review missed.

2. **Dependencies matter.** Three tasks had incorrect or missing dependencies
   that would have caused them to target already-deleted code. Catching these
   during planning saved wasted work.

3. **Dedup before decompose.** Extracting shared helpers (atomicwrite,
   ListTaskFiles, AtomicMove) before splitting large files meant the split
   files were already cleaner — less code to move, fewer decisions about where
   each function belongs.

4. **Pure refactoring is safe refactoring.** Every commit preserved exact
   behavior. No features were added, no APIs changed externally. The test
   suite served as a continuous safety net.

5. **Verify after you ship.** A post-execution audit mapped each commit to its
   task file and checked 2-4 acceptance criteria from the task's "Steps to fix"
   against the actual diff. All 24/24 met criteria, and dependency ordering was
   confirmed — every task that declared `depends_on` landed after its
   prerequisites in the commit sequence.

## Commit log

| # | Task | Commit |
|---|------|--------|
| 1 | `extract-atomic-write-utility` | `6c4ade2` |
| 2 | `deduplicate-flag-parsing` | `454af6c` |
| 3 | `consolidate-test-file-helpers` | `5e3e6d7` |
| 4 | `simplify-extract-known-flags` | `2e0a420` |
| 5 | `extract-queue-directory-constants` | `b9c54a3` |
| 6 | `extract-git-tool-finder` | `15d4639` |
| 7 | `simplify-claim-rollback` | `13fc8bc` |
| 8 | `consolidate-queue-manifest` | `6d48af6` |
| 9 | `decouple-ensuregitignored-from-commit` | `72e6907` |
| 10 | `consolidate-ready-queue-logic` | `a1b6758` |
| 11 | `consolidate-task-id-collectors` | `b08217f` |
| 12 | `break-up-runner-run` | `301778c` |
| 13 | `extract-atomic-file-move-helper` | `db57d1e` |
| 14 | `simplify-post-agent-push` | `75ef8e0` |
| 15 | `extract-task-file-enumeration-helper` | `220273b` |
| 16 | `split-merge-file` | `9ab9303` |
| 17 | `consolidate-active-affects` | `2e27585` |
| 18 | `break-up-status-showto` | `eff7340` |
| 19 | `refactor-docker-config` | `1429826` |
| 20 | `enrich-error-context` | `1dedb52` |
| 21 | `simplify-review-functions` | `9d10b8d` |
| 22 | `centralize-task-comment-metadata` | `a09317f` |
| 23 | `unify-lock-management` | `942d09f` |
| 24 | `split-queue-file` | `74cda31` |
