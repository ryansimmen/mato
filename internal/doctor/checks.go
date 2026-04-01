package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/dag"
	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/process"
	"mato/internal/queue"
	"mato/internal/runner"
)

// checkContext carries shared state across checks within a single doctor run.
type checkContext struct {
	ctx       context.Context
	repoInput string
	repoRoot  string // populated by resolveRepo on success
	repoErr   error  // populated by resolveRepo on failure
	tasksDir  string // derived from repoRoot
	opts      Options
	idx       *queue.PollIndex // lazily built, shared across checks
}

var osStatFn = os.Stat

// hasRepo returns true if the git check resolved a valid repo root.
func (c *checkContext) hasRepo() bool {
	return c.repoRoot != ""
}

// hasTasksDir returns true if tasksDir is resolved.
func (c *checkContext) hasTasksDir() bool {
	return c.tasksDir != ""
}

// resolveRepo attempts to resolve repoRoot from repoInput using git.
// It populates c.repoRoot on success or c.repoErr on failure. This is
// called unconditionally before the check loop so that --only filters
// that skip "git" still have access to the repo root for deriving
// tasksDir.
func (c *checkContext) resolveRepo() {
	if c.repoRoot != "" {
		return
	}
	out, err := exec.CommandContext(c.ctx, "git", "-C", c.repoInput, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		c.repoErr = err
		return
	}
	c.repoRoot = strings.TrimSpace(string(out))
}

