// Package ui provides shared CLI formatting helpers for terminal output.
// It centralizes color/style primitives, warning output, format flag
// validation, and task-directory checks so command and renderer code can
// reuse a single implementation.
package ui

import (
	"fmt"
	"io"
	"os"

	"github.com/fatih/color"
)

// ColorSet holds color formatting functions used by CLI renderers.
type ColorSet struct {
	Bold   func(a ...interface{}) string
	Green  func(a ...interface{}) string
	Red    func(a ...interface{}) string
	Yellow func(a ...interface{}) string
	Cyan   func(a ...interface{}) string
	Dim    func(a ...interface{}) string
}

// NewColorSet returns a ColorSet wired to ANSI terminal colors via
// fatih/color. Color output is automatically disabled when stdout is
// not a TTY.
func NewColorSet() ColorSet {
	return ColorSet{
		Bold:   color.New(color.Bold).SprintFunc(),
		Green:  color.New(color.FgGreen).SprintFunc(),
		Red:    color.New(color.FgRed).SprintFunc(),
		Yellow: color.New(color.FgYellow).SprintFunc(),
		Cyan:   color.New(color.FgCyan).SprintFunc(),
		Dim:    color.New(color.Faint).SprintFunc(),
	}
}

// warningWriter is the destination for warning messages. When nil,
// Warnf writes to os.Stderr (resolved at call time so that test
// helpers that reassign os.Stderr work transparently).
var warningWriter io.Writer

// Warnf writes a formatted warning to stderr following the repo
// convention of "warning: ..." messages.
func Warnf(format string, args ...any) {
	w := warningWriter
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
}

// SetWarningWriter replaces the warning destination and returns the
// previous value so callers (typically tests) can restore it.  Passing
// nil restores the default (os.Stderr at call time).
func SetWarningWriter(w io.Writer) io.Writer {
	prev := warningWriter
	warningWriter = w
	return prev
}

// ValidateFormat checks that format is one of the allowed values and
// returns a descriptive error if it is not.
func ValidateFormat(format string, allowed []string) error {
	for _, a := range allowed {
		if format == a {
			return nil
		}
	}
	return fmt.Errorf("unsupported format %q", format)
}

// RequireTasksDir checks that tasksDir exists and is a directory.
func RequireTasksDir(tasksDir string) error {
	info, err := os.Stat(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(".mato/ directory not found - run 'mato init' first")
		}
		return fmt.Errorf("stat %s: %w", tasksDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", tasksDir)
	}
	return nil
}
