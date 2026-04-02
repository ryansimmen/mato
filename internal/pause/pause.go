package pause

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/atomicwrite"
)

// ProblemKind classifies non-fatal pause sentinel read problems.
type ProblemKind int

const (
	ProblemNone ProblemKind = iota
	ProblemUnreadable
	ProblemMalformed
)

// State holds the result of reading the .mato/.paused sentinel.
// Since is always UTC when ProblemKind == ProblemNone and Active == true.
type State struct {
	Active      bool
	Since       time.Time
	ProblemKind ProblemKind
	Problem     string
}

// PauseResult describes what Pause did. Since is always UTC-normalized.
type PauseResult struct {
	AlreadyPaused bool      `json:"already_paused"`
	Repaired      bool      `json:"repaired"`
	Since         time.Time `json:"since"`
}

// ResumeResult describes what Resume did.
type ResumeResult struct {
	WasActive bool `json:"was_active"`
}

var statFn = os.Stat
var readFileFn = os.ReadFile
var writeFileFn = atomicwrite.WriteFile

func sentinelPath(tasksDir string) string {
	return filepath.Join(tasksDir, ".paused")
}

// Read reads the durable pause sentinel from tasksDir.
func Read(tasksDir string) (State, error) {
	path := sentinelPath(tasksDir)
	if _, err := statFn(path); err != nil {
		if os.IsNotExist(err) {
			return State{Active: false}, nil
		}
		return State{}, fmt.Errorf("stat pause sentinel %s: %w", path, err)
	}

	data, err := readFileFn(path)
	if err != nil {
		return State{
			Active:      true,
			ProblemKind: ProblemUnreadable,
			Problem:     fmt.Sprintf("unreadable: %v", err),
		}, nil
	}

	trimmed := strings.TrimSpace(string(data))
	ts, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return State{
			Active:      true,
			ProblemKind: ProblemMalformed,
			Problem:     fmt.Sprintf("invalid timestamp: %q", trimmed),
		}, nil
	}

	return State{Active: true, Since: ts.UTC()}, nil
}

// Pause writes or repairs the pause sentinel in tasksDir.
func Pause(tasksDir string, now time.Time) (PauseResult, error) {
	state, err := Read(tasksDir)
	if err != nil {
		return PauseResult{}, err
	}

	now = now.UTC()
	path := sentinelPath(tasksDir)
	content := []byte(now.Format(time.RFC3339) + "\n")

	if !state.Active {
		if err := writeFileFn(path, content); err != nil {
			return PauseResult{}, fmt.Errorf("write pause sentinel %s: %w", path, err)
		}
		return PauseResult{Since: now}, nil
	}

	if state.ProblemKind == ProblemNone {
		return PauseResult{AlreadyPaused: true, Since: state.Since.UTC()}, nil
	}

	if err := writeFileFn(path, content); err != nil {
		return PauseResult{}, fmt.Errorf("repair pause sentinel %s after %s: %w", path, problemLabel(state.ProblemKind), err)
	}
	return PauseResult{Repaired: true, Since: now}, nil
}

// Resume removes the pause sentinel from tasksDir.
func Resume(tasksDir string) (ResumeResult, error) {
	path := sentinelPath(tasksDir)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return ResumeResult{WasActive: false}, nil
		}
		return ResumeResult{}, fmt.Errorf("remove pause sentinel %s: %w", path, err)
	}
	return ResumeResult{WasActive: true}, nil
}

func problemLabel(kind ProblemKind) string {
	switch kind {
	case ProblemUnreadable:
		return "unreadable sentinel"
	case ProblemMalformed:
		return "malformed sentinel"
	default:
		return "unknown problem"
	}
}
