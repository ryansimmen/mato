package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/identity"
	"github.com/ryansimmen/mato/internal/lockfile"
	"github.com/ryansimmen/mato/internal/queue"
	"github.com/ryansimmen/mato/internal/taskfile"
)

// ---------- F. Locks & Orphans ----------

func checkLocksAndOrphans(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "locks", Status: CheckRan, Findings: []Finding{}}

	locksDir := filepath.Join(cc.tasksDir, ".locks")

	// Verify .locks is readable before scanning.
	if _, err := os.ReadDir(locksDir); err != nil {
		sev := SeverityError
		code := "locks.unreadable"
		msg := fmt.Sprintf("cannot read locks directory: %v", err)
		if os.IsNotExist(err) {
			// Missing .locks is already caught by queue-layout check.
			// Report as warning here since it may have been created
			// by --fix in the queue check.
			sev = SeverityWarning
			code = "locks.missing_dir"
			msg = fmt.Sprintf("locks directory does not exist: %s", locksDir)
		}
		cr.Findings = append(cr.Findings, Finding{
			Code:     code,
			Severity: sev,
			Message:  msg,
			Path:     locksDir,
		})
		return cr
	}

	// Scan for stale .pid files.
	cr.Findings = append(cr.Findings, scanStalePIDLocks(locksDir, cc.opts.Fix, cc.tasksDir)...)

	// Scan for stale review locks.
	cr.Findings = append(cr.Findings, scanStaleReviewLocks(locksDir, cc.opts.Fix)...)

	// Scan for orphaned in-progress tasks.
	cr.Findings = append(cr.Findings, scanOrphanedTasks(cc.tasksDir, cc.opts.Fix)...)

	// Count active agents.
	activeCount := countActiveAgents(locksDir)
	if activeCount > 0 {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "locks.active_agents",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("active agent registrations: %d", activeCount),
		})
	}

	return cr
}

// scanStalePIDLocks checks for stale agent .pid files.
func scanStalePIDLocks(locksDir string, fix bool, tasksDir string) []Finding {
	var findings []Finding
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return findings
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".pid")
		status, err := identity.DescribeAgentActivity(tasksDir, agentID)
		if err != nil {
			findings = append(findings, Finding{
				Code:     "locks.unreadable_pid",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("unreadable agent lock: %s", e.Name()),
				Path:     filepath.Join(locksDir, e.Name()),
			})
			continue
		}
		if status == identity.AgentActive {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())

		findings = append(findings, Finding{
			Code:     "locks.stale_pid",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("stale agent lock: %s", e.Name()),
			Path:     lockPath,
			Fixable:  true,
		})
	}

	if fix && len(findings) > 0 {
		for i := range findings {
			if findings[i].Code != "locks.stale_pid" {
				continue
			}
			if err := os.Remove(findings[i].Path); err != nil && !os.IsNotExist(err) {
				findings[i].Message += fmt.Sprintf(" (fix failed: %v)", err)
				continue
			} else if os.IsNotExist(err) {
				findings[i].Fixed = true
				findings[i].Fixable = false
				continue
			}
			if _, statErr := os.Stat(findings[i].Path); os.IsNotExist(statErr) {
				findings[i].Fixed = true
				findings[i].Fixable = false
			}
		}
	}
	return findings
}

// scanStaleReviewLocks checks for stale review-*.lock files.
func scanStaleReviewLocks(locksDir string, fix bool) []Finding {
	var findings []Finding
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return findings
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())
		held, err := lockfile.CheckHeld(lockPath)
		if err != nil {
			findings = append(findings, Finding{
				Code:     "locks.unreadable_review",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("unreadable review lock: %s: %v", e.Name(), err),
				Path:     lockPath,
			})
			continue
		}
		if held {
			continue
		}

		f := Finding{
			Code:     "locks.stale_review",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("stale review lock: %s", e.Name()),
			Path:     lockPath,
			Fixable:  true,
		}

		if fix {
			if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
				f.Message += fmt.Sprintf(" (fix failed: %v)", err)
			}
			// Re-scan to verify.
			if _, statErr := os.Stat(lockPath); os.IsNotExist(statErr) {
				f.Fixed = true
				f.Fixable = false
			}
		}

		findings = append(findings, f)
	}
	return findings
}

// scanOrphanedTasks checks for in-progress tasks claimed by dead agents.
func scanOrphanedTasks(tasksDir string, fix bool) []Finding {
	var findings []Finding

	inProgress := filepath.Join(tasksDir, dirs.InProgress)
	names, err := taskfile.ListTaskFiles(inProgress)
	if err != nil {
		return findings
	}

	for _, name := range names {
		src := filepath.Join(inProgress, name)

		// Check for stale duplicate (already in a later state).
		if laterDir, _ := queue.LaterStateDuplicateDir(name,
			filepath.Join(tasksDir, dirs.ReadyReview),
			filepath.Join(tasksDir, dirs.ReadyMerge),
			filepath.Join(tasksDir, dirs.Completed),
			filepath.Join(tasksDir, dirs.Failed),
		); laterDir != "" {
			findings = append(findings, Finding{
				Code:     "locks.stale_duplicate",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("stale in-progress copy of %s (already in %s/)", name, laterDir),
				Path:     src,
				Fixable:  true,
			})
			continue
		}

		// Check claim marker and agent liveness.
		agent := queue.ParseClaimedBy(src)
		if agent != "" {
			status, err := identity.DescribeAgentActivity(tasksDir, agent)
			if err != nil {
				findings = append(findings, Finding{
					Code:     "locks.unreadable_pid",
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("could not verify claimed-by lock for %s: %v", name, err),
					Path:     src,
				})
				continue
			}
			if status == identity.AgentActive {
				continue // agent is alive, skip
			}
		}

		if agent == "" {
			// No valid claimed-by marker — different corruption case.
			findings = append(findings, Finding{
				Code:     "locks.unclaimed_in_progress",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("in-progress task %s has no claimed-by marker", name),
				Path:     src,
				Fixable:  true,
			})
		} else {
			// Valid claim marker but agent is dead.
			findings = append(findings, Finding{
				Code:     "locks.orphaned_task",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("in-progress task %s claimed by dead agent %s", name, agent),
				Path:     src,
				Fixable:  true,
			})
		}
	}

	if fix && len(findings) > 0 {
		_ = queue.RecoverOrphanedTasks(tasksDir)
		for i := range findings {
			if _, statErr := os.Stat(findings[i].Path); os.IsNotExist(statErr) {
				findings[i].Fixed = true
				findings[i].Fixable = false
			}
		}
	}
	return findings
}

// countActiveAgents counts .pid files for live agents.
func countActiveAgents(locksDir string) int {
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())
		meta, err := lockfile.ReadMetadata(lockPath)
		if err == nil && meta.IsActive() {
			count++
		}
	}
	return count
}
