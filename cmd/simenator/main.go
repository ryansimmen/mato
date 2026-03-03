package main

import (
	"bufio"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

//go:embed task-instructions.md
var taskInstructions string

const maxTaskTurns = 50
const defaultModel = "claude-sonnet-4.5"

func main() {
	taskMode := flag.Bool("task", false, "run in autonomous task mode")
	model := flag.String("model", defaultModel, "model to use for the Copilot session")
	flag.Parse()

	if sessionCwd := os.Getenv("SIMENATOR_SESSION_CWD"); sessionCwd != "" {
		if err := os.Chdir(sessionCwd); err != nil {
			log.Fatalf("set session working directory: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := copilot.NewClient(nil)

	if err := client.Start(ctx); err != nil {
		log.Fatalf("start client: %v", err)
	}
	defer client.Stop()

	// taskDone is signalled by the task_complete tool when the agent finishes.
	taskDone := make(chan struct{}, 1)

	var tools []copilot.Tool
	if *taskMode {
		type TaskCompleteParams struct {
			Summary string `json:"summary" jsonschema:"brief summary of what was completed"`
		}
		tools = append(tools, copilot.DefineTool("task_complete",
			"Call this tool when you have finished the task (success or failure). "+
				"This signals the host to shut down.",
			func(params TaskCompleteParams, inv copilot.ToolInvocation) (string, error) {
				fmt.Printf("Task complete: %s\n", params.Summary)
				select {
				case taskDone <- struct{}{}:
				default:
				}
				return "Acknowledged. Shutting down.", nil
			}))
	}

	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		OnPermissionRequest: copilot.PermissionHandler.ApproveAll,
		Model:               *model,
		SystemMessage: &copilot.SystemMessageConfig{
			Mode:    "append",
			Content: taskInstructions,
		},
		Tools: tools,
	})
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	defer session.Destroy()

	if *taskMode {
		runTaskMode(ctx, session, taskDone)
		return
	}

	runInteractive(ctx, session)
}

func runTaskMode(ctx context.Context, session *copilot.Session, taskDone <-chan struct{}) {
	tasksDir := os.Getenv("SIMENATOR_TASKS_DIR")
	if tasksDir == "" {
		tasksDir = "./tasks"
	}

	prompt := fmt.Sprintf(
		"You have a task queue at %s. Your working directory is your git worktree. "+
			"Follow your system instructions to claim and complete the next available task. "+
			"When you are finished, call the task_complete tool.",
		tasksDir,
	)

	for turn := 0; turn < maxTaskTurns; turn++ {
		if ctx.Err() != nil {
			return
		}
		// Give each turn 10 minutes — agent work (edits, tests, git) can be slow.
		turnCtx, turnCancel := context.WithTimeout(ctx, 10*time.Minute)
		reply, err := session.SendAndWait(turnCtx, copilot.MessageOptions{Prompt: prompt})
		turnCancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Fatalf("task turn %d: %v", turn+1, err)
		}
		if reply != nil && reply.Data.Content != nil {
			fmt.Printf("Agent [turn %d]: %s\n\n", turn+1, *reply.Data.Content)
		}

		select {
		case <-taskDone:
			return
		default:
		}
		prompt = "Continue"
	}
	log.Fatalf("task did not complete within %d turns", maxTaskTurns)
}

func runInteractive(ctx context.Context, session *copilot.Session) {
	fmt.Println("Chat with Copilot (Ctrl+D to exit)")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		if ctx.Err() != nil {
			break
		}
		fmt.Print("You: ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		reply, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: input})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("send message: %v", err)
			continue
		}
		if reply != nil && reply.Data.Content != nil {
			fmt.Printf("Assistant: %s\n\n", *reply.Data.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Fatalf("read input: %v", err)
	}
}
