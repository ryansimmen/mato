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

On 2026-04-25, this command reported total statement coverage of 86.6%, which exceeds the OpenSSF Silver 80% statement-coverage criterion.

## Regression Tests

Project policy requires tests for behavior changes. For OpenSSF Silver regression-test evidence, bug-fix commits on `main` can be audited by checking whether `fix:` commits touched test files.

The audit command used on 2026-04-25 was:

```bash
bash -lc 'count=0; with_tests=0; while IFS= read -r commit; do count=$((count+1)); has_test=0; while IFS= read -r file; do case "$file" in *_test.go|internal/integration/*|testdata/*) has_test=1 ;; esac; done < <(git diff-tree --no-commit-id --name-only -r "$commit"); if [ "$has_test" -eq 1 ]; then with_tests=$((with_tests+1)); fi; done < <(git log --since="6 months ago" --format="%H" --grep="^fix:" main); printf "%s fix commits; %s touched tests; %s%%\n" "$count" "$with_tests" "$((with_tests * 100 / count))"'
```

The result was:

```text
271 fix commits; 235 touched tests; 86%
```

This is an automated proxy, not a substitute for human audit of every bug report. It counts a bug-fix commit as having regression-test evidence when the same `fix:` commit changes at least one Go test file, integration test file, or testdata path. It does not count nearby follow-up `test:` commits, so manual review may find additional covered fixes. It also counts fix commits rather than distinct user-reported bugs; use this as supporting evidence when completing the OpenSSF form, not as the only audit artifact if stricter review is requested.
