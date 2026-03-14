# Task Storage Location Evaluation for `mato`

## 1. Current behavior in `main.go`

Today `mato` treats task storage as a repo-local runtime directory:

- `run()` resolves the git toplevel, then sets `tasksDir := filepath.Join(repoRoot, ".tasks")`.
- It creates `backlog/`, `in-progress/`, `completed/`, and `failed/` under that directory.
- Agent liveness is tracked in `.tasks/.locks/` via PID files.
- `ensureGitignored()` appends `/.tasks/` to `.gitignore` and commits that change.
- `runOnce()` bind-mounts the host task directory into the container as `/workspace/.tasks`.
- Queue polling, stale-lock cleanup, and orphan recovery all operate on that host-side `tasksDir`.

That means the containers do **not** care where the task directory lives on the host. They only care that the host path is mounted into the container at `/workspace/.tasks`.

So changing storage location is mostly a host-side concern:

1. how `mato` discovers the directory
2. how stable the directory identity is across runs and re-clones
3. whether the directory is safe from git accidents
4. whether the path is easy to bind-mount into Docker
5. how stale directories get cleaned up

---

## 2. Evaluation criteria

For each candidate location, this document evaluates:

- **Discoverability** — can `mato` derive the path automatically, or does the user need configuration?
- **Persistence** — does the queue survive reboots, branch switches, and repo re-clones?
- **Multi-repo isolation** — do repo A and repo B get separate queues?
- **Docker mount implications** — what changes in `runOnce()` and bind mounts?
- **Git safety** — can task files accidentally end up in commits?
- **Multi-user safety** — what happens when multiple human users share one machine?
- **Cleanup story** — how do orphaned queues get pruned?
- **Portability** — does it work cleanly on Linux, macOS, and CI?

### Important note on `<repo-hash>`

For the external-storage options, the quality of the design depends heavily on what `repo-hash` means.

If `repo-hash` is derived from the **checkout path**, then the queue is still tied to one clone, just in a different directory.
That weakens the main benefit of moving storage out of the repo.

If `repo-hash` is derived from a **stable repo identity** — ideally a normalized git remote URL, with a fallback to canonical repo path for repos that have no remote — then the queue can survive deleting and re-cloning the repository.

For `mato`, the best default is:

1. try normalized `origin` URL (or another configured remote)
2. fall back to `filepath.EvalSymlinks(repoRoot)`
3. optionally include the target branch only if you want one queue per branch

I would keep the default namespace **per repository**, not per branch, because the current architecture already treats the queue as repo-wide state.

---

## 3. Option 1: In-repo `.tasks/` (current approach)

**Path:** `<repo>/.tasks/`

### Discoverability

**Excellent.** No extra lookup logic is needed. `mato` already has `repoRoot`, so `filepath.Join(repoRoot, ".tasks")` is trivial and deterministic.

### Persistence

**Mixed.**

- **Survives reboot:** yes
- **Survives branch switches:** yes, because the directory is gitignored runtime state, not branch content
- **Survives repo re-clones:** no
- **Survives deleting the checkout:** no

This is the biggest architectural drawback: the queue belongs to one checkout, not to the logical repository identity.

### Multi-repo isolation

**Good per checkout, not per logical repo.**

Each checkout gets its own `.tasks/`, so unrelated repos do not collide. But two clones of the same repo also get two unrelated queues, which may or may not be what you want.

### Docker mount implications

**Best possible ergonomics.** The current code already does:

```go
"-v", fmt.Sprintf("%s:%s/.tasks", tasksDir, workdir)
```

Because `tasksDir` lives inside the repo checkout, the mount path is easy to derive and easy for humans to inspect.

### Git safety

**Better than tracked files, but not fully safe.**

The current code mitigates risk by appending `/.tasks/` to `.gitignore`, but that still has several downsides:

- the first `mato` run mutates `.gitignore`
- `mato` makes a commit just to support runtime state
- force-adding or manually moving files can still bypass ignore rules
- the repo directory still contains runtime clutter that looks related to source control

So the risk is reduced, but not eliminated.

### Multi-user safety

**Weak to mixed.**

If multiple users share one checkout on one machine, they also share one task queue and one `.locks/` directory. That may be intentional in some environments, but it is not private and it is not strongly isolated.

The design also assumes that PID files represent active local processes. That works best when one user owns the working copy and its child processes.

### Cleanup story

**Simple but sloppy.**

Deleting the checkout deletes the queue, which is convenient. But long-lived checkouts can accumulate stale `completed/` and `failed/` tasks forever unless `mato` grows explicit cleanup commands.

### Portability

**Good.** Works anywhere git and Docker can see the repo checkout.

The main portability issue is organizational rather than technical: some CI or shared-workspace environments discourage runtime writes inside the checkout tree.

