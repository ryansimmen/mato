package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/queue"
)

// setupTasksDir creates the standard queue directory structure in a temp dir.
func setupTasksDir(t *testing.T) string {
	t.Helper()
	tasksDir := t.TempDir()
	for _, dir := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.locks): %v", err)
	}
	return tasksDir
}

// writeTask writes a task markdown file to dir/name with the given content.
func writeTask(t *testing.T, tasksDir, dir, name, content string) {
	t.Helper()
	path := filepath.Join(tasksDir, dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestListTasksFromIndex_CountsViaIndex(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  int
	}{
		{
			name:  "empty directory",
			files: nil,
			want:  0,
		},
		{
			name:  "single task",
			files: []string{"task-a.md"},
			want:  1,
		},
		{
			name:  "multiple tasks",
			files: []string{"alpha.md", "beta.md", "gamma.md"},
			want:  3,
		},
		{
			name:  "non-md files ignored",
			files: []string{"readme.txt", "notes.md", "config.yaml"},
			want:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := setupTasksDir(t)
			for _, f := range tt.files {
				os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, f), []byte("# Task\n"), 0o644)
			}
			idx := queue.BuildIndex(tasksDir)
			got := len(idx.TasksByState(queue.DirBacklog))
			if got != tt.want {
				t.Errorf("len(idx.TasksByState) = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestListTasksFromIndex_MissingDir(t *testing.T) {
	// With PollIndex, missing directories result in zero-length slices.
	idx := queue.BuildIndex(filepath.Join(t.TempDir(), "nonexistent"))
	got := len(idx.TasksByState(queue.DirBacklog))
	if got != 0 {
		t.Errorf("len(idx.TasksByState(missing dir)) = %d, want 0", got)
	}
}

func TestListTasksFromIndex_Empty(t *testing.T) {
	tasksDir := setupTasksDir(t)
	idx := queue.BuildIndex(tasksDir)
	tasks := listTasksFromIndex(idx, queue.DirBacklog)
	if len(tasks) != 0 {
		t.Errorf("listTasksFromIndex(empty) returned %d tasks, want 0", len(tasks))
	}
}

func TestListTasksFromIndex_SortsByPriorityThenName(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirBacklog, "z-task.md", "---\nid: z-task\npriority: 20\n---\n# Z task\n")
	writeTask(t, tasksDir, queue.DirBacklog, "a-task.md", "---\nid: a-task\npriority: 10\n---\n# A task\n")
	writeTask(t, tasksDir, queue.DirBacklog, "b-task.md", "---\nid: b-task\npriority: 10\n---\n# B task\n")

	idx := queue.BuildIndex(tasksDir)
	tasks := listTasksFromIndex(idx, queue.DirBacklog)
	if len(tasks) != 3 {
		t.Fatalf("listTasksFromIndex returned %d tasks, want 3", len(tasks))
	}
	// Priority 10 before 20.
	if tasks[0].name != "a-task.md" {
		t.Errorf("tasks[0].name = %q, want a-task.md", tasks[0].name)
	}
	if tasks[1].name != "b-task.md" {
		t.Errorf("tasks[1].name = %q, want b-task.md", tasks[1].name)
	}
	if tasks[2].name != "z-task.md" {
		t.Errorf("tasks[2].name = %q, want z-task.md", tasks[2].name)
	}
}

func TestListTasksFromIndex_ExtractsTitleAndMeta(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirBacklog, "my-task.md", "---\nid: my-task\npriority: 15\nmax_retries: 5\n---\n# My fancy title\n")

	idx := queue.BuildIndex(tasksDir)
	tasks := listTasksFromIndex(idx, queue.DirBacklog)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].title != "My fancy title" {
		t.Errorf("title = %q, want %q", tasks[0].title, "My fancy title")
	}
	if tasks[0].id != "my-task" {
		t.Errorf("id = %q, want %q", tasks[0].id, "my-task")
	}
	if tasks[0].priority != 15 {
		t.Errorf("priority = %d, want 15", tasks[0].priority)
	}
	if tasks[0].maxRetries != 5 {
		t.Errorf("maxRetries = %d, want 5", tasks[0].maxRetries)
	}
}

func TestListTasksFromIndex_MalformedFrontmatter(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirBacklog, "bad.md", "---\n: invalid yaml [\n---\n# Title\n")

	idx := queue.BuildIndex(tasksDir)
	// Parse failures are included with default metadata so they remain visible.
	tasks := listTasksFromIndex(idx, queue.DirBacklog)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	// With parse error, defaults are used.
	if tasks[0].priority != 50 {
		t.Errorf("priority = %d, want default 50", tasks[0].priority)
	}
	if tasks[0].maxRetries != 3 {
		t.Errorf("maxRetries = %d, want default 3", tasks[0].maxRetries)
	}
}

func TestListTasksFromIndex_SnapshotMetadata(t *testing.T) {
	tasksDir := setupTasksDir(t)
	content := "<!-- claimed-by: agent-abc  claimed-at: 2026-06-15T12:30:00Z -->\n" +
		"<!-- branch: task/my-task -->\n" +
		"<!-- failure: agent-abc at 2026-06-15T12:35:00Z — tests failed -->\n" +
		"<!-- failure: agent-abc at 2026-06-15T12:40:00Z — lint errors -->\n" +
		"<!-- cycle-failure: mato at 2026-06-15T13:00:00Z — circular dep -->\n" +
		"<!-- terminal-failure: mato at 2026-06-15T14:00:00Z — invalid glob -->\n" +
		"---\nid: my-task\npriority: 10\nmax_retries: 5\n---\n# My task\n"
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", content)

	idx := queue.BuildIndex(tasksDir)
	tasks := listTasksFromIndex(idx, queue.DirInProgress)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	task := tasks[0]
	if task.branch != "task/my-task" {
		t.Errorf("branch = %q, want %q", task.branch, "task/my-task")
	}
	if task.claimedBy != "agent-abc" {
		t.Errorf("claimedBy = %q, want %q", task.claimedBy, "agent-abc")
	}
	want := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	if !task.claimedAt.Equal(want) {
		t.Errorf("claimedAt = %v, want %v", task.claimedAt, want)
	}
	if task.failureCount != 2 {
		t.Errorf("failureCount = %d, want 2", task.failureCount)
	}
	if task.lastFailureReason != "lint errors" {
		t.Errorf("lastFailureReason = %q, want %q", task.lastFailureReason, "lint errors")
	}
	if task.lastCycleFailureReason != "circular dep" {
		t.Errorf("lastCycleFailureReason = %q, want %q", task.lastCycleFailureReason, "circular dep")
	}
	if task.lastTerminalFailureReason != "invalid glob" {
		t.Errorf("lastTerminalFailureReason = %q, want %q", task.lastTerminalFailureReason, "invalid glob")
	}
}

