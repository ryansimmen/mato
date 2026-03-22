# Glob Affects Matching — Multi-Agent Debate & Consensus Plan

## Participants

- **Claude Opus 4.6** — Emphasizes correctness, backward compatibility, and minimal API surface
- **GPT-5.4** — Emphasizes practical developer ergonomics and incremental adoption
- **Gemini 3.1 Pro** — Emphasizes performance optimization and operator experience

---

## Round 1: Pattern Syntax & Library Choice

### Claude Opus 4.6

The current `affects` field supports two modes: exact string match (`foo.go` == `foo.go`) and directory prefix (`pkg/client/` matches `pkg/client/http.go`). Both work well but don't scale — users who affect an entire package must either list every file or use the directory prefix, which is too coarse.

I propose supporting **standard glob syntax** using Go's `filepath.Match` semantics, plus `**` for recursive matching via a small dependency like `doublestar`. The key design constraint: **no regex**. Glob patterns are familiar to every developer (`.gitignore`, shell expansion, GitHub CODEOWNERS). Regex would be overpowered and error-prone in YAML frontmatter.

Specifically:
- `*` — matches any sequence of non-separator characters
- `?` — matches a single non-separator character
- `[abc]` — character class
- `**` — matches zero or more path segments (requires `doublestar`)

### GPT-5.4

I agree with glob syntax over regex. However, I'd push back on adding `doublestar` as a dependency. Go's stdlib `filepath.Match` doesn't support `**`, but the directory prefix syntax (`internal/runner/`) already covers the "everything under here" use case. Adding a dependency introduces supply-chain risk for marginal gain.

Counter-proposal: use `filepath.Match` for single-segment globs (`internal/runner/*.go`) and keep the existing trailing-`/` convention for recursive matching. This covers 95% of use cases with zero new dependencies.

### Gemini 3.1 Pro

I disagree with GPT here. The trailing `/` convention and `filepath.Match` have a gap: you can't express "all Go files under `internal/`" — `internal/` matches everything, and `internal/*.go` only matches one level deep. `internal/**/*.go` is the natural way to express this. The `doublestar` library (github.com/bmatcuk/doublestar) is small, well-maintained, and has zero transitive dependencies. It's the right tool.

I'd also add: we should **not** expand globs to concrete file lists at parse time. Expansion is fragile (depends on filesystem state) and violates the "filesystem as state" principle. Globs should be stored as-is in the task file and compared at overlap-detection time.

### Claude Opus 4.6 (response)

Gemini's point about the gap is valid. The `doublestar` library is a reasonable dependency — it's pure Go, no transitive deps, and well-tested. I'll accept it.

### **Round 1 Consensus**: Use `doublestar.Match` for glob matching. Support `*`, `?`, `[...]`, and `**`. No regex. Globs stored as-is in task files, matched at comparison time. One new dependency: `github.com/bmatcuk/doublestar/v4`.

---

## Round 2: API Design — Where and How to Match

### Claude Opus 4.6

The matching logic lives in `internal/queue/overlap.go`. Currently, `affectsMatch(a, b string) bool` handles exact and prefix matching. I propose extending it to handle globs:

```go
func affectsMatch(a, b string) bool {
    // 1. Exact match (unchanged)
    if a == b { return true }

    // 2. Directory prefix (unchanged)
    if isDirPrefix(a) && strings.HasPrefix(b, a) { return true }
    if isDirPrefix(b) && strings.HasPrefix(a, b) { return true }

    // 3. Glob match (new)
    if isGlob(a) {
        if matched, _ := doublestar.Match(a, b); matched { return true }
    }
    if isGlob(b) {
        if matched, _ := doublestar.Match(b, a); matched { return true }
    }

    return false
}
```

Add a helper `isGlob(s string) bool` that checks for `*`, `?`, or `[` characters. This preserves the existing fast paths — if neither entry is a glob, we never call `doublestar.Match`.

### GPT-5.4

That's clean, but there's a subtle issue: **glob-vs-glob matching**. If Task A affects `internal/runner/*.go` and Task B affects `internal/runner/r*.go`, should they conflict? Neither is a concrete path, so `doublestar.Match(a, b)` won't match (it expects one pattern and one concrete path).

