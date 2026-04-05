// Package runtimedata stores host-owned runtime metadata under .mato/runtime/.
package runtimedata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/dirs"
)

const taskStateVersion = 1

// Outcome constants for TaskState.LastOutcome. These cover the full task
// lifecycle so callers never need raw strings.
const (
	// Work phase
	OutcomeWorkBranchPushed = "work-branch-pushed" // branch push succeeded; queue transition pending
	OutcomeWorkPushed       = "work-pushed"        // branch pushed and task moved to ready-for-review

	// Review phase
	OutcomeReviewLaunched            = "review-launched"              // review agent started
	OutcomeReviewApproved            = "review-approved"              // review approved; task moved to ready-to-merge
	OutcomeReviewRejected            = "review-rejected"              // review rejected; task moved to backlog
	OutcomeReviewError               = "review-error"                 // review agent reported an error
	OutcomeReviewIncomplete          = "review-incomplete"            // verdict missing or unparseable
	OutcomeReviewBranchMissing       = "review-branch-missing"        // task branch does not exist in repo
	OutcomeReviewBranchMarkerMissing = "review-branch-marker-missing" // branch marker missing from task file
	OutcomeReviewMoveFailed          = "review-move-failed"           // failed to move reviewed task

	// Merge phase
	OutcomeMergeConflictCleanup = "merge-conflict-cleanup" // squash merge conflict; task returned to backlog
)

// TaskState records lightweight runtime metadata for a task.
type TaskState struct {
	Version         int    `json:"version"`
	TaskFile        string `json:"task_file"`
	TaskBranch      string `json:"task_branch"`
	TargetBranch    string `json:"target_branch"`
	LastHeadSHA     string `json:"last_head_sha"`
	LastReviewedSHA string `json:"last_reviewed_sha"`
	LastOutcome     string `json:"last_outcome"`
	UpdatedAt       string `json:"updated_at"`
}

// LoadTaskState returns the stored runtime state for a task. Missing files
// return (nil, nil).
func LoadTaskState(tasksDir, taskFilename string) (*TaskState, error) {
	statePath, err := taskStatePathFor(tasksDir, taskFilename)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read taskstate %s: %w", statePath, err)
	}
	var state TaskState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse taskstate %s: %w", statePath, err)
	}
	if strings.TrimSpace(state.TaskFile) == "" {
		state.TaskFile = taskFilename
	}
	if state.Version == 0 {
		state.Version = taskStateVersion
	}
	return &state, nil
}

// UpdateTaskState applies a load-modify-save update to a task's runtime state.
// Corrupt files are replaced with a fresh state.
func UpdateTaskState(tasksDir, taskFilename string, fn func(*TaskState)) error {
	statePath, err := taskStatePathFor(tasksDir, taskFilename)
	if err != nil {
		return err
	}
	state := TaskState{Version: taskStateVersion, TaskFile: taskFilename}
	if data, err := os.ReadFile(statePath); err == nil {
		var loaded TaskState
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			state = loaded
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read taskstate %s: %w", statePath, err)
	}
	state.Version = taskStateVersion
	state.TaskFile = taskFilename
	if fn != nil {
		fn(&state)
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return fmt.Errorf("create taskstate dir for %s: %w", taskFilename, err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal taskstate %s: %w", statePath, err)
	}
	if err := atomicwrite.WriteFile(statePath, append(data, '\n')); err != nil {
		return fmt.Errorf("write taskstate %s: %w", statePath, err)
	}
	return nil
}

// DeleteTaskState removes a task's runtime state file. Missing files are
// ignored.
func DeleteTaskState(tasksDir, taskFilename string) error {
	statePath, err := taskStatePathFor(tasksDir, taskFilename)
	if err != nil {
		return err
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete taskstate %s: %w", statePath, err)
	}
	return nil
}

// SweepTaskState removes runtime state for tasks that are no longer in
// non-terminal queue directories.
func SweepTaskState(tasksDir string) error {
	runtimeDir := taskStateRuntimeDir(tasksDir)
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read taskstate dir %s: %w", runtimeDir, err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		taskFilename := strings.TrimSuffix(entry.Name(), ".json")
		if taskFilename == "" {
			continue
		}
		if dirs.IsActive(tasksDir, taskFilename) {
			continue
		}
		if err := os.Remove(filepath.Join(runtimeDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("delete stale taskstate %s: %w", entry.Name(), err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func taskStatePathFor(tasksDir, taskFilename string) (string, error) {
	tasksDir = strings.TrimSpace(tasksDir)
	if tasksDir == "" {
		return "", fmt.Errorf("tasks directory must not be empty")
	}
	taskFilename = strings.TrimSpace(taskFilename)
	if taskFilename == "" {
		return "", fmt.Errorf("task filename must not be empty")
	}
	if filepath.Base(taskFilename) != taskFilename {
		return "", fmt.Errorf("task filename %q must be a base name", taskFilename)
	}
	return filepath.Join(taskStateRuntimeDir(tasksDir), taskFilename+".json"), nil
}

func taskStateRuntimeDir(tasksDir string) string {
	return filepath.Join(tasksDir, "runtime", "taskstate")
}
