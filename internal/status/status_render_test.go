package status

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mato/internal/dirs"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/queue"
	"mato/internal/ui"
)

// plainColorSet returns a colorSet with no ANSI formatting.
func plainColorSet() colorSet {
	plain := func(a ...interface{}) string {
		var buf bytes.Buffer
		for i, v := range a {
			if i > 0 {
				buf.WriteString(" ")
			}
			switch vv := v.(type) {
			case string:
				buf.WriteString(vv)
			default:
				// Use fmt.Sprintf for non-string types.
				buf.WriteString(genericStr(vv))
			}
		}
		return buf.String()
	}
	return colorSet{Bold: plain, Green: plain, Red: plain, Yellow: plain, Cyan: plain, Dim: plain}
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

func genericStr(v interface{}) string {
	switch vv := v.(type) {
	case string:
		return vv
	case int:
		return intToStr(vv)
	case bool:
		if vv {
			return "true"
		}
		return "false"
	default:
		return "?"
	}
}

func TestRenderQueueOverview_ZeroCounts(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		queueCounts:    map[string]int{},
		deferredDetail: map[string]queue.DeferralInfo{},
	}

	renderQueueOverview(&buf, c, data)
	output := buf.String()

	for _, want := range []string{"Queue Overview", "runnable:", "deferred:", "blocked:", "in-progress:", "ready-review:", "ready-to-merge:", "completed:", "failed:", "merge queue:"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if !strings.Contains(output, "idle") {
		t.Errorf("merge queue should show idle, got:\n%s", output)
	}
}

func TestRenderQueueOverview_MergeLockActive(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		queueCounts:     map[string]int{dirs.Backlog: 3, dirs.InProgress: 2},
		deferredDetail:  map[string]queue.DeferralInfo{},
		runnable:        3,
		mergeLockActive: true,
	}

	renderQueueOverview(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "active") {
		t.Errorf("merge queue should show active, got:\n%s", output)
	}
}

func TestRenderQueueOverview_PauseState(t *testing.T) {
	tests := []struct {
		name  string
		state pause.State
		want  string
	}{
		{name: "not paused", state: pause.State{}, want: "pause state:    not paused"},
		{name: "paused valid", state: pause.State{Active: true, Since: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)}, want: "pause state:    paused since 2026-03-23T10:00:00Z"},
		{name: "paused problem", state: pause.State{Active: true, ProblemKind: pause.ProblemMalformed, Problem: `invalid timestamp: "bad"`}, want: `pause state:    paused (problem: invalid timestamp: "bad")`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderQueueOverview(&buf, plainColorSet(), statusData{queueCounts: map[string]int{}, deferredDetail: map[string]queue.DeferralInfo{}, pauseState: tt.state})
			if !strings.Contains(buf.String(), tt.want) {
				t.Fatalf("output missing %q, got:\n%s", tt.want, buf.String())
			}
		})
	}
}

func TestRenderActiveAgents_None(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderActiveAgents(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Active Agents") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "(none)") {
		t.Errorf("output should show (none), got:\n%s", output)
	}
}

func TestRenderActiveAgents_WithPresence(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "abc12345", PID: 1234}},
		presenceMap: map[string]messaging.PresenceInfo{
			"abc12345": {Task: "my-task.md", Branch: "task/my-task"},
		},
	}

	renderActiveAgents(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "agent-abc12345") {
		t.Errorf("output missing agent name, got:\n%s", output)
	}
	if !strings.Contains(output, "PID 1234") {
		t.Errorf("output missing PID, got:\n%s", output)
	}
	if !strings.Contains(output, "my-task.md") {
		t.Errorf("output missing task, got:\n%s", output)
	}
	if !strings.Contains(output, "task/my-task") {
		t.Errorf("output missing branch, got:\n%s", output)
	}
}

func TestRenderActiveAgents_WithoutPresence(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents:      []statusAgent{{ID: "noinfo01", PID: 9999}},
		presenceMap: map[string]messaging.PresenceInfo{},
	}

	renderActiveAgents(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "agent-noinfo01") {
		t.Errorf("output missing agent name, got:\n%s", output)
	}
	if !strings.Contains(output, "PID 9999") {
		t.Errorf("output missing PID, got:\n%s", output)
	}
	// Should NOT contain task or branch info.
	if strings.Contains(output, ".md") {
		t.Errorf("output should not contain task when no presence, got:\n%s", output)
	}
}

func TestRenderAgentProgress_None(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderAgentProgress(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Current Agent Progress") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "(none)") {
		t.Errorf("output should show (none), got:\n%s", output)
	}
}

func TestRenderAgentProgress_WithEntries(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		activeProgress: []progressEntry{
			{displayID: "agent-a1", body: "Step: WORK", task: "task-a.md", ago: "2 min"},
			{displayID: "agent-b2", body: "Step: COMMIT", task: "task-b.md", ago: "30 sec"},
		},
	}

	renderAgentProgress(&buf, c, data)
	output := buf.String()

	for _, want := range []string{"agent-a1", "Step: WORK", "task-a.md", "2 min", "agent-b2", "Step: COMMIT", "task-b.md", "30 sec"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestRenderInProgressTasks_Empty(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderInProgressTasks(&buf, c, data)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty in-progress, got:\n%s", buf.String())
	}
}

