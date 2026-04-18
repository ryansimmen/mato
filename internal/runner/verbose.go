package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/config"
	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/queue"
	"github.com/ryansimmen/mato/internal/queueview"
)

type pollVerboseSummary struct {
	backlog string
	review  string
	merge   string
}

type pollVerboseSummaryKey struct{}

func withPollVerboseSummary(ctx context.Context, summary *pollVerboseSummary) context.Context {
	if summary == nil {
		return ctx
	}
	return context.WithValue(ctx, pollVerboseSummaryKey{}, summary)
}

func pollVerboseSummaryFromContext(ctx context.Context) *pollVerboseSummary {
	summary, _ := ctx.Value(pollVerboseSummaryKey{}).(*pollVerboseSummary)
	return summary
}

func verbosef(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stderr, "[mato] verbose: "+format+"\n", args...)
}

func resolvedDockerImage(opts RunOptions) string {
	if image := strings.TrimSpace(opts.DockerImage); image != "" {
		return image
	}
	return config.DefaultDockerImage
}

func resolvedAgentTimeout(opts RunOptions) time.Duration {
	if opts.AgentTimeout > 0 {
		return opts.AgentTimeout
	}
	return config.DefaultAgentTimeout
}

func resolvedRetryCooldown(opts RunOptions) time.Duration {
	if opts.RetryCooldown > 0 {
		return opts.RetryCooldown
	}
	return config.DefaultRetryCooldown
}

func emitRunStartupSummary(mode, branch string, opts RunOptions) {
	verbosef(opts.Verbose,
		"%s startup: branch=%s image=%s task_model=%s review_model=%s retry_cooldown=%s agent_timeout=%s",
		mode,
		branch,
		resolvedDockerImage(opts),
		opts.TaskModel,
		opts.ReviewModel,
		formatDurationShort(resolvedRetryCooldown(opts)),
		formatDurationShort(resolvedAgentTimeout(opts)),
	)
}

func emitDockerLaunchSummary(enabled bool, action, image, model string, task *queue.ClaimedTask) {
	if task != nil {
		verbosef(enabled, "docker launch: action=%s task=%s branch=%s image=%s model=%s",
			action, task.Filename, task.Branch, image, model)
		return
	}
	verbosef(enabled, "docker launch: action=%s image=%s model=%s", action, image, model)
}

func summarizeWaitingState(tasksDir string, idx *queueview.PollIndex) string {
	promotable := queue.CountPromotableWaitingTasks(tasksDir, idx)
	waitingCount := len(idx.TasksByState(dirs.Waiting))
	switch {
	case promotable > 0:
		return fmt.Sprintf("%d promotable", promotable)
	case waitingCount == 0:
		return "none"
	default:
		return fmt.Sprintf("%d blocked", waitingCount)
	}
}

func summarizeBacklogState(view queueview.RunnableBacklogView, failedDirExcluded map[string]struct{}) string {
	runnableCount := len(queueview.OrderedRunnableFilenames(view, failedDirExcluded))
	switch {
	case runnableCount > 0:
		return fmt.Sprintf("%d runnable", runnableCount)
	case len(view.Deferred) > 0 && len(view.DependencyBlocked) > 0:
		return fmt.Sprintf("none (%d deferred, %d dependency-blocked)", len(view.Deferred), len(view.DependencyBlocked))
	case len(view.Deferred) > 0:
		return fmt.Sprintf("none (%d deferred)", len(view.Deferred))
	case len(view.DependencyBlocked) > 0:
		return fmt.Sprintf("none (%d dependency-blocked)", len(view.DependencyBlocked))
	default:
		return "none"
	}
}

func summarizeReviewOutcome(tasksDir string, task *queue.ClaimedTask) string {
	if task == nil {
		return "no review tasks"
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyMerge, task.Filename)); err == nil {
		return fmt.Sprintf("approved %s", task.Filename)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, task.Filename)); err == nil {
		return fmt.Sprintf("rejected %s", task.Filename)
	}
	if _, err := os.Stat(task.TaskPath); err == nil {
		return fmt.Sprintf("incomplete %s", task.Filename)
	}
	return fmt.Sprintf("processed %s", task.Filename)
}

func emitPollCycleSummary(tasksDir string, idx *queueview.PollIndex, view queueview.RunnableBacklogView, failedDirExcluded map[string]struct{}, result iterationResult, summary *pollVerboseSummary) {
	if summary == nil {
		return
	}

	waiting := summarizeWaitingState(tasksDir, idx)
	backlog := summarizeBacklogState(view, failedDirExcluded)
	if summary.backlog != "" {
		backlog = summary.backlog
	}

	review := "no review tasks"
	if result.hasReviewTasks {
		review = "review pending"
	}
	if summary.review != "" {
		review = summary.review
	}

	mergeSummary := "no ready tasks"
	if result.hasReadyMerge {
		mergeSummary = "merge pending"
	}
	if summary.merge != "" {
		mergeSummary = summary.merge
	}

	actionable := queue.CountPromotableWaitingTasks(tasksDir, idx) > 0 ||
		result.claimedTask ||
		result.reviewProcessed ||
		result.mergeCount > 0 ||
		len(queueview.OrderedRunnableFilenames(view, failedDirExcluded)) > 0 ||
		result.hasReviewTasks ||
		result.hasReadyMerge

	verbosef(true, "poll actionable=%t; waiting=%s; backlog=%s; review=%s; merge=%s",
		actionable, waiting, backlog, review, mergeSummary)
}