// repoErrDetail returns a human-readable description of the repo
// resolution failure, extracting stderr from exec.ExitError when
// available so callers don't get a bare "exit status 128".
func (c *checkContext) repoErrDetail() string {
	if c.repoErr == nil {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(c.repoErr, &exitErr) {
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		if stderr != "" {
			return stderr
		}
	}
	return c.repoErr.Error()
}

// ensureIndex lazily builds the PollIndex from tasksDir.
func (c *checkContext) ensureIndex() *queue.PollIndex {
	if c.idx == nil {
		c.idx = queue.BuildIndex(c.tasksDir)
	}
	return c.idx
}

// checkDef associates a check name with its implementation.
type checkDef struct {
	name string
	run  func(*checkContext) CheckReport
}

// checks is the ordered list of all health checks.
var checks = []checkDef{
	{"git", checkGit},
	{"tools", checkTools},
	{"docker", checkDocker},
	{"queue", checkQueueLayout},
	{"tasks", checkTaskParsing},
	{"locks", checkLocksAndOrphans},
	{"hygiene", checkHygiene},
	{"deps", checkDependencies},
}

// Function variable hooks for test injection.
var inspectHostToolsFn = runner.InspectHostTools

var dockerLookPathFn = func() error {
	_, err := exec.LookPath("docker")
	return err
}

var dockerProbe = func(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run()
}

// ExportInspectHostToolsFn returns the current inspectHostToolsFn for saving
// and restoring in integration tests.
func ExportInspectHostToolsFn() func() runner.ToolReport {
	return inspectHostToolsFn
}

// SetInspectHostToolsFn overrides inspectHostToolsFn for testing.
func SetInspectHostToolsFn(fn func() runner.ToolReport) {
	inspectHostToolsFn = fn
}

// ExportDockerLookPathFn returns the current dockerLookPathFn for saving
// and restoring in integration tests.
func ExportDockerLookPathFn() func() error {
	return dockerLookPathFn
}

// SetDockerLookPathFn overrides dockerLookPathFn for testing.
func SetDockerLookPathFn(fn func() error) {
	dockerLookPathFn = fn
}

// ExportDockerProbe returns the current dockerProbe for saving and restoring
// in integration tests.
func ExportDockerProbe() func(context.Context) error {
	return dockerProbe
}

// SetDockerProbe overrides dockerProbe for testing.
func SetDockerProbe(fn func(context.Context) error) {
	dockerProbe = fn
}

// dockerImageInspectFn checks whether a Docker image is available locally.
var dockerImageInspectFn = func(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run()
}

// dockerImagePullFn pulls a Docker image. Used by --fix mode.
var dockerImagePullFn = func(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExportDockerImageInspectFn returns the current dockerImageInspectFn for
// saving and restoring in integration tests.
func ExportDockerImageInspectFn() func(context.Context, string) error {
	return dockerImageInspectFn
}

// SetDockerImageInspectFn overrides dockerImageInspectFn for testing.
func SetDockerImageInspectFn(fn func(context.Context, string) error) {
	dockerImageInspectFn = fn
}

// ExportDockerImagePullFn returns the current dockerImagePullFn for saving
// and restoring in integration tests.
func ExportDockerImagePullFn() func(context.Context, string) error {
	return dockerImagePullFn
}

// SetDockerImagePullFn overrides dockerImagePullFn for testing.
func SetDockerImagePullFn(fn func(context.Context, string) error) {
	dockerImagePullFn = fn
}

// resolveDockerImage returns the configured Docker image name from the
// environment, falling back to the default ubuntu:24.04.
func resolveDockerImage() string {
	if img := strings.TrimSpace(os.Getenv("MATO_DOCKER_IMAGE")); img != "" {
		return img
	}
	return "ubuntu:24.04"
}

// ---------- A. Git Repository ----------

func checkGit(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "git", Status: CheckRan, Findings: []Finding{}}

	// resolveRepo was already called before the check loop, so
	// cc.repoRoot is populated on success and cc.repoErr on failure.
	if cc.repoRoot == "" {
		code := "git.resolve_failed"
		detail := cc.repoErrDetail()
		msg := fmt.Sprintf("failed to resolve git repository: %s", detail)

		// Only classify as "not a repo" when git itself says so.
		if strings.Contains(detail, "not a git repository") {
			code = "git.not_a_repo"
			msg = fmt.Sprintf("not a git repository: %s", cc.repoInput)
		}

		cr.Findings = append(cr.Findings, Finding{
			Code:     code,
			Severity: SeverityError,
			Message:  msg,
			Path:     cc.repoInput,
		})
		return cr
	}

	repoRoot := cc.repoRoot

	cr.Findings = append(cr.Findings, Finding{
		Code:     "git.repo_root",
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("repo root: %s", repoRoot),
	})

	// Current branch.
	branchOut, err := exec.CommandContext(cc.ctx, "git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		branch := strings.TrimSpace(string(branchOut))
		cr.Findings = append(cr.Findings, Finding{
			Code:     "git.branch",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("current branch: %s", branch),
		})
	}

	return cr
}

// ---------- B. Host Tools ----------

func checkTools(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "tools", Status: CheckRan, Findings: []Finding{}}

	report := inspectHostToolsFn()
	for _, tf := range report.Findings {
		var sev Severity
		if !tf.Found {
			if tf.Required {
				sev = SeverityError
			} else {
				sev = SeverityWarning
			}
		} else {
			sev = SeverityInfo
		}

		code := "tools.found"
		if !tf.Found {
			if tf.Required {
				if tf.Name == ".copilot" {
					code = "tools.missing_copilot_dir"
				} else {
					code = "tools.missing_required"
				}
			} else {
				code = "tools.missing_optional"
			}
		}

		cr.Findings = append(cr.Findings, Finding{
			Code:     code,
			Severity: sev,
			Message:  tf.Message,
			Path:     tf.Path,
		})
	}

	return cr
}

// ---------- C. Docker ----------

