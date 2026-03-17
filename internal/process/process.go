// Package process provides shared helpers for process identity and liveness
// checks used by the merge queue and agent lock systems.
package process

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// LockIdentity returns "PID:starttime" for the given process.
// Falls back to just "PID" if start time is unavailable (non-Linux).
func LockIdentity(pid int) string {
	startTime := ProcessStartTime(pid)
	if startTime != "" {
		return fmt.Sprintf("%d:%s", pid, startTime)
	}
	return strconv.Itoa(pid)
}

// IsLockHolderAlive checks if the process described by a lock identity
// ("PID" or "PID:starttime") is still running with the same start time.
// Legacy PID-only identities are handled gracefully.
func IsLockHolderAlive(identity string) bool {
	parts := strings.SplitN(identity, ":", 2)
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 0 {
		return false
	}
	if !IsProcessActive(pid) {
		return false
	}
	// If we have a start time, verify it matches (detect PID reuse).
	if len(parts) == 2 && parts[1] != "" {
		currentStart := ProcessStartTime(pid)
		if currentStart != "" && currentStart != parts[1] {
			return false // PID was reused by a different process
		}
	}
	return true
}

// ProcessStartTime reads the start time of a process from /proc/<pid>/stat.
// Returns empty string if unavailable (non-Linux or process gone).
func ProcessStartTime(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// Field 22 (1-indexed) is starttime. Find it after the comm field
	// which is enclosed in parens and may contain spaces.
	s := string(data)
	closeParenIdx := strings.LastIndex(s, ")")
	if closeParenIdx < 0 || closeParenIdx+2 >= len(s) {
		return ""
	}
	fields := strings.Fields(s[closeParenIdx+2:])
	// After the closing paren, fields are: state(1), ppid(2), pgrp(3), ...
	// starttime is field 20 (0-indexed from after the paren)
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// IsProcessActive checks whether a process with the given PID is currently running.
func IsProcessActive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