func TestRenderReadyToMerge_Empty(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderReadyToMerge(&buf, c, data)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty ready-to-merge, got:\n%s", buf.String())
	}
}

func TestRenderReadyToMerge_WithTasks(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		readyToMerge: []taskEntry{
			{name: "merge-me.md", title: "Ready to merge", priority: 5},
			{name: "also-me.md", title: "Also ready", priority: 10},
		},
	}

	renderReadyToMerge(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Ready to Merge") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "merge-me.md") {
		t.Errorf("output missing task name, got:\n%s", output)
	}
	if !strings.Contains(output, "Ready to merge") {
		t.Errorf("output missing task title, got:\n%s", output)
	}
	if !strings.Contains(output, "priority 5") {
		t.Errorf("output missing priority, got:\n%s", output)
	}
}

func TestRenderReadyToMerge_NoTitle(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		readyToMerge: []taskEntry{
			{name: "no-title.md", title: "", priority: 50},
		},
	}

	renderReadyToMerge(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "no-title.md") {
		t.Errorf("output missing task name, got:\n%s", output)
	}
	// Should not have a dangling dash from empty title.
	if strings.Contains(output, "no-title.md —") {
		t.Errorf("output should not have dash with empty title, got:\n%s", output)
	}
}

func TestRenderDependencyBlocked_None(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderDependencyBlocked(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Dependency-Blocked") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "(none)") {
		t.Errorf("output should show (none), got:\n%s", output)
	}
}

func TestRenderDependencyBlocked_WithTasks(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		waitingTasks: []waitingTaskSummary{
			{
				Name:     "wait-task.md",
				Title:    "A waiting task",
				Priority: 10,
				State:    dirs.Backlog,
				Dependencies: []waitingDep{
					{ID: "dep-a", Status: "in-progress"},
					{ID: "dep-b", Status: "completed"},
				},
			},
		},
	}

	renderDependencyBlocked(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "wait-task.md") {
		t.Errorf("output missing task name, got:\n%s", output)
	}
	if !strings.Contains(output, "A waiting task") {
		t.Errorf("output missing title, got:\n%s", output)
	}
	if !strings.Contains(output, "(backlog/)") {
		t.Errorf("output missing state suffix, got:\n%s", output)
	}
	if !strings.Contains(output, "depends on:") {
		t.Errorf("output missing 'depends on:', got:\n%s", output)
	}
	if !strings.Contains(output, "dep-a (✗ in-progress)") {
		t.Errorf("output missing dep-a with cross mark, got:\n%s", output)
	}
	if !strings.Contains(output, "dep-b (✓ completed)") {
		t.Errorf("output missing dep-b with check mark, got:\n%s", output)
	}
}

func TestRenderConflictDeferred_None(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		deferredDetail: map[string]queue.DeferralInfo{},
	}

	renderConflictDeferred(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Conflict-Deferred") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "(none)") {
		t.Errorf("output should show (none), got:\n%s", output)
	}
}

func TestRenderConflictDeferred_WithEntries(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		deferredDetail: map[string]queue.DeferralInfo{
			"deferred-task.md": {
				BlockedBy:          "active-task.md",
				BlockedByDir:       "in-progress",
				ConflictingAffects: []string{"src/main.go", "src/util.go"},
			},
		},
	}

	renderConflictDeferred(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "deferred-task.md") {
		t.Errorf("output missing deferred task, got:\n%s", output)
	}
	if !strings.Contains(output, "blocked by: active-task.md") {
		t.Errorf("output missing blocked-by info, got:\n%s", output)
	}
	if !strings.Contains(output, "in-progress") {
		t.Errorf("output missing blocked-by dir, got:\n%s", output)
	}
	if !strings.Contains(output, "src/main.go") {
		t.Errorf("output missing conflicting affects, got:\n%s", output)
	}
}

func TestRenderConflictDeferred_SortedByName(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		deferredDetail: map[string]queue.DeferralInfo{
			"z-task.md": {BlockedBy: "x.md", BlockedByDir: "in-progress", ConflictingAffects: []string{"a.go"}},
			"a-task.md": {BlockedBy: "x.md", BlockedByDir: "in-progress", ConflictingAffects: []string{"b.go"}},
		},
	}

	renderConflictDeferred(&buf, c, data)
	output := buf.String()

	aIdx := strings.Index(output, "a-task.md")
	zIdx := strings.Index(output, "z-task.md")
	if aIdx < 0 || zIdx < 0 {
		t.Fatalf("output missing expected tasks, got:\n%s", output)
	}
	if aIdx >= zIdx {
		t.Errorf("a-task.md should appear before z-task.md, got:\n%s", output)
	}
}

func TestRenderFailedTasks_Empty(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderFailedTasks(&buf, c, data)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty failed tasks, got:\n%s", buf.String())
	}
}

func TestRenderRecentCompletions_Empty(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderRecentCompletions(&buf, c, data)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty completions, got:\n%s", buf.String())
	}
}

