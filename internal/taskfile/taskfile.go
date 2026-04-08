// Package taskfile provides helpers for reading metadata embedded in task
// markdown files, such as the branch name recorded in an HTML comment.
package taskfile

import (
	"errors"
	"fmt"
	"os"
)

// ErrTaskFileNotRegular reports that a queue entry is not a regular file.
var ErrTaskFileNotRegular = errors.New("task file is not a regular file")

// CheckRegularTaskFile verifies that path currently refers to a regular file.
func CheckRegularTaskFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat task file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lstat task file %s: %w", path, ErrTaskFileNotRegular)
	}
	return nil
}

// ReadRegularTaskFile verifies that path is a regular file before reading it.
func ReadRegularTaskFile(path string) ([]byte, error) {
	if err := CheckRegularTaskFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read task file %s: %w", path, err)
	}
	return data, nil
}

// ParseBranch reads a task file at path and extracts the branch name from a
// standalone <!-- branch: ... --> HTML comment outside code fences. Returns ""
// if the marker is missing, malformed, unterminated, the path is not a regular
// file, or the file cannot be read.
func ParseBranch(path string) string {
	data, err := ReadRegularTaskFile(path)
	if err != nil {
		return ""
	}
	branch, _ := ParseBranchMarkerLine(data)
	return branch
}
