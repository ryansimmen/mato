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
go run ./cmd/simenator-launcher
```

- Creates `../staging-labs.worktrees/agent#` as a git worktree if missing.
- Allocates `#` from a file-based lock + counter under `../staging-labs.worktrees`.
- Mounts that agent worktree into the container at `/workspace` and runs simenator from the launcher repo mounted at `/simenator`.
- Sets simenator runtime working directory to `/workspace`, so prompts operate on the agent worktree.
- Mounts host Go toolchain + module/build caches so launcher reuses local dependencies in the container.
- Mounts host `copilot` CLI (and `~/.copilot` when present) into the container.
- Mounts host `gh` CLI (and `~/.config/gh` when present) into the container.
- Mounts host CA certificates (`/etc/ssl/certs`) when available for TLS in Ubuntu containers.
- Forwards extra args to simenator, e.g. `go run ./cmd/simenator-launcher -- -model GPT-5.3-Codex`.
- Defaults to `ubuntu:24.04` and mounts the host Go toolchain at `/usr/local/go` (override with `SIMENATOR_DOCKER_IMAGE`).
- Uses `/home/ryansimmen/staging-labs` as the worktree source repo by default (override with `SIMENATOR_WORKTREE_REPO`).
- Uses the current working directory as the simenator app repo by default (override with `SIMENATOR_APP_REPO`).
- Cleanup all launcher worktrees with `make clean-worktrees`.

## Notes

- This module is pinned to your local SDK checkout via `replace github.com/github/copilot-sdk/go => ./.copilot-sdk/go`.
- The sample uses `copilot.PermissionHandler.ApproveAll` for simplicity.
- Authenticate first with `copilot login`.
