// Package inspect explains the current state of a single task.
package inspect

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/dag"
	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/queue"
	"mato/internal/ui"
)

type blockingTask struct {
	Filename string `json:"filename"`
	State    string `json:"state"`
}

type blockingDependency struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Filename string `json:"filename,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type inspectResult struct {
	TaskID   string
	Filename string
	Title    string
	State    string
	Status   string
	Reason   string
	NextStep string

	Branch    string
	ClaimedBy string
	ClaimedAt time.Time

	QueuePosition int
	QueueTotal    int

	BlockingTask         *blockingTask
	BlockingDependencies []blockingDependency
	ConflictingAffects   []string

	FailureKind           string
	FailureCount          int
	ReviewFailureCount    int
	MaxRetries            int
	LastFailureReason     string
	LastCycleReason       string
	LastTerminalReason    string
	ReviewRejectionReason string

	ParseError string
}

type candidate struct {
	filename     string
	state        string
	stem         string
	explicitID   string
	snapshot     *queue.TaskSnapshot
	parseFailure *queue.ParseFailure
}

type dependencyLocation struct {
	Filename string
	State    string
}

type inspectContext struct {
	idx               *queue.PollIndex
	diag              queue.DependencyDiagnostics
	view              queue.RunnableBacklogView
	locationsByRef    map[string][]dependencyLocation
	cycleByID         map[string][]string
	blockedByFilename map[string][]dag.BlockDetail
	runnablePos       map[string]int
	runnableTotal     int
	deferredByName    map[string]queue.DeferralInfo
	depBlockedByName  map[string][]queue.DependencyBlock
	retainedWaiting   map[string]string
}

// Show writes the inspection result to stdout.
func Show(repoRoot, taskRef, format string) error {
	return ShowTo(os.Stdout, repoRoot, taskRef, format)
}

// ShowTo writes the inspection result to w.
func ShowTo(w io.Writer, repoRoot, taskRef, format string) error {
	if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
		return err
	}

	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return err
	}
	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := ui.RequireTasksDir(tasksDir); err != nil {
		return err
	}

	result, err := inspectTask(tasksDir, taskRef)
	if err != nil {
		return fmt.Errorf("inspect task: %w", err)
	}

	if format == "json" {
		return renderJSON(w, result)
	}
	renderText(w, result)
	return nil
}

func inspectTask(tasksDir, taskRef string) (inspectResult, error) {
	idx := queue.BuildIndex(tasksDir)
	match, err := resolveCandidate(idx, taskRef)
	if err != nil {
		return inspectResult{}, err
	}

	ctx := buildInspectContext(tasksDir, idx)
	return buildResult(match, ctx), nil
}

func resolveCandidate(idx *queue.PollIndex, taskRef string) (candidate, error) {
	match, err := queue.ResolveTask(idx, taskRef)
	if err != nil {
		return candidate{}, err
	}
	cand := candidate{
		filename: match.Filename,
		state:    match.State,
		stem:     frontmatter.TaskFileStem(match.Filename),
		snapshot: match.Snapshot,
	}
	if match.Snapshot != nil {
		cand.explicitID = match.Snapshot.Meta.ID
	}
	if match.ParseFailure != nil {
		cand.parseFailure = match.ParseFailure
	}
	return cand, nil
}

func buildInspectContext(tasksDir string, idx *queue.PollIndex) inspectContext {
	diag := queue.DiagnoseDependencies(tasksDir, idx)
	view := queue.ComputeRunnableBacklogView(tasksDir, idx)
	ctx := inspectContext{
		idx:               idx,
		diag:              diag,
		view:              view,
		locationsByRef:    buildLocationsByRef(idx),
		cycleByID:         buildCycleMap(diag.Analysis.Cycles),
		blockedByFilename: buildBlockedByFilename(idx, diag.Analysis.Blocked),
		deferredByName:    view.Deferred,
		depBlockedByName:  view.DependencyBlocked,
		retainedWaiting:   diag.RetainedFiles,
		runnablePos:       make(map[string]int, len(view.Runnable)),
		runnableTotal:     len(view.Runnable),
	}
	for i, snap := range view.Runnable {
		ctx.runnablePos[snap.Filename] = i + 1
	}
	return ctx
}

func buildLocationsByRef(idx *queue.PollIndex) map[string][]dependencyLocation {
	locations := make(map[string][]dependencyLocation)
	for _, dir := range queue.AllDirs {
		for _, snap := range idx.TasksByState(dir) {
			loc := dependencyLocation{Filename: snap.Filename, State: dir}
			locations[frontmatter.TaskFileStem(snap.Filename)] = appendUniqueLocation(locations[frontmatter.TaskFileStem(snap.Filename)], loc)
			if snap.Meta.ID != "" {
				locations[snap.Meta.ID] = appendUniqueLocation(locations[snap.Meta.ID], loc)
			}
		}
	}
	for _, pf := range idx.ParseFailures() {
		stem := frontmatter.TaskFileStem(pf.Filename)
		loc := dependencyLocation{Filename: pf.Filename, State: pf.State}
		locations[stem] = appendUniqueLocation(locations[stem], loc)
	}
	return locations
}

func appendUniqueLocation(existing []dependencyLocation, loc dependencyLocation) []dependencyLocation {
	for _, item := range existing {
		if item == loc {
			return existing
		}
	}
	return append(existing, loc)
}

func buildCycleMap(cycles [][]string) map[string][]string {
	byID := make(map[string][]string)
	for _, cycle := range cycles {
		copyCycle := append([]string(nil), cycle...)
		for _, id := range cycle {
			byID[id] = copyCycle
		}
	}
	return byID
}

func buildBlockedByFilename(idx *queue.PollIndex, blocked map[string][]dag.BlockDetail) map[string][]dag.BlockDetail {
	byFilename := make(map[string][]dag.BlockDetail)
	for _, snap := range idx.TasksByState(queue.DirWaiting) {
		if details, ok := blocked[snap.Meta.ID]; ok {
			byFilename[snap.Filename] = details
		}
	}
	return byFilename
}

func buildResult(match candidate, ctx inspectContext) inspectResult {
	if match.parseFailure != nil {
		return buildParseFailureResult(*match.parseFailure)
	}
	return buildSnapshotResult(match.snapshot, ctx)
}

func buildParseFailureResult(pf queue.ParseFailure) inspectResult {
	result := inspectResult{
		TaskID:                frontmatter.TaskFileStem(pf.Filename),
		Filename:              pf.Filename,
		Title:                 frontmatter.TaskFileStem(pf.Filename),
		State:                 pf.State,
		Status:                "invalid",
		Reason:                fmt.Sprintf("task frontmatter cannot be parsed: %v", pf.Err),
		NextStep:              "fix the task frontmatter so mato can index the file again",
		Branch:                pf.Branch,
		ClaimedBy:             pf.ClaimedBy,
		ClaimedAt:             pf.ClaimedAt,
		FailureCount:          pf.FailureCount,
		LastFailureReason:     pf.LastFailureReason,
		LastCycleReason:       pf.LastCycleFailureReason,
		LastTerminalReason:    pf.LastTerminalFailureReason,
		ReviewRejectionReason: pf.LastReviewRejectionReason,
		ParseError:            pf.Err.Error(),
		ReviewFailureCount:    0,
		MaxRetries:            0,
	}
	if pf.State == queue.DirReadyReview {
		result.NextStep = "fix the task frontmatter before the next review pass quarantines it to failed/"
	}
	if pf.Cancelled {
		result.FailureKind = "cancelled"
	} else if pf.LastTerminalFailureReason != "" {
		result.FailureKind = "terminal"
	} else if pf.LastCycleFailureReason != "" {
		result.FailureKind = "cycle"
	} else if pf.FailureCount > 0 {
		result.FailureKind = "retry"
	}
	if pf.State == queue.DirFailed {
		result.Status = "failed"
		result.Reason = "task frontmatter cannot be parsed and the task is quarantined in failed/"
		switch result.FailureKind {
		case "cancelled":
			result.NextStep = "fix the task frontmatter, then requeue with mato retry if you want to run it again"
		case "terminal":
			result.NextStep = "fix the task frontmatter and the structural failure, then retry the task"
		case "cycle":
			result.NextStep = "fix the task frontmatter and dependency cycle, then retry the task"
		default:
			result.NextStep = "fix the task frontmatter and last failure cause, then requeue the task with mato retry"
		}
	}
	return result
}

func buildSnapshotResult(snap *queue.TaskSnapshot, ctx inspectContext) inspectResult {
	result := inspectResult{
		TaskID:                snapshotTaskID(snap),
		Filename:              snap.Filename,
		Title:                 frontmatter.ExtractTitle(snap.Filename, snap.Body),
		State:                 snap.State,
		Branch:                snap.Branch,
		ClaimedBy:             snap.ClaimedBy,
		ClaimedAt:             snap.ClaimedAt,
		FailureCount:          snap.FailureCount,
		ReviewFailureCount:    snap.ReviewFailureCount,
		MaxRetries:            snap.Meta.MaxRetries,
		LastFailureReason:     snap.LastFailureReason,
		LastCycleReason:       snap.LastCycleFailureReason,
		LastTerminalReason:    snap.LastTerminalFailureReason,
		ReviewRejectionReason: snap.LastReviewRejectionReason,
	}

	switch snap.State {
	case queue.DirWaiting:
		buildWaitingResult(&result, snap, ctx)
	case queue.DirBacklog:
		buildBacklogResult(&result, snap, ctx)
	case queue.DirInProgress:
		result.Status = "running"
		result.Reason = runningReason(snap)
		result.NextStep = "wait for the active agent to finish or release the task"
	case queue.DirReadyReview:
		if snap.ReviewFailureCount >= snap.Meta.MaxRetries {
			result.Status = "invalid"
			result.Reason = fmt.Sprintf("review retry budget exhausted (%d/%d failures); the next review selection pass will move this task to failed/", snap.ReviewFailureCount, snap.Meta.MaxRetries)
			result.NextStep = "inspect the review failures, then retry only after fixing the underlying review issue"
			break
		}
		result.Status = "ready_for_review"
		if snap.Branch != "" {
			result.Reason = fmt.Sprintf("branch %s is queued for AI review", snap.Branch)
		} else {
			result.Reason = "task is queued for AI review"
		}
		result.NextStep = "wait for the review agent to approve, reject, or record a review failure"
	case queue.DirReadyMerge:
		result.Status = "ready_to_merge"
		result.Reason = "review passed; task is queued for host squash merge"
		result.NextStep = "wait for the merge queue to squash-merge the task branch"
	case queue.DirCompleted:
		result.Status = "completed"
		result.Reason = "task is already merged and completed"
		result.NextStep = "no further action is required"
	case queue.DirFailed:
		buildFailedResult(&result, snap)
	default:
		result.Status = snap.State
		result.Reason = fmt.Sprintf("task is currently in %s/", snap.State)
		result.NextStep = "inspect the queue state and take the appropriate follow-up action"
	}

	return result
}

func buildWaitingResult(result *inspectResult, snap *queue.TaskSnapshot, ctx inspectContext) {
	if snap.GlobError != nil {
		result.Status = "invalid"
		result.Reason = fmt.Sprintf("task has invalid affects glob syntax: %v", snap.GlobError)
		result.NextStep = "fix the invalid affects glob before reconcile moves the task to failed/"
		return
	}
	if retained, ok := ctx.retainedWaiting[snap.Meta.ID]; ok && retained != snap.Filename {
		result.Status = "invalid"
		result.Reason = fmt.Sprintf("duplicate waiting task id %q; %s is the retained copy", snap.Meta.ID, retained)
		result.NextStep = "remove or rename the duplicate task id so only one waiting task keeps this id"
		return
	}
	if cycle := ctx.cycleByID[snap.Meta.ID]; len(cycle) > 0 {
		result.Status = "invalid"
		if len(cycle) == 1 {
			result.Reason = fmt.Sprintf("task depends on itself through id %q", snap.Meta.ID)
		} else {
			result.Reason = fmt.Sprintf("task is part of a circular dependency: %s", strings.Join(cycle, " -> "))
		}
		result.NextStep = "fix the dependency cycle before reconcile moves the task to failed/"
		return
	}
	if details, ok := ctx.blockedByFilename[snap.Filename]; ok && len(details) > 0 {
		result.Status = "blocked"
		result.BlockingDependencies = buildBlockedDependencies(details, ctx.locationsByRef)
		result.Reason = blockedReason(result.BlockingDependencies)
		result.NextStep = "complete or fix the blocking dependencies so this task can leave waiting/"
		return
	}
	if ctx.idx.HasActiveOverlap(snap.Meta.Affects) {
		result.Status = "blocked"
		result.Reason = "task dependencies are satisfied, but active overlapping work still prevents promotion from waiting/"
		result.NextStep = "wait for overlapping active work to finish or narrow this task's affects entries"
		return
	}
	// Safety net for stale or manually-edited queue states observed before the
	// next reconcile pass moves a ready waiting task into backlog/.
	result.Status = "blocked"
	result.Reason = "dependencies are satisfied; waiting for the next reconcile pass to move the task into backlog/"
	result.NextStep = "run reconcile or wait for the next poll cycle"
}

func buildBacklogResult(result *inspectResult, snap *queue.TaskSnapshot, ctx inspectContext) {
	if snap.GlobError != nil {
		result.Status = "invalid"
		result.Reason = fmt.Sprintf("task has invalid affects glob syntax: %v", snap.GlobError)
		result.NextStep = "fix the invalid affects glob before reconcile moves the task to failed/"
		return
	}
	if blocks, ok := ctx.depBlockedByName[snap.Filename]; ok && len(blocks) > 0 {
		result.Status = "blocked"
		result.BlockingDependencies = buildQueueDependencyBlocks(blocks, ctx.locationsByRef)
		result.Reason = blockedReason(result.BlockingDependencies)
		result.NextStep = "complete or fix the blocking dependencies; reconcile will move this task back to waiting/"
		return
	}
	if def, ok := ctx.deferredByName[snap.Filename]; ok {
		result.Status = "deferred"
		result.BlockingTask = &blockingTask{Filename: def.BlockedBy, State: def.BlockedByDir}
		result.ConflictingAffects = append([]string(nil), def.ConflictingAffects...)
		result.Reason = fmt.Sprintf("conflicts with %s/%s on overlapping affects", def.BlockedByDir, def.BlockedBy)
		result.NextStep = fmt.Sprintf("wait for %s/%s to clear or narrow the overlapping affects", def.BlockedByDir, def.BlockedBy)
		return
	}
	result.Status = "runnable"
	result.QueuePosition = ctx.runnablePos[snap.Filename]
	result.QueueTotal = ctx.runnableTotal
	if result.QueuePosition <= 1 {
		result.Reason = "task is the next claim candidate"
	} else {
		result.Reason = fmt.Sprintf("%d runnable task(s) are ahead in claim order", result.QueuePosition-1)
	}
	result.NextStep = "wait for an agent to claim the task from backlog/"
}

func buildFailedResult(result *inspectResult, snap *queue.TaskSnapshot) {
	result.Status = "failed"
	switch {
	case snap.Cancelled:
		result.FailureKind = "cancelled"
		result.Reason = "task was deliberately cancelled by an operator"
		result.NextStep = "use mato retry to requeue if you want to run it again"
	case snap.LastTerminalFailureReason != "":
		result.FailureKind = "terminal"
		result.Reason = fmt.Sprintf("task failed with a structural error: %s", snap.LastTerminalFailureReason)
		result.NextStep = "fix the structural problem, then retry the task"
	case snap.LastCycleFailureReason != "":
		result.FailureKind = "cycle"
		result.Reason = fmt.Sprintf("task failed because of a dependency cycle: %s", snap.LastCycleFailureReason)
		result.NextStep = "fix the dependency cycle, then retry the task"
	default:
		result.FailureKind = "retry"
		reason := snap.LastFailureReason
		if reason == "" {
			reason = "no recorded failure reason"
		}
		result.Reason = fmt.Sprintf("task exhausted its retry budget after %d/%d failures: %s", snap.FailureCount, snap.Meta.MaxRetries, reason)
		result.NextStep = "fix the failure cause, then requeue the task with mato retry"
	}
}

func runningReason(snap *queue.TaskSnapshot) string {
	if snap.ClaimedBy == "" && snap.ClaimedAt.IsZero() {
		return "task is currently being worked on"
	}
	if snap.ClaimedBy != "" && !snap.ClaimedAt.IsZero() {
		return fmt.Sprintf("claimed by %s at %s", snap.ClaimedBy, snap.ClaimedAt.UTC().Format(time.RFC3339))
	}
	if snap.ClaimedBy != "" {
		return fmt.Sprintf("claimed by %s", snap.ClaimedBy)
	}
	return fmt.Sprintf("claimed at %s", snap.ClaimedAt.UTC().Format(time.RFC3339))
}

func buildBlockedDependencies(details []dag.BlockDetail, locationsByRef map[string][]dependencyLocation) []blockingDependency {
	deps := make([]blockingDependency, 0, len(details))
	for _, detail := range details {
		reason := blockReasonString(detail.Reason)
		// Reason preserves the DAG-level blocker classification, while State is a
		// presentation field that may be rewritten to a concrete queue state when
		// the dependency can be located in the current snapshot.
		dep := blockingDependency{ID: detail.DependencyID, State: reason, Reason: reason}
		populateDependencyLocation(&dep, locationsByRef[detail.DependencyID])
		deps = append(deps, dep)
	}
	return deps
}

func buildQueueDependencyBlocks(blocks []queue.DependencyBlock, locationsByRef map[string][]dependencyLocation) []blockingDependency {
	deps := make([]blockingDependency, 0, len(blocks))
	for _, block := range blocks {
		dep := blockingDependency{ID: block.DependencyID, State: block.State, Reason: block.State}
		populateDependencyLocation(&dep, locationsByRef[block.DependencyID])
		deps = append(deps, dep)
	}
	return deps
}

func populateDependencyLocation(dep *blockingDependency, locations []dependencyLocation) {
	if len(locations) == 0 {
		return
	}
	states := make(map[string]struct{}, len(locations))
	filenames := make(map[string]struct{}, len(locations))
	for _, loc := range locations {
		states[loc.State] = struct{}{}
		filenames[loc.Filename] = struct{}{}
	}
	if len(states) == 1 && len(filenames) == 1 {
		dep.Filename = locations[0].Filename
	}
	if dep.State == "" || dep.State == "external" {
		dep.State = strings.Join(sortedKeys(states), ",")
	}
	if dep.State == "" {
		dep.State = "unknown"
	}
	if dep.Filename == "" && len(filenames) == 1 {
		dep.Filename = locations[0].Filename
	}
}

func blockedReason(deps []blockingDependency) string {
	if len(deps) == 0 {
		return "task is blocked by unsatisfied dependencies"
	}
	parts := make([]string, 0, len(deps))
	for _, dep := range deps {
		if dep.Filename != "" {
			parts = append(parts, fmt.Sprintf("%s (%s/%s)", dep.ID, dep.State, dep.Filename))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", dep.ID, dep.State))
	}
	return "task is blocked by " + strings.Join(parts, ", ")
}

func blockReasonString(reason dag.BlockReason) string {
	switch reason {
	case dag.BlockedByWaiting:
		return queue.DirWaiting
	case dag.BlockedByUnknown:
		return "unknown"
	case dag.BlockedByExternal:
		return "external"
	case dag.BlockedByAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func snapshotTaskID(snap *queue.TaskSnapshot) string {
	if snap.Meta.ID != "" {
		return snap.Meta.ID
	}
	return frontmatter.TaskFileStem(snap.Filename)
}
