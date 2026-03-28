// Package messaging implements the inter-agent messaging protocol. It supports
// intent, progress, and completion event types, enabling coordination between
// concurrently running agents through filesystem-backed JSON messages.
package messaging

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/identity"
	"mato/internal/taskfile"
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
	TaskID       string    `json:"task_id"`
	TaskFile     string    `json:"task_file"`
	Branch       string    `json:"branch"`
	CommitSHA    string    `json:"commit_sha"`
	FilesChanged []string  `json:"files_changed"`
	Title        string    `json:"title"`
	MergedAt     time.Time `json:"merged_at"`
}

// PresenceInfo describes a running agent's current task and branch.
type PresenceInfo struct {
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
	"progress":         true,
}

// MessagingDirs lists the relative paths (from tasksDir) of the messaging
// subdirectories created by Init(). Exported so that doctor can check for
// their existence without duplicating the list.
var MessagingDirs = []string{
	"messages/events",
	"messages/presence",
	"messages/completions",
}

func Init(tasksDir string) error {
	for _, relDir := range MessagingDirs {
		dir := filepath.Join(tasksDir, relDir)
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
		id, err := identity.GenerateAgentID()
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
		safeFilePart(msg.From, "unknown"),
		safeFilePart(msg.Type, "message"),
		safeFilePart(msg.ID, "message"),
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
			fmt.Fprintf(os.Stderr, "warning: could not parse message %s: %v\n", entry.Name(), err)
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

// ReadRecentMessages reads the most recent limit messages from the events
// directory. It leverages the timestamp-prefixed filenames (sorted lexically
// by os.ReadDir) to skip older entries without reading or parsing them.
// If limit <= 0, all messages are read (equivalent to ReadMessages with a
// zero time).
func ReadRecentMessages(tasksDir string, limit int) ([]Message, error) {
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read messages dir: %w", err)
	}

	// Filter to JSON files only.
	jsonEntries := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			jsonEntries = append(jsonEntries, e)
		}
	}

	// os.ReadDir returns entries sorted by name. Since filenames are
	// timestamp-prefixed, the last entries are the most recent.
	if limit > 0 && len(jsonEntries) > limit {
		jsonEntries = jsonEntries[len(jsonEntries)-limit:]
	}

	messages := make([]Message, 0, len(jsonEntries))
	for _, entry := range jsonEntries {
		data, err := os.ReadFile(filepath.Join(eventsDir, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read message %s: %w", entry.Name(), err)
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse message %s: %v\n", entry.Name(), err)
			continue
		}
		messages = append(messages, msg)
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
	info := PresenceInfo{
		AgentID:   agentID,
		Task:      taskFile,
		Branch:    branch,
		UpdatedAt: time.Now().UTC(),
	}

	path := filepath.Join(tasksDir, "messages", "presence", safeFilePart(agentID, "unknown")+".json")
	if err := writeJSONAtomically(path, info); err != nil {
		return fmt.Errorf("write presence: %w", err)
	}
	return nil
}

// ReadAllPresence reads all presence JSON files and returns a map keyed by agent ID.
func ReadAllPresence(tasksDir string) (map[string]PresenceInfo, error) {
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read presence dir: %w", err)
	}

	result := make(map[string]PresenceInfo, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(presenceDir, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read presence file %s: %w", entry.Name(), err)
		}
		var info PresenceInfo
		if err := json.Unmarshal(data, &info); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse presence file %s: %v\n", entry.Name(), err)
			continue
		}
		result[info.AgentID] = info
	}
	return result, nil
}

func CleanStalePresence(tasksDir string) {
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read presence directory: %v\n", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(presenceDir, entry.Name())

		// Read the JSON payload to get the canonical agent_id rather than
		// deriving it from the sanitized filename, which loses information
		// for agent IDs containing non-filename-safe characters.
		agentID := strings.TrimSuffix(entry.Name(), ".json")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: could not read presence file %s: %v\n", entry.Name(), err)
			continue
		}
		var info PresenceInfo
		if err := json.Unmarshal(data, &info); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse presence file %s: %v\n", entry.Name(), err)
			continue
		}
		if info.AgentID != "" {
			agentID = info.AgentID
		}

		if !identity.IsAgentActive(tasksDir, agentID) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: could not remove stale presence file %s: %v\n", entry.Name(), err)
			}
		}
	}
}

