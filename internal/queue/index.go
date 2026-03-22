package queue

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/frontmatter"
	"mato/internal/taskfile"
)

// TaskSnapshot holds all metadata extracted from a single task file during
// index build. The raw file bytes are read once and all metadata is parsed
// from them, eliminating redundant I/O across consumers.
type TaskSnapshot struct {
	Filename           string
	State              string // directory name: "waiting", "backlog", etc.
	Path               string // full filesystem path
	Meta               frontmatter.TaskMeta
	Body               string
	Branch             string // from <!-- branch: ... --> comment, "" if absent
	FailureCount       int    // <!-- failure: ... --> markers (excluding review-failure)
	ReviewFailureCount int    // <!-- review-failure: ... --> markers
}

// ParseFailure records a task file that could not be parsed during index build.
type ParseFailure struct {
	Filename string
	State    string // directory the file was found in
	Path     string
	Err      error
}

// BuildWarning records a non-fatal filesystem warning encountered while
// scanning a queue directory.
type BuildWarning struct {
	State string
	Path  string
	Err   error
}

// PollIndex is an in-memory snapshot of all task files across the queue
// directories. It is built once per poll cycle by scanning each directory
// and reading each file exactly once. All consumers query the index instead
// of performing independent filesystem scans.
//
// PollIndex is a plain struct with no concurrency protection. Each agent
// process runs in a separate terminal; cross-process safety is handled by
// existing atomic filesystem operations (AtomicMove via os.Link + os.Remove).
type PollIndex struct {
	// tasks maps "state/filename" to the parsed snapshot.
	tasks map[string]*TaskSnapshot

	// byState maps directory name to the list of snapshots in that directory.
	byState map[string][]*TaskSnapshot

	// completedIDs is the set of task IDs (both filename stems and
	// frontmatter IDs) found in completed/.
	completedIDs map[string]struct{}

	// nonCompletedIDs is the set of task IDs found in all directories
	// except completed/.
	nonCompletedIDs map[string]struct{}

	// allIDs is the set of task IDs found across all queue directories.
	allIDs map[string]struct{}

	// activeAffects maps each affects path to the list of task filenames
	// (in active dirs) that declare it.
	activeAffects map[string][]string

	// activeAffectsPrefixes holds affects entries from active dirs that
	// end with "/" (directory prefixes). Stored separately so
	// HasActiveOverlap can check prefix relationships without scanning
	// the full activeAffects map on every call.
	activeAffectsPrefixes []string

	// activeBranches is the set of branch names in active directories
	// (in-progress, ready-for-review, ready-to-merge).
	activeBranches map[string]struct{}

	// parseFailures records files that failed to parse during build.
	parseFailures []ParseFailure

	// buildWarnings records non-fatal filesystem issues encountered while
	// scanning directories.
	buildWarnings []BuildWarning
}

// activeDirs are the directories representing tasks actively being worked on.
var activeDirs = []string{DirInProgress, DirReadyReview, DirReadyMerge}

// nonCompletedDirs are all directories except completed/.
var nonCompletedDirs = []string{DirWaiting, DirBacklog, DirInProgress, DirReadyReview, DirReadyMerge, DirFailed}