func checkDocker(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "docker", Status: CheckRan, Findings: []Finding{}}

	// Check if docker CLI exists.
	if err := dockerLookPathFn(); err != nil {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "docker.cli_missing",
			Severity: SeverityError,
			Message:  "docker CLI not found in PATH",
		})
		return cr
	}

	if err := dockerProbe(cc.ctx); err != nil {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "docker.daemon_unreachable",
			Severity: SeverityError,
			Message:  fmt.Sprintf("docker daemon unreachable: %v", err),
		})
		return cr
	}

	cr.Findings = append(cr.Findings, Finding{
		Code:     "docker.reachable",
		Severity: SeverityInfo,
		Message:  "docker daemon reachable",
	})

	// Check if the configured Docker image is available locally.
	image := cc.opts.DockerImage
	if image == "" {
		image = resolveDockerImage()
	}
	if err := dockerImageInspectFn(cc.ctx, image); err != nil {
		f := Finding{
			Code:     "docker.image_missing",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("Docker image %s not found locally", image),
			Fixable:  true,
		}

		if cc.opts.Fix {
			if pullErr := dockerImagePullFn(cc.ctx, image); pullErr != nil {
				f.Message = fmt.Sprintf("Docker image %s not found locally; pull failed: %v", image, pullErr)
				f.Severity = SeverityError
				f.Fixable = false
			} else {
				f.Fixed = true
				f.Fixable = false
				f.Message = fmt.Sprintf("Docker image %s pulled successfully", image)
			}
		}

		cr.Findings = append(cr.Findings, f)
	} else {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "docker.image_available",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("Docker image %s available locally", image),
		})
	}

	return cr
}

// ---------- D. Queue Layout ----------

func checkQueueLayout(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "queue", Status: CheckRan, Findings: []Finding{}}

	// Check if tasksDir itself exists.
	if info, err := osStatFn(cc.tasksDir); err != nil {
		if os.IsNotExist(err) {
			// Missing root .mato is fixable — create it along with
			// all expected subdirectories below.
			f := Finding{
				Code:     "queue.missing_tasks_root",
				Severity: SeverityError,
				Message:  fmt.Sprintf("tasks directory does not exist: %s", cc.tasksDir),
				Path:     cc.tasksDir,
				Fixable:  true,
			}
			if cc.opts.Fix {
				if mkErr := os.MkdirAll(cc.tasksDir, 0o755); mkErr == nil {
					f.Fixed = true
					f.Fixable = false
				}
			}
			cr.Findings = append(cr.Findings, f)
		} else {
			// Non-ENOENT error (permission denied, etc.) is a hard error.
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.unreadable_tasks_dir",
				Severity: SeverityError,
				Message:  fmt.Sprintf("tasks directory not readable: %v", err),
				Path:     cc.tasksDir,
			})
			return cr
		}
	} else if !info.IsDir() {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "queue.not_a_directory",
			Severity: SeverityError,
			Message:  fmt.Sprintf("tasks path exists but is not a directory: %s", cc.tasksDir),
			Path:     cc.tasksDir,
		})
		return cr
	}

	// Check for expected directories.
	var expectedDirs []string
	for _, d := range queue.AllDirs {
		expectedDirs = append(expectedDirs, d)
	}
	expectedDirs = append(expectedDirs, ".locks")
	for _, md := range messaging.MessagingDirs {
		expectedDirs = append(expectedDirs, md)
	}

	for _, dir := range expectedDirs {
		dirPath := filepath.Join(cc.tasksDir, dir)
		info, err := osStatFn(dirPath)
		if os.IsNotExist(err) {
			f := Finding{
				Code:     "queue.missing_dir",
				Severity: SeverityError,
				Message:  fmt.Sprintf("missing directory: %s", dir),
				Path:     dirPath,
				Fixable:  true,
			}

			if cc.opts.Fix {
				if mkErr := os.MkdirAll(dirPath, 0o755); mkErr == nil {
					// Re-scan to verify.
					if _, statErr := osStatFn(dirPath); statErr == nil {
						f.Fixed = true
						f.Fixable = false
					}
				}
			}

			cr.Findings = append(cr.Findings, f)
		} else if err != nil {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.unreadable_dir",
				Severity: SeverityError,
				Message:  fmt.Sprintf("could not stat expected directory %s: %v", dir, err),
				Path:     dirPath,
			})
		} else if err == nil && !info.IsDir() {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.not_a_directory",
				Severity: SeverityError,
				Message:  fmt.Sprintf("expected directory but found a file: %s", dir),
				Path:     dirPath,
			})
		}
	}

	// Build index to get per-directory counts and build warnings.
	idx := cc.ensureIndex()

	// Surface directory-level read errors from BuildWarnings.
	for _, bw := range idx.BuildWarnings() {
		// Only report directory-level errors here (not glob warnings).
		// Glob warnings are handled in the tasks check.
		if !strings.Contains(bw.Err.Error(), "glob") && !strings.Contains(bw.Err.Error(), "affects") {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.read_error",
				Severity: SeverityError,
				Message:  fmt.Sprintf("directory read error: %v", bw.Err),
				Path:     bw.Path,
			})
		}
	}

	// Per-directory task counts.
	for _, dir := range queue.AllDirs {
		tasks := idx.TasksByState(dir)
		if len(tasks) > 0 {
			cr.Findings = append(cr.Findings, Finding{
				Code:     "queue.dir_count",
				Severity: SeverityInfo,
				Message:  fmt.Sprintf("%s: %d tasks", dir, len(tasks)),
			})
		}
	}

	return cr
}

