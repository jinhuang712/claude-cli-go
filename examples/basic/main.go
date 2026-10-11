// Command basic runs a gotato agent on a Claude Code CLI session: one tool
// call, then a follow-up turn in the same conversation, which reuses the
// live session.
//
// It needs the Claude Code CLI on PATH, logged in.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	gotato "github.com/jinhuang712/gotato"

	claudecli "github.com/jinhuang712/claude-cli-go"
)

type clockIn struct{}

func main() {
	ctx := context.Background()
	pool := claudecli.New(claudecli.Config{ServerName: "example"}, nil)
	defer func() { _ = pool.Close() }()

	clock, err := gotato.NewFuncTool("current_time", "Return the current time in RFC 3339.",
		func(context.Context, clockIn) (string, error) { return time.Now().Format(time.RFC3339), nil })
	if err != nil {
		log.Fatal(err)
	}
	agent, err := gotato.NewAgent(
		gotato.WithModel(pool.Model("example-conversation", "sonnet")),
		gotato.WithInstruction("You are a concise assistant. Answer in one sentence."),
		gotato.WithTools(clock),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = agent.Close(context.WithoutCancel(ctx)) }()

	for _, question := range []string{"What time is it? Use the tool.", "What did I just ask you?"} {
		start := time.Now()
		res, err := agent.Prompt(ctx, gotato.UserMessage(question))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\n  %s (%.1fs)\n", question, gotato.TextOf(*res.FinalMessage), time.Since(start).Seconds())
	}
}
