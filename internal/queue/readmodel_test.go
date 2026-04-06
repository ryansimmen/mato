package queue

import "mato/internal/queueview"

func newPollIndexWithWarnings(warnings []BuildWarning) *PollIndex {
	return queueview.NewPollIndexForTests(warnings)
}
