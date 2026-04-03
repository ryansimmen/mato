package taskfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// verdictPayload mirrors the JSON structure written by the review agent.
type verdictPayload struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// VerdictRejection holds the rejection reason and approximate timestamp
// from a preserved verdict file.
type VerdictRejection struct {
	Reason    string
	Timestamp time.Time
}

// ReadVerdictRejection reads the preserved verdict JSON file for the given
// task filename and returns the rejection details. The timestamp is derived
// from the file's modification time since the verdict JSON does not contain
// one. Returns ok=false if the file does not exist, is not a rejection, or
// cannot be parsed.
func ReadVerdictRejection(tasksDir, filename string) (VerdictRejection, bool) {
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+filename+".json")
	info, err := os.Stat(verdictPath)
	if err != nil {
		return VerdictRejection{}, false
	}
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		return VerdictRejection{}, false
	}
	var v verdictPayload
	if err := json.Unmarshal(data, &v); err != nil {
		return VerdictRejection{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(v.Verdict), "reject") || strings.TrimSpace(v.Reason) == "" {
		return VerdictRejection{}, false
	}
	return VerdictRejection{
		Reason:    v.Reason,
		Timestamp: info.ModTime().UTC(),
	}, true
}
