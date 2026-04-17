// Package runner manages the agent lifecycle including Docker-based task
// execution, review orchestration, and the top-level poll loop that drives
// claiming, running, and merging tasks.
package runner

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/identity"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/queueview"
	"mato/internal/ui"
)

//go:embed task-instructions.md
var taskInstructions string

//go:embed review-instructions.md
var reviewInstructions string

// gracefulShutdownDelay is the time to wait after sending SIGTERM to a
// Docker container before escalating to SIGKILL.
const gracefulShutdownDelay = 10 * time.Second

// RunMode controls how long runner.Run keeps polling before exiting.
type RunMode int

const (
	// RunModeDaemon is the default long-running polling loop.
	RunModeDaemon RunMode = iota
	// RunModeOnce runs exactly one poll iteration, then exits.
	RunModeOnce
	// RunModeUntilIdle keeps polling until no actionable queue work remains.
	RunModeUntilIdle
)

// RunOptions holds configuration values for a mato run.
//
// TaskModel, ReviewModel, TaskReasoningEffort, and ReviewReasoningEffort
// must already be resolved to non-empty values before calling Run or DryRun.
// DockerImage, AgentTimeout, RetryCooldown, and Mode may be left zero to use
// downstream defaults.
type RunOptions struct {
	DockerImage                string
	Mode                       RunMode
	TaskModel                  string
	ReviewModel                string
	ReviewSessionResumeEnabled bool
	TaskReasoningEffort        string
	ReviewReasoningEffort      string
	AgentTimeout               time.Duration
	RetryCooldown              time.Duration
	Verbose                    bool
}

func normalizeAndValidateRunOptions(opts RunOptions) (RunOptions, error) {
	opts.TaskModel = strings.TrimSpace(opts.TaskModel)
	opts.ReviewModel = strings.TrimSpace(opts.ReviewModel)
	opts.TaskReasoningEffort = strings.TrimSpace(opts.TaskReasoningEffort)
	opts.ReviewReasoningEffort = strings.TrimSpace(opts.ReviewReasoningEffort)

	switch opts.Mode {
	case RunModeDaemon, RunModeOnce, RunModeUntilIdle:
	default:
		return opts, fmt.Errorf("invalid run mode %d", opts.Mode)
	}

	if opts.TaskModel == "" {
		return opts, ui.WithHint(fmt.Errorf("task model must not be empty"), "set it with --task-model, MATO_TASK_MODEL, or task_model in .mato.yaml")
	}
	if opts.ReviewModel == "" {
		return opts, ui.WithHint(fmt.Errorf("review model must not be empty"), "set it with --review-model, MATO_REVIEW_MODEL, or review_model in .mato.yaml")
	}
	if opts.TaskReasoningEffort == "" {
		return opts, ui.WithHint(fmt.Errorf("task reasoning effort must not be empty"), "set it with --task-reasoning-effort, MATO_TASK_REASONING_EFFORT, or task_reasoning_effort in .mato.yaml")
	}
	if opts.ReviewReasoningEffort == "" {
		return opts, ui.WithHint(fmt.Errorf("review reasoning effort must not be empty"), "set it with --review-reasoning-effort, MATO_REVIEW_REASONING_EFFORT, or review_reasoning_effort in .mato.yaml")
	}

	return opts, nil
}

// DryRun validates the task queue setup without launching Docker containers.
// It runs one iteration of queue management (dependency promotion, overlap
// detection, manifest writing) and reports the results to w, then exits.
func DryRun(w io.Writer, repoRoot, branch string, opts RunOptions) error {
	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	opts, err = normalizeAndValidateRunOptions(opts)
	if err != nil {
		return fmt.Errorf("validate run options: %w", err)
	}
	emitRunStartupSummary("dry-run", branch, opts)

	tasksDir := filepath.Join(repoRoot, dirs.Root)

	subdirs := dirs.All

	// Verify directory structure
	missingDirs := 0
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			ui.Warnf("warning: missing directory: %s/\n", sub)
			missingDirs++
		}
	}
	if missingDirs > 0 {
		return fmt.Errorf("%d required queue directories missing — run `mato init` to create them", missingDirs)
	}

	// Build a single shared index for all sections.
	idx := queueview.BuildIndex(tasksDir)
	surfaceBuildWarnings(idx)

	r := &DryRunRenderer{
		W:     w,
		Color: ui.NewColorSet(),
		Width: writerWidthFn(w, defaultDryRunWidth),
	}

	parseFailures := idx.ParseFailures()
	totalTasks := len(parseFailures)
	for _, sub := range subdirs {
		totalTasks += len(idx.TasksByState(sub))
	}
	r.RenderValidation(parseFailures, totalTasks)

	promotable := queue.CountPromotableWaitingTasks(tasksDir, idx)
	r.RenderDependencyResolution(promotable)

	r.RenderDependencySummary(tasksDir, idx)

	backlogView := queueview.ComputeRunnableBacklogView(tasksDir, idx)
	r.RenderAffectsConflicts(backlogView)

	deferredSet := make(map[string]struct{}, len(backlogView.Deferred))
	for name := range backlogView.Deferred {
		deferredSet[name] = struct{}{}
	}
	r.RenderExecutionOrder(backlogView.Runnable)

	r.RenderBacklogSummary(idx, deferredSet, backlogView.DependencyBlocked)

	r.RenderResolvedSettings(opts)

	parseFailuresByDir := make(map[string]int)
	for _, pf := range parseFailures {
		parseFailuresByDir[pf.State]++
	}
	r.RenderQueueSummary(idx, subdirs, parseFailuresByDir, len(backlogView.Deferred))

	fmt.Fprintln(r.W, "\nDry run complete (read-only). No files were modified and no Docker containers were launched.")
	return nil
}

