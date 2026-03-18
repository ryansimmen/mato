package messaging

import (
	"encoding/json"
	"errors"
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
		filepath.Join(tasksDir, "messages", "completions"),
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
		Type:   "intent",
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

func TestReadAllPresence(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := WritePresence(tasksDir, "agent-a", "task-a.md", "task/branch-a"); err != nil {
		t.Fatalf("WritePresence a: %v", err)
	}
	if err := WritePresence(tasksDir, "agent-b", "task-b.md", "task/branch-b"); err != nil {
		t.Fatalf("WritePresence b: %v", err)
	}

	result, err := ReadAllPresence(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllPresence: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 presence entries, got %d", len(result))
	}
	a, ok := result["agent-a"]
	if !ok {
		t.Fatal("expected presence for agent-a")
	}
	if a.Task != "task-a.md" || a.Branch != "task/branch-a" {
		t.Fatalf("agent-a presence = %+v", a)
	}
	b, ok := result["agent-b"]
	if !ok {
		t.Fatal("expected presence for agent-b")
	}
	if b.Task != "task-b.md" || b.Branch != "task/branch-b" {
		t.Fatalf("agent-b presence = %+v", b)
	}
}

func TestReadAllPresenceEmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	result, err := ReadAllPresence(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllPresence: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(result))
	}
}

