package doctor

import (
"context"
"errors"
"os"
"os/exec"
"strings"

"mato/internal/queue"
)

// checkContext carries shared state across checks within a single doctor run.
type checkContext struct {
ctx       context.Context
repoInput string
repoRoot  string // populated by resolveRepo on success
repoErr   error  // populated by resolveRepo on failure
tasksDir  string // derived from repoRoot
opts      Options
idx       *queue.PollIndex // lazily built, shared across checks
}

var osStatFn = os.Stat

// hasRepo returns true if the git check resolved a valid repo root.
func (c *checkContext) hasRepo() bool {
return c.repoRoot != ""
}

// hasTasksDir returns true if tasksDir is resolved.
func (c *checkContext) hasTasksDir() bool {
return c.tasksDir != ""
}

// resolveRepo attempts to resolve repoRoot from repoInput using git.
// It populates c.repoRoot on success or c.repoErr on failure. This is
// called unconditionally before the check loop so that --only filters
// that skip "git" still have access to the repo root for deriving
// tasksDir.
func (c *checkContext) resolveRepo() {
if c.repoRoot != "" {
return
}
out, err := exec.CommandContext(c.ctx, "git", "-C", c.repoInput, "rev-parse", "--show-toplevel").Output()
if err != nil {
c.repoErr = err
return
}
c.repoRoot = strings.TrimSpace(string(out))
}

// repoErrDetail returns a human-readable description of the repo
// resolution failure, extracting stderr from exec.ExitError when
// available so callers don't get a bare "exit status 128".
func (c *checkContext) repoErrDetail() string {
if c.repoErr == nil {
return ""
}
var exitErr *exec.ExitError
if errors.As(c.repoErr, &exitErr) {
stderr := strings.TrimSpace(string(exitErr.Stderr))
if stderr != "" {
return stderr
}
}
return c.repoErr.Error()
}

// ensureIndex lazily builds the PollIndex from tasksDir.
func (c *checkContext) ensureIndex() *queue.PollIndex {
if c.idx == nil {
c.idx = queue.BuildIndex(c.tasksDir)
}
return c.idx
}

// checkDef associates a check name with its implementation.
type checkDef struct {
name string
run  func(*checkContext) CheckReport
}

// checks is the ordered list of all health checks.
var checks = []checkDef{
{"git", checkGit},
{"tools", checkTools},
{"docker", checkDocker},
{"queue", checkQueueLayout},
{"tasks", checkTaskParsing},
{"locks", checkLocksAndOrphans},
{"hygiene", checkHygiene},
{"deps", checkDependencies},
}
