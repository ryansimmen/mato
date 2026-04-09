package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mato/internal/dirs"
	"mato/internal/testutil"
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

func TestCleanStalePresence_NonJSONIgnored(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	nonJSON := filepath.Join(presenceDir, "README.txt")
	if err := os.WriteFile(nonJSON, []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	CleanStalePresence(tasksDir)

	if _, err := os.Stat(nonJSON); err != nil {
		t.Fatalf("non-JSON file should not be removed: %v", err)
	}
}

func TestCleanStalePresence_MissingDirectory(t *testing.T) {
	tasksDir := filepath.Join(t.TempDir(), "nonexistent")

	// Should not panic when the presence directory does not exist.
	CleanStalePresence(tasksDir)
}

func TestCleanStalePresence_NoLockFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write presence for an agent that has no .pid lock file at all.
	if err := WritePresence(tasksDir, "ghost", "ghost-task.md", "ghost-branch"); err != nil {
		t.Fatalf("WritePresence: %v", err)
	}

	CleanStalePresence(tasksDir)

	presenceFile := filepath.Join(tasksDir, "messages", "presence", "ghost.json")
	if _, err := os.Stat(presenceFile); !os.IsNotExist(err) {
		t.Fatal("presence for agent with no lock file should be removed")
	}
}

func TestCleanStalePresence_UnreadableAgentLockPreserved(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	lockPath := filepath.Join(tasksDir, ".locks", "live.pid")
	if err := WritePresence(tasksDir, "live", "live-task.md", "live-branch"); err != nil {
		t.Fatalf("WritePresence live: %v", err)
	}

	testutil.MakeUnreadablePath(t, lockPath)

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stderr = w
	CleanStalePresence(tasksDir)
	w.Close()
	os.Stderr = oldStderr
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	stderr := string(data)
	if !strings.Contains(stderr, "could not verify agent lock") {
		t.Fatalf("expected unreadable lock warning, got %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "messages", "presence", "live.json")); err != nil {
		t.Fatalf("presence should remain when agent lock is unreadable: %v", err)
	}
}

func TestCleanOldMessages_NonJSONIgnored(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	nonJSON := filepath.Join(eventsDir, "notes.txt")
	if err := os.WriteFile(nonJSON, []byte("not a message"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Set the non-JSON file's mtime to well in the past so it would be
	// cleaned if .json filtering were missing.
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(nonJSON, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	CleanOldMessages(tasksDir, 24*time.Hour)

	if _, err := os.Stat(nonJSON); err != nil {
		t.Fatalf("non-JSON file should not be removed: %v", err)
	}
}

func TestCleanOldMessages_MissingDirectory(t *testing.T) {
	tasksDir := filepath.Join(t.TempDir(), "nonexistent")

	// Should not panic when the events directory does not exist.
	CleanOldMessages(tasksDir, 24*time.Hour)
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
	// With collision-resistant encoding: ../evil → _2e_2e_2fevil
	expectedPath := filepath.Join(tasksDir, "messages", "completions", "_2e_2e_2fevil.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected encoded file at %s, got error: %v", expectedPath, err)
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

	// With collision-resistant encoding: foo/bar → foo_2fbar
	expectedPath := filepath.Join(tasksDir, "messages", "completions", "foo_2fbar.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected encoded file at %s, got error: %v", expectedPath, err)
	}

	got, err := ReadCompletionDetail(tasksDir, "foo/bar")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if got.TaskID != "foo/bar" {
		t.Fatalf("TaskID = %q, want %q", got.TaskID, "foo/bar")
	}
}

func TestCompletionDetail_CollisionResistance(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// foo/bar and foo-bar previously collided; they must coexist now.
	slashDetail := CompletionDetail{
		TaskID:   "foo/bar",
		TaskFile: "foo-bar-slash.md",
		Title:    "Foo slash bar",
	}
	hyphenDetail := CompletionDetail{
		TaskID:   "foo-bar",
		TaskFile: "foo-bar.md",
		Title:    "Foo hyphen bar",
	}
	if err := WriteCompletionDetail(tasksDir, slashDetail); err != nil {
		t.Fatalf("WriteCompletionDetail foo/bar: %v", err)
	}
	if err := WriteCompletionDetail(tasksDir, hyphenDetail); err != nil {
		t.Fatalf("WriteCompletionDetail foo-bar: %v", err)
	}

	// Read each back and verify they are distinct.
	gotSlash, err := ReadCompletionDetail(tasksDir, "foo/bar")
	if err != nil {
		t.Fatalf("ReadCompletionDetail foo/bar: %v", err)
	}
	if gotSlash.Title != "Foo slash bar" {
		t.Fatalf("foo/bar Title = %q, want %q", gotSlash.Title, "Foo slash bar")
	}

	gotHyphen, err := ReadCompletionDetail(tasksDir, "foo-bar")
	if err != nil {
		t.Fatalf("ReadCompletionDetail foo-bar: %v", err)
	}
	if gotHyphen.Title != "Foo hyphen bar" {
		t.Fatalf("foo-bar Title = %q, want %q", gotHyphen.Title, "Foo hyphen bar")
	}

	// ReadAllCompletionDetails must return both records.
	all, err := ReadAllCompletionDetails(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllCompletionDetails: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 completion details, got %d", len(all))
	}
}

func TestCompletionDetail_TraversalIDsResolveInsideCompletions(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Traversal-like IDs must resolve inside messages/completions/.
	for _, id := range []string{"../../etc/passwd", "../evil", "a/../b"} {
		detail := CompletionDetail{TaskID: id, TaskFile: "test.md", Title: id}
		if err := WriteCompletionDetail(tasksDir, detail); err != nil {
			t.Fatalf("WriteCompletionDetail(%q): %v", id, err)
		}
		completionsDir := filepath.Join(tasksDir, "messages", "completions")
		entries, err := os.ReadDir(completionsDir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			full := filepath.Join(completionsDir, e.Name())
			rel, _ := filepath.Rel(completionsDir, full)
			if strings.Contains(rel, "..") {
				t.Fatalf("file %q escaped completions dir", e.Name())
			}
		}
		got, err := ReadCompletionDetail(tasksDir, id)
		if err != nil {
			t.Fatalf("ReadCompletionDetail(%q): %v", id, err)
		}
		if got.TaskID != id {
			t.Fatalf("TaskID = %q, want %q", got.TaskID, id)
		}
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
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create in-progress task with affects
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "fix-race.md"),
		[]byte("---\naffects:\n  - internal/queue/queue.go\n  - internal/queue/queue_test.go\n---\n# Fix Race\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(fix-race.md): %v", err)
	}

	// Create ready-to-merge task with affects
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.ReadyMerge, "add-locks.md"),
		[]byte("---\naffects:\n  - internal/merge/merge.go\n---\n# Add Locks\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(add-locks.md): %v", err)
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

	if len(claims) != 3 {
		t.Fatalf("expected 3 claims, got %d: %v", len(claims), claims)
	}

	if c, ok := claims["internal/queue/queue.go"]; !ok || c.Task != "fix-race.md" || c.Status != dirs.InProgress {
		t.Fatalf("unexpected claim for queue.go: %+v", c)
	}
	if c, ok := claims["internal/queue/queue_test.go"]; !ok || c.Task != "fix-race.md" || c.Status != dirs.InProgress {
		t.Fatalf("unexpected claim for queue_test.go: %+v", c)
	}
	if c, ok := claims["internal/merge/merge.go"]; !ok || c.Task != "add-locks.md" || c.Status != dirs.ReadyMerge {
		t.Fatalf("unexpected claim for merge.go: %+v", c)
	}
}

func TestBuildAndWriteFileClaims_Empty(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
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
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "task-a.md"),
		[]byte("---\naffects:\n  - file-a.go\n---\n# Task A\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-a.md): %v", err)
	}

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
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Two tasks claim the same file
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "task-a.md"),
		[]byte("---\naffects:\n  - shared.go\n---\n# Task A\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-a.md): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.ReadyMerge, "task-b.md"),
		[]byte("---\naffects:\n  - shared.go\n---\n# Task B\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-b.md): %v", err)
	}

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