func TestRenderRecentCompletions_WithEntries(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		completions: []messaging.CompletionDetail{
			{
				TaskFile:     "done-task.md",
				Title:        "A completed task",
				CommitSHA:    "abc123def456789",
				FilesChanged: []string{"a.go", "b.go"},
				MergedAt:     time.Now().UTC().Add(-5 * time.Minute),
			},
		},
	}

	renderRecentCompletions(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Recent Completions") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "done-task.md") {
		t.Errorf("output missing task file, got:\n%s", output)
	}
	if !strings.Contains(output, "A completed task") {
		t.Errorf("output missing title, got:\n%s", output)
	}
	if !strings.Contains(output, "abc123d") {
		t.Errorf("output missing short SHA, got:\n%s", output)
	}
	if !strings.Contains(output, "2 files") {
		t.Errorf("output missing file count, got:\n%s", output)
	}
}

func TestRenderRecentCompletions_TruncatesTo5(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	completions := make([]messaging.CompletionDetail, 8)
	for i := range completions {
		completions[i] = messaging.CompletionDetail{
			TaskFile:     "task-" + intToStr(i) + ".md",
			CommitSHA:    "abcdef1234567890",
			FilesChanged: []string{"file.go"},
			MergedAt:     time.Now().UTC().Add(-time.Duration(i) * time.Minute),
		}
	}
	data := statusData{completions: completions}

	renderRecentCompletions(&buf, c, data)
	output := buf.String()

	// Should show first 5 completions (indices 0-4), not 5-7.
	if !strings.Contains(output, "task-4.md") {
		t.Errorf("output should show task-4.md (5th entry), got:\n%s", output)
	}
	if strings.Contains(output, "task-5.md") {
		t.Errorf("output should NOT show task-5.md (6th entry), got:\n%s", output)
	}
}

func TestRenderRecentCompletions_SingleFile(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		completions: []messaging.CompletionDetail{
			{
				TaskFile:     "one-file.md",
				CommitSHA:    "abc1234",
				FilesChanged: []string{"only.go"},
				MergedAt:     time.Now().UTC(),
			},
		},
	}

	renderRecentCompletions(&buf, c, data)
	output := buf.String()

	// Should use singular "file" not "files".
	if !strings.Contains(output, "1 file") {
		t.Errorf("output should show '1 file' (singular), got:\n%s", output)
	}
}

func TestRenderRecentCompletions_ShortSHA(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		completions: []messaging.CompletionDetail{
			{
				TaskFile:     "short.md",
				CommitSHA:    "abc",
				FilesChanged: []string{"f.go"},
				MergedAt:     time.Now().UTC(),
			},
		},
	}

	renderRecentCompletions(&buf, c, data)
	output := buf.String()

	// Short SHA should be shown as-is (not truncated further).
	if !strings.Contains(output, "abc") {
		t.Errorf("output should show short SHA 'abc', got:\n%s", output)
	}
}

func TestRenderRecentMessages_None(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderRecentMessages(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Recent Messages") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "(none)") {
		t.Errorf("output should show (none), got:\n%s", output)
	}
}

func TestRenderRecentMessages_WithMessages(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		recentMessages: []messaging.Message{
			{From: "abc12345", Type: "intent", Body: "Starting work", SentAt: time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)},
			{From: "agent-def67890", Type: "progress", Body: "Step: WORK", SentAt: time.Date(2026, 3, 15, 10, 31, 0, 0, time.UTC)},
		},
	}

	renderRecentMessages(&buf, c, data)
	output := buf.String()

	// Bare agent ID should get "agent-" prefix.
	if !strings.Contains(output, "agent-abc12345") {
		t.Errorf("output should prefix bare agent ID, got:\n%s", output)
	}
	// Already-prefixed ID should not get double prefix.
	if strings.Contains(output, "agent-agent-def67890") {
		t.Errorf("output should not double-prefix agent ID, got:\n%s", output)
	}
	if !strings.Contains(output, "intent") {
		t.Errorf("output missing message type, got:\n%s", output)
	}
	if !strings.Contains(output, "Starting work") {
		t.Errorf("output missing message body, got:\n%s", output)
	}
}

func TestRenderRecentMessages_ReverseOrder(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		recentMessages: []messaging.Message{
			{From: "a1", Type: "intent", Body: "First", SentAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{From: "a2", Type: "progress", Body: "Second", SentAt: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)},
			{From: "a3", Type: "completion", Body: "Third", SentAt: time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)},
		},
	}

	renderRecentMessages(&buf, c, data)
	output := buf.String()

	// Most recent message should appear first (reversed order).
	thirdIdx := strings.Index(output, "Third")
	firstIdx := strings.Index(output, "First")
	if thirdIdx < 0 || firstIdx < 0 {
		t.Fatalf("output missing expected messages, got:\n%s", output)
	}
	if thirdIdx >= firstIdx {
		t.Errorf("most recent message should appear before oldest, got:\n%s", output)
	}
}

func TestRenderRecentMessages_EmptyBody(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		recentMessages: []messaging.Message{
			{From: "a1", Type: "intent", Body: "", SentAt: time.Now().UTC()},
		},
	}

	renderRecentMessages(&buf, c, data)
	output := buf.String()

	// Empty body should fall back to showing just the type.
	if !strings.Contains(output, "intent") {
		t.Errorf("output should show type for empty body, got:\n%s", output)
	}
}

