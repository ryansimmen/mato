package runtimedata

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/dirs"
)

const sessionVersion = 1

const (
	KindWork   = "work"
	KindReview = "review"
)

// Session records durable Copilot resume metadata for a task phase.
type Session struct {
	Version          int    `json:"version"`
	Kind             string `json:"kind"`
	TaskFile         string `json:"task_file"`
	TaskBranch       string `json:"task_branch"`
	CopilotSessionID string `json:"copilot_session_id"`
	UpdatedAt        string `json:"updated_at"`
	LastHeadSHA      string `json:"last_head_sha"`
}

// LoadSession returns the stored session metadata for a task phase. Missing
// files return (nil, nil).
func LoadSession(tasksDir, kind, taskFilename string) (*Session, error) {
	statePath, err := sessionPathFor(tasksDir, kind, taskFilename)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessionmeta %s: %w", statePath, err)
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("parse sessionmeta %s: %w", statePath, err)
	}
	normalizeSession(&session, kind, taskFilename)
	return &session, nil
}

// LoadOrCreateSession returns existing session metadata or creates a fresh
// session when metadata is missing or corrupt.
func LoadOrCreateSession(tasksDir, kind, taskFilename, taskBranch string) (*Session, error) {
	statePath, err := sessionPathFor(tasksDir, kind, taskFilename)
	if err != nil {
		return nil, err
	}

	session := Session{}
	createdFresh := false
	branchChanged := false
	if data, err := os.ReadFile(statePath); err == nil {
		if jsonErr := json.Unmarshal(data, &session); jsonErr != nil {
			session = Session{}
			createdFresh = true
		}
	} else if os.IsNotExist(err) {
		createdFresh = true
	} else {
		return nil, fmt.Errorf("read sessionmeta %s: %w", statePath, err)
	}

	normalizeSession(&session, kind, taskFilename)
	if trimmedTaskBranch := strings.TrimSpace(taskBranch); trimmedTaskBranch != "" {
		branchChanged = session.TaskBranch != trimmedTaskBranch
		session.TaskBranch = trimmedTaskBranch
	}
	if strings.TrimSpace(session.CopilotSessionID) == "" || branchChanged {
		sessionID, err := newSessionID()
		if err != nil {
			return nil, err
		}
		session.CopilotSessionID = sessionID
		if !branchChanged {
			createdFresh = true
		}
	}
	if createdFresh || branchChanged {
		if err := writeSession(statePath, &session); err != nil {
			return nil, err
		}
	}
	return &session, nil
}

