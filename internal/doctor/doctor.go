// Package doctor performs structured health checks on a mato repository
// and task queue, producing a report with machine-usable exit codes.
package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"mato/internal/dirs"
)

// Severity classifies the severity of a diagnostic finding.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// CheckStatus indicates whether a check was executed or skipped.
type CheckStatus string

const (
	CheckRan     CheckStatus = "ran"
	CheckSkipped CheckStatus = "skipped"
)

// Finding describes a single diagnostic observation.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Path     string   `json:"path,omitempty"`
	Fixable  bool     `json:"fixable,omitempty"`
	Fixed    bool     `json:"fixed,omitempty"`
}

// CheckReport holds the result of a single check category.
type CheckReport struct {
	Name     string      `json:"name"`
	Status   CheckStatus `json:"status"`
	Findings []Finding   `json:"findings"`
}

// Summary aggregates counts of remaining warnings, errors, and fixed items.
type Summary struct {
	Warnings int `json:"warnings"`
	Errors   int `json:"errors"`
	Fixed    int `json:"fixed"`
}

// Report is the complete output of a doctor run.
type Report struct {
	RepoInput string        `json:"repo_input"`
	RepoRoot  string        `json:"repo_root,omitempty"`
	TasksDir  string        `json:"tasks_dir"`
	Checks    []CheckReport `json:"checks"`
	Summary   Summary       `json:"summary"`
	ExitCode  int           `json:"exit_code"`
}

// Options configures a doctor run.
type Options struct {
	Fix         bool
	Format      string   // "text" or "json"
	Only        []string // optional check name filter
	DockerImage string   // resolved docker image (env > config > default)
}

// validCheckNames is the set of valid --only filter values.
var validCheckNames = map[string]bool{
	"git":     true,
	"tools":   true,
	"config":  true,
	"docker":  true,
	"queue":   true,
	"tasks":   true,
	"locks":   true,
	"hygiene": true,
	"deps":    true,
}

// IsValidCheckName reports whether name is a supported doctor check name.
func IsValidCheckName(name string) bool {
	return validCheckNames[name]
}

// Run executes all configured checks and returns a report. The context
// is threaded to checks that perform external probes (e.g., Docker) so
// that Ctrl+C cancellation works cleanly.
//
// repoInput is the raw --repo flag value (or empty for cwd); the git
// check resolves it into the actual root and stores it in the report.
//
// Returns an error only for hard failures (canceled context, internal
// setup errors). Health findings -- including errors -- belong in the
// report, not in the returned error.
func Run(ctx context.Context, repoInput string, opts Options) (Report, error) {
	// Validate --only names.
	onlySet := make(map[string]bool, len(opts.Only))
	for _, name := range opts.Only {
		if !validCheckNames[name] {
			// Build a sorted list of valid names from the checks
			// list (which is already ordered).
			var valid []string
			for _, cd := range checks {
				valid = append(valid, cd.name)
			}
			report := Report{
				RepoInput: repoInput,
				Checks:    make([]CheckReport, 0, len(checks)),
			}
			for _, cd := range checks {
				report.Checks = append(report.Checks, CheckReport{
					Name:     cd.name,
					Status:   CheckSkipped,
					Findings: []Finding{},
				})
			}
			report.Checks = append([]CheckReport{{
				Name:   "doctor",
				Status: CheckRan,
				Findings: []Finding{{
					Code:     "doctor.invalid_only",
					Severity: SeverityError,
					Message:  fmt.Sprintf("unknown --only check name %q; valid names: %s", name, strings.Join(valid, ", ")),
				}},
			}}, report.Checks...)
			report.Summary.Errors = 1
			report.ExitCode = 2
			return report, nil
		}
		onlySet[name] = true
	}

	cc := &checkContext{
		ctx:            ctx,
		repoInput:      repoInput,
		opts:           opts,
		selectedChecks: onlySet,
	}

	// Resolve repoInput to an absolute path for the git check.
	if repoInput == "" {
		cc.repoInput = "."
	}

	// Always resolve the repo root eagerly so that --only filters
	// that skip "git" still have access to repoRoot for deriving
	// tasksDir. This is a no-op if repoRoot is already set.
	cc.resolveRepo()
	if cc.repoRoot != "" {
		cc.tasksDir = filepath.Join(cc.repoRoot, dirs.Root)
	}

	report := Report{
		RepoInput: repoInput,
		TasksDir:  cc.tasksDir,
		RepoRoot:  cc.repoRoot,
		Checks:    make([]CheckReport, 0, len(checks)),
	}

	for _, cd := range checks {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}

		selected := len(onlySet) == 0 || onlySet[cd.name]
		if !selected {
			report.Checks = append(report.Checks, CheckReport{
				Name:     cd.name,
				Status:   CheckSkipped,
				Findings: []Finding{},
			})
			continue
		}

		// For filesystem checks, ensure tasksDir is available.
		needsTasksDir := cd.name == "queue" || cd.name == "tasks" || cd.name == "locks" || cd.name == "hygiene" || cd.name == "deps"
		if needsTasksDir && !cc.hasTasksDir() {
			msg := "cannot determine tasks directory: no valid git repository"
			if cc.repoErr != nil {
				msg = fmt.Sprintf("cannot determine tasks directory (%s)", cc.repoErrDetail())
			}
			report.Checks = append(report.Checks, CheckReport{
				Name:   cd.name,
				Status: CheckRan,
				Findings: []Finding{{
					Code:     cd.name + ".no_tasks_dir",
					Severity: SeverityError,
					Message:  msg,
				}},
			})
			continue
		}

		cr := cd.run(cc)
		report.Checks = append(report.Checks, cr)
	}

	// Compute summary and exit code from remaining findings.
	computeSummary(&report)

	return report, nil
}

