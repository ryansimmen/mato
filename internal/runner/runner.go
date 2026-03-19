package runner

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/queue"
)

//go:embed task-instructions.md
var taskInstructions string

//go:embed review-instructions.md
var reviewInstructions string

var reviewBranchRe = regexp.MustCompile(`<!-- branch:\s*(\S+)`)

type dockerConfig struct {
	image, workdir, prompt                         string
	copilotPath, gitPath, gitUploadPackPath         string
	gitReceivePackPath, ghPath, goRoot              string
	gitName, gitEmail, homeDir, ghConfigDir         string
	hasGhConfig                                    bool
	gitTemplatesDir                                string
	hasGitTemplates                                bool
	systemCertsDir                                 string
	hasSystemCerts                                 bool
	agentID                                        string
	copilotArgs                                    []string
	repoRoot, cloneDir, tasksDir                   string
}

func buildDockerArgs(cfg dockerConfig, extraEnvs []string, extraVolumes []string) []string {
	containerHome := cfg.homeDir
	copilotDir := filepath.Join(cfg.homeDir, ".copilot")
	goModCache := filepath.Join(cfg.homeDir, "go", "pkg", "mod")
	goBuildCache := filepath.Join(cfg.homeDir, ".cache", "go-build")

	args := []string{
		"run", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", fmt.Sprintf("%s:%s", cfg.cloneDir, cfg.workdir),
		"-v", fmt.Sprintf("%s:%s/.tasks", cfg.tasksDir, cfg.workdir),
		"-v", fmt.Sprintf("%s:%s", cfg.repoRoot, cfg.repoRoot),
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", cfg.copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git:ro", cfg.gitPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-upload-pack:ro", cfg.gitUploadPackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-receive-pack:ro", cfg.gitReceivePackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", cfg.ghPath),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", cfg.goRoot),
		"-e", "GOROOT=/usr/local/go",
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	args = append(args,
		"-e", "MATO_AGENT_ID="+cfg.agentID,
		"-e", "MATO_MESSAGING_ENABLED=1",
		"-e", fmt.Sprintf("MATO_MESSAGES_DIR=%s/.tasks/messages", cfg.workdir),
	)
	for _, env := range extraEnvs {
		args = append(args, "-e", env)
	}
	args = append(args,
		"-e", "GIT_CONFIG_COUNT=1",
		"-e", "GIT_CONFIG_KEY_0=safe.directory",
		"-e", "GIT_CONFIG_VALUE_0=*",
	)
	if n := strings.TrimSpace(cfg.gitName); n != "" {
		args = append(args, "-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n)
	}
	if e := strings.TrimSpace(cfg.gitEmail); e != "" {
		args = append(args, "-e", "GIT_AUTHOR_EMAIL="+e, "-e", "GIT_COMMITTER_EMAIL="+e)
	}
	args = append(args,
		"-e", fmt.Sprintf("HOME=%s", containerHome),
		"-v", fmt.Sprintf("%s:%s/.copilot", copilotDir, containerHome),
		"-e", fmt.Sprintf("GOPATH=%s/go", containerHome),
		"-e", fmt.Sprintf("GOMODCACHE=%s/go/pkg/mod", containerHome),
		"-e", fmt.Sprintf("GOCACHE=%s/.cache/go-build", containerHome),
		"-v", fmt.Sprintf("%s:%s/go/pkg/mod", goModCache, containerHome),
		"-v", fmt.Sprintf("%s:%s/.cache/go-build", goBuildCache, containerHome),
	)
	if cfg.hasGhConfig {
		args = append(args, "-v", fmt.Sprintf("%s:%s/.config/gh:ro", cfg.ghConfigDir, containerHome))
	}
	if cfg.hasGitTemplates {
		args = append(args, "-v", fmt.Sprintf("%s:%s:ro", cfg.gitTemplatesDir, cfg.gitTemplatesDir))
	}
	if cfg.hasSystemCerts {
		args = append(args, "-v", fmt.Sprintf("%s:/etc/ssl/certs:ro", cfg.systemCertsDir))
	}
	for _, vol := range extraVolumes {
		args = append(args, "-v", vol)
	}
	args = append(args,
		"-w", cfg.workdir,
		cfg.image,
		"copilot", "-p", cfg.prompt, "--autopilot", "--allow-all",
	)
	if !hasModelArg(cfg.copilotArgs) {
		args = append(args, "--model", "claude-opus-4.6")
	}
	args = append(args, cfg.copilotArgs...)
	return args
}

func Run(repoRoot, branch, tasksDirOverride string, copilotArgs []string) error {
	repoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	if err := git.EnsureBranch(repoRoot, branch); err != nil {
		return err
	}

	tasksDir := tasksDirOverride
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-for-review", "ready-to-merge", "completed", "failed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create .tasks subdirectory %s: %w", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		return fmt.Errorf("init messaging: %w", err)
	}

	agentID, err := queue.GenerateAgentID()
	if err != nil {
		return fmt.Errorf("generate agent ID: %w", err)
	}

	cleanupLock, err := queue.RegisterAgent(tasksDir, agentID)
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	defer cleanupLock()

	gitName, _ := git.Output(repoRoot, "config", "user.name")
	gitEmail, _ := git.Output(repoRoot, "config", "user.email")
	if strings.TrimSpace(gitName) == "" {
		gitName, _ = git.Output("", "config", "--global", "user.name")
	}
	if strings.TrimSpace(gitEmail) == "" {
		gitEmail, _ = git.Output("", "config", "--global", "user.email")
	}
	if n := strings.TrimSpace(gitName); n != "" {
		git.Output(repoRoot, "config", "user.name", n)
	}
	if e := strings.TrimSpace(gitEmail); e != "" {
		git.Output(repoRoot, "config", "user.email", e)
	}

	if err := git.EnsureGitignored(repoRoot, "/.tasks/"); err != nil {
		return err
	}

	image := os.Getenv("MATO_DOCKER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	workdir := "/workspace"

	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		return fmt.Errorf("find copilot CLI: %w", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("find git CLI: %w", err)
	}
	gitUploadPackPath, err := exec.LookPath("git-upload-pack")
	if err != nil {
		out, execErr := exec.Command("git", "--exec-path").Output()
		if execErr != nil {
			return fmt.Errorf("find git-upload-pack: git --exec-path failed: %w", execErr)
		}
		candidate := filepath.Join(strings.TrimSpace(string(out)), "git-upload-pack")
		if _, statErr := os.Stat(candidate); statErr != nil {
			return fmt.Errorf("find git-upload-pack: %w", statErr)
		}
		gitUploadPackPath = candidate
	}
	gitReceivePackPath, err := exec.LookPath("git-receive-pack")
	if err != nil {
		out, execErr := exec.Command("git", "--exec-path").Output()
		if execErr != nil {
			return fmt.Errorf("find git-receive-pack: git --exec-path failed: %w", execErr)
		}
		candidate := filepath.Join(strings.TrimSpace(string(out)), "git-receive-pack")
		if _, statErr := os.Stat(candidate); statErr != nil {
			return fmt.Errorf("find git-receive-pack: %w", statErr)
		}
		gitReceivePackPath = candidate
	}
	ghPath := "/usr/bin/gh"
	if info, statErr := os.Stat(ghPath); statErr != nil || info.IsDir() {
		ghPath, err = exec.LookPath("gh")
		if err != nil {
			return fmt.Errorf("find gh CLI: %w", err)
		}
	}
	goRoot := runtime.GOROOT()

	gitTemplatesDir := "/usr/share/git-core/templates"
	hasGitTemplates := false
	if info, statErr := os.Stat(gitTemplatesDir); statErr == nil && info.IsDir() {
		hasGitTemplates = true
	}

	systemCertsDir := "/etc/ssl/certs"
	hasSystemCerts := false
	if info, statErr := os.Stat(systemCertsDir); statErr == nil && info.IsDir() {
		hasSystemCerts = true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasGhConfig := false
	if info, statErr := os.Stat(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/.tasks")
	prompt = strings.ReplaceAll(prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	prompt = strings.ReplaceAll(prompt, "MESSAGES_DIR_PLACEHOLDER", workdir+"/.tasks/messages")

	cfg := dockerConfig{
		image:              image,
		workdir:            workdir,
		prompt:             prompt,
		copilotPath:        copilotPath,
		gitPath:            gitPath,
		gitUploadPackPath:  gitUploadPackPath,
		gitReceivePackPath: gitReceivePackPath,
		ghPath:             ghPath,
		goRoot:             goRoot,
		gitName:            gitName,
		gitEmail:           gitEmail,
		homeDir:            homeDir,
		ghConfigDir:        ghConfigDir,
		hasGhConfig:        hasGhConfig,
		gitTemplatesDir:    gitTemplatesDir,
		hasGitTemplates:    hasGitTemplates,
		systemCertsDir:     systemCertsDir,
		hasSystemCerts:     hasSystemCerts,
		agentID:            agentID,
		copilotArgs:        copilotArgs,
		repoRoot:           repoRoot,
		tasksDir:            tasksDir,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	wasIdle := false
	failedDirExcluded := make(map[string]struct{})
	for {
		queue.RecoverOrphanedTasks(tasksDir)
		queue.CleanStaleLocks(tasksDir)
		queue.CleanStaleReviewLocks(tasksDir)
		messaging.CleanStalePresence(tasksDir)
		messaging.CleanOldMessages(tasksDir, 24*time.Hour)

		queue.ReconcileReadyQueue(tasksDir)
		deferred := queue.DeferredOverlappingTasks(tasksDir)
		// Merge in tasks excluded due to failed/ being unavailable so they
		// are not re-selected on each poll, preventing livelock.
		for name := range failedDirExcluded {
			deferred[name] = struct{}{}
		}
		if err := queue.WriteQueueManifest(tasksDir, deferred); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write queue manifest: %v\n", err)
		}

		claimed, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, deferred)
		var fdErr *queue.FailedDirUnavailableError
		if errors.As(claimErr, &fdErr) {
			failedDirExcluded[fdErr.TaskFilename] = struct{}{}
			fmt.Fprintf(os.Stderr, "warning: excluding retry-exhausted task %s from future polls (failed/ directory unavailable)\n", fdErr.TaskFilename)
		} else if claimErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not claim task: %v\n", claimErr)
		}
		hasBacklogTasks := claimed != nil
		if claimed != nil {
			messaging.WriteMessage(tasksDir, messaging.Message{
				From:   agentID,
				Type:   "intent",
				Task:   claimed.Filename,
				Branch: claimed.Branch,
				Body:   "Starting work",
			})

			messaging.WritePresence(tasksDir, agentID, claimed.Filename, claimed.Branch)

			if err := messaging.BuildAndWriteFileClaims(tasksDir, claimed.Filename); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not build file claims: %v\n", err)
			}

			if err := runOnce(cfg, claimed); err != nil {
				fmt.Fprintf(os.Stderr, "warning: agent run failed: %v\n", err)
			}

			recoverStuckTask(tasksDir, agentID, claimed)
		}

		if reviewTask, reviewCleanup := selectAndLockReview(tasksDir); reviewTask != nil {
			fmt.Printf("Reviewing task %s on branch %s\n", reviewTask.Filename, reviewTask.Branch)
			if err := runReview(cfg, reviewTask, branch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: review agent failed: %v\n", err)
			}
			reviewCleanup()
		}

		if cleanup, ok := merge.AcquireLock(tasksDir); ok {
			func() {
				defer cleanup()
				merged := merge.ProcessQueue(repoRoot, tasksDir, branch)
				if merged > 0 {
					fmt.Printf("Merged %d task(s) into %s\n", merged, branch)
				}
			}()
		}

		hasReviewTasks := selectTaskForReview(tasksDir) != nil
		isIdle := !hasBacklogTasks && !hasReviewTasks && !merge.HasReadyTasks(tasksDir)
		if checkIdleTransition(isIdle, &wasIdle) {
			fmt.Println("No tasks found in backlog, ready-for-review, or ready-to-merge. Waiting...")
		}

		select {
		case <-sigCh:
			fmt.Println("\nInterrupted. Exiting.")
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

func runOnce(cfg dockerConfig, claimed *queue.ClaimedTask) error {
	cloneDir, err := git.CreateClone(cfg.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureReceiveDeny(cfg.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	cfg.cloneDir = cloneDir

	fmt.Printf("Launching agent from %s (clone: %s)\n", cfg.repoRoot, cloneDir)

	maxRetries := 3
	extraEnvs := []string{}
	if claimed != nil {
		if meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath); err == nil {
			maxRetries = meta.MaxRetries
		}
		extraEnvs = append(extraEnvs,
			"MATO_TASK_FILE="+claimed.Filename,
			"MATO_TASK_BRANCH="+claimed.Branch,
			"MATO_TASK_TITLE="+claimed.Title,
			fmt.Sprintf("MATO_TASK_PATH=%s/.tasks/in-progress/%s", cfg.workdir, claimed.Filename),
			fmt.Sprintf("MATO_FILE_CLAIMS=%s/.tasks/messages/file-claims.json", cfg.workdir),
		)
		if depCtx := buildDependencyContext(cfg.tasksDir, claimed); depCtx != "" {
			extraEnvs = append(extraEnvs, "MATO_DEPENDENCY_CONTEXT="+depCtx)
		}
		if failures := extractFailureLines(claimed.TaskPath); failures != "" {
			extraEnvs = append(extraEnvs, "MATO_PREVIOUS_FAILURES="+failures)
		}
		if reviewFeedback := extractReviewRejections(claimed.TaskPath); reviewFeedback != "" {
			extraEnvs = append(extraEnvs, "MATO_REVIEW_FEEDBACK="+reviewFeedback)
		}
	}
	extraEnvs = append(extraEnvs, fmt.Sprintf("MATO_MAX_RETRIES=%d", maxRetries))

	args := buildDockerArgs(cfg, extraEnvs, nil)

	cmd := exec.Command("docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// recoverStuckTask checks whether a claimed task is still in in-progress/
// after the agent container exits. If so, the agent did not complete its
// lifecycle (crash, OOM, SIGKILL, etc.), so the host moves the task back
// to backlog/ with a failure record for a future retry attempt.
func recoverStuckTask(tasksDir, agentID string, claimed *queue.ClaimedTask) {
	if _, err := os.Stat(claimed.TaskPath); err != nil {
		// Task was moved by the agent (to ready-to-merge or backlog); nothing to do.
		return
	}

	dst := filepath.Join(tasksDir, "backlog", claimed.Filename)
	// Use os.Link + os.Remove instead of os.Rename to atomically prevent
	// overwriting an existing file at dst (TOCTOU race fix). os.Link fails
	// with os.ErrExist if the destination already exists.
	if err := os.Link(claimed.TaskPath, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "warning: could not recover stuck task %s: destination already exists in backlog\n", claimed.Filename)
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not recover stuck task %s: %v\n", claimed.Filename, err)
		}
		return
	}
	if err := os.Remove(claimed.TaskPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove in-progress task %s after linking to backlog: %v\n", claimed.Filename, err)
	}

	f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open task file to append failure record for %s: %v\n", claimed.Filename, err)
	} else {
		_, writeErr := fmt.Fprintf(f, "\n<!-- failure: %s at %s — agent container exited without cleanup -->\n",
			agentID, time.Now().UTC().Format(time.RFC3339))
		closeErr := f.Close()
		if writeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", claimed.Filename, writeErr)
		} else if closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", claimed.Filename, closeErr)
		}
	}

	fmt.Printf("Recovered task %s after agent exit\n", claimed.Filename)
}

func hasModelArg(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "--model" {
			return true
		}
		if strings.HasPrefix(arg, "--model=") {
			return true
		}
	}
	return false
}

// buildDependencyContext collects completion details for all resolved
// dependencies of the given task and returns them as a JSON array string.
// Returns "" if the task has no dependencies or none have completion files.
func buildDependencyContext(tasksDir string, claimed *queue.ClaimedTask) string {
	meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath)
	if err != nil || len(meta.DependsOn) == 0 {
		return ""
	}
	var details []messaging.CompletionDetail
	for _, dep := range meta.DependsOn {
		detail, err := messaging.ReadCompletionDetail(tasksDir, dep)
		if err != nil {
			continue
		}
		details = append(details, *detail)
	}
	if len(details) == 0 {
		return ""
	}
	data, err := json.Marshal(details)
	if err != nil {
		return ""
	}
	return string(data)
}

