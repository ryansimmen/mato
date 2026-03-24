# Glob Affects Matching — Implementation Plan

> **Status: Implemented** — This proposal has been fully implemented.
> The text below describes the original design; see the source code for
> the current implementation.

## Summary

Extend `affects:` matching so task files can express glob patterns such as
`internal/runner/*.go` and `internal/**/*_test.go`, while preserving the
current exact-path and trailing-`/` directory-prefix semantics.

Estimated effort: ~1 day.

## Goals

- Reduce false negatives in overlap detection without changing the task file
  format.
- Preserve the current O(n+m) fast path for exact-only `affects` lists.
- Keep glob patterns stored as-is in task files; never expand them to concrete
  file lists at parse time.

## Non-Goals

- No regex support.
- No glob negation (`!foo/**`) in v1.
- No filesystem expansion or "show me everything this glob matches" feature.

## Supported Syntax

`affects:` continues to support three forms:

| Form | Meaning | Example |
|------|---------|---------|
| Exact path | One file path | `internal/runner/runner.go` |
| Directory prefix | Entire subtree (trailing `/`) | `internal/runner/` |
| Glob pattern | Pattern match using metacharacters | `internal/runner/*.go` |

Glob metacharacters:

- `*` matches within a single path segment.
- `?` matches a single non-separator character.
- `[abc]` character classes.
- `**` matches across path separators (recursive).
- `{a,b}` brace expansion (supported by `doublestar`).

A trailing `/` retains its current meaning: directory-prefix claim, not a glob.
Combining glob metacharacters with a trailing `/` (e.g., `internal/*/`) is
invalid and rejected at parse time — see Parse-time validation below.

All `affects` patterns are repo-relative and use `/` as the path separator
regardless of host OS. This is consistent with the use of `doublestar.Match`
(not `doublestar.PathMatch`, which is OS-aware).

## Library Choice

Use `github.com/bmatcuk/doublestar/v4`. Pure Go, zero transitive dependencies,
MIT license. Go's stdlib `filepath.Match` lacks `**` support, which is the most
useful pattern for multi-level directory trees.

## Implementation Boundary

Keep matching logic in `internal/queue/overlap.go` — this is already the
canonical location for affects comparison. Add a `staticPrefix` helper and an
`isGlob` alias alongside the existing `affectsMatch` and `isDirPrefix`.

To avoid duplicating the glob metacharacter set (`*?[{`) in both
`internal/queue/` and `internal/frontmatter/`, define `IsGlob` as an exported
function in `internal/frontmatter/` (where it naturally belongs alongside
affects parsing) and have `internal/queue/` call it. The frontmatter package is
a leaf dependency that queue already imports, so this adds no new dependency
edges. `internal/queue/overlap.go` can define a local `isGlob = frontmatter.IsGlob`
alias for readability.

## Matching Rules

`affectsMatch(a, b)` gains three new cases after the existing exact and prefix
checks:

| Case | Method |
|------|--------|
| Exact match | `a == b` (unchanged) |
| Directory prefix | `strings.HasPrefix` (unchanged) |
| Glob vs concrete path | `doublestar.Match(pattern, path)` |
| Glob vs directory prefix | Static prefix overlap check (explicit; `doublestar.Match` expects concrete file paths, not directory markers) |
| Glob vs glob | Static prefix overlap check (conservative) |

### Conservative conflict detection

The important property is **no false negatives**. False positives (unnecessary
deferral) are cheap; false negatives (merge conflicts) are expensive.

For glob-vs-glob, two patterns cannot be directly compared by `doublestar.Match`
(it requires one pattern and one concrete path). Instead, compare static
prefixes: if they share a common ancestor, assume conflict.

| A | B | Result | Reason |
|---|---|--------|--------|
| `main.go` | `main.go` | conflict | Exact match |
| `internal/runner/` | `internal/runner/task.go` | conflict | Prefix |
| `internal/runner/*.go` | `internal/runner/task.go` | conflict | Glob vs concrete |
| `internal/runner/*.go` | `internal/runner/` | conflict | Glob prefix starts with dir prefix |
| `**/*.go` | `internal/` | conflict | Empty static prefix = could match anywhere |
| `internal/**/*.go` | `pkg/client/` | no conflict | Static prefixes disjoint |
| `internal/*.go` | `internal/r*.go` | conflict | Overlapping static prefix |

