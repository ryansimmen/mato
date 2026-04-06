package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/process"
	"mato/internal/queue"
)

// ---------- G. Hygiene (Messages, Merge Lock, Temp Files) ----------

// messageRetention is the maximum age for event message files.
const messageRetention = 24 * time.Hour

// tempFileMaxAge is the minimum age before leftover temp files are eligible
// for removal in --fix mode.
const tempFileMaxAge = 1 * time.Hour

func checkHygiene(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "hygiene", Status: CheckRan, Findings: []Finding{}}

	cr.Findings = append(cr.Findings, scanOrphanedMessages(cc.tasksDir, cc.opts.Fix)...)
	cr.Findings = append(cr.Findings, scanStaleMergeLock(cc.tasksDir, cc.opts.Fix)...)
	cr.Findings = append(cr.Findings, scanLeftoverTempFiles(cc.tasksDir, cc.opts.Fix)...)
	cr.Findings = append(cr.Findings, scanPauseSentinel(cc.tasksDir, pause.Read)...)

	return cr
}

func scanPauseSentinel(tasksDir string, readFn func(string) (pause.State, error)) []Finding {
	state, err := readFn(tasksDir)
	if err != nil {
		return []Finding{{
			Code:     "hygiene.pause_unreadable",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("cannot read pause sentinel: %v", err),
			Path:     filepath.Join(tasksDir, ".paused"),
		}}
	}
	if !state.Active {
		return nil
	}
	if state.ProblemKind == pause.ProblemMalformed {
		return []Finding{{
			Code:     "hygiene.invalid_pause_file",
			Severity: SeverityWarning,
			Message:  state.Problem,
			Path:     filepath.Join(tasksDir, ".paused"),
		}}
	}
	if state.ProblemKind == pause.ProblemUnreadable {
		return []Finding{{
			Code:     "hygiene.pause_unreadable",
			Severity: SeverityWarning,
			Message:  state.Problem,
			Path:     filepath.Join(tasksDir, ".paused"),
		}}
	}
	rawAge := time.Since(state.Since)
	if rawAge <= 24*time.Hour {
		return nil
	}
	age := rawAge.Round(time.Hour)
	return []Finding{{
		Code:     "hygiene.paused",
		Severity: SeverityWarning,
		Message:  fmt.Sprintf("queue has been paused since %s (%s ago)", state.Since.Format(time.RFC3339), age),
		Path:     filepath.Join(tasksDir, ".paused"),
	}}
}

// scanOrphanedMessages checks for event message files older than the
// 24-hour retention window.
func scanOrphanedMessages(tasksDir string, fix bool) []Finding {
	var findings []Finding

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return findings
		}
		findings = append(findings, Finding{
			Code:     "hygiene.events_unreadable",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("cannot read events directory: %v", err),
			Path:     eventsDir,
		})
		return findings
	}

	cutoff := time.Now().UTC().Add(-messageRetention)
	var staleFiles []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			staleFiles = append(staleFiles, entry.Name())
		}
	}

	if len(staleFiles) == 0 {
		return findings
	}

	f := Finding{
		Code:     "hygiene.stale_events",
		Severity: SeverityWarning,
		Message:  fmt.Sprintf("%d event message(s) older than 24h retention window", len(staleFiles)),
		Path:     eventsDir,
		Fixable:  true,
	}

	if fix {
		messaging.CleanOldMessages(tasksDir, messageRetention)
		// Re-scan to verify cleanup.
		remaining := 0
		if recheck, err := os.ReadDir(eventsDir); err == nil {
			for _, e := range recheck {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				if info.ModTime().Before(cutoff) {
					remaining++
				}
			}
		}
		if remaining == 0 {
			f.Fixed = true
			f.Fixable = false
		}
	}

	findings = append(findings, f)
	return findings
}

