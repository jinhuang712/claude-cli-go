package claudecli

import (
	"context"
	"encoding/json"

	gotato "github.com/jinhuang712/gotato"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// conn is one CLI process holding one conversation.
type conn interface {
	Query(ctx context.Context, prompt string) error
	Messages() <-chan claudecode.Message
	Close() error
}

// spec describes a process to start.
type spec struct {
	Model  string
	System string
	Tools  []gotato.ToolSpec
	Resume string // a session ID to continue, or empty
	// Call serves one tool call the CLI makes and returns the result text
	// and whether it is an error. It blocks until the caller's result
	// arrives.
	Call func(ctx context.Context, toolID string, args map[string]any) (string, bool)
}

type dialer func(ctx context.Context, sp spec) (conn, error)

// sdkDialer starts processes through the CLI's Go SDK. The CLI runs in safe
// mode with its own tools off, so the model sees the caller's system prompt
// and the caller's tools only.
func sdkDialer(cfg Config) dialer {
	serverName := cfg.ServerName
	return func(ctx context.Context, sp spec) (conn, error) {
		opts := []claudecode.Option{
			claudecode.WithModel(sp.Model),
			claudecode.WithSystemPrompt(sp.System),
			claudecode.WithTools([]string{}...),
			claudecode.WithSettingSources(),
			claudecode.WithExtraArgs(map[string]*string{"safe-mode": nil}),
			claudecode.WithEnvVar("SHELL", "/bin/bash"),
			claudecode.WithPermissionMode(claudecode.PermissionModeBypassPermissions),
			claudecode.WithIncludePartialMessages(true),
		}
		if len(sp.Tools) > 0 {
			tools := make([]*claudecode.McpTool, 0, len(sp.Tools))
			allowed := make([]string, 0, len(sp.Tools))
			for _, t := range sp.Tools {
				schema := map[string]any{"type": "object"}
				if len(t.InputSchema) > 0 {
					_ = json.Unmarshal(t.InputSchema, &schema)
				}
				id := t.ID
				tools = append(tools, claudecode.NewTool(toolName(id), t.Description, schema,
					func(ctx context.Context, args map[string]any) (*claudecode.McpToolResult, error) {
						text, isError := sp.Call(ctx, id, args)
						return &claudecode.McpToolResult{Content: []claudecode.McpContent{{Type: "text", Text: text}}, IsError: isError}, nil
					}))
				allowed = append(allowed, "mcp__"+serverName+"__"+toolName(id))
			}
			opts = append(opts,
				claudecode.WithSdkMcpServer(serverName, claudecode.CreateSDKMcpServer(serverName, "1.0.0", tools...)),
				claudecode.WithAllowedTools(allowed...))
		}
		if sp.Resume != "" {
			opts = append(opts, claudecode.WithResume(sp.Resume))
		}
		if cfg.CLIPath != "" {
			opts = append(opts, claudecode.WithCLIPath(cfg.CLIPath))
		}
		if cfg.Dir != "" {
			opts = append(opts, claudecode.WithCwd(cfg.Dir))
		}
		c := claudecode.NewClient(opts...)
		if err := c.Connect(ctx); err != nil {
			return nil, err
		}
		return &sdkConn{c: c, msgs: c.ReceiveMessages(ctx)}, nil
	}
}

type sdkConn struct {
	c    claudecode.Client
	msgs <-chan claudecode.Message
}

func (c *sdkConn) Query(ctx context.Context, prompt string) error { return c.c.Query(ctx, prompt) }
func (c *sdkConn) Messages() <-chan claudecode.Message            { return c.msgs }
func (c *sdkConn) Close() error                                   { return c.c.Disconnect() }
