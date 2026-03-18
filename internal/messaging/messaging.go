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

type presence struct {
	AgentID   string    `json:"agent_id"`
	Task      string    `json:"task"`
	Branch    string    `json:"branch"`
	UpdatedAt time.Time `json:"updated_at"`
}

var safeMessageFilePart = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func Init(tasksDir string) error {
	for _, dir := range []string{
		filepath.Join(tasksDir, "messages", "events"),
		filepath.Join(tasksDir, "messages", "presence"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create messaging directory %s: %w", dir, err)
		}
	}
	return nil
}

func WriteMessage(tasksDir string, msg Message) error {
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