## API Changes

### New helpers in `internal/queue/overlap.go`

```go
// isGlob is an alias for frontmatter.IsGlob, kept local for readability.
var isGlob = frontmatter.IsGlob
```

### New helper in `internal/frontmatter/frontmatter.go`

```go
// IsGlob reports whether s contains glob metacharacters.
// Checks for *, ?, [, and { because doublestar supports brace expansion.
func IsGlob(s string) bool {
	return strings.ContainsAny(s, "*?[{")
}
```

### `staticPrefix` in `internal/queue/overlap.go`

```go
// staticPrefix returns the longest directory path before the first glob
// metacharacter. Returns the full string if no metacharacters are present.
//
// Note: patterns like {a,b/c} where braces contain a "/" will return a
// prefix that doesn't fully capture the matching scope. This is acceptable
// because the result is used for conservative conflict detection — a shorter
// prefix only produces false positives (unnecessary deferral), never false
// negatives.
func staticPrefix(pattern string) string {
	for i, c := range pattern {
		if c == '*' || c == '?' || c == '[' || c == '{' {
			return pattern[:strings.LastIndex(pattern[:i], "/")+1]
		}
	}
	return pattern
}
```

### Extended `affectsMatch()`

```go
func affectsMatch(a, b string) bool {
	if a == b {
		return true
	}
	if strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) {
		return true
	}
	if strings.HasSuffix(b, "/") && strings.HasPrefix(a, b) {
		return true
	}

	aGlob, bGlob := isGlob(a), isGlob(b)

	// Glob vs directory prefix: compare the glob's static prefix against the
	// directory prefix. We cannot rely on doublestar.Match here because it
	// expects concrete file paths, not directory markers with trailing "/".
	if aGlob && isDirPrefix(b) {
		pa := staticPrefix(a)
		if pa == "" {
			return true // empty prefix could match anywhere
		}
		return strings.HasPrefix(pa, b) || strings.HasPrefix(b, pa)
	}
	if bGlob && isDirPrefix(a) {
		pb := staticPrefix(b)
		if pb == "" {
			return true
		}
		return strings.HasPrefix(pb, a) || strings.HasPrefix(a, pb)
	}

	if aGlob && bGlob {
		pa, pb := staticPrefix(a), staticPrefix(b)
		if pa == "" || pb == "" {
			return true // empty prefix could match anything
		}
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	}
	if aGlob {
		matched, _ := doublestar.Match(a, b)
		return matched
	}
	if bGlob {
		matched, _ := doublestar.Match(b, a)
		return matched
	}

	return false
}
```

### Extended `overlappingAffects()` scan loop

The fast path (hash-map intersection) is used only when neither list has
prefixes nor globs:

```go
aClean := make([]string, 0, len(a))
hasPrefixA, hasGlobA := false, false
for _, item := range a {
	if item == "" {
		continue
	}
	aClean = append(aClean, item)
	if isDirPrefix(item) {
		hasPrefixA = true
	}
	if isGlob(item) {
		hasGlobA = true
	}
}
// ... same for b ...

if !hasPrefixA && !hasPrefixB && !hasGlobA && !hasGlobB {
	// existing O(n+m) hash-map intersection (unchanged)
}
// else: pairwise comparison via affectsMatch (unchanged)
```

### `PollIndex` changes in `internal/queue/index.go`

Add a third field alongside `activeAffects` and `activeAffectsPrefixes`:

```go
type PollIndex struct {
	activeAffects         map[string][]string // all affects entries (exact, prefix, and glob)
	activeAffectsPrefixes []string            // dir prefixes (trailing /)
	activeAffectsGlobs    []string            // glob patterns (new)
	// ...existing fields...
}
```

