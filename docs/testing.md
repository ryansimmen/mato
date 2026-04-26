# Testing

`mato` uses Go's standard `testing` package, integration tests under `internal/integration/`, native Go fuzz targets, and CI workflows for build, lint, race, vulnerability, and static-analysis checks.

## Standard Verification

Fast local checks:

```bash
go build ./...
make test-fast
```

Full verification before release or substantial PRs:

```bash
make verify
go test -race ./...
```

CI runs `make verify`, race tests, CodeQL, and govulncheck on pull requests, pushes to `main`, and merge queue events. The scheduled fuzz workflow runs native Go fuzz targets for parser, sanitizer, and messaging code.

## Coverage

Statement coverage is measured with Go's built-in coverage tooling:

```bash
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
```

On 2026-04-25, this command reported total statement coverage of 86.6%.

## Regression Tests

Project policy requires tests for behavior changes. Bug-fix commits on `main` can be audited by checking whether `fix:` commits touched test files.

This is supporting evidence for the project testing policy, not a substitute for judgment about individual bug reports. `mato` is a young project with mostly maintainer-filed fixes; the audit below tracks the convention the project uses in practice: bug-fix commits should include regression-test changes when the fix is behavioral and testable.

The audit command used on 2026-04-25 was:

```bash
bash -lc 'count=0; with_tests=0; while IFS= read -r commit; do count=$((count+1)); has_test=0; while IFS= read -r file; do case "$file" in *_test.go|internal/integration/*|testdata/*) has_test=1 ;; esac; done < <(git diff-tree --no-commit-id --name-only -r "$commit"); if [ "$has_test" -eq 1 ]; then with_tests=$((with_tests+1)); fi; done < <(git log --since="6 months ago" --format="%H" --grep="^fix:" main); printf "%s fix commits; %s touched tests; %s%%\n" "$count" "$with_tests" "$((with_tests * 100 / count))"'
```

The result was:

```text
271 fix commits; 235 touched tests; 86%
```

This audit counted `fix:` commits rather than distinct externally reported bugs. It counts a bug-fix commit as having regression-test evidence when the same `fix:` commit changes at least one Go test file, integration test file, or testdata path. It does not count nearby follow-up `test:` commits, so manual review may find additional covered fixes. It also does not require tests for non-behavioral fixes such as CI configuration corrections when a test is not practical.

Because the measured rate is high even with those conservative limitations, the result supports the project practice of adding regression tests for behavioral fixes. If a future review needs stricter evidence, audit individual bug reports and release-note bug fixes against their associated pull requests.