func TestReadAllPresenceNonExistentDir(t *testing.T) {
	result, err := ReadAllPresence(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("ReadAllPresence should not error on missing dir: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
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

func TestWriteMessage_ValidTypes(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	for msgType := range ValidMessageTypes {
		msg := Message{
			From:   "agent",
			Type:   msgType,
			Task:   "task.md",
			Branch: "branch",
			Body:   "test",
		}
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage with type %q should succeed: %v", msgType, err)
		}
	}

	got, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(got) != len(ValidMessageTypes) {
		t.Fatalf("expected %d messages, got %d", len(ValidMessageTypes), len(got))
	}
}

func TestWriteMessage_InvalidType(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	msg := Message{
		From:   "agent",
		Type:   "unknown-type",
		Task:   "task.md",
		Branch: "branch",
		Body:   "test",
	}
	err := WriteMessage(tasksDir, msg)
	if err == nil {
		t.Fatal("WriteMessage with unknown type should return an error")
	}
	if !strings.Contains(err.Error(), "unknown message type") {
		t.Fatalf("error should mention unknown message type, got: %v", err)
	}
}

func TestWriteMessage_EmptyType(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	msg := Message{
		From:   "agent",
		Task:   "task.md",
		Branch: "branch",
		Body:   "test",
	}
	err := WriteMessage(tasksDir, msg)
	if err == nil {
		t.Fatal("WriteMessage with empty type should return an error")
	}
	if !strings.Contains(err.Error(), "empty message type") {
		t.Fatalf("error should mention empty message type, got: %v", err)
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

func TestWriteCompletionDetail(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	mergedAt := time.Date(2026, time.March, 17, 21, 35, 0, 0, time.UTC)
	detail := CompletionDetail{
		TaskID:       "add-http-retries",
		TaskFile:     "add-http-retries.md",
		Branch:       "task/add-http-retries",
		CommitSHA:    "abc123def",
		FilesChanged: []string{"pkg/client/http.go", "pkg/client/retry.go"},
		Title:        "Add HTTP retries",
		MergedAt:     mergedAt,
	}
	if err := WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	path := filepath.Join(tasksDir, "messages", "completions", "add-http-retries.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("completion detail file missing: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("completion detail file should contain valid JSON")
	}

	var got CompletionDetail
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.TaskID != detail.TaskID {
		t.Fatalf("TaskID = %q, want %q", got.TaskID, detail.TaskID)
	}
	if got.CommitSHA != detail.CommitSHA {
		t.Fatalf("CommitSHA = %q, want %q", got.CommitSHA, detail.CommitSHA)
	}
	if !reflect.DeepEqual(got.FilesChanged, detail.FilesChanged) {
		t.Fatalf("FilesChanged = %v, want %v", got.FilesChanged, detail.FilesChanged)
	}
	if !got.MergedAt.Equal(mergedAt) {
		t.Fatalf("MergedAt = %v, want %v", got.MergedAt, mergedAt)
	}
}

func TestReadCompletionDetail(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	mergedAt := time.Date(2026, time.March, 17, 21, 35, 0, 0, time.UTC)
	detail := CompletionDetail{
		TaskID:       "add-retries",
		TaskFile:     "add-retries.md",
		Branch:       "task/add-retries",
		CommitSHA:    "def456abc",
		FilesChanged: []string{"main.go"},
		Title:        "Add retries",
		MergedAt:     mergedAt,
	}
	if err := WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	got, err := ReadCompletionDetail(tasksDir, "add-retries")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if got.TaskID != "add-retries" {
		t.Fatalf("TaskID = %q, want %q", got.TaskID, "add-retries")
	}
	if got.CommitSHA != "def456abc" {
		t.Fatalf("CommitSHA = %q, want %q", got.CommitSHA, "def456abc")
	}
	if !reflect.DeepEqual(got.FilesChanged, []string{"main.go"}) {
		t.Fatalf("FilesChanged = %v, want [main.go]", got.FilesChanged)
	}
}

func TestReadCompletionDetail_NotFound(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	_, err := ReadCompletionDetail(tasksDir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent completion detail")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestWriteCompletionDetail_EmptyTaskID(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	err := WriteCompletionDetail(tasksDir, CompletionDetail{})
	if err == nil {
		t.Fatal("expected error for empty task ID")
	}
	if !strings.Contains(err.Error(), "requires a task ID") {
		t.Fatalf("error = %q, want 'requires a task ID'", err)
	}
}

func TestWriteCompletionDetail_DefaultsMergedAt(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	before := time.Now().UTC()
	detail := CompletionDetail{
		TaskID:   "auto-time",
		TaskFile: "auto-time.md",
		Title:    "Auto time",
	}
	if err := WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}
	after := time.Now().UTC()

	got, err := ReadCompletionDetail(tasksDir, "auto-time")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if got.MergedAt.Before(before) || got.MergedAt.After(after) {
		t.Fatalf("MergedAt = %v, want between %v and %v", got.MergedAt, before, after)
	}
}

func TestWriteCompletionDetail_SanitizesPathTraversal(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	detail := CompletionDetail{
		TaskID:   "../evil",
		TaskFile: "evil.md",
		Title:    "Evil task",
	}
	if err := WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	// The file must be written inside completions/, not outside it.
	sanitized := "--evil"
	sanitized = strings.Trim(sanitized, "-_. ")
	expectedPath := filepath.Join(tasksDir, "messages", "completions", "evil.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected sanitized file at %s, got error: %v", expectedPath, err)
	}

	// Verify we can read it back using the same TaskID.
	got, err := ReadCompletionDetail(tasksDir, "../evil")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if got.TaskID != "../evil" {
		t.Fatalf("TaskID = %q, want %q", got.TaskID, "../evil")
	}
}

func TestWriteCompletionDetail_SanitizesSlashes(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	detail := CompletionDetail{
		TaskID:   "foo/bar",
		TaskFile: "foo-bar.md",
		Title:    "Foo bar",
	}
	if err := WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	// File should be foo-bar.json, not foo/bar.json
	expectedPath := filepath.Join(tasksDir, "messages", "completions", "foo-bar.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected sanitized file at %s, got error: %v", expectedPath, err)
	}

	got, err := ReadCompletionDetail(tasksDir, "foo/bar")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if got.TaskID != "foo/bar" {
		t.Fatalf("TaskID = %q, want %q", got.TaskID, "foo/bar")
	}
}

func TestWriteCompletionDetail_NormalIDUnchanged(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	detail := CompletionDetail{
		TaskID:   "add-http-retries",
		TaskFile: "add-http-retries.md",
		Title:    "Add HTTP retries",
	}
	if err := WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	// Normal kebab-case ID should remain unchanged.
	expectedPath := filepath.Join(tasksDir, "messages", "completions", "add-http-retries.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected file at %s, got error: %v", expectedPath, err)
	}

	got, err := ReadCompletionDetail(tasksDir, "add-http-retries")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if got.TaskID != "add-http-retries" {
		t.Fatalf("TaskID = %q, want %q", got.TaskID, "add-http-retries")
	}
}

func TestBuildAndWriteFileClaims(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create in-progress task with affects
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "fix-race.md"),
		[]byte("---\naffects:\n  - internal/queue/queue.go\n  - internal/queue/queue_test.go\n---\n# Fix Race\n"), 0o644)

	// Create ready-to-merge task with affects
	os.WriteFile(filepath.Join(tasksDir, "ready-to-merge", "add-locks.md"),
		[]byte("---\naffects:\n  - internal/merge/merge.go\n---\n# Add Locks\n"), 0o644)

	if err := BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	path := filepath.Join(tasksDir, "messages", "file-claims.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}

	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(claims) != 3 {
		t.Fatalf("expected 3 claims, got %d: %v", len(claims), claims)
	}

	if c, ok := claims["internal/queue/queue.go"]; !ok || c.Task != "fix-race.md" || c.Status != "in-progress" {
		t.Fatalf("unexpected claim for queue.go: %+v", c)
	}
	if c, ok := claims["internal/queue/queue_test.go"]; !ok || c.Task != "fix-race.md" || c.Status != "in-progress" {
		t.Fatalf("unexpected claim for queue_test.go: %+v", c)
	}
	if c, ok := claims["internal/merge/merge.go"]; !ok || c.Task != "add-locks.md" || c.Status != "ready-to-merge" {
		t.Fatalf("unexpected claim for merge.go: %+v", c)
	}
}

