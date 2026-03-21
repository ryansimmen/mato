package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/process"
)

var claimedByRe = regexp.MustCompile(`<!-- claimed-by:\s*(\S+)`)

// HasAvailableTasks reports whether there is at least one claimable .md task
// file in backlog/ that is not in the deferred exclusion set.
func HasAvailableTasks(tasksDir string, deferred map[string]struct{}) bool {
	entries, err := os.ReadDir(filepath.Join(tasksDir, DirBacklog))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if deferred != nil {
			if _, excluded := deferred[e.Name()]; excluded {
				continue
			}
		}
		return true
	}
	return false
}

// RegisterAgent writes a lock file containing "PID:starttime" so concurrent
// mato instances can detect PID reuse. Falls back to PID-only when start time
// is unavailable (non-Linux). Returns a cleanup function.
func RegisterAgent(tasksDir, agentID string) (func(), error) {
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create locks dir: %w", err)
	}
	lockFile := filepath.Join(locksDir, agentID+".pid")
	identity := process.LockIdentity(os.Getpid())
	if err := os.WriteFile(lockFile, []byte(identity), 0o644); err != nil {
		return nil, fmt.Errorf("write agent lock: %w", err)
	}
	return func() { os.Remove(lockFile) }, nil
}

// ParseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func ParseClaimedBy(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := claimedByRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// CleanStaleLocks removes lock files for agents that are no longer running.
func CleanStaleLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, ".locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".pid")
		if !identity.IsAgentActive(tasksDir, agentID) {
			os.Remove(filepath.Join(locksDir, e.Name()))
		}
	}
}

// AcquireReviewLock attempts to acquire an exclusive lock for reviewing a
// specific task file. Returns a cleanup function and true if acquired, or
// nil and false if the lock is already held by a live process.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireReviewLock(tasksDir, taskFilename string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Acquire(locksDir, "review-"+taskFilename)
}

// CleanStaleReviewLocks removes review lock files for processes that are no
// longer running, so that review tasks are not permanently blocked by dead
// agents.
func CleanStaleReviewLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, ".locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(locksDir, e.Name()))
		if err != nil {
			continue
		}
		if !process.IsLockHolderAlive(strings.TrimSpace(string(data))) {
			os.Remove(filepath.Join(locksDir, e.Name()))
		}
	}
}

// RecoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
// If the same task already exists in a later-state directory, the
// in-progress copy is treated as stale and removed instead of recovered.
func RecoverOrphanedTasks(tasksDir string) {
	inProgress := filepath.Join(tasksDir, DirInProgress)
	entries, err := os.ReadDir(inProgress)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		src := filepath.Join(inProgress, e.Name())

		if laterDir := laterStateDuplicateDir(tasksDir, e.Name()); laterDir != "" {
			if err := os.Remove(src); err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "warning: could not remove stale in-progress copy %s: %v\n", e.Name(), err)
				}
				continue
			}
			fmt.Printf("Removing stale in-progress copy of %s (already in %s/)\n", e.Name(), laterDir)
			continue
		}

		if agent := ParseClaimedBy(src); agent != "" && identity.IsAgentActive(tasksDir, agent) {
			fmt.Printf("Skipping in-progress task %s (agent %s still active)\n", e.Name(), agent)
			continue
		}

		dst := filepath.Join(tasksDir, DirBacklog, e.Name())
		if err := safeRename(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not recover orphaned task %s: %v\n", e.Name(), err)
			continue
		}

		f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not open task file to append failure record for %s: %v\n", e.Name(), err)
		} else {
			_, writeErr := fmt.Fprintf(f, "\n<!-- failure: mato-recovery at %s — agent was interrupted -->\n",
				time.Now().UTC().Format(time.RFC3339))
			closeErr := f.Close()
			if writeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", e.Name(), writeErr)
			} else if closeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", e.Name(), closeErr)
			}
		}

		fmt.Printf("Recovered orphaned task %s back to backlog\n", e.Name())
	}
}

func laterStateDuplicateDir(tasksDir, name string) string {
	for _, laterDir := range []string{DirReadyReview, DirReadyMerge, DirCompleted, DirFailed} {
		if _, err := os.Stat(filepath.Join(tasksDir, laterDir, name)); err == nil {
			return laterDir
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not check %s for duplicate %s: %v\n", laterDir, name, err)
		}
	}
	return ""
}

