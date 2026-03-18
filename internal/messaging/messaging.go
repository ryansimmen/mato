package messaging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mato/internal/queue"
)

type Message struct {
	ID     string    `json:"id"`
	From   string    `json:"from"`
	Type   string    `json:"type"`
	Task   string    `json:"task"`
	Branch string    `json:"branch"`
	Files  []string  `json:"files,omitempty"`
	Body   string    `json:"body"`
	SentAt time.Time `json:"sent_at"`
}

// CompletionDetail records metadata about a task that was successfully
// merged, so downstream dependent tasks can see what changed.
type CompletionDetail struct {
	TaskID       string   `json:"task_id"`
	TaskFile     string   `json:"task_file"`
	Branch       string   `json:"branch"`
	CommitSHA    string   `json:"commit_sha"`
	FilesChanged []string `json:"files_changed"`
	Title        string   `json:"title"`
	MergedAt     time.Time `json:"merged_at"`
}

type presence struct {
	AgentID   string    `json:"agent_id"`
	Task      string    `json:"task"`
	Branch    string    `json:"branch"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidMessageTypes defines the set of accepted message types for WriteMessage.
var ValidMessageTypes = map[string]bool{
	"intent":           true,
	"conflict-warning": true,
	"completion":       true,
}

var safeMessageFilePart = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func Init(tasksDir string) error {
	for _, dir := range []string{
		filepath.Join(tasksDir, "messages", "events"),
		filepath.Join(tasksDir, "messages", "presence"),
		filepath.Join(tasksDir, "messages", "completions"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create messaging directory %s: %w", dir, err)
		}
	}
	return nil
}

func WriteMessage(tasksDir string, msg Message) error {
	if msg.Type == "" {
		return fmt.Errorf("write message: empty message type")
	}
	if !ValidMessageTypes[msg.Type] {
		return fmt.Errorf("write message: unknown message type %q", msg.Type)
	}

	if msg.ID == "" {
		id, err := queue.GenerateAgentID()
		if err != nil {
			return fmt.Errorf("generate message ID: %w", err)
		}
		msg.ID = id
	}
	if msg.SentAt.IsZero() {
		msg.SentAt = time.Now().UTC()
	} else {
		msg.SentAt = msg.SentAt.UTC()
	}

	filename := fmt.Sprintf("%s-%s-%s-%s.json",
		msg.SentAt.Format("20060102T150405.000000000Z"),
		messageFilePart(msg.From, "unknown"),
		messageFilePart(msg.Type, "message"),
		messageFilePart(msg.ID, "message"),
	)

	path := filepath.Join(tasksDir, "messages", "events", filename)
	if err := writeJSONAtomically(path, msg); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	return nil
}

func ReadMessages(tasksDir string, since time.Time) ([]Message, error) {
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read messages dir: %w", err)
	}

	messages := make([]Message, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(eventsDir, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read message %s: %w", entry.Name(), err)
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.SentAt.After(since) {
			messages = append(messages, msg)
		}
	}

	sort.Slice(messages, func(i, j int) bool {
		if messages[i].SentAt.Equal(messages[j].SentAt) {
			return messages[i].ID < messages[j].ID
		}
		return messages[i].SentAt.Before(messages[j].SentAt)
	})

	return messages, nil
}

func WritePresence(tasksDir, agentID, taskFile, branch string) error {
	info := presence{
		AgentID:   agentID,
		Task:      taskFile,
		Branch:    branch,
		UpdatedAt: time.Now().UTC(),
	}

	path := filepath.Join(tasksDir, "messages", "presence", messageFilePart(agentID, "unknown")+".json")
	if err := writeJSONAtomically(path, info); err != nil {
		return fmt.Errorf("write presence: %w", err)
	}
	return nil
}

func CleanStalePresence(tasksDir string) {
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		agentID := strings.TrimSuffix(entry.Name(), ".json")
		if !queue.IsAgentActive(tasksDir, agentID) {
			os.Remove(filepath.Join(presenceDir, entry.Name()))
		}
	}
}

func CleanOldMessages(tasksDir string, maxAge time.Duration) {
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(eventsDir, entry.Name()))
		}
	}
}

// WriteCompletionDetail writes a completion-detail JSON file for a merged task
// so downstream dependent tasks can read what changed.
func WriteCompletionDetail(tasksDir string, detail CompletionDetail) error {
	if detail.MergedAt.IsZero() {
		detail.MergedAt = time.Now().UTC()
	} else {
		detail.MergedAt = detail.MergedAt.UTC()
	}
	if detail.TaskID == "" {
		return fmt.Errorf("completion detail requires a task ID")
	}
	path := filepath.Join(tasksDir, "messages", "completions", detail.TaskID+".json")
	if err := writeJSONAtomically(path, detail); err != nil {
		return fmt.Errorf("write completion detail: %w", err)
	}
	return nil
}

// ReadCompletionDetail reads the completion-detail JSON for a given task ID.
// Returns os.ErrNotExist if the file does not exist.
func ReadCompletionDetail(tasksDir, taskID string) (*CompletionDetail, error) {
	path := filepath.Join(tasksDir, "messages", "completions", taskID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var detail CompletionDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return nil, fmt.Errorf("parse completion detail %s: %w", taskID, err)
	}
	return &detail, nil
}

// FileClaim describes which task is actively modifying a given file.
type FileClaim struct {
	Task   string `json:"task"`
	Status string `json:"status"`
}

// BuildAndWriteFileClaims builds a file-claims.json index from tasks in
// in-progress/ and ready-to-merge/ that have affects: metadata, then writes
// it atomically to .tasks/messages/file-claims.json.
func BuildAndWriteFileClaims(tasksDir string) error {
	active := queue.CollectActiveAffects(tasksDir)
	claims := make(map[string]FileClaim, len(active)*2)
	for _, t := range active {
		for _, file := range t.Affects {
			// First writer wins; later entries don't overwrite.
			if _, exists := claims[file]; !exists {
				claims[file] = FileClaim{Task: t.Name, Status: t.Dir}
			}
		}
	}
	path := filepath.Join(tasksDir, "messages", "file-claims.json")
	if err := writeJSONAtomically(path, claims); err != nil {
		return fmt.Errorf("write file claims: %w", err)
	}
	return nil
}

func writeJSONAtomically(path string, value any) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}

	if err := tmpFile.Chmod(0o644); err != nil {
		cleanup()
		return err
	}
	if err := json.NewEncoder(tmpFile).Encode(value); err != nil {
		cleanup()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func messageFilePart(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	value = safeMessageFilePart.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-._")
	if value == "" {
		return fallback
	}
	return value
}
