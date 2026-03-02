package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	copilot "github.com/github/copilot-sdk/go"
)

func main() {
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

	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		OnPermissionRequest: copilot.PermissionHandler.ApproveAll,
	})
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	defer session.Destroy()

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
