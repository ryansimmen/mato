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
	"golang.org/x/term"
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

// TerminalWidth returns the width of the terminal attached to fd. If
// fd is not a terminal or the width cannot be determined, defaultWidth
// is returned.
func TerminalWidth(fd int, defaultWidth int) int {
	w, _, err := term.GetSize(fd)
	if err != nil || w <= 0 {
		return defaultWidth
	}
	return w
}

// fdProvider is the interface checked by WriterWidth to extract a file
// descriptor from an io.Writer (satisfied by *os.File).
type fdProvider interface {
	Fd() uintptr
}

// WriterWidth returns the terminal width for w if w exposes a file
// descriptor attached to a terminal. Otherwise it returns defaultWidth.
func WriterWidth(w io.Writer, defaultWidth int) int {
	if fp, ok := w.(fdProvider); ok {
		return TerminalWidth(int(fp.Fd()), defaultWidth)
	}
	return defaultWidth
}

// Truncate shortens s to at most maxLen runes, replacing the final
// rune with "…" when truncation is needed. If maxLen is zero or
// negative, s is returned unchanged.
func Truncate(s string, maxLen int) string {
	runes := []rune(s)
	if maxLen <= 0 || len(runes) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	return string(runes[:maxLen-1]) + "…"
}

// termWidthFn is the function used by TermWidth to detect the
// terminal width. It can be replaced in tests via SetTermWidthFunc.
var termWidthFn = defaultTermWidth

func defaultTermWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

// TermWidth returns the current terminal width in columns.
// Returns 0 when stdout is not a terminal, signaling callers to
// skip width-based truncation so piped and test output stays
// deterministic.
func TermWidth() int {
	return termWidthFn()
}

// SetTermWidthFunc replaces the terminal width detection function
// and returns the previous value so callers (typically tests) can
// restore it with defer.
func SetTermWidthFunc(fn func() int) func() int {
	prev := termWidthFn
	termWidthFn = fn
	return prev
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