// ResetSessionID rotates the stored Copilot session ID while preserving the
// rest of the session record and returns the updated session.
func ResetSessionID(tasksDir, kind, taskFilename, taskBranch string) (*Session, error) {
	statePath, err := sessionPathFor(tasksDir, kind, taskFilename)
	if err != nil {
		return nil, err
	}
	session := Session{}
	if data, err := os.ReadFile(statePath); err == nil {
		var loaded Session
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			session = loaded
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read sessionmeta %s: %w", statePath, err)
	}
	normalizeSession(&session, kind, taskFilename)
	if trimmedTaskBranch := strings.TrimSpace(taskBranch); trimmedTaskBranch != "" {
		session.TaskBranch = trimmedTaskBranch
	}
	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}
	session.CopilotSessionID = sessionID
	if err := writeSession(statePath, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// UpdateSession applies a load-modify-save update to a task phase's session
// metadata. Corrupt files are replaced with a fresh record.
func UpdateSession(tasksDir, kind, taskFilename string, fn func(*Session)) error {
	statePath, err := sessionPathFor(tasksDir, kind, taskFilename)
	if err != nil {
		return fmt.Errorf("resolve session path: %w", err)
	}
	session := Session{}
	if data, err := os.ReadFile(statePath); err == nil {
		var loaded Session
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			session = loaded
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read sessionmeta %s: %w", statePath, err)
	}
	normalizeSession(&session, kind, taskFilename)
	if fn != nil {
		fn(&session)
	}
	if strings.TrimSpace(session.CopilotSessionID) == "" {
		sessionID, err := newSessionID()
		if err != nil {
			return err
		}
		session.CopilotSessionID = sessionID
	}
	return writeSession(statePath, &session)
}

// DeleteSession removes a task phase's session metadata file. Missing files are
// ignored.
func DeleteSession(tasksDir, kind, taskFilename string) error {
	statePath, err := sessionPathFor(tasksDir, kind, taskFilename)
	if err != nil {
		return fmt.Errorf("resolve session path: %w", err)
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete sessionmeta %s: %w", statePath, err)
	}
	return nil
}

// DeleteAllSessions removes both work and review session metadata files for a
// task.
func DeleteAllSessions(tasksDir, taskFilename string) error {
	var errs []error
	for _, kind := range []string{KindWork, KindReview} {
		if err := DeleteSession(tasksDir, kind, taskFilename); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// SweepSessions removes session metadata for tasks that are no longer in
// non-terminal queue directories.
func SweepSessions(tasksDir string) error {
	runtimeDir := sessionRuntimeDir(tasksDir)
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read sessionmeta dir %s: %w", runtimeDir, err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		kind, taskFilename, ok := parseSessionEntryName(entry.Name())
		if !ok || kind == "" || taskFilename == "" {
			continue
		}
		active, err := dirs.IsActive(tasksDir, taskFilename)
		if err != nil {
			errs = append(errs, fmt.Errorf("check task activity for %s: %w", taskFilename, err))
			continue
		}
		if active {
			continue
		}
		if err := os.Remove(filepath.Join(runtimeDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("delete stale sessionmeta %s: %w", entry.Name(), err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func sessionPathFor(tasksDir, kind, taskFilename string) (string, error) {
	tasksDir = strings.TrimSpace(tasksDir)
	if tasksDir == "" {
		return "", fmt.Errorf("tasks directory must not be empty")
	}
	kind = strings.TrimSpace(kind)
	if !validSessionKind(kind) {
		return "", fmt.Errorf("invalid session kind %q", kind)
	}
	taskFilename = strings.TrimSpace(taskFilename)
	if taskFilename == "" {
		return "", fmt.Errorf("task filename must not be empty")
	}
	if filepath.Base(taskFilename) != taskFilename {
		return "", fmt.Errorf("task filename %q must be a base name", taskFilename)
	}
	return filepath.Join(sessionRuntimeDir(tasksDir), kind+"-"+taskFilename+".json"), nil
}

func sessionRuntimeDir(tasksDir string) string {
	return filepath.Join(tasksDir, "runtime", "sessionmeta")
}

func validSessionKind(kind string) bool {
	return kind == KindWork || kind == KindReview
}

func normalizeSession(session *Session, kind, taskFilename string) {
	session.Version = sessionVersion
	session.Kind = kind
	session.TaskFile = taskFilename
	session.TaskBranch = strings.TrimSpace(session.TaskBranch)
	session.CopilotSessionID = strings.TrimSpace(session.CopilotSessionID)
	session.LastHeadSHA = strings.TrimSpace(session.LastHeadSHA)
	if strings.TrimSpace(session.UpdatedAt) == "" {
		session.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
}

func writeSession(statePath string, session *Session) error {
	session.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return fmt.Errorf("create sessionmeta dir for %s: %w", session.TaskFile, err)
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sessionmeta %s: %w", statePath, err)
	}
	if err := atomicwrite.WriteFile(statePath, append(data, '\n')); err != nil {
		return fmt.Errorf("write sessionmeta %s: %w", statePath, err)
	}
	return nil
}

func parseSessionEntryName(name string) (kind, taskFilename string, ok bool) {
	for _, candidate := range []string{KindWork, KindReview} {
		prefix := candidate + "-"
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		return candidate, strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json"), true
	}
	return "", "", false
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	), nil
}
