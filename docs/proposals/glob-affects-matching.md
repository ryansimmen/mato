# Glob Pattern Matching for `affects`

## Summary

Add glob pattern support (`*`, `?`, `[...]`, `**`) to the `affects` frontmatter field, enabling tasks to declare file-scope conflicts using wildcards instead of enumerating every path. Uses the `doublestar` library for matching. Fully backward compatible — existing task files are unaffected.

## Motivation

The `affects` field currently supports exact string matching and directory-prefix matching (trailing `/`). This forces users to either:

1. List every file individually — error-prone, stale as files are added/renamed.
2. Use a directory prefix — too coarse; `internal/runner/` blocks any task touching that directory, even if only test files overlap.

Glob patterns close the gap. `internal/runner/*.go` expresses "all Go files in runner" without blocking tasks that only affect `internal/runner/testdata/`. `internal/**/*_test.go` expresses "all test files under internal" — impossible today without listing every one.

## Design

### Library Choice

**`github.com/bmatcuk/doublestar/v4`** — pure Go, zero transitive dependencies, MIT license. Supports `*`, `?`, `[...]`, and `**` (recursive directory matching). Go's stdlib `filepath.Match` lacks `**` support, which is the most useful pattern for multi-level directory trees.

### Matching Rules

The `affectsMatch(a, b string) bool` function gains two new cases, evaluated after the existing exact and prefix checks:

| Case | Method | Example |
|------|--------|---------|
| Exact match | `a == b` | `foo.go` vs `foo.go` |
| Directory prefix | `strings.HasPrefix` | `pkg/client/` vs `pkg/client/http.go` |
| **Glob vs concrete** | `doublestar.Match(pattern, path)` | `internal/runner/*.go` vs `internal/runner/runner.go` |
| **Glob vs glob** | Static prefix overlap check | `internal/*.go` vs `internal/r*.go` → conflict (prefixes overlap) |

Glob-vs-glob matching is **conservative**: if the static prefixes of two patterns share a common ancestor, they are treated as conflicting. This avoids false negatives (merge conflicts) at the cost of occasional false positives (unnecessary deferral). Two globs with disjoint prefixes (`internal/runner/*.go` vs `pkg/client/*.go`) are correctly identified as non-conflicting.

### `isGlob()` Gate

A fast `isGlob()` check prevents glob processing unless metacharacters are present. If neither entry is a glob, all code paths are identical to today.

**Brace expansion note:** `doublestar` supports `{a,b}` syntax (e.g., `internal/{runner,queue}/*.go`). The `isGlob()` function must check for `{` in addition to `*`, `?`, and `[` to correctly identify all patterns that `doublestar` will interpret.

### Parse-Time Validation

Invalid glob patterns (e.g., `internal/[bad`) are caught in `ParseTaskData()` before the task enters the queue. This matches the existing behavior for malformed YAML — the task fails to parse and is moved to `failed/`. At comparison time, `doublestar.Match` errors silently result in no-match as defense in depth.

### Fast-Path Preservation

The current `overlappingAffects()` uses O(n+m) hash-map intersection when neither list contains prefix entries. The scan loop is extended to detect glob entries, and the fast path is used **only** when neither list has prefixes nor globs. Since most tasks use exact paths, the common case remains O(n+m).

### `PollIndex` Changes

`BuildIndex()` in `index.go` currently partitions active affects entries during the index build loop (lines 196-209) into two structures:

- `activeAffects map[string][]string` — exact path entries, keyed for O(1) lookup.
- `activeAffectsPrefixes []string` — directory prefix entries (trailing `/`), stored in a slice for linear scan.

A third structure is added:

```go
activeAffectsGlobs []string // glob patterns from active tasks
```

During the index build, the existing `isDirPrefix(af)` check is extended:

```go
for _, af := range meta.Affects {
    idx.activeAffects[af] = append(idx.activeAffects[af], name)
    if isDirPrefix(af) {
        if _, ok := activeAffectsPrefixSet[af]; !ok {
            activeAffectsPrefixSet[af] = struct{}{}
            idx.activeAffectsPrefixes = append(idx.activeAffectsPrefixes, af)
        }
    } else if isGlob(af) {
        if _, ok := activeAffectsGlobSet[af]; !ok {
            activeAffectsGlobSet[af] = struct{}{}
            idx.activeAffectsGlobs = append(idx.activeAffectsGlobs, af)
        }
    }
}
```