I propose: when both entries are globs, treat them as **potentially conflicting** (return true). This is the conservative approach — false positives (unnecessary deferral) are better than false negatives (merge conflicts). We can refine later with a `hasOverlappingPrefix` heuristic.

### Gemini 3.1 Pro

GPT's conservative approach is correct for now. But we should add a **prefix comparison** optimization: if the static prefixes of two globs don't share a common ancestor, they can't possibly overlap. For example:
- `internal/runner/*.go` prefix is `internal/runner/`
- `pkg/client/*.go` prefix is `pkg/client/`
- These can't overlap — skip the expensive comparison.

Only when prefixes overlap (or one is empty) do we fall through to the conservative "assume conflict" behavior.

```go
func staticPrefix(pattern string) string {
    // Return the longest path prefix before the first glob metacharacter
    for i, c := range pattern {
        if c == '*' || c == '?' || c == '[' {
            return pattern[:strings.LastIndex(pattern[:i], "/")+1]
        }
    }
    return pattern
}
```

### Claude Opus 4.6 (response)

Both good points. The final `affectsMatch` should be:
1. Exact match
2. Directory prefix match
3. One glob, one concrete: `doublestar.Match`
4. Both globs: static prefix comparison → assume conflict if prefixes overlap

### **Round 2 Consensus**: Extend `affectsMatch()` with glob support. Add `isGlob()` and `staticPrefix()` helpers. Glob-vs-glob: use prefix comparison, assume conflict if prefixes overlap. Conservative — no false negatives.

---

## Round 3: Fast Path Preservation

### GPT-5.4

Performance matters here. The current code has a crucial optimization in `overlappingAffects()`: when neither list contains prefix entries, it uses O(n+m) hash-map intersection instead of O(n×m) pairwise comparison. We need to preserve this fast path.

I propose extending the scan loop that detects prefix entries to also detect glob entries:

```go
hasGlobA := false
for _, item := range a {
    if isDirPrefix(item) { hasPrefixA = true }
    if isGlob(item) { hasGlobA = true }
}
```

The fast path (hash-map intersection) is used **only** when neither list has prefixes **nor** globs. If either list has a glob, we fall through to pairwise comparison.

### Claude Opus 4.6

Agreed. The key insight is that the fast path handles the common case (exact file paths like `main.go`, `internal/runner/runner.go`). Globs are opt-in and expected to be less common. Most task files will still use exact paths and directory prefixes. The performance impact of adding glob support to existing workflows is **zero** — globs only trigger the slow path when actually present.

### Gemini 3.1 Pro

One additional optimization: in `HasActiveOverlap()` on the `PollIndex`, we should add a third index for glob patterns (alongside `activeAffects` map and `activeAffectsPrefixes` slice). This keeps index lookups fast:

```go
type PollIndex struct {
    activeAffects         map[string][]string  // exact paths
    activeAffectsPrefixes []string             // dir prefixes
    activeAffectsGlobs    []string             // glob patterns (new)
}
```

When checking overlap, first do the O(1) exact lookup, then check prefixes, then check globs. The glob check is always O(n) where n is the number of active glob entries — acceptable since globs are rare.

### **Round 3 Consensus**: Preserve O(n+m) fast path for exact-only lists. Add glob detection to the scan loop. Extend `PollIndex` with `activeAffectsGlobs` field. Glob checks are O(n) against active globs — acceptable given expected rarity.

---

## Round 4: Backward Compatibility

### Claude Opus 4.6

This is non-negotiable. Every existing task file must behave identically after this change. The compatibility contract:

1. **Exact paths** (`foo.go`) — unchanged. No glob characters → no glob matching.
2. **Directory prefixes** (`pkg/client/`) — unchanged. Trailing `/` triggers prefix logic before glob logic.
3. **New: glob patterns** (`internal/runner/*.go`) — opt-in. Only triggers when glob metacharacters are present.

The `isGlob()` function is the gate. If it returns false, the code path is identical to today.

### GPT-5.4