func TestRenderWarnings_None(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderWarnings(&buf, c, data)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty warnings, got:\n%s", buf.String())
	}
}

func TestRenderWarnings_WithWarnings(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{warnings: []string{"could not read agent presence: boom", "could not read completion details: nope"}}

	renderWarnings(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Warnings") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "could not read agent presence: boom") {
		t.Errorf("output missing first warning, got:\n%s", output)
	}
	if !strings.Contains(output, "could not read completion details: nope") {
		t.Errorf("output missing second warning, got:\n%s", output)
	}
}

func TestRenderCompactAgents_PrefixedAgentIDUsesProgress(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "agent-abc12345", PID: 1234}},
		activeProgress: []progressEntry{{
			displayID: "agent-abc12345",
			body:      "Step: WORK",
			task:      "task-a.md",
			ago:       "2 min",
		}},
	}

	renderCompactAgents(&buf, c, data)
	output := buf.String()

	for _, want := range []string{"agent-abc12345", "task-a.md", "WORK", "2 min"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestRenderCompactQueueSummary(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		queueCounts: map[string]int{
			dirs.Backlog:     24,
			dirs.InProgress:  7,
			dirs.ReadyReview: 3,
			dirs.ReadyMerge:  1,
			dirs.Failed:      2,
		},
		runnable: 9,
	}

	renderCompactQueueSummary(&buf, c, data)
	output := buf.String()

	for _, want := range []string{
		"Queue: 24 backlog | 9 runnable | 7 running | 3 review | 1 merge | 2 failed",
		"Pause: not paused",
		"Merge queue: idle",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestRenderCompactAgents_WithPresenceNoProgress(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "abc12345", PID: 1234}},
		presenceMap: map[string]messaging.PresenceInfo{
			"abc12345": {Task: "my-task.md", Branch: "task/my-task"},
		},
	}

	renderCompactAgents(&buf, c, data)
	output := buf.String()

	for _, want := range []string{"agent-abc12345", "my-task.md", "task/my-task"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "WORK") {
		t.Errorf("output should not invent a progress stage, got:\n%s", output)
	}
}

func TestRenderCompactAgents_WithProgressNoPresenceFallsBackToProgressTask(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "abc12345", PID: 1234}},
		activeProgress: []progressEntry{{
			displayID: "agent-abc12345",
			body:      "Step: COMMIT",
			task:      "fallback-task.md",
			ago:       "30 sec",
		}},
	}

	renderCompactAgents(&buf, c, data)
	output := buf.String()

	for _, want := range []string{"agent-abc12345", "fallback-task.md", "COMMIT", "30 sec"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestRenderCompactAgents_TruncatesLongList(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	agents := make([]statusAgent, 0, 7)
	for i := range 7 {
		agents = append(agents, statusAgent{ID: "agent-" + intToStr(i), PID: 1000 + i})
	}

	renderCompactAgents(&buf, c, statusData{agents: agents})
	output := buf.String()

	if !strings.Contains(output, "... +2 more") {
		t.Errorf("output should include truncation summary, got:\n%s", output)
	}
	if strings.Contains(output, "agent-5") || strings.Contains(output, "agent-6") {
		t.Errorf("output should omit rows beyond compact limit, got:\n%s", output)
	}
}

func TestRenderCompactAttention_OrphanedInProgressTaskShown(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		inProgressTasks: []taskEntry{{
			name:      "orphaned-task.md",
			claimedBy: "missing-agent",
		}},
	}

	renderCompactAttention(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "orphaned-task.md") {
		t.Errorf("output missing orphaned in-progress task, got:\n%s", output)
	}
	if !strings.Contains(output, "running without active agent") {
		t.Errorf("output missing orphaned-task summary, got:\n%s", output)
	}
}

func TestRenderCompactAttention_ShowsWarningsInlineWhenShort(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{warnings: []string{"warning one", "warning two"}}

	renderCompactAttention(&buf, c, data)
	output := buf.String()

	for _, want := range []string{"Attention", "warning: warning one", "warning: warning two"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestRenderCompactAttention_SummarizesWarningsWhenMany(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{warnings: []string{"one", "two", "three", "four"}}

	renderCompactAttention(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "4 warnings") {
		t.Errorf("output should summarize warnings count, got:\n%s", output)
	}
	if strings.Contains(output, "warning: one") {
		t.Errorf("output should not list individual warnings when summarized, got:\n%s", output)
	}
}

func TestRenderCompactNextUp_TruncatesLongList(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	tasks := make([]taskEntry, 0, 7)
	for i := range 7 {
		tasks = append(tasks, taskEntry{name: "task-" + intToStr(i) + ".md", title: "Task " + intToStr(i)})
	}

	renderCompactNextUp(&buf, c, statusData{runnableBacklog: tasks})
	output := buf.String()

	if !strings.Contains(output, "... +2 more") {
		t.Errorf("output should include truncation summary, got:\n%s", output)
	}
	if strings.Contains(output, "task-5.md") || strings.Contains(output, "task-6.md") {
		t.Errorf("output should omit tasks beyond compact limit, got:\n%s", output)
	}
}

func TestCompactProgressLabel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "short raw body", body: "plain progress", want: "plain progress"},
		{name: "exact boundary", body: "123456789012345678901234", want: "123456789012345678901234"},
		{name: "long raw body", body: "1234567890123456789012345", want: "123456789012345678901..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactProgressLabel(tt.body); got != tt.want {
				t.Fatalf("compactProgressLabel(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestRenderReadyForReview_Empty(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderReadyForReview(&buf, c, data)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty ready-for-review, got:\n%s", buf.String())
	}
}

