// Package identity provides agent identity generation and liveness checks.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"

	"mato/internal/lockfile"
)

// GenerateAgentID returns a random 8-character hex string suitable for use
// as an agent or message identifier.
func GenerateAgentID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CheckAgentActive checks whether the agent that wrote a lock file is still
// running. Unlike IsAgentActive, it returns an error when the lock file
// exists but cannot be read, allowing callers to distinguish unreadable
// locks from dead or missing ones.
func CheckAgentActive(tasksDir, agentID string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, nil
	}
	if strings.Contains(agentID, "/") || strings.Contains(agentID, "\\") {
		return false, nil
	}
	lockFile := filepath.Join(tasksDir, ".locks", agentID+".pid")
	return lockfile.CheckHeld(lockFile)
}

// IsAgentActive checks whether the agent that wrote a lock file is still running.
// Supports both the "PID:starttime" format and legacy "PID" format. Agent
// IDs containing path separators are rejected so lock-file lookups cannot
// escape the .locks directory.
func IsAgentActive(tasksDir, agentID string) bool {
	active, _ := CheckAgentActive(tasksDir, agentID)
	return active
}
