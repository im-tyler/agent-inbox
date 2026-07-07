# agent-inbox — Plan

## What this is

A federated supervisor for CLI coding agents. One inbox holds N independent projects, each backed by its own long-lived Claude Code or OpenCode session. You see who's `waiting`, `send` a project a message, `view` its last reply, or `attach` to drop into the live session.

This is the inverse of Agent Teams / Claude Squad / CAO, which decompose one project into parallel workers. Here each project is an independent peer with its own context; the value is reducing context-switching cost across a portfolio.

## Current state

**Alpha, live-verified on the core flow.** Per the README status table:

| Piece | State |
|---|---|
| Driver interface + abstraction | done |
| Mock driver | done, exercised |
| Claude adapter | done, live-verified (send / resume / session persistence) |
| OpenCode adapter | done, live-verified (new session + resume + export-based reply, free model) |
| Inbox REPL (ls/send/view/attach) | done, background sends + restart persistence verified |
| State persistence | done |
| Stop-hook bridge + live notify | done, live-verified |

Phase 1 (Claude + OpenCode) is functionally complete. 4 commits in the repo, but the working tree represents substantial live-tested functionality.

## Architecture

```
main.go                 REPL + wiring
internal/config         config.json (projects + per-tool settings)
internal/inbox          project state, mutex-guarded; background Send; persistence
internal/driver         Driver interface + adapters (mock, claude, opencode)
```

The only vendor-specific code lives in `internal/driver/*.go`. Each adapter implements:

```go
Send(ctx, dir, sessionID, prompt) Result   // empty sessionID = new session
AttachArgs(dir, sessionID) []string          // argv for interactive drop-in
```

Key v1 simplification: in headless one-shot mode, the process returns when the turn is done, so the normalized status is simply `waiting` on success — sidestepping the fuzzy "blocked vs done vs working" classification until streaming mode.

## Roadmap

### Shipped
- Claude adapter (verified against Claude 2.1.167)
- OpenCode adapter (verified against OpenCode 1.15.11, free default model)
- Inbox REPL with background sends
- State persistence across restarts
- Stop-hook bridge + live transcript notification

### Next (v0.2)
- Additional driver: Cursor CLI (if/when it stabilizes a headless mode)
- Streaming mode: stop relying on turn-returns-when-done; classify working/blocked/waiting live
- TUI: replace the basic REPL with a proper Bubble Tea (or equivalent) TUI showing all projects + statuses in a single dashboard
- Multi-host: projects live on different machines, agent-inbox coordinates over Tailscale

### Later
- Web UI (read-only dashboard across the portfolio)
- Driver SDK: third parties can ship adapters for new agents
- Approval flows: long-lived sessions ping agent-inbox for permission, you approve from the inbox

## Out of scope (deliberate)

- **Decomposing a single project into parallel workers** — that's what Agent Teams / Claude Squad do; we are explicitly the opposite
- **Replacing the underlying agents** — maccel rides on top of Claude Code / OpenCode, doesn't reimplement them
- **Non-CLI agents** (web-based, IDE-embedded) — v1 is CLI-only

## Design decisions to defend

1. **One inbox, N projects** (not N tabs, N terminals). Single dashboard for the whole portfolio.
2. **Drivers wrap existing CLIs** rather than calling model APIs directly. Insulates us from provider changes; works with any model the underlying CLI supports.
3. **Headless one-shot as the v1 primitive.** Avoids the messy "is it actually done?" classification until streaming is justified.
4. **Go, single binary.** Matches the Teploy philosophy; trivially installable.

## Open questions

- Streaming mode classification heuristic — needs design before implementation
- Whether TUI lives in this repo or ships as a separate binary that talks to a daemonized agent-inbox

## License

MIT — see [LICENSE](LICENSE).
