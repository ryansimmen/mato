package queueview

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/taskfile"
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
	Cancelled          bool
	Branch             string // from <!-- branch: ... --> comment, "" if absent
	FailureCount       int    // <!-- failure: ... --> markers (excluding review-failure)
	LastFailureAt      time.Time
	ReviewFailureCount int // <!-- review-failure: ... --> markers
	// ClaimedBy is the agent ID from <!-- claimed-by: ... -->, "" if absent.
	ClaimedBy string
	// ClaimedAt is the timestamp from <!-- claimed-by: ... claimed-at: ... -->.
	ClaimedAt time.Time
	// LastFailureReason is the reason from the last <!-- failure: ... --> comment.
	LastFailureReason string
	// LastCycleFailureReason is the reason from the last <!-- cycle-failure: ... --> comment.
	LastCycleFailureReason string
	// LastTerminalFailureReason is the reason from the last <!-- terminal-failure: ... --> comment.
	LastTerminalFailureReason string
	// LastReviewRejectionReason is the reason from the last
	// <!-- review-rejection: ... --> comment.
	LastReviewRejectionReason string
	// GlobError caches the result of ValidateAffectsGlobs, computed once
	// during index build. nil means all glob patterns are valid.
	GlobError error
}

// ParseFailure records a task file that could not be parsed during index build.
type ParseFailure struct {
	Filename  string
	State     string // directory the file was found in
	Path      string
	Err       error
	Cancelled bool
	Branch    string // from <!-- branch: ... --> comment, extracted before parse failure
	// ClaimedBy is the agent ID from <!-- claimed-by: ... -->, "" if absent.
	ClaimedBy string
	// ClaimedAt is the timestamp from <!-- claimed-by: ... claimed-at: ... -->.
	ClaimedAt time.Time
	// FailureCount is the number of <!-- failure: ... --> markers.
	FailureCount int
	// LastFailureReason is the reason from the last <!-- failure: ... --> comment.
	LastFailureReason string
	// LastCycleFailureReason is the reason from the last <!-- cycle-failure: ... --> comment.
	LastCycleFailureReason string
	// LastTerminalFailureReason is the reason from the last <!-- terminal-failure: ... --> comment.
	LastTerminalFailureReason string
	// LastReviewRejectionReason is the reason from the last
	// <!-- review-rejection: ... --> comment.
	LastReviewRejectionReason string
	// RecoveredAffects holds affects entries recovered from malformed active
	// tasks for conservative overlap blocking. Nil when recovery was not needed
	// or not possible.
	RecoveredAffects []string
}

// BuildWarning records a non-fatal filesystem warning encountered while
// scanning a queue directory.
type BuildWarning struct {
	State    string
	Path     string
	Err      error
	DirLevel bool // true when the warning is a directory-level read failure
}

// PollIndex is an in-memory snapshot of all task files across the queue
// directories. It is built once per poll cycle by scanning each directory
// and reading each file exactly once. All consumers query the index instead
// of performing independent filesystem scans.
//
// PollIndex is a plain struct with no concurrency protection. Each agent
// process runs in a separate terminal; cross-process safety is handled by
// existing atomic filesystem operations in queue mutation paths.
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

	// activeAffectsGlobs holds affects entries from active dirs that
	// contain glob metacharacters. Stored separately so HasActiveOverlap
	// can match incoming entries against active glob patterns.
	activeAffectsGlobs []string

	// activeBranches is the set of branch names in active directories
	// (in-progress, ready-for-review, ready-to-merge).
	activeBranches map[string]struct{}

	// activeRecoveredAffects holds affects recovered from active parse failures
	// so overlap checks still see them even when the task snapshot is invalid.
	activeRecoveredAffects []taskfile.ActiveTask

	// activeUnknownOverlapBlockers records malformed active tasks whose
	// affects could not be recovered safely. These conservatively block any
	// incoming non-empty affects from being treated as overlap-free.
	activeUnknownOverlapBlockers []activeOverlapBlocker

	// parseFailures records files that failed to parse during build.
	parseFailures []ParseFailure

	// buildWarnings records non-fatal filesystem issues encountered while
	// scanning directories.
	buildWarnings []BuildWarning
}

// activeDirs are the directories representing tasks actively being worked on.
var activeDirs = []string{dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge}