// BuildIndex scans all queue directories under tasksDir and reads each task
// file exactly once, building an in-memory index. Returns a fully populated
// PollIndex.
func BuildIndex(tasksDir string) *PollIndex {
	idx := &PollIndex{
		tasks:           make(map[string]*TaskSnapshot),
		byState:         make(map[string][]*TaskSnapshot, len(AllDirs)),
		completedIDs:    make(map[string]struct{}),
		nonCompletedIDs: make(map[string]struct{}),
		allIDs:          make(map[string]struct{}),
		activeAffects:   make(map[string][]string),
		activeBranches:  make(map[string]struct{}),
	}
	activeAffectsPrefixSet := make(map[string]struct{})

	isActive := make(map[string]bool, len(activeDirs))
	for _, d := range activeDirs {
		isActive[d] = true
	}
	isNonCompleted := make(map[string]bool, len(nonCompletedDirs))
	for _, d := range nonCompletedDirs {
		isNonCompleted[d] = true
	}

	for _, dir := range AllDirs {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := ListTaskFiles(dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Directory may not exist yet (e.g. first run). Skip.
				continue
			}
			idx.buildWarnings = append(idx.buildWarnings, BuildWarning{State: dir, Path: dirPath, Err: err})
			continue
		}

		snapshots := make([]*TaskSnapshot, 0, len(names))
		for _, name := range names {
			path := filepath.Join(dirPath, name)

			// Always register the filename stem so ID-based
			// dependency lookups work even when frontmatter is
			// malformed.  meta.ID is added only on successful
			// parse below.
			stem := frontmatter.TaskFileStem(name)
			idx.allIDs[stem] = struct{}{}
			if dir == DirCompleted {
				idx.completedIDs[stem] = struct{}{}
			}
			if isNonCompleted[dir] {
				idx.nonCompletedIDs[stem] = struct{}{}
			}

			data, err := os.ReadFile(path)
			if err != nil {
				idx.parseFailures = append(idx.parseFailures, ParseFailure{
					Filename: name, State: dir, Path: path, Err: err,
				})
				continue
			}

			branch, _ := taskfile.ParseBranchComment(data)

			meta, body, err := frontmatter.ParseTaskData(data, path)
			if err != nil {
				idx.parseFailures = append(idx.parseFailures, ParseFailure{
					Filename: name, State: dir, Path: path, Err: err,
				})
				if isActive[dir] && branch != "" {
					idx.activeBranches[branch] = struct{}{}
				}
				continue
			}

			snap := &TaskSnapshot{
				Filename:           name,
				State:              dir,
				Path:               path,
				Meta:               meta,
				Body:               body,
				Branch:             branch,
				FailureCount:       taskfile.CountFailureMarkers(data),
				ReviewFailureCount: taskfile.CountReviewFailureMarkers(data),
			}

			key := dir + "/" + name
			idx.tasks[key] = snap
			snapshots = append(snapshots, snap)

			// Register frontmatter meta.ID (may differ from stem).
			idx.allIDs[meta.ID] = struct{}{}
			if dir == DirCompleted {
				idx.completedIDs[meta.ID] = struct{}{}
			}
			if isNonCompleted[dir] {
				idx.nonCompletedIDs[meta.ID] = struct{}{}
			}

			// Populate active-only indexes.
			if isActive[dir] {
				if branch != "" {
					idx.activeBranches[branch] = struct{}{}
				}
				for _, af := range meta.Affects {
					idx.activeAffects[af] = append(idx.activeAffects[af], name)
					if isDirPrefix(af) {
						if _, ok := activeAffectsPrefixSet[af]; ok {
							continue
						}
						activeAffectsPrefixSet[af] = struct{}{}
						idx.activeAffectsPrefixes = append(idx.activeAffectsPrefixes, af)
					}
				}
			}
		}

		idx.byState[dir] = snapshots
	}

	return idx
}

// BuildWarnings returns non-fatal filesystem warnings captured during index build.
func (idx *PollIndex) BuildWarnings() []BuildWarning {
	if idx == nil {
		return nil
	}
	return idx.buildWarnings
}

// TasksByState returns all snapshots in the given directory. Returns nil if
// the directory has no tasks or was not found during build.
func (idx *PollIndex) TasksByState(state string) []*TaskSnapshot {
	if idx == nil {
		return nil
	}
	return idx.byState[state]
}

// CompletedIDs returns the set of task IDs found in completed/. Both filename
// stems and frontmatter IDs are included.
func (idx *PollIndex) CompletedIDs() map[string]struct{} {
	if idx == nil {
		return nil
	}
	return idx.completedIDs
}

// NonCompletedIDs returns the set of task IDs found in all directories except
// completed/. Both filename stems and frontmatter IDs are included.
func (idx *PollIndex) NonCompletedIDs() map[string]struct{} {
	if idx == nil {
		return nil
	}
	return idx.nonCompletedIDs
}

// AllIDs returns the set of task IDs found across all queue directories.
func (idx *PollIndex) AllIDs() map[string]struct{} {
	if idx == nil {
		return nil
	}
	return idx.allIDs
}