### Bottom line

This is a good prototype location because it is simple and obvious, but it is a poor long-term default because it couples queue state to a particular checkout and pollutes the repo with runtime data.

---

## 4. Option 2: Home directory `~/.mato/<repo-hash>/`

**Path:** `~/.mato/<repo-hash>/`

### Discoverability

**Good.** `mato` can derive it automatically with `os.UserHomeDir()` plus deterministic repo hashing.

There is no standards lookup required beyond finding the home directory, so implementation is straightforward.

### Persistence

**Strong.**

- **Survives reboot:** yes
- **Survives branch switches:** yes
- **Survives repo re-clones:** yes, if `repo-hash` is based on stable repo identity
- **Survives checkout deletion:** yes

This is a major improvement over `.tasks/` because queue state is no longer bound to a specific clone.

### Multi-repo isolation

**Strong.**

Per-repo hashing prevents repo A from colliding with repo B, and the top-level `~/.mato/` keeps all `mato` state grouped together.

### Docker mount implications

**Simple.** `runOnce()` can keep mounting the resolved host path to `/workspace/.tasks`.

From the container's perspective, nothing changes.
The host path is just no longer under `repoRoot`.

Operationally this is usually fine because Docker can normally mount paths from a user's home directory on Linux and macOS.

### Git safety

**Excellent.** The queue is outside the worktree, so accidental commits largely disappear.

An additional benefit is that `ensureGitignored()` becomes unnecessary for the default path, which removes an odd side effect from startup.

### Multi-user safety

**Good by default.** Each user gets a separate queue namespace under their own home directory.

That is safer than repo-local storage, but it also means two users working on the same repo do **not** automatically share a queue. In practice that is usually a feature, not a bug, unless you explicitly want a shared team queue on one host.

### Cleanup story

**Needs explicit garbage collection.**

Because the queue outlives the checkout, `~/.mato/` can accumulate orphaned repo directories after repos are deleted or renamed.

A good implementation would add a small metadata file, e.g.:

```text
~/.mato/<repo-hash>/metadata.json
```

with fields like:

- normalized repo identity
- last seen repo path
- last used timestamp
- target branch

Then `mato gc` could prune entries that have not been touched in a long time or no longer map to an accessible repo.

### Portability

**Very good.** Home directories exist on Linux, macOS, and nearly all CI systems.

The main downside is convention: `~/.mato` is easy and portable, but it does not follow the XDG Base Directory convention on Linux.

### Bottom line

This is a strong pragmatic default if simplicity matters more than strict standards compliance.
It solves the repo-pollution problem without adding much complexity.

---

## 5. Option 3: XDG data directory `$XDG_DATA_HOME/mato/<repo-hash>/`

**Path:** `$XDG_DATA_HOME/mato/<repo-hash>/`

If `XDG_DATA_HOME` is unset, the standard fallback is `~/.local/share/mato/<repo-hash>/`.

### Discoverability

**Good.** `mato` can derive this automatically from the environment and fall back to the XDG default.

This is slightly more logic than `~/.mato`, but still simple.

### Persistence

**Strong.** Same persistence profile as the home-directory approach:

- **Survives reboot:** yes
- **Survives branch switches:** yes
- **Survives repo re-clones:** yes, if `repo-hash` is stable
- **Survives checkout deletion:** yes

### Multi-repo isolation

**Strong.** Same as `~/.mato/<repo-hash>/`.

### Docker mount implications

**Simple.** Exactly the same host/container bind-mount shape as any other external directory.

The code change is just in the host-side task-dir resolver; the container can still see `/workspace/.tasks`.

### Git safety

**Excellent.** The queue is outside the repo, so there is effectively no accidental-commit risk.

As with `~/.mato`, moving here removes the need for the startup `.gitignore` commit.

### Multi-user safety

**Good.** XDG data directories are naturally per-user, which gives better default isolation than repo-local or sibling storage.

### Cleanup story

**Needs explicit garbage collection, but the responsibility is clear.**

Because this is explicitly application data, it is natural for `mato` to own cleanup policies here.
That is cleaner than letting repo directories silently fill with runtime artifacts.

### Portability

**Good, with one caveat.**

- **Linux:** best fit; this is the platform convention
- **macOS:** works, but it is not the native Apple convention
- **CI:** usually fine, as long as `HOME` exists for the XDG fallback

So this is portable enough, but slightly more Linux-centric than `~/.mato`.

### Bottom line

This is the **best standards-aligned default** for a Linux-first CLI like `mato`.
It has the same practical benefits as `~/.mato`, but in a more conventional home for application data.

---

## 6. Option 4: Sibling directory `<repo>/../.mato-<repo-name>/`

**Path:** `<repo>/../.mato-<repo-name>/`