// nonCompletedDirs are all directories except completed/.
var nonCompletedDirs = []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge, dirs.Failed}

type taskFileMetadata struct {
	branch                    string
	claimedBy                 string
	claimedAt                 time.Time
	cancelled                 bool
	failureCount              int
	lastFailureAt             time.Time
	reviewFailureCount        int
	lastFailureReason         string
	lastCycleFailureReason    string
	lastTerminalFailureReason string
	lastReviewRejectionReason string
}

type activeOverlapBlocker struct {
	name string
	dir  string
}

type recoveredActiveAffects struct {
	affects  []string
	stripped []frontmatter.StrippedAffect
	globErr  error
	blockAll bool
}

func scanTaskFileMetadata(tasksDir, filename string, data []byte) taskFileMetadata {
	branch, _ := taskfile.ParseBranchMarkerLine(data)
	claimedBy, _ := taskfile.ParseClaimedBy(data)
	claimedAt, _ := taskfile.ParseClaimedAt(data)
	lastFailureAt, _ := lastFailureTime(data)

	info := taskFileMetadata{
		branch:                    branch,
		claimedBy:                 claimedBy,
		claimedAt:                 claimedAt,
		cancelled:                 taskfile.ContainsCancelledMarker(data),
		failureCount:              taskfile.CountFailureMarkers(data),
		lastFailureAt:             lastFailureAt,
		reviewFailureCount:        taskfile.CountReviewFailureMarkers(data),
		lastFailureReason:         taskfile.LastFailureReason(data),
		lastCycleFailureReason:    taskfile.LastCycleFailureReason(data),
		lastTerminalFailureReason: taskfile.LastTerminalFailureReason(data),
		lastReviewRejectionReason: taskfile.LastReviewRejectionReason(data),
	}
	if info.lastReviewRejectionReason == "" && tasksDir != "" && filename != "" {
		if vr, ok := taskfile.ReadVerdictRejection(tasksDir, filename); ok {
			info.lastReviewRejectionReason = vr.Reason
		}
	}
	return info
}

func snapshotFromData(state, filename, path string, data []byte, info taskFileMetadata) (*TaskSnapshot, error) {
	meta, body, err := frontmatter.ParseTaskData(data, path)
	if err != nil {
		return nil, err
	}

	snap := &TaskSnapshot{
		Filename:                  filename,
		State:                     state,
		Path:                      path,
		Meta:                      meta,
		Body:                      body,
		Cancelled:                 info.cancelled,
		Branch:                    info.branch,
		FailureCount:              info.failureCount,
		LastFailureAt:             info.lastFailureAt,
		ReviewFailureCount:        info.reviewFailureCount,
		ClaimedBy:                 info.claimedBy,
		ClaimedAt:                 info.claimedAt,
		LastFailureReason:         info.lastFailureReason,
		LastCycleFailureReason:    info.lastCycleFailureReason,
		LastTerminalFailureReason: info.lastTerminalFailureReason,
		LastReviewRejectionReason: info.lastReviewRejectionReason,
	}
	if globErr := frontmatter.ValidateAffectsGlobs(meta.Affects); globErr != nil {
		snap.GlobError = globErr
	}
	return snap, nil
}

func indexActiveAffects(idx *PollIndex, name string, affects []string, prefixSet, globSet map[string]struct{}) {
	if len(affects) == 0 {
		return
	}
	for _, af := range affects {
		idx.activeAffects[af] = append(idx.activeAffects[af], name)
		if isDirPrefix(af) {
			if _, ok := prefixSet[af]; ok {
				continue
			}
			prefixSet[af] = struct{}{}
			idx.activeAffectsPrefixes = append(idx.activeAffectsPrefixes, af)
			continue
		}
		if isGlob(af) {
			if _, ok := globSet[af]; ok {
				continue
			}
			globSet[af] = struct{}{}
			idx.activeAffectsGlobs = append(idx.activeAffectsGlobs, af)
		}
	}
}

func appendRecoveredAffectsWarnings(idx *PollIndex, dir, path string, recovered recoveredActiveAffects) {
	if recovered.globErr != nil {
		idx.buildWarnings = append(idx.buildWarnings, BuildWarning{
			State: dir,
			Path:  path,
			Err:   recovered.globErr,
		})
	}
	for _, sa := range recovered.stripped {
		idx.buildWarnings = append(idx.buildWarnings, BuildWarning{
			State: dir,
			Path:  path,
			Err:   fmt.Errorf("unsafe affects entry %q: %s", sa.Entry, sa.Reason),
		})
	}
}