// configureReceiveDeny sets receive.denyCurrentBranch=updateInstead on the
// host repo so that Docker agent clones can push back into the checked-out
// target branch. Returns an error if the git config command fails.
func configureReceiveDeny(repoRoot string) error {
	_, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead")
	return err
}

// checkIdleTransition returns true when the system transitions from active to
// idle, so the caller should print the idle message exactly once per idle period.
func checkIdleTransition(isIdle bool, wasIdle *bool) bool {
	shouldPrint := isIdle && !*wasIdle
	*wasIdle = isIdle
	return shouldPrint
}

// extractFailureLines reads a task file and returns all failure record lines
// (lines containing "<!-- failure:") joined by newlines.
// Returns "" if the file has no failure records or cannot be read.
func extractFailureLines(taskPath string) string {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "<!-- failure:") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return strings.Join(lines, "\n")
}

// reviewCandidates scans ready-for-review/ and returns all review candidates
// sorted by priority (ascending) then filename. Tasks whose review retry
// budget is exhausted are moved to failed/ and excluded from the result.
func reviewCandidates(tasksDir string) []*queue.ClaimedTask {
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	entries, err := os.ReadDir(reviewDir)
	if err != nil {
		return nil
	}

	failedDir := filepath.Join(tasksDir, "failed")

	type candidate struct {
		task     *queue.ClaimedTask
		priority int
	}

	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(reviewDir, entry.Name())
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse review candidate %s: %v\n", entry.Name(), err)
			continue
		}

		// Check review retry budget before including as a candidate.
		maxRetries := meta.MaxRetries
		failures := queue.CountFailureLines(path)
		if failures >= maxRetries {
			dst := filepath.Join(failedDir, entry.Name())
			if mvErr := os.Rename(path, dst); mvErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: %v\n", entry.Name(), mvErr)
			} else {
				fmt.Printf("review retry budget exhausted for %s (%d failures >= max_retries %d), moved to failed/\n",
					entry.Name(), failures, maxRetries)
			}
			continue
		}

		branch := parseBranchFromTaskFile(path)
		if branch == "" {
			branch = "task/" + frontmatter.SanitizeBranchName(entry.Name())
		}
		title := frontmatter.ExtractTitle(entry.Name(), body)
		candidates = append(candidates, candidate{
			task: &queue.ClaimedTask{
				Filename: entry.Name(),
				Branch:   branch,
				Title:    title,
				TaskPath: path,
			},
			priority: meta.Priority,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].task.Filename < candidates[j].task.Filename
	})

	result := make([]*queue.ClaimedTask, len(candidates))
	for i, c := range candidates {
		result[i] = c.task
	}
	return result
}

