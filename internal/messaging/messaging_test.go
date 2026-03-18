package messaging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInitMessaging(t *testing.T) {
	tasksDir := t.TempDir()

	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
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
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
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
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
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

func TestWriteMessage_EmptyFields(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := WriteMessage(tasksDir, Message{Type: "intent"}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message file, got %d", len(entries))
	}

	data, err := os.ReadFile(filepath.Join(eventsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("message file should contain valid JSON")
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Type != "intent" {
		t.Fatalf("message type = %q, want %q", got.Type, "intent")
	}
	if got.ID == "" {
		t.Fatal("message ID should be generated")
	}
	if got.SentAt.IsZero() {
		t.Fatal("message SentAt should be set")
	}
}

func TestWriteMessage_SpecialCharsInFields(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	msg := Message{
		ID:     "special-1",
		From:   "agent \"雪\"\nbackslash \\",
		Type:   "status \"update\"\nline",
		Task:   "task \"quote\"\n第二行",
		Branch: "feature/雪\\branch",
		Body:   "body with \"quotes\"\nmultiple lines\nunicode 雪\nbackslash \\",
		SentAt: time.Date(2024, time.May, 1, 13, 0, 0, 0, time.UTC),
	}
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0], msg) {
		t.Fatalf("round-trip message = %#v, want %#v", got[0], msg)
	}
}

func TestReadMessagesSortedByTime(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	messages := []Message{
		{ID: "m2", From: "agent", Type: "intent", Task: "task-2", Branch: "branch", Body: "second", SentAt: base.Add(2 * time.Minute)},
		{ID: "m1", From: "agent", Type: "intent", Task: "task-1", Branch: "branch", Body: "first", SentAt: base.Add(1 * time.Minute)},
		{ID: "m3", From: "agent", Type: "completion", Task: "task-3", Branch: "branch", Body: "third", SentAt: base.Add(3 * time.Minute)},
	}
	for _, msg := range messages {
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", msg.ID, err)
		}
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
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
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	for _, msg := range []Message{
		{ID: "old", From: "agent", Type: "intent", Task: "old", Branch: "branch", Body: "old", SentAt: base.Add(1 * time.Minute)},
		{ID: "edge", From: "agent", Type: "intent", Task: "edge", Branch: "branch", Body: "edge", SentAt: base.Add(2 * time.Minute)},
		{ID: "new", From: "agent", Type: "completion", Task: "new", Branch: "branch", Body: "new", SentAt: base.Add(3 * time.Minute)},
	} {
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", msg.ID, err)
		}
	}

	got, err := ReadMessages(tasksDir, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message newer than cutoff, got %d", len(got))
	}
	if got[0].ID != "new" {
		t.Fatalf("expected newest message, got %q", got[0].ID)
	}
}

