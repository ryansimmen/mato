package status

import (
	"fmt"
	"io"
	"time"

	"mato/internal/pause"
	"mato/internal/ui"
)

// colorSet is an alias for the shared ui.ColorSet used by render helpers.
type colorSet = ui.ColorSet

func newColorSet() colorSet {
	return ui.NewColorSet()
}

const compactListLimit = 5

// minTruncWidth is the smallest budget allowed when clamping
// width-based truncation on very narrow terminals.
const minTruncWidth = 6

type renderWriter struct {
	w   io.Writer
	err error
}

func (rw *renderWriter) printf(format string, args ...any) {
	if rw.err != nil {
		return
	}
	_, rw.err = fmt.Fprintf(rw.w, format, args...)
}

func (rw *renderWriter) println(args ...any) {
	if rw.err != nil {
		return
	}
	_, rw.err = fmt.Fprintln(rw.w, args...)
}

func renderPauseState(c colorSet, state pause.State) string {
	if !state.Active {
		return c.Dim("not paused")
	}
	if state.ProblemKind != pause.ProblemNone {
		return c.Yellow(fmt.Sprintf("paused (problem: %s)", state.Problem))
	}
	return c.Yellow("paused since " + state.Since.Format(time.RFC3339))
}
