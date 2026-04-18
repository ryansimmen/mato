package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/testutil"
)

func TestCheckGit_FindingClassification(t *testing.T) {
	tests := []struct {
		name     string
		repoErr  error
		wantCode string
	}{
		{
			name:     "not a git repository",
			repoErr:  errors.New("git rev-parse: exit status 128 (fatal: not a git repository (or any of the parent directories): .git)"),
			wantCode: "git.not_a_repo",
		},
		{
			name:     "cannot change to directory",
			repoErr:  errors.New("git rev-parse: exit status 1 (cannot change to /tmp/some-dir)"),
			wantCode: "git.resolve_failed",
		},
		{
			name:     "plain error without parens",
			repoErr:  errors.New("some other resolve error"),
			wantCode: "git.resolve_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := &checkContext{
				ctx:       context.Background(),
				repoInput: "/tmp/test-dir",
				repoErr:   tt.repoErr,
			}

			cr := checkGit(cc)

			if cr.Name != "git" {
				t.Fatalf("name = %q, want %q", cr.Name, "git")
			}
			if cr.Status != CheckRan {
				t.Fatalf("status = %q, want %q", cr.Status, CheckRan)
			}
			if len(cr.Findings) != 1 {
				t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
			}

			f := cr.Findings[0]
			if f.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", f.Code, tt.wantCode)
			}
			if f.Severity != SeverityError {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
			}
			if f.Path != "/tmp/test-dir" {
				t.Errorf("path = %q, want %q", f.Path, "/tmp/test-dir")
			}
		})
	}
}

func TestCheckGit_NotARepoMessage(t *testing.T) {
	cc := &checkContext{
		ctx:       context.Background(),
		repoInput: "/tmp/my-dir",
		repoErr:   errors.New("git rev-parse: exit status 128 (fatal: not a git repository)"),
	}

	cr := checkGit(cc)

	if len(cr.Findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
	}
	f := cr.Findings[0]
	if !strings.Contains(f.Message, "not a git repository") {
		t.Errorf("message = %q, want it to contain %q", f.Message, "not a git repository")
	}
	if !strings.Contains(f.Message, "/tmp/my-dir") {
		t.Errorf("message = %q, want it to contain repoInput", f.Message)
	}
}

func TestCheckGit_ResolveFailedMessage(t *testing.T) {
	cc := &checkContext{
		ctx:       context.Background(),
		repoInput: "/tmp/some-dir",
		repoErr:   errors.New("git rev-parse: exit status 1 (cannot change to /tmp/some-dir)"),
	}

	cr := checkGit(cc)

	if len(cr.Findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
	}
	f := cr.Findings[0]
	if !strings.Contains(f.Message, "failed to resolve git repository") {
		t.Errorf("message = %q, want it to contain %q", f.Message, "failed to resolve git repository")
	}
}

func TestCheckGit_BranchDetection(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	cc := &checkContext{
		ctx:       context.Background(),
		repoInput: repoRoot,
		repoRoot:  repoRoot,
	}

	cr := checkGit(cc)

	foundRoot := false
	foundBranch := false
	for _, f := range cr.Findings {
		switch f.Code {
		case "git.repo_root":
			foundRoot = true
			if f.Severity != SeverityInfo {
				t.Errorf("repo_root severity = %q, want %q", f.Severity, SeverityInfo)
			}
			if !strings.Contains(f.Message, repoRoot) {
				t.Errorf("repo_root message = %q, want it to contain repo path", f.Message)
			}
		case "git.branch":
			foundBranch = true
			if f.Severity != SeverityInfo {
				t.Errorf("branch severity = %q, want %q", f.Severity, SeverityInfo)
			}
			if !strings.Contains(f.Message, "current branch:") {
				t.Errorf("branch message = %q, want 'current branch:' prefix", f.Message)
			}
		}
	}
	if !foundRoot {
		t.Error("expected git.repo_root finding")
	}
	if !foundBranch {
		t.Error("expected git.branch finding")
	}
}

func TestRepoErrDetail(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "not a git repository",
			err:  errors.New("git rev-parse: exit status 128 (fatal: not a git repository)"),
			want: "fatal: not a git repository",
		},
		{
			name: "cannot change to directory",
			err:  errors.New("git rev-parse: exit status 1 (cannot change to /tmp/foo)"),
			want: "cannot change to /tmp/foo",
		},
		{
			name: "fatal error with nested parens falls back to full message",
			err:  errors.New("git rev-parse: exit status 128 (fatal: not a git repository (or any parent))"),
			want: "git rev-parse: exit status 128 (fatal: not a git repository (or any parent))",
		},
		{
			name: "plain error without parens",
			err:  errors.New("some other error"),
			want: "some other error",
		},
		{
			name: "error with parens but no recognized keyword",
			err:  errors.New("git rev-parse: exit status 1 (unknown problem)"),
			want: "git rev-parse: exit status 1 (unknown problem)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := &checkContext{repoErr: tt.err}
			got := cc.repoErrDetail()
			if got != tt.want {
				t.Errorf("repoErrDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}