// ---------- E. Task Parsing ----------

func checkTaskParsing(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "tasks", Status: CheckRan, Findings: []Finding{}}

	idx := cc.ensureIndex()

	// Parse failures.
	for _, pf := range idx.ParseFailures() {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "tasks.parse_error",
			Severity: SeverityError,
			Message:  fmt.Sprintf("%s/%s: %v", pf.State, pf.Filename, pf.Err),
			Path:     pf.Path,
		})
	}

	// Invalid glob and unsafe affects errors from BuildWarnings — error
	// severity to match the runtime, which quarantines affected tasks into
	// failed/.
	for _, bw := range idx.BuildWarnings() {
		errMsg := bw.Err.Error()
		if strings.Contains(errMsg, "glob") || strings.Contains(errMsg, "affects") {
			code := "tasks.invalid_glob"
			if strings.Contains(errMsg, "unsafe affects") {
				code = "tasks.unsafe_affects"
			}
			cr.Findings = append(cr.Findings, Finding{
				Code:     code,
				Severity: SeverityError,
				Message:  fmt.Sprintf("%s: %v", filepath.Base(bw.Path), bw.Err),
				Path:     bw.Path,
			})
		}
	}

	// Total parsed count.
	total := 0
	for _, dir := range queue.AllDirs {
		total += len(idx.TasksByState(dir))
	}
	cr.Findings = append(cr.Findings, Finding{
		Code:     "tasks.total_count",
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("total parsed tasks: %d", total),
	})

	return cr
}

// ---------- F. Locks & Orphans ----------

func checkLocksAndOrphans(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "locks", Status: CheckRan, Findings: []Finding{}}

	locksDir := filepath.Join(cc.tasksDir, ".locks")

	// Verify .locks is readable before scanning.
	if _, err := os.ReadDir(locksDir); err != nil {
		sev := SeverityError
		code := "locks.unreadable"
		msg := fmt.Sprintf("cannot read locks directory: %v", err)
		if os.IsNotExist(err) {
			// Missing .locks is already caught by queue-layout check.
			// Report as warning here since it may have been created
			// by --fix in the queue check.
			sev = SeverityWarning
			code = "locks.missing_dir"
			msg = fmt.Sprintf("locks directory does not exist: %s", locksDir)
		}
		cr.Findings = append(cr.Findings, Finding{
			Code:     code,
			Severity: sev,
			Message:  msg,
			Path:     locksDir,
		})
		return cr
	}

	// Scan for stale .pid files.
	cr.Findings = append(cr.Findings, scanStalePIDLocks(locksDir, cc.opts.Fix, cc.tasksDir)...)

	// Scan for stale review locks.
	cr.Findings = append(cr.Findings, scanStaleReviewLocks(locksDir, cc.opts.Fix)...)

	// Scan for orphaned in-progress tasks.
	cr.Findings = append(cr.Findings, scanOrphanedTasks(cc.tasksDir, cc.opts.Fix)...)

	// Count active agents.
	activeCount := countActiveAgents(locksDir)
	if activeCount > 0 {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "locks.active_agents",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("active agent registrations: %d", activeCount),
		})
	}

	return cr
}