func ReconcileReadyQueue(tasksDir string) int {
	completedIDs := completedTaskIDs(tasksDir)
	nonCompletedIDs := nonCompletedTaskIDs(tasksDir)

	// Remove ambiguous IDs: if an ID appears in both completed and
	// non-completed directories, we cannot safely assume the dependency
	// is satisfied — it may refer to the non-completed copy.
	for id := range nonCompletedIDs {
		if _, dup := completedIDs[id]; dup {
			fmt.Fprintf(os.Stderr, "warning: task ID %q exists in both completed and non-completed directories; dependency on it will not be satisfied\n", id)
			delete(completedIDs, id)
		}
	}

	knownIDs := allKnownTaskIDs(tasksDir)
	waitingDir := filepath.Join(tasksDir, DirWaiting)
	entries, err := os.ReadDir(waitingDir)
	if err != nil {
		return 0
	}

	type waitingTask struct {
		name string
		path string
		meta frontmatter.TaskMeta
	}

	waitingTasks := make([]waitingTask, 0, len(entries))
	waitingDeps := make(map[string][]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}

		path := filepath.Join(waitingDir, e.Name())
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: moving unparseable waiting task %s to failed/: %v\n", e.Name(), err)
			failedPath := filepath.Join(tasksDir, DirFailed, e.Name())
			if moveErr := safeRename(path, failedPath); moveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", e.Name(), moveErr)
			}
			continue
		}

		waitingTasks = append(waitingTasks, waitingTask{name: e.Name(), path: path, meta: meta})
		waitingDeps[meta.ID] = meta.DependsOn
	}

	promoted := 0
	loggedCircularDeps := make(map[string]struct{})
	for _, task := range waitingTasks {
		ready := true
		for _, dep := range task.meta.DependsOn {
			if dep == task.meta.ID {
				fmt.Fprintf(os.Stderr, "warning: task %s depends on itself\n", task.meta.ID)
				ready = false
				continue
			}
			if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, task.meta.ID, waitingDeps, map[string]struct{}{}) {
				logCircularDependency(loggedCircularDeps, task.meta.ID, dep)
				ready = false
				continue
			}
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			if _, ok := knownIDs[dep]; !ok {
				fmt.Fprintf(os.Stderr, "warning: waiting task %s depends on unknown task ID %q (not found in any queue directory)\n", task.name, dep)
			}
			ready = false
		}
		if !ready {
			continue
		}
		if hasActiveOverlap(tasksDir, task.meta.Affects) {
			continue
		}

		dst := filepath.Join(tasksDir, DirBacklog, task.name)
		if err := safeRename(task.path, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not promote waiting task %s: %v\n", task.name, err)
			continue
		}
		promoted++
	}

	return promoted
}

// CountPromotableWaitingTasks is a read-only variant of ReconcileReadyQueue.
// It returns the number of waiting tasks whose dependencies are satisfied and
// would be promoted, without actually moving any files.
func CountPromotableWaitingTasks(tasksDir string) int {
	completedIDs := completedTaskIDs(tasksDir)
	nonCompletedIDs := nonCompletedTaskIDs(tasksDir)

	for id := range nonCompletedIDs {
		if _, dup := completedIDs[id]; dup {
			delete(completedIDs, id)
		}
	}

	knownIDs := allKnownTaskIDs(tasksDir)
	waitingDir := filepath.Join(tasksDir, DirWaiting)
	entries, err := os.ReadDir(waitingDir)
	if err != nil {
		return 0
	}

	type waitingTask struct {
		meta frontmatter.TaskMeta
	}

	waitingDeps := make(map[string][]string)
	var tasks []waitingTask
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(waitingDir, e.Name())
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			continue
		}
		tasks = append(tasks, waitingTask{meta: meta})
		waitingDeps[meta.ID] = meta.DependsOn
	}

	count := 0
	for _, task := range tasks {
		ready := true
		for _, dep := range task.meta.DependsOn {
			if dep == task.meta.ID {
				ready = false
				continue
			}
			if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, task.meta.ID, waitingDeps, map[string]struct{}{}) {
				ready = false
				continue
			}
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			_ = knownIDs // suppress unused warning; matches ReconcileReadyQueue logic
			ready = false
		}
		if !ready {
			continue
		}
		if hasActiveOverlap(tasksDir, task.meta.Affects) {
			continue
		}
		count++
	}
	return count
}