func TestRenderRunnableBacklog_Empty(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{}

	renderRunnableBacklog(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Runnable Backlog") {
		t.Errorf("output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "(none)") {
		t.Errorf("output should show (none), got:\n%s", output)
	}
}

func TestRenderRunnableBacklog_Ordering(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		runnableBacklog: []taskEntry{
			{name: "high-pri.md", title: "High priority task", priority: 5},
			{name: "mid-pri.md", title: "Medium priority task", priority: 25},
			{name: "low-pri.md", title: "Low priority task", priority: 50},
		},
	}

	renderRunnableBacklog(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "Runnable Backlog (execution order)") {
		t.Errorf("output missing header, got:\n%s", output)
	}

	// Verify numbered entries.
	if !strings.Contains(output, "1. high-pri.md") {
		t.Errorf("output missing first entry, got:\n%s", output)
	}
	if !strings.Contains(output, "2. mid-pri.md") {
		t.Errorf("output missing second entry, got:\n%s", output)
	}
	if !strings.Contains(output, "3. low-pri.md") {
		t.Errorf("output missing third entry, got:\n%s", output)
	}

	// Verify titles are shown.
	if !strings.Contains(output, "High priority task") {
		t.Errorf("output missing title, got:\n%s", output)
	}

	// Verify priorities are shown.
	if !strings.Contains(output, "priority 5") {
		t.Errorf("output missing priority, got:\n%s", output)
	}

	// Verify order: high-pri appears before low-pri.
	highIdx := strings.Index(output, "high-pri.md")
	lowIdx := strings.Index(output, "low-pri.md")
	if highIdx >= lowIdx {
		t.Errorf("high-pri should appear before low-pri, got:\n%s", output)
	}
}

func TestRenderRunnableBacklog_NoTitle(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		runnableBacklog: []taskEntry{
			{name: "no-title.md", title: "", priority: 10},
		},
	}

	renderRunnableBacklog(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "no-title.md") {
		t.Errorf("output missing task name, got:\n%s", output)
	}
	// Should not have a dangling dash from empty title.
	if strings.Contains(output, "no-title.md —") {
		t.Errorf("output should not have dash with empty title, got:\n%s", output)
	}
}

func TestRenderFailedTasks_TerminalFailureOnly(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		failedTasks: []taskEntry{{
			name:                      "terminal.md",
			title:                     "Terminal task",
			maxRetries:                3,
			lastTerminalFailureReason: "unparseable frontmatter",
		}},
	}

	renderFailedTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "structural failure: unparseable frontmatter") {
		t.Errorf("output should show 'structural failure: unparseable frontmatter', got:\n%s", output)
	}
	if strings.Contains(output, "retries exhausted") {
		t.Errorf("output should NOT show retry budget for terminal failure, got:\n%s", output)
	}
}

func TestRenderFailedTasks_Cancelled(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		failedTasks: []taskEntry{{
			name:      "cancelled.md",
			title:     "Cancelled task",
			cancelled: true,
		}},
	}

	renderFailedTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "cancelled.md") || !strings.Contains(output, "(cancelled)") {
		t.Fatalf("output should render cancelled task, got:\n%s", output)
	}
}

func TestRenderFailedTasks_TerminalWithRegularFailures(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		failedTasks: []taskEntry{{
			name:                      "mixed.md",
			title:                     "Mixed task",
			maxRetries:                3,
			failureCount:              1,
			lastFailureReason:         "tests failed",
			lastTerminalFailureReason: "invalid glob",
		}},
	}

	renderFailedTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "structural failure: invalid glob") {
		t.Errorf("output should show 'structural failure: invalid glob', got:\n%s", output)
	}
	if !strings.Contains(output, "1/3 retries used") {
		t.Errorf("output should show '1/3 retries used' alongside terminal failure, got:\n%s", output)
	}
	if !strings.Contains(output, "tests failed") {
		t.Errorf("output should show last regular failure reason, got:\n%s", output)
	}
}

func TestRenderFailedTasks_TerminalAndCycleFailure(t *testing.T) {
	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		failedTasks: []taskEntry{{
			name:                      "both.md",
			title:                     "Both task",
			maxRetries:                3,
			lastCycleFailureReason:    "circular dependency",
			lastTerminalFailureReason: "review retry exhaustion",
		}},
	}

	renderFailedTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "structural failure: review retry exhaustion") {
		t.Errorf("output should show terminal reason as primary, got:\n%s", output)
	}
	if !strings.Contains(output, "circular dependency") {
		t.Errorf("output should also mention cycle failure, got:\n%s", output)
	}
}