// scanStalePIDLocks checks for stale agent .pid files.
func scanStalePIDLocks(locksDir string, fix bool, tasksDir string) []Finding {
	var findings []Finding
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return findings
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".pid")
		status, err := identity.DescribeAgentActivity(tasksDir, agentID)
		if err != nil {
			findings = append(findings, Finding{
				Code:     "locks.unreadable_pid",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("unreadable agent lock: %s", e.Name()),
				Path:     filepath.Join(locksDir, e.Name()),
			})
			continue
		}
		if status == identity.AgentActive {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())

		findings = append(findings, Finding{
			Code:     "locks.stale_pid",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("stale agent lock: %s", e.Name()),
			Path:     lockPath,
			Fixable:  true,
		})
	}

	if fix && len(findings) > 0 {
		queue.CleanStaleLocks(tasksDir)
		for i := range findings {
			if _, statErr := os.Stat(findings[i].Path); os.IsNotExist(statErr) {
				findings[i].Fixed = true
				findings[i].Fixable = false
			}
		}
	}
	return findings
}

// scanStaleReviewLocks checks for stale review-*.lock files.
func scanStaleReviewLocks(locksDir string, fix bool) []Finding {
	var findings []Finding
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return findings
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())
		if lockfile.IsHeld(lockPath) {
			continue
		}

		f := Finding{
			Code:     "locks.stale_review",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("stale review lock: %s", e.Name()),
			Path:     lockPath,
			Fixable:  true,
		}

		if fix {
			os.Remove(lockPath)
			// Re-scan to verify.
			if _, statErr := os.Stat(lockPath); os.IsNotExist(statErr) {
				f.Fixed = true
				f.Fixable = false
			}
		}

		findings = append(findings, f)
	}
	return findings
}

// scanOrphanedTasks checks for in-progress tasks claimed by dead agents.
func scanOrphanedTasks(tasksDir string, fix bool) []Finding {
	var findings []Finding

	inProgress := filepath.Join(tasksDir, queue.DirInProgress)
	names, err := queue.ListTaskFiles(inProgress)
	if err != nil {
		return findings
	}

	for _, name := range names {
		src := filepath.Join(inProgress, name)

		// Check for stale duplicate (already in a later state).
		if laterDir := laterStateDuplicateDir(tasksDir, name); laterDir != "" {
			findings = append(findings, Finding{
				Code:     "locks.stale_duplicate",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("stale in-progress copy of %s (already in %s/)", name, laterDir),
				Path:     src,
				Fixable:  true,
			})
			continue
		}

		// Check claim marker and agent liveness.
		agent := queue.ParseClaimedBy(src)
		if agent != "" {
			status, err := identity.DescribeAgentActivity(tasksDir, agent)
			if err != nil {
				findings = append(findings, Finding{
					Code:     "locks.unreadable_pid",
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("could not verify claimed-by lock for %s: %v", name, err),
					Path:     src,
				})
				continue
			}
			if status == identity.AgentActive {
				continue // agent is alive, skip
			}
		}

		if agent == "" {
			// No valid claimed-by marker — different corruption case.
			findings = append(findings, Finding{
				Code:     "locks.unclaimed_in_progress",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("in-progress task %s has no claimed-by marker", name),
				Path:     src,
				Fixable:  true,
			})
		} else {
			// Valid claim marker but agent is dead.
			findings = append(findings, Finding{
				Code:     "locks.orphaned_task",
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("in-progress task %s claimed by dead agent %s", name, agent),
				Path:     src,
				Fixable:  true,
			})
		}
	}

	if fix && len(findings) > 0 {
		_ = queue.RecoverOrphanedTasks(tasksDir)
		for i := range findings {
			if _, statErr := os.Stat(findings[i].Path); os.IsNotExist(statErr) {
				findings[i].Fixed = true
				findings[i].Fixable = false
			}
		}
	}
	return findings
}

// laterStateDuplicateDir mirrors the check in queue.go.
func laterStateDuplicateDir(tasksDir, name string) string {
	for _, laterDir := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed} {
		if _, err := os.Stat(filepath.Join(tasksDir, laterDir, name)); err == nil {
			return laterDir
		}
	}
	return ""
}

