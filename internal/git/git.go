package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mato/internal/atomicwrite"
)

var validateBranchFn = func(name string) error {
	out, err := exec.Command("git", "check-ref-format", "--branch", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid branch name %q: git check-ref-format rejected it (%s)", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// ValidateBranch checks that branch is a valid git branch name.
func ValidateBranch(branch string) error {
	return validateBranchFn(branch)
}

// ExportValidateBranchFn returns the current branch-validation hook.
func ExportValidateBranchFn() func(string) error {
	return validateBranchFn
}

// SetValidateBranchFn overrides branch validation for tests.
func SetValidateBranchFn(fn func(string) error) {
	validateBranchFn = fn
}

// ResolveRepoRoot resolves the repository root directory for the given path
// by running "git rev-parse --show-toplevel". The result is trimmed of
// whitespace so callers receive a clean absolute path.
func ResolveRepoRoot(dir string) (string, error) {
	out, err := Output(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Output runs a git command and returns only its stdout. Stderr is captured
// separately so that git warnings (e.g. detached HEAD, fsmonitor) never
// pollute the parsed output. On error, the returned message includes both
// stdout and stderr for diagnostics.
func Output(dir string, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if dir != "" {
		cmdArgs = append(cmdArgs, "-C", dir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, combined)
	}
	return stdout.String(), nil
}

func CreateClone(repoRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "mato-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	if _, err := Output("", "clone", "--quiet", repoRoot, dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("clone repo: %w", err)
	}
	return dir, nil
}

func RemoveClone(dir string) {
	os.RemoveAll(dir)
}

// BranchSource describes how EnsureBranch resolved the target branch.
type BranchSource string

const (
	BranchSourceLocal                 BranchSource = "local"
	BranchSourceRemote                BranchSource = "remote"
	BranchSourceRemoteCached          BranchSource = "remote_cached"
	BranchSourceHeadRemoteMissing     BranchSource = "head_remote_missing"
	BranchSourceHeadRemoteUnavailable BranchSource = "head_remote_unavailable"
)

const defaultGitName = "mato"
const defaultGitEmail = "mato@local.invalid"

// EnsureBranchResult describes how EnsureBranch resolved the target branch.
type EnsureBranchResult struct {
	Branch string
	Source BranchSource
}

// DescribeBranchSource returns a human-readable description of a branch source.
func DescribeBranchSource(branch string, source BranchSource) string {
	switch source {
	case BranchSourceLocal:
		return "existing local branch"
	case BranchSourceRemote:
		return fmt.Sprintf("live origin/%s", branch)
	case BranchSourceRemoteCached:
		return fmt.Sprintf("cached origin/%s (origin unavailable)", branch)
	case BranchSourceHeadRemoteMissing:
		return fmt.Sprintf("current HEAD (origin/%s not found on remote)", branch)
	case BranchSourceHeadRemoteUnavailable:
		return "current HEAD (origin unavailable)"
	default:
		return "unknown"
	}
}

// SourceDescription returns a human-readable description of the branch source.
func (r EnsureBranchResult) SourceDescription() string {
	return DescribeBranchSource(r.Branch, r.Source)
}

type remoteBranchStatus struct {
	available bool
	exists    bool
}

// EnsureBranch ensures the target branch exists and checks it out.
// It prefers the live remote branch (origin/<branch>) as the starting point
// when the local branch is missing. If the remote is reachable and the branch is
// absent there, it creates the branch from HEAD and ignores any stale cached
// remote-tracking ref. If origin is unavailable, it falls back to a cached
// remote-tracking ref before using HEAD.
//
// Before using the remote-tracking ref, it verifies the branch against the live
// remote so deleted remote branches are not silently resurrected from stale
// local metadata.
func EnsureBranch(repoRoot, branch string) (EnsureBranchResult, error) {
	result := EnsureBranchResult{Branch: branch}

	// If the local branch already exists, just check it out.
	if ok, err := refExists(repoRoot, "refs/heads/"+branch); err != nil {
		return result, err
	} else if ok {
		if _, err := Output(repoRoot, "checkout", branch); err != nil {
			return result, fmt.Errorf("checkout branch %s: %w", branch, err)
		}
		result.Source = BranchSourceLocal
		return result, nil
	}

	remoteStatus, err := queryRemoteBranch(repoRoot, "origin", branch)
	if err != nil {
		return result, err
	}

	if remoteStatus.available && remoteStatus.exists {
		if _, err := Output(repoRoot, "fetch", "--quiet", "origin", branch); err != nil {
			return result, fmt.Errorf("fetch branch %s from origin after confirming it exists: %w", branch, err)
		}
		ok, err := refExists(repoRoot, "refs/remotes/origin/"+branch)
		if err != nil {
			return result, err
		}
		if !ok {
			return result, fmt.Errorf("remote branch origin/%s was reported by ls-remote, but refs/remotes/origin/%s is missing after fetch", branch, branch)
		}
		if _, err := Output(repoRoot, "checkout", "-b", branch, "origin/"+branch); err != nil {
			return result, fmt.Errorf("create branch %s from origin/%s: %w", branch, branch, err)
		}
		result.Source = BranchSourceRemote
		return result, nil
	}

	if remoteStatus.available {
		if _, err := Output(repoRoot, "checkout", "-b", branch); err != nil {
			return result, fmt.Errorf("create branch %s from HEAD: %w", branch, err)
		}
		result.Source = BranchSourceHeadRemoteMissing
		return result, nil
	}

	if ok, err := refExists(repoRoot, "refs/remotes/origin/"+branch); err != nil {
		return result, err
	} else if ok {
		if _, err := Output(repoRoot, "checkout", "-b", branch, "origin/"+branch); err != nil {
			return result, fmt.Errorf("create branch %s from cached origin/%s: %w", branch, branch, err)
		}
		result.Source = BranchSourceRemoteCached
		return result, nil
	}

	if _, err := Output(repoRoot, "checkout", "-b", branch); err != nil {
		return result, fmt.Errorf("create branch %s from HEAD: %w", branch, err)
	}
	result.Source = BranchSourceHeadRemoteUnavailable
	return result, nil
}

func refExists(repoRoot, ref string) (bool, error) {
	_, err := Output(repoRoot, "show-ref", "--verify", "--quiet", ref)
	if err == nil {
		return true, nil
	}
	code, ok := exitCode(err)
	if ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check ref %s: %w", ref, err)
}

func queryRemoteBranch(repoRoot, remote, branch string) (remoteBranchStatus, error) {
	_, err := Output(repoRoot, "ls-remote", "--exit-code", "--heads", remote, "refs/heads/"+branch)
	if err == nil {
		return remoteBranchStatus{available: true, exists: true}, nil
	}
	code, ok := exitCode(err)
	if ok {
		if code == 2 {
			return remoteBranchStatus{available: true, exists: false}, nil
		}
		// Any other ls-remote exit code means the branch existence check could not
		// complete reliably (for example auth, DNS, or transport failure), so the
		// caller should treat the remote as unavailable and fall back accordingly.
		return remoteBranchStatus{available: false, exists: false}, nil
	}
	return remoteBranchStatus{}, fmt.Errorf("query remote branch %s/%s: %w", remote, branch, err)
}

func exitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitErr.ExitCode(), true
}

// EnsureGitignoreContains ensures the given pattern is present in the repo's
// .gitignore file, appending it atomically if missing. Returns whether the
// file was modified. No git operations are performed, making this function
// independently testable without a repository.
func EnsureGitignoreContains(repoRoot, pattern string) (bool, error) {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	data, err := os.ReadFile(gitignorePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read .gitignore: %w", err)
	}
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return false, nil
			}
		}
	}

	// Build updated content: existing data + trailing newline if needed + pattern.
	var content string
	if len(data) > 0 && data[len(data)-1] != '\n' {
		content = string(data) + "\n" + pattern + "\n"
	} else {
		content = string(data) + pattern + "\n"
	}

	if err := atomicwrite.WriteFile(gitignorePath, []byte(content)); err != nil {
		return false, fmt.Errorf("write .gitignore: %w", err)
	}

	return true, nil
}

