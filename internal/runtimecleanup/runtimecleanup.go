// Package runtimecleanup removes runtime state for terminal task transitions.
package runtimecleanup

import (
	"fmt"
	"os"

	"mato/internal/sessionmeta"
	"mato/internal/taskfile"
	"mato/internal/taskstate"
)

// DeleteAll removes taskstate, sessionmeta, and any preserved review verdict
// file for a task. Failures are warnings because cleanup is best-effort and
// periodic sweeps backstop it.
func DeleteAll(tasksDir, filename string) {
	if err := taskstate.Delete(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete taskstate for %s: %v\n", filename, err)
	}
	if err := sessionmeta.DeleteAll(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete sessionmeta for %s: %v\n", filename, err)
	}
	if err := taskfile.DeleteVerdict(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete verdict for %s: %v\n", filename, err)
	}
}
