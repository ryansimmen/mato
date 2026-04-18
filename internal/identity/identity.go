// Package identity provides agent identity generation and liveness checks.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ryansimmen/mato/internal/lockfile"
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
	meta, err := agentLockMetadata(tasksDir, agentID)
	if err != nil {
		return false, err
	}
	return meta.IsActive(), nil
}

func agentLockMetadata(tasksDir, agentID string) (lockfile.Metadata, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return lockfile.Metadata{}, nil
	}
	if strings.Contains(agentID, "/") || strings.Contains(agentID, "\\") {
		return lockfile.Metadata{}, nil
	}
	lockFile := filepath.Join(tasksDir, ".locks", agentID+".pid")
	return lockfile.ReadMetadata(lockFile)
}

// IsAgentActive checks whether the agent that wrote a lock file is still running.
// Supports both the "PID:starttime" format and legacy "PID" format. Agent
// IDs containing path separators are rejected so lock-file lookups cannot
// escape the .locks directory.
func IsAgentActive(tasksDir, agentID string) bool {
	active, _ := CheckAgentActive(tasksDir, agentID)
	return active
}

// AgentActivity reports whether an agent lock indicates a live process.
// Unreadable lock files are reported as unknown so callers can avoid treating
// them as stale or dead by mistake.
type AgentActivity int

const (
	AgentInactive AgentActivity = iota
	AgentActive
	AgentUnknown
)

// DescribeAgentActivity returns the liveness status of an agent lock file.
// It treats missing or invalid agent IDs as inactive, active locks as active,
// and unreadable lock files as unknown with context.
func DescribeAgentActivity(tasksDir, agentID string) (AgentActivity, error) {
	meta, err := agentLockMetadata(tasksDir, agentID)
	if err != nil {
		return AgentUnknown, fmt.Errorf("read agent lock %s: %w", agentID, err)
	}
	if meta.IsActive() {
		return AgentActive, nil
	}
	return AgentInactive, nil
}
