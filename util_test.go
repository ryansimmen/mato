package main

import "testing"

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "simple name", input: "add-feature.md", want: "add-feature"},
		{name: "spaces and special chars", input: "fix the bug (urgent).md", want: "fix-the-bug-urgent"},
		{name: "already clean no extension", input: "my-task", want: "my-task"},
		{name: "consecutive special chars", input: "foo---bar___baz.md", want: "foo-bar-baz"},
		{name: "leading and trailing specials", input: "---hello---.md", want: "hello"},
		{name: "empty after strip", input: ".md", want: "unnamed"},
		{name: "unicode characters", input: "tâche-résumé.md", want: "t-che-r-sum"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBranchName(tt.input); got != tt.want {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateAgentID(t *testing.T) {
	id, err := generateAgentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("expected 8 hex chars, got %q (len %d)", id, len(id))
	}
	id2, err := generateAgentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == id2 {
		t.Errorf("two consecutive IDs should differ: %q == %q", id, id2)
	}
}

func TestHasModelArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no model flag", args: []string{"--autopilot"}, want: false},
		{name: "model with value", args: []string{"--model", "gpt-5"}, want: true},
		{name: "model equals syntax", args: []string{"--model=gpt-5"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasModelArg(tt.args); got != tt.want {
				t.Fatalf("hasModelArg(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