`HasActiveOverlap()` gains a third check phase after exact lookup and prefix scan:

```go
// Check if af matches any active glob pattern.
for _, g := range idx.activeAffectsGlobs {
    if affectsMatch(g, af) {
        return true
    }
}
```

This is O(g) where g is the number of active glob entries — acceptable since globs are expected to be rare.

## API Changes

### New Functions in `internal/queue/overlap.go`

```go
// isGlob reports whether s contains glob metacharacters.
func isGlob(s string) bool {
    return strings.ContainsAny(s, "*?[{")
}

// staticPrefix returns the longest directory path before the first glob
// metacharacter. Returns the full string if no metacharacters are present.
func staticPrefix(pattern string) string {
    for i, c := range pattern {
        if c == '*' || c == '?' || c == '[' || c == '{' {
            return pattern[:strings.LastIndex(pattern[:i], "/")+1]
        }
    }
    return pattern
}
```

### Modified `affectsMatch()`

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

### Modified `overlappingAffects()` Scan Loop

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
    // existing O(n+m) hash-map intersection
}
// else: pairwise comparison (unchanged)
```

### Modified `ParseTaskData()` in `internal/frontmatter/frontmatter.go`

After the existing `filterEmpty(meta.Affects)` call:

```go
for _, af := range meta.Affects {
    if isGlob(af) {
        if _, err := doublestar.Match(af, ""); err != nil {
            return TaskMeta{}, "", fmt.Errorf("invalid glob in affects %q: %w", af, err)
        }
    }
}
```

Note: `isGlob` is defined in `internal/queue/overlap.go`. Either expose it as an exported function, or extract a small shared helper (e.g., in a `internal/affects/` package or by adding the check inline in frontmatter). The simplest approach is to inline the `strings.ContainsAny(af, "*?[{")` check directly in `ParseTaskData()`.

## Test Plan

### Unit Tests — `internal/queue/overlap_test.go`

| Test | Cases |
|------|-------|
| `TestIsGlob` | `foo.go` → false, `*.go` → true, `internal/**` → true, `pkg/client/` → false, `data[1].csv` → true, `internal/{runner,queue}/*.go` → true |
| `TestStaticPrefix` | `internal/runner/*.go` → `internal/runner/`, `**/*.go` → `""`, `foo.go` → `foo.go`, `internal/{a,b}/*.go` → `internal/` |
| `TestAffectsMatch` (new rows) | Glob vs exact match, glob vs exact no-match, `**` pattern, glob vs directory prefix (`internal/runner/*.go` vs `internal/runner/`), glob vs glob overlapping prefixes, glob vs glob disjoint prefixes |
| `TestAffectsMatch_Symmetry` | Verify `affectsMatch(a, b) == affectsMatch(b, a)` for all test rows including new glob cases |
| `TestOverlappingAffects` (new rows) | Mixed lists with globs, exact paths, and prefixes |
| `TestAffectsMatch_GlobVsDirPrefix` | Explicit test: `affects: ["internal/runner/"]` vs `affects: ["internal/runner/*.go"]` — must conflict via the existing prefix check since `internal/runner/*.go` has prefix `internal/runner/` |

### Unit Tests — `internal/frontmatter/frontmatter_test.go`

| Test | Cases |
|------|-------|
| `TestParseTaskData_ValidGlob` | `affects: ["internal/**/*.go"]` parses successfully |
| `TestParseTaskData_InvalidGlob` | `affects: ["internal/[bad"]` returns descriptive error |

### Index Tests — `internal/queue/index_test.go` or `queue_test.go`

| Test | Cases |
|------|-------|
| `TestHasActiveOverlap_Globs` | Active task with glob affects, query with matching concrete path |
| `TestDeferredOverlapping_Glob` | Two backlog tasks with glob-overlapping affects, verify correct deferral |

### Integration Tests — `internal/integration/`

| Test | Scenario |
|------|----------|
| End-to-end glob deferral | Two tasks with glob affects, verify lower-priority task is deferred |
| Mixed glob/exact overlap | One task with exact path, one with glob that matches it |

## Backward Compatibility