// ResolveIdentity reads git user.name and user.email from the local repo
// config in repoRoot, falling back to global config, and applying defaults
// (defaultGitName / defaultGitEmail) when neither is set. Returns the resolved
// name and email.
func ResolveIdentity(repoRoot string) (name, email string) {
	name, _ = Output(repoRoot, "config", "user.name")
	if strings.TrimSpace(name) == "" {
		name, _ = Output("", "config", "--global", "user.name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultGitName
	}

	email, _ = Output(repoRoot, "config", "user.email")
	if strings.TrimSpace(email) == "" {
		email, _ = Output("", "config", "--global", "user.email")
	}
	email = strings.TrimSpace(email)
	if email == "" {
		email = defaultGitEmail
	}

	return name, email
}

// EnsureIdentity ensures git user.name and user.email are set in the local
// repository config. It resolves values via ResolveIdentity, then best-effort
// writes them back to the repo-local git config for later commands.
func EnsureIdentity(repoRoot string) (name, email string) {
	name, email = ResolveIdentity(repoRoot)
	_, _ = Output(repoRoot, "config", "user.name", name)
	_, _ = Output(repoRoot, "config", "user.email", email)
	return name, email
}

// CommitGitignore stages .gitignore and commits it with the given message.
// This is a simple wrapper that lets callers decide when to commit, rather
// than coupling the commit to the file modification.
func CommitGitignore(repoRoot, message string) error {
	if _, err := Output(repoRoot, "add", "--", ".gitignore"); err != nil {
		return fmt.Errorf("git add .gitignore: %w", err)
	}
	if _, err := Output(repoRoot, "commit", "-m", message, "--", ".gitignore"); err != nil {
		return fmt.Errorf("git commit .gitignore: %w", err)
	}
	return nil
}
