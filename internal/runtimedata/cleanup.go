package runtimedata

import (
	"fmt"
	"os"

	"mato/internal/taskfile"
)

// DeleteRuntimeArtifacts removes taskstate, sessionmeta, and any preserved
// review verdict file for a task. Failures are warnings because cleanup is
// best-effort and periodic sweeps backstop it.
func DeleteRuntimeArtifacts(tasksDir, filename string) {
	if err := DeleteTaskState(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete taskstate for %s: %v\n", filename, err)
	}
	if err := DeleteAllSessions(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete sessionmeta for %s: %v\n", filename, err)
	}
	if err := taskfile.DeleteVerdict(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete verdict for %s: %v\n", filename, err)
	}
}

// DeleteRuntimeArtifactsPreservingVerdict removes taskstate and sessionmeta but
// keeps the preserved review verdict file. Use this for retry-exhausted
// transitions into failed/ where the verdict fallback may still be needed by a
// subsequent mato retry cycle.
func DeleteRuntimeArtifactsPreservingVerdict(tasksDir, filename string) {
	if err := DeleteTaskState(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete taskstate for %s: %v\n", filename, err)
	}
	if err := DeleteAllSessions(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete sessionmeta for %s: %v\n", filename, err)
	}
}