func recoverActiveAffectsFromMalformedTask(path string, data []byte) recoveredActiveAffects {
	block, ok := rawFrontmatterBlock(data)
	if !ok {
		return recoveredActiveAffects{blockAll: true}
	}
	snippet, found := extractAffectsSnippet(block)
	if !found {
		return recoveredActiveAffects{blockAll: true}
	}
	meta, _, err := frontmatter.ParseTaskData([]byte("---\n"+snippet+"\n---\n"), path)
	if err != nil {
		return recoveredActiveAffects{blockAll: true}
	}
	blockAll := len(meta.Affects) == 0 && len(meta.StrippedAffects) > 0
	return recoveredActiveAffects{
		affects:  meta.Affects,
		stripped: meta.StrippedAffects,
		globErr:  frontmatter.ValidateAffectsGlobs(meta.Affects),
		blockAll: blockAll,
	}
}

func rawFrontmatterBlock(data []byte) (string, bool) {
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	start := 0
	for start < len(lines) {
		trimmed := strings.TrimSpace(lines[start])
		if trimmed == "" || isStandaloneCommentLine(trimmed) {
			start++
			continue
		}
		break
	}
	if start >= len(lines) || strings.TrimSpace(lines[start]) != "---" {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	return strings.Join(lines[start+1:end], "\n"), true
}

func isStandaloneCommentLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->")
}

func extractAffectsSnippet(block string) (string, bool) {
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent != 0 {
			continue
		}
		key, _, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(key) != "affects" {
			continue
		}
		end := i + 1
		for end < len(lines) {
			next := lines[end]
			nextTrimmed := strings.TrimSpace(next)
			if nextTrimmed == "" || strings.HasPrefix(nextTrimmed, "#") {
				end++
				continue
			}
			nextIndent := len(next) - len(strings.TrimLeft(next, " \t"))
			if nextIndent == 0 {
				if nextKey, _, ok := strings.Cut(nextTrimmed, ":"); ok && strings.TrimSpace(nextKey) != "" {
					break
				}
			}
			end++
		}
		return strings.Join(lines[i:end], "\n"), true
	}
	return "", false
}

// LoadTaskSnapshot reads and parses one task file into a snapshot.
func LoadTaskSnapshot(tasksDir, state, filename, path string) (*TaskSnapshot, error) {
	data, err := taskfile.ReadRegularTaskFile(path)
	if err != nil {
		return nil, err
	}
	info := scanTaskFileMetadata(tasksDir, filename, data)
	return snapshotFromData(state, filename, path, data, info)
}

func lastFailureTime(data []byte) (time.Time, bool) {
	records := taskfile.ParseFailureMarkers(data)
	if len(records) == 0 {
		return time.Time{}, false
	}
	last := records[0].Timestamp
	for _, r := range records[1:] {
		if r.Timestamp.After(last) {
			last = r.Timestamp
		}
	}
	return last, true
}