func TestReadMessages_EmptyEventsDir(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	got, err := ReadMessages(tasksDir, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if got == nil {
		t.Fatal("expected empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected no messages, got %d", len(got))
	}
}

func TestReadMessages_ConcurrentWrite(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const total = 20
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < total; i++ {
			msg := Message{
				ID:     "msg-" + strconv.Itoa(i),
				From:   "writer",
				Type:   "intent",
				Task:   "task-" + strconv.Itoa(i),
				Branch: "branch",
				Body:   strings.Repeat("payload-", 16) + strconv.Itoa(i),
				SentAt: base.Add(time.Duration(i) * time.Millisecond),
			}
			if err := WriteMessage(tasksDir, msg); err != nil {
				errCh <- fmt.Errorf("WriteMessage(%s): %w", msg.ID, err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(5 * time.Second)
		for {
			msgs, err := ReadMessages(tasksDir, time.Time{})
			if err != nil {
				errCh <- fmt.Errorf("ReadMessages: %w", err)
				return
			}
			if len(msgs) == total {
				for i, msg := range msgs {
					wantID := "msg-" + strconv.Itoa(i)
					if msg.ID != wantID {
						errCh <- fmt.Errorf("messages[%d].ID = %q, want %q", i, msg.ID, wantID)
						return
					}
				}
				return
			}
			if time.Now().After(deadline) {
				errCh <- fmt.Errorf("timed out waiting for %d messages, got %d", total, len(msgs))
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages final: %v", err)
	}
	if len(got) != total {
		t.Fatalf("final ReadMessages count = %d, want %d", len(got), total)
	}
}

func TestReadMessages_SkipsMalformedFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	valid := Message{
		ID:     "valid",
		From:   "agent",
		Type:   "intent",
		Task:   "task.md",
		Branch: "branch",
		Body:   "ok",
		SentAt: time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := WriteMessage(tasksDir, valid); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	if err := os.WriteFile(filepath.Join(eventsDir, "broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatalf("WriteFile broken.json: %v", err)
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 valid message, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0], valid) {
		t.Fatalf("message = %#v, want %#v", got[0], valid)
	}
}

func TestReadMessages_FileDeletedDuringRead(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const total = 5
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	payload := strings.Repeat("payload-", 256*1024)
	wantByID := make(map[string]Message, total)
	for i := 0; i < total; i++ {
		msg := Message{
			ID:     "msg-" + strconv.Itoa(i),
			From:   "writer",
			Type:   "intent",
			Task:   "task-" + strconv.Itoa(i),
			Branch: "branch",
			Body:   payload + strconv.Itoa(i),
			SentAt: base.Add(time.Duration(i) * time.Millisecond),
		}
		wantByID[msg.ID] = msg
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", msg.ID, err)
		}
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != total {
		t.Fatalf("expected %d message files, got %d", total, len(entries))
	}

	errCh := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10; i++ {
			msgs, err := ReadMessages(tasksDir, time.Time{})
			if err != nil {
				errCh <- fmt.Errorf("ReadMessages iteration %d: %w", i, err)
				return
			}
			if len(msgs) > total {
				errCh <- fmt.Errorf("ReadMessages iteration %d returned %d messages, want <= %d", i, len(msgs), total)
				return
			}
			for j, msg := range msgs {
				want, ok := wantByID[msg.ID]
				if !ok {
					errCh <- fmt.Errorf("ReadMessages iteration %d returned unexpected message ID %q", i, msg.ID)
					return
				}
				if !reflect.DeepEqual(msg, want) {
					errCh <- fmt.Errorf("ReadMessages iteration %d returned %#v, want %#v", i, msg, want)
					return
				}
				if j > 0 && msgs[j-1].SentAt.After(msg.SentAt) {
					errCh <- fmt.Errorf("ReadMessages iteration %d returned messages out of order", i)
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(1 * time.Millisecond)
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(eventsDir, entry.Name())); err != nil && !os.IsNotExist(err) {
				errCh <- fmt.Errorf("Remove(%s): %w", entry.Name(), err)
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages final: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("final ReadMessages count = %d, want 0", len(got))
	}
}

func TestWritePresence(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := WritePresence(tasksDir, "agent-1", "task.md", "feature/branch"); err != nil {
		t.Fatalf("WritePresence: %v", err)
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
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
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
	if err := WritePresence(tasksDir, "live", "live-task.md", "live-branch"); err != nil {
		t.Fatalf("WritePresence live: %v", err)
	}
	if err := WritePresence(tasksDir, "dead", "dead-task.md", "dead-branch"); err != nil {
		t.Fatalf("WritePresence dead: %v", err)
	}

	CleanStalePresence(tasksDir)

	if _, err := os.Stat(filepath.Join(tasksDir, "messages", "presence", "live.json")); err != nil {
		t.Fatalf("live presence should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "messages", "presence", "dead.json")); !os.IsNotExist(err) {
		t.Fatal("dead presence should be removed")
	}
}

func TestCleanOldMessages(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	oldMsg := Message{ID: "oldmsg", From: "agent", Type: "intent", Task: "old", Branch: "branch", Body: "old", SentAt: time.Date(2024, time.May, 1, 10, 0, 0, 0, time.UTC)}
	recentMsg := Message{ID: "newmsg", From: "agent", Type: "intent", Task: "new", Branch: "branch", Body: "new", SentAt: time.Date(2024, time.May, 1, 11, 0, 0, 0, time.UTC)}
	for _, msg := range []Message{oldMsg, recentMsg} {
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", msg.ID, err)
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

	CleanOldMessages(tasksDir, 24*time.Hour)

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("old message should be removed")
	}
	if _, err := os.Stat(recentFile); err != nil {
		t.Fatalf("recent message should remain: %v", err)
	}
}

func TestWriteMessageAtomicWrite(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
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
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
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

func TestFilesFieldRoundtrip(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	msg := Message{
		ID:     "files-rt",
		From:   "agent-1",
		Type:   "conflict-warning",
		Task:   "task.md",
		Branch: "feature/test",
		Files:  []string{"internal/messaging/messaging.go", "docs/messaging.md"},
		Body:   "About to push",
		SentAt: time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0].Files, msg.Files) {
		t.Fatalf("Files = %v, want %v", got[0].Files, msg.Files)
	}
	if !reflect.DeepEqual(got[0], msg) {
		t.Fatalf("round-trip message = %#v, want %#v", got[0], msg)
	}
}

func TestFilesFieldBackwardCompat(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write a message without the files field (simulates old-format JSON)
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	raw := `{"id":"no-files","from":"agent","type":"intent","task":"task.md","branch":"branch","body":"hello","sent_at":"2024-05-01T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(eventsDir, "no-files.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Files != nil {
		t.Fatalf("Files should be nil for messages without files field, got %v", got[0].Files)
	}
	if got[0].ID != "no-files" {
		t.Fatalf("ID = %q, want %q", got[0].ID, "no-files")
	}
}

func TestFilesFieldOmitempty(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	msg := Message{
		ID:     "no-files-omit",
		From:   "agent",
		Type:   "intent",
		Task:   "task.md",
		Branch: "branch",
		Body:   "working",
		SentAt: time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(eventsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), `"files"`) {
		t.Fatalf("JSON should not contain files key when Files is nil, got: %s", data)
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
