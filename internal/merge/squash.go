package merge

import (
	"fmt"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/ui"
)

var gitOutput = git.Output
var resolveIdentity = git.ResolveIdentity

func mergeReadyTask(repoRoot, branch string, task mergeQueueTask) (*mergeResult, error) {
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("create temp clone: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureMergeCloneIdentity(repoRoot, cloneDir); err != nil {
		return nil, err
	}
	if _, err := gitOutput(cloneDir, "fetch", "origin"); err != nil {
		return nil, fmt.Errorf("fetch origin: %w", err)
	}
	if _, err := gitOutput(cloneDir, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return nil, fmt.Errorf("checkout target branch %s: %w", branch, err)
	}

	taskBranch := taskBranchName(task)
	if _, err := gitOutput(cloneDir, "rev-parse", "--verify", "origin/"+taskBranch); err != nil {
		return nil, fmt.Errorf("%w: task branch %s not found on origin (agent may not have pushed)", errTaskBranchNotPushed, taskBranch)
	}

	// Extract the earliest commit message on the task branch so the squash
	// commit subject reflects the task's primary intent, not a later
	// review-fix commit. We use rev-list --reverse to find the first SHA,
	// then read its full message.
	var agentLog string
	if revList, err := gitOutput(cloneDir, "rev-list", "--reverse", "origin/"+branch+"..origin/"+taskBranch); err == nil {
		if firstSHA := firstNonEmptyLine(revList); firstSHA != "" {
			agentLog, _ = gitOutput(cloneDir, "log", "-1", "--format=%B", firstSHA)
		}
	}

	if _, err := gitOutput(cloneDir, "merge", "--squash", "origin/"+taskBranch); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errSquashMergeConflict, taskBranch, err)
	}

	// If the squash produced no staged changes, the task branch is already
	// fully merged into the target (e.g. a prior push succeeded but
	// post-push bookkeeping failed).  Return a mergeResult with recovered
	// metadata so the caller can write the completion detail that was
	// missed on the prior run, without creating a duplicate commit.
	if _, err := gitOutput(cloneDir, "diff", "--cached", "--quiet"); err == nil {
		result := recoverMergedTaskMetadata(cloneDir, branch, task)
		return &result, nil
	}

	if _, err := gitOutput(cloneDir, "commit", "-m", formatSquashCommitMessage(task, agentLog)); err != nil {
		return nil, fmt.Errorf("commit squash merge: %w", err)
	}
	if _, err := gitOutput(cloneDir, "push", "origin", branch); err != nil {
		return nil, fmt.Errorf("%w: push %s: %w", errPushAfterSquashFailed, branch, err)
	}

	// Capture merge result for completion detail.
	sha, err := gitOutput(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		ui.Warnf("warning: could not determine commit SHA after push: %v\n", err)
		sha = "unknown"
	}
	filesOut, _ := gitOutput(cloneDir, "diff", "--name-only", "HEAD~1..HEAD")
	filesChanged := parseFilesChanged(filesOut)
	mergedAt := commitTimestampForRef(cloneDir, "HEAD")

	return &mergeResult{
		commitSHA:    strings.TrimSpace(sha),
		filesChanged: filesChanged,
		mergedAt:     mergedAt,
	}, nil
}

func recoverMergedTaskMetadata(cloneDir, branch string, task mergeQueueTask) mergeResult {
	if sha := findMergedTaskCommit(cloneDir, branch, task.id); sha != "" {
		return mergeResult{
			commitSHA:    sha,
			filesChanged: filesChangedForCommit(cloneDir, sha),
			mergedAt:     commitTimestampForRef(cloneDir, sha),
		}
	}

	sha, _ := gitOutput(cloneDir, "rev-parse", "HEAD")
	filesOut, _ := gitOutput(cloneDir, "diff", "--name-only", "origin/"+branch+"...origin/"+taskBranchName(task))
	return mergeResult{
		commitSHA:    strings.TrimSpace(sha),
		filesChanged: parseFilesChanged(filesOut),
	}
}

