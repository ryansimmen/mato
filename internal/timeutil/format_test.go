package timeutil

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "1 sec"},
		{"sub-second", 500 * time.Millisecond, "1 sec"},
		{"one second", time.Second, "1 sec"},
		{"thirty seconds", 30 * time.Second, "30 sec"},
		{"fifty-nine seconds", 59 * time.Second, "59 sec"},
		{"one minute", time.Minute, "1 min"},
		{"five minutes", 5 * time.Minute, "5 min"},
		{"fifty-nine minutes", 59 * time.Minute, "59 min"},
		{"one hour", time.Hour, "1 hr"},
		{"two hours", 2 * time.Hour, "2 hr"},
		{"twenty-three hours", 23 * time.Hour, "23 hr"},
		{"one day", 24 * time.Hour, "1 day"},
		{"three days", 3 * 24 * time.Hour, "3 days"},
		{"seven days", 7 * 24 * time.Hour, "7 days"},
		{"negative one minute", -1 * time.Minute, "1 min"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDuration(tt.d)
			if got != tt.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"30 seconds ago", now.Add(-30 * time.Second), "[30 sec ago]"},
		{"5 minutes ago", now.Add(-5 * time.Minute), "[5 min ago]"},
		{"2 hours ago", now.Add(-2 * time.Hour), "[2 hr ago]"},
		{"1 day ago", now.Add(-24 * time.Hour), "[1 day ago]"},
		{"3 days ago", now.Add(-3 * 24 * time.Hour), "[3 days ago]"},
		{"6 days ago", now.Add(-6 * 24 * time.Hour), "[6 days ago]"},
		{"exactly 7 days", now.Add(-7 * 24 * time.Hour), ""},
		{"8 days ago", now.Add(-8 * 24 * time.Hour), ""},
		{"future timestamp", now.Add(5 * time.Minute), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RelativeTime(tt.ts, now)
			if got != tt.want {
				t.Errorf("RelativeTime(%v, now) = %q, want %q", tt.ts, got, tt.want)
			}
		})
	}
}