func TestListTasksFromIndex_ParseFailureBranch(t *testing.T) {
	tasksDir := setupTasksDir(t)
	content := "<!-- branch: task/bad-task -->\n---\n: invalid yaml [\n---\n"
	writeTask(t, tasksDir, queue.DirReadyReview, "bad.md", content)

	idx := queue.BuildIndex(tasksDir)
	tasks := listTasksFromIndex(idx, queue.DirReadyReview)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].branch != "task/bad-task" {
		t.Errorf("branch = %q, want %q", tasks[0].branch, "task/bad-task")
	}
}

func TestListTasksFromIndex_CancelledPropagation(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirFailed, "cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: cancelled\n---\n# Cancelled\n")
	writeTask(t, tasksDir, queue.DirFailed, "broken.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n")

	idx := queue.BuildIndex(tasksDir)
	tasks := listTasksFromIndex(idx, queue.DirFailed)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	for _, task := range tasks {
		if !task.cancelled {
			t.Fatalf("task %+v should be marked cancelled", task)
		}
	}
}

func TestStatusAgentDisplayName(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"abc12345", "agent-abc12345"},
		{"agent-abc12345", "agent-abc12345"},
		{"x", "agent-x"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			a := statusAgent{ID: tt.id}
			if got := a.displayName(); got != tt.want {
				t.Errorf("displayName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestActiveAgents_EmptyLocksDir(t *testing.T) {
	tasksDir := setupTasksDir(t)
	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

func TestActiveAgents_NoLocksDir(t *testing.T) {
	tasksDir := t.TempDir()
	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if agents != nil {
		t.Errorf("expected nil agents, got %v", agents)
	}
}

func TestActiveAgents_DeadProcess(t *testing.T) {
	tasksDir := setupTasksDir(t)
	// PID that almost certainly doesn't exist.
	os.WriteFile(filepath.Join(tasksDir, ".locks", "dead0001.pid"), []byte("2147483647"), 0o644)

	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents (dead process), got %d", len(agents))
	}
}

func TestActiveAgents_LiveProcess(t *testing.T) {
	tasksDir := setupTasksDir(t)
	cleanup, err := queue.RegisterAgent(tasksDir, "live0001")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].ID != "live0001" {
		t.Errorf("agent ID = %q, want %q", agents[0].ID, "live0001")
	}
	if agents[0].PID != os.Getpid() {
		t.Errorf("agent PID = %d, want %d", agents[0].PID, os.Getpid())
	}
}

func TestActiveAgents_SortedByDisplayName(t *testing.T) {
	tasksDir := setupTasksDir(t)
	// Write multiple lock files for the current PID so they appear active.
	pid := strconv.Itoa(os.Getpid())
	for _, id := range []string{"zzz00001", "aaa00001"} {
		cleanup, err := queue.RegisterAgent(tasksDir, id)
		if err != nil {
			t.Fatalf("RegisterAgent(%s): %v", id, err)
		}
		defer cleanup()
		_ = pid // registered via RegisterAgent
	}

	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) < 2 {
		t.Fatalf("expected >= 2 agents, got %d", len(agents))
	}
	for i := 1; i < len(agents); i++ {
		if agents[i-1].displayName() >= agents[i].displayName() {
			t.Errorf("agents not sorted: %q >= %q", agents[i-1].displayName(), agents[i].displayName())
		}
	}
}

func TestActiveAgents_SkipsNonPIDFiles(t *testing.T) {
	tasksDir := setupTasksDir(t)
	// Non-.pid file and a directory should be ignored.
	os.WriteFile(filepath.Join(tasksDir, ".locks", "notes.txt"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(tasksDir, ".locks", "subdir"), 0o755)

	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

func TestActiveAgents_InvalidPID(t *testing.T) {
	tasksDir := setupTasksDir(t)
	// Lock file with non-numeric content — should be skipped.
	os.WriteFile(filepath.Join(tasksDir, ".locks", "badpid01.pid"), []byte("not-a-number"), 0o644)

	agents, _, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents (invalid PID), got %d", len(agents))
	}
}

func TestActiveAgents_UnreadableLockFile(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Register a real agent so IsAgentActive returns true.
	cleanup, err := queue.RegisterAgent(tasksDir, "good0001")
	if err != nil {
		t.Fatalf("RegisterAgent(good0001): %v", err)
	}
	defer cleanup()

	// Register a second agent whose lock file will be "unreadable".
	cleanup2, err := queue.RegisterAgent(tasksDir, "bad00001")
	if err != nil {
		t.Fatalf("RegisterAgent(bad00001): %v", err)
	}
	defer cleanup2()

	// Inject a ReadFile hook that fails for the bad agent's lock file.
	origFn := readLockFileFn
	readLockFileFn = func(name string) ([]byte, error) {
		if filepath.Base(name) == "bad00001.pid" {
			return nil, errors.New("simulated read error")
		}
		return origFn(name)
	}
	defer func() { readLockFileFn = origFn }()

	agents, warnings, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents returned error: %v", err)
	}

	// The good agent should still appear.
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].ID != "good0001" {
		t.Errorf("agent ID = %q, want %q", agents[0].ID, "good0001")
	}

	// A warning should be emitted for the unreadable lock file.
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "bad00001.pid") {
		t.Errorf("warning should mention bad00001.pid, got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "simulated read error") {
		t.Errorf("warning should mention error, got %q", warnings[0])
	}
}

