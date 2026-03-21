// Package taskfile provides helpers for reading metadata embedded in task
// markdown files, such as the branch name recorded in an HTML comment.
package taskfile

import (
	"os"
)

// ParseBranch reads a task file at path and extracts the branch name from
// a complete <!-- branch: ... --> HTML comment. Returns "" if the marker is
// missing, malformed, unterminated, or the file cannot be read.
func ParseBranch(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	branch, _ := ParseBranchComment(data)
	return branch
}
