# Contributing

Thanks for helping. This file covers how to propose a change.

## Before You Start

For a large change, open an issue first, so we can agree on the approach before you write it.

## Making a Change

1. Fork the repository, and create a branch named `feat/`, `fix/`, `docs/`, `test/`, `refactor/`, or `chore/` plus a short topic.
2. Add a test for every behavior change. Tests use the fake CLI in `claudecli_test.go` and need no network and no login.
3. Run the checks:

   ```sh
   gofmt -l .
   go vet ./...
   go test -race ./...
   golangci-lint run ./...
   ```

4. Update `README.md` and `CHANGELOG.md` when the public API or the behavior changes.

## Commits

Use conventional subjects in the imperative mood, under about 72 characters, for example `fix: release waiting handlers when a session closes`. Keep one change per commit when practical.

## Reporting a Bug

Include:
- the Go version;
- the Claude Code CLI version (`claude --version`);
- a minimal program that shows the problem;
- what you expected, and what happened instead.