Agreed. One edge case to call out: a path containing a literal `[` or `?` character (e.g., `data/file[1].csv`). Today these are treated as exact strings. After this change, `isGlob()` would flag them as globs, and `doublestar.Match` might interpret them as character classes.

This is a **real but rare** edge case. Most source code paths don't contain `[`, `?`, or `*`. We should document it as a known limitation and consider adding a `\` escape mechanism in a future version.

### Gemini 3.1 Pro

For v1, I agree — document the limitation. If it becomes a real problem (unlikely for source code repos), we can add `doublestar.PathMatch` which provides path-specific escaping, or define our own escaping convention.

### **Round 4 Consensus**: Full backward compatibility. `isGlob()` is the gate — no glob processing unless metacharacters are present. Document the `[` literal edge case as a known limitation. No migration needed.

---

## Round 5: Validation & Error Handling

### Claude Opus 4.6

Invalid glob patterns (e.g., `internal/[bad`) should be caught early. I propose validating at **parse time** in `frontmatter.ParseTaskData()`:

```go
for _, af := range meta.Affects {
    if isGlob(af) {
        if _, err := doublestar.Match(af, ""); err != nil {
            return TaskMeta{}, "", fmt.Errorf("invalid glob pattern in affects: %q: %w", af, err)
        }
    }
}
```

If the pattern is malformed, the task file fails to parse — it never enters the queue. This is consistent with how we handle malformed YAML today.

### GPT-5.4

I'd go further: also validate in `affectsMatch()` by checking the error return from `doublestar.Match`. Even if parse-time validation catches most issues, defense in depth is good:

```go
if matched, err := doublestar.Match(a, b); err == nil && matched {
    return true
}
```

The `err == nil` check ensures malformed patterns silently fail to match rather than panicking or producing false positives.

### Gemini 3.1 Pro

Both approaches are fine. I'd lean toward parse-time validation as the primary mechanism (fail fast, clear error message) and silent no-match at comparison time as the safety net. This matches the codebase's existing pattern — `ReconcileReadyQueue` already moves unparseable task files to `failed/`.

### **Round 5 Consensus**: Validate glob patterns at parse time in `ParseTaskData()`. Malformed patterns cause task parse failure. At comparison time, `doublestar.Match` errors silently result in no-match (defense in depth).

---

## Round 6: Testing Strategy

### Claude Opus 4.6

Tests should cover:

**Unit tests in `internal/queue/overlap_test.go`:**
1. `isGlob()` — positive and negative cases
2. `staticPrefix()` — various patterns
3. `affectsMatch()` — new glob rows:
   - Glob matches concrete path: `*.go` vs `main.go` → true
   - Glob matches nested path: `internal/**/*.go` vs `internal/queue/queue.go` → true
   - Glob doesn't match: `internal/runner/*.go` vs `pkg/client/http.go` → false
   - Glob vs prefix: `internal/runner/*.go` vs `internal/runner/` → true
   - Glob vs glob (overlapping prefixes): `internal/*.go` vs `internal/r*.go` → true (conservative)
   - Glob vs glob (disjoint prefixes): `internal/runner/*.go` vs `pkg/client/*.go` → false
4. `overlappingAffects()` — mixed glob/exact/prefix lists
5. Symmetry: `affectsMatch(a, b) == affectsMatch(b, a)` for all test cases

### GPT-5.4

Add **index-level tests** in `internal/queue/index_test.go` or `queue_test.go`:
1. `HasActiveOverlap()` with glob entries in the index
2. `ActiveAffects()` includes glob entries
3. `DeferredOverlappingTasksDetailed()` defers based on glob overlap

And **integration tests** in `internal/integration/`:
1. End-to-end: create two tasks with glob affects, verify deferral behavior
2. Edge case: task with no glob, task with glob, overlapping paths

### Gemini 3.1 Pro

Don't forget **parse validation tests** in `internal/frontmatter/frontmatter_test.go`:
1. Valid glob parses successfully
2. Invalid glob pattern (e.g., `[bad`) causes parse failure with descriptive error

### **Round 6 Consensus**: Comprehensive test coverage across three packages: `overlap_test.go` (matching logic), `queue_test.go` / `index_test.go` (scheduling integration), `frontmatter_test.go` (parse validation). Symmetry property verified for all `affectsMatch` cases.