// Narrow-terminal tests for compact sections.

func TestRenderCompactAgents_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 60 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "abc12345", PID: 1234}},
		presenceMap: map[string]messaging.PresenceInfo{
			"abc12345": {
				Task:   "implement-very-long-feature-name-that-exceeds-terminal-width.md",
				Branch: "task/implement-very-long-feature-name-that-exceeds-terminal-width",
			},
		},
	}

	renderCompactAgents(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow compact agents, got:\n%s", output)
	}
}

func TestRenderCompactAgents_NarrowTerminalWithStageAgeFitsWidth(t *testing.T) {
	const termW = 40
	prev := ui.SetTermWidthFunc(func() int { return termW })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "abc12345", PID: 1234}},
		presenceMap: map[string]messaging.PresenceInfo{
			"abc12345": {
				Task:   "implement-very-long-feature-name-that-exceeds-terminal-width.md",
				Branch: "task/implement-very-long-feature-name-that-exceeds-terminal-width",
			},
		},
		activeProgress: []progressEntry{{
			displayID: "abc12345",
			body:      "Step: WORK",
			task:      "implement-very-long-feature-name-that-exceeds-terminal-width.md",
			ago:       "5m",
		}},
	}

	renderCompactAgents(&buf, c, data)

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.HasPrefix(line, "  ") && utf8.RuneCountInString(line) > termW {
			t.Errorf("data line exceeds terminal width %d: runes=%d, line=%q", termW, utf8.RuneCountInString(line), line)
		}
	}
}

func TestRenderCompactAgents_VeryNarrowTerminalTruncatesAgentID(t *testing.T) {
	tests := []struct {
		name  string
		termW int
	}{
		{"width 12", 12},
		{"width 6", 6},
		{"width 4", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := ui.SetTermWidthFunc(func() int { return tt.termW })
			defer ui.SetTermWidthFunc(prev)

			var buf bytes.Buffer
			c := plainColorSet()
			data := statusData{
				agents: []statusAgent{{ID: "abc12345", PID: 1234}},
				presenceMap: map[string]messaging.PresenceInfo{
					"abc12345": {
						Task:   "some-task.md",
						Branch: "task/some-task",
					},
				},
			}

			renderCompactAgents(&buf, c, data)

			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if strings.HasPrefix(line, "  ") && utf8.RuneCountInString(line) > tt.termW {
					t.Errorf("data line exceeds terminal width %d: runes=%d, line=%q", tt.termW, utf8.RuneCountInString(line), line)
				}
			}
		})
	}
}

func TestRenderCompactAgents_ExtremelyNarrowTerminal(t *testing.T) {
	tests := []struct {
		name  string
		termW int
	}{
		{"width 1", 1},
		{"width 2", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := ui.SetTermWidthFunc(func() int { return tt.termW })
			defer ui.SetTermWidthFunc(prev)

			var buf bytes.Buffer
			c := plainColorSet()
			data := statusData{
				agents: []statusAgent{{ID: "abc12345", PID: 1234}},
				presenceMap: map[string]messaging.PresenceInfo{
					"abc12345": {
						Task:   "some-task.md",
						Branch: "task/some-task",
					},
				},
			}

			renderCompactAgents(&buf, c, data)

			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			for i, line := range lines {
				if i == 0 {
					continue // skip header "Agents (N)"
				}
				if utf8.RuneCountInString(line) > tt.termW {
					t.Errorf("data line exceeds terminal width %d: runes=%d, line=%q",
						tt.termW, utf8.RuneCountInString(line), line)
				}
			}
		})
	}
}

func TestRenderCompactAgents_OverflowSummaryBoundedOnTinyTerminals(t *testing.T) {
	tests := []struct {
		name  string
		termW int
	}{
		{"width 1", 1},
		{"width 2", 2},
		{"width 4", 4},
		{"width 10", 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := ui.SetTermWidthFunc(func() int { return tt.termW })
			defer ui.SetTermWidthFunc(prev)

			var buf bytes.Buffer
			c := plainColorSet()

			// Create more agents than compactListLimit to trigger the overflow summary row.
			agents := make([]statusAgent, 0, compactListLimit+3)
			for i := range compactListLimit + 3 {
				agents = append(agents, statusAgent{ID: "agent-" + intToStr(i), PID: 1000 + i})
			}

			renderCompactAgents(&buf, c, statusData{agents: agents})

			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			wantLines := compactListLimit + 2 // header + compact rows + summary
			if len(lines) != wantLines {
				t.Fatalf("expected %d lines including overflow summary row, got %d lines:\n%s", wantLines, len(lines), buf.String())
			}
			summaryLine := lines[len(lines)-1]
			if utf8.RuneCountInString(summaryLine) == 0 {
				t.Fatalf("expected non-empty overflow summary row, got final line %q", summaryLine)
			}
			if tt.termW >= 4 && !strings.Contains(summaryLine, "…") {
				t.Fatalf("expected truncated overflow summary marker in final line %q", summaryLine)
			}
			for i, line := range lines {
				if i == 0 {
					continue // skip header "Agents (N)"
				}
				if utf8.RuneCountInString(line) > tt.termW {
					t.Errorf("line exceeds terminal width %d: runes=%d, line=%q",
						tt.termW, utf8.RuneCountInString(line), line)
				}
			}
		})
	}
}

