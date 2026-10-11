# Changelog

All notable changes to this project are listed here. The project follows [Semantic Versioning](https://semver.org/).

## v0.1.1 — 2026-10-11

- **Dependencies:** requires gotato v0.1.0, its first tagged release, in place of a commit pseudo-version. The gotato code is the same.

## v0.1.0 — 2026-10-11

First release.

- **`claudecli.Pool`:** serves `gotato.Model` calls from long-lived Claude Code CLI sessions, one per conversation key.
- **Continuing requests:** a request that continues its conversation sends only what is new.
- **Tools:** tool calls run as in-process tools that wait for the caller's results.
- **Calls:** each call ends at the end of one model response, with its tool calls, usage, and stop reason.
- **Idle sessions:** they close after `IdleClose` and resume by their session ID. `MaxSessions` bounds live processes.
- **Restarts:** a request that does not continue its session starts a new one from the conversation as text.
- **Example:** `examples/basic`.
