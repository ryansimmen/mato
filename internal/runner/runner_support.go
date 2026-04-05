package runner

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/git"
	"mato/internal/queue"
	"mato/internal/runtimedata"
	"mato/internal/ui"
)

const (
	// idleHeartbeatThreshold is the number of consecutive idle polls before
	// the runner switches from the initial idle message to throttled heartbeats.
	idleHeartbeatThreshold = 5

	// heartbeatInterval is the minimum time between throttled heartbeat
	// messages once the idle threshold is reached.
	heartbeatInterval = time.Minute
)

// idleHeartbeat tracks the idle heartbeat state so the runner can provide
// periodic output when the queue is empty for an extended period.
type idleHeartbeat struct {
	consecutiveIdlePolls int
	lastActivityTime     time.Time
	lastHeartbeatTime    time.Time
	startTime            time.Time
}

// newIdleHeartbeat creates an idleHeartbeat initialised with the given time.
func newIdleHeartbeat(now time.Time) idleHeartbeat {
	return idleHeartbeat{
		startTime:        now,
		lastActivityTime: now,
	}
}

// recordActivity resets the idle counters when work is performed (task
// claimed, merge processed, review completed).
func (h *idleHeartbeat) recordActivity(now time.Time) {
	h.consecutiveIdlePolls = 0
	h.lastActivityTime = now
	h.lastHeartbeatTime = time.Time{}
}

// idleMessage increments the idle counter and returns the message to print,
// or "" if nothing should be printed this cycle. nextPoll is the duration
// until the next poll iteration.
func (h *idleHeartbeat) idleMessage(now time.Time, nextPoll time.Duration) string {
	h.consecutiveIdlePolls++
	if h.consecutiveIdlePolls == 1 {
		return fmt.Sprintf("[mato] idle — waiting for tasks (next poll in %s)", formatDurationShort(nextPoll))
	}
	if h.consecutiveIdlePolls <= idleHeartbeatThreshold {
		return ""
	}
	if !h.lastHeartbeatTime.IsZero() && now.Sub(h.lastHeartbeatTime) < heartbeatInterval {
		return ""
	}
	h.lastHeartbeatTime = now
	uptime := now.Sub(h.startTime)
	lastAct := now.Sub(h.lastActivityTime)
	return fmt.Sprintf("[mato] idle — no tasks available (uptime: %s, last activity: %s ago)",
		formatDurationShort(uptime), formatDurationShort(lastAct))
}

// pausedMessage returns the paused heartbeat message or "" when nothing should
// be printed this cycle. When priorPaused is false, the message is emitted
// immediately and the paused heartbeat cadence is reset.
func (h *idleHeartbeat) pausedMessage(now time.Time, priorPaused bool) string {
	if !priorPaused {
		h.lastHeartbeatTime = now
		return "[mato] paused - run 'mato resume' to continue"
	}
	if !h.lastHeartbeatTime.IsZero() && now.Sub(h.lastHeartbeatTime) < heartbeatInterval {
		return ""
	}
	h.lastHeartbeatTime = now
	return "[mato] paused - run 'mato resume' to continue"
}