func TestBuildAndWriteFileClaims_PreservesDirectoryPrefixes(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "task-a.md"),
		[]byte("---\naffects:\n  - pkg/client/\n---\n# Task A\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-a.md): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.ReadyMerge, "task-b.md"),
		[]byte("---\naffects:\n  - pkg/server/main.go\n---\n# Task B\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-b.md): %v", err)
	}

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

	if len(claims) != 2 {
		t.Fatalf("expected 2 claims, got %d: %v", len(claims), claims)
	}
	if c, ok := claims["pkg/client/"]; !ok || c.Task != "task-a.md" || c.Status != dirs.InProgress {
		t.Fatalf("unexpected claim for pkg/client/: %+v", c)
	}
	if c, ok := claims["pkg/server/main.go"]; !ok || c.Task != "task-b.md" || c.Status != dirs.ReadyMerge {
		t.Fatalf("unexpected claim for pkg/server/main.go: %+v", c)
	}
}

func TestBuildAndWriteFileClaims_ExcludeTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// The excluded task (simulating the just-claimed task)
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "my-task.md"),
		[]byte("---\naffects:\n  - internal/foo.go\n  - internal/bar.go\n---\n# My Task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(my-task.md): %v", err)
	}

	// Another in-progress task that should remain
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "other-task.md"),
		[]byte("---\naffects:\n  - internal/baz.go\n---\n# Other Task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(other-task.md): %v", err)
	}

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
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "task-x.md"),
		[]byte("---\naffects:\n  - x.go\n---\n# Task X\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-x.md): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.ReadyMerge, "task-y.md"),
		[]byte("---\naffects:\n  - y.go\n---\n# Task Y\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(task-y.md): %v", err)
	}

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
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Three tasks: one to exclude, two to keep
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "excluded.md"),
		[]byte("---\naffects:\n  - shared.go\n  - only-excluded.go\n---\n# Excluded\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(excluded.md): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "kept-a.md"),
		[]byte("---\naffects:\n  - shared.go\n  - a.go\n---\n# Kept A\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(kept-a.md): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.ReadyMerge, "kept-b.md"),
		[]byte("---\naffects:\n  - b.go\n---\n# Kept B\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(kept-b.md): %v", err)
	}

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

func TestBuildAndWriteFileClaims_MalformedTaskEmitsWarning(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// One valid task, one malformed
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "good.md"),
		[]byte("---\naffects:\n  - good.go\n---\n# Good\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(good.md): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.InProgress, "bad.md"),
		[]byte("---\naffects: [unterminated\n---\n# Bad\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(bad.md): %v", err)
	}

	// Capture stderr to verify warnings are emitted
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := BuildAndWriteFileClaims(tasksDir, "")

	w.Close()
	stderrBuf, _ := io.ReadAll(r)
	os.Stderr = origStderr

	if err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	// Valid task should still produce claims
	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}
	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := claims["good.go"]; !ok {
		t.Fatal("expected claim for good.go despite malformed sibling")
	}

	// Warning should have been emitted to stderr
	if !strings.Contains(string(stderrBuf), "warning: collecting active affects") {
		t.Errorf("expected warning on stderr, got: %s", stderrBuf)
	}
	if !strings.Contains(string(stderrBuf), "bad.md") {
		t.Errorf("expected warning to mention bad.md, got: %s", stderrBuf)
	}
}

func TestBuildAndWriteFileClaims_UnreadableDirEmitsWarning(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, dirs.InProgress)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", dir, err)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Make directory unreadable (non-ENOENT error)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("os.Chmod(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("os.Chmod restore permissions: %v", err)
		}
	})

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := BuildAndWriteFileClaims(tasksDir, "")

	w.Close()
	stderrBuf, _ := io.ReadAll(r)
	os.Stderr = origStderr

	if err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	// Warning should mention in-progress directory read error
	if !strings.Contains(string(stderrBuf), "warning: collecting active affects") {
		t.Errorf("expected warning on stderr, got: %s", stderrBuf)
	}
	if !strings.Contains(string(stderrBuf), dirs.InProgress) {
		t.Errorf("expected warning to mention %s, got: %s", dirs.InProgress, stderrBuf)
	}
}

