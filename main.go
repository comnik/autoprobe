package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/anthropics/anthropic-sdk-go"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hopper <programs-dir>")
		os.Exit(1)
	}
	programsDir := os.Args[1]

	client := anthropic.NewClient()

	agent := NewAgent(&client, programsDir)
	err := agent.Run(context.TODO())
	if err != nil {
		fmt.Printf("Error: %s\n", err.Error())
	}
}

func NewAgent(client *anthropic.Client, programsDir string) *Agent {
	return &Agent{
		client:      client,
		programsDir: programsDir,
	}
}

type Agent struct {
	client      *anthropic.Client
	programsDir string
}

func (a *Agent) Run(ctx context.Context) error {
	for {
		conversation, err := a.buildConversation(ctx)
		if err != nil {
			return err
		}

		message, err := a.runInference(ctx, conversation)
		if err != nil {
			return err
		}

		toolUses := 0
		for _, content := range message.Content {
			switch content.Type {
			case "text":
				fmt.Printf("\u001b[93mClaude\u001b[0m: %s\n", content.Text)
			case "tool_use":
				toolUses++
			}
		}

		if toolUses == 0 {
			return nil
		}
	}
}

func (a *Agent) buildConversation(ctx context.Context) ([]anthropic.MessageParam, error) {
	entries, err := os.ReadDir(a.programsDir)
	if err != nil {
		return nil, fmt.Errorf("reading programs dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Indexed slots, not append, so this can be parallelized later without changing
	// ordering.
	outputs := make([][]byte, len(names))
	for i, name := range names {
		path := filepath.Join(a.programsDir, name)
		out, err := exec.CommandContext(ctx, path).Output()
		if err != nil {
			return nil, fmt.Errorf("running %s: %w", name, err)
		}
		outputs[i] = out
	}

	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(outputs))
	for _, out := range outputs {
		blocks = append(blocks, anthropic.NewTextBlock(string(out)))
	}

	return []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)}, nil
}

func (a *Agent) runInference(ctx context.Context, conversation []anthropic.MessageParam) (*anthropic.Message, error) {
	message, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet4_6,
		MaxTokens: int64(1024),
		Messages:  conversation,
	})
	return message, err
}
