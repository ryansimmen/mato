// Package process provides shared helpers for process identity and liveness
// checks used by the merge queue and agent lock systems.
//
// Liveness checks (isProcessActive) use os.FindProcess + signal 0, which works
// on all platforms. Start-time verification, however, reads /proc/<pid>/stat
// and is therefore Linux-only. On Linux, lock identities are "PID:starttime"
// pairs that detect PID reuse; on non-Linux platforms the identity falls back
// to a bare PID, which is still correct but does not guard against PID reuse.
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
	startTime := processStartTime(pid)
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
	if !isProcessActive(pid) {
		return false
	}
	// If we have a start time, verify it matches (detect PID reuse).
	if len(parts) == 2 && parts[1] != "" {
		currentStart := processStartTime(pid)
		if currentStart != "" && currentStart != parts[1] {
			return false // PID was reused by a different process
		}
	}
	return true
}

// processStartTime reads the start time of a process from /proc/<pid>/stat.
// This is Linux-specific: /proc is not available on macOS, Windows, or other
// non-Linux platforms, so this function silently returns "" there, causing
// LockIdentity to fall back to PID-only identities.
func processStartTime(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	return parseStartTime(string(data))
}

// parseStartTime extracts the starttime field from /proc/<pid>/stat content.
// The comm field (field 2) is wrapped in parens and may contain spaces or
// closing parens. Per the Linux kernel, the first '(' marks the start
// and the last ')' marks the end of the comm field.
func parseStartTime(s string) string {
	openIdx := strings.Index(s, "(")
	if openIdx < 0 {
		return ""
	}
	closeIdx := strings.LastIndex(s, ")")
	if closeIdx < 0 || closeIdx <= openIdx || closeIdx+2 >= len(s) {
		return ""
	}
	fields := strings.Fields(s[closeIdx+2:])
	// After the closing paren, fields are: state(1), ppid(2), pgrp(3), ...
	// starttime is field 20 (0-indexed from after the paren)
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// isProcessActive checks whether a process with the given PID is currently
// running. Uses os.FindProcess + signal 0, which is cross-platform.
func isProcessActive(pid int) bool {
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
