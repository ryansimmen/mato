package integration_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"mato/internal/pause"
	"mato/internal/status"
	"mato/internal/testutil"
)

func TestPauseResume_StatusReflectsPauseState(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	if _, err := pause.Pause(tasksDir, time.Now().UTC()); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	var buf bytes.Buffer
	if err := status.ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}
	var result status.StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !result.Paused.Active {
		t.Fatal("expected paused.active=true after pause")
	}

	buf.Reset()
	if _, err := pause.Resume(tasksDir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := status.ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if result.Paused.Active {
		t.Fatal("expected paused.active=false after resume")
	}
}