func TestBuildAndWriteFileClaims_Empty(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	path := filepath.Join(tasksDir, "messages", "file-claims.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}

	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(claims) != 0 {
		t.Fatalf("expected empty claims, got %d: %v", len(claims), claims)
	}
}

func TestBuildAndWriteFileClaims_AtomicWrite(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	os.WriteFile(filepath.Join(tasksDir, "in-progress", "task-a.md"),
		[]byte("---\naffects:\n  - file-a.go\n---\n# Task A\n"), 0o644)

	if err := BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	// Verify no temp files remain in messages dir
	entries, err := os.ReadDir(filepath.Join(tasksDir, "messages"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file should not remain: %q", e.Name())
		}
	}

	// Verify the file is valid JSON
	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("file-claims.json should contain valid JSON")
	}
}

func TestBuildAndWriteFileClaims_FirstWriterWins(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Two tasks claim the same file
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "task-a.md"),
		[]byte("---\naffects:\n  - shared.go\n---\n# Task A\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "ready-to-merge", "task-b.md"),
		[]byte("---\naffects:\n  - shared.go\n---\n# Task B\n"), 0o644)

	if err := BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Should have exactly one entry for shared.go (first writer wins)
	if len(claims) != 1 {
		t.Fatalf("expected 1 claim, got %d", len(claims))
	}
	c, ok := claims["shared.go"]
	if !ok {
		t.Fatal("expected claim for shared.go")
	}
	// The first writer should be from in-progress (collectActiveAffects scans it first)
	if c.Task != "task-a.md" {
		t.Fatalf("expected first writer task-a.md, got %q", c.Task)
	}
}

func TestBuildAndWriteFileClaims_ExcludeTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// The excluded task (simulating the just-claimed task)
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "my-task.md"),
		[]byte("---\naffects:\n  - internal/foo.go\n  - internal/bar.go\n---\n# My Task\n"), 0o644)

	// Another in-progress task that should remain
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "other-task.md"),
		[]byte("---\naffects:\n  - internal/baz.go\n---\n# Other Task\n"), 0o644)

	if err := BuildAndWriteFileClaims(tasksDir, "my-task.md"); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}

	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Only the non-excluded task's files should appear
	if len(claims) != 1 {
		t.Fatalf("expected 1 claim, got %d: %v", len(claims), claims)
	}
	if _, ok := claims["internal/foo.go"]; ok {
		t.Fatal("excluded task's file internal/foo.go should not appear in claims")
	}
	if _, ok := claims["internal/bar.go"]; ok {
		t.Fatal("excluded task's file internal/bar.go should not appear in claims")
	}
	if c, ok := claims["internal/baz.go"]; !ok || c.Task != "other-task.md" {
		t.Fatalf("expected claim for internal/baz.go from other-task.md, got %+v", claims)
	}
}

