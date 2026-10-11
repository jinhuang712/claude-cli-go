# AGENTS.md — Instructions for Coding Agents

claude-cli-go serves `gotato.Model` calls from long-lived Claude Code CLI sessions. Read [README.md](README.md) first.

## Layout

| File | Holds |
|---|---|
| `claudecli.go` | `Config`, `Pool`, the model and stream types, idle closing |
| `session.go` | One conversation: continuing a request, delivering tool results, reading one response |
| `conn.go` | The CLI process through the SDK, and the fake-able `conn` and `dialer` seams |
| `render.go` | Message hashes, tool names, the text form of a conversation, tool result text |
| `claudecli_test.go` | The fake CLI and the tests |
| `examples/basic` | A runnable example that needs a real, logged-in CLI |

## Rules

- **The caller sees a `gotato.Model` and nothing else.** Keep CLI details inside the package.
- **One call ends at the end of one model response,** on the response's stop event.
- **Tool handlers wait for the caller's results.** They never run tools themselves.
- **A closed process never leaves a handler waiting.** Every wait also watches the session's close signal and the call's context.
- **Tests use the fake CLI.** Network calls and a logged-in CLI belong in `examples/` only.
- **The module supports Go 1.22.** Avoid standard library features newer than that.

## Checks

```sh
gofmt -l .
go vet ./...
go test -race ./...
golangci-lint run ./...
```

Report any check you could not run.