func CleanOldMessages(tasksDir string, maxAge time.Duration) {
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read events directory: %v\n", err)
		return
	}

	cutoff := time.Now().UTC().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(eventsDir, entry.Name())); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: could not remove old message file %s: %v\n", entry.Name(), err)
			}
		}
	}
}

// safeEncode encodes an arbitrary string into a collision-resistant filename
// component. Characters in [a-zA-Z0-9-] pass through unchanged; all others
// are encoded as _XX where XX is the lowercase hex value of each byte. This
// is reversible and guarantees distinct input strings map to distinct outputs.
func safeEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
			b.WriteString(hex.EncodeToString([]byte{c}))
		}
	}
	return b.String()
}

// safeFilePart encodes a value for use as a filename component using
// collision-resistant encoding. If the value is empty or whitespace-only,
// the fallback is returned instead.
func safeFilePart(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return safeEncode(value)
}

// completionFilename encodes a task ID into a collision-resistant filename
// safe for use inside the completions directory.
func completionFilename(taskID string) string {
	s := safeEncode(taskID)
	if s == "" {
		return "unknown"
	}
	return s
}

// WriteCompletionDetail writes a completion-detail JSON file for a merged task
// so downstream dependent tasks can read what changed. The filename is derived
// from TaskID using a collision-resistant encoding (see completionFilename).
func WriteCompletionDetail(tasksDir string, detail CompletionDetail) error {
	if detail.MergedAt.IsZero() {
		detail.MergedAt = time.Now().UTC()
	} else {
		detail.MergedAt = detail.MergedAt.UTC()
	}
	if detail.TaskID == "" {
		return fmt.Errorf("completion detail requires a task ID")
	}
	name := completionFilename(detail.TaskID)
	path := filepath.Join(tasksDir, "messages", "completions", name+".json")
	if err := writeJSONAtomically(path, detail); err != nil {
		return fmt.Errorf("write completion detail: %w", err)
	}
	return nil
}

// ReadCompletionDetail reads the completion-detail JSON for a given task ID.
func ReadCompletionDetail(tasksDir, taskID string) (*CompletionDetail, error) {
	name := completionFilename(taskID)
	path := filepath.Join(tasksDir, "messages", "completions", name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read completion detail %s: %w", taskID, err)
	}
	var detail CompletionDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return nil, fmt.Errorf("parse completion detail %s: %w", taskID, err)
	}
	return &detail, nil
}

// ReadAllCompletionDetails reads all completion-detail JSON files and returns
// them sorted by MergedAt descending (most recent first).
func ReadAllCompletionDetails(tasksDir string) ([]CompletionDetail, error) {
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	entries, err := os.ReadDir(completionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read completions dir: %w", err)
	}

	var details []CompletionDetail
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(completionsDir, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read completion detail %s: %w", entry.Name(), err)
		}
		var detail CompletionDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse completion detail %s: %v\n", entry.Name(), err)
			continue
		}
		details = append(details, detail)
	}

	sort.Slice(details, func(i, j int) bool {
		return details[i].MergedAt.After(details[j].MergedAt)
	})

	return details, nil
}

// FileClaim describes which task is actively modifying a given file or
// directory prefix declared in affects: metadata.
type FileClaim struct {
	Task   string `json:"task"`
	Status string `json:"status"`
}

// BuildAndWriteFileClaims builds a file-claims.json index from tasks in
// in-progress/, ready-for-review/, and ready-to-merge/ that have affects:
// metadata, then writes it atomically to .mato/messages/file-claims.json.
// If excludeTask is non-empty, skip the task with that filename so the
// just-claimed task does not see its own files as conflicting. Entries ending
// with "/" are preserved as directory-prefix claims.
func BuildAndWriteFileClaims(tasksDir, excludeTask string) error {
	active := taskfile.CollectActiveAffects(tasksDir)
	claims := make(map[string]FileClaim, len(active)*2)
	for _, t := range active {
		if excludeTask != "" && t.Name == excludeTask {
			continue
		}
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
	return atomicwrite.WriteFunc(path, func(f *os.File) error {
		return json.NewEncoder(f).Encode(value)
	})
}