func TestLatestProgressByAgent(t *testing.T) {
	now := time.Now().UTC()
	messages := []messaging.Message{
		{ID: "1", From: "a1", Type: "progress", Body: "Step: WORK", SentAt: now.Add(-5 * time.Minute)},
		{ID: "2", From: "a1", Type: "progress", Body: "Step: COMMIT", SentAt: now.Add(-1 * time.Minute)},
		{ID: "3", From: "a2", Type: "progress", Body: "Step: VERIFY", SentAt: now.Add(-3 * time.Minute)},
		{ID: "4", From: "a1", Type: "intent", Body: "Starting", SentAt: now},
	}

	result := latestProgressByAgent(messages)

	if len(result) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(result))
	}
	if result["a1"].Body != "Step: COMMIT" {
		t.Errorf("a1 latest = %q, want %q", result["a1"].Body, "Step: COMMIT")
	}
	if result["a2"].Body != "Step: VERIFY" {
		t.Errorf("a2 latest = %q, want %q", result["a2"].Body, "Step: VERIFY")
	}
}

func TestLatestProgressByAgent_NoProgressMessages(t *testing.T) {
	messages := []messaging.Message{
		{ID: "1", From: "a1", Type: "intent", Body: "Starting"},
		{ID: "2", From: "a2", Type: "completion", Body: "Done"},
	}
	result := latestProgressByAgent(messages)
	if len(result) != 0 {
		t.Errorf("expected 0 agents, got %d", len(result))
	}
}

func TestLatestProgressByAgent_Empty(t *testing.T) {
	result := latestProgressByAgent(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 agents for nil input, got %d", len(result))
	}
}

func TestLatestProgressByAgent_EqualTimestampTieBreak(t *testing.T) {
	now := time.Now().UTC()
	// Two progress messages from the same agent with the same timestamp.
	// The canonical rule says the lexically smallest ID wins.
	messages := []messaging.Message{
		{ID: "z-msg", From: "a1", Type: "progress", Body: "body-z", SentAt: now},
		{ID: "a-msg", From: "a1", Type: "progress", Body: "body-a", SentAt: now},
	}

	result := latestProgressByAgent(messages)

	if len(result) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(result))
	}
	if result["a1"].ID != "a-msg" {
		t.Errorf("a1 ID = %q, want %q (smallest ID for equal timestamps)", result["a1"].ID, "a-msg")
	}

	// Reverse input order — result must be the same.
	reversed := []messaging.Message{messages[1], messages[0]}
	resultRev := latestProgressByAgent(reversed)

	if resultRev["a1"].ID != "a-msg" {
		t.Errorf("reversed: a1 ID = %q, want %q (order-independent tie-break)", resultRev["a1"].ID, "a-msg")
	}
}

func TestLatestProgressByAgent_EqualTimestampMultipleAgents(t *testing.T) {
	now := time.Now().UTC()
	messages := []messaging.Message{
		{ID: "z1", From: "a1", Type: "progress", Body: "z1-body", SentAt: now},
		{ID: "a1", From: "a1", Type: "progress", Body: "a1-body", SentAt: now},
		{ID: "z2", From: "a2", Type: "progress", Body: "z2-body", SentAt: now},
		{ID: "a2", From: "a2", Type: "progress", Body: "a2-body", SentAt: now},
	}

	result := latestProgressByAgent(messages)

	if len(result) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(result))
	}
	if result["a1"].ID != "a1" {
		t.Errorf("a1 ID = %q, want %q", result["a1"].ID, "a1")
	}
	if result["a2"].ID != "a2" {
		t.Errorf("a2 ID = %q, want %q", result["a2"].ID, "a2")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"sub-second", 500 * time.Millisecond, "1 sec"},
		{"one second", time.Second, "1 sec"},
		{"30 seconds", 30 * time.Second, "30 sec"},
		{"59 seconds", 59 * time.Second, "59 sec"},
		{"one minute", time.Minute, "1 min"},
		{"5 minutes", 5 * time.Minute, "5 min"},
		{"90 seconds", 90 * time.Second, "1 min"},
		{"2 hours", 2 * time.Hour, "2 hr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestPluralize(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "tasks"},
		{1, "task"},
		{2, "tasks"},
		{100, "tasks"},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.n), func(t *testing.T) {
			got := pluralize(tt.n, "task", "tasks")
			if got != tt.want {
				t.Errorf("pluralize(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestReverseDepsFromIndex_MultipleDeps(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirWaiting, "a.md", "---\nid: a\ndepends_on: [x, y]\n---\n# A\n")
	writeTask(t, tasksDir, queue.DirWaiting, "b.md", "---\nid: b\ndepends_on: [x]\n---\n# B\n")

	idx := queue.BuildIndex(tasksDir)
	result := reverseDepsFromIndex(idx)
	if len(result["x"]) != 2 {
		t.Errorf("x has %d dependents, want 2", len(result["x"]))
	}
	if len(result["y"]) != 1 {
		t.Errorf("y has %d dependents, want 1", len(result["y"]))
	}
}

func TestReverseDepsFromIndex_EmptyWaiting(t *testing.T) {
	tasksDir := setupTasksDir(t)
	idx := queue.BuildIndex(tasksDir)
	result := reverseDepsFromIndex(idx)
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestReverseDepsFromIndex_NoDependencies(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirWaiting, "standalone.md", "---\nid: standalone\n---\n# Standalone\n")

	idx := queue.BuildIndex(tasksDir)
	result := reverseDepsFromIndex(idx)
	if len(result) != 0 {
		t.Errorf("expected empty map for task with no deps, got %d entries", len(result))
	}
}

func TestTaskStatesByIDFromIndex(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirBacklog, "task-a.md", "---\nid: task-a\n---\n# A\n")
	writeTask(t, tasksDir, queue.DirInProgress, "task-b.md", "---\nid: task-b\n---\n# B\n")
	writeTask(t, tasksDir, queue.DirCompleted, "task-c.md", "---\nid: task-c\n---\n# C\n")
	writeTask(t, tasksDir, queue.DirFailed, "task-d.md", "---\nid: task-d\n---\n# D\n")

	idx := queue.BuildIndex(tasksDir)
	states := taskStatesByIDFromIndex(idx)

	// Both the ID and the filename stem should be mapped.
	expectations := map[string]string{
		"task-a": queue.DirBacklog,
		"task-b": queue.DirInProgress,
		"task-c": queue.DirCompleted,
		"task-d": queue.DirFailed,
	}
	for id, wantState := range expectations {
		if got := states[id]; got != wantState {
			t.Errorf("states[%q] = %q, want %q", id, got, wantState)
		}
	}
}

func TestTaskStatesByIDFromIndex_Empty(t *testing.T) {
	tasksDir := setupTasksDir(t)
	idx := queue.BuildIndex(tasksDir)
	states := taskStatesByIDFromIndex(idx)
	if len(states) != 0 {
		t.Errorf("expected empty states, got %d", len(states))
	}
}

func TestTaskStatesByIDFromIndex_FileStemKey(t *testing.T) {
	tasksDir := setupTasksDir(t)
	// Task with no explicit id — should be keyed by filename stem.
	writeTask(t, tasksDir, queue.DirBacklog, "no-id-task.md", "---\npriority: 10\n---\n# No ID\n")

	idx := queue.BuildIndex(tasksDir)
	states := taskStatesByIDFromIndex(idx)
	stem := frontmatter.TaskFileStem("no-id-task.md")
	if got := states[stem]; got != queue.DirBacklog {
		t.Errorf("states[%q] = %q, want %q", stem, got, queue.DirBacklog)
	}
}

func TestTaskStatesByIDFromIndex_ParseFailureStemKey(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirFailed, "broken.md", "---\npriority: nope\n---\n# Broken\n")

	idx := queue.BuildIndex(tasksDir)
	states := taskStatesByIDFromIndex(idx)
	if got := states["broken"]; got != queue.DirFailed {
		t.Errorf("states[%q] = %q, want %q", "broken", got, queue.DirFailed)
	}
}

