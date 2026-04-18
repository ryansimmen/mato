package doctor

import (
	"context"
	"testing"

	"github.com/ryansimmen/mato/internal/runner"
)

func TestCheckTools_SeverityMapping(t *testing.T) {
	tests := []struct {
		name     string
		finding  runner.ToolFinding
		wantCode string
		wantSev  Severity
	}{
		{
			name: "required tool found",
			finding: runner.ToolFinding{
				Name: "git", Path: "/usr/bin/git", Required: true, Found: true,
				Message: "git: /usr/bin/git",
			},
			wantCode: "tools.found",
			wantSev:  SeverityInfo,
		},
		{
			name: "required tool missing is error",
			finding: runner.ToolFinding{
				Name: "git", Required: true, Found: false,
				Message: "git: not found",
			},
			wantCode: "tools.missing_required",
			wantSev:  SeverityError,
		},
		{
			name: "optional tool missing is warning",
			finding: runner.ToolFinding{
				Name: "docker", Required: false, Found: false,
				Message: "docker: not found",
			},
			wantCode: "tools.missing_optional",
			wantSev:  SeverityWarning,
		},
		{
			name: "optional tool found is info",
			finding: runner.ToolFinding{
				Name: "docker", Path: "/usr/bin/docker", Required: false, Found: true,
				Message: "docker: /usr/bin/docker",
			},
			wantCode: "tools.found",
			wantSev:  SeverityInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubTools(t, func() runner.ToolReport {
				return runner.ToolReport{Findings: []runner.ToolFinding{tt.finding}}
			})

			cc := &checkContext{ctx: context.Background()}
			cr := checkTools(cc)

			if cr.Name != "tools" {
				t.Fatalf("name = %q, want %q", cr.Name, "tools")
			}
			if len(cr.Findings) != 1 {
				t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
			}

			f := cr.Findings[0]
			if f.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", f.Code, tt.wantCode)
			}
			if f.Severity != tt.wantSev {
				t.Errorf("severity = %q, want %q", f.Severity, tt.wantSev)
			}
		})
	}
}

func TestCheckTools_MissingCopilotDir(t *testing.T) {
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: ".copilot", Required: true, Found: false, Message: ".copilot: directory not found"},
		}}
	})

	cc := &checkContext{ctx: context.Background()}
	cr := checkTools(cc)

	if len(cr.Findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
	}

	f := cr.Findings[0]
	if f.Code != "tools.missing_copilot_dir" {
		t.Errorf("code = %q, want %q", f.Code, "tools.missing_copilot_dir")
	}
	if f.Severity != SeverityError {
		t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
	}
}

func TestCheckTools_MessagePassthrough(t *testing.T) {
	const wantMsg = "copilot: /usr/local/bin/copilot (v1.2.3)"
	const wantPath = "/usr/local/bin/copilot"

	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: "copilot", Path: wantPath, Required: true, Found: true, Message: wantMsg},
		}}
	})

	cc := &checkContext{ctx: context.Background()}
	cr := checkTools(cc)

	if len(cr.Findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
	}

	f := cr.Findings[0]
	if f.Message != wantMsg {
		t.Errorf("message = %q, want %q", f.Message, wantMsg)
	}
	if f.Path != wantPath {
		t.Errorf("path = %q, want %q", f.Path, wantPath)
	}
}

func TestCheckTools_MultipleFindings(t *testing.T) {
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: "copilot", Path: "/usr/bin/copilot", Required: true, Found: true, Message: "copilot: found"},
			{Name: "git", Required: true, Found: false, Message: "git: not found"},
			{Name: "docker", Required: false, Found: false, Message: "docker: not found"},
		}}
	})

	cc := &checkContext{ctx: context.Background()}
	cr := checkTools(cc)

	if len(cr.Findings) != 3 {
		t.Fatalf("len(findings) = %d, want 3", len(cr.Findings))
	}

	// First: found
	if cr.Findings[0].Code != "tools.found" {
		t.Errorf("findings[0].code = %q, want %q", cr.Findings[0].Code, "tools.found")
	}
	// Second: required missing
	if cr.Findings[1].Code != "tools.missing_required" {
		t.Errorf("findings[1].code = %q, want %q", cr.Findings[1].Code, "tools.missing_required")
	}
	if cr.Findings[1].Severity != SeverityError {
		t.Errorf("findings[1].severity = %q, want %q", cr.Findings[1].Severity, SeverityError)
	}
	// Third: optional missing
	if cr.Findings[2].Code != "tools.missing_optional" {
		t.Errorf("findings[2].code = %q, want %q", cr.Findings[2].Code, "tools.missing_optional")
	}
	if cr.Findings[2].Severity != SeverityWarning {
		t.Errorf("findings[2].severity = %q, want %q", cr.Findings[2].Severity, SeverityWarning)
	}
}

func TestCheckTools_EmptyReport(t *testing.T) {
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{}}
	})

	cc := &checkContext{ctx: context.Background()}
	cr := checkTools(cc)

	if cr.Status != CheckRan {
		t.Fatalf("status = %q, want %q", cr.Status, CheckRan)
	}
	if len(cr.Findings) != 0 {
		t.Errorf("len(findings) = %d, want 0", len(cr.Findings))
	}
}