// formatDurationShort formats a duration in a compact human-readable form
// (e.g. "10s", "5m", "1h30m").
func formatDurationShort(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func reportBranchResolution(result git.EnsureBranchResult) {
	if result.Source == git.BranchSourceLocal {
		return
	}

	switch result.Source {
	case git.BranchSourceRemoteCached, git.BranchSourceHeadRemoteUnavailable:
		ui.Warnf("warning: using branch %s (%s)\n", result.Branch, result.SourceDescription())
	default:
		fmt.Printf("Using branch %s (%s)\n", result.Branch, result.SourceDescription())
	}
}

// resolveGitIdentity reads git user.name and user.email from the local
// repo config, falling back to global config and defaults via
// git.ResolveIdentity, and ensures both are set on the local repo for
// use inside Docker containers.
func resolveGitIdentity(repoRoot string) (name, email string) {
	return git.EnsureIdentity(repoRoot)
}

// setupSignalContext creates a context.Context that is cancelled when
// SIGINT or SIGTERM is received. The caller must defer both the returned
// cancel function and signal.Stop on the signal channel to ensure the
// signal-listener goroutine exits and signal registration is cleaned up.
func setupSignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Store sigCh in ctx before launching goroutine to avoid data race
	// on the ctx variable.
	ctx = context.WithValue(ctx, signalChanKey{}, sigCh)

	go func() {
		select {
		case <-sigCh:
			fmt.Println("\nShutting down, waiting for current task to finish...")
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}

// signalChanKey is the context key for the signal channel.
type signalChanKey struct{}

// signalChan retrieves the signal channel from a context created by
// setupSignalContext. Returns nil if not present.
func signalChan(ctx context.Context) chan<- os.Signal {
	ch, _ := ctx.Value(signalChanKey{}).(chan os.Signal)
	return ch
}

// cloneDirPrefix is the prefix used by git.CreateClone when creating temp
// directories. Only directories matching this prefix are considered for
// cleanup.
const cloneDirPrefix = "mato-"

// staleCloneMaxAge is the age threshold beyond which a preserved debug clone
// is considered stale and eligible for removal. 24 hours gives operators
// ample time to inspect a failed push before the directory is reclaimed.
const staleCloneMaxAge = 24 * time.Hour

// cleanStaleClones removes clone directories in the given temp directory
// that were preserved for debugging after a failed push (see runOnce in
// task.go).
//
// A directory is removed only when all three conditions are met:
//  1. Its name starts with the "mato-" prefix used by git.CreateClone.
//  2. It contains the ".mato-debug-clone" sentinel marker written by
//     writeDebugMarker when a clone is intentionally preserved after a
//     postAgentPush failure. This positively identifies the directory as
//     a mato debug clone rather than an active or unrelated temp clone.
//  3. Its modification time is older than maxAge relative to now.
//
// This runs once at runner startup, before the poll loop begins, so that
// stale clones from previous runs are reclaimed without risking removal of
// clones that may still be in active use by a currently running agent.
func cleanStaleClones(tmpDir string, now time.Time, maxAge time.Duration) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		ui.Warnf("warning: could not list temp directory for clone cleanup: %v\n", err)
		return
	}

	cutoff := now.Add(-maxAge)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), cloneDirPrefix) {
			continue
		}
		dirPath := filepath.Join(tmpDir, entry.Name())

		// Only remove directories that contain the debug marker,
		// confirming they were intentionally preserved after a
		// postAgentPush failure.
		markerPath := filepath.Join(dirPath, debugMarkerFile)
		if _, err := os.Stat(markerPath); err != nil {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		if err := os.RemoveAll(dirPath); err != nil {
			ui.Warnf("warning: could not remove stale clone %s: %v\n", dirPath, err)
			continue
		}
		fmt.Printf("Cleaned up stale clone directory: %s (age: %s)\n", dirPath, now.Sub(info.ModTime()).Truncate(time.Minute))
	}
}

// surfaceBuildWarnings logs non-fatal build warnings from a PollIndex to
// stderr. It returns true when any warning indicates a directory-level read
// failure (incomplete index), which callers should treat as a poll-cycle error
// to trigger backoff signaling.
func surfaceBuildWarnings(idx *queue.PollIndex) bool {
	warnings := idx.BuildWarnings()
	if len(warnings) == 0 {
		return false
	}
	hasDirReadFailure := false
	for _, w := range warnings {
		ui.Warnf("warning: index build: %s (%s): %v\n", w.Path, w.State, w.Err)
		// Directory-level read failures produce paths without a .md
		// suffix; these mean the index is missing an entire queue
		// directory and downstream scheduling may be distorted.
		if !strings.HasSuffix(w.Path, ".md") {
			hasDirReadFailure = true
		}
	}
	return hasDirReadFailure
}

// appendToFileFn is the function used to append text to files in post-agent
// and review flows. It is a variable so tests can inject failures.
//
// NOTE: Package-level test seam — prevents t.Parallel(). See execCommandContext.
var appendToFileFn = atomicwrite.AppendToFile

func recordTaskStateUpdate(tasksDir, filename, action string, fn func(*runtimedata.TaskState)) {
	if err := runtimedata.UpdateTaskState(tasksDir, filename, fn); err != nil {
		ui.Warnf("warning: could not %s for %s: %v\n", action, filename, err)
	}
}

func recordSessionUpdate(tasksDir, kind, filename, action string, fn func(*runtimedata.Session)) {
	if err := runtimedata.UpdateSession(tasksDir, kind, filename, fn); err != nil {
		ui.Warnf("warning: could not %s for %s: %v\n", action, filename, err)
	}
}

func loadOrCreateSession(tasksDir, kind, filename, branch string) *runtimedata.Session {
	session, err := runtimedata.LoadOrCreateSession(tasksDir, kind, filename, branch)
	if err != nil {
		ui.Warnf("warning: could not prepare %s session for %s: %v\n", kind, filename, err)
		return nil
	}
	return session
}

func resetSession(tasksDir, kind, filename, branch string) string {
	session, err := runtimedata.ResetSessionID(tasksDir, kind, filename, branch)
	if err != nil {
		ui.Warnf("warning: could not reset %s session for %s: %v\n", kind, filename, err)
		return ""
	}
	if session == nil {
		return ""
	}
	return session.CopilotSessionID
}