---

## Round 7: Documentation

### GPT-5.4

Three documentation updates needed:

1. **`docs/task-format.md`** — Update the `affects` field description to document glob syntax. Add examples for `*`, `**`, `?`, and `[...]`.

2. **`docs/architecture.md`** — Mention glob support in the overlap detection section.

3. **`.github/skills/mato/SKILL.md`** — Update the task planning skill to generate glob patterns when appropriate.

### Claude Opus 4.6

I'd add clear guidance in `task-format.md` on **when to use each syntax**:

| Syntax | Use case | Example |
|--------|----------|---------|
| Exact path | Known single file | `internal/runner/runner.go` |
| Directory prefix | Entire directory tree | `internal/runner/` |
| Single-level glob | Files matching a pattern in one dir | `internal/runner/*.go` |
| Recursive glob | Files matching across subdirectories | `internal/**/*_test.go` |

### Gemini 3.1 Pro

Good. Also update `mato status` output to visually distinguish glob entries (e.g., show them with the glob marker). Small DX improvement.

### **Round 7 Consensus**: Update `task-format.md` (primary), `architecture.md` (brief mention), and `SKILL.md` (task generation). Include a syntax table showing when to use each pattern type.

---

## CONSENSUS PLAN

All three agents agree on the following implementation plan.

### 1. Files to Create/Modify

| File | Change |
|------|--------|
| `go.mod` / `go.sum` | Add `github.com/bmatcuk/doublestar/v4` dependency |
| `internal/queue/overlap.go` | Add `isGlob()`, `staticPrefix()`, extend `affectsMatch()` |
| `internal/queue/overlap_test.go` | Add glob matching test cases |
| `internal/queue/index.go` | Add `activeAffectsGlobs` field, update `HasActiveOverlap()` |
| `internal/frontmatter/frontmatter.go` | Add glob validation in `ParseTaskData()` |
| `internal/frontmatter/frontmatter_test.go` | Add glob validation test cases |
| `internal/queue/queue_test.go` | Add glob-aware deferral integration tests |
| `docs/task-format.md` | Document glob syntax, add examples, add syntax table |
| `docs/architecture.md` | Mention glob support in overlap detection section |

### 2. New Functions

```go
// internal/queue/overlap.go

// isGlob reports whether s contains glob metacharacters (*, ?, [).
func isGlob(s string) bool {
    return strings.ContainsAny(s, "*?[")
}

// staticPrefix returns the longest directory path prefix before the first
// glob metacharacter. Returns the full string if no metacharacters are present.
func staticPrefix(pattern string) string {
    for i, c := range pattern {
        if c == '*' || c == '?' || c == '[' {
            return pattern[:strings.LastIndex(pattern[:i], "/")+1]
        }
    }
    return pattern
}
```

### 3. Modified Functions

**`affectsMatch(a, b string) bool`** — Extended with:
```go
// After existing exact and prefix checks:
aGlob, bGlob := isGlob(a), isGlob(b)
if aGlob && bGlob {
    // Both are globs: check if static prefixes overlap
    pa, pb := staticPrefix(a), staticPrefix(b)
    if pa == "" || pb == "" {
        return true // empty prefix = could match anything
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
```

**`overlappingAffects(a, b []string) []string`** — Extended scan loop to detect globs:
```go
hasGlobA := false
for _, item := range a {
    if isGlob(item) { hasGlobA = true }
    // ... existing prefix detection
}
// Fast path: only if no prefixes AND no globs
if !hasPrefixA && !hasPrefixB && !hasGlobA && !hasGlobB {
    // existing hash-map intersection
}
```

**`PollIndex` in `index.go`** — New field `activeAffectsGlobs []string`, populated during index build when an active task has a glob affects entry. `HasActiveOverlap()` extended with a third check phase against glob entries.

**`ParseTaskData()` in `frontmatter.go`** — Validate glob entries:
```go
for _, af := range meta.Affects {
    if isGlob(af) {
        if _, err := doublestar.Match(af, ""); err != nil {
            return TaskMeta{}, "", fmt.Errorf("invalid glob in affects %q: %w", af, err)
        }
    }
}
```