- **Zero migration required.** Existing task files have no glob metacharacters → `isGlob()` returns false → all code paths identical to today.
- **No format changes.** The `affects` field syntax is extended, not changed.
- **Fast path preserved.** Exact-only lists still use O(n+m) hash-map intersection.
- **Known limitation:** Paths containing literal `[`, `?`, `*`, or `{` characters are misinterpreted as globs. This is rare in source code paths and documented. A future version can add an escape mechanism if needed.

## Dependencies

**Add:** `github.com/bmatcuk/doublestar/v4` (latest stable)

- Pure Go, zero transitive dependencies
- MIT license
- Used in `internal/queue/overlap.go` and `internal/frontmatter/frontmatter.go`

## Effort Estimate

| Task | Effort |
|------|--------|
| Add `doublestar` dependency | 10 min |
| Implement `isGlob()`, `staticPrefix()` + tests | 45 min |
| Extend `affectsMatch()` + `overlappingAffects()` + tests | 1.5 hours |
| Extend `PollIndex` build + `HasActiveOverlap()` + tests | 1.5 hours |
| Add glob validation to `ParseTaskData()` + tests | 45 min |
| Integration tests | 1 hour |
| Update documentation | 45 min |
| Buffer for edge cases and review | 45 min |
| **Total** | **~1 day** |

## Implementation Order

1. **Add dependency.** `go get github.com/bmatcuk/doublestar/v4`. Verify build: `go build ./...`. Self-contained, no code changes.

2. **Implement `isGlob()` and `staticPrefix()` with unit tests.** Add both functions to `internal/queue/overlap.go`. Add `TestIsGlob` and `TestStaticPrefix` table-driven tests. Verify: `go test -race ./internal/queue/...`. Self-contained — nothing calls these yet.

3. **Extend `affectsMatch()` with glob matching.** Add the glob-vs-concrete and glob-vs-glob cases after the existing prefix checks. Add new rows to `TestAffectsMatch` covering glob-vs-exact, glob-vs-prefix, glob-vs-glob (overlapping and disjoint), and `**` patterns. Add explicit `TestAffectsMatch_GlobVsDirPrefix` test. Verify symmetry. Verify: `go test -race ./internal/queue/...`. Self-contained — `overlappingAffects` delegates to `affectsMatch`.

4. **Update `overlappingAffects()` fast-path guard.** Extend the scan loop to detect glob entries via `isGlob()`. Gate the hash-map fast path on `!hasGlobA && !hasGlobB` in addition to the existing prefix check. Add new rows to `TestOverlappingAffects` with mixed glob/exact/prefix lists. Verify: `go test -race ./internal/queue/...`.

5. **Extend `PollIndex` with `activeAffectsGlobs`.** Add the `activeAffectsGlobs []string` field to `PollIndex`. Update `BuildIndex()` to partition glob entries during the index build loop (alongside the existing prefix partitioning at `index.go:200-209`). Update `HasActiveOverlap()` to check incoming entries against active globs. Add index-level tests. Verify: `go test -race ./internal/queue/...`.

6. **Add glob validation to `ParseTaskData()`.** Validate glob entries after `filterEmpty(meta.Affects)`. Invalid patterns cause a parse error. Add `TestParseTaskData_ValidGlob` and `TestParseTaskData_InvalidGlob`. Verify: `go test -race ./internal/frontmatter/...`.

7. **Write integration tests.** Add end-to-end tests in `internal/integration/` covering glob-based deferral and mixed glob/exact overlap scenarios. Verify: `go test -race ./internal/integration/...`.

8. **Update documentation.** Update `docs/task-format.md` to document glob syntax with examples and a pattern syntax table. Add brief mention in `docs/architecture.md`. Update `.github/skills/mato/SKILL.md` if task generation guidance changes. Full verification: `go build ./... && go vet ./... && go test -count=1 ./...`.

## Open Questions

1. **Glob negation.** `!internal/test_*.go` (exclude patterns) could be useful but adds complexity. Defer until a concrete use case arises.
2. **Glob expansion in `mato status`.** Should `mato status` show which concrete files a glob would match? Useful for debugging but requires filesystem access at display time. Consider as a follow-up.
3. **Literal escape mechanism.** For the rare case of paths containing `[`, `?`, `*`, or `{` characters. `doublestar` supports backslash escaping; a future version could document this convention.
