package queueview

// NewPollIndexForTests builds a minimal PollIndex containing only build warnings.
// It exists so mutation-package tests can exercise manifest/error paths without
// reaching into queueview's unexported fields.
func NewPollIndexForTests(warnings []BuildWarning) *PollIndex {
	return &PollIndex{buildWarnings: warnings}
}
