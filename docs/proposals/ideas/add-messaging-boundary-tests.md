---
id: add-messaging-boundary-tests
priority: 33
affects:
  - internal/messaging/messaging.go
  - internal/messaging/messaging_test.go
tags: [test, reliability]
estimated_complexity: simple
---
# Add boundary and edge-case tests for messaging package

The messaging package has good overall coverage but is missing tests for
several boundary conditions that could cause subtle bugs under load.

## Test cases to add

1. **`CleanOldMessages` with zero maxAge** — Call with `maxAge = 0`.
   Current behavior is unclear: does it remove all messages or none?
   Add a test that documents and asserts the correct behavior.

2. **`CleanOldMessages` with negative maxAge** — Call with
   `maxAge = -1 * time.Hour`. Verify it does not panic or exhibit
   undefined behavior.

3. **Duplicate message ID handling** — Write two messages with the same
   `ID` field but different `Body` content. Call `ReadMessages` and verify
   both are returned (current behavior: separate files, no dedup). This
   documents the contract.

4. **`CleanStalePresence` concurrent with `WritePresence`** — Start a
   goroutine writing presence repeatedly while another goroutine calls
   `CleanStalePresence`. Verify no panics or data corruption. Run with
   `-race`.

5. **`WriteMessage` timestamp collision** — Write two messages in a tight
   loop with the same agent ID. Verify both files are created (filenames
   include nanosecond suffix + message ID, so collisions should not occur).
   This is a regression guard.

## Verification

- `go test -race ./internal/messaging/...`