func TestGatherStatus_EmptyTasksDir(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	// All queue counts should be zero.
	for _, dir := range queue.AllDirs {
		if data.queueCounts[dir] != 0 {
			t.Errorf("queueCounts[%q] = %d, want 0", dir, data.queueCounts[dir])
		}
	}
	if data.runnable != 0 {
		t.Errorf("runnable = %d, want 0", data.runnable)
	}
	if len(data.agents) != 0 {
		t.Errorf("agents = %d, want 0", len(data.agents))
	}
	if len(data.inProgressTasks) != 0 {
		t.Errorf("inProgressTasks = %d, want 0", len(data.inProgressTasks))
	}
	if len(data.failedTasks) != 0 {
		t.Errorf("failedTasks = %d, want 0", len(data.failedTasks))
	}
	if len(data.recentMessages) != 0 {
		t.Errorf("recentMessages = %d, want 0", len(data.recentMessages))
	}
	if data.mergeLockActive {
		t.Error("mergeLockActive should be false")
	}
}

func TestGatherStatus_PopulatedQueue(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	writeTask(t, tasksDir, queue.DirBacklog, "b1.md", "---\nid: b1\npriority: 10\n---\n# Backlog 1\n")
	writeTask(t, tasksDir, queue.DirBacklog, "b2.md", "---\nid: b2\npriority: 20\n---\n# Backlog 2\n")
	writeTask(t, tasksDir, queue.DirInProgress, "ip.md", "<!-- claimed-by: agent1  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: ip\n---\n# In Progress\n")
	writeTask(t, tasksDir, queue.DirCompleted, "done.md", "---\nid: done\n---\n# Done\n")
	writeTask(t, tasksDir, queue.DirFailed, "fail.md", "<!-- failure: a1 at 2026-01-01T00:01:00Z — broken -->\n---\nid: fail\nmax_retries: 3\n---\n# Fail\n")

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	if data.queueCounts[queue.DirBacklog] != 2 {
		t.Errorf("backlog count = %d, want 2", data.queueCounts[queue.DirBacklog])
	}
	if data.queueCounts[queue.DirInProgress] != 1 {
		t.Errorf("in-progress count = %d, want 1", data.queueCounts[queue.DirInProgress])
	}
	if data.queueCounts[queue.DirCompleted] != 1 {
		t.Errorf("completed count = %d, want 1", data.queueCounts[queue.DirCompleted])
	}
	if data.queueCounts[queue.DirFailed] != 1 {
		t.Errorf("failed count = %d, want 1", data.queueCounts[queue.DirFailed])
	}
	if len(data.inProgressTasks) != 1 {
		t.Errorf("inProgressTasks = %d, want 1", len(data.inProgressTasks))
	}
	if len(data.failedTasks) != 1 {
		t.Errorf("failedTasks = %d, want 1", len(data.failedTasks))
	}
}

func TestGatherStatus_RecentMessagesLimitedTo5(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		messaging.WriteMessage(tasksDir, messaging.Message{
			ID:     "m" + strconv.Itoa(i),
			From:   "agent1",
			Type:   "progress",
			Task:   "task.md",
			Body:   "msg " + strconv.Itoa(i),
			SentAt: now.Add(time.Duration(i) * time.Second),
		})
	}

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	if len(data.recentMessages) > 5 {
		t.Errorf("recentMessages = %d, want <= 5", len(data.recentMessages))
	}
}

func TestGatherStatus_RunnableExcludesDeferred(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create two backlog tasks that overlap on affects with an in-progress task.
	writeTask(t, tasksDir, queue.DirInProgress, "active.md", "<!-- claimed-by: a1  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: active\naffects:\n  - src/main.go\n---\n# Active\n")
	writeTask(t, tasksDir, queue.DirBacklog, "deferred.md", "---\nid: deferred\naffects:\n  - src/main.go\n---\n# Deferred\n")
	writeTask(t, tasksDir, queue.DirBacklog, "runnable.md", "---\nid: runnable\naffects:\n  - src/other.go\n---\n# Runnable\n")

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	// Total backlog is 2, but 1 is deferred — runnable should be 1.
	if data.queueCounts[queue.DirBacklog] != 2 {
		t.Errorf("backlog count = %d, want 2", data.queueCounts[queue.DirBacklog])
	}
	if data.runnable != 1 {
		t.Errorf("runnable = %d, want 1", data.runnable)
	}
}

func TestGatherStatus_DependencyBlockedBacklogExcludedFromRunnable(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	writeTask(t, tasksDir, queue.DirBacklog, "blocked.md", "---\nid: blocked\ndepends_on: [missing]\npriority: 10\n---\n# Blocked\n")
	writeTask(t, tasksDir, queue.DirBacklog, "runnable.md", "---\nid: runnable\npriority: 20\n---\n# Runnable\n")

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	if data.runnable != 1 {
		t.Fatalf("runnable = %d, want 1", data.runnable)
	}
	if len(data.runnableBacklog) != 1 || data.runnableBacklog[0].name != "runnable.md" {
		t.Fatalf("runnableBacklog = %#v, want only runnable.md", data.runnableBacklog)
	}
	if len(data.waitingTasks) != 1 {
		t.Fatalf("waitingTasks = %d, want 1", len(data.waitingTasks))
	}
	if data.waitingTasks[0].Name != "blocked.md" || data.waitingTasks[0].State != queue.DirBacklog {
		t.Fatalf("waitingTasks[0] = %#v, want blocked backlog task", data.waitingTasks[0])
	}
}

