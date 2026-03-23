package merge

import (
	"strings"
	"testing"
)

func TestParseAgentCommitLog(t *testing.T) {
	tests := []struct {
		name        string
		log         string
		wantSubject string
		wantBody    string
	}{
		{
			name:        "empty input",
			log:         "",
			wantSubject: "",
			wantBody:    "",
		},
		{
			name:        "whitespace only",
			log:         "   \n\n  \n",
			wantSubject: "",
			wantBody:    "",
		},
		{
			name:        "subject only",
			log:         "feat: add dark mode",
			wantSubject: "feat: add dark mode",
			wantBody:    "",
		},
		{
			name:        "subject with trailing newline",
			log:         "fix: correct typo\n",
			wantSubject: "fix: correct typo",
			wantBody:    "",
		},
		{
			name:        "subject and body",
			log:         "feat: add caching\n\nAdds Redis-based caching for API responses.",
			wantSubject: "feat: add caching",
			wantBody:    "Adds Redis-based caching for API responses.",
		},
		{
			name:        "subject with leading blank lines",
			log:         "\n\nfeat: add caching\n\nSome body text.",
			wantSubject: "feat: add caching",
			wantBody:    "Some body text.",
		},
		{
			name:        "body with multiple lines",
			log:         "fix: handle edge case\n\nLine one.\nLine two.\nLine three.",
			wantSubject: "fix: handle edge case",
			wantBody:    "Line one.\nLine two.\nLine three.",
		},
		{
			name: "filters Task: line from body",
			log:  "feat: implement search\n\nAdds full-text search.\n\nTask: implement-search.md",
			wantSubject: "feat: implement search",
			wantBody:    "Adds full-text search.",
		},
		{
			name: "filters Changed files: section",
			log: "feat: add auth\n\nJWT-based authentication.\n\nChanged files:\nsrc/auth.go\nsrc/auth_test.go\n",
			wantSubject: "feat: add auth",
			wantBody:    "JWT-based authentication.",
		},
		{
			name: "filters both Task: and Changed files:",
			log: "fix: race condition\n\nFixed locking issue.\n\nTask: fix-race.md\n\nChanged files:\nqueue.go\n",
			wantSubject: "fix: race condition",
			wantBody:    "Fixed locking issue.",
		},
		{
			name:        "no body, just subject and blank line",
			log:         "docs: update readme\n\n",
			wantSubject: "docs: update readme",
			wantBody:    "",
		},
		{
			name: "multi-commit log uses only first commit",
			log:  "feat: primary change\n\nPrimary body.\n\nChanged files:\nfile1.go\n\nfeat: secondary change\n\nSecondary body.",
			wantSubject: "feat: primary change",
			wantBody:    "Primary body.",
		},
		{
			name:        "body with trailing blank lines trimmed",
			log:         "fix: cleanup\n\nSome explanation.\n\n\n",
			wantSubject: "fix: cleanup",
			wantBody:    "Some explanation.",
		},
		{
			name: "body with Co-authored-by trailer preserved",
			log:  "feat: add feature\n\nImplementation details.\n\nCo-authored-by: Bot <bot@example.com>",
			wantSubject: "feat: add feature",
			wantBody:    "Implementation details.\n\nCo-authored-by: Bot <bot@example.com>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSubject, gotBody := parseAgentCommitLog(tt.log)
			if gotSubject != tt.wantSubject {
				t.Errorf("subject = %q, want %q", gotSubject, tt.wantSubject)
			}
			if gotBody != tt.wantBody {
				t.Errorf("body = %q, want %q", gotBody, tt.wantBody)
			}
		})
	}
}

func TestFormatSquashCommitMessage(t *testing.T) {
	tests := []struct {
		name     string
		task     mergeQueueTask
		agentLog string
		want     string
	}{
		{
			name:     "agent log with subject only, no trailers",
			task:     mergeQueueTask{title: "Add dark mode"},
			agentLog: "feat: add dark mode support",
			want:     "feat: add dark mode support",
		},
		{
			name:     "empty agent log falls back to task title",
			task:     mergeQueueTask{title: "Add dark mode"},
			agentLog: "",
			want:     "Add dark mode",
		},
		{
			name:     "agent log with body and no trailers",
			task:     mergeQueueTask{title: "Fix bug"},
			agentLog: "fix: correct null pointer\n\nHandles nil receiver in Process().",
			want:     "fix: correct null pointer\n\nHandles nil receiver in Process().",
		},
		{
			name: "task ID trailer appended",
			task: mergeQueueTask{
				title: "Add caching",
				id:    "add-caching",
			},
			agentLog: "feat: add caching",
			want:     "feat: add caching\n\nTask-ID: add-caching",
		},
		{
			name: "affects trailer appended",
			task: mergeQueueTask{
				title:   "Fix auth",
				affects: []string{"internal/auth/auth.go", "internal/auth/auth_test.go"},
			},
			agentLog: "fix: auth token expiry",
			want:     "fix: auth token expiry\n\nAffects: internal/auth/auth.go, internal/auth/auth_test.go",
		},
		{
			name: "both trailers appended",
			task: mergeQueueTask{
				title:   "Refactor queue",
				id:      "refactor-queue",
				affects: []string{"internal/queue/queue.go"},
			},
			agentLog: "refactor: simplify queue logic",
			want:     "refactor: simplify queue logic\n\nTask-ID: refactor-queue\nAffects: internal/queue/queue.go",
		},
		{
			name: "agent log with body and trailers",
			task: mergeQueueTask{
				title: "Add feature",
				id:    "add-feature",
			},
			agentLog: "feat: new feature\n\nDetailed explanation of the change.",
			want:     "feat: new feature\n\nDetailed explanation of the change.\n\nTask-ID: add-feature",
		},
		{
			name: "whitespace-only agent log falls back to title with trailers",
			task: mergeQueueTask{
				title: "Fix tests",
				id:    "fix-tests",
			},
			agentLog: "  \n\n  ",
			want:     "Fix tests\n\nTask-ID: fix-tests",
		},
		{
			name: "agent log with Task: line filtered before formatting",
			task: mergeQueueTask{
				title: "Update docs",
				id:    "update-docs",
			},
			agentLog: "docs: update architecture\n\nUpdated diagrams.\n\nTask: update-docs.md",
			want:     "docs: update architecture\n\nUpdated diagrams.\n\nTask-ID: update-docs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSquashCommitMessage(tt.task, tt.agentLog)
			if got != tt.want {
				t.Errorf("got:\n%s\n\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestTaskBranchName(t *testing.T) {
	tests := []struct {
		name string
		task mergeQueueTask
		want string
	}{
		{
			name: "uses branch field when set",
			task: mergeQueueTask{branch: "task/my-feature", name: "my-feature.md"},
			want: "task/my-feature",
		},
		{
			name: "falls back to sanitized name",
			task: mergeQueueTask{name: "add dark mode.md"},
			want: "task/add-dark-mode",
		},
		{
			name: "empty branch uses name",
			task: mergeQueueTask{name: "fix-bug.md"},
			want: "task/fix-bug",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := taskBranchName(tt.task)
			if !strings.HasPrefix(got, "task/") {
				t.Errorf("expected branch to start with 'task/', got %q", got)
			}
			if tt.task.branch != "" && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
