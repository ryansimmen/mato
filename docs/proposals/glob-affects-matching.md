# Glob Affects Matching — Implementation Plan

## Summary

Extend `affects:` matching so task files can express glob patterns such as
`internal/runner/*.go` and `internal/**/*_test.go`, while preserving the
current exact-path and trailing-`/` directory-prefix semantics.

The implementation should centralize `affects` semantics in one shared helper
so `frontmatter`, queue overlap detection, `PollIndex.HasActiveOverlap()`, and
host-provided file-claim metadata all agree on the same rules.

Estimated effort: ~1-2 days.

## Goals

- Reduce false negatives in overlap detection without changing the task file
  format.
- Preserve the current O(n+m) fast path for exact-only `affects` lists.
- Keep glob patterns stored as-is in task files; never expand them to concrete
  file lists at parse time.
- Document path semantics clearly enough that task authors can predict matches.

## Non-Goals

- No regex support.
- No glob negation (`!foo/**`) in v1.
- No filesystem expansion or "show me everything this glob matches" feature in
  `mato status` yet.

## Supported Syntax

`affects:` continues to support three forms:

| Form | Meaning | Example |
|------|---------|---------|
| Exact path | One file path | `internal/runner/runner.go` |
| Directory prefix | Entire subtree rooted at that directory | `internal/runner/` |
| Glob pattern | Pattern match using glob metacharacters | `internal/runner/*.go` |

### Path semantics

These rules should be documented in `docs/task-format.md` and enforced
consistently in code:

- Paths are repo-relative and slash-separated (`internal/queue/overlap.go`),
  even on Windows.
- `*` matches within a single path segment.
- `?` matches a single non-separator character.
- `[abc]` and similar character classes are supported.
- `**` matches across path separators.
- A trailing `/` keeps its current meaning: directory-prefix claim, not a glob.

## Library Choice

Use `github.com/bmatcuk/doublestar/v4` for glob matching. It covers `**`
without introducing transitive dependencies, and it is small enough to justify
the extra import for a correctness-sensitive feature.

## Shared Implementation Boundary

Do not duplicate `affects` logic across packages.

Create a small shared helper package:

```go
// internal/affects/affects.go
package affects

func IsGlob(s string) bool
func StaticPrefix(pattern string) string
func Validate(pattern string) error
func Match(a, b string) bool
func Overlap(a, b []string) []string
```

Why this boundary:

- `internal/frontmatter` needs validation but should not import `internal/queue`.
- `internal/queue` needs matching and overlap checks.
- `PollIndex.HasActiveOverlap()` and backlog deferral must share the same
  matching rules.

If the package feels too broad during implementation, `Validate` can stay in
`internal/frontmatter`, but `Match` and `Overlap` must still have one canonical
implementation.

## Matching Rules

`affects.Match(a, b)` should use conservative conflict detection:

1. **Exact match**: unchanged.
2. **Directory prefix match**: unchanged.
3. **Glob vs concrete path**: use `doublestar.Match`.
4. **Glob vs directory prefix**: compare the glob's static prefix with the
   directory prefix. If the static prefix is empty, assume conflict. If one
   prefix contains the other, assume conflict.
5. **Glob vs glob**: if static prefixes are disjoint, return no match; if they
   overlap or either static prefix is empty, assume conflict.

### Why the conservative rules matter

The important property is **no false negatives** for overlap detection. False
positives only defer work; false negatives allow conflicting tasks to run.

Examples:

| A | B | Result | Reason |
|---|---|--------|--------|
| `main.go` | `main.go` | conflict | Exact match |
| `internal/runner/` | `internal/runner/task.go` | conflict | Prefix |
| `internal/runner/*.go` | `internal/runner/task.go` | conflict | Glob vs concrete |
| `**/*.go` | `internal/` | conflict | Empty static prefix means "could match anywhere" |
| `internal/**/*.go` | `pkg/client/` | no conflict | Static prefixes disjoint |
| `internal/*.go` | `internal/r*.go` | conflict | Overlapping static prefix |

## Validation and Error Handling

Validate glob patterns at parse time in `frontmatter.ParseTaskData()`.

- Valid glob pattern -> parse succeeds.
- Malformed glob pattern -> parse failure, same as malformed YAML today.
- At comparison time, any unexpected `doublestar` error should fall back to
  "no match" as a defensive safety net.

Prefer a dedicated pattern-validation helper over abusing a fake match against
an empty path. The plan should use whatever validation entry point `doublestar`
exposes cleanly; if none exists, a small wrapper can normalize that behavior in
`internal/affects`.

## Queue and Index Integration

### `internal/queue/overlap.go`

Replace the current local matching helpers with calls into `internal/affects`.

- `DeferredOverlappingTasksDetailed()` should call `affects.Overlap()`.
- `overlappingAffects()` can move into `internal/affects` entirely if that
  simplifies reuse.

### `internal/queue/index.go`

Keep the current exact-match fast path, but make the index layout explicit.

Add three active lookup buckets:

```go
type PollIndex struct {
    activeAffectsExact    map[string][]string
    activeAffectsPrefixes []string
    activeAffectsGlobs    []string
    // ...existing fields...
}
```

`HasActiveOverlap()` should check in this order:

1. Exact match map lookup.
2. Prefix comparisons.
3. Glob comparisons against `activeAffectsGlobs` using the same shared matcher.

