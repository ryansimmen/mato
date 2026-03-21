package runner

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/identity"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/queue"
)

//go:embed task-instructions.md
var taskInstructions string

//go:embed review-instructions.md
var reviewInstructions string

// defaultAgentTimeout is the default execution timeout for Docker agent
// containers. Override with MATO_AGENT_TIMEOUT (Go duration string).
const defaultAgentTimeout = 30 * time.Minute

// gracefulShutdownDelay is the time to wait after sending SIGTERM to a
// Docker container before escalating to SIGKILL.
const gracefulShutdownDelay = 10 * time.Second

const (
	// basePollInterval is the default polling interval between loop iterations.
	basePollInterval = 10 * time.Second

	// maxPollInterval is the upper bound for exponential backoff.
	maxPollInterval = 5 * time.Minute

	// errBackoffThreshold is the number of consecutive poll errors before
	// the loop enters backoff mode.
	errBackoffThreshold = 5
)

// pollBackoff returns the poll interval given the number of consecutive errors.
// Below errBackoffThreshold it returns basePollInterval. Above the threshold it
// doubles the interval for each additional error, capped at maxPollInterval.
func pollBackoff(consecutiveErrors int) time.Duration {
	if consecutiveErrors < errBackoffThreshold {
		return basePollInterval
	}
	d := basePollInterval
	for i := 0; i < consecutiveErrors-errBackoffThreshold+1; i++ {
		d *= 2
		if d >= maxPollInterval {
			return maxPollInterval
		}
	}
	return d
}