In practice, I would include a short hash too, e.g. `.mato-<repo-name>-<short-hash>/`, because repo names alone are not globally unique.

### Discoverability

**Good.** It is easy to derive from `repoRoot` without extra configuration.

### Persistence

**Mixed.**

- **Survives reboot:** yes
- **Survives branch switches:** yes
- **Survives repo re-clones:** usually no
- **Survives checkout deletion:** maybe, but often leaves an orphan behind

This location is still tied to the checkout neighborhood, just not the checkout itself.

### Multi-repo isolation

**Mixed to good.**

Different repos usually get separate sibling directories, but this is still really “per clone” isolation, not “per logical repo” isolation.

It also creates visible clutter in parent workspace directories.

### Docker mount implications

**Easy.** It is still a normal host path that can be mounted to `/workspace/.tasks`.

No special Docker behavior is required.

### Git safety

**Good.** Because it is outside the repo, accidental commits are much less likely than with in-repo storage.

### Multi-user safety

**Mixed.**

If the repo lives in a shared parent directory, the sibling queue likely inherits that same shared context. It is not naturally per-user.

That can be helpful if a team intentionally shares one checkout on a machine, but it is weaker isolation than home/XDG storage.

### Cleanup story

**Awkward.**

Deleting or moving the repo can leave a mysterious hidden sibling behind. The queue is nearby, but not obviously governed by either git or the operating system's app-data conventions.

### Portability

**Mostly good.** Works on Linux, macOS, and CI as long as the parent directory is writable.

The main failure mode is policy-based: some workspace roots or mounted directories do not want tools creating extra siblings.

### Bottom line

This is a decent compromise if you want locality without polluting the repo itself, but it still feels tied to one checkout and has an unattractive cleanup story.

---

## 7. Option 5: System temp `$TMPDIR/mato/<repo-hash>/`

**Path:** `$TMPDIR/mato/<repo-hash>/`

### Discoverability

**Good.** `TMPDIR` is easy to discover, and a deterministic repo hash gives automatic namespacing.

### Persistence

**Poor.**

- **Survives reboot:** not reliably
- **Survives branch switches:** yes, while the temp directory still exists
- **Survives repo re-clones:** maybe, but only until temp cleanup happens
- **Survives CI job boundaries:** generally no

This is the wrong durability profile for a task queue that may need to survive host restarts or operator interruptions.

### Multi-repo isolation

**Good in principle.** The repo hash keeps queues separate.

### Docker mount implications

**Technically easy, operationally a little shakier.**

Docker can usually mount temp paths, but temp locations vary more across Linux, macOS, and CI. On macOS especially, `TMPDIR` often points into long per-user paths under `/var/folders/...`, which is functional but not very ergonomic.

### Git safety

**Excellent.** It is fully outside the worktree.

### Multi-user safety

**Mixed.**

If `TMPDIR` is per-user, isolation is fine. If the tool ends up under a shared temp root, permissions must be handled carefully (`0700` directories, no trusting pre-existing paths).

This option needs more defensive filesystem handling than home/XDG storage.

### Cleanup story

**Automatic, but too aggressive.**

OS cleanup is the selling point and the dealbreaker at the same time. Temp storage is ideal for scratch clones and caches, but not for durable queue state.

### Portability

**Available everywhere, behavior inconsistent everywhere.**

All target environments have temp storage, but retention rules vary wildly.
That unpredictability is a bad fit for queue state.

### Bottom line

This is useful for explicitly ephemeral modes, tests, or demos, but it is a poor default for production task storage.

---

## 8. Option 6: Configurable path via `--tasks-dir`

**Path:** user-provided

### Discoverability

**Poor as a standalone strategy, excellent as an override.**

A flag does not help `mato` discover anything automatically. Users have to supply it, script it, or store it elsewhere.

That makes it unsuitable as the *only* answer, but very valuable as an escape hatch.

### Persistence

**Depends entirely on the chosen path.**

A user can point it at durable storage (`~/shared/mato/repo-x`) or ephemeral storage (`/tmp/mato/repo-x`).

### Multi-repo isolation

**Depends on user discipline.**

If users point multiple repos at the same directory, queues can collide.
If they point each repo at a distinct namespaced path, isolation is fine.

So the flag is powerful, but it pushes correctness onto the operator.

### Docker mount implications

**Very flexible.** `runOnce()` can simply mount the resolved path to `/workspace/.tasks`.

The main implementation concern is validation:

- resolve to an absolute path
- clean it
- create required subdirectories
- reject obviously invalid values

### Git safety

**Usually excellent, unless the user points back into the repo.**

### Multi-user safety

**Depends on path and permissions.**

This is the one option that can intentionally support a shared queue on a multi-user machine, but only because the operator chooses that behavior.