### 4. Test Plan

| Test | File | Cases |
|------|------|-------|
| `TestIsGlob` | `overlap_test.go` | `foo.go` → false, `*.go` → true, `internal/**` → true, `pkg/client/` → false, `data[1].csv` → true |
| `TestStaticPrefix` | `overlap_test.go` | `internal/runner/*.go` → `internal/runner/`, `**/*.go` → ``, `foo.go` → `foo.go` |
| `TestAffectsMatch` (extended) | `overlap_test.go` | 8+ new glob rows: glob-vs-exact, glob-vs-prefix, glob-vs-glob (overlapping), glob-vs-glob (disjoint), `**` patterns |
| `TestAffectsMatch_Symmetry` | `overlap_test.go` | Verify `affectsMatch(a,b) == affectsMatch(b,a)` for all cases |
| `TestOverlappingAffects` (extended) | `overlap_test.go` | Mixed lists with globs, prefixes, and exact paths |
| `TestHasActiveOverlap_Globs` | `queue_test.go` or `index_test.go` | Index with glob entry, query with matching concrete path |
| `TestDeferredOverlapping_Glob` | `queue_test.go` | Two backlog tasks with glob-overlapping affects |
| `TestParseTaskData_InvalidGlob` | `frontmatter_test.go` | Malformed glob causes parse error |
| `TestParseTaskData_ValidGlob` | `frontmatter_test.go` | Well-formed glob parses without error |

### 5. Backward Compatibility

- **Zero migration required.** Existing task files have no glob metacharacters → `isGlob()` returns false → all code paths identical to today.
- **No file format changes.** The `affects` field syntax is extended, not changed.
- **No behavioral changes for existing workflows.** The fast path (hash-map intersection) is preserved for exact-only lists.
- **Known limitation:** Paths containing literal `[`, `?`, or `*` characters will be misinterpreted as globs. This is documented but not fixed in v1 (rare in source code paths).

### 6. Dependency

- **Add:** `github.com/bmatcuk/doublestar/v4` (latest stable)
  - Pure Go, zero transitive dependencies
  - MIT license
  - Well-maintained, widely used (23M+ downloads)
  - Used only in `internal/queue/overlap.go` and `internal/frontmatter/frontmatter.go`

### 7. Effort Estimate

| Task | Effort |
|------|--------|
| Add `doublestar` dependency | 10 min |
| Implement `isGlob()`, `staticPrefix()` | 30 min |
| Extend `affectsMatch()` and `overlappingAffects()` | 1 hour |
| Extend `HasActiveOverlap()` and index build | 45 min |
| Add glob validation to `ParseTaskData()` | 30 min |
| Write unit tests (overlap, frontmatter) | 2 hours |
| Write integration tests (queue deferral) | 1 hour |
| Update documentation | 45 min |
| **Total** | **~6.5 hours** |

### 8. Implementation Order

1. Add `doublestar` dependency (`go get`)
2. Implement `isGlob()` and `staticPrefix()` with unit tests
3. Extend `affectsMatch()` with glob cases + tests
4. Update `overlappingAffects()` fast-path guard + tests
5. Extend `PollIndex` with `activeAffectsGlobs` + `HasActiveOverlap()` + tests
6. Add glob validation to `ParseTaskData()` + tests
7. Write integration test for glob-based deferral
8. Update `docs/task-format.md` and `docs/architecture.md`

### 9. Open Questions (Deferred to Future Work)

1. **Glob negation** (`!internal/test_*.go`) — useful for excluding test files from affects. Defer until use case arises.
2. **Glob expansion in `mato status`** — should `mato status` show which concrete files a glob would match? Potentially useful for debugging but requires filesystem access.
3. **Pre-computed overlap index** — for very large task queues (100+ tasks with globs), consider pre-computing a glob intersection index. Current O(n×m) pairwise comparison is fine for expected scale.
4. **Literal escape mechanism** — for paths containing `[`, `?`, or `*` characters. Consider `doublestar.PathMatch` or custom escaping convention.