func TestRenderCompactNextUp_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 40 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		runnableBacklog: []taskEntry{{
			name:  "implement-very-long-feature-name-that-exceeds-terminal-width.md",
			title: "A very long title that makes the line overflow",
		}},
	}

	renderCompactNextUp(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow compact next up, got:\n%s", output)
	}
}

func TestRenderRecentMessages_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 60 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		recentMessages: []messaging.Message{{
			From:   "abcd1234",
			Type:   "progress",
			Body:   "This is a very long progress message that should be truncated on narrow terminals to prevent overflow",
			SentAt: time.Now().UTC(),
		}},
	}

	renderRecentMessages(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow messages, got:\n%s", output)
	}
}

// Narrow-terminal tests for verbose sections.

func TestRenderActiveAgents_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 60 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "abc12345", PID: 1234}},
		presenceMap: map[string]messaging.PresenceInfo{
			"abc12345": {
				Task:   "implement-very-long-feature-name-that-exceeds-terminal-width.md",
				Branch: "task/implement-very-long-feature-name-that-exceeds-terminal-width",
			},
		},
	}

	renderActiveAgents(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow active agents, got:\n%s", output)
	}
}

func TestRenderRunnableBacklog_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		runnableBacklog: []taskEntry{{
			name:     "implement-very-long-feature-name-exceeds-width.md",
			title:    "This is a very long task title that must be truncated",
			priority: 100,
		}},
	}

	renderRunnableBacklog(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow runnable backlog, got:\n%s", output)
	}
}

func TestRenderReadyForReview_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		readyForReview: []taskEntry{{
			name:   "implement-very-long-feature-name.md",
			title:  "An extremely long title that will overflow narrow terminals easily",
			branch: "task/implement-very-long-feature-name",
		}},
	}

	renderReadyForReview(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow ready-for-review, got:\n%s", output)
	}
}

func TestRenderRecentCompletions_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 60 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		completions: []messaging.CompletionDetail{{
			TaskFile:     "very-long-task-name-with-many-words.md",
			Title:        "An extremely long title that will overflow narrow terminals easily",
			CommitSHA:    "abc1234567890",
			FilesChanged: []string{"a.go", "b.go"},
			MergedAt:     time.Now().UTC().Add(-5 * time.Minute),
		}},
	}

	renderRecentCompletions(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow completions, got:\n%s", output)
	}
}

// Very-narrow-terminal tests (width=20) where the budget goes non-positive.
// Ensures truncation still happens instead of printing unbounded text.

func TestRenderActiveAgents_VeryNarrowTerminalTruncates(t *testing.T) {
	const termW = 20
	prev := ui.SetTermWidthFunc(func() int { return termW })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "agent-abcd1234", PID: 9999}},
		presenceMap: map[string]messaging.PresenceInfo{
			"agent-abcd1234": {
				Task:   "implement-very-long-feature-name-that-exceeds-any-reasonable-width.md",
				Branch: "task/implement-very-long-feature-name-that-exceeds-any-reasonable-width",
			},
		},
	}

	renderActiveAgents(&buf, c, data)
	output := buf.String()

	// At width=20, the fixed prefix (indent + name + PID) already exceeds
	// the budget, so both task and branch are dropped entirely.
	if strings.Contains(output, "implement-very-long") {
		t.Errorf("task should be dropped at very narrow width, got:\n%s", output)
	}
	if strings.Contains(output, "task/implement") {
		t.Errorf("branch should be dropped at very narrow width, got:\n%s", output)
	}

	// Verify every data line fits within the configured terminal width.
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(line, "  ") && utf8.RuneCountInString(line) > termW {
			t.Errorf("data line exceeds terminal width %d: runes=%d, line=%q", termW, utf8.RuneCountInString(line), line)
		}
	}
}

func TestRenderRunnableBacklog_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		runnableBacklog: []taskEntry{{
			name:     "implement-very-long-feature-name-exceeds-width.md",
			title:    "Very long task title that must be truncated",
			priority: 100,
		}},
	}

	renderRunnableBacklog(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

func TestRenderActiveAgents_NarrowTerminalFitsWidth(t *testing.T) {
	const termW = 50
	prev := ui.SetTermWidthFunc(func() int { return termW })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		agents: []statusAgent{{ID: "agent-ab12", PID: 42}},
		presenceMap: map[string]messaging.PresenceInfo{
			"agent-ab12": {
				Task:   "implement-very-long-feature-name-that-exceeds-any-reasonable-width.md",
				Branch: "task/implement-very-long-feature-name-that-exceeds-any-reasonable-width",
			},
		},
	}

	renderActiveAgents(&buf, c, data)
	output := buf.String()

	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "  (") && utf8.RuneCountInString(line) > termW {
			t.Errorf("data line exceeds terminal width %d: runes=%d, line=%q", termW, utf8.RuneCountInString(line), line)
		}
	}
}