// countActiveAgents counts .pid files for live agents.
func countActiveAgents(locksDir string) int {
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())
		if lockfile.IsHeld(lockPath) {
			count++
		}
	}
	return count
}

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
		// No merge.lock is the normal case.
		return findings
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
			os.Remove(mergeLockPath)
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
			os.Remove(mergeLockPath)
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
// the primary path (.*.tmp-*), the EXDEV cross-device fallback (.*.xdev-*)
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
	scanDirs := make([]string, 0, len(queue.AllDirs)+len(messaging.MessagingDirs))
	for _, d := range queue.AllDirs {
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

// ---------- H. Dependency Integrity ----------

func checkDependencies(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "deps", Status: CheckRan, Findings: []Finding{}}

	idx := cc.ensureIndex()
	diag := queue.DiagnoseDependencies(cc.tasksDir, idx)

	// Map DependencyIssue entries to Finding structs.
	for _, issue := range diag.Issues {
		var code string
		var sev Severity
		var msg string

		switch issue.Kind {
		case queue.DependencyAmbiguousID:
			code = "deps.ambiguous_id"
			sev = SeverityWarning
			msg = fmt.Sprintf("task ID %q exists in both completed and non-completed directories", issue.TaskID)
		case queue.DependencyDuplicateID:
			code = "deps.duplicate_id"
			sev = SeverityError
			msg = fmt.Sprintf("duplicate waiting task ID %q (retained: %s, duplicate: %s) — duplicate will be moved to failed/", issue.TaskID, issue.DependsOn, issue.Filename)
		case queue.DependencySelfCycle:
			code = "deps.self_dependency"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q depends on itself", issue.TaskID)
		case queue.DependencyCycle:
			code = "deps.cycle"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q is part of a circular dependency", issue.TaskID)
		case queue.DependencyUnknownID:
			code = "deps.unknown_id"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q depends on unknown ID %q", issue.TaskID, issue.DependsOn)
		default:
			code = "deps.unknown_issue"
			sev = SeverityWarning
			msg = fmt.Sprintf("task %q: unknown issue kind %s", issue.TaskID, issue.Kind)
		}

		f := Finding{
			Code:     code,
			Severity: sev,
			Message:  msg,
		}
		if issue.Filename != "" {
			f.Path = filepath.Join(cc.tasksDir, queue.DirWaiting, issue.Filename)
		}
		cr.Findings = append(cr.Findings, f)
	}

	// Surface blocked-by-external and blocked-by-ambiguous as INFO.
	for taskID, details := range diag.Analysis.Blocked {
		for _, detail := range details {
			switch detail.Reason {
			case dag.BlockedByExternal:
				cr.Findings = append(cr.Findings, Finding{
					Code:     "deps.blocked_external",
					Severity: SeverityInfo,
					Message:  fmt.Sprintf("task %q blocked by %q (in non-completed, non-waiting state)", taskID, detail.DependencyID),
				})
			case dag.BlockedByAmbiguous:
				cr.Findings = append(cr.Findings, Finding{
					Code:     "deps.blocked_ambiguous",
					Severity: SeverityInfo,
					Message:  fmt.Sprintf("task %q blocked by ambiguous ID %q", taskID, detail.DependencyID),
				})
			}
		}
	}

	blockedBacklog := queue.DependencyBlockedBacklogTasksDetailed(cc.tasksDir, idx)
	blockedNames := make([]string, 0, len(blockedBacklog))
	for name := range blockedBacklog {
		blockedNames = append(blockedNames, name)
	}
	sort.Strings(blockedNames)
	for _, name := range blockedNames {
		details := blockedBacklog[name]
		cr.Findings = append(cr.Findings, Finding{
			Code:     "deps.backlog_blocked",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("backlog task %q is dependency-blocked and should be in waiting/ (blocked by %s)", name, queue.FormatDependencyBlocks(details)),
			Path:     filepath.Join(cc.tasksDir, queue.DirBacklog, name),
		})
	}

	// Deps-satisfied count.
	depsSatisfied := len(diag.Analysis.DepsSatisfied)
	if depsSatisfied > 0 {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "deps.satisfied_count",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("deps-satisfied waiting tasks: %d", depsSatisfied),
		})
	}

	return cr
}
