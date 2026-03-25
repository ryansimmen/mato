package doctor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSeveritySuffix(t *testing.T) {
	tests := []struct {
		name    string
		finding Finding
		want    string
	}{
		{
			name:    "info severity returns empty",
			finding: Finding{Severity: SeverityInfo},
			want:    "",
		},
		{
			name:    "warning severity",
			finding: Finding{Severity: SeverityWarning},
			want:    " (warning)",
		},
		{
			name:    "error severity",
			finding: Finding{Severity: SeverityError},
			want:    " (error)",
		},
		{
			name:    "fixable info returns fixable only",
			finding: Finding{Severity: SeverityInfo, Fixable: true},
			want:    " (fixable with --fix)",
		},
		{
			name:    "fixable warning",
			finding: Finding{Severity: SeverityWarning, Fixable: true},
			want:    " (warning, fixable with --fix)",
		},
		{
			name:    "fixable error",
			finding: Finding{Severity: SeverityError, Fixable: true},
			want:    " (error, fixable with --fix)",
		},
		{
			name:    "fixed overrides everything",
			finding: Finding{Severity: SeverityError, Fixable: true, Fixed: true},
			want:    " (fixed)",
		},
		{
			name:    "fixed warning",
			finding: Finding{Severity: SeverityWarning, Fixed: true},
			want:    " (fixed)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := severitySuffix(tt.finding)
			if got != tt.want {
				t.Errorf("severitySuffix() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSummaryLine(t *testing.T) {
	tests := []struct {
		name    string
		summary Summary
		want    string
	}{
		{
			name:    "all passed",
			summary: Summary{},
			want:    "mato doctor: all checks passed",
		},
		{
			name:    "errors only",
			summary: Summary{Errors: 3},
			want:    "mato doctor: 3 errors",
		},
		{
			name:    "single error",
			summary: Summary{Errors: 1},
			want:    "mato doctor: 1 error",
		},
		{
			name:    "warnings only",
			summary: Summary{Warnings: 2},
			want:    "mato doctor: 2 warnings",
		},
		{
			name:    "single warning",
			summary: Summary{Warnings: 1},
			want:    "mato doctor: 1 warning",
		},
		{
			name:    "fixed only",
			summary: Summary{Fixed: 4},
			want:    "mato doctor: 4 fixed",
		},
		{
			name:    "single fixed",
			summary: Summary{Fixed: 1},
			want:    "mato doctor: 1 fixed",
		},
		{
			name:    "errors and warnings",
			summary: Summary{Errors: 2, Warnings: 1},
			want:    "mato doctor: 2 errors, 1 warning",
		},
		{
			name:    "all three",
			summary: Summary{Errors: 1, Warnings: 2, Fixed: 3},
			want:    "mato doctor: 1 error, 2 warnings, 3 fixed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSummaryLine(tt.summary)
			if got != tt.want {
				t.Errorf("formatSummaryLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCategoryIndicator(t *testing.T) {
	tests := []struct {
		name string
		cr   CheckReport
		want string
	}{
		{
			name: "skipped check",
			cr:   CheckReport{Name: "docker", Status: CheckSkipped, Findings: []Finding{}},
			want: "[SKIP]",
		},
		{
			name: "error findings",
			cr: CheckReport{Name: "git", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityError, Message: "fail"},
			}},
			want: "[ERROR]",
		},
		{
			name: "warning findings",
			cr: CheckReport{Name: "tools", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityWarning, Message: "missing"},
			}},
			want: "[WARN]",
		},
		{
			name: "info only",
			cr: CheckReport{Name: "git", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityInfo, Message: "ok"},
			}},
			want: "[OK]",
		},
		{
			name: "no findings",
			cr:   CheckReport{Name: "tasks", Status: CheckRan, Findings: []Finding{}},
			want: "[OK]",
		},
		{
			name: "fixed error shows OK",
			cr: CheckReport{Name: "queue", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityError, Fixed: true, Message: "was bad"},
			}},
			want: "[OK]",
		},
		{
			name: "mixed fixed and warning",
			cr: CheckReport{Name: "locks", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityError, Fixed: true, Message: "was bad"},
				{Severity: SeverityWarning, Message: "still iffy"},
			}},
			want: "[WARN]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := categoryIndicator(tt.cr)
			if got != tt.want {
				t.Errorf("categoryIndicator() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorstSeverity(t *testing.T) {
	tests := []struct {
		name     string
		findings []Finding
		want     Severity
	}{
		{
			name:     "empty findings",
			findings: []Finding{},
			want:     SeverityInfo,
		},
		{
			name:     "info only",
			findings: []Finding{{Severity: SeverityInfo}},
			want:     SeverityInfo,
		},
		{
			name:     "warning present",
			findings: []Finding{{Severity: SeverityInfo}, {Severity: SeverityWarning}},
			want:     SeverityWarning,
		},
		{
			name:     "error present",
			findings: []Finding{{Severity: SeverityInfo}, {Severity: SeverityWarning}, {Severity: SeverityError}},
			want:     SeverityError,
		},
		{
			name:     "fixed errors are ignored",
			findings: []Finding{{Severity: SeverityError, Fixed: true}, {Severity: SeverityWarning}},
			want:     SeverityWarning,
		},
		{
			name:     "all fixed",
			findings: []Finding{{Severity: SeverityError, Fixed: true}, {Severity: SeverityWarning, Fixed: true}},
			want:     SeverityInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := worstSeverity(tt.findings)
			if got != tt.want {
				t.Errorf("worstSeverity() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFixedCount(t *testing.T) {
	tests := []struct {
		name string
		cr   CheckReport
		want int
	}{
		{
			name: "no findings",
			cr:   CheckReport{Findings: []Finding{}},
			want: 0,
		},
		{
			name: "none fixed",
			cr: CheckReport{Findings: []Finding{
				{Severity: SeverityWarning},
				{Severity: SeverityError},
			}},
			want: 0,
		},
		{
			name: "some fixed",
			cr: CheckReport{Findings: []Finding{
				{Severity: SeverityWarning, Fixed: true},
				{Severity: SeverityError},
				{Severity: SeverityInfo, Fixed: true},
			}},
			want: 2,
		},
		{
			name: "all fixed",
			cr: CheckReport{Findings: []Finding{
				{Severity: SeverityWarning, Fixed: true},
				{Severity: SeverityError, Fixed: true},
			}},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fixedCount(tt.cr)
			if got != tt.want {
				t.Errorf("fixedCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRenderText_FixedFindings(t *testing.T) {
	report := Report{
		Checks: []CheckReport{
			{Name: "queue", Status: CheckRan, Findings: []Finding{
				{Code: "queue.missing_dir", Severity: SeverityWarning, Fixable: true, Fixed: true,
					Message: "missing directory", Path: ".mato/waiting"},
				{Code: "queue.missing_dir", Severity: SeverityWarning, Fixable: true,
					Message: "missing directory", Path: ".mato/backlog"},
			}},
		},
		Summary:  Summary{Warnings: 1, Fixed: 1},
		ExitCode: 1,
	}

	var buf bytes.Buffer
	RenderText(&buf, report)
	output := buf.String()

	// Fixed finding should show "(fixed)"
	if !strings.Contains(output, "(fixed)") {
		t.Errorf("expected (fixed) in output, got:\n%s", output)
	}

	// Unfixed fixable finding should show "fixable with --fix"
	if !strings.Contains(output, "fixable with --fix") {
		t.Errorf("expected 'fixable with --fix' in output, got:\n%s", output)
	}

	// Category header should show fixed count
	if !strings.Contains(output, "(1 fixed)") {
		t.Errorf("expected '(1 fixed)' in category header, got:\n%s", output)
	}
}

func TestRenderText_SkippedCheck(t *testing.T) {
	report := Report{
		Checks: []CheckReport{
			{Name: "docker", Status: CheckSkipped, Findings: []Finding{}},
			{Name: "git", Status: CheckRan, Findings: []Finding{
				{Code: "git.repo_root", Severity: SeverityInfo, Message: "repo root: /repo"},
			}},
		},
		Summary:  Summary{},
		ExitCode: 0,
	}

	var buf bytes.Buffer
	RenderText(&buf, report)
	output := buf.String()

	if !strings.Contains(output, "[SKIP] docker") {
		t.Errorf("expected [SKIP] docker, got:\n%s", output)
	}
	if !strings.Contains(output, "[OK] git") {
		t.Errorf("expected [OK] git, got:\n%s", output)
	}
	if !strings.Contains(output, "all checks passed") {
		t.Errorf("expected 'all checks passed', got:\n%s", output)
	}
}

func TestRenderText_ErrorOnlyCheck(t *testing.T) {
	report := Report{
		Checks: []CheckReport{
			{Name: "git", Status: CheckRan, Findings: []Finding{
				{Code: "git.not_a_repo", Severity: SeverityError,
					Message: "not a git repository: /tmp/bad", Path: "/tmp/bad"},
			}},
		},
		Summary:  Summary{Errors: 1},
		ExitCode: 2,
	}

	var buf bytes.Buffer
	RenderText(&buf, report)
	output := buf.String()

	if !strings.Contains(output, "[ERROR] git") {
		t.Errorf("expected [ERROR] git, got:\n%s", output)
	}
	if !strings.Contains(output, "(error)") {
		t.Errorf("expected (error) suffix, got:\n%s", output)
	}
	if !strings.Contains(output, "1 error") {
		t.Errorf("expected '1 error' in summary, got:\n%s", output)
	}
}

func TestRenderText_WarningOnlyCheck(t *testing.T) {
	report := Report{
		Checks: []CheckReport{
			{Name: "locks", Status: CheckRan, Findings: []Finding{
				{Code: "locks.stale_pid", Severity: SeverityWarning,
					Message: "stale PID file for dead agent"},
			}},
		},
		Summary:  Summary{Warnings: 1},
		ExitCode: 1,
	}

	var buf bytes.Buffer
	RenderText(&buf, report)
	output := buf.String()

	if !strings.Contains(output, "[WARN] locks") {
		t.Errorf("expected [WARN] locks, got:\n%s", output)
	}
	if !strings.Contains(output, "(warning)") {
		t.Errorf("expected (warning) suffix, got:\n%s", output)
	}
}

func TestRenderText_FindingWithPath(t *testing.T) {
	report := Report{
		Checks: []CheckReport{
			{Name: "tools", Status: CheckRan, Findings: []Finding{
				{Code: "tools.found", Severity: SeverityInfo,
					Message: "git: /usr/bin/git", Path: "/usr/bin/git"},
				{Code: "tools.missing_optional", Severity: SeverityWarning,
					Message: "gh not found"},
			}},
		},
		Summary:  Summary{Warnings: 1},
		ExitCode: 1,
	}

	var buf bytes.Buffer
	RenderText(&buf, report)
	output := buf.String()

	// Finding with path uses "path: message" format
	if !strings.Contains(output, "/usr/bin/git: git: /usr/bin/git") {
		t.Errorf("expected path-prefixed finding, got:\n%s", output)
	}
	// Finding without path uses "message" format only
	if !strings.Contains(output, "  - gh not found") {
		t.Errorf("expected pathless finding, got:\n%s", output)
	}
}

func TestRenderJSON_RoundTrip(t *testing.T) {
	report := Report{
		RepoInput: ".",
		RepoRoot:  "/repo",
		TasksDir:  "/repo/.mato",
		Checks: []CheckReport{
			{Name: "git", Status: CheckRan, Findings: []Finding{
				{Code: "git.repo_root", Severity: SeverityInfo, Message: "repo root: /repo"},
			}},
			{Name: "queue", Status: CheckRan, Findings: []Finding{
				{Code: "queue.missing_dir", Severity: SeverityWarning, Fixable: true,
					Message: "missing", Path: ".mato/waiting"},
			}},
			{Name: "docker", Status: CheckSkipped, Findings: []Finding{}},
		},
		Summary:  Summary{Warnings: 1},
		ExitCode: 1,
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	if parsed.RepoRoot != report.RepoRoot {
		t.Errorf("repo_root: got %q, want %q", parsed.RepoRoot, report.RepoRoot)
	}
	if parsed.ExitCode != report.ExitCode {
		t.Errorf("exit_code: got %d, want %d", parsed.ExitCode, report.ExitCode)
	}
	if len(parsed.Checks) != len(report.Checks) {
		t.Fatalf("checks count: got %d, want %d", len(parsed.Checks), len(report.Checks))
	}

	// Verify finding fields survive round-trip
	queueCheck := parsed.Checks[1]
	if len(queueCheck.Findings) != 1 {
		t.Fatalf("queue findings: got %d, want 1", len(queueCheck.Findings))
	}
	f := queueCheck.Findings[0]
	if f.Code != "queue.missing_dir" {
		t.Errorf("code: got %q, want %q", f.Code, "queue.missing_dir")
	}
	if !f.Fixable {
		t.Error("expected fixable=true")
	}
	if f.Path != ".mato/waiting" {
		t.Errorf("path: got %q, want %q", f.Path, ".mato/waiting")
	}
}

func TestRenderJSON_FixedFinding(t *testing.T) {
	report := Report{
		Checks: []CheckReport{
			{Name: "locks", Status: CheckRan, Findings: []Finding{
				{Code: "locks.stale_pid", Severity: SeverityWarning,
					Fixable: true, Fixed: true, Message: "removed stale lock"},
			}},
		},
		Summary:  Summary{Fixed: 1},
		ExitCode: 0,
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	f := parsed.Checks[0].Findings[0]
	if !f.Fixed {
		t.Error("expected fixed=true in JSON output")
	}
	if !f.Fixable {
		t.Error("expected fixable=true in JSON output")
	}
	if parsed.Summary.Fixed != 1 {
		t.Errorf("summary fixed: got %d, want 1", parsed.Summary.Fixed)
	}
}

func TestColorIndicator(t *testing.T) {
	// fatih/color disables color output when stdout is not a TTY (as in
	// tests), so colorIndicator returns plain-text indicators unchanged.
	tests := []struct {
		name      string
		indicator string
		want      string
	}{
		{"ok", "[OK]", "[OK]"},
		{"error", "[ERROR]", "[ERROR]"},
		{"warn", "[WARN]", "[WARN]"},
		{"skip", "[SKIP]", "[SKIP]"},
		{"unknown passthrough", "[UNKNOWN]", "[UNKNOWN]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := colorIndicator(tt.indicator)
			if got != tt.want {
				t.Errorf("colorIndicator(%q) = %q, want %q", tt.indicator, got, tt.want)
			}
		})
	}
}

func TestComputeSummary(t *testing.T) {
	tests := []struct {
		name         string
		checks       []CheckReport
		wantWarnings int
		wantErrors   int
		wantFixed    int
		wantExitCode int
	}{
		{
			name:         "no findings",
			checks:       []CheckReport{{Name: "test", Status: CheckRan, Findings: []Finding{}}},
			wantExitCode: 0,
		},
		{
			name: "info only",
			checks: []CheckReport{{Name: "test", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityInfo},
			}}},
			wantExitCode: 0,
		},
		{
			name: "warnings bump exit code to 1",
			checks: []CheckReport{{Name: "test", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityWarning},
				{Severity: SeverityWarning},
			}}},
			wantWarnings: 2,
			wantExitCode: 1,
		},
		{
			name: "errors bump exit code to 2",
			checks: []CheckReport{{Name: "test", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityError},
			}}},
			wantErrors:   1,
			wantExitCode: 2,
		},
		{
			name: "fixed findings are not counted as warnings or errors",
			checks: []CheckReport{{Name: "test", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityWarning, Fixed: true},
				{Severity: SeverityError, Fixed: true},
			}}},
			wantFixed:    2,
			wantExitCode: 0,
		},
		{
			name: "mixed fixed and unfixed",
			checks: []CheckReport{{Name: "test", Status: CheckRan, Findings: []Finding{
				{Severity: SeverityWarning, Fixed: true},
				{Severity: SeverityWarning},
			}}},
			wantWarnings: 1,
			wantFixed:    1,
			wantExitCode: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Report{Checks: tt.checks}
			computeSummary(&r)
			if r.Summary.Warnings != tt.wantWarnings {
				t.Errorf("warnings: got %d, want %d", r.Summary.Warnings, tt.wantWarnings)
			}
			if r.Summary.Errors != tt.wantErrors {
				t.Errorf("errors: got %d, want %d", r.Summary.Errors, tt.wantErrors)
			}
			if r.Summary.Fixed != tt.wantFixed {
				t.Errorf("fixed: got %d, want %d", r.Summary.Fixed, tt.wantFixed)
			}
			if r.ExitCode != tt.wantExitCode {
				t.Errorf("exit code: got %d, want %d", r.ExitCode, tt.wantExitCode)
			}
		})
	}
}
