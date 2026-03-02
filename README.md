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

## Notes

- This module is pinned to your local SDK checkout via `replace github.com/github/copilot-sdk/go => ./.copilot-sdk/go`.
- The sample uses `copilot.PermissionHandler.ApproveAll` for simplicity.
- Authenticate first with `copilot login`.