func TestRenderAgentProgress_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		activeProgress: []progressEntry{{
			displayID: "agent-abcd1234",
			body:      "Building the entire application from scratch and running all tests",
			task:      "very-long-task.md",
			ago:       "5m",
		}},
	}

	renderAgentProgress(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

func TestRenderRecentMessages_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		recentMessages: []messaging.Message{{
			From:   "abcd1234",
			Type:   "progress",
			Body:   "This is a very long progress message that exceeds any narrow terminal",
			SentAt: time.Now().UTC(),
		}},
	}

	renderRecentMessages(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

func TestRenderRecentCompletions_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		completions: []messaging.CompletionDetail{{
			TaskFile:     "very-long-task-name-with-many-words.md",
			Title:        "An extremely long title that overflows",
			CommitSHA:    "abc1234567890",
			FilesChanged: []string{"a.go", "b.go"},
			MergedAt:     time.Now().UTC().Add(-5 * time.Minute),
		}},
	}

	renderRecentCompletions(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

func TestRenderReadyForReview_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		readyForReview: []taskEntry{{
			name:   "implement-very-long-feature-name.md",
			title:  "An extremely long title that will overflow",
			branch: "task/implement-very-long-feature-name",
		}},
	}

	renderReadyForReview(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

func TestRenderCompactNextUp_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		runnableBacklog: []taskEntry{{
			name:  "implement-very-long-feature-name-exceeds-width.md",
			title: "A title too long for twenty columns",
		}},
	}

	renderCompactNextUp(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

// Narrow-terminal tests for the remaining verbose sections.

func TestRenderInProgressTasks_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		inProgressTasks: []taskEntry{{
			name:      "implement-very-long-feature-name-that-exceeds-terminal-width.md",
			title:     "An extremely long title that overflows any narrow terminal",
			claimedBy: "abc12345",
			claimedAt: time.Now().UTC().Add(-10 * time.Minute),
		}},
	}

	renderInProgressTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow in-progress tasks, got:\n%s", output)
	}
	if !strings.Contains(output, "agent abc12345") {
		t.Errorf("should still contain agent info, got:\n%s", output)
	}
}

func TestRenderReadyToMerge_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		readyToMerge: []taskEntry{{
			name:     "implement-very-long-feature-name-that-exceeds-terminal-width.md",
			title:    "An extremely long title that overflows narrow terminals easily",
			priority: 100,
		}},
	}

	renderReadyToMerge(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow ready-to-merge, got:\n%s", output)
	}
	if !strings.Contains(output, "priority 100") {
		t.Errorf("should still contain priority suffix, got:\n%s", output)
	}
}

func TestRenderDependencyBlocked_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		waitingTasks: []waitingTaskSummary{{
			Name:  "implement-very-long-feature-name-that-exceeds-terminal-width.md",
			Title: "An extremely long title",
			State: dirs.Waiting,
			Dependencies: []waitingDep{
				{ID: "very-long-dependency-task-name-that-also-overflows", Status: "backlog"},
			},
		}},
	}

	renderDependencyBlocked(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow dependency-blocked, got:\n%s", output)
	}
}

func TestRenderConflictDeferred_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		deferredDetail: map[string]queue.DeferralInfo{
			"my-deferred-task.md": {
				BlockedBy:          "some-very-long-blocking-task-name-that-exceeds-terminal-width.md",
				BlockedByDir:       "in-progress",
				ConflictingAffects: []string{"internal/very/long/path/to/file.go", "internal/another/very/long/path/to/file.go", "internal/yet/another/long/path.go"},
			},
		},
	}

	renderConflictDeferred(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow conflict-deferred, got:\n%s", output)
	}
}

func TestRenderFailedTasks_NarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 50 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		failedTasks: []taskEntry{{
			name:              "implement-very-long-feature-name-that-exceeds-terminal-width.md",
			title:             "An extremely long title that overflows narrow terminals easily",
			failureCount:      3,
			maxRetries:        5,
			lastFailureReason: "build failed with a very long error description that goes on and on",
		}},
	}

	renderFailedTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow failed tasks, got:\n%s", output)
	}
	if !strings.Contains(output, "retries") {
		t.Errorf("should still contain retry info, got:\n%s", output)
	}
}

// Very-narrow tests (width=20) for the remaining verbose sections.

func TestRenderInProgressTasks_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		inProgressTasks: []taskEntry{{
			name:      "implement-very-long-feature-name.md",
			title:     "A very long title",
			claimedBy: "abc12345",
			claimedAt: time.Now().UTC().Add(-5 * time.Minute),
		}},
	}

	renderInProgressTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}

func TestRenderFailedTasks_VeryNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 20 })
	defer ui.SetTermWidthFunc(prev)

	var buf bytes.Buffer
	c := plainColorSet()
	data := statusData{
		failedTasks: []taskEntry{{
			name:              "implement-very-long-feature-name.md",
			title:             "Very long title here",
			failureCount:      2,
			maxRetries:        3,
			lastFailureReason: "build failed",
		}},
	}

	renderFailedTasks(&buf, c, data)
	output := buf.String()

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker at very narrow width, got:\n%s", output)
	}
}