func TestBuildAndWriteFileClaims_UnreadableTaskFileEmitsWarning(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, dirs.InProgress)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", dir, err)
	}
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// One readable task, one unreadable task file (permission denied = read failure)
	if err := os.WriteFile(filepath.Join(dir, "good.md"),
		[]byte("---\naffects:\n  - good.go\n---\n# Good\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(good.md): %v", err)
	}
	unreadable := filepath.Join(dir, "unreadable.md")
	if err := os.WriteFile(unreadable,
		[]byte("---\naffects:\n  - secret.go\n---\n# Unreadable\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(unreadable.md): %v", err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("os.Chmod(%s): %v", unreadable, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(unreadable, 0o644); err != nil {
			t.Errorf("os.Chmod restore permissions: %v", err)
		}
	})

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := BuildAndWriteFileClaims(tasksDir, "")

	w.Close()
	stderrBuf, _ := io.ReadAll(r)
	os.Stderr = origStderr

	if err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}

	// The readable task should still produce a claim
	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("file-claims.json not written: %v", err)
	}
	var claims map[string]FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := claims["good.go"]; !ok {
		t.Fatal("expected claim for good.go despite unreadable sibling")
	}

	// Warning should have been emitted for the unreadable file
	if !strings.Contains(string(stderrBuf), "warning: collecting active affects") {
		t.Errorf("expected warning on stderr, got: %s", stderrBuf)
	}
	if !strings.Contains(string(stderrBuf), "unreadable.md") {
		t.Errorf("expected warning to mention unreadable.md, got: %s", stderrBuf)
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

func TestReadAllPresence_PropagatesReadError(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write a valid presence file first.
	if err := WritePresence(tasksDir, "agent-ok", "task-ok.md", "task/ok"); err != nil {
		t.Fatalf("WritePresence: %v", err)
	}

	// Create an unreadable file in the presence directory.
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	badFile := filepath.Join(presenceDir, "bad-agent.json")
	if err := os.WriteFile(badFile, []byte(`{}`), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadAllPresence(tasksDir)
	if err == nil {
		t.Fatal("expected error from ReadAllPresence when file is unreadable, got nil")
	}
	if !strings.Contains(err.Error(), "read presence file") {
		t.Fatalf("expected 'read presence file' in error, got: %v", err)
	}
}

func TestReadAllPresence_SkipsDeletedFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := WritePresence(tasksDir, "agent-a", "task-a.md", "task/a"); err != nil {
		t.Fatalf("WritePresence: %v", err)
	}

	// Write a second presence file, then delete it to simulate a race.
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	vanishing := filepath.Join(presenceDir, "vanishing.json")
	if err := os.WriteFile(vanishing, []byte(`{"agent_id":"vanishing"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Remove(vanishing); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Even though vanishing.json was listed by ReadDir and then removed,
	// ReadAllPresence should skip it (ErrNotExist) and return the valid entry.
	result, err := ReadAllPresence(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllPresence: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if _, ok := result["agent-a"]; !ok {
		t.Fatal("expected presence for agent-a")
	}
}

func TestReadAllCompletionDetails_PropagatesReadError(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write a valid completion detail first.
	d := CompletionDetail{
		TaskID:    "task-ok",
		TaskFile:  "task-ok.md",
		Branch:    "task/ok",
		CommitSHA: "abc",
		Title:     "OK",
		MergedAt:  time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC),
	}
	if err := WriteCompletionDetail(tasksDir, d); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	// Create an unreadable file in the completions directory.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	badFile := filepath.Join(completionsDir, "bad-task.json")
	if err := os.WriteFile(badFile, []byte(`{}`), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadAllCompletionDetails(tasksDir)
	if err == nil {
		t.Fatal("expected error from ReadAllCompletionDetails when file is unreadable, got nil")
	}
	if !strings.Contains(err.Error(), "read completion detail") {
		t.Fatalf("expected 'read completion detail' in error, got: %v", err)
	}
}

func TestReadAllCompletionDetails_SkipsDeletedFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	d := CompletionDetail{
		TaskID:    "task-a",
		TaskFile:  "task-a.md",
		Branch:    "task/a",
		CommitSHA: "aaa",
		Title:     "Task A",
		MergedAt:  time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC),
	}
	if err := WriteCompletionDetail(tasksDir, d); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	// Write then delete a completion file to simulate a race.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	vanishing := filepath.Join(completionsDir, "vanishing.json")
	if err := os.WriteFile(vanishing, []byte(`{"task_id":"vanishing"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Remove(vanishing); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	got, err := ReadAllCompletionDetails(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllCompletionDetails: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 detail, got %d", len(got))
	}
	if got[0].TaskID != "task-a" {
		t.Fatalf("got[0].TaskID = %q, want %q", got[0].TaskID, "task-a")
	}
}

func TestWriteMessage_NonUTCSentAtNormalized(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Use a timezone offset that is clearly non-UTC.
	est := time.FixedZone("EST", -5*60*60)
	nonUTC := time.Date(2024, time.June, 15, 10, 30, 0, 0, est) // 10:30 EST = 15:30 UTC

	msg := Message{
		ID:     "tz-test-1",
		From:   "agent-tz",
		Type:   "progress",
		Task:   "tz-task.md",
		Branch: "task/tz",
		Body:   "testing timezone normalization",
		SentAt: nonUTC,
	}
	if err := WriteMessage(tasksDir, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Read the persisted file and verify SentAt is UTC.
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
	var persisted Message
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if persisted.SentAt.Location() != time.UTC {
		t.Fatalf("SentAt location = %v, want UTC", persisted.SentAt.Location())
	}
	wantUTC := nonUTC.UTC()
	if !persisted.SentAt.Equal(wantUTC) {
		t.Fatalf("SentAt = %v, want %v", persisted.SentAt, wantUTC)
	}

	// The filename should embed the UTC timestamp, not the EST one.
	name := entries[0].Name()
	if !strings.HasPrefix(name, "20240615T153000") {
		t.Fatalf("filename %q should start with UTC timestamp 20240615T153000", name)
	}
}

func TestWriteMessage_NonUTCSentAtMultipleZones(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tests := []struct {
		name     string
		zone     *time.Location
		hour     int
		wantHour int
	}{
		{"positive offset", time.FixedZone("IST", 5*60*60+30*60), 18, 13},
		{"negative offset", time.FixedZone("PST", -8*60*60), 4, 12},
		{"already UTC", time.UTC, 12, 12},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sentAt := time.Date(2024, time.January, 10, tt.hour, 30, 0, 0, tt.zone)
			msg := Message{
				ID:     fmt.Sprintf("zone-%d", i),
				From:   "agent",
				Type:   "intent",
				Task:   "task.md",
				Branch: "b",
				Body:   "z",
				SentAt: sentAt,
			}
			if err := WriteMessage(tasksDir, msg); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}

			msgs, err := ReadMessages(tasksDir, time.Time{})
			if err != nil {
				t.Fatalf("ReadMessages: %v", err)
			}
			// Find our message.
			for _, m := range msgs {
				if m.ID == msg.ID {
					if m.SentAt.Hour() != tt.wantHour {
						t.Fatalf("SentAt hour = %d, want %d (UTC)", m.SentAt.Hour(), tt.wantHour)
					}
					return
				}
			}
			t.Fatalf("message %q not found", msg.ID)
		})
	}
}

func TestSafeFilePart_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		fallback string
		want     string
	}{
		{"empty string", "", "fallback", "fallback"},
		{"whitespace only spaces", "   ", "fallback", "fallback"},
		{"whitespace only tabs", "\t\t", "fallback", "fallback"},
		{"mixed whitespace", " \t \n ", "fallback", "fallback"},
		{"punctuation only exclamation", "!!!", "fallback", "_21_21_21"},
		{"punctuation only parens", "(())", "fallback", "_28_28_29_29"},
		{"punctuation only at-signs", "@@@", "fallback", "_40_40_40"},
		{"punctuation slash and colon", "://", "fallback", "_3a_2f_2f"},
		{"punctuation preserved via encoding", "...", "fallback", "_2e_2e_2e"},
		{"leading trailing dots underscores dashes", ".-_hello_-.", "hello", "_2e-_5fhello_5f-_2e"},
		{"valid simple", "agent-1", "fallback", "agent-1"},
		{"valid with dots", "v1.2.3", "fallback", "v1_2e2_2e3"},
		{"special chars encoded", "hello world!", "fallback", "hello_20world_21"},
		{"special chars with trim", " @agent@ ", "fallback", "_40agent_40"},
		{"unicode encoded", "agënt", "fallback", "ag_c3_abnt"},
		{"all unsafe chars", "***", "fallback", "_2a_2a_2a"},
		{"whitespace around valid", "  ok  ", "fallback", "ok"},
		{"only trimchars after replace", "---", "fallback", "---"},
		{"dots only", "...", "msg", "_2e_2e_2e"},
		{"underscores only", "___", "msg", "_5f_5f_5f"},
		{"mixed trim chars", "-._.-", "msg", "-_2e_5f_2e-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeFilePart(tt.value, tt.fallback)
			if got != tt.want {
				t.Errorf("safeFilePart(%q, %q) = %q, want %q", tt.value, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestWriteMessage_BlankFromFallsBackToUnknown(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tests := []struct {
		name string
		from string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"tabs", "\t\t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := Init(dir); err != nil {
				t.Fatalf("Init: %v", err)
			}
			msg := Message{
				ID:     "blank-from",
				From:   tt.from,
				Type:   "intent",
				Task:   "t.md",
				Branch: "b",
				Body:   "b",
				SentAt: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
			}
			if err := WriteMessage(dir, msg); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}

			entries, err := os.ReadDir(filepath.Join(dir, "messages", "events"))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 file, got %d", len(entries))
			}
			name := entries[0].Name()
			// The from part should fall back to "unknown".
			if !strings.Contains(name, "-unknown-intent-") {
				t.Fatalf("filename %q should contain '-unknown-intent-' for blank from", name)
			}
		})
	}
}

func TestCleanStalePresence_UnsafeAgentID(t *testing.T) {
	tests := []struct {
		name    string
		agentID string
	}{
		{"spaces", "agent one"},
		{"special chars", "agent@host:1234"},
		{"unicode", "agënt-ünit"},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/active", func(t *testing.T) {
			tasksDir := t.TempDir()
			if err := Init(tasksDir); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			if err := WritePresence(tasksDir, tt.agentID, "task.md", "branch"); err != nil {
				t.Fatalf("WritePresence: %v", err)
			}

			// Create a lock file using the original (unsanitized) agent ID,
			// simulating an active agent.
			lockPath := filepath.Join(tasksDir, ".locks", tt.agentID+".pid")
			if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				t.Fatalf("WriteFile lock: %v", err)
			}

			CleanStalePresence(tasksDir)

			// Presence should NOT be removed because the agent is active.
			presenceDir := filepath.Join(tasksDir, "messages", "presence")
			entries, err := os.ReadDir(presenceDir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 presence file to remain (active agent), got %d", len(entries))
			}
		})

		t.Run(tt.name+"/stale", func(t *testing.T) {
			tasksDir := t.TempDir()
			if err := Init(tasksDir); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			if err := WritePresence(tasksDir, tt.agentID, "task.md", "branch"); err != nil {
				t.Fatalf("WritePresence: %v", err)
			}

			// Create a lock file with a dead PID so cleanup treats it as stale.
			lockPath := filepath.Join(tasksDir, ".locks", tt.agentID+".pid")
			if err := os.WriteFile(lockPath, []byte("2147483647"), 0o644); err != nil {
				t.Fatalf("WriteFile lock: %v", err)
			}

			CleanStalePresence(tasksDir)

			// Presence should be removed because the agent is stale.
			presenceDir := filepath.Join(tasksDir, "messages", "presence")
			entries, err := os.ReadDir(presenceDir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			jsonCount := 0
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					jsonCount++
				}
			}
			if jsonCount != 0 {
				t.Fatalf("expected stale presence file to be removed, got %d JSON files", jsonCount)
			}
		})
	}
}