During the index build loop (where `activeAffects` is populated and
`activeAffectsPrefixes` is filled), partition glob entries into the new field.
Note that glob entries are intentionally stored in both `activeAffects` (so two
identical glob strings match via the exact-lookup fast path) and
`activeAffectsGlobs` (for pattern-based comparison against concrete paths):

```go
for _, af := range meta.Affects {
	idx.activeAffects[af] = append(idx.activeAffects[af], name)
	if isDirPrefix(af) {
		// existing prefix handling
	} else if isGlob(af) {
		if _, ok := activeAffectsGlobSet[af]; !ok {
			activeAffectsGlobSet[af] = struct{}{}
			idx.activeAffectsGlobs = append(idx.activeAffectsGlobs, af)
		}
	}
}
```

`HasActiveOverlap()` gains two new check phases after the existing exact lookup
and prefix scans. First, active glob patterns are checked against the incoming
entry. Second, when the incoming `af` is itself a glob, it is tested against
all active keys in `activeAffects` — the exact-match lookup only catches cases
where an active task declares the identical glob string, so this iteration is
needed to find active concrete paths and prefixes that the incoming glob would
match:

```go
for _, g := range idx.activeAffectsGlobs {
	if affectsMatch(g, af) {
		return true
	}
}
// If af is a glob, the exact-match lookup above only catches the
// literal string; we must also test it against every active key.
// No separate activeAffectsPrefixes loop is needed here because
// prefix entries are already stored in activeAffects.
if isGlob(af) {
	for key := range idx.activeAffects {
		if affectsMatch(af, key) {
			return true
		}
	}
}
```

Note: `isGlob` (aliased from `frontmatter.IsGlob`) and `isDirPrefix` are both
in `internal/queue/overlap.go`, so the index build can call them directly — no
cross-package import needed.

### Parse-time validation in `internal/frontmatter/frontmatter.go`

After the existing `filterEmpty(meta.Affects)` call, validate glob entries
using the `IsGlob` helper defined in the same package:

```go
for _, af := range meta.Affects {
	if IsGlob(af) {
		if strings.HasSuffix(af, "/") {
			return TaskMeta{}, "", fmt.Errorf("affects %q combines glob syntax with trailing /; use a glob pattern without trailing / or a plain directory prefix", af)
		}
		if _, err := doublestar.Match(af, ""); err != nil {
			return TaskMeta{}, "", fmt.Errorf("invalid glob in affects %q: %w", af, err)
		}
	}
}
```

This requires adding `doublestar` as an import in `frontmatter.go` — a small
but acceptable cost for parse-time validation. The alternative (deferring
validation to comparison time) risks malformed patterns sitting undetected in
the queue.

### File-claims impact

`BuildAndWriteFileClaims()` in `internal/messaging/` collects raw `affects`
strings from active tasks. Glob entries will appear in `file-claims.json`
as-is — no expansion. The embedded task instructions
(`internal/runner/task-instructions.md`) should instruct agents to treat a
file as potentially claimed if it matches a glob-pattern key, not just exact
or prefix keys. This is a documentation-only change to messaging.

## Files to Modify

