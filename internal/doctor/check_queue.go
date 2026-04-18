package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/messaging"
)

// ---------- D. Queue Layout ----------

func checkQueueLayout(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "queue", Status: CheckRan, Findings: []Finding{}}

	// Check if tasksDir itself exists.
	if info, err := osStatFn(cc.tasksDir); err != nil {
		if os.IsNotExist(err) {
			// Missing root .mato is fixable — create it along with
			// all expected subdirectories below.
			f := Finding{
				Code:     "queue.missing_tasks_root",
				Severity: SeverityError,
				Message:  fmt.Sprintf("tasks directory does not exist: %s", cc.tasksDir),
				Path:     cc.tasksDir,
				Fixable:  true,
			}
			if cc.opts.Fix {
				if mkErr := os.MkdirAll(cc.tasksDir, 0o755); mkErr == nil {
					f.Fixed = true
					f.Fixable = false
				}
			}
			cr.Findings = append(cr.Findings, f)
		} else {
			// Non-ENOENT error (permission denied, etc.) is a hard error.
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.unreadable_tasks_dir",
				Severity: SeverityError,
				Message:  fmt.Sprintf("tasks directory not readable: %v", err),
				Path:     cc.tasksDir,
			})
			return cr
		}
	} else if !info.IsDir() {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "queue.not_a_directory",
			Severity: SeverityError,
			Message:  fmt.Sprintf("tasks path exists but is not a directory: %s", cc.tasksDir),
			Path:     cc.tasksDir,
		})
		return cr
	}

	// Check for expected directories.
	var expectedDirs []string
	expectedDirs = append(expectedDirs, dirs.All...)
	expectedDirs = append(expectedDirs, ".locks")
	expectedDirs = append(expectedDirs, messaging.MessagingDirs...)

	for _, dir := range expectedDirs {
		dirPath := filepath.Join(cc.tasksDir, dir)
		info, err := osStatFn(dirPath)
		if os.IsNotExist(err) {
			f := Finding{
				Code:     "queue.missing_dir",
				Severity: SeverityError,
				Message:  fmt.Sprintf("missing directory: %s", dir),
				Path:     dirPath,
				Fixable:  true,
			}

			if cc.opts.Fix {
				if mkErr := os.MkdirAll(dirPath, 0o755); mkErr == nil {
					// Re-scan to verify.
					if _, statErr := osStatFn(dirPath); statErr == nil {
						f.Fixed = true
						f.Fixable = false
					}
				}
			}

			cr.Findings = append(cr.Findings, f)
		} else if err != nil {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.unreadable_dir",
				Severity: SeverityError,
				Message:  fmt.Sprintf("could not stat expected directory %s: %v", dir, err),
				Path:     dirPath,
			})
		} else if err == nil && !info.IsDir() {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.not_a_directory",
				Severity: SeverityError,
				Message:  fmt.Sprintf("expected directory but found a file: %s", dir),
				Path:     dirPath,
			})
		}
	}

	// Build index to get per-directory counts and build warnings.
	idx := cc.ensureIndex()

	// Surface directory-level read errors from BuildWarnings.
	for _, bw := range idx.BuildWarnings() {
		// Only report directory-level errors here (not glob warnings).
		// Glob warnings are handled in the tasks check.
		if !strings.Contains(bw.Err.Error(), "glob") && !strings.Contains(bw.Err.Error(), "affects") {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.read_error",
				Severity: SeverityError,
				Message:  fmt.Sprintf("directory read error: %v", bw.Err),
				Path:     bw.Path,
			})
		}
	}

	// Per-directory task counts.
	for _, dir := range dirs.All {
		tasks := idx.TasksByState(dir)
		if len(tasks) > 0 {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.dir_count",
				Severity: SeverityInfo,
				Message:  fmt.Sprintf("%s: %d tasks", dir, len(tasks)),
			})
		}
	}

	return cr
}
