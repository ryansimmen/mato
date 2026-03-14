package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInitMessaging(t *testing.T) {
	tasksDir := t.TempDir()

	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	for _, dir := range []string{
		filepath.Join(tasksDir, "messages", "events"),
		filepath.Join(tasksDir, "messages", "presence"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected %s to exist: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("expected %s to be a directory", dir)
		}
	}
}

func TestWriteMessage(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	sentAt := time.Date(2024, time.May, 1, 12, 34, 56, 123456789, time.UTC)
	msg := Message{
		ID:     "abc12345",
		From:   "agent-1",
		Type:   "intent",
		Task:   "task.md",
		Branch: "feature/test",
		Body:   "working on it",
		SentAt: sentAt,
	}
	if err := writeMessage(tasksDir, msg); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message file, got %d", len(entries))
	}

	name := entries[0].Name()
	pattern := regexp.MustCompile(`^20240501T123456\.123456789Z-agent-1-intent-abc12345\.json$`)
	if !pattern.MatchString(name) {
		t.Fatalf("filename %q does not match expected pattern", name)
	}

	data, err := os.ReadFile(filepath.Join(eventsDir, name))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("message file is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, msg) {
		t.Fatalf("message = %#v, want %#v", got, msg)
	}
}

func TestReadMessagesSortedByTime(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	messages := []Message{
		{ID: "m2", From: "agent", Type: "intent", Task: "task-2", Branch: "branch", Body: "second", SentAt: base.Add(2 * time.Minute)},
		{ID: "m1", From: "agent", Type: "intent", Task: "task-1", Branch: "branch", Body: "first", SentAt: base.Add(1 * time.Minute)},
		{ID: "m3", From: "agent", Type: "completion", Task: "task-3", Branch: "branch", Body: "third", SentAt: base.Add(3 * time.Minute)},
	}
	for _, msg := range messages {
		if err := writeMessage(tasksDir, msg); err != nil {
			t.Fatalf("writeMessage(%s): %v", msg.ID, err)
		}
	}

	got, err := readMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("readMessages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
	wantOrder := []string{"m1", "m2", "m3"}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Fatalf("messages[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

func TestReadMessagesFiltersBySince(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	for _, msg := range []Message{
		{ID: "old", From: "agent", Type: "intent", Task: "old", Branch: "branch", Body: "old", SentAt: base.Add(1 * time.Minute)},
		{ID: "edge", From: "agent", Type: "intent", Task: "edge", Branch: "branch", Body: "edge", SentAt: base.Add(2 * time.Minute)},
		{ID: "new", From: "agent", Type: "completion", Task: "new", Branch: "branch", Body: "new", SentAt: base.Add(3 * time.Minute)},
	} {
		if err := writeMessage(tasksDir, msg); err != nil {
			t.Fatalf("writeMessage(%s): %v", msg.ID, err)
		}
	}

	got, err := readMessages(tasksDir, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("readMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message newer than cutoff, got %d", len(got))
	}
	if got[0].ID != "new" {
		t.Fatalf("expected newest message, got %q", got[0].ID)
	}
}

func TestWritePresence(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	if err := writePresence(tasksDir, "agent-1", "task.md", "feature/branch"); err != nil {
		t.Fatalf("writePresence: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "presence", "agent-1.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got struct {
		AgentID   string    `json:"agent_id"`
		Task      string    `json:"task"`
		Branch    string    `json:"branch"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("presence file is not valid JSON: %v", err)
	}
	if got.AgentID != "agent-1" || got.Task != "task.md" || got.Branch != "feature/branch" {
		t.Fatalf("presence = %#v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("presence timestamp should be set")
	}
}

func TestCleanStalePresence(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, ".locks", "live.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile live lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, ".locks", "dead.pid"), []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("WriteFile dead lock: %v", err)
	}
	if err := writePresence(tasksDir, "live", "live-task.md", "live-branch"); err != nil {
		t.Fatalf("writePresence live: %v", err)
	}
	if err := writePresence(tasksDir, "dead", "dead-task.md", "dead-branch"); err != nil {
		t.Fatalf("writePresence dead: %v", err)
	}

	cleanStalePresence(tasksDir)

	if _, err := os.Stat(filepath.Join(tasksDir, "messages", "presence", "live.json")); err != nil {
		t.Fatalf("live presence should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "messages", "presence", "dead.json")); !os.IsNotExist(err) {
		t.Fatal("dead presence should be removed")
	}
}

func TestCleanOldMessages(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	oldMsg := Message{ID: "oldmsg", From: "agent", Type: "intent", Task: "old", Branch: "branch", Body: "old", SentAt: time.Date(2024, time.May, 1, 10, 0, 0, 0, time.UTC)}
	recentMsg := Message{ID: "newmsg", From: "agent", Type: "intent", Task: "new", Branch: "branch", Body: "new", SentAt: time.Date(2024, time.May, 1, 11, 0, 0, 0, time.UTC)}
	for _, msg := range []Message{oldMsg, recentMsg} {
		if err := writeMessage(tasksDir, msg); err != nil {
			t.Fatalf("writeMessage(%s): %v", msg.ID, err)
		}
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	oldFile := findEventFile(t, eventsDir, "oldmsg")
	recentFile := findEventFile(t, eventsDir, "newmsg")
	now := time.Now()
	if err := os.Chtimes(oldFile, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Chtimes old: %v", err)
	}
	if err := os.Chtimes(recentFile, now, now); err != nil {
		t.Fatalf("Chtimes recent: %v", err)
	}

	cleanOldMessages(tasksDir, 24*time.Hour)

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("old message should be removed")
	}
	if _, err := os.Stat(recentFile); err != nil {
		t.Fatalf("recent message should remain: %v", err)
	}
}

func TestWriteMessageAtomicWrite(t *testing.T) {
	tasksDir := t.TempDir()
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	msg := Message{
		ID:     "atomic01",
		From:   "agent-1",
		Type:   "completion",
		Task:   "task.md",
		Branch: "branch",
		Body:   strings.Repeat("done", 1024),
		SentAt: time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := writeMessage(tasksDir, msg); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 final file, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), ".tmp-") {
		t.Fatalf("temporary file should not remain: %q", entries[0].Name())
	}

	data, err := os.ReadFile(filepath.Join(eventsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("final message file should contain complete JSON")
	}
}

func findEventFile(t *testing.T, eventsDir, id string) string {
	t.Helper()
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), id) {
			return filepath.Join(eventsDir, entry.Name())
		}
	}
	t.Fatalf("message file for %q not found", id)
	return ""
}
