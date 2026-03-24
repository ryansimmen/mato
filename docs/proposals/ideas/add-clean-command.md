---
id: add-clean-command
priority: 44
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
tags: [feature, cli]
estimated_complexity: medium
---
# Add mato clean command to remove old completed/failed tasks

The `completed/` and `failed/` directories accumulate task files
indefinitely. Over time this degrades `BuildIndex` performance (which
scans all directories on each poll cycle) and clutters the filesystem.
There is no built-in way to archive or remove old tasks.

## Steps to fix

1. Add a `clean` subcommand to `cmd/mato/main.go`:
   ```
   mato clean [--completed] [--failed] [--older-than 168h] [--dry-run]
   ```

2. Behavior:
   - `--completed`: remove files from `completed/` (default: true).
   - `--failed`: remove files from `failed/` (default: false, to avoid
     accidental deletion of tasks users may want to retry).
   - `--older-than <duration>`: only remove files whose `<!-- merged: ... -->`
     or last `<!-- failure: ... -->` timestamp is older than the given
     duration (default: `168h`). Parse as a Go duration string.
   - `--dry-run`: list files that would be removed without deleting them.
   - With no flags, remove completed tasks older than 168 hours.

3. Also clean corresponding completion detail files from
   `messages/completions/` for removed completed tasks.

4. Print a summary: `Removed N completed tasks, M failed tasks`.

5. Add tests:
   - Clean with age filter (old tasks removed, recent ones kept).
   - Clean with --dry-run (no files removed, output lists them).
   - Clean --failed flag.
   - Clean removes corresponding completion details.

6. Update `README.md` and `docs/configuration.md`.

7. Run `go build ./... && go test -count=1 ./...` to verify.