// ComputeQueueManifest returns the queue manifest content as a string without
// writing it to disk. This is the read-only equivalent of WriteQueueManifest.
// It returns an error if the backlog directory cannot be read.
func ComputeQueueManifest(tasksDir string, exclude map[string]struct{}) (string, error) {
	entries, err := os.ReadDir(filepath.Join(tasksDir, DirBacklog))
	if err != nil {
		return "", err
	}

	queueEntries := make([]queueEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if exclude != nil {
			if _, excluded := exclude[e.Name()]; excluded {
				continue
			}
		}
		meta, _, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, DirBacklog, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for queue manifest: %v\n", e.Name(), err)
			continue
		}
		queueEntries = append(queueEntries, queueEntry{name: e.Name(), priority: meta.Priority})
	}

	sort.Slice(queueEntries, func(i, j int) bool {
		if queueEntries[i].priority != queueEntries[j].priority {
			return queueEntries[i].priority < queueEntries[j].priority
		}
		return queueEntries[i].name < queueEntries[j].name
	})

	lines := make([]string, 0, len(queueEntries))
	for _, entry := range queueEntries {
		lines = append(lines, entry.name)
	}
	manifest := strings.Join(lines, "\n")
	if manifest != "" {
		manifest += "\n"
	}
	return manifest, nil
}

func completedTaskIDs(tasksDir string) map[string]struct{} {
	completedDir := filepath.Join(tasksDir, DirCompleted)
	entries, err := os.ReadDir(completedDir)
	if err != nil {
		return map[string]struct{}{}
	}

	ids := make(map[string]struct{}, len(entries)*2)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		stem := frontmatter.TaskFileStem(e.Name())
		ids[stem] = struct{}{}
		path := filepath.Join(completedDir, e.Name())
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse completed task %s: %v\n", e.Name(), err)
			continue
		}
		ids[meta.ID] = struct{}{}
	}
	return ids
}

// nonCompletedTaskIDs returns the set of task IDs found in all directories except completed/.
func nonCompletedTaskIDs(tasksDir string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, dir := range []string{DirWaiting, DirBacklog, DirInProgress, DirReadyReview, DirReadyMerge, DirFailed} {
		entries, err := os.ReadDir(filepath.Join(tasksDir, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			ids[frontmatter.TaskFileStem(e.Name())] = struct{}{}
			path := filepath.Join(tasksDir, dir, e.Name())
			if meta, _, err := frontmatter.ParseTaskFile(path); err == nil {
				ids[meta.ID] = struct{}{}
			}
		}
	}
	return ids
}

// allKnownTaskIDs returns the set of task IDs found across all queue directories.
func allKnownTaskIDs(tasksDir string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, dir := range AllDirs {
		entries, err := os.ReadDir(filepath.Join(tasksDir, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			ids[frontmatter.TaskFileStem(e.Name())] = struct{}{}
			path := filepath.Join(tasksDir, dir, e.Name())
			if meta, _, err := frontmatter.ParseTaskFile(path); err == nil {
				ids[meta.ID] = struct{}{}
			}
		}
	}
	return ids
}

type queueEntry struct {
	name     string
	priority int
}

type backlogTask struct {
	name     string
	dir      string // source directory (e.g., "backlog", "in-progress", "ready-to-merge")
	path     string
	priority int
	affects  []string
}

func collectActiveAffects(tasksDir string) []backlogTask {
	var active []backlogTask
	for _, dir := range []string{DirInProgress, DirReadyReview, DirReadyMerge} {
		dirPath := filepath.Join(tasksDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dirPath, e.Name())
			meta, _, err := frontmatter.ParseTaskFile(path)
			if err != nil {
				continue
			}
			if len(meta.Affects) == 0 {
				continue
			}
			active = append(active, backlogTask{
				name:     e.Name(),
				dir:      dir,
				path:     path,
				priority: 0,
				affects:  meta.Affects,
			})
		}
	}
	return active
}

func hasActiveOverlap(tasksDir string, affects []string) bool {
	if len(affects) == 0 {
		return false
	}
	// Only check in-progress, ready-for-review, and ready-to-merge — these represent
	// tasks that are actively being worked on, under review, or awaiting merge.
	// We intentionally exclude backlog/
	// because DeferredOverlappingTasks handles backlog-vs-backlog conflicts with
	// proper priority ordering. Including backlog here would cause priority
	// inversion: a high-priority waiting task would be blocked by a lower-priority
	// backlog task that hasn't even been claimed yet.
	for _, dir := range []string{DirInProgress, DirReadyReview, DirReadyMerge} {
		dirPath := filepath.Join(tasksDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			meta, _, err := frontmatter.ParseTaskFile(filepath.Join(dirPath, e.Name()))
			if err != nil {
				continue
			}
			if len(overlappingAffects(affects, meta.Affects)) > 0 {
				return true
			}
		}
	}
	return false
}

// DeferredOverlappingTasks returns the set of backlog task filenames that should
// be excluded from the queue because they conflict with higher-priority backlog
// tasks or active tasks in in-progress/ready-for-review/ready-to-merge. Tasks remain in backlog/
// (no file movement) to avoid churn between waiting/ and backlog/.
// DeferralInfo describes why a task was excluded from the runnable queue.
type DeferralInfo struct {
	BlockedBy    string   // name of the conflicting task
	BlockedByDir string   // directory of the conflicting task (e.g., "in-progress", "backlog")
	OverlapFiles []string // files both tasks claim in affects
}

func DeferredOverlappingTasks(tasksDir string) map[string]struct{} {
	detailed := DeferredOverlappingTasksDetailed(tasksDir)
	simple := make(map[string]struct{}, len(detailed))
	for name := range detailed {
		simple[name] = struct{}{}
	}
	return simple
}

// DeferredOverlappingTasksDetailed returns deferred tasks with the reason for deferral.
func DeferredOverlappingTasksDetailed(tasksDir string) map[string]DeferralInfo {
	deferred := make(map[string]DeferralInfo)
	backlogDir := filepath.Join(tasksDir, DirBacklog)
	entries, err := os.ReadDir(backlogDir)
	if err != nil {
		return deferred
	}

	tasks := make([]backlogTask, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(backlogDir, entry.Name())
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for overlap detection: %v\n", entry.Name(), err)
			continue
		}
		tasks = append(tasks, backlogTask{
			name:     entry.Name(),
			path:     path,
			priority: meta.Priority,
			affects:  meta.Affects,
		})
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})

	activeAffects := collectActiveAffects(tasksDir)
	kept := make([]backlogTask, 0, len(tasks)+len(activeAffects))
	kept = append(kept, activeAffects...)
	for _, task := range tasks {
		isDef := false
		for _, other := range kept {
			overlap := overlappingAffects(task.affects, other.affects)
			if len(overlap) > 0 {
				blockedByDir := other.dir
				if blockedByDir == "" {
					blockedByDir = DirBacklog
				}
				deferred[task.name] = DeferralInfo{
					BlockedBy:    other.name,
					BlockedByDir: blockedByDir,
					OverlapFiles: overlap,
				}
				isDef = true
				break
			}
		}
		if !isDef {
			task.dir = DirBacklog
			kept = append(kept, task)
		}
	}

	return deferred
}