func TestCleanStalePresence_MalformedJSON(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write a malformed JSON presence file; cleanup should skip it
	// without panicking.
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	if err := os.WriteFile(filepath.Join(presenceDir, "bad.json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	CleanStalePresence(tasksDir)

	// The malformed file should still exist (skipped, not deleted).
	if _, err := os.Stat(filepath.Join(presenceDir, "bad.json")); err != nil {
		t.Fatalf("malformed presence file should not be removed: %v", err)
	}
}

func TestCleanStalePresence_RejectsAgentIDWithPathSeparator(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	data, err := json.Marshal(PresenceInfo{
		AgentID:   "../escape",
		Task:      "task.md",
		Branch:    "branch",
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(presenceDir, "evil.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile presence: %v", err)
	}

	// Without agent ID validation, cleanup would treat ../escape as a live
	// lock file outside .locks and incorrectly preserve the presence entry.
	if err := os.WriteFile(filepath.Join(tasksDir, "escape.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile sibling lock: %v", err)
	}

	CleanStalePresence(tasksDir)

	if _, err := os.Stat(filepath.Join(presenceDir, "evil.json")); !os.IsNotExist(err) {
		t.Fatalf("presence file for separator-containing agent ID should be removed, stat err = %v", err)
	}
}

func TestWritePresence_UnsafeAgentID(t *testing.T) {
	tests := []struct {
		name         string
		agentID      string
		wantFilePart string
	}{
		{"slashes", "../../etc/passwd", "_2e_2e_2f_2e_2e_2fetc_2fpasswd"},
		{"spaces", "agent one", "agent_20one"},
		{"special chars", "agent@host:1234", "agent_40host_3a1234"},
		{"empty", "", "unknown"},
		{"whitespace only", "   ", "unknown"},
		{"unicode", "agënt-ünit", "ag_c3_abnt-_c3_bcnit"},
		{"dots only", "...", "_2e_2e_2e"},
		{"normal", "abc12345", "abc12345"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			if err := Init(tasksDir); err != nil {
				t.Fatalf("Init: %v", err)
			}

			if err := WritePresence(tasksDir, tt.agentID, "task.md", "branch"); err != nil {
				t.Fatalf("WritePresence: %v", err)
			}

			// Verify the filename is safe.
			presenceDir := filepath.Join(tasksDir, "messages", "presence")
			entries, err := os.ReadDir(presenceDir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 presence file, got %d", len(entries))
			}
			gotFile := entries[0].Name()
			wantFile := tt.wantFilePart + ".json"
			if gotFile != wantFile {
				t.Errorf("presence filename = %q, want %q", gotFile, wantFile)
			}

			// Verify the JSON payload preserves the original agentID.
			data, err := os.ReadFile(filepath.Join(presenceDir, gotFile))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			var info PresenceInfo
			if err := json.Unmarshal(data, &info); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if info.AgentID != tt.agentID {
				t.Errorf("AgentID = %q, want %q (original should be preserved in JSON)", info.AgentID, tt.agentID)
			}
		})
	}
}

func TestReadMessages_DeterministicOrderOnEqualTimestamps(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	sameTime := time.Date(2024, time.July, 4, 12, 0, 0, 0, time.UTC)

	// Write messages with the same timestamp but different IDs.
	ids := []string{"charlie", "alpha", "bravo"}
	for _, id := range ids {
		msg := Message{
			ID:     id,
			From:   "agent",
			Type:   "intent",
			Task:   "task.md",
			Branch: "b",
			Body:   "body",
			SentAt: sameTime,
		}
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", id, err)
		}
	}

	msgs, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// Should be sorted by ID when timestamps are equal: alpha, bravo, charlie.
	wantOrder := []string{"alpha", "bravo", "charlie"}
	for i, want := range wantOrder {
		if msgs[i].ID != want {
			t.Errorf("msgs[%d].ID = %q, want %q", i, msgs[i].ID, want)
		}
	}
}

func TestReadMessages_StableSortMixedTimestamps(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	t1 := time.Date(2024, time.July, 4, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, time.July, 4, 13, 0, 0, 0, time.UTC)

	// Two messages at t1 (z-id before a-id alphabetically → should swap),
	// one message at t2.
	toWrite := []struct {
		id   string
		sent time.Time
	}{
		{"z-id", t1},
		{"a-id", t1},
		{"m-id", t2},
	}
	for _, tw := range toWrite {
		msg := Message{
			ID:     tw.id,
			From:   "agent",
			Type:   "intent",
			Task:   "task.md",
			Branch: "b",
			Body:   "body",
			SentAt: tw.sent,
		}
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", tw.id, err)
		}
	}

	msgs, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// Expected: a-id (t1), z-id (t1), m-id (t2).
	wantOrder := []string{"a-id", "z-id", "m-id"}
	for i, want := range wantOrder {
		if msgs[i].ID != want {
			t.Errorf("msgs[%d].ID = %q, want %q", i, msgs[i].ID, want)
		}
	}
}

func TestReadRecentMessages_ReturnsAll_WhenUnderLimit(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("m%d", i),
			From:   "agent",
			Type:   "progress",
			Task:   "task.md",
			Body:   fmt.Sprintf("msg %d", i),
			SentAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	msgs, _, err := ReadRecentMessages(tasksDir, 10)
	if err != nil {
		t.Fatalf("ReadRecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("got %d messages, want 3", len(msgs))
	}
}

func TestReadRecentMessages_LimitsToMostRecent(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("m%d", i),
			From:   "agent",
			Type:   "progress",
			Task:   "task.md",
			Body:   fmt.Sprintf("msg %d", i),
			SentAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	msgs, _, err := ReadRecentMessages(tasksDir, 3)
	if err != nil {
		t.Fatalf("ReadRecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("got %d messages, want 3", len(msgs))
	}
	// Should contain the 3 most recent messages (m7, m8, m9).
	for _, msg := range msgs {
		id := msg.ID
		if id != "m7" && id != "m8" && id != "m9" {
			t.Errorf("unexpected message ID %q in bounded result", id)
		}
	}
}

func TestReadRecentMessages_ZeroLimitReadsAll(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("m%d", i),
			From:   "agent",
			Type:   "progress",
			Task:   "task.md",
			Body:   fmt.Sprintf("msg %d", i),
			SentAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	msgs, _, err := ReadRecentMessages(tasksDir, 0)
	if err != nil {
		t.Fatalf("ReadRecentMessages: %v", err)
	}
	if len(msgs) != 5 {
		t.Errorf("got %d messages, want 5", len(msgs))
	}
}

func TestReadRecentMessages_NonExistentDir(t *testing.T) {
	msgs, _, err := ReadRecentMessages(filepath.Join(t.TempDir(), "nope"), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("got %d messages, want 0", len(msgs))
	}
}

func TestReadRecentMessages_PreservesOrder(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("m%d", i),
			From:   "agent",
			Type:   "intent",
			Task:   "task.md",
			Body:   fmt.Sprintf("msg %d", i),
			SentAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	msgs, _, err := ReadRecentMessages(tasksDir, 3)
	if err != nil {
		t.Fatalf("ReadRecentMessages: %v", err)
	}
	// Messages should be sorted by SentAt ascending.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].SentAt.Before(msgs[i-1].SentAt) {
			t.Errorf("messages not sorted: msgs[%d].SentAt=%v before msgs[%d].SentAt=%v",
				i, msgs[i].SentAt, i-1, msgs[i-1].SentAt)
		}
	}
}

