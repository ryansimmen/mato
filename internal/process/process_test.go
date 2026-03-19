package process

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestIsProcessActive_CurrentPID(t *testing.T) {
	if !isProcessActive(os.Getpid()) {
		t.Fatal("current process should be active")
	}
}

func TestIsProcessActive_DeadPID(t *testing.T) {
	if isProcessActive(2147483647) {
		t.Fatal("non-existent PID should not be active")
	}
}

func TestIsProcessActive_InvalidPID(t *testing.T) {
	if isProcessActive(0) {
		t.Fatal("PID 0 should not be active")
	}
	if isProcessActive(-1) {
		t.Fatal("negative PID should not be active")
	}
}

func TestIsProcessActive_EPERMTreatedAsAlive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (PID 1 returns EPERM only for non-root)")
	}
	// PID 1 (init/systemd) belongs to root; Signal(0) returns EPERM for non-root callers.
	if !isProcessActive(1) {
		t.Fatal("PID 1 should be considered active (EPERM means process exists)")
	}
}

// buildStatLine constructs a synthetic /proc/<pid>/stat line with the given
// process name and starttime (field 22, 1-indexed). The 20 fields after the
// comm field are filled with placeholder values, with field 20 (0-indexed
// from after the closing paren) set to starttime.
func buildStatLine(pid int, comm string, starttime string) string {
	// Fields after comm: state ppid pgrp session tty_nr tpgid flags
	//   minflt cminflt majflt cmajflt utime stime cutime cstime priority
	//   nice num_threads itrealvalue starttime ...
	afterComm := []string{
		"S",   // 1: state
		"1",   // 2: ppid
		"1",   // 3: pgrp
		"1",   // 4: session
		"0",   // 5: tty_nr
		"-1",  // 6: tpgid
		"0",   // 7: flags
		"100", // 8: minflt
		"0",   // 9: cminflt
		"0",   // 10: majflt
		"0",   // 11: cmajflt
		"50",  // 12: utime
		"10",  // 13: stime
		"0",   // 14: cutime
		"0",   // 15: cstime
		"20",  // 16: priority
		"0",   // 17: nice
		"1",   // 18: num_threads
		"0",   // 19: itrealvalue
	}
	afterComm = append(afterComm, starttime) // 20: starttime (0-indexed)
	// Add a few more trailing fields to be realistic
	afterComm = append(afterComm, "0", "0")
	return fmt.Sprintf("%d (%s) %s", pid, comm, strings.Join(afterComm, " "))
}

func TestParseStartTime(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple process name",
			input:    buildStatLine(42, "bash", "12345"),
			expected: "12345",
		},
		{
			name:     "process name with closing paren",
			input:    buildStatLine(99, "my-tool)", "67890"),
			expected: "67890",
		},
		{
			name:     "process name with multiple closing parens",
			input:    buildStatLine(100, "a))b)", "11111"),
			expected: "11111",
		},
		{
			name:     "process name with open and close parens",
			input:    buildStatLine(101, "foo(bar)baz", "22222"),
			expected: "22222",
		},
		{
			name:     "process name with spaces",
			input:    buildStatLine(102, "my process", "33333"),
			expected: "33333",
		},
		{
			name:     "empty input",
			input:    "",
			expected: "",
		},
		{
			name:     "no opening paren",
			input:    "42 bash S 1 1",
			expected: "",
		},
		{
			name:     "no closing paren",
			input:    "42 (bash S 1 1",
			expected: "",
		},
		{
			name:     "closing paren before opening",
			input:    ")42 (bash",
			expected: "",
		},
		{
			name:     "too few fields after comm",
			input:    "42 (bash) S 1",
			expected: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStartTime(tt.input)
			if got != tt.expected {
				t.Errorf("parseStartTime(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
