# simenator

Simple Go example that uses the local Copilot SDK clone in `.copilot-sdk/go` to run a minimal interactive chat.

## What It Does

- Starts a Copilot SDK client and session
- Reads prompts from stdin (`You: ...`)
- Sends each prompt with `session.SendAndWait`
- Prints the assistant response and continues until EOF (Ctrl+D)

## Run

```bash
go run ./cmd/simenator
```

## Docker launcher (worktree per instance)

```bash
make run-launcher
```

- Creates `../staging-labs.worktrees/agent#` as a git worktree if missing.
- Allocates `#` from a file-based lock + counter under `../staging-labs.worktrees`.
- Reuses the lowest available `agent#` whose container is not currently running.
- Mounts that agent worktree into the container at `/workspace` and runs simenator from the launcher repo mounted at `/simenator`.
- Sets simenator runtime working directory to `/workspace`, so prompts operate on the agent worktree.
- Mounts host Go toolchain + module/build caches so launcher reuses local dependencies in the container.
- Mounts host `copilot` CLI, `gh` CLI, and `~/.copilot/skills` into the container.
- Mounts host CA certificates (`/etc/ssl/certs`) when available for TLS in Ubuntu containers.
- Requires `--worktree-repo` (Makefile passes this via `WORKTREE_REPO`).
- Forwards extra args to simenator, e.g. `go run ./cmd/simenator-launcher --worktree-repo /path/to/repo -- -model GPT-5.3-Codex`.
- Defaults to `ubuntu:24.04` and mounts the host Go toolchain at `/usr/local/go` (override with `SIMENATOR_DOCKER_IMAGE`).
- Uses the current working directory as the simenator app repo by default (override with `SIMENATOR_APP_REPO`).
- Cleanup all launcher worktrees with `make clean-worktrees` (override repo with `WORKTREE_REPO=/path/to/worktree-source-repo`).

## Notes

- This module is pinned to your local SDK checkout via `replace github.com/github/copilot-sdk/go => ./.copilot-sdk/go`.
- The sample uses `copilot.PermissionHandler.ApproveAll` for simplicity.
- Authenticate first with `copilot login`.
