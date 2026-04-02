package doctor

import (
	"context"
	"os"
	"strings"

	"mato/internal/git"
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
	root, err := git.ResolveRepoRoot(c.repoInput)
	if err != nil {
		c.repoErr = err
		return
	}
	c.repoRoot = root
}

// repoErrDetail returns a human-readable description of the repo
// resolution failure. git.ResolveRepoRoot wraps the error from
// git.Output, which includes stderr in parentheses at the end of the
// message. This function extracts that parenthesized content so
// callers get the actual git error (e.g. "fatal: not a git repository")
// rather than a technical "exit status 128".
func (c *checkContext) repoErrDetail() string {
	if c.repoErr == nil {
		return ""
	}
	msg := c.repoErr.Error()
	// git.Output formats errors as "git ...: exit status N (stderr)".
	// Extract the trailing parenthesized stderr for a user-friendly message.
	// Use the final " (...)" suffix rather than the first '(' so paths like
	// /tmp/foo(bar) do not confuse the parser.
	if strings.HasSuffix(msg, ")") {
		if i := strings.LastIndex(msg, " ("); i >= 0 {
			if j := len(msg) - 1; j > i+1 {
				detail := msg[i+2 : j]
				if strings.Contains(detail, "fatal:") || strings.Contains(detail, "not a git repository") || strings.Contains(detail, "cannot change to") {
					return detail
				}
			}
		}
	}
	return msg
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