func findMergedTaskCommit(cloneDir, branch, taskID string) string {
	if taskID == "" {
		return ""
	}
	out, err := gitOutput(cloneDir, "log", "origin/"+branch, "--format=%H", "-F", "--grep", "Task-ID: "+taskID)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func filesChangedForCommit(cloneDir, sha string) []string {
	if sha == "" {
		return nil
	}
	out, err := gitOutput(cloneDir, "show", "--pretty=", "--name-only", sha)
	if err != nil {
		return nil
	}
	return parseFilesChanged(out)
}

func commitTimestampForRef(cloneDir, ref string) time.Time {
	if strings.TrimSpace(ref) == "" {
		return time.Time{}
	}
	out, err := gitOutput(cloneDir, "log", "-1", "--format=%cI", ref)
	if err != nil {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if err != nil {
		return time.Time{}
	}
	return ts.UTC()
}

func parseFilesChanged(out string) []string {
	var filesChanged []string
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			filesChanged = append(filesChanged, f)
		}
	}
	return filesChanged
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// formatSquashCommitMessage builds the squash-merge commit message.
// It prefers the agent's commit message (from agentLog) for the subject and
// body, falling back to the task title when no agent log is available.
// Task-ID and Affects trailers are always appended when present.
func formatSquashCommitMessage(task mergeQueueTask, agentLog string) string {
	subject, body := parseAgentCommitLog(agentLog)
	if subject == "" {
		subject = task.title
	}

	var trailers []string
	if task.id != "" {
		trailers = append(trailers, "Task-ID: "+task.id)
	}
	if len(task.affects) > 0 {
		trailers = append(trailers, "Affects: "+strings.Join(task.affects, ", "))
	}

	var parts []string
	parts = append(parts, subject)
	if body != "" || len(trailers) > 0 {
		parts = append(parts, "") // blank line after subject
	}
	if body != "" {
		parts = append(parts, body)
	}
	if len(trailers) > 0 {
		if body != "" {
			parts = append(parts, "") // blank line before trailers
		}
		parts = append(parts, strings.Join(trailers, "\n"))
	}

	return strings.Join(parts, "\n")
}

// parseAgentCommitLog extracts the subject and body from the agent's commit
// log output. The caller is expected to pass a --reverse log so the earliest
// commit (the task's primary intent) appears first. For multi-commit branches,
// only the first commit's message is used. Lines matching "Task: <filename>"
// and "Changed files:" sections are stripped from the body since that metadata
// is redundant with the trailers.
func parseAgentCommitLog(log string) (subject, body string) {
	log = strings.TrimSpace(log)
	if log == "" {
		return "", ""
	}

	lines := strings.Split(log, "\n")

	// First non-empty line is the subject.
	var subjectLine string
	bodyStart := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			subjectLine = strings.TrimSpace(line)
			bodyStart = i + 1
			break
		}
	}
	if subjectLine == "" {
		return "", ""
	}

	// Skip the blank line after the subject.
	if bodyStart < len(lines) && strings.TrimSpace(lines[bodyStart]) == "" {
		bodyStart++
	}

	// Collect body lines, filtering out mechanical "Task:" and "Changed files:" sections.
	var bodyLines []string
	skipChangedFiles := false
	for i := bodyStart; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])

		// Stop at the next commit boundary (double blank lines typically
		// separate commits in git log --format=%B output).
		if skipChangedFiles && trimmed == "" {
			// End of the changed files block; stop processing this commit.
			break
		}
		if skipChangedFiles {
			continue
		}

		if strings.HasPrefix(trimmed, "Task:") {
			continue
		}
		if trimmed == "Changed files:" {
			skipChangedFiles = true
			continue
		}

		bodyLines = append(bodyLines, lines[i])
	}

	// Trim trailing blank lines.
	for len(bodyLines) > 0 && strings.TrimSpace(bodyLines[len(bodyLines)-1]) == "" {
		bodyLines = bodyLines[:len(bodyLines)-1]
	}

	return subjectLine, strings.Join(bodyLines, "\n")
}

func configureMergeCloneIdentity(repoRoot, cloneDir string) error {
	name, email := resolveIdentity(repoRoot)

	if _, err := gitOutput(cloneDir, "config", "user.name", name); err != nil {
		return fmt.Errorf("configure merge user.name: %w", err)
	}
	if _, err := gitOutput(cloneDir, "config", "user.email", email); err != nil {
		return fmt.Errorf("configure merge user.email: %w", err)
	}
	return nil
}

func taskBranchName(task mergeQueueTask) string {
	return strings.TrimSpace(task.branch)
}

func cleanupTaskBranch(repoRoot, branchName string) {
	if strings.TrimSpace(branchName) == "" {
		return
	}
	// Clean up the stale task branch so the next agent can push a fresh one.
	// Cleanup is best-effort: log warnings but never abort the merge flow.
	if _, err := gitOutput(repoRoot, "branch", "-D", "--", branchName); err != nil {
		ui.Warnf("warning: could not delete local task branch %s: %v\n", branchName, err)
	}
	if _, err := gitOutput(repoRoot, "push", "origin", "--delete", "--", branchName); err != nil {
		if strings.Contains(err.Error(), "remote ref does not exist") {
			return
		}
		ui.Warnf("warning: could not delete remote task branch %s: %v\n", branchName, err)
	}
}