func Run(repoRoot, branch string, opts RunOptions) error {
	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	opts, err = normalizeAndValidateRunOptions(opts)
	if err != nil {
		return fmt.Errorf("validate run options: %w", err)
	}

	branchResult, err := git.EnsureBranch(repoRoot, branch)
	if err != nil {
		return fmt.Errorf("ensure branch: %w", err)
	}
	reportBranchResolution(branchResult)
	emitRunStartupSummary("run", branchResult.Branch, opts)

	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := checkDocker(); err != nil {
		return fmt.Errorf("check docker: %w", err)
	}

	for _, sub := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create %s subdirectory %s: %w", dirs.Root, sub, err)
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

	gitName, gitEmail := resolveGitIdentity(repoRoot)

	if err := ensureTasksDirGitignore(repoRoot); err != nil {
		return fmt.Errorf("ensure %s/ is gitignored: %w", dirs.Root, err)
	}

	tools, err := discoverHostTools()
	if err != nil {
		return fmt.Errorf("discover host tools: %w", err)
	}

	cfg, run := buildEnvAndRunContext(branch, tools, agentID, gitName, gitEmail, repoRoot, tasksDir, opts)

	ctx, cancel := setupSignalContext()
	defer cancel()
	defer signal.Stop(signalChan(ctx))

	if err := ensureDockerImage(ctx, cfg.image); err != nil {
		return fmt.Errorf("ensure docker image: %w", err)
	}

	cleanStaleClones(os.TempDir(), time.Now(), staleCloneMaxAge)

	return pollLoop(ctx, cfg, run, repoRoot, tasksDir, branch, agentID, opts.RetryCooldown, opts.Mode)
}

func ensureTasksDirGitignore(repoRoot string) error {
	ignorePattern := "/" + dirs.Root + "/"

	contains, err := gitignoreContains(repoRoot, ignorePattern)
	if err != nil {
		return err
	}
	if contains {
		return nil
	}

	dirty, err := pathHasLocalChanges(repoRoot, ".gitignore")
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("cannot update .gitignore: file has local changes; commit, stash, or discard them first")
	}

	changed, err := git.EnsureGitignoreContains(repoRoot, ignorePattern)
	if err != nil {
		return fmt.Errorf("update .gitignore: %w", err)
	}
	if !changed {
		return nil
	}
	if err := git.CommitGitignore(repoRoot, "chore: add "+ignorePattern+" to .gitignore"); err != nil {
		return fmt.Errorf("commit .gitignore: %w", err)
	}
	return nil
}

func gitignoreContains(repoRoot, pattern string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read .gitignore: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return true, nil
		}
	}
	return false, nil
}

func pathHasLocalChanges(repoRoot, path string) (bool, error) {
	out, err := git.Output(repoRoot, "status", "--porcelain", "--", path)
	if err != nil {
		return false, fmt.Errorf("check %s status: %w", path, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// buildEnvAndRunContext assembles the envConfig and runContext from resolved
// host tools, agent identity, and runtime settings.
func buildEnvAndRunContext(branch string, tools hostTools, agentID, gitName, gitEmail, repoRoot, tasksDir string, opts RunOptions) (envConfig, runContext) {
	image := resolvedDockerImage(opts)
	timeout := resolvedAgentTimeout(opts)
	workdir := "/workspace"

	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/"+dirs.Root)
	prompt = strings.ReplaceAll(prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	prompt = strings.ReplaceAll(prompt, "MESSAGES_DIR_PLACEHOLDER", workdir+"/"+dirs.Root+"/messages")

	env := envConfig{
		image:                      image,
		workdir:                    workdir,
		copilotPath:                tools.copilotPath,
		copilotRuntimeRoot:         tools.copilotRuntimeRoot,
		copilotBinDir:              tools.copilotBinDir,
		gitPath:                    tools.gitPath,
		gitUploadPackPath:          tools.gitUploadPackPath,
		gitReceivePackPath:         tools.gitReceivePackPath,
		ghPath:                     tools.ghPath,
		goplsPath:                  tools.goplsPath,
		goRoot:                     tools.goRoot,
		hostGoModCache:             tools.goModCache,
		hostGoBuildCache:           tools.goBuildCache,
		copilotConfigDir:           tools.copilotConfigDir,
		copilotCacheDir:            tools.copilotCacheDir,
		gitName:                    gitName,
		gitEmail:                   gitEmail,
		homeDir:                    tools.homeDir,
		ghConfigDir:                tools.ghConfigDir,
		hasGhConfig:                tools.hasGhConfig,
		gitTemplatesDir:            tools.gitTemplatesDir,
		hasGitTemplates:            tools.hasGitTemplates,
		systemCertsDir:             tools.systemCertsDir,
		hasSystemCerts:             tools.hasSystemCerts,
		warnMissingGopls:           tools.goplsPath == "",
		repoRoot:                   repoRoot,
		tasksDir:                   tasksDir,
		targetBranch:               branch,
		reviewModel:                opts.ReviewModel,
		reviewReasoningEffort:      opts.ReviewReasoningEffort,
		reviewSessionResumeEnabled: opts.ReviewSessionResumeEnabled,
		verbose:                    opts.Verbose,
		isTTY:                      isTerminal(os.Stdin),
	}

	run := runContext{
		prompt:          prompt,
		agentID:         agentID,
		model:           opts.TaskModel,
		reasoningEffort: opts.TaskReasoningEffort,
		timeout:         timeout,
	}

	return env, run
}
