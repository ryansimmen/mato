package ui

import (
	"fmt"
	"io"
)

// TextWriter wraps an io.Writer and remembers the first write error so
// callers can render linearly and check the final error once at the end.
type TextWriter struct {
	w   io.Writer
	err error
}

// NewTextWriter returns a TextWriter that writes to w.
func NewTextWriter(w io.Writer) TextWriter {
	return TextWriter{w: w}
}

// Print writes args to the underlying writer unless a previous write failed.
func (tw *TextWriter) Print(args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprint(tw.w, args...)
}

// Printf formats according to format and writes the result unless a previous
// write failed.
func (tw *TextWriter) Printf(format string, args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintf(tw.w, format, args...)
}

// Println writes args with a trailing newline unless a previous write failed.
func (tw *TextWriter) Println(args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintln(tw.w, args...)
}

// Err returns the first write error seen by the TextWriter.
func (tw *TextWriter) Err() error {
	return tw.err
}