| File | Change |
|------|--------|
| `go.mod` / `go.sum` | Add `github.com/bmatcuk/doublestar/v4` |
| `internal/queue/overlap.go` | Add `isGlob` alias, `staticPrefix()`, extend `affectsMatch()` and `overlappingAffects()` |
| `internal/queue/overlap_test.go` | Add glob matching test cases |
| `internal/queue/index.go` | Add `activeAffectsGlobs` field, update index build and `HasActiveOverlap()` |
| `internal/queue/index_test.go` | Add `HasActiveOverlap()` glob coverage |
| `internal/frontmatter/frontmatter.go` | Add `IsGlob()`, add glob validation in `ParseTaskData()` |
| `internal/frontmatter/frontmatter_test.go` | Valid/invalid glob parse tests |
| `docs/task-format.md` | Document syntax, examples, and when-to-use-what table |
| `docs/architecture.md` | Rewrite line 298 ("mato does not interpret it as globs") to reflect glob support; add brief description of glob-aware overlap detection |
| `internal/runner/task-instructions.md` | Update agent prompt so agents treat a file as potentially claimed if it matches a glob-pattern key, not just exact or prefix keys (line 93 currently says "exact file paths or directory prefixes") |
| `internal/integration/prompt_test.go` | Add glob-claims prompt validation test (required by `AGENTS.md:148`: changes to `task-instructions.md` need prompt validation tests; existing `TestPromptFileClaimsMentionDirectoryPrefixes` at line 141 only covers directory prefixes) |
| `docs/messaging.md` | Update file-claims documentation to describe glob-pattern keys and instruct that a file is claimed if it matches any key — exact, prefix, or glob (line 87 currently says "file paths" and "directory-prefix claims") |
| `docs/configuration.md` | Update `MATO_FILE_CLAIMS` description (line 69) which uses prefix-only language; add glob pattern mention |
| `.github/skills/mato/SKILL.md` | Update duplicated task format spec to document glob syntax in `affects:` (required: `docs/task-format.md:3` sync comment and `AGENTS.md:157` both mandate keeping this in sync with task format changes) |

## Test Plan

### Unit tests (`internal/queue/overlap_test.go`)