// computeSummary populates the Summary and ExitCode fields from the report's
// check findings.
func computeSummary(r *Report) {
	for _, cr := range r.Checks {
		for _, f := range cr.Findings {
			if f.Fixed {
				r.Summary.Fixed++
				continue
			}
			switch f.Severity {
			case SeverityWarning:
				r.Summary.Warnings++
			case SeverityError:
				r.Summary.Errors++
			}
		}
	}

	if r.Summary.Errors > 0 {
		r.ExitCode = 2
	} else if r.Summary.Warnings > 0 {
		r.ExitCode = 1
	} else {
		r.ExitCode = 0
	}
}

// pluralize returns a simple plural form.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// worstSeverity returns the worst non-fixed severity in a list of findings.
func worstSeverity(findings []Finding) Severity {
	worst := SeverityInfo
	for _, f := range findings {
		if f.Fixed {
			continue
		}
		if f.Severity == SeverityError {
			return SeverityError
		}
		if f.Severity == SeverityWarning {
			worst = SeverityWarning
		}
	}
	return worst
}

// categoryIndicator returns the text prefix for a check based on its status
// and worst remaining severity.
func categoryIndicator(cr CheckReport) string {
	if cr.Status == CheckSkipped {
		return "[SKIP]"
	}
	worst := worstSeverity(cr.Findings)
	switch worst {
	case SeverityError:
		return "[ERROR]"
	case SeverityWarning:
		return "[WARN]"
	default:
		return "[OK]"
	}
}

// fixedCount returns the number of fixed findings in a check.
func fixedCount(cr CheckReport) int {
	n := 0
	for _, f := range cr.Findings {
		if f.Fixed {
			n++
		}
	}
	return n
}

// formatSummaryLine produces the one-line header for text output.
func formatSummaryLine(s Summary) string {
	var parts []string
	if s.Errors > 0 {
		parts = append(parts, pluralize(s.Errors, "error", "errors"))
	}
	if s.Warnings > 0 {
		parts = append(parts, pluralize(s.Warnings, "warning", "warnings"))
	}
	if s.Fixed > 0 {
		parts = append(parts, pluralize(s.Fixed, "fixed", "fixed"))
	}
	if len(parts) == 0 {
		return "mato doctor: all checks passed"
	}
	return "mato doctor: " + strings.Join(parts, ", ")
}