func TestBuildAndWriteFileClaims_EmptyExclude(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	os.WriteFile(filepath.Join(tasksDir, "in-progress", "task-x.md"),
		[]byte("---\naffects:\n  - x.go\n---\n# Task X\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "ready-to-merge", "task-y.md"),
		[]byte("---\naffects:\n  - y.go\n---\n# Task Y\n"), 0o644)

	// Empty excludeTask means all tasks are included (backward compat)
	if err := BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}

	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(claims) != 2 {
		t.Fatalf("expected 2 claims, got %d: %v", len(claims), claims)
	}
	if _, ok := claims["x.go"]; !ok {
		t.Fatal("expected claim for x.go")
	}
	if _, ok := claims["y.go"]; !ok {
		t.Fatal("expected claim for y.go")
	}
}

func TestBuildAndWriteFileClaims_ExcludeOnlyMatchingTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Three tasks: one to exclude, two to keep
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "excluded.md"),
		[]byte("---\naffects:\n  - shared.go\n  - only-excluded.go\n---\n# Excluded\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "kept-a.md"),
		[]byte("---\naffects:\n  - shared.go\n  - a.go\n---\n# Kept A\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "ready-to-merge", "kept-b.md"),
		[]byte("---\naffects:\n  - b.go\n---\n# Kept B\n"), 0o644)

	if err := BuildAndWriteFileClaims(tasksDir, "excluded.md"); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}

	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Should have 3 files: shared.go (from kept-a), a.go, b.go
	if len(claims) != 3 {
		t.Fatalf("expected 3 claims, got %d: %v", len(claims), claims)
	}
	if _, ok := claims["only-excluded.go"]; ok {
		t.Fatal("only-excluded.go should not appear in claims")
	}
	if c := claims["shared.go"]; c.Task != "kept-a.md" {
		t.Fatalf("shared.go should be claimed by kept-a.md, got %q", c.Task)
	}
	if c := claims["a.go"]; c.Task != "kept-a.md" {
		t.Fatalf("a.go should be claimed by kept-a.md, got %q", c.Task)
	}
	if c := claims["b.go"]; c.Task != "kept-b.md" {
		t.Fatalf("b.go should be claimed by kept-b.md, got %q", c.Task)
	}
}

func TestWriteMessage_ProgressType(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	msg := Message{
		ID:     "progress1",
		From:   "agent-abc12345",
		Type:   "progress",
		Task:   "fix-race.md",
		Branch: "task/fix-race",
		Body:   "Step: WORK",
		SentAt: time.Date(2026, time.March, 18, 2, 30, 0, 0, time.UTC),
	}
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage with type progress should succeed: %v", err)
	}

	messages, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Type != "progress" {
		t.Fatalf("type = %q, want %q", messages[0].Type, "progress")
	}
	if messages[0].Body != "Step: WORK" {
		t.Fatalf("body = %q, want %q", messages[0].Body, "Step: WORK")
	}
}

func TestReadAllCompletionDetails(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	t1 := time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, time.March, 16, 12, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, time.March, 17, 14, 0, 0, 0, time.UTC)

	for _, d := range []CompletionDetail{
		{TaskID: "task-a", TaskFile: "task-a.md", Branch: "task/a", CommitSHA: "aaa", Title: "Task A", MergedAt: t1},
		{TaskID: "task-b", TaskFile: "task-b.md", Branch: "task/b", CommitSHA: "bbb", Title: "Task B", MergedAt: t2},
		{TaskID: "task-c", TaskFile: "task-c.md", Branch: "task/c", CommitSHA: "ccc", Title: "Task C", MergedAt: t3},
	} {
		if err := WriteCompletionDetail(tasksDir, d); err != nil {
			t.Fatalf("WriteCompletionDetail %s: %v", d.TaskID, err)
		}
	}

	got, err := ReadAllCompletionDetails(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllCompletionDetails: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 completion details, got %d", len(got))
	}
	if got[0].TaskID != "task-c" {
		t.Fatalf("got[0].TaskID = %q, want %q", got[0].TaskID, "task-c")
	}
	if got[1].TaskID != "task-b" {
		t.Fatalf("got[1].TaskID = %q, want %q", got[1].TaskID, "task-b")
	}
	if got[2].TaskID != "task-a" {
		t.Fatalf("got[2].TaskID = %q, want %q", got[2].TaskID, "task-a")
	}
}

func TestReadAllCompletionDetailsEmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	got, err := ReadAllCompletionDetails(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllCompletionDetails: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestReadAllCompletionDetailsNonExistentDir(t *testing.T) {
	got, err := ReadAllCompletionDetails(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("ReadAllCompletionDetails should not error on missing dir: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}
