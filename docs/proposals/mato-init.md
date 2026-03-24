# `mato init` — Implementation Plan

## 1. Goal

Add a `mato init` subcommand that sets up a repository for mato use in a
single, explicit step. Currently, initialization logic is embedded inside
`runner.Run()` and requires Docker + Copilot. This creates friction:

- `mato --dry-run` **fails** on uninitialized repos ("run mato once to create
  them").
- Users need the full orchestrator stack just to create a directory layout.
- There is no dedicated, lightweight setup command.

`mato init` provides an idempotent initialization command that creates the
`.tasks/` tree, updates `.gitignore`, and ensures the target branch has at
least one commit.

## 2. Scope

### In Scope

- New `init` Cobra subcommand in `cmd/mato/main.go`.
- New `internal/setup/` package with `InitRepo()` function.
- New `EnsureIdentity()` in `internal/git/git.go` (shared helper).
- Refactor runner's `resolveGitIdentity()` to delegate to
  `git.EnsureIdentity()`.
- Full test coverage: unit, command, integration.
- Docs: `README.md`, `docs/configuration.md`, `docs/architecture.md`,
  `AGENTS.md`.
- Update DryRun error message.

### Out of Scope

- Task scaffolding (proposed separately as `mato new`).
- Running Docker or launching agents.
- Interactive prompts or wizard-style setup.
- Changing runner execution order or Docker gate behavior.
- `receive.denyCurrentBranch` (runtime concern, applied lazily during
  task/review execution).
- Fixing runner's hardcoded `/.tasks/` gitignore for custom tasks-dirs
  (pre-existing limitation).
- Extending DryRun to validate `.locks` / messaging dirs (pre-existing scope).
- Doctor `--fix` or testutil reuse of `InitRepo`.

## 3. Design

### CLI Specification

```text
mato init [flags]

Flags:
  --repo <path>        Path to the git repository (default: current directory)
  --branch <name>      Target branch name (default: mato)
  --tasks-dir <path>   Path to the tasks directory (default: <repo>/.tasks)
```

Normal Cobra subcommand (no `DisableFlagParsing`).

### CLI Validation Flow (`newInitCmd().RunE`)

1. `resolveRepo()` — default CWD.
2. `validateRepoPath()` — exists, is dir, is git repo.
3. `resolveRepoRoot()` — canonicalize via `git rev-parse --show-toplevel`.
4. `validateBranch()` — `git check-ref-format`.
5. **Resolve tasks dir** — if relative, resolve against `repoRoot` (not CWD).
   This is more intuitive for a setup command: `mato init --repo /path/to/repo
   --tasks-dir custom` creates `/path/to/repo/custom`, not `$CWD/custom`.
6. `setup.InitRepo(repoRoot, branch, tasksDir)`.
7. Format and print result.

> **Note on relative tasks-dir**: The `mato init` command resolves relative
> `--tasks-dir` values against the resolved repo root. This differs from the
> legacy runner behavior which uses `filepath.Abs()` (CWD-relative) in
> `validateTasksDir()`. The init-specific behavior is intentional: since init
> is a setup command that takes `--repo`, users expect paths relative to the
> repo, not the shell's CWD.

### Package Structure

**`internal/setup/`** — New package owning repo bootstrap orchestration.
Imports `mato/internal/git`, `mato/internal/messaging`, `mato/internal/dirs`.
No import cycle risk (none of these import `setup`).

This separates repo-level orchestration (branch checkout, identity, gitignore,
dir creation) from queue-state management (`internal/queue/`). The `queue`
package continues to own task claiming, manifests, reconciliation, and overlap
detection. The `setup` package composes existing helpers into a single init
workflow.

### Design Decision: Runner and Init Are Separate Paths

`runner.Run()` keeps its existing inline initialization logic and execution
order (branch → Docker gate → dirs → messaging → identity → gitignore). The
new `setup.InitRepo()` is used only by `mato init`.

**Rationale**: Runner has a legitimate need for its ordering — the Docker gate
must run before most setup steps, and changing runner's execution order is a
behavioral change beyond this plan's scope. The two paths share low-level
helpers (`git.EnsureIdentity()`, `git.EnsureBranch()`, `git.EnsureGitignoreContains()`,
`messaging.Init()`, `dirs.All`, `dirs.Locks`) which prevents implementation
drift at the level that matters. The sequencing differences (Docker gate
position, `.locks` creation timing, relative-path resolution) are intentional.

To prevent silent drift, the integration tests verify that running `mato init`
followed by `runner.DryRun()` produces no warnings — i.e., init creates
everything runner expects.

### `git.EnsureIdentity()` — Shared Helper

Extracted from runner's `resolveGitIdentity()` (runner.go line 346):

```go
// EnsureIdentity ensures git user.name and user.email are set in the local
// repo config. Reads local → global → defaults ("mato" / "mato@local.invalid").
// Config writes are best-effort (errors are silently ignored, matching the
// existing runner behavior). Returns the resolved name and email.
func EnsureIdentity(repoRoot string) (name, email string) {
    name, email = ResolveIdentity(repoRoot)
    Output(repoRoot, "config", "user.name", name)
    Output(repoRoot, "config", "user.email", email)
    return name, email
}
```

Runner's `resolveGitIdentity()` becomes `return git.EnsureIdentity(repoRoot)`.

### `setup.InitRepo()`

**File**: `internal/setup/setup.go`

```go
// InitResult describes what the initialization did.
type InitResult struct {
    DirsCreated      []string // newly created dirs (relative to tasksDir)
    GitignoreUpdated bool     // whether .gitignore was modified
    GitignoreSkipped bool     // true when tasks dir is outside the repo
    BranchName       string   // target branch name
    BranchExisted    bool     // true if local branch ref existed before init
    AlreadyOnBranch  bool     // true if HEAD was already on target branch
    TasksDir         string   // resolved absolute path
}

// InitRepo performs the full non-Docker initialization of a mato repository.
// All steps are idempotent. The tasksDir parameter must be an absolute path
// or empty (defaults to <repoRoot>/.tasks).
func InitRepo(repoRoot, branch, tasksDir string) (*InitResult, error)
```

### Tasks Dir Resolution (inside InitRepo)

1. Empty → `filepath.Join(repoRoot, ".tasks")`.
2. Must be absolute (caller is responsible for resolution).
3. Verify parent exists and is a directory.
4. Reject if equals `repoRoot`: `fmt.Errorf("tasks directory must not be the
   repository root %s", repoRoot)`.

### Execution Order (inside InitRepo)

1. **Resolve and validate tasksDir**.

2. **Probe repo state**:
   - `git symbolic-ref --short HEAD` → current branch name (failure on
     detached HEAD → treat as "not on target").
   - `git rev-parse --verify refs/heads/<branch>` → whether target branch
     ref exists → `BranchExisted`.
   - `git rev-parse HEAD` → whether repo has any commits (determines "empty
     repo" state).

3. **Reject**: Empty repo + tasks dir outside repo → error.

4. **Branch handling**:
   - If HEAD already on target (born or unborn, via symbolic-ref) →
     `AlreadyOnBranch = true`, skip `EnsureBranch`.
   - Otherwise → `git.EnsureBranch(repoRoot, branch)`.
     - On non-empty repos: follows existing branch checkout/create flow.
     - On empty repos with unborn HEAD on a different branch (e.g., `main`):
       `EnsureBranch` falls through to `git checkout -b <target>`, which
       switches the unborn ref to the target branch. This is valid git
       behavior.

5. **Create queue + lock directories** — private `initDirs(tasksDir)` using
   `dirs.All` + `dirs.Locks`. Pre-check with `os.Stat`, create with
   `os.MkdirAll(path, 0o755)`, track newly created.

6. **Create messaging directories** — `messaging.Init(tasksDir)`. Track newly
   created by checking existence before/after.

7. **Ensure git identity** — `git.EnsureIdentity(repoRoot)`. Best-effort.

8. **Update `.gitignore`**:
   - `computeIgnorePattern(repoRoot, tasksDir)`:
     - Inside repo → `git.EnsureGitignoreContains(repoRoot, pattern)`. If
       modified → `git.CommitGitignore(repoRoot, "chore: add "+pattern+" to .gitignore")`.
     - Outside repo → skip, `GitignoreSkipped = true`.

9. **Ensure born branch**: If target branch ref is still unborn (checked via
   `git rev-parse --verify refs/heads/<branch>`):
   - `git.Output(repoRoot, "add", "--", ".gitignore")`
   - `git.Output(repoRoot, "commit", "-m", "chore: initialize mato", "--", ".gitignore")`
   - Uses path-limited commit (only `.gitignore`). Other staged files are
     untouched.
   - `.gitignore` is guaranteed to exist because tasks dir is inside the repo
     (outside + empty is rejected in step 3), so step 8 either created or
     verified it.

10. **Return `InitResult`**.

### Gitignore Path Logic

```go
func computeIgnorePattern(repoRoot, tasksDir string) (string, bool) {
    rel, err := filepath.Rel(repoRoot, tasksDir)
    if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
        return "", false // outside repo
    }
    return "/" + filepath.ToSlash(rel) + "/", true
}
```

Cases:
- Default `<repo>/.tasks` → `/.tasks/`
- Custom inside repo `<repo>/custom/queue` → `/custom/queue/`
- Outside repo `/tmp/ext` → skip (returns false)
- Edge case `..tasks` dir → correctly treated as inside repo

### Runner — Zero Behavioral Change

Only change: `resolveGitIdentity()` body → `return git.EnsureIdentity(repoRoot)`.
Everything else stays exactly as-is: inline dir loop with `queue.AllDirs`,
Docker gate position, gitignore handling (hardcoded `/.tasks/`).

### DryRun

DryRun validates the 7 queue state directories from `queue.AllDirs`
(intentionally narrower than init's full footprint which also creates `.locks`
and messaging dirs). This is a pre-existing design choice. DryRun's error
message is updated: "run mato once" → "run `mato init`".

### Output Contract

| Condition | Message |
|-----------|---------|
| `len(DirsCreated) > 0` | `Created <dir>/` per dir |
| `len(DirsCreated) == 0` | `.tasks/ directory structure already exists` |
| `GitignoreUpdated` | `Added <pattern> to .gitignore` |
| `!GitignoreUpdated && !GitignoreSkipped` | `.gitignore already contains <pattern>` |
| `GitignoreSkipped` | `Skipped .gitignore (tasks directory is outside the repository)` |
| `!BranchExisted && !AlreadyOnBranch` | `Created branch: <name>` |
| `BranchExisted && !AlreadyOnBranch` | `Switched to branch: <name>` |
| `AlreadyOnBranch` | `Already on branch: <name>` |
| All no-ops | `Nothing to do — already initialized.` |
| Otherwise | `Ready to add tasks to <tasksDir>/backlog/` |

### Backward Compatibility

`runner.Run()` auto-initializes with its existing behavior. `mato init` is
recommended but not required. Custom `--tasks-dir` must be re-specified per
command (no persistent configuration).

## 4. Step-by-Step Breakdown

### Step 1: Add `git.EnsureIdentity()`

**File**: `internal/git/git.go`
**Dependency**: None.

### Step 2: Tests for `EnsureIdentity()`

**File**: `internal/git/git_test.go`
**Dependency**: Step 1.

### Step 3: Create `internal/setup/` package

**File**: `internal/setup/setup.go` (new)
Contains: `InitResult`, `InitRepo()`, `initDirs()`, `computeIgnorePattern()`.
Imports: `mato/internal/git`, `mato/internal/messaging`, `mato/internal/dirs`.
**Dependency**: Step 1.

### Step 4: Unit tests for setup package

**File**: `internal/setup/setup_test.go` (new)
**Dependency**: Step 3.

### Step 5: Add `newInitCmd()`

**File**: `cmd/mato/main.go`
Resolves repo to top-level. Resolves relative tasks-dir against repo root.
Registers: `root.AddCommand(newInitCmd())`.
**Dependency**: Step 3.

### Step 6: Command-level tests

**File**: `cmd/mato/main_test.go`
**Dependency**: Step 5.

### Step 7: Refactor `resolveGitIdentity()` and update DryRun

**File**: `internal/runner/runner.go`
- `resolveGitIdentity()` body → `return git.EnsureIdentity(repoRoot)`.
- DryRun error: "run mato once" → "run `mato init`".
**Dependency**: Step 1.

### Step 8: Integration tests

**File**: `internal/integration/init_test.go` (new)
**Dependency**: Steps 1, 3.

### Step 9: Documentation

**Files**: `README.md`, `docs/configuration.md`, `docs/architecture.md`,
`AGENTS.md`
**Dependency**: Steps 1–8.

## 5. File Changes

| File | Action | What Changes |
|------|--------|--------------|
| `internal/git/git.go` | Modify | Add `EnsureIdentity()` |
| `internal/git/git_test.go` | Modify | Add `TestEnsureIdentity_*` |
| `internal/setup/setup.go` | Create | `InitResult`, `InitRepo()`, `initDirs()`, `computeIgnorePattern()` |
| `internal/setup/setup_test.go` | Create | Unit tests |
| `cmd/mato/main.go` | Modify | Add `newInitCmd()`, register |
| `cmd/mato/main_test.go` | Modify | Add `TestInitCmd_*` |
| `internal/runner/runner.go` | Modify | Refactor `resolveGitIdentity()`, update DryRun message |
| `internal/integration/init_test.go` | Create | Integration tests |
| `README.md` | Modify | Quick Start, CLI usage |
| `docs/configuration.md` | Modify | Subcommand section |
| `docs/architecture.md` | Modify | Note init command, `internal/setup/` package |
| `AGENTS.md` | Modify | Add `setup/` to project layout |

## 6. Error Handling

**`setup.InitRepo()`**:
- `tasksDir == repoRoot` → `fmt.Errorf("tasks directory must not be the
  repository root %s", repoRoot)`.
- Empty repo + outside tasks dir → `fmt.Errorf("cannot initialize: repository
  has no commits and tasks directory is outside the repository; create an
  initial commit first or use the default tasks directory")`.
- Parent dir missing → `fmt.Errorf("tasks directory parent %s does not exist:
  %w", parent, err)`.
- Dir creation → `fmt.Errorf("create queue directory %s: %w", dir, err)`.
- Git errors propagated from helpers.
- All: lowercase verb, `%w`. Non-fatal conditions → result flags.

**`newInitCmd().RunE`**:
- Validates repo (exists, is dir, is git repo) before `InitRepo()`.
- Validates branch via `git check-ref-format`.
- Returns `error` → Cobra prints `mato error: <message>` and exits 1.

## 7. Testing Strategy

### `internal/git/git_test.go`

| Test | Verifies |
|------|----------|
| `TestEnsureIdentity_SetsDefaults` | No identity → "mato" / "mato@local.invalid" |
| `TestEnsureIdentity_PreservesExisting` | Existing identity unchanged |
| `TestEnsureIdentity_Idempotent` | Two calls → same result |

### `internal/setup/setup_test.go`

| Test | Verifies |
|------|----------|
| `TestInitDirs_CreatesAll` | All queue + lock dirs created |
| `TestInitDirs_Idempotent` | Second call returns empty list |
| `TestInitDirs_PartialExistence` | Only missing dirs created |
| `TestInitDirs_UnwritableParent` | Error returned |
| `TestInitRepo_CreatesAllDirs` | Queue + lock + messaging dirs |
| `TestInitRepo_Idempotent` | No-op on second call |
| `TestInitRepo_CustomTasksDirInsideRepo` | Correct gitignore pattern |
| `TestInitRepo_CustomTasksDirOutsideRepo` | GitignoreSkipped, no commit |
| `TestInitRepo_TasksDirEqualsRepoRoot` | Error |
| `TestInitRepo_BranchAlreadyExists` | BranchExisted=true |
| `TestInitRepo_AlreadyOnBranch` | AlreadyOnBranch=true |
| `TestInitRepo_DefaultTasksDir` | Defaults to .tasks |
| `TestInitRepo_UnwritableDir` | Wrapped error |
| `TestInitRepo_GitIdentitySet` | Identity configured after init |
| `TestInitRepo_EmptyRepo` | Branch born after init |
| `TestInitRepo_EmptyRepoUnbornOnTargetBranch` | Works, skips EnsureBranch |
| `TestInitRepo_EmptyRepoSwitchBranch` | Unborn on `main`, init `mato` |
| `TestInitRepo_EmptyRepoOutsideTasksDir` | Rejected with error |
| `TestInitRepo_EmptyRepoGitignorePreexisting` | Bootstrap commits only .gitignore |
| `TestInitRepo_EmptyRepoStagedFilesPreserved` | Staged files not in bootstrap |
| `TestComputeIgnorePattern_InsideRepo` | `/<rel>/`, true |
| `TestComputeIgnorePattern_OutsideRepo` | "", false |
| `TestComputeIgnorePattern_Default` | `/.tasks/` |
| `TestComputeIgnorePattern_Nested` | `/custom/queue/` |
| `TestComputeIgnorePattern_DotDotTasks` | Correctly inside repo |

### `cmd/mato/main_test.go`

| Test | Verifies |
|------|----------|
| `TestInitCmd_CreatesDirectoryStructure` | Full init in temp repo |
| `TestInitCmd_Idempotent` | Re-run succeeds |
| `TestInitCmd_InvalidRepo` | Non-git dir → error |
| `TestInitCmd_NonexistentRepo` | Missing path → error |
| `TestInitCmd_GitignoreUpdated` | Pattern in .gitignore |
| `TestInitCmd_BranchCreated` | Branch exists, checked out |
| `TestInitCmd_CustomBranch` | `--branch develop` |
| `TestInitCmd_CustomTasksDir` | `--tasks-dir` inside repo |
| `TestInitCmd_NoExtraArgs` | Rejected |
| `TestInitCmd_InvalidBranch` | Error |
| `TestInitCmd_OutputIdempotent` | "Nothing to do" message |
| `TestInitCmd_OutputOutsideRepo` | "Skipped .gitignore" message |
| `TestInitCmd_SubdirectoryRepo` | `--repo` subdirectory → root |
| `TestInitCmd_RelativeTasksDir` | Resolved against repo root |
| `TestInitCmd_Help` | `--help` works |

### `internal/integration/init_test.go`

| Test | Verifies |
|------|----------|
| `TestInitRepo_EndToEnd` | Full flow: dirs + gitignore + branch |
| `TestInitRepo_IdempotentEndToEnd` | Second call no-op |
| `TestInitRepo_ThenDryRunWorks` | DryRun passes after init |
| `TestInitRepo_NonDefaultBranch` | Gitignore commit on target branch |
| `TestInitRepo_OutsideRepoTasksDir` | No gitignore, dirs created |
| `TestInitRepo_GitIdentityBootstrap` | Works without pre-configured identity |
| `TestInitRepo_EmptyRepo` | All setup on empty repo |
| `TestInitRepo_EmptyRepoBootstrapCommit` | Born branch after init |
| `TestInitRepo_EmptyRepoSwitchBranch` | Unborn `main` → `mato` |

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Branch checkout surprises users | Documented; matches run mode behavior |
| Import cycles setup → git/messaging/dirs | Verified: none import setup |
| Unborn HEAD on target branch | Short-circuit via symbolic-ref |
| Unborn HEAD on non-target branch | EnsureBranch checkout -b switches unborn ref; tested |
| Bootstrap commit includes staged files | Path-limited: `git commit -- .gitignore` only |
| Empty repo + outside tasks dir | Rejected with clear error |
| Runner behavior change | None — only `resolveGitIdentity` body changes |
| Runner/init init paths diverge | Share low-level helpers; integration test verifies init+DryRun compatibility |
| Runner gitignore hardcoded to `/.tasks/` | Pre-existing limitation; out of scope |
| EnsureIdentity config writes fail | Best-effort matches existing runner behavior; git errors are clear |

## 9. Open Questions

None — all questions from review iterations have been resolved.

## 10. Evolution Notes

This plan went through 11 review iterations. Key design decisions that evolved:

1. **Runner shares init path** (rejected): Initially proposed having runner
   call `InitRepo()`. Rejected because runner's Docker gate must run between
   branch checkout and dir creation, and changing runner ordering is a
   behavioral change beyond scope. Runner and init share helpers, not
   orchestration.

2. **Package placement**: Started in `internal/queue/`, moved to
   `internal/setup/` because branch/identity/gitignore orchestration is a
   repo-level concern, not queue-state management.

3. **Empty repo support**: Iteratively hardened through multiple rounds.
   Final design: born-branch guarantee via path-limited bootstrap commit,
   rejection of empty-repo + outside-tasks-dir combination, and explicit
   handling of unborn HEAD on both target and non-target branches.

4. **Relative tasks-dir**: Init resolves against repo root (user-intuitive),
   while runner preserves legacy CWD-relative behavior.