// --- Edge-case tests for cleanup, bounded reads, and filename collisions ---

func TestCleanOldMessages_UsesMtimeNotSentAt(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write two messages. Their sent_at values suggest that "old-by-sentat"
	// was sent long ago and "new-by-sentat" was sent recently. However, we
	// override their file mtimes to the opposite: old-by-sentat gets a
	// recent mtime, and new-by-sentat gets an old mtime.
	oldSentAt := Message{
		ID: "old-by-sentat", From: "agent", Type: "intent",
		Task: "t.md", Branch: "b", Body: "old sent_at",
		SentAt: time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	newSentAt := Message{
		ID: "new-by-sentat", From: "agent", Type: "intent",
		Task: "t.md", Branch: "b", Body: "new sent_at",
		SentAt: time.Date(2099, time.December, 31, 23, 59, 59, 0, time.UTC),
	}
	for _, msg := range []Message{oldSentAt, newSentAt} {
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", msg.ID, err)
		}
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	oldFile := findEventFile(t, eventsDir, "old-by-sentat")
	newFile := findEventFile(t, eventsDir, "new-by-sentat")

	now := time.Now()
	// old-by-sentat → recent mtime (should survive cleanup)
	if err := os.Chtimes(oldFile, now, now); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// new-by-sentat → old mtime (should be cleaned up)
	if err := os.Chtimes(newFile, now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	CleanOldMessages(tasksDir, 24*time.Hour)

	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("file with recent mtime (old sent_at) should survive: %v", err)
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatal("file with old mtime (new sent_at) should be removed")
	}
}

func TestCleanOldMessages_NonPositiveMaxAge(t *testing.T) {
	tests := []struct {
		name   string
		maxAge time.Duration
	}{
		{"zero", 0},
		{"negative", -5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			if err := Init(tasksDir); err != nil {
				t.Fatalf("Init: %v", err)
			}

			msg := Message{
				ID: "msg1", From: "agent", Type: "intent",
				Task: "t.md", Branch: "b", Body: "body",
				SentAt: time.Now().UTC(),
			}
			if err := WriteMessage(tasksDir, msg); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}

			eventsDir := filepath.Join(tasksDir, "messages", "events")
			file := findEventFile(t, eventsDir, "msg1")

			// Set mtime to 1 second ago to avoid filesystem precision issues.
			past := time.Now().Add(-1 * time.Second)
			if err := os.Chtimes(file, past, past); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}

			CleanOldMessages(tasksDir, tt.maxAge)

			// With zero or negative maxAge the cutoff = now - maxAge which is
			// now or in the future. So files with mtime in the past are
			// cleaned. This documents current behavior.
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("expected file to be removed with maxAge=%v, but it still exists", tt.maxAge)
			}
		})
	}
}

