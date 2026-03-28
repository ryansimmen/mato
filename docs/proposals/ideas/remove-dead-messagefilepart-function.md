# Remove Dead `messageFilePart` Function

**Priority:** Low
**Effort:** Trivial
**Source:** Multi-model feature research debate (GPT-5.4, Claude Opus 4.6, Gemini 3.1 Pro)

## Problem

The `messageFilePart` function in `internal/messaging/messaging.go` (line 516) is
dead code. It was replaced by the collision-resistant `safeFilePart` / `safeEncode`
pair but was never removed.

The old function uses regex-based lossy sanitization (`safeMessageFilePart` regex),
which could produce filename collisions for distinct inputs. The replacement
`safeFilePart` (line 356) uses `safeEncode` (line 339) with hex-encoding to avoid
collisions entirely.

## Evidence

- `messaging.go:516-527` — `messageFilePart` is defined but has zero callers in
  production code. Only test files reference it.
- `messaging.go:106-108` — `WriteMessage` uses `safeFilePart`, not
  `messageFilePart`.
- `messaging.go:227` — `WritePresence` uses `safeFilePart`.
- The `safeMessageFilePart` regex (line 62) is still referenced by
  `legacyCompletionFilename` (line 378) for backward-compatible filename matching,
  but that function calls the regex directly, not `messageFilePart`.

## Idea

1. Remove the `messageFilePart` function (lines 516-527) from `messaging.go`.
2. Update or remove tests in `messaging_test.go` that exercise the dead function.
3. Keep the `safeMessageFilePart` regex if still needed by `legacyCompletionFilename`.

## Design Considerations

- **Trivial scope:** This is a pure dead-code removal with no behavioral change.
- **Test cleanup:** Several tests in `messaging_test.go` call `messageFilePart`
  directly. These tests should be removed or migrated to test `safeFilePart`
  instead.
- **Legacy regex:** The `safeMessageFilePart` regex is used by
  `legacyCompletionFilename` for backward compatibility with pre-existing completion
  files. It should be preserved independently of the function removal.
