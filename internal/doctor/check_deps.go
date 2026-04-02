package doctor

import (
	"fmt"
	"path/filepath"
	"sort"

	"mato/internal/dag"
	"mato/internal/queue"
)

// ---------- H. Dependency Integrity ----------

func checkDependencies(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "deps", Status: CheckRan, Findings: []Finding{}}

	idx := cc.ensureIndex()
	diag := queue.DiagnoseDependencies(cc.tasksDir, idx)

	// Map DependencyIssue entries to Finding structs.
	for _, issue := range diag.Issues {
		var code string
		var sev Severity
		var msg string

		switch issue.Kind {
		case queue.DependencyAmbiguousID:
			code = "deps.ambiguous_id"
			sev = SeverityWarning
			msg = fmt.Sprintf("task ID %q exists in both completed and non-completed directories", issue.TaskID)
		case queue.DependencyDuplicateID:
			code = "deps.duplicate_id"
			sev = SeverityError
			msg = fmt.Sprintf("duplicate waiting task ID %q (retained: %s, duplicate: %s) — duplicate will be moved to failed/", issue.TaskID, issue.DependsOn, issue.Filename)
		case queue.DependencySelfCycle:
			code = "deps.self_dependency"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q depends on itself", issue.TaskID)
		case queue.DependencyCycle:
			code = "deps.cycle"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q is part of a circular dependency", issue.TaskID)
		case queue.DependencyUnknownID:
			code = "deps.unknown_id"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q depends on unknown ID %q", issue.TaskID, issue.DependsOn)
		default:
			code = "deps.unknown_issue"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q: unknown issue kind %s", issue.TaskID, issue.Kind)
		}

		f := Finding{
			Code:     code,
			Severity: sev,
			Message:  msg,
		}
		if issue.Filename != "" {
			f.Path = filepath.Join(cc.tasksDir, queue.DirWaiting, issue.Filename)
		}
		cr.Findings = append(cr.Findings, f)
	}

	// Surface blocked-by-external and blocked-by-ambiguous as INFO.
	for taskID, details := range diag.Analysis.Blocked {
		for _, detail := range details {
			switch detail.Reason {
			case dag.BlockedByExternal:
				cr.Findings = append(cr.Findings, Finding{
					Code:     "deps.blocked_external",
					Severity: SeverityInfo,
					Message:  fmt.Sprintf("task %q blocked by %q (in non-completed, non-waiting state)", taskID, detail.DependencyID),
				})
			case dag.BlockedByAmbiguous:
				cr.Findings = append(cr.Findings, Finding{
					Code:     "deps.blocked_ambiguous",
					Severity: SeverityInfo,
					Message:  fmt.Sprintf("task %q blocked by ambiguous ID %q", taskID, detail.DependencyID),
				})
			}
		}
	}

	blockedBacklog := queue.DependencyBlockedBacklogTasksDetailed(cc.tasksDir, idx)
	blockedNames := make([]string, 0, len(blockedBacklog))
	for name := range blockedBacklog {
		blockedNames = append(blockedNames, name)
	}
	sort.Strings(blockedNames)
	for _, name := range blockedNames {
		details := blockedBacklog[name]
		cr.Findings = append(cr.Findings, Finding{
			Code:     "deps.backlog_blocked",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("backlog task %q is dependency-blocked and should be in waiting/ (blocked by %s)", name, queue.FormatDependencyBlocks(details)),
			Path:     filepath.Join(cc.tasksDir, queue.DirBacklog, name),
		})
	}

	// Deps-satisfied count.
	depsSatisfied := len(diag.Analysis.DepsSatisfied)
	if depsSatisfied > 0 {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "deps.satisfied_count",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("deps-satisfied waiting tasks: %d", depsSatisfied),
		})
	}

	return cr
}