// scanStaleMergeLock checks whether .mato/.locks/merge.lock is held by
// a dead process.
func scanStaleMergeLock(tasksDir string, fix bool) []Finding {
	var findings []Finding

	mergeLockPath := filepath.Join(tasksDir, ".locks", "merge.lock")
	data, err := os.ReadFile(mergeLockPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No merge.lock is the normal case.
			return findings
		}
		return []Finding{{
			Code:     "hygiene.merge_lock_unreadable",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("cannot read merge.lock: %v", err),
			Path:     mergeLockPath,
		}}
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		// Empty lock file is stale.
		f := Finding{
			Code:     "hygiene.stale_merge_lock",
			Severity: SeverityWarning,
			Message:  "merge.lock exists but is empty",
			Path:     mergeLockPath,
			Fixable:  true,
		}
		if fix {
			if err := os.Remove(mergeLockPath); err != nil && !os.IsNotExist(err) {
				f.Message += fmt.Sprintf(" (fix failed: %v)", err)
			}
			if _, statErr := os.Stat(mergeLockPath); os.IsNotExist(statErr) {
				f.Fixed = true
				f.Fixable = false
			}
		}
		findings = append(findings, f)
		return findings
	}

	if !process.IsLockHolderAlive(content) {
		f := Finding{
			Code:     "hygiene.stale_merge_lock",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("merge.lock held by dead process (%s)", content),
			Path:     mergeLockPath,
			Fixable:  true,
		}
		if fix {
			if err := os.Remove(mergeLockPath); err != nil && !os.IsNotExist(err) {
				f.Message += fmt.Sprintf(" (fix failed: %v)", err)
			}
			if _, statErr := os.Stat(mergeLockPath); os.IsNotExist(statErr) {
				f.Fixed = true
				f.Fixable = false
			}
		}
		findings = append(findings, f)
	}

	return findings
}

// tempFilePatterns matches leftover atomic-write temp files produced by
// the primary path (.*.tmp-*) the EXDEV cross-device fallback (.*.xdev-*)
// in internal/atomicwrite, and retry temp files (.*.retry-*) from
// queue.RetryTask.
var tempFilePatterns = []string{".tmp-", ".xdev-", queue.RetryTempInfix}

// isTempFile reports whether name matches one of the known temp file
// patterns: it must start with "." and contain ".tmp-", ".xdev-", or
// ".retry-".
func isTempFile(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	for _, p := range tempFilePatterns {
		if strings.Contains(name, p) {
			return true
		}
	}
	return false
}

// scanLeftoverTempFiles scans queue and message directories for leftover
// temp files matching .*.tmp-*, .*.xdev-*, and .*.retry-* patterns.
func scanLeftoverTempFiles(tasksDir string, fix bool) []Finding {
	var findings []Finding

	// Directories to scan for temp files.
	scanDirs := make([]string, 0, len(dirs.All)+len(messaging.MessagingDirs))
	for _, d := range dirs.All {
		scanDirs = append(scanDirs, filepath.Join(tasksDir, d))
	}
	for _, d := range messaging.MessagingDirs {
		scanDirs = append(scanDirs, filepath.Join(tasksDir, d))
	}

	now := time.Now().UTC()
	type tempFile struct {
		path string
		age  time.Duration
	}
	var tempFiles []tempFile

	for _, dir := range scanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !isTempFile(name) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			tempFiles = append(tempFiles, tempFile{
				path: filepath.Join(dir, name),
				age:  now.Sub(info.ModTime()),
			})
		}
	}

	if len(tempFiles) == 0 {
		return findings
	}

	f := Finding{
		Code:     "hygiene.leftover_temp_files",
		Severity: SeverityWarning,
		Message:  fmt.Sprintf("%d leftover temp file(s) found", len(tempFiles)),
		Fixable:  true,
	}

	if fix {
		removed := 0
		for _, tf := range tempFiles {
			if tf.age >= tempFileMaxAge {
				if err := os.Remove(tf.path); err == nil || os.IsNotExist(err) {
					removed++
				}
			}
		}
		if removed == len(tempFiles) {
			f.Fixed = true
			f.Fixable = false
		} else if removed > 0 {
			f.Message = fmt.Sprintf("%d leftover temp file(s) found, %d removed (remaining are less than 1h old)", len(tempFiles), removed)
		}
	}

	findings = append(findings, f)
	return findings
}
