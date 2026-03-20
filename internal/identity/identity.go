// Package identity provides agent identity generation and liveness checks.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"mato/internal/process"
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

// IsAgentActive checks whether the agent that wrote a lock file is still running.
// Supports both the "PID:starttime" format and legacy "PID" format.
func IsAgentActive(tasksDir, agentID string) bool {
	if agentID == "" {
		return false
	}
	lockFile := filepath.Join(tasksDir, ".locks", agentID+".pid")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return false
	}
	return process.IsLockHolderAlive(strings.TrimSpace(string(data)))
}