// HasActiveOverlap reports whether any task in the active directories
// (in-progress, ready-for-review, ready-to-merge) declares an affects path
// that overlaps with the given list. An entry ending with "/" is treated as
// a directory prefix that matches any path underneath it.
func (idx *PollIndex) HasActiveOverlap(affects []string) bool {
	if idx == nil || len(affects) == 0 {
		return false
	}
	for _, af := range affects {
		// Exact match (fast path).
		if len(idx.activeAffects[af]) > 0 {
			return true
		}
		// If af is a prefix, check if any active key or active prefix
		// falls under it.
		if isDirPrefix(af) {
			for key := range idx.activeAffects {
				if strings.HasPrefix(key, af) {
					return true
				}
			}
			for _, activePrefix := range idx.activeAffectsPrefixes {
				if strings.HasPrefix(activePrefix, af) {
					return true
				}
			}
		}
		// Check if af falls under any active prefix entry.
		for _, prefix := range idx.activeAffectsPrefixes {
			if strings.HasPrefix(af, prefix) {
				return true
			}
		}
	}
	return false
}

// ActiveAffects returns the list of ActiveTask entries derived from the index.
// This replaces taskfile.CollectActiveAffects for callers that have an index.
func (idx *PollIndex) ActiveAffects() []taskfile.ActiveTask {
	if idx == nil {
		return nil
	}
	// Collect unique tasks from active dirs that have affects.
	type taskKey struct {
		name string
		dir  string
	}
	seen := make(map[taskKey]struct{})
	var result []taskfile.ActiveTask

	for _, dir := range activeDirs {
		for _, snap := range idx.byState[dir] {
			if len(snap.Meta.Affects) == 0 {
				continue
			}
			key := taskKey{name: snap.Filename, dir: dir}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, taskfile.ActiveTask{
				Name:    snap.Filename,
				Dir:     dir,
				Affects: snap.Meta.Affects,
			})
		}
	}
	return result
}

// ActiveBranches returns the set of branch names currently in use across
// active directories (in-progress, ready-for-review, ready-to-merge).
func (idx *PollIndex) ActiveBranches() map[string]struct{} {
	if idx == nil {
		return nil
	}
	return idx.activeBranches
}

// BacklogByPriority returns backlog tasks sorted by priority (ascending),
// then by filename (ascending). Tasks in the exclude set are omitted.
func (idx *PollIndex) BacklogByPriority(exclude map[string]struct{}) []*TaskSnapshot {
	if idx == nil {
		return nil
	}
	backlog := idx.byState[DirBacklog]
	result := make([]*TaskSnapshot, 0, len(backlog))
	for _, snap := range backlog {
		if exclude != nil {
			if _, excluded := exclude[snap.Filename]; excluded {
				continue
			}
		}
		result = append(result, snap)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Meta.Priority != result[j].Meta.Priority {
			return result[i].Meta.Priority < result[j].Meta.Priority
		}
		return result[i].Filename < result[j].Filename
	})
	return result
}

// ParseFailures returns the list of files that could not be parsed during
// index build. Callers can use this to move unparseable files to failed/.
func (idx *PollIndex) ParseFailures() []ParseFailure {
	if idx == nil {
		return nil
	}
	return idx.parseFailures
}

// WaitingParseFailures returns parse failures from the waiting/ directory.
func (idx *PollIndex) WaitingParseFailures() []ParseFailure {
	if idx == nil {
		return nil
	}
	var result []ParseFailure
	for _, pf := range idx.parseFailures {
		if pf.State == DirWaiting {
			result = append(result, pf)
		}
	}
	return result
}

// BacklogParseFailures returns parse failures from the backlog/ directory.
func (idx *PollIndex) BacklogParseFailures() []ParseFailure {
	if idx == nil {
		return nil
	}
	var result []ParseFailure
	for _, pf := range idx.parseFailures {
		if pf.State == DirBacklog {
			result = append(result, pf)
		}
	}
	return result
}

// Snapshot returns the TaskSnapshot for a specific state/filename, or nil if
// not found.
func (idx *PollIndex) Snapshot(state, filename string) *TaskSnapshot {
	if idx == nil {
		return nil
	}
	return idx.tasks[state+"/"+filename]
}

// buildTemporaryIndex creates a PollIndex for callers that pass nil. This
// preserves backward compatibility for code paths outside the poll loop
// (DryRun, status command, integration tests).
func buildTemporaryIndex(tasksDir string) *PollIndex {
	return BuildIndex(tasksDir)
}

// ensureIndex returns idx if non-nil, otherwise builds a temporary index.
func ensureIndex(tasksDir string, idx *PollIndex) *PollIndex {
	if idx != nil {
		return idx
	}
	return buildTemporaryIndex(tasksDir)
}