// BuildIndex scans all queue directories under tasksDir and reads each task
// file exactly once, building an in-memory index. Returns a fully populated
// PollIndex.
func BuildIndex(tasksDir string) *PollIndex {
	idx := &PollIndex{
		tasks:           make(map[string]*TaskSnapshot),
		byState:         make(map[string][]*TaskSnapshot, len(dirs.All)),
		completedIDs:    make(map[string]struct{}),
		nonCompletedIDs: make(map[string]struct{}),
		allIDs:          make(map[string]struct{}),
		activeAffects:   make(map[string][]string),
		activeBranches:  make(map[string]struct{}),
	}
	activeAffectsPrefixSet := make(map[string]struct{})
	activeAffectsGlobSet := make(map[string]struct{})

	isActive := make(map[string]bool, len(activeDirs))
	for _, d := range activeDirs {
		isActive[d] = true
	}
	isNonCompleted := make(map[string]bool, len(nonCompletedDirs))
	for _, d := range nonCompletedDirs {
		isNonCompleted[d] = true
	}

	for _, dir := range dirs.All {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := taskfile.ListTaskFiles(dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			idx.buildWarnings = append(idx.buildWarnings, BuildWarning{State: dir, Path: dirPath, Err: err, DirLevel: true})
			continue
		}

		snapshots := make([]*TaskSnapshot, 0, len(names))
		for _, name := range names {
			path := filepath.Join(dirPath, name)

			stem := frontmatter.TaskFileStem(name)
			idx.allIDs[stem] = struct{}{}
			if dir == dirs.Completed {
				idx.completedIDs[stem] = struct{}{}
			}
			if isNonCompleted[dir] {
				idx.nonCompletedIDs[stem] = struct{}{}
			}

			data, err := taskfile.ReadRegularTaskFile(path)
			if err != nil {
				pf := ParseFailure{
					Filename: name, State: dir, Path: path, Err: err,
				}
				idx.parseFailures = append(idx.parseFailures, pf)
				if isActive[dir] {
					idx.activeUnknownOverlapBlockers = append(idx.activeUnknownOverlapBlockers, activeOverlapBlocker{
						name: name,
						dir:  dir,
					})
				}
				continue
			}

			info := scanTaskFileMetadata(tasksDir, name, data)
			snap, err := snapshotFromData(dir, name, path, data, info)
			if err != nil {
				pf := ParseFailure{
					Filename:                  name,
					State:                     dir,
					Path:                      path,
					Err:                       err,
					Cancelled:                 info.cancelled,
					Branch:                    info.branch,
					ClaimedBy:                 info.claimedBy,
					ClaimedAt:                 info.claimedAt,
					FailureCount:              info.failureCount,
					LastFailureReason:         info.lastFailureReason,
					LastCycleFailureReason:    info.lastCycleFailureReason,
					LastTerminalFailureReason: info.lastTerminalFailureReason,
					LastReviewRejectionReason: info.lastReviewRejectionReason,
				}
				if isActive[dir] && info.branch != "" {
					idx.activeBranches[info.branch] = struct{}{}
				}
				if isActive[dir] {
					recovered := recoverActiveAffectsFromMalformedTask(path, data)
					appendRecoveredAffectsWarnings(idx, dir, path, recovered)
					switch {
					case len(recovered.affects) > 0:
						pf.RecoveredAffects = append([]string(nil), recovered.affects...)
						idx.activeRecoveredAffects = append(idx.activeRecoveredAffects, taskfile.ActiveTask{
							Name:    name,
							Dir:     dir,
							Affects: append([]string(nil), recovered.affects...),
						})
						indexActiveAffects(idx, name, recovered.affects, activeAffectsPrefixSet, activeAffectsGlobSet)
					case recovered.blockAll:
						idx.activeUnknownOverlapBlockers = append(idx.activeUnknownOverlapBlockers, activeOverlapBlocker{
							name: name,
							dir:  dir,
						})
					}
				}
				idx.parseFailures = append(idx.parseFailures, pf)
				continue
			}
			meta := snap.Meta

			if snap.GlobError != nil {
				idx.buildWarnings = append(idx.buildWarnings, BuildWarning{
					State: dir, Path: path, Err: snap.GlobError,
				})
			}

			for _, sa := range meta.StrippedAffects {
				idx.buildWarnings = append(idx.buildWarnings, BuildWarning{
					State: dir, Path: path,
					Err: fmt.Errorf("unsafe affects entry %q: %s", sa.Entry, sa.Reason),
				})
			}

			key := dir + "/" + name
			idx.tasks[key] = snap
			snapshots = append(snapshots, snap)

			idx.allIDs[meta.ID] = struct{}{}
			if dir == dirs.Completed {
				idx.completedIDs[meta.ID] = struct{}{}
			}
			if isNonCompleted[dir] {
				idx.nonCompletedIDs[meta.ID] = struct{}{}
			}

			if isActive[dir] {
				if info.branch != "" {
					idx.activeBranches[info.branch] = struct{}{}
				}
				indexActiveAffects(idx, name, meta.Affects, activeAffectsPrefixSet, activeAffectsGlobSet)
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
// stems and frontmatter IDs are included. The returned map is a shallow copy;
// callers may mutate it without affecting the index.
func (idx *PollIndex) CompletedIDs() map[string]struct{} {
	if idx == nil {
		return nil
	}
	out := make(map[string]struct{}, len(idx.completedIDs))
	for k, v := range idx.completedIDs {
		out[k] = v
	}
	return out
}

// NonCompletedIDs returns the set of task IDs found in all directories except
// completed/. Both filename stems and frontmatter IDs are included. The
// returned map is a shallow copy; callers may mutate it without affecting the
// index.
func (idx *PollIndex) NonCompletedIDs() map[string]struct{} {
	if idx == nil {
		return nil
	}
	out := make(map[string]struct{}, len(idx.nonCompletedIDs))
	for k, v := range idx.nonCompletedIDs {
		out[k] = v
	}
	return out
}

// AllIDs returns the set of task IDs found across all queue directories.
// The returned map is a shallow copy; callers may mutate it without
// affecting the index.
func (idx *PollIndex) AllIDs() map[string]struct{} {
	if idx == nil {
		return nil
	}
	out := make(map[string]struct{}, len(idx.allIDs))
	for k, v := range idx.allIDs {
		out[k] = v
	}
	return out
}

// HasActiveOverlap reports whether any task in the active directories
// (in-progress, ready-for-review, ready-to-merge) declares an affects path
// that overlaps with the given list. An entry ending with "/" is treated as
// a directory prefix that matches any path underneath it. Entries containing
// glob metacharacters (*, ?, [, {) are matched using doublestar pattern
// matching.
func (idx *PollIndex) HasActiveOverlap(affects []string) bool {
	if idx == nil || len(affects) == 0 {
		return false
	}
	if len(idx.activeUnknownOverlapBlockers) > 0 {
		return true
	}
	for _, af := range affects {
		if len(idx.activeAffects[af]) > 0 {
			return true
		}
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
		for _, prefix := range idx.activeAffectsPrefixes {
			if isInvalidGlob(prefix) {
				if affectsMatch(prefix, af) {
					return true
				}
				continue
			}
			if strings.HasPrefix(af, prefix) {
				return true
			}
		}
		for _, g := range idx.activeAffectsGlobs {
			if affectsMatch(g, af) {
				return true
			}
		}
		if isGlob(af) {
			for key := range idx.activeAffects {
				if affectsMatch(af, key) {
					return true
				}
			}
		}
	}
	return false
}

// ActiveAffects returns the list of ActiveTask entries derived from the index.
func (idx *PollIndex) ActiveAffects() []taskfile.ActiveTask {
	if idx == nil {
		return nil
	}
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
	for _, recovered := range idx.activeRecoveredAffects {
		if len(recovered.Affects) == 0 {
			continue
		}
		key := taskKey{name: recovered.Name, dir: recovered.Dir}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, taskfile.ActiveTask{
			Name:    recovered.Name,
			Dir:     recovered.Dir,
			Affects: append([]string(nil), recovered.Affects...),
		})
	}
	return result
}

// ActiveBranches returns the set of branch names currently in use across
// active directories (in-progress, ready-for-review, ready-to-merge).
// The returned map is a shallow copy; callers may mutate it without
// affecting the index.
func (idx *PollIndex) ActiveBranches() map[string]struct{} {
	if idx == nil {
		return nil
	}
	out := make(map[string]struct{}, len(idx.activeBranches))
	for k, v := range idx.activeBranches {
		out[k] = v
	}
	return out
}

// BacklogByPriority returns backlog tasks sorted by priority (ascending),
// then by filename (ascending). Tasks in the exclude set are omitted.
func (idx *PollIndex) BacklogByPriority(exclude map[string]struct{}) []*TaskSnapshot {
	if idx == nil {
		return nil
	}
	backlog := idx.byState[dirs.Backlog]
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
		if pf.State == dirs.Waiting {
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
		if pf.State == dirs.Backlog {
			result = append(result, pf)
		}
	}
	return result
}

// ReviewParseFailures returns parse failures from the ready-for-review/ directory.
func (idx *PollIndex) ReviewParseFailures() []ParseFailure {
	if idx == nil {
		return nil
	}
	var result []ParseFailure
	for _, pf := range idx.parseFailures {
		if pf.State == dirs.ReadyReview {
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

func buildTemporaryIndex(tasksDir string) *PollIndex {
	return BuildIndex(tasksDir)
}

func ensureIndex(tasksDir string, idx *PollIndex) *PollIndex {
	if idx != nil {
		return idx
	}
	return buildTemporaryIndex(tasksDir)
}
