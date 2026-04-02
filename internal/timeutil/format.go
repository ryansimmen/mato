// Package timeutil provides shared time-formatting helpers for CLI display.
package timeutil

import (
	"fmt"
	"time"
)

// FormatDuration returns a human-friendly duration string such as "3 sec",
// "12 min", "2 hr", "1 day", or "3 days".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		sec := int(d.Seconds())
		if sec < 1 {
			sec = 1
		}
		return fmt.Sprintf("%d sec", sec)
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hr", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
}

// fallbackThreshold is the age beyond which RelativeTime falls back to the
// absolute date instead of a relative annotation.
const fallbackThreshold = 7 * 24 * time.Hour

// RelativeTime returns a relative time annotation for display next to an
// absolute timestamp. For events within the last 7 days it returns a string
// like "[5 min ago]". For older events it returns an empty string, signalling
// the caller should show only the absolute timestamp.
func RelativeTime(t time.Time, now time.Time) string {
	d := now.Sub(t)
	if d < 0 || d >= fallbackThreshold {
		return ""
	}
	return fmt.Sprintf("[%s ago]", FormatDuration(d))
}
