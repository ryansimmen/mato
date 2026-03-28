# Remove Obsolete Legacy Message Filename Compatibility

**Priority:** Low
**Effort:** Trivial
**Source:** Multi-model feature research debate (GPT-5.4, Claude Opus 4.6, Gemini 3.1 Pro)

## Problem

`internal/messaging/messaging.go` still carries (or carried, before cleanup) an
obsolete compatibility layer
from the old lossy filename sanitization scheme:

- Dead helper `messageFilePart`
- Legacy regex `safeMessageFilePart`
- Legacy completion-file naming helper `legacyCompletionFilename`
- Fallback logic in `ReadCompletionDetail` that tries old-format filenames

The codebase already writes message and completion filenames with the
collision-resistant `safeFilePart` / `safeEncode` path. The remaining legacy code
is unnecessary compatibility baggage when old lossy-named completion files do not
need to be preserved.

## Evidence

- `WriteMessage` and `WritePresence` already use `safeFilePart` for current
  writes.
- `messageFilePart` had zero production callers and was only referenced by tests.
- Completion filenames already used collision-resistant encoding for writes, while
  the legacy compatibility logic existed only on the read path.
- In a fresh or single-user installation where all completion files already match
  the current scheme, the compatibility path has no practical value.

## Idea

Remove the entire obsolete filename-compatibility path:

1. Remove `messageFilePart` from `messaging.go`.
2. Remove the legacy completion-detail fallback and any regex/helper code that only
   exists to support it.
3. Remove or rewrite tests in `messaging_test.go` that exist only to exercise the
   deleted legacy path.
4. Keep the collision-resistance tests, but express them in terms of current
   behavior rather than comparing against removed helpers.

## Design Considerations

- **Prefer full cleanup over narrow cleanup:** Removing only `messageFilePart`
  leaves dead code behind. The helper, its dedicated regex, and helper-only tests
  should be removed together.
- **Current-state assumption:** This cleanup is only safe when existing completion
  files already use the current filename scheme or are otherwise disposable.
- **Behavioral simplification:** `ReadCompletionDetail` becomes a single-path read,
  which makes the dependency-context flow easier to reason about.
- **Test cleanup:** Collision-resistance tests remain useful, but helper-only tests
  should become current-behavior tests rather than historical comparisons.