// selectTaskForReview scans ready-for-review/ and returns the highest-priority
// task that needs review. Returns nil if no tasks need review.
// This does not acquire a lock; use selectAndLockReview for mutual exclusion.
func selectTaskForReview(tasksDir string) *queue.ClaimedTask {
	candidates := reviewCandidates(tasksDir)
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

// selectAndLockReview returns the highest-priority review candidate that this
// agent can exclusively lock, along with a cleanup function to release the
// lock. Returns (nil, nil) when no unlocked review task is available.
func selectAndLockReview(tasksDir string) (*queue.ClaimedTask, func()) {
	for _, task := range reviewCandidates(tasksDir) {
		cleanup, ok := queue.AcquireReviewLock(tasksDir, task.Filename)
		if ok {
			return task, cleanup
		}
	}
	return nil, nil
}

// parseBranchFromTaskFile reads a task file and extracts the branch name from
// a <!-- branch: ... --> HTML comment. Returns "" if not found.
func parseBranchFromTaskFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := reviewBranchRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func runReview(cfg dockerConfig, task *queue.ClaimedTask, branch string) error {
	cfg.prompt = strings.ReplaceAll(reviewInstructions, "TASKS_DIR_PLACEHOLDER", cfg.workdir+"/.tasks")
	cfg.prompt = strings.ReplaceAll(cfg.prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	cfg.prompt = strings.ReplaceAll(cfg.prompt, "MESSAGES_DIR_PLACEHOLDER", cfg.workdir+"/.tasks/messages")

	cloneDir, err := git.CreateClone(cfg.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone for review: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureReceiveDeny(cfg.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	cfg.cloneDir = cloneDir

	fmt.Printf("Launching review agent from %s (clone: %s)\n", cfg.repoRoot, cloneDir)

	extraEnvs := []string{
		"MATO_REVIEW_MODE=1",
		"MATO_TASK_FILE=" + task.Filename,
		"MATO_TASK_BRANCH=" + task.Branch,
		"MATO_TASK_TITLE=" + task.Title,
		fmt.Sprintf("MATO_TASK_PATH=%s/.tasks/ready-for-review/%s", cfg.workdir, task.Filename),
	}

	args := buildDockerArgs(cfg, extraEnvs, nil)

	cmd := exec.Command("docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// extractReviewRejections returns all review-rejection comment lines from a task file,
// joined by newlines. Returns "" if none found or file cannot be read.
func extractReviewRejections(taskPath string) string {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return ""
	}
	var rejections []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "<!-- review-rejection:") {
			rejections = append(rejections, strings.TrimSpace(line))
		}
	}
	return strings.Join(rejections, "\n")
}