This keeps glob logic opt-in. Exact-only repos should see the same fast path as
today.

### `ActiveAffects()` and file claims

`ActiveAffects()` should continue returning the raw `affects` strings from task
files. Glob entries must remain visible to downstream consumers rather than
being expanded.

That means `file-claims.json` can now contain:

- exact paths,
- directory prefixes ending in `/`, and
- glob patterns.

The host instructions must explain all three forms.

## Files to Create or Modify

| File | Change |
|------|--------|
| `go.mod` / `go.sum` | Add `github.com/bmatcuk/doublestar/v4` |
| `internal/affects/affects.go` | New shared matching/validation helper |
| `internal/affects/affects_test.go` | Unit tests for matching and validation |
| `internal/frontmatter/frontmatter.go` | Validate glob patterns via shared helper |
| `internal/frontmatter/frontmatter_test.go` | Valid/invalid glob parse tests |
| `internal/queue/overlap.go` | Use shared overlap helper |
| `internal/queue/overlap_test.go` | Extend overlap coverage for globs |
| `internal/queue/index.go` | Add explicit active glob bucket + shared matching |
| `internal/queue/index_test.go` | Add `HasActiveOverlap()` glob coverage |
| `internal/queue/queue_test.go` | Add backlog deferral coverage for globs |
| `internal/messaging/messaging.go` | Update comments or behavior assumptions around raw file claims if needed |
| `internal/messaging/messaging_test.go` | Cover file-claims output containing glob entries |
| `internal/runner/task-instructions.md` | Document exact, prefix, and glob file claims |
| `internal/integration/prompt_test.go` | Validate prompt/docs stay in sync |
| `docs/task-format.md` | Document syntax and path semantics |
| `docs/architecture.md` | Briefly mention glob-aware overlap detection |
| `docs/messaging.md` | Document raw file-claim keys, including globs |
| `.github/skills/mato/SKILL.md` | Teach the planning skill when to use exact paths vs prefixes vs globs |

## Test Plan

### Unit tests (`internal/affects/affects_test.go` or `internal/queue/overlap_test.go`)

| Test | Cases |
|------|-------|
| `TestIsGlob` | `foo.go` false, `*.go` true, `internal/**` true, `pkg/client/` false |
| `TestStaticPrefix` | `internal/runner/*.go`, `**/*.go`, `foo.go`, `internal/**/x.go` |
| `TestMatch` | exact/exact, prefix/file, glob/file, glob/prefix, glob/glob overlapping, glob/glob disjoint |
| `TestMatch_Symmetric` | `Match(a, b) == Match(b, a)` for representative cases |
| `TestOverlap` | mixed exact, prefix, and glob lists |
| `TestValidate` | valid patterns accepted, malformed patterns rejected |

Must include the previously missed cases:

- `**/*.go` vs `internal/` -> conflict
- `internal/**/*.go` vs `internal/` -> conflict
- `internal/**/*.go` vs `pkg/client/` -> no conflict

### Queue/index tests

| Test | Coverage |
|------|----------|
| `TestHasActiveOverlap_GlobAgainstExact` | Active glob, incoming exact path |
| `TestHasActiveOverlap_GlobAgainstPrefix` | Active glob, incoming directory prefix |
| `TestDeferredOverlappingTasks_Glob` | Backlog deferral triggered by glob overlap |
| `TestDeferredOverlappingTasks_ExactFastPath` | Exact-only behavior unchanged |

### Frontmatter tests

| Test | Coverage |
|------|----------|
| `TestParseTaskData_ValidGlob` | Valid glob parses successfully |
| `TestParseTaskData_InvalidGlob` | Malformed glob produces parse failure |

### Messaging/prompt tests

| Test | Coverage |
|------|----------|
| `TestBuildAndWriteFileClaims_PreservesGlobs` | Raw claim keys include glob strings unchanged |
| `TestPromptMentionsGlobClaims` | Embedded task instructions document glob claim semantics |

## Backward Compatibility

This is **mostly backward compatible**, but not literally zero-risk.

### Unchanged behavior

- Existing exact paths still behave the same.
- Existing directory-prefix entries ending in `/` still behave the same.
- Exact-only task files still use the same fast path.

### Intentional compatibility edge cases

- Paths containing literal glob metacharacters such as `*`, `?`, or `[` will now
  be interpreted as patterns.
- Previously tolerated malformed patterns should now fail parse-time validation.

These cases are rare for source-code repos, but they should be documented as a
real behavior change rather than described as "fully backward compatible."

## Implementation Order

1. Add `doublestar` dependency.
2. Create `internal/affects` with `IsGlob`, `StaticPrefix`, `Validate`, and
   `Match`, plus unit tests.
3. Update `internal/frontmatter/frontmatter.go` to validate patterns.
4. Replace queue overlap logic with shared helper usage.
5. Extend `PollIndex` with explicit exact/prefix/glob buckets.
6. Add queue/index tests for active overlap and backlog deferral.
7. Update file-claims messaging and embedded task instructions.
8. Update docs and skill guidance.

## Deferred Questions

1. **Literal escaping**: if literal `[` or `?` paths ever become a real use
   case, add an escaping convention in a later version.
2. **Negated patterns**: useful, but too easy to make confusing in v1.
3. **Pattern visualization**: `mato status` could someday show representative
   matches for a glob, but that would require filesystem expansion and careful
   UX.
