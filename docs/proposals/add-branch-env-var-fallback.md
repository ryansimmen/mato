---
id: add-branch-env-var-fallback
priority: 43
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - docs/configuration.md
tags: [feature, configuration, ux]
estimated_complexity: simple
---
# Add MATO_BRANCH environment variable fallback

The default merge target branch is hardcoded to `"mato"` in
`cmd/mato/main.go` (line 126, `resolveBranch`). Users whose main branch is
`main` or `master` must always pass `--branch`, which is tedious for
repeated invocations. There is no environment variable fallback.

## Steps to fix

1. In `resolveBranch`, add a precedence chain:
   - CLI flag `--branch` (highest priority).
   - `MATO_BRANCH` environment variable.
   - Default `"mato"` (lowest priority).

2. Apply the same whitespace/empty validation to the env var value that
   already exists for the CLI flag.

3. Add tests:
   - Env var set, no flag: uses env var value.
   - Flag and env var both set: flag wins.
   - Neither set: uses "mato".
   - Env var set to empty/whitespace: rejected with error.

4. Update `docs/configuration.md` to add `MATO_BRANCH` to the environment
   variables table with the precedence description.

5. Run `go build ./... && go test -count=1 ./...` to verify.
