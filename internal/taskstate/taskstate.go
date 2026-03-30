// Package taskstate stores lightweight host-owned runtime metadata for tasks.
package taskstate

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

const version = 1

// OutcomeWorkBranchPushed marks a task whose branch push succeeded but whose
// queue transition to ready-for-review has not completed yet.
const OutcomeWorkBranchPushed = "work-branch-pushed"

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

// Load returns the stored runtime state for a task. Missing files return
// (nil, nil).
func Load(tasksDir, taskFilename string) (*TaskState, error) {
	statePath, err := pathFor(tasksDir, taskFilename)
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
		state.Version = version
	}
	return &state, nil
}

// Update applies a load-modify-save update to a task's runtime state. Corrupt
// files are replaced with a fresh state.
func Update(tasksDir, taskFilename string, fn func(*TaskState)) error {
	statePath, err := pathFor(tasksDir, taskFilename)
	if err != nil {
		return err
	}
	state := TaskState{Version: version, TaskFile: taskFilename}
	if data, err := os.ReadFile(statePath); err == nil {
		var loaded TaskState
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			state = loaded
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read taskstate %s: %w", statePath, err)
	}
	state.Version = version
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

// Delete removes a task's runtime state file. Missing files are ignored.
func Delete(tasksDir, taskFilename string) error {
	statePath, err := pathFor(tasksDir, taskFilename)
	if err != nil {
		return err
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete taskstate %s: %w", statePath, err)
	}
	return nil
}

// Sweep removes runtime state for tasks that are no longer in non-terminal
// queue directories.
func Sweep(tasksDir string) error {
	runtimeDir := runtimeDir(tasksDir)
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
		if isActive(tasksDir, taskFilename) {
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

func pathFor(tasksDir, taskFilename string) (string, error) {
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
	return filepath.Join(runtimeDir(tasksDir), taskFilename+".json"), nil
}

func runtimeDir(tasksDir string) string {
	return filepath.Join(tasksDir, "runtime", "taskstate")
}

func isActive(tasksDir, taskFilename string) bool {
	for _, dir := range []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge} {
		if _, err := os.Stat(filepath.Join(tasksDir, dir, taskFilename)); err == nil {
			return true
		}
	}
	return false
}