func TestIsMergeLockActive_NoLock(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if isMergeLockActive(tasksDir) {
		t.Error("isMergeLockActive should be false when no lock file exists")
	}
}

func TestIsMergeLockActive_DeadProcess(t *testing.T) {
	tasksDir := setupTasksDir(t)
	os.WriteFile(filepath.Join(tasksDir, ".locks", "merge.lock"), []byte("2147483647"), 0o644)
	if isMergeLockActive(tasksDir) {
		t.Error("isMergeLockActive should be false for dead process")
	}
}

func TestWaitingTasksFromIndex_Empty(t *testing.T) {
	tasksDir := setupTasksDir(t)
	idx := queue.BuildIndex(tasksDir)
	tasks := waitingTasksFromIndex(idx, nil)
	if len(tasks) != 0 {
		t.Errorf("expected 0 waiting tasks, got %d", len(tasks))
	}
}

func TestWaitingTasksFromIndex_SortsByPriorityThenName(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirWaiting, "z-wait.md", "---\nid: z-wait\npriority: 20\ndepends_on: [dep-x]\n---\n# Z waiting\n")
	writeTask(t, tasksDir, queue.DirWaiting, "a-wait.md", "---\nid: a-wait\npriority: 10\ndepends_on: [dep-y]\n---\n# A waiting\n")
	writeTask(t, tasksDir, queue.DirWaiting, "b-wait.md", "---\nid: b-wait\npriority: 10\ndepends_on: [dep-z]\n---\n# B waiting\n")

	idx := queue.BuildIndex(tasksDir)
	tasks := waitingTasksFromIndex(idx, nil)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 waiting tasks, got %d", len(tasks))
	}
	if tasks[0].Name != "a-wait.md" {
		t.Errorf("tasks[0] = %q, want a-wait.md", tasks[0].Name)
	}
	if tasks[1].Name != "b-wait.md" {
		t.Errorf("tasks[1] = %q, want b-wait.md", tasks[1].Name)
	}
	if tasks[2].Name != "z-wait.md" {
		t.Errorf("tasks[2] = %q, want z-wait.md", tasks[2].Name)
	}
}

func TestWaitingTasksFromIndex_CompletedDepShowsCheck(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirCompleted, "dep-done.md", "---\nid: dep-done\n---\n# Done\n")
	writeTask(t, tasksDir, queue.DirWaiting, "waiter.md", "---\nid: waiter\ndepends_on: [dep-done]\n---\n# Waiter\n")

	idx := queue.BuildIndex(tasksDir)
	tasks := waitingTasksFromIndex(idx, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 waiting task, got %d", len(tasks))
	}
	dep := tasks[0].Dependencies[0]
	if dep.ID != "dep-done" {
		t.Errorf("dep ID = %q, want %q", dep.ID, "dep-done")
	}
	if dep.Status != queue.DirCompleted {
		t.Errorf("dep Status = %q, want %q", dep.Status, queue.DirCompleted)
	}
}

func TestWaitingTasksFromIndex_MissingDepShowsCross(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirWaiting, "waiter.md", "---\nid: waiter\ndepends_on: [nonexistent]\n---\n# Waiter\n")

	idx := queue.BuildIndex(tasksDir)
	tasks := waitingTasksFromIndex(idx, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 waiting task, got %d", len(tasks))
	}
	dep := tasks[0].Dependencies[0]
	if dep.ID != "nonexistent" {
		t.Errorf("dep ID = %q, want %q", dep.ID, "nonexistent")
	}
	if dep.Status != "missing" {
		t.Errorf("dep Status = %q, want %q", dep.Status, "missing")
	}
}

func TestWaitingTasksFromIndex_IncludesBlockedBacklogFromSharedMap(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, queue.DirBacklog, "blocked.md", "---\nid: blocked\ndepends_on: [missing]\npriority: 10\n---\n# Blocked\n")

	idx := queue.BuildIndex(tasksDir)
	view := queue.ComputeRunnableBacklogView(tasksDir, idx)
	tasks := waitingTasksFromIndex(idx, view.DependencyBlocked)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 dependency-blocked task, got %d", len(tasks))
	}
	if tasks[0].Name != "blocked.md" {
		t.Fatalf("tasks[0].Name = %q, want %q", tasks[0].Name, "blocked.md")
	}
	if tasks[0].State != queue.DirBacklog {
		t.Fatalf("tasks[0].State = %q, want %q", tasks[0].State, queue.DirBacklog)
	}
	if len(tasks[0].Dependencies) != 1 || tasks[0].Dependencies[0].Status != "unknown" {
		t.Fatalf("Dependencies = %#v, want unknown blocked dependency", tasks[0].Dependencies)
	}
}

func TestGatherStatus_PresenceReadError(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Make the presence directory unreadable to trigger an error.
	presenceDir := filepath.Join(tasksDir, "messages", "presence")
	if err := os.Chmod(presenceDir, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(presenceDir, 0o755) })

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus should not return error for presence failure, got: %v", err)
	}

	if len(data.warnings) == 0 {
		t.Fatal("expected at least one warning for presence read failure")
	}
	found := false
	for _, w := range data.warnings {
		if containsSubstring(w, "presence") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warning mentioning 'presence', got: %v", data.warnings)
	}
	if data.presenceMap != nil {
		t.Errorf("presenceMap should be nil on error, got: %v", data.presenceMap)
	}
}

func TestGatherStatus_CompletionReadError(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Make the completions directory unreadable to trigger an error.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	if err := os.Chmod(completionsDir, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(completionsDir, 0o755) })

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus should not return error for completion failure, got: %v", err)
	}

	if len(data.warnings) == 0 {
		t.Fatal("expected at least one warning for completion read failure")
	}
	found := false
	for _, w := range data.warnings {
		if containsSubstring(w, "completion") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warning mentioning 'completion', got: %v", data.warnings)
	}
	if data.completions != nil {
		t.Errorf("completions should be nil on error, got: %v", data.completions)
	}
}