func overlappingAffects(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(a))
	for _, item := range a {
		if item == "" {
			continue
		}
		seen[item] = struct{}{}
	}

	overlap := make([]string, 0)
	added := make(map[string]struct{})
	for _, item := range b {
		if _, ok := seen[item]; !ok {
			continue
		}
		if _, ok := added[item]; ok {
			continue
		}
		added[item] = struct{}{}
		overlap = append(overlap, item)
	}
	sort.Strings(overlap)
	return overlap
}

// WriteQueueManifest computes the queue manifest via ComputeQueueManifest
// and atomically writes it to the .queue file in tasksDir.
func WriteQueueManifest(tasksDir string, exclude map[string]struct{}) error {
	manifest, err := ComputeQueueManifest(tasksDir, exclude)
	if err != nil {
		return err
	}
	return atomicwrite.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest))
}

func safeRename(src, dst string) error {
	// Use os.Link + os.Remove instead of os.Stat + os.Rename to eliminate
	// the TOCTOU race window. os.Link fails atomically with EEXIST if the
	// destination already exists.
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("destination already exists: %s", dst)
		}
		// Cross-device link (EXDEV): the source and destination are on
		// different filesystems. Falling back to Stat+Rename would
		// reintroduce the same TOCTOU race this function exists to
		// prevent, so we return an actionable error instead.
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("cannot atomically move %s to %s: cross-device link (source and destination must be on the same filesystem)", src, dst)
		}
		return fmt.Errorf("link %s to %s: %w", src, dst, err)
	}
	// Link succeeded; remove the original.
	if err := os.Remove(src); err != nil {
		// The move is logically complete (dst exists), so warn but don't fail.
		fmt.Fprintf(os.Stderr, "warning: could not remove source %s after linking to %s: %v\n", src, dst, err)
	}
	return nil
}

func dependsOnWaitingTask(taskID, targetID string, waitingDeps map[string][]string, visited map[string]struct{}) bool {
	if taskID == targetID {
		return true
	}
	if _, ok := visited[taskID]; ok {
		return false
	}
	visited[taskID] = struct{}{}
	for _, dep := range waitingDeps[taskID] {
		if dep == targetID {
			return true
		}
		if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, targetID, waitingDeps, visited) {
			return true
		}
	}
	return false
}

func logCircularDependency(logged map[string]struct{}, a, b string) {
	if a > b {
		a, b = b, a
	}
	key := a + "\x00" + b
	if _, ok := logged[key]; ok {
		return
	}
	logged[key] = struct{}{}
	fmt.Fprintf(os.Stderr, "warning: circular dependency detected between %s and %s\n", a, b)
}