// DryRun validates the task queue setup without launching Docker containers.
// It runs one iteration of queue management (dependency promotion, overlap
// detection, manifest writing) and reports the results, then exits.
func DryRun(repoRoot, branch, tasksDirOverride string) error {
	repoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	tasksDir := tasksDirOverride
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}
	tasksDir, err = validateTasksDir(tasksDir)
	if err != nil {
		return err
	}

	subdirs := queue.AllDirs

	// Verify directory structure
	missingDirs := 0
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			fmt.Fprintf(os.Stderr, "warning: missing directory: %s/\n", sub)
			missingDirs++
		}
	}
	if missingDirs > 0 {
		return fmt.Errorf("%d required queue directories missing — run mato once to create them", missingDirs)
	}

	// Parse all task files and report errors
	fmt.Println("=== Task File Validation ===")
	parseErrors := 0
	totalTasks := 0
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			totalTasks++
			path := filepath.Join(dir, e.Name())
			if _, _, parseErr := frontmatter.ParseTaskFile(path); parseErr != nil {
				fmt.Printf("  ERROR %s/%s: %v\n", sub, e.Name(), parseErr)
				parseErrors++
			}
		}
	}
	if parseErrors > 0 {
		fmt.Printf("  %d of %d task file(s) have parse errors\n", parseErrors, totalTasks)
	} else {
		fmt.Printf("  All %d task file(s) parsed successfully\n", totalTasks)
	}

	// Run dependency reconciliation (read-only: report what would be promoted)
	fmt.Println("\n=== Dependency Resolution ===")
	promotable := queue.CountPromotableWaitingTasks(tasksDir)
	if promotable > 0 {
		fmt.Printf("  %d task(s) in waiting/ would be promoted to backlog/\n", promotable)
	} else {
		fmt.Println("  No waiting tasks ready for promotion")
	}

	// Detect affects conflicts
	fmt.Println("\n=== Affects Conflict Detection ===")
	detailed := queue.DeferredOverlappingTasksDetailed(tasksDir)
	if len(detailed) > 0 {
		// Sort deferred task names for stable output.
		names := make([]string, 0, len(detailed))
		for name := range detailed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			info := detailed[name]
			fmt.Printf("  DEFERRED %s (blocked by %s in %s/, overlap: %v)\n",
				name, info.BlockedBy, info.BlockedByDir, info.OverlapFiles)
		}
	} else {
		fmt.Println("  No affects conflicts detected")
	}

	// Compute and display queue manifest (read-only: no file written)
	fmt.Println("\n=== Queue Manifest ===")
	deferredSimple := make(map[string]struct{}, len(detailed))
	for name := range detailed {
		deferredSimple[name] = struct{}{}
	}
	manifest := queue.ComputeQueueManifest(tasksDir, deferredSimple)
	if len(strings.TrimSpace(manifest)) == 0 {
		fmt.Println("  (queue is empty)")
	} else {
		for i, line := range strings.Split(strings.TrimSpace(manifest), "\n") {
			fmt.Printf("  %d. %s\n", i+1, line)
		}
	}

	// Summary counts
	fmt.Println("\n=== Queue Summary ===")
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		entries, _ := os.ReadDir(dir)
		count := 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				count++
			}
		}
		fmt.Printf("  %-20s %d\n", sub, count)
	}
	if len(detailed) > 0 {
		fmt.Printf("  %-20s %d\n", "deferred", len(detailed))
	}

	fmt.Println("\nDry run complete (read-only). No files were modified and no Docker containers were launched.")
	return nil
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
	tasksDir, err = validateTasksDir(tasksDir)
	if err != nil {
		return err
	}

	if err := checkDocker(); err != nil {
		return err
	}

	for _, sub := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create .tasks subdirectory %s: %w", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		return fmt.Errorf("init messaging: %w", err)
	}

	agentID, err := identity.GenerateAgentID()
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

	agentTimeout, err := parseAgentTimeout(os.Getenv("MATO_AGENT_TIMEOUT"))
	if err != nil {
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
	gitUploadPackPath, err := findGitHelper("git-upload-pack")
	if err != nil {
		return err
	}
	gitReceivePackPath, err := findGitHelper("git-receive-pack")
	if err != nil {
		return err
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
		tasksDir:           tasksDir,
		targetBranch:       branch,
		timeout:            agentTimeout,
		isTTY:              isTerminal(os.Stdin),
	}

	// Create a context that is cancelled when SIGINT or SIGTERM is received.
	// This context is passed to runOnce and runReview so that running Docker
	// containers receive a graceful SIGTERM followed by SIGKILL after
	// gracefulShutdownDelay.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	wasIdle := false
	failedDirExcluded := make(map[string]struct{})
	consecutiveErrors := 0
	for {
		pollHadError := false

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
			pollHadError = true
		}

		claimed, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, deferred)
		var fdErr *queue.FailedDirUnavailableError
		if errors.As(claimErr, &fdErr) {
			failedDirExcluded[fdErr.TaskFilename] = struct{}{}
			fmt.Fprintf(os.Stderr, "warning: excluding retry-exhausted task %s from future polls (failed/ directory unavailable)\n", fdErr.TaskFilename)
		} else if claimErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not claim task: %v\n", claimErr)
			pollHadError = true
		}
		claimedTask := claimed != nil
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

			if err := runOnce(ctx, cfg, claimed); err != nil {
				fmt.Fprintf(os.Stderr, "warning: agent run failed: %v\n", err)
			}

			recoverStuckTask(tasksDir, agentID, claimed)
		}

		if reviewTask, reviewCleanup := selectAndLockReview(tasksDir); reviewTask != nil {
			// Verify the task branch exists before launching the review agent.
			if _, err := git.Output(cfg.repoRoot, "rev-parse", "--verify", "refs/heads/"+reviewTask.Branch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: task branch %s missing from host repo, recording review failure for %s\n", reviewTask.Branch, reviewTask.Filename)
				appendReviewFailure(reviewTask.TaskPath, agentID, "task branch "+reviewTask.Branch+" not found in host repo")
			} else {
				fmt.Printf("Reviewing task %s on branch %s\n", reviewTask.Filename, reviewTask.Branch)
				if err := runReview(ctx, cfg, reviewTask, branch); err != nil {
					fmt.Fprintf(os.Stderr, "warning: review agent failed: %v\n", err)
				}
				postReviewAction(tasksDir, agentID, reviewTask)
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
		isIdle := !claimedTask && !hasReviewTasks && !merge.HasReadyTasks(tasksDir)
		if checkIdleTransition(isIdle, &wasIdle) {
			fmt.Println("No tasks found in backlog, ready-for-review, or ready-to-merge. Waiting...")
		}

		if pollHadError {
			consecutiveErrors++
			if consecutiveErrors == errBackoffThreshold {
				fmt.Fprintf(os.Stderr, "warning: entering backoff mode after %d consecutive poll errors\n", consecutiveErrors)
			}
		} else {
			if consecutiveErrors >= errBackoffThreshold {
				fmt.Printf("Poll succeeded, exiting backoff mode (was at %d consecutive errors)\n", consecutiveErrors)
			}
			consecutiveErrors = 0
		}

		select {
		case <-ctx.Done():
			fmt.Println("\nInterrupted. Exiting.")
			return nil
		case <-time.After(pollBackoff(consecutiveErrors)):
		}
	}
}

// checkIdleTransition returns true when the system transitions from active to
// idle, so the caller should print the idle message exactly once per idle period.
func checkIdleTransition(isIdle bool, wasIdle *bool) bool {
	shouldPrint := isIdle && !*wasIdle
	*wasIdle = isIdle
	return shouldPrint
}

// appendToFileFn is the function used to append text to files in post-agent
// and review flows. It is a variable so tests can inject failures.
var appendToFileFn = appendToFile

// appendToFile appends text to a file. Returns an error on failure.
func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s for append: %w", path, err)
	}
	_, writeErr := f.WriteString(text)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("append to %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s after append: %w", path, closeErr)
	}
	return nil
}