func TestGatherStatus_MessageReadError(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Make the events directory unreadable to trigger an error.
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	if err := os.Chmod(eventsDir, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(eventsDir, 0o755) })

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus should not return error for message read failure, got: %v", err)
	}

	if len(data.warnings) == 0 {
		t.Fatal("expected at least one warning for message read failure")
	}
	found := false
	for _, w := range data.warnings {
		if containsSubstring(w, "message") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warning mentioning 'message', got: %v", data.warnings)
	}
	if data.recentMessages != nil {
		t.Errorf("recentMessages should be nil on error, got: %v", data.recentMessages)
	}
}

func TestGatherStatus_PartialMessageReadFailure(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Write a valid message.
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID: "good-msg", From: "agent", Type: "intent",
		Task: "task.md", Branch: "branch", Body: "ok",
		SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Create a single unreadable message file to trigger a per-file warning.
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	unreadable := filepath.Join(eventsDir, base.Add(time.Minute).Format("20060102T150405.000000000Z")+"-unreadable.json")
	if err := os.WriteFile(unreadable, []byte(`{"id":"bad"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o644) })

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus should not return error for partial message failure, got: %v", err)
	}

	// The valid message should still be present.
	if len(data.recentMessages) != 1 {
		t.Fatalf("expected 1 recent message, got %d", len(data.recentMessages))
	}
	if data.recentMessages[0].ID != "good-msg" {
		t.Errorf("expected message ID 'good-msg', got %q", data.recentMessages[0].ID)
	}

	// A structured warning about the unreadable file should be surfaced.
	found := false
	for _, w := range data.warnings {
		if containsSubstring(w, "could not read message") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warning mentioning 'could not read message', got: %v", data.warnings)
	}
}

func TestGatherStatus_NoWarningsOnSuccess(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	if len(data.warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", data.warnings)
	}
}

func TestGatherStatus_PauseStateVariants(t *testing.T) {
	tests := []struct {
		name       string
		state      pause.State
		err        error
		wantActive bool
		wantWarn   bool
	}{
		{name: "unpaused", state: pause.State{}, wantActive: false},
		{name: "paused valid", state: pause.State{Active: true, Since: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)}, wantActive: true},
		{name: "paused malformed", state: pause.State{Active: true, ProblemKind: pause.ProblemMalformed, Problem: `invalid timestamp: "bad"`}, wantActive: true, wantWarn: true},
		{name: "hard error", err: errors.New("stat boom"), wantActive: true, wantWarn: true},
	}

	orig := pauseReadFn
	defer func() { pauseReadFn = orig }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pauseReadFn = func(string) (pause.State, error) {
				return tt.state, tt.err
			}
			tasksDir := setupTasksDir(t)
			if err := messaging.Init(tasksDir); err != nil {
				t.Fatalf("messaging.Init: %v", err)
			}
			data, err := gatherStatus(tasksDir)
			if err != nil {
				t.Fatalf("gatherStatus: %v", err)
			}
			if data.pauseState.Active != tt.wantActive {
				t.Fatalf("pauseState.Active = %v, want %v", data.pauseState.Active, tt.wantActive)
			}
			hasWarning := len(data.warnings) > 0
			if hasWarning != tt.wantWarn {
				t.Fatalf("warnings present = %v, want %v (%v)", hasWarning, tt.wantWarn, data.warnings)
			}
		})
	}
}

func TestStatusDataToJSON_ZeroDependencyWaitingTask(t *testing.T) {
	data := statusData{
		queueCounts: map[string]int{},
		waitingTasks: []waitingTaskSummary{
			{
				Name:         "no-deps.md",
				Title:        "No deps",
				Priority:     10,
				State:        queue.DirWaiting,
				Dependencies: nil,
			},
		},
	}
	out := statusDataToJSON(data)
	if len(out.Waiting) != 1 {
		t.Fatalf("expected 1 waiting task, got %d", len(out.Waiting))
	}
	if out.Waiting[0].Dependencies == nil {
		t.Fatal("expected non-nil empty dependencies slice, got nil")
	}
	if len(out.Waiting[0].Dependencies) != 0 {
		t.Fatalf("expected 0 dependencies, got %d: %+v", len(out.Waiting[0].Dependencies), out.Waiting[0].Dependencies)
	}
}

func TestWaitingTasks_TextAndJSONConsistency(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	writeTask(t, tasksDir, queue.DirCompleted, "setup.md", "---\nid: setup\n---\n# Setup\n")
	writeTask(t, tasksDir, queue.DirBacklog, "auth.md", "---\nid: auth\npriority: 10\n---\n# Auth\n")
	writeTask(t, tasksDir, queue.DirWaiting, "api.md", "---\nid: api\npriority: 20\ndepends_on: [setup, auth, nonexistent]\n---\n# API\n")
	writeTask(t, tasksDir, queue.DirWaiting, "docs.md", "---\nid: docs\npriority: 30\ndepends_on: [api]\n---\n# Docs\n")

	data, err := gatherStatus(tasksDir)
	if err != nil {
		t.Fatalf("gatherStatus: %v", err)
	}

	// Convert the shared model to JSON representation.
	jsonOut := statusDataToJSON(data)

	// Verify same number of waiting tasks.
	if len(data.waitingTasks) != len(jsonOut.Waiting) {
		t.Fatalf("waiting task count: text=%d, json=%d",
			len(data.waitingTasks), len(jsonOut.Waiting))
	}

	// Verify each waiting task's name, title, priority, and
	// dependency data match between text and JSON representations.
	for i, wt := range data.waitingTasks {
		jw := jsonOut.Waiting[i]
		if wt.Name != jw.Name {
			t.Errorf("task[%d] name: text=%q, json=%q", i, wt.Name, jw.Name)
		}
		if wt.Title != jw.Title {
			t.Errorf("task[%d] title: text=%q, json=%q", i, wt.Title, jw.Title)
		}
		if wt.Priority != jw.Priority {
			t.Errorf("task[%d] priority: text=%d, json=%d", i, wt.Priority, jw.Priority)
		}
		if len(wt.Dependencies) != len(jw.Dependencies) {
			t.Errorf("task[%d] dep count: text=%d, json=%d",
				i, len(wt.Dependencies), len(jw.Dependencies))
			continue
		}
		for j, dep := range wt.Dependencies {
			jd := jw.Dependencies[j]
			if dep.ID != jd.ID {
				t.Errorf("task[%d] dep[%d] ID: text=%q, json=%q",
					i, j, dep.ID, jd.ID)
			}
			if dep.Status != jd.Status {
				t.Errorf("task[%d] dep[%d] status: text=%q, json=%q",
					i, j, dep.Status, jd.Status)
			}
		}
	}

	// Verify specific expected dependency states.
	apiTask := data.waitingTasks[0]
	if apiTask.Name != "api.md" {
		t.Fatalf("expected first waiting task to be api.md, got %q", apiTask.Name)
	}
	depMap := make(map[string]string, len(apiTask.Dependencies))
	for _, d := range apiTask.Dependencies {
		depMap[d.ID] = d.Status
	}
	if depMap["setup"] != queue.DirCompleted {
		t.Errorf("setup dep status = %q, want %q", depMap["setup"], queue.DirCompleted)
	}
	if depMap["auth"] != queue.DirBacklog {
		t.Errorf("auth dep status = %q, want %q", depMap["auth"], queue.DirBacklog)
	}
	if depMap["nonexistent"] != "missing" {
		t.Errorf("nonexistent dep status = %q, want %q", depMap["nonexistent"], "missing")
	}
}

// TestGatherStatus_ActiveAgentProgressOutsideRecentWindow verifies that an
// active agent's latest progress message is still shown even when more than
// statusMessageLimit newer messages have been written by other agents.
func TestGatherStatus_ActiveAgentProgressOutsideRecentWindow(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register an active agent.
	cleanup, err := queue.RegisterAgent(tasksDir, "slowagent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write a progress message from slowagent early in the timeline.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID:     "slow-progress",
		From:   "slowagent",
		Type:   "progress",
		Task:   "slow-task.md",
		Body:   "Step: WORK",
		SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage(slow): %v", err)
	}

	// Flood with statusMessageLimit+10 newer messages from other agents,
	// pushing slowagent's progress outside the recent-message window.
	for i := 0; i < statusMessageLimit+10; i++ {
		if err := messaging.WriteMessage(tasksDir, messaging.Message{
			ID:     fmt.Sprintf("flood-%d", i),
			From:   "otheragent",
			Type:   "progress",
			Task:   "other-task.md",
			Body:   fmt.Sprintf("flood %d", i),
			SentAt: base.Add(time.Duration(1+i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage(flood %d): %v", i, err)
		}
	}

	data, gatherErr := gatherStatus(tasksDir)
	if gatherErr != nil {
		t.Fatalf("gatherStatus: %v", gatherErr)
	}

	// slowagent should still appear in activeProgress despite its message
	// being outside the 50-message window.
	found := false
	for _, p := range data.activeProgress {
		if p.displayID == "agent-slowagent" {
			found = true
			if p.body != "Step: WORK" {
				t.Errorf("slowagent progress body = %q, want %q", p.body, "Step: WORK")
			}
			if p.task != "slow-task.md" {
				t.Errorf("slowagent progress task = %q, want %q", p.task, "slow-task.md")
			}
			break
		}
	}
	if !found {
		t.Errorf("slowagent not found in activeProgress; got %d entries: %v",
			len(data.activeProgress), data.activeProgress)
	}
}

// TestGatherStatus_ActiveAgentProgressInsideWindowNotDuplicated verifies that
// when an active agent's progress is already inside the recent window, no
// older-message scan is performed and the result is correct.
func TestGatherStatus_ActiveAgentProgressInsideWindowNotDuplicated(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	cleanup, err := queue.RegisterAgent(tasksDir, "fastagent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write a few messages, then a progress from fastagent — all within the window.
	for i := 0; i < 5; i++ {
		if err := messaging.WriteMessage(tasksDir, messaging.Message{
			ID:     fmt.Sprintf("m%d", i),
			From:   "otheragent",
			Type:   "progress",
			Task:   "task.md",
			Body:   fmt.Sprintf("msg %d", i),
			SentAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID:     "fast-progress",
		From:   "fastagent",
		Type:   "progress",
		Task:   "fast-task.md",
		Body:   "Step: COMMIT",
		SentAt: base.Add(10 * time.Second),
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	data, gatherErr := gatherStatus(tasksDir)
	if gatherErr != nil {
		t.Fatalf("gatherStatus: %v", gatherErr)
	}

	count := 0
	for _, p := range data.activeProgress {
		if p.displayID == "agent-fastagent" {
			count++
			if p.body != "Step: COMMIT" {
				t.Errorf("fastagent progress body = %q, want %q", p.body, "Step: COMMIT")
			}
		}
	}
	if count != 1 {
		t.Errorf("fastagent appeared %d times in activeProgress, want 1", count)
	}
}

// TestGatherStatus_EqualTimestampProgressConsistency verifies that an active
// agent with multiple equal-timestamp progress messages gets the same result
// regardless of whether those messages are inside or outside the recent window.
func TestGatherStatus_EqualTimestampProgressConsistency(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	cleanup, err := queue.RegisterAgent(tasksDir, "eqagent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sameTime := base

	// Write two equal-timestamp progress messages with IDs "z-msg" and "a-msg".
	// latestProgressByAgent keeps "a-msg" (smaller ID). The fallback must agree.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID: "z-msg", From: "eqagent", Type: "progress",
		Task: "eq-task.md", Body: "body-z",
		SentAt: sameTime,
	}); err != nil {
		t.Fatalf("WriteMessage(z-msg): %v", err)
	}
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID: "a-msg", From: "eqagent", Type: "progress",
		Task: "eq-task.md", Body: "body-a",
		SentAt: sameTime,
	}); err != nil {
		t.Fatalf("WriteMessage(a-msg): %v", err)
	}

	// Flood with statusMessageLimit+10 newer messages to push eqagent outside.
	for i := 0; i < statusMessageLimit+10; i++ {
		if err := messaging.WriteMessage(tasksDir, messaging.Message{
			ID:     fmt.Sprintf("flood-%d", i),
			From:   "otheragent",
			Type:   "progress",
			Task:   "other.md",
			Body:   fmt.Sprintf("flood %d", i),
			SentAt: base.Add(time.Duration(1+i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage(flood %d): %v", i, err)
		}
	}

	data, gatherErr := gatherStatus(tasksDir)
	if gatherErr != nil {
		t.Fatalf("gatherStatus: %v", gatherErr)
	}

	found := false
	for _, p := range data.activeProgress {
		if p.displayID == "agent-eqagent" {
			found = true
			// Must match the same message that latestProgressByAgent would pick
			// (smallest ID "a-msg" for equal timestamps).
			if p.body != "body-a" {
				t.Errorf("eqagent body = %q, want %q (smallest-ID tie-break)", p.body, "body-a")
			}
			break
		}
	}
	if !found {
		t.Errorf("eqagent not found in activeProgress")
	}
}

// TestGatherStatus_OlderProgressUnreadableWarning verifies that when an older
// progress file is unreadable, gatherStatus still recovers valid fallback
// progress for other agents and surfaces the degraded read as a warning.
func TestGatherStatus_OlderProgressUnreadableWarning(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register two active agents.
	cleanupGood, err := queue.RegisterAgent(tasksDir, "goodagent")
	if err != nil {
		t.Fatalf("RegisterAgent(good): %v", err)
	}
	defer cleanupGood()

	cleanupBad, err := queue.RegisterAgent(tasksDir, "badagent")
	if err != nil {
		t.Fatalf("RegisterAgent(bad): %v", err)
	}
	defer cleanupBad()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write progress messages for both agents early in the timeline.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID: "good-prog", From: "goodagent", Type: "progress",
		Task: "good-task.md", Body: "Step: WORK", SentAt: base,
	}); err != nil {
		t.Fatalf("WriteMessage(good): %v", err)
	}
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID: "bad-prog", From: "badagent", Type: "progress",
		Task: "bad-task.md", Body: "Step: VERIFY", SentAt: base.Add(time.Second),
	}); err != nil {
		t.Fatalf("WriteMessage(bad): %v", err)
	}

	// Make badagent's progress file unreadable via the messaging read hook.
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, readErr := os.ReadDir(eventsDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	var unreadablePath string
	for _, e := range entries {
		if strings.Contains(e.Name(), "-progress-") && strings.Contains(e.Name(), "bad-prog") {
			unreadablePath = filepath.Join(eventsDir, e.Name())
			break
		}
	}
	origReadFile := messaging.TestHookReadFile()
	messaging.SetTestHookReadFile(func(path string) ([]byte, error) {
		if path == unreadablePath {
			return nil, fmt.Errorf("permission denied")
		}
		return origReadFile(path)
	})
	t.Cleanup(func() { messaging.SetTestHookReadFile(origReadFile) })

	// Flood with newer messages to push both agents outside the recent window.
	for i := 0; i < statusMessageLimit+10; i++ {
		if err := messaging.WriteMessage(tasksDir, messaging.Message{
			ID:     fmt.Sprintf("flood-%d", i),
			From:   "otheragent",
			Type:   "progress",
			Task:   "other.md",
			Body:   fmt.Sprintf("flood %d", i),
			SentAt: base.Add(time.Duration(10+i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage(flood %d): %v", i, err)
		}
	}

	data, gatherErr := gatherStatus(tasksDir)
	if gatherErr != nil {
		t.Fatalf("gatherStatus: %v", gatherErr)
	}

	// goodagent should still appear in activeProgress.
	foundGood := false
	for _, p := range data.activeProgress {
		if p.displayID == "agent-goodagent" {
			foundGood = true
			if p.body != "Step: WORK" {
				t.Errorf("goodagent body = %q, want %q", p.body, "Step: WORK")
			}
			break
		}
	}
	if !foundGood {
		t.Errorf("goodagent not found in activeProgress despite valid file")
	}

	// There should be a warning about the unreadable file.
	foundWarning := false
	for _, w := range data.warnings {
		if strings.Contains(w, "could not read older progress message") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected warning about unreadable older progress message, got warnings: %v", data.warnings)
	}
}

// TestGatherStatus_AgentStyleFilenameProgressRecovery verifies that the
// fallback progress recovery finds progress messages written with agent-style
// filenames (e.g. nonce-agent-work.json) that do not contain "-progress-".
func TestGatherStatus_AgentStyleFilenameProgressRecovery(t *testing.T) {
	tasksDir := setupTasksDir(t)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register an active agent.
	cleanup, err := queue.RegisterAgent(tasksDir, "shellagent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Write a progress message with an agent-style filename (no "-progress-").
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	agentMsg := messaging.Message{
		ID: "sa-work", From: "shellagent", Type: "progress",
		Task: "shell-task.md", Body: "Step: WORK",
		SentAt: base,
	}
	data, marshalErr := json.Marshal(agentMsg)
	if marshalErr != nil {
		t.Fatalf("Marshal: %v", marshalErr)
	}
	fname := base.Format("20060102T150405.000000000Z") + "-shellagent-work.json"
	if err := os.WriteFile(filepath.Join(eventsDir, fname), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Flood with newer messages to push shellagent's progress outside the window.
	for i := 0; i < statusMessageLimit+10; i++ {
		if err := messaging.WriteMessage(tasksDir, messaging.Message{
			ID:     fmt.Sprintf("flood-%d", i),
			From:   "otheragent",
			Type:   "progress",
			Task:   "other-task.md",
			Body:   fmt.Sprintf("flood %d", i),
			SentAt: base.Add(time.Duration(1+i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteMessage(flood %d): %v", i, err)
		}
	}

	gathered, gatherErr := gatherStatus(tasksDir)
	if gatherErr != nil {
		t.Fatalf("gatherStatus: %v", gatherErr)
	}

	// shellagent should appear in activeProgress despite its agent-style filename.
	found := false
	for _, p := range gathered.activeProgress {
		if p.displayID == "agent-shellagent" {
			found = true
			if p.body != "Step: WORK" {
				t.Errorf("shellagent body = %q, want %q", p.body, "Step: WORK")
			}
			if p.task != "shell-task.md" {
				t.Errorf("shellagent task = %q, want %q", p.task, "shell-task.md")
			}
			break
		}
	}
	if !found {
		t.Errorf("shellagent not found in activeProgress; got %d entries: %v",
			len(gathered.activeProgress), gathered.activeProgress)
	}
}