| Test | Cases |
|------|-------|
| `TestStaticPrefix` | `internal/runner/*.go` → `internal/runner/`, `**/*.go` → `""`, `foo.go` → `foo.go`, `internal/{a,b}/*.go` → `internal/` |
| `TestAffectsMatch` (new rows) | Glob vs exact match, glob vs exact no-match, `**` pattern, glob vs glob overlapping, glob vs glob disjoint |
| `TestAffectsMatch_GlobVsDirPrefix` | `**/*.go` vs `internal/` — must conflict via static prefix check (existing prefix check misses this because the glob text doesn't start with `internal/`); also `internal/runner/*.go` vs `internal/runner/` — must conflict (caught by existing prefix check); also `internal/*.go` vs `pkg/` — no conflict (disjoint static prefix) |
| `TestAffectsMatch_Symmetry` | `affectsMatch(a, b) == affectsMatch(b, a)` for all test rows |
| `TestAffectsMatch_MalformedGlob` | Malformed pattern that bypasses parse-time validation returns `false`, not panic |
| `TestOverlappingAffects` (new rows) | Mixed lists with globs, exact paths, and prefixes |

### Frontmatter tests (`internal/frontmatter/frontmatter_test.go`)

| Test | Cases |
|------|-------|
| `TestIsGlob` | `foo.go` false, `*.go` true, `internal/**` true, `pkg/client/` false, `data[1].csv` true, `internal/{a,b}/*.go` true |
| `TestParseTaskData_ValidGlob` | `affects: ["internal/**/*.go"]` parses successfully |
| `TestParseTaskData_InvalidGlob` | `affects: ["internal/[bad"]` returns descriptive error |
| `TestParseTaskData_GlobWithTrailingSlash` | `affects: ["internal/*/"]` rejected with descriptive error (glob + trailing `/` ambiguity) |

### Index tests (`internal/queue/index_test.go`)

| Test | Cases |
|------|-------|
| `TestHasActiveOverlap_GlobVsExact` | Active glob, incoming exact path |
| `TestHasActiveOverlap_GlobVsPrefix` | Active glob, incoming directory prefix |
| `TestHasActiveOverlap_IncomingGlobVsExact` | Active exact path, incoming glob pattern |
| `TestHasActiveOverlap_IncomingGlobVsPrefix` | Active directory prefix, incoming glob pattern |
| `TestHasActiveOverlap_GlobVsGlob` | Active glob, incoming glob — overlapping static prefix (conflict) and disjoint static prefix (no conflict) |
| `TestHasActiveOverlap_IncomingDoublestar` | Active exact or prefix, incoming `**/*.go` pattern (empty static prefix = always conflicts) |
| `TestHasActiveOverlap_ExactFastPath` | Exact-only behavior unchanged |

### Integration tests (`internal/integration/`)

| Test | Scenario |
|------|----------|
| Glob deferral | Two tasks with glob affects, verify deferral |
| Mixed overlap | One task exact path, one task glob, overlapping |
| `TestPromptFileClaimsMentionGlobPatterns` | Assert that `task-instructions.md` explains glob-pattern file-claim keys and instructs agents to treat a file as claimed when it matches a glob key (mirrors existing `TestPromptFileClaimsMentionDirectoryPrefixes` at `prompt_test.go:141`) |

## Backward Compatibility

Mostly backward compatible, with one documented edge case.

**Unchanged behavior:**

- Existing exact paths still use the same code path and fast path.
- Directory-prefix entries (trailing `/`) still use the same code path.
- Task files with no glob metacharacters follow identical logic.

**Edge case (documented, not fixed in v1):**

Paths containing literal `[`, `?`, `*`, or `{` characters (e.g.,
`data/file[1].csv`) will now be interpreted as glob patterns. This is rare
in source code repositories. A future version can add an escape mechanism
if needed.

## Effort Estimate

| Task | Effort |
|------|--------|
| Add `doublestar` dependency | 10 min |
| Implement `isGlob()`, `staticPrefix()` + tests | 45 min |
| Extend `affectsMatch()` + `overlappingAffects()` + tests | 1.5 hours |
| Extend `PollIndex` build + `HasActiveOverlap()` + tests | 1.5 hours |
| Add glob validation to `ParseTaskData()` + tests | 30 min |
| Integration tests | 1 hour |
| Update documentation | 45 min |
| Buffer for edge cases | 30 min |
| **Total** | **~1 day** |

## Implementation Order

1. **Add dependency.** `go get github.com/bmatcuk/doublestar/v4`. Verify:
   `go build ./...`.

2. **Add `IsGlob()` and `staticPrefix()` with tests.** Add `IsGlob` to
   `internal/frontmatter/frontmatter.go`. Add `staticPrefix` and the
   `isGlob = frontmatter.IsGlob` alias to `internal/queue/overlap.go`. Add
   `TestIsGlob` to `frontmatter_test.go` and `TestStaticPrefix` to
   `overlap_test.go`.
   Verify: `go test -race ./internal/queue/... ./internal/frontmatter/...`.

3. **Extend `affectsMatch()` with glob cases and tests.** Add glob-vs-concrete,
   glob-vs-glob, and glob-vs-prefix logic. Add new rows to `TestAffectsMatch`,
   add `TestAffectsMatch_GlobVsDirPrefix`, verify symmetry. Verify:
   `go test -race ./internal/queue/...`.

4. **Update `overlappingAffects()` fast-path guard.** Extend the scan loop to
   detect globs. Gate fast path on `!hasGlobA && !hasGlobB`. Add new rows to
   `TestOverlappingAffects`. Verify: `go test -race ./internal/queue/...`.

5. **Extend `PollIndex` with `activeAffectsGlobs`.** Add the field, update
   index build, update `HasActiveOverlap()`. Add index-level tests. Verify:
   `go test -race ./internal/queue/...`.

6. **Add glob validation to `ParseTaskData()`.** Use the `IsGlob` helper
   (defined in step 2) + `doublestar.Match` validation. Add frontmatter
   tests. Verify: `go test -race ./internal/frontmatter/...`.

7. **Integration tests.** Glob-based deferral and mixed overlap scenarios.
   Verify: `go test -race ./internal/integration/...`.

8. **Documentation.** Update `docs/task-format.md` with syntax table and
   examples. Brief mention in `docs/architecture.md`. Full verification:
   `go build ./... && go vet ./... && go test -count=1 ./...`.

## Open Questions

1. **Glob negation.** `!internal/test_*.go` — useful but easy to make confusing.
   Defer until a concrete use case arises.
2. **Literal escape mechanism.** For paths containing `[`, `?`, `*`, or `{`.
   `doublestar` supports backslash escaping; document this if the edge case
   becomes real.
3. **Pattern visualization.** `mato status` could show representative matches
   for a glob, but that requires filesystem expansion and careful UX. Defer.