### Cleanup story

**Operator-defined.** This is powerful but inconsistent.

### Portability

**Potentially excellent.** The flag is especially useful in CI, networked workspaces, or unusual Docker setups.

### Bottom line

`--tasks-dir` should absolutely exist, but as a **policy override**, not as the default location strategy.

---

## 9. Comparative summary

### Criteria summary table

| Location | Discoverability | Persistence | Multi-repo isolation | Docker mount fit | Git safety | Multi-user safety | Cleanup story | Portability |
|---|---|---|---|---|---|---|---|---|
| In-repo `.tasks/` | Excellent | Mixed | Good per checkout | Excellent | Mixed | Weak-Mixed | Simple but sloppy | Good |
| `~/.mato/<repo-hash>/` | Good | Strong | Strong | Strong | Excellent | Good | Needs GC | Very good |
| `$XDG_DATA_HOME/mato/<repo-hash>/` | Good | Strong | Strong | Strong | Excellent | Good | Needs GC | Good-Very good |
| Sibling `../.mato-<repo-name>/` | Good | Mixed | Mixed-Good | Strong | Good | Mixed | Awkward | Good |
| `$TMPDIR/mato/<repo-hash>/` | Good | Poor | Good | Good | Excellent | Mixed | Automatic but unsafe for queues | Mixed |
| `--tasks-dir` | Poor alone / Excellent as override | Variable | Variable | Strong | Variable | Variable | Variable | Excellent |

### Pros / cons table

| Location | Biggest pros | Biggest cons | Best use case |
|---|---|---|---|
| In-repo `.tasks/` | Zero lookup complexity; easy to inspect; already implemented | Pollutes checkout; tied to one clone; requires `.gitignore` mutation | Prototype / simplest possible setup |
| `~/.mato/<repo-hash>/` | Easy implementation; per-user; outside git; durable | Non-standard on Linux; needs GC | Pragmatic cross-platform default |
| `$XDG_DATA_HOME/mato/<repo-hash>/` | Standards-aligned; per-user; outside git; durable | Slightly more lookup logic; less native on macOS | Best default for a Linux-first CLI |
| Sibling directory | Local to the repo without being inside it | Still tied to checkout neighborhood; awkward cleanup | Users who want path locality |
| Temp directory | No repo pollution; automatic cleanup | Queue can disappear on reboot/cleanup | Explicit ephemeral or test mode |
| `--tasks-dir` | Maximum flexibility; supports CI and shared setups | No safe default by itself; easy to misconfigure | Override / escape hatch |

---

## 10. Recommendation

### Recommended default: XDG data directory

The best fit for `mato`'s architecture is:

```text
$XDG_DATA_HOME/mato/<repo-hash>/
```

with the standard fallback:

```text
~/.local/share/mato/<repo-hash>/
```

and a **supported override** via:

```text
--tasks-dir <path>
```

### Why this fits `mato` best

1. **It keeps the container contract unchanged.**
   `runOnce()` can keep mounting the host queue to `/workspace/.tasks`, so the agent prompt and in-container workflow do not need to care where the host stores the files.

2. **It removes git-related footguns.**
   Runtime queue state no longer lives in the repo, and `mato` no longer needs to patch and commit `.gitignore` on startup.

3. **It is persistent enough for a queue.**
   Unlike temp storage, XDG data survives reboots and ordinary workstation churn.

4. **It gives better user isolation.**
   Each user gets their own queue namespace by default, which is safer on shared machines.

5. **It is standards-aligned without being complex.**
   This is a better long-term home for durable application data than a hidden repo directory or hidden sibling directory.

6. **It still allows escape hatches.**
   `--tasks-dir` covers the special cases: shared queues, CI-specific paths, temporary test runs, or unusual Docker mount constraints.

### Practical recommendation

If `mato` changes storage location, I would implement this policy:

1. **Default:** resolve tasks into XDG data using a stable repo identity hash
2. **Fallback:** use XDG's default path if `XDG_DATA_HOME` is unset
3. **Override:** honor `--tasks-dir` exactly
4. **Compatibility:** optionally continue accepting in-repo `.tasks/` for transitional users
5. **Cleanup:** add lightweight metadata plus a future `mato gc` command

### Final ranking

1. **XDG data directory** — best overall default
2. **Home directory `~/.mato`** — best simple fallback if you do not want XDG logic
3. **Configurable `--tasks-dir`** — essential override, not a default
4. **In-repo `.tasks/`** — fine for the current prototype, weak long-term default
5. **Sibling directory** — acceptable but awkward
6. **Temp directory** — only for explicitly ephemeral modes

In short: **move task storage out of the repo, default it into XDG data, and add `--tasks-dir` for operators who need something else.**
