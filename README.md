# claude-cli-go

[![CI](https://github.com/jinhuang712/claude-cli-go/actions/workflows/ci.yml/badge.svg)](https://github.com/jinhuang712/claude-cli-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/jinhuang712/claude-cli-go.svg)](https://pkg.go.dev/github.com/jinhuang712/claude-cli-go)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

claude-cli-go runs a [gotato](https://github.com/jinhuang712/gotato) agent on long-lived Claude Code CLI sessions. The package `claudecli` implements `gotato.Model`, so an agent runs the same on it as on a Messages API endpoint: it sees a model and nothing else.

Use it when the Claude Code CLI is how you reach Claude, for example on a Claude plan without an API key.

This is an independent project. It is not affiliated with or endorsed by Anthropic.

## Why Long-Lived Sessions

Starting a CLI process for every model call costs seconds of latency and CPU per call. claude-cli-go keeps one CLI process per conversation and sends it only what is new, so a continuing call costs about what a direct API call does.

Measured on a Mac with a Sonnet model, 2026-10:

| Measure | A CLI process per call | One long-lived session |
|---|---|---|
| Time to the first event, continuing call | 4–8 s | 1.1–2.3 s |
| CPU per call | about 3 s | about 0.25 s |
| Memory | a process per call in flight | one process per live conversation, about 150–330 MB |

## Install

```sh
go get github.com/jinhuang712/claude-cli-go
```

Requirements:
- Go 1.22 or newer.
- gotato v0.1.0 or newer. `go get` adds it.
- The Claude Code CLI on `PATH`, or at `Config.CLIPath`, already logged in.

## Quick Start

```go
pool := claudecli.New(claudecli.Config{ServerName: "myapp"}, nil)
defer pool.Close()

agent, err := gotato.NewAgent(
	gotato.WithModel(pool.Model("conversation-42", "sonnet")),
	gotato.WithInstruction("You are a concise assistant."),
	gotato.WithTools(tools...),
)
```

`pool.Model(key, model)` returns a `gotato.Model` for one conversation:
- The key identifies the conversation. Calls on one key run one at a time, and each key gets its own CLI process.
- The model is any model name the CLI accepts, such as `sonnet` or a full model ID.

[examples/basic](examples/basic/main.go) runs an agent with one tool across two turns.

## How It Works

| Situation | What claudecli does |
|---|---|
| A request continues the conversation the session holds | Sends only what is new: a user message, or the results of the tool calls the session waits on |
| The model calls tools | The request's tools run inside the CLI as in-process tools whose handlers wait. The call ends at the end of the model response, with its tool calls and usage. Your agent runs the tools as usual. |
| The next request brings the tool results | The waiting handlers return them, and the session continues. A message after the results reaches the model with the last result. |
| No call for `IdleClose` | The session's process ends. The next call resumes the session by its ID. |
| A request does not continue the session | A new session starts, with the conversation so far written out as text |

The CLI runs in safe mode, with its own tools off and a plain bash shell. The model sees your system prompt and your tools only. Tool names reach the model as `mcp__<ServerName>__<tool>`.

Each response ends a call with:
- streamed text and reasoning deltas;
- each tool call;
- usage, with cache reads and writes counted as input, as gotato's Messages adapter counts them;
- the stop reason.

## Config

| Field | Default | Meaning |
|---|---|---|
| `CLIPath` | `claude` on `PATH` | The CLI binary |
| `Dir` | the process's working directory | The CLI processes' working directory |
| `IdleClose` | 10 min | A session's process ends after this long without a call |
| `MaxSessions` | 8 | Live processes at most. The least recently used idle session closes first. |
| `ServerName` | `gotato` | The name of the in-process tool server |

`Pool.Close` ends every process, all at once, and waits for them. `Pool.Live` reports the live processes.

## Trade-offs

| Topic | Detail |
|---|---|
| Request settings | The CLI decides max tokens, temperature, tool choice, and caching. It writes the one-hour prompt cache. |
| History | A request must continue its conversation to keep the session. A rewritten history starts a new session from text, which loses the structure of the earlier turns. |
| Memory | Each live conversation holds one CLI process. `IdleClose` and `MaxSessions` bound them. |
| Framing | The CLI adds about 1k tokens per conversation. |
| Usage | Calls count against the plan the CLI is logged in to, like any Claude Code use. Check Anthropic's terms for your use. |

## Development

```sh
gofmt -l .
go vet ./...
go test -race ./...
golangci-lint run ./...
```

Tests run against a fake CLI that plays scripted responses the way the real CLI does, so they need no network and no login. See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md).

## Status

Version 0.x: the API may change before 1.0. Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## Acknowledgments

[`severity1/claude-agent-sdk-go`](https://github.com/severity1/claude-agent-sdk-go) (MIT) drives the CLI.

## License

[Apache-2.0](LICENSE)