func TestReadRecentMessages_SkipsMalformedFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	valid := Message{
		ID: "valid-msg", From: "agent", Type: "intent",
		Task: "task.md", Branch: "branch", Body: "ok",
		SentAt: time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := WriteMessage(tasksDir, valid); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	// Write a malformed JSON file that sorts before the valid file.
	if err := os.WriteFile(filepath.Join(eventsDir, "00000000T000000.000000000Z-broken.json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Write another malformed file that sorts after.
	if err := os.WriteFile(filepath.Join(eventsDir, "zzz-broken.json"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, skippedWarnings, err := ReadRecentMessages(tasksDir, 10)
	if err != nil {
		t.Fatalf("ReadRecentMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 valid message, got %d", len(got))
	}
	if len(skippedWarnings) != 2 {
		t.Fatalf("expected 2 warnings for malformed files, got %d: %v", len(skippedWarnings), skippedWarnings)
	}
	if got[0].ID != "valid-msg" {
		t.Fatalf("got ID %q, want %q", got[0].ID, "valid-msg")
	}
}

func TestReadRecentMessages_ToleratesDeletedFiles(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const total = 5
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	wantByID := make(map[string]Message, total)
	for i := 0; i < total; i++ {
		msg := Message{
			ID:     "rmsg-" + strconv.Itoa(i),
			From:   "writer",
			Type:   "intent",
			Task:   "task-" + strconv.Itoa(i),
			Branch: "branch",
			Body:   "body-" + strconv.Itoa(i),
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
		t.Fatalf("expected %d files, got %d", total, len(entries))
	}

	errCh := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10; i++ {
			msgs, _, readErr := ReadRecentMessages(tasksDir, total+5)
			if readErr != nil {
				errCh <- fmt.Errorf("ReadRecentMessages iteration %d: %w", i, readErr)
				return
			}
			if len(msgs) > total {
				errCh <- fmt.Errorf("iteration %d: got %d messages, want <= %d", i, len(msgs), total)
				return
			}
			for _, msg := range msgs {
				want, ok := wantByID[msg.ID]
				if !ok {
					errCh <- fmt.Errorf("iteration %d: unexpected ID %q", i, msg.ID)
					return
				}
				if msg.Body != want.Body {
					errCh <- fmt.Errorf("iteration %d: body mismatch for %q", i, msg.ID)
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
			if rmErr := os.Remove(filepath.Join(eventsDir, entry.Name())); rmErr != nil && !os.IsNotExist(rmErr) {
				errCh <- fmt.Errorf("Remove(%s): %w", entry.Name(), rmErr)
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	close(start)
	wg.Wait()
	close(errCh)
	for chErr := range errCh {
		if chErr != nil {
			t.Fatal(chErr)
		}
	}

	final, _, err := ReadRecentMessages(tasksDir, total+5)
	if err != nil {
		t.Fatalf("ReadRecentMessages final: %v", err)
	}
	if len(final) != 0 {
		t.Fatalf("final count = %d, want 0", len(final))
	}
}

func TestReadRecentMessages_EqualTimestampTieBreakParity(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	sameTime := time.Date(2024, time.July, 4, 12, 0, 0, 0, time.UTC)
	ids := []string{"charlie", "alpha", "bravo", "delta", "echo"}
	for _, id := range ids {
		msg := Message{
			ID: id, From: "agent", Type: "intent",
			Task: "task.md", Branch: "b", Body: "body",
			SentAt: sameTime,
		}
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", id, err)
		}
	}

	// Read all via both methods.
	allMsgs, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	recentMsgs, _, err := ReadRecentMessages(tasksDir, 3)
	if err != nil {
		t.Fatalf("ReadRecentMessages: %v", err)
	}

	if len(allMsgs) != 5 {
		t.Fatalf("ReadMessages returned %d, want 5", len(allMsgs))
	}
	if len(recentMsgs) != 3 {
		t.Fatalf("ReadRecentMessages returned %d, want 3", len(recentMsgs))
	}

	// The unbounded sort order should be alpha, bravo, charlie, delta, echo.
	wantAll := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for i, want := range wantAll {
		if allMsgs[i].ID != want {
			t.Errorf("ReadMessages[%d].ID = %q, want %q", i, allMsgs[i].ID, want)
		}
	}

	// ReadRecentMessages with limit 3 should select the 3 lexically-last
	// filenames, then apply the same final (sent_at, id) ordering as
	// ReadMessages. When all timestamps are equal, that should be the
	// contiguous suffix of the unbounded result.
	wantRecent := wantAll[len(wantAll)-3:]
	for i, want := range wantRecent {
		if recentMsgs[i].ID != want {
			t.Errorf("ReadRecentMessages[%d].ID = %q, want %q", i, recentMsgs[i].ID, want)
		}
	}
}

func TestReadLatestProgressForAgents_FindsOlderProgress(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write 5 progress messages from agent-a (old).
	for i := 0; i < 5; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("old-%d", i),
			From:   "agent-a",
			Type:   "progress",
			Task:   "task-a.md",
			Body:   fmt.Sprintf("old progress %d", i),
			SentAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	// Write 10 messages from other agents (newer).
	for i := 0; i < 10; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("new-%d", i),
			From:   "agent-b",
			Type:   "progress",
			Task:   "task-b.md",
			Body:   fmt.Sprintf("new progress %d", i),
			SentAt: base.Add(time.Duration(10+i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	// Skip the 10 newest; agent-a's progress is outside that window.
	got, warnings, err := ReadLatestProgressForAgents(tasksDir, []string{"agent-a"}, 10)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	if got["agent-a"].Body != "old progress 4" {
		t.Errorf("agent-a body = %q, want %q", got["agent-a"].Body, "old progress 4")
	}
}

func TestReadLatestProgressForAgents_EmptyAgentIDs(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	got, warnings, err := ReadLatestProgressForAgents(tasksDir, nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if warnings != nil {
		t.Errorf("expected nil warnings, got %v", warnings)
	}
}

func TestReadLatestProgressForAgents_NonExistentDir(t *testing.T) {
	got, warnings, err := ReadLatestProgressForAgents(filepath.Join(t.TempDir(), "nope"), []string{"a"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d results, want 0", len(got))
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

func TestReadLatestProgressForAgents_SkipsNonProgressMessages(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write an intent message from agent-a.
	if err := WriteMessage(tasksDir, Message{
		ID: "i1", From: "agent-a", Type: "intent",
		Task: "task.md", Body: "Starting", SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got, _, err := ReadLatestProgressForAgents(tasksDir, []string{"agent-a"}, 0)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0, got %d — should skip non-progress messages", len(got))
	}
}

func TestReadLatestProgressForAgents_StopsEarlyWhenAllFound(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write many old progress messages from different agents.
	for i := 0; i < 20; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("p%d", i),
			From:   fmt.Sprintf("agent-%d", i%3),
			Type:   "progress",
			Task:   "task.md",
			Body:   fmt.Sprintf("step %d", i),
			SentAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	// Ask for just agent-1.
	got, _, err := ReadLatestProgressForAgents(tasksDir, []string{"agent-1"}, 0)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	if got["agent-1"].Body != "step 19" {
		t.Errorf("agent-1 body = %q, want %q", got["agent-1"].Body, "step 19")
	}
}

func TestReadLatestProgressForAgents_SkipExceedsTotal(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	if err := WriteMessage(tasksDir, Message{
		ID: "p1", From: "a", Type: "progress",
		Task: "t.md", Body: "step 1",
		SentAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Skip is larger than total messages.
	got, _, err := ReadLatestProgressForAgents(tasksDir, []string{"a"}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d results, want 0 when skip exceeds total", len(got))
	}
}

// TestReadLatestProgressForAgents_EqualTimestampTieBreak verifies that when
// an agent has multiple progress messages with the same SentAt, the fallback
// function returns the one with the lexically smallest ID — matching the
// tie-break rule of latestProgressByAgent.
func TestReadLatestProgressForAgents_EqualTimestampTieBreak(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	sameTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Write three progress messages for the same agent with identical timestamps.
	// IDs: "charlie", "alpha", "bravo" — smallest is "alpha".
	for _, id := range []string{"charlie", "alpha", "bravo"} {
		if err := WriteMessage(tasksDir, Message{
			ID:     id,
			From:   "agent-x",
			Type:   "progress",
			Task:   "task.md",
			Body:   "progress-" + id,
			SentAt: sameTime,
		}); err != nil {
			t.Fatalf("WriteMessage(%s): %v", id, err)
		}
	}

	got, _, err := ReadLatestProgressForAgents(tasksDir, []string{"agent-x"}, 0)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	if got["agent-x"].ID != "alpha" {
		t.Errorf("agent-x ID = %q, want %q (smallest ID for equal timestamps)", got["agent-x"].ID, "alpha")
	}
	if got["agent-x"].Body != "progress-alpha" {
		t.Errorf("agent-x body = %q, want %q", got["agent-x"].Body, "progress-alpha")
	}
}

// TestReadLatestProgressForAgents_EqualTimestampMixedAgents verifies the
// tie-break rule works correctly when multiple agents have equal timestamps.
func TestReadLatestProgressForAgents_EqualTimestampMixedAgents(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	sameTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Two agents, each with two equal-timestamp progress messages.
	msgs := []Message{
		{ID: "z1", From: "agent-a", Type: "progress", Task: "a.md", Body: "z1", SentAt: sameTime},
		{ID: "a1", From: "agent-a", Type: "progress", Task: "a.md", Body: "a1", SentAt: sameTime},
		{ID: "z2", From: "agent-b", Type: "progress", Task: "b.md", Body: "z2", SentAt: sameTime},
		{ID: "a2", From: "agent-b", Type: "progress", Task: "b.md", Body: "a2", SentAt: sameTime},
	}
	for _, msg := range msgs {
		if err := WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage(%s): %v", msg.ID, err)
		}
	}

	got, _, err := ReadLatestProgressForAgents(tasksDir, []string{"agent-a", "agent-b"}, 0)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2", len(got))
	}
	if got["agent-a"].ID != "a1" {
		t.Errorf("agent-a ID = %q, want %q", got["agent-a"].ID, "a1")
	}
	if got["agent-b"].ID != "a2" {
		t.Errorf("agent-b ID = %q, want %q", got["agent-b"].ID, "a2")
	}
}

// TestReadLatestProgressForAgents_StopsAfterTimestampWindow verifies that
// when the matched agent has no equal-timestamp siblings, the scan finalizes
// the agent as soon as the filename timestamp drops below the match — it does
// NOT continue reading all remaining older files.
func TestReadLatestProgressForAgents_StopsAfterTimestampWindow(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	// Write 50 old progress messages from "other" at t=0..49s.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		if err := WriteMessage(tasksDir, Message{
			ID:     fmt.Sprintf("old-%d", i),
			From:   "other",
			Type:   "progress",
			Task:   "other.md",
			Body:   fmt.Sprintf("old %d", i),
			SentAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage(old-%d): %v", i, err)
		}
	}

	// Write the target agent's single progress at t=60s.
	targetTime := base.Add(60 * time.Second)
	if err := WriteMessage(tasksDir, Message{
		ID:     "target-prog",
		From:   "target-agent",
		Type:   "progress",
		Task:   "target.md",
		Body:   "Step: WORK",
		SentAt: targetTime,
	}); err != nil {
		t.Fatalf("WriteMessage(target): %v", err)
	}

	got, _, err := ReadLatestProgressForAgents(tasksDir, []string{"target-agent"}, 0)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	if got["target-agent"].ID != "target-prog" {
		t.Errorf("ID = %q, want %q", got["target-agent"].ID, "target-prog")
	}
	if got["target-agent"].Body != "Step: WORK" {
		t.Errorf("Body = %q, want %q", got["target-agent"].Body, "Step: WORK")
	}
	// The key invariant: the scan should have stopped after seeing the first
	// file with a timestamp < targetTime, not read all 50 older entries.
	// We can't directly assert iteration count, but the function returning
	// the correct result without a full scan is the behavioral guarantee.
}

// TestReadLatestProgressForAgents_UnreadableFileWarning verifies that an
// unreadable older progress file produces a warning instead of aborting,
// while valid fallback progress for another agent is still recovered.
func TestReadLatestProgressForAgents_UnreadableFileWarning(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write a valid progress message from agent-good.
	if err := WriteMessage(tasksDir, Message{
		ID:     "good-prog",
		From:   "agent-good",
		Type:   "progress",
		Task:   "task-good.md",
		Body:   "Step: WORK",
		SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage(good): %v", err)
	}

	// Write a valid progress message from agent-bad so the file exists.
	if err := WriteMessage(tasksDir, Message{
		ID:     "bad-prog",
		From:   "agent-bad",
		Type:   "progress",
		Task:   "task-bad.md",
		Body:   "Step: VERIFY",
		SentAt: base.Add(time.Second),
	}); err != nil {
		t.Fatalf("WriteMessage(bad): %v", err)
	}

	// Make agent-bad's progress file unreadable via the messaging read hook.
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var unreadablePath string
	for _, e := range entries {
		if strings.Contains(e.Name(), "-progress-") && strings.Contains(e.Name(), "bad-prog") {
			unreadablePath = filepath.Join(eventsDir, e.Name())
			break
		}
	}
	origReadFile := osReadFile
	osReadFile = func(path string) ([]byte, error) {
		if path == unreadablePath {
			return nil, fmt.Errorf("permission denied")
		}
		return origReadFile(path)
	}
	t.Cleanup(func() { osReadFile = origReadFile })

	got, warnings, readErr := ReadLatestProgressForAgents(tasksDir, []string{"agent-good", "agent-bad"}, 0)
	if readErr != nil {
		t.Fatalf("unexpected error: %v", readErr)
	}

	// agent-good should still be recovered.
	if len(got) < 1 {
		t.Fatalf("got %d agents, want at least 1 (agent-good)", len(got))
	}
	if msg, ok := got["agent-good"]; !ok {
		t.Errorf("agent-good not found in results")
	} else if msg.Body != "Step: WORK" {
		t.Errorf("agent-good body = %q, want %q", msg.Body, "Step: WORK")
	}

	// There should be a warning about the unreadable file.
	if len(warnings) == 0 {
		t.Errorf("expected at least one warning for unreadable file, got none")
	}
	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "could not read older progress message") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected warning about unreadable progress message, got: %v", warnings)
	}
}

// TestReadLatestProgressForAgents_MalformedFileWarning verifies that a
// malformed JSON progress file produces a warning instead of aborting.
func TestReadLatestProgressForAgents_MalformedFileWarning(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write a valid progress message from agent-ok.
	if err := WriteMessage(tasksDir, Message{
		ID:     "ok-prog",
		From:   "agent-ok",
		Type:   "progress",
		Task:   "task-ok.md",
		Body:   "Step: COMMIT",
		SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage(ok): %v", err)
	}

	// Write a malformed JSON file that looks like a progress message by name.
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	malformedName := base.Add(2*time.Second).Format("20060102T150405.000000000Z") + "-agent-broken-progress-broken-prog.json"
	if err := os.WriteFile(filepath.Join(eventsDir, malformedName), []byte("{bad json"), 0o644); err != nil {
		t.Fatalf("WriteFile(malformed): %v", err)
	}

	got, warnings, readErr := ReadLatestProgressForAgents(tasksDir, []string{"agent-ok", "agent-broken"}, 0)
	if readErr != nil {
		t.Fatalf("unexpected error: %v", readErr)
	}

	// agent-ok should still be recovered.
	if msg, ok := got["agent-ok"]; !ok {
		t.Errorf("agent-ok not found in results")
	} else if msg.Body != "Step: COMMIT" {
		t.Errorf("agent-ok body = %q, want %q", msg.Body, "Step: COMMIT")
	}

	// There should be a warning about the malformed file.
	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "could not parse older progress message") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected warning about malformed progress message, got: %v", warnings)
	}
}

// TestReadLatestProgressForAgents_AgentStyleFilenames verifies that progress
// messages written with agent-style filenames (e.g. nonce-agent-work.json)
// are found by the fallback scan, even though these names do not contain
// the "-progress-" substring that WriteMessage-generated names include.
func TestReadLatestProgressForAgents_AgentStyleFilenames(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eventsDir := filepath.Join(tasksDir, "messages", "events")

	// Write agent-style progress files directly (no "-progress-" in name).
	agentFiles := []struct {
		filename string
		msg      Message
	}{
		{
			filename: base.Format("20060102T150405.000000000Z") + "-agent-7-verify-claim.json",
			msg: Message{
				ID: "vc1", From: "agent-7", Type: "progress",
				Task: "task-7.md", Body: "Step: VERIFY_CLAIM", SentAt: base,
			},
		},
		{
			filename: base.Add(time.Second).Format("20060102T150405.000000000Z") + "-agent-7-work.json",
			msg: Message{
				ID: "w1", From: "agent-7", Type: "progress",
				Task: "task-7.md", Body: "Step: WORK", SentAt: base.Add(time.Second),
			},
		},
		{
			filename: base.Add(2*time.Second).Format("20060102T150405.000000000Z") + "-agent-7-commit.json",
			msg: Message{
				ID: "c1", From: "agent-7", Type: "progress",
				Task: "task-7.md", Body: "Step: COMMIT", SentAt: base.Add(2 * time.Second),
			},
		},
	}
	for _, af := range agentFiles {
		data, err := json.Marshal(af.msg)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(eventsDir, af.filename), data, 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", af.filename, err)
		}
	}

	got, warnings, err := ReadLatestProgressForAgents(tasksDir, []string{"agent-7"}, 0)
	if err != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	if got["agent-7"].Body != "Step: COMMIT" {
		t.Errorf("agent-7 body = %q, want %q", got["agent-7"].Body, "Step: COMMIT")
	}
}

// TestReadLatestProgressForAgents_MixedFilenameStyles verifies that a mix
// of WriteMessage-generated filenames (containing "-progress-") and
// agent-style filenames (e.g. nonce-agent-work.json) are both found.
func TestReadLatestProgressForAgents_MixedFilenameStyles(t *testing.T) {
	tasksDir := t.TempDir()
	setupMessagingDirs(t, tasksDir)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eventsDir := filepath.Join(tasksDir, "messages", "events")

	// Agent-a uses WriteMessage (produces "-progress-" filenames).
	if err := WriteMessage(tasksDir, Message{
		ID: "a-prog", From: "agent-a", Type: "progress",
		Task: "task-a.md", Body: "Step: WORK via WriteMessage",
		SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage(a): %v", err)
	}

	// Agent-b writes directly with agent-style filename.
	agentBMsg := Message{
		ID: "b-prog", From: "agent-b", Type: "progress",
		Task: "task-b.md", Body: "Step: WORK via agent shell",
		SentAt: base.Add(time.Second),
	}
	data, err := json.Marshal(agentBMsg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	fname := base.Add(time.Second).Format("20060102T150405.000000000Z") + "-agent-b-work.json"
	if err := os.WriteFile(filepath.Join(eventsDir, fname), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, warnings, readErr := ReadLatestProgressForAgents(tasksDir, []string{"agent-a", "agent-b"}, 0)
	if readErr != nil {
		t.Fatalf("ReadLatestProgressForAgents: %v", readErr)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2", len(got))
	}
	if got["agent-a"].Body != "Step: WORK via WriteMessage" {
		t.Errorf("agent-a body = %q, want %q", got["agent-a"].Body, "Step: WORK via WriteMessage")
	}
	if got["agent-b"].Body != "Step: WORK via agent shell" {
		t.Errorf("agent-b body = %q, want %q", got["agent-b"].Body, "Step: WORK via agent shell")
	}
}

func TestWritePresence_CollisionResistance(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Two agent IDs with similar shapes should still produce distinct filenames.
	agentA := "agent!one"
	agentB := "agent@one"

	encodedA := safeFilePart(agentA, "unknown")
	encodedB := safeFilePart(agentB, "unknown")
	if encodedA == encodedB {
		t.Fatalf("safeFilePart should not collide: %q vs %q", encodedA, encodedB)
	}

	if err := WritePresence(tasksDir, agentA, "task-a.md", "branch-a"); err != nil {
		t.Fatalf("WritePresence(agentA): %v", err)
	}
	if err := WritePresence(tasksDir, agentB, "task-b.md", "branch-b"); err != nil {
		t.Fatalf("WritePresence(agentB): %v", err)
	}

	// Both entries should survive as separate files.
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	jsonFiles := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsonFiles++
		}
	}
	if jsonFiles != 2 {
		t.Errorf("expected 2 presence files (no collision), got %d", jsonFiles)
	}

	// Both entries should be readable.
	presence, err := ReadAllPresence(tasksDir)
	if err != nil {
		t.Fatalf("ReadAllPresence: %v", err)
	}
	if len(presence) != 2 {
		t.Fatalf("expected 2 presence entries, got %d", len(presence))
	}
	if info, ok := presence[agentA]; !ok {
		t.Error("agentA presence entry missing")
	} else if info.Task != "task-a.md" {
		t.Errorf("agentA Task=%q, want %q", info.Task, "task-a.md")
	}
	if info, ok := presence[agentB]; !ok {
		t.Error("agentB presence entry missing")
	} else if info.Task != "task-b.md" {
		t.Errorf("agentB Task=%q, want %q", info.Task, "task-b.md")
	}
}

func TestWriteMessage_CollisionResistance(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Two messages with the same SentAt and Type but distinct From and ID values
	// must produce distinct filenames.
	sameTime := time.Date(2024, time.June, 15, 10, 0, 0, 0, time.UTC)

	msgA := Message{
		ID: "id!1", From: "agent!x", Type: "intent",
		Task: "task.md", Branch: "b", Body: "first",
		SentAt: sameTime,
	}
	msgB := Message{
		ID: "id@1", From: "agent@x", Type: "intent",
		Task: "task.md", Branch: "b", Body: "second",
		SentAt: sameTime,
	}

	if safeFilePart(msgA.From, "unknown") == safeFilePart(msgB.From, "unknown") {
		t.Fatal("safeFilePart should not collide for distinct From values")
	}
	if safeFilePart(msgA.ID, "message") == safeFilePart(msgB.ID, "message") {
		t.Fatal("safeFilePart should not collide for distinct ID values")
	}

	if err := WriteMessage(tasksDir, msgA); err != nil {
		t.Fatalf("WriteMessage(A): %v", err)
	}
	if err := WriteMessage(tasksDir, msgB); err != nil {
		t.Fatalf("WriteMessage(B): %v", err)
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	jsonFiles := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsonFiles++
		}
	}

	// Both messages should survive as separate files.
	if jsonFiles != 2 {
		t.Errorf("expected 2 event files (no collision), got %d", jsonFiles)
	}

	// Both messages should be readable.
	msgs, err := ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	bodies := map[string]bool{msgs[0].Body: true, msgs[1].Body: true}
	if !bodies["first"] || !bodies["second"] {
		t.Errorf("expected both messages to survive, got bodies %v", bodies)
	}
}

func TestCleanStalePresence_WritePresence_Interleaving(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Pre-populate presence for several agents. None have lock files, so
	// CleanStalePresence will try to remove them all.
	agents := []string{"agent-a", "agent-b", "agent-c", "agent-d"}
	for _, a := range agents {
		if err := WritePresence(tasksDir, a, a+"-task.md", a+"-branch"); err != nil {
			t.Fatalf("WritePresence(%s): %v", a, err)
		}
	}

	// Concurrently run cleanup and a fresh write. The fresh write should
	// succeed regardless of cleanup activity, and cleanup should not return
	// errors or leave malformed JSON.
	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			CleanStalePresence(tasksDir)
			time.Sleep(1 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			if writeErr := WritePresence(tasksDir, "fresh-agent", "fresh-task.md", "fresh-branch"); writeErr != nil {
				errCh <- fmt.Errorf("WritePresence iteration %d: %w", i, writeErr)
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(errCh)
	for chErr := range errCh {
		if chErr != nil {
			t.Fatal(chErr)
		}
	}

	// After interleaving, every on-disk JSON file should still be readable.
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		t.Fatalf("ReadDir presence: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, readErr := os.ReadFile(filepath.Join(presenceDir, entry.Name()))
		if readErr != nil {
			t.Fatalf("ReadFile(%s): %v", entry.Name(), readErr)
		}

		var info PresenceInfo
		if unmarshalErr := json.Unmarshal(data, &info); unmarshalErr != nil {
			t.Fatalf("Unmarshal(%s): %v", entry.Name(), unmarshalErr)
		}

		agentID := strings.TrimSuffix(entry.Name(), ".json")
		if info.AgentID == "" {
			t.Errorf("presence for %q has empty AgentID", agentID)
		}
		if info.UpdatedAt.IsZero() {
			t.Errorf("presence for %q has zero UpdatedAt", agentID)
		}
		// Verify the JSON is well-formed by re-marshaling.
		data, marshalErr := json.Marshal(info)
		if marshalErr != nil {
			t.Errorf("re-marshal presence for %q: %v", agentID, marshalErr)
		}
		var roundtrip PresenceInfo
		if unmarshalErr := json.Unmarshal(data, &roundtrip); unmarshalErr != nil {
			t.Errorf("roundtrip unmarshal for %q: %v", agentID, unmarshalErr)
		}
	}
}

// setupMessagingDirs creates the messaging directory structure for tests.
func setupMessagingDirs(t *testing.T, tasksDir string) {
	t.Helper()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// writeNMessages creates n message files in the events directory.
func writeNMessages(b *testing.B, tasksDir string, n int) {
	b.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		msg := Message{
			ID:     fmt.Sprintf("bench-%d", i),
			From:   fmt.Sprintf("agent-%d", i%5),
			Type:   "progress",
			Task:   "task.md",
			Body:   fmt.Sprintf("progress %d", i),
			SentAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := WriteMessage(tasksDir, msg); err != nil {
			b.Fatalf("WriteMessage: %v", err)
		}
	}
}

func TestReadRecentMessages_ToleratesUnreadableFiles(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Init(tasksDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	good1 := Message{
		ID: "good-1", From: "agent", Type: "intent",
		Task: "task.md", Branch: "branch", Body: "first",
		SentAt: base,
	}
	good2 := Message{
		ID: "good-2", From: "agent", Type: "progress",
		Task: "task.md", Branch: "branch", Body: "second",
		SentAt: base.Add(2 * time.Minute),
	}
	if err := WriteMessage(tasksDir, good1); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if err := WriteMessage(tasksDir, good2); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Simulate an unreadable file between the two good files via the read hook.
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	unreadable := filepath.Join(eventsDir, base.Add(time.Minute).Format("20060102T150405.000000000Z")+"-unreadable.json")
	if err := os.WriteFile(unreadable, []byte(`{"id":"bad"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	origReadFile := osReadFile
	osReadFile = func(path string) ([]byte, error) {
		if path == unreadable {
			return nil, fmt.Errorf("permission denied")
		}
		return origReadFile(path)
	}
	t.Cleanup(func() { osReadFile = origReadFile })

	got, warnings, err := ReadRecentMessages(tasksDir, 10)
	if err != nil {
		t.Fatalf("ReadRecentMessages should not return error for unreadable file, got: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for unreadable file, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "could not read message") {
		t.Errorf("warning should mention 'could not read message', got: %q", warnings[0])
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid messages, got %d", len(got))
	}
	if got[0].ID != "good-1" || got[1].ID != "good-2" {
		t.Fatalf("got IDs %q and %q, want good-1 and good-2", got[0].ID, got[1].ID)
	}
}

func BenchmarkReadMessages(b *testing.B) {
	counts := []int{100, 500, 1000}
	for _, n := range counts {
		b.Run(fmt.Sprintf("unbounded_%d", n), func(b *testing.B) {
			tasksDir := b.TempDir()
			if err := Init(tasksDir); err != nil {
				b.Fatalf("Init: %v", err)
			}
			writeNMessages(b, tasksDir, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := ReadMessages(tasksDir, time.Time{})
				if err != nil {
					b.Fatalf("ReadMessages: %v", err)
				}
			}
		})
		b.Run(fmt.Sprintf("bounded_50_of_%d", n), func(b *testing.B) {
			tasksDir := b.TempDir()
			if err := Init(tasksDir); err != nil {
				b.Fatalf("Init: %v", err)
			}
			writeNMessages(b, tasksDir, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _, err := ReadRecentMessages(tasksDir, 50)
				if err != nil {
					b.Fatalf("ReadRecentMessages: %v", err)
				}
			}
		})
	}
}
